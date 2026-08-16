package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	controlapi "github.com/bian-cloud-skill/agentos/internal/control/api"
	"github.com/bian-cloud-skill/agentos/internal/control/auth"
	"github.com/bian-cloud-skill/agentos/internal/kernel/agentpkg"
	"github.com/bian-cloud-skill/agentos/internal/kernel/memory"
	postgresstore "github.com/bian-cloud-skill/agentos/internal/kernel/store/postgres"
	"github.com/bian-cloud-skill/agentos/internal/platform/otel"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

func main() {
	databaseURL := flag.String("database-url", os.Getenv("DATABASE_URL"), "PostgreSQL connection URL (or DATABASE_URL)")
	listen := flag.String("listen", "127.0.0.1:8080", "HTTP listen address")
	devTenant := flag.String("dev-tenant", "", "fixed development tenant; requires a loopback listen address")
	oidcIssuer := flag.String("oidc-issuer", "", "OIDC issuer for Bearer ID-token authentication")
	oidcClientID := flag.String("oidc-client-id", "", "OIDC client ID the API's audience must match")
	oidcTenantClaim := flag.String("oidc-tenant-claim", "tenant", "OIDC claim carrying the tenant scope")
	var packageTrustKeys []string
	flag.Func("package-trust-key", "trusted package signing key as keyID=base64rawpub (repeatable; ADR-010 publish admission)", func(value string) error {
		packageTrustKeys = append(packageTrustKeys, value)
		return nil
	})
	auditSigningKey := flag.String("audit-signing-key", "", "base64 raw std ed25519 private key that signs audit exports (ADR-014; absent = unsigned dev exports)")
	auditSigningKeyID := flag.String("audit-signing-key-id", "control-plane-audit", "key identity recorded on signed audit exports")
	flag.Parse()

	if *databaseURL == "" {
		slog.Error("database URL is required")
		os.Exit(2)
	}
	devMode := strings.TrimSpace(*devTenant) != ""
	oidcMode := strings.TrimSpace(*oidcIssuer) != "" || strings.TrimSpace(*oidcClientID) != ""
	if devMode == oidcMode {
		slog.Error("exactly one of -dev-tenant (loopback development) or -oidc-issuer/-oidc-client-id (verified identity) is required")
		os.Exit(2)
	}
	if devMode && !loopbackAddress(*listen) {
		slog.Error("static development identity may only listen on loopback", "listen", *listen)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdownTelemetry, err := otel.Init(ctx)
	if err != nil {
		slog.Error("initialize OpenTelemetry", "error", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = shutdownTelemetry(shutdownCtx)
	}()
	pool, err := pgxpool.New(ctx, *databaseURL)
	if err != nil {
		slog.Error("create PostgreSQL pool", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		slog.Error("connect to PostgreSQL", "error", err)
		os.Exit(1)
	}

	repository := postgresstore.New(pool)
	var options []controlapi.Option
	if registry, err := packageTrustRegistry(packageTrustKeys); err != nil {
		slog.Error("configure package trust registry", "error", err)
		os.Exit(1)
	} else if registry != nil {
		options = append(options, controlapi.WithPackageAdmission(registry))
		slog.Info("publish admission active: agent versions require a signed package", "trustedKeys", registry.Len())
	} else {
		slog.Warn("publish admission is OFF: unsigned agent version publications are allowed (dev mode)")
	}
	options = append(options, controlapi.WithAuditStore(repository))
	if strings.TrimSpace(*auditSigningKey) != "" {
		decoded, err := agentpkg.DecodePrivateKey(*auditSigningKey)
		if err != nil {
			slog.Error("configure audit signing key", "error", err)
			os.Exit(1)
		}
		options = append(options, controlapi.WithAuditSigningKey(*auditSigningKeyID, decoded))
		slog.Info("audit exports are signed", "keyId", *auditSigningKeyID)
	} else {
		slog.Warn("audit exports are UNSIGNED (dev mode); configure -audit-signing-key for signed WORM archives")
	}
	handler := controlapi.NewHandler(repository, repository, repository,
		memory.NewGateway(memory.DevEmbedder{}, repository), options...)
	if devMode {
		handler = auth.StaticMiddleware(auth.Principal{Subject: "local-developer", TenantID: *devTenant}, handler)
		slog.Info("control API using static development identity", "tenant", *devTenant)
	} else {
		var err error
		handler, err = auth.OIDCMiddleware(*oidcIssuer, *oidcClientID, *oidcTenantClaim, handler)
		if err != nil {
			slog.Error("configure OIDC authentication", "error", err)
			os.Exit(1)
		}
		slog.Info("control API using OIDC identity", "issuer", *oidcIssuer, "tenantClaim", *oidcTenantClaim)
	}
	// Reference observability: HTTP spans and server metrics via otelhttp.
	handler = otelhttp.NewHandler(handler, "agentos.control.http")
	server := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	slog.Info("Agent OS development control API listening", "address", *listen, "tenant", *devTenant)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("serve control API", "error", err)
		os.Exit(1)
	}
}

func loopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// packageTrustRegistry builds the ADR-010 trust registry from
// -package-trust-key flags (or AGENTOS_PACKAGE_TRUST_KEYS, comma-separated).
// A nil registry means publish admission is off (dev mode).
func packageTrustRegistry(flagKeys []string) (*agentpkg.Registry, error) {
	entries := append([]string(nil), flagKeys...)
	if env := strings.TrimSpace(os.Getenv("AGENTOS_PACKAGE_TRUST_KEYS")); env != "" {
		entries = append(entries, strings.Split(env, ",")...)
	}
	if len(entries) == 0 {
		return nil, nil
	}
	registry := agentpkg.NewRegistry()
	for _, entry := range entries {
		id, encoded, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if !ok || strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("package trust key %q must be keyID=base64rawpub", entry)
		}
		publicKey, err := agentpkg.DecodePublicKey(encoded)
		if err != nil {
			return nil, fmt.Errorf("package trust key %q: %w", id, err)
		}
		if err := registry.Add(agentpkg.Key{ID: id, PublicKey: publicKey}); err != nil {
			return nil, err
		}
	}
	return registry, nil
}
