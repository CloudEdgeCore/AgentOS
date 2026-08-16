package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	gatewayv1alpha1 "github.com/bian-cloud-skill/agentos/gen/go/agentos/gateway/v1alpha1"
	modelv1alpha1 "github.com/bian-cloud-skill/agentos/gen/go/agentos/model/v1alpha1"
	"github.com/bian-cloud-skill/agentos/internal/gateway"
	"github.com/bian-cloud-skill/agentos/internal/gateway/bao"
	"github.com/bian-cloud-skill/agentos/internal/kernel/model"
	"github.com/bian-cloud-skill/agentos/internal/kernel/policy"
	kernelstore "github.com/bian-cloud-skill/agentos/internal/kernel/store"
	postgresstore "github.com/bian-cloud-skill/agentos/internal/kernel/store/postgres"
	"github.com/bian-cloud-skill/agentos/internal/kernel/tool"
	"github.com/bian-cloud-skill/agentos/internal/platform/grpcx"
	"github.com/bian-cloud-skill/agentos/internal/platform/otel"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
)

func main() {
	databaseURL := flag.String("database-url", os.Getenv("DATABASE_URL"), "PostgreSQL connection URL (or DATABASE_URL)")
	listen := flag.String("listen", "127.0.0.1:9091", "loopback-only gRPC listen address")
	tenantPoliciesFile := flag.String("tenant-policies", "", "JSON tenant policy data (absent tenants are denied by default)")
	tenantID := flag.String("tenant", "", "fixed development tenant")
	seedDevTools := flag.Bool("seed-dev-tools", false, "idempotently register the development echo.dev tool")
	devMode := flag.Bool("dev-mode", false, "acknowledge the development executor and fixed tenant")
	baoAddr := flag.String("bao-addr", "", "OpenBao address for the Secret Broker (e.g. http://127.0.0.1:58200)")
	baoToken := flag.String("bao-token", "", "OpenBao token with read access to the agentos KV v2 paths")
	baoMount := flag.String("bao-mount", bao.DefaultMount, "OpenBao KV v2 mount (default secret)")
	baoNamespace := flag.String("bao-namespace", "", "optional OpenBao namespace header")
	baoCacheTTL := flag.Duration("bao-cache-ttl", 30*time.Second, "issued secret handle cache TTL")
	flag.Parse()
	if *databaseURL == "" || strings.TrimSpace(*tenantID) == "" || !*devMode {
		slog.Error("database URL, fixed tenant, and explicit -dev-mode are required")
		os.Exit(2)
	}
	if !isLoopback(*listen) {
		slog.Error("the development gateway must listen on loopback only", "listen", *listen)
		os.Exit(2)
	}
	tenantPolicies := policy.TenantPolicies{}
	if *tenantPoliciesFile != "" {
		var err error
		tenantPolicies, err = loadTenantPolicies(*tenantPoliciesFile)
		if err != nil {
			slog.Error("load tenant policies", "error", err)
			os.Exit(2)
		}
	}
	policyEngine, err := policy.New(tenantPolicies)
	if err != nil {
		slog.Error("prepare policy engine", "error", err)
		os.Exit(1)
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

	if *seedDevTools {
		seed, seedErr := repository.RegisterToolDescriptor(ctx, kernelstore.RegisterToolDescriptorInput{
			TenantID: *tenantID, Name: "echo.dev", Version: "1.0.0", SideEffectRisk: kernelstore.ToolRiskNone,
			Actions: []string{"echo"}, ResourcePatterns: []string{"*"},
			ParamsSchema: []byte(`{"type":"object","additionalProperties":true}`),
		})
		if seedErr != nil {
			slog.Error("seed development tool", "error", seedErr)
			os.Exit(1)
		}
		slog.Info("development tool registered", "tool", seed.Name+"@"+seed.Version)
	}

	executor := &gateway.DevExecutor{MaxOutputBytes: 1 << 20}
	var secrets tool.SecretBroker
	if strings.TrimSpace(*baoAddr) != "" {
		if strings.TrimSpace(*baoToken) == "" {
			slog.Error("-bao-addr requires -bao-token")
			os.Exit(2)
		}
		var options []bao.Option
		if *baoMount != bao.DefaultMount {
			options = append(options, bao.WithMount(*baoMount))
		}
		if strings.TrimSpace(*baoNamespace) != "" {
			options = append(options, bao.WithNamespace(*baoNamespace))
		}
		if *baoCacheTTL > 0 {
			options = append(options, bao.WithCacheTTL(*baoCacheTTL))
		}
		broker, err := bao.NewBroker(*baoAddr, *baoToken, options...)
		if err != nil {
			slog.Error("configure OpenBao Secret Broker", "error", err)
			os.Exit(1)
		}
		pingCtx, cancelPing := context.WithTimeout(ctx, 5*time.Second)
		if err := broker.Ping(pingCtx); err != nil {
			cancelPing()
			slog.Error("OpenBao Secret Broker is not reachable", "error", err)
			os.Exit(1)
		}
		cancelPing()
		secrets = broker
		slog.Info("OpenBao Secret Broker active", "address", *baoAddr, "mount", *baoMount, "cacheTtl", *baoCacheTTL)
	} else {
		secrets = &gateway.DevSecretBroker{}
		slog.Warn("Secret Broker is the development stub: no real credentials are issued")
	}
	decisionGateway := tool.NewGateway(policyEngine, repository, repository, repository, executor, secrets)
	modelGateway := model.NewGateway(policyEngine, repository, repository, repository)
	server := grpc.NewServer(grpcx.ServerOptions()...)
	gatewayv1alpha1.RegisterToolGatewayServiceServer(server, gateway.NewService(decisionGateway, *tenantID))
	modelv1alpha1.RegisterModelGatewayServiceServer(server, gateway.NewModelService(modelGateway, *tenantID))
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		slog.Error("listen for Tool Gateway", "error", err)
		os.Exit(1)
	}
	go func() {
		<-ctx.Done()
		server.GracefulStop()
	}()
	slog.Info("Agent OS development Tool Gateway listening", "address", *listen, "tenant", *tenantID)
	if err := server.Serve(listener); err != nil {
		slog.Error("Tool Gateway server", "error", err)
		os.Exit(1)
	}
}

func isLoopback(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func loadTenantPolicies(path string) (policy.TenantPolicies, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var policies policy.TenantPolicies
	if err := decoder.Decode(&policies); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("tenant policy file must contain exactly one JSON document")
	}
	return policies, nil
}
