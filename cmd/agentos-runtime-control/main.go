package main

import (
	"context"
	"crypto/tls"
	"flag"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	runtimev1alpha1 "github.com/bian-cloud-skill/agentos/gen/go/agentos/runtime/v1alpha1"
	postgresstore "github.com/bian-cloud-skill/agentos/internal/kernel/store/postgres"
	"github.com/bian-cloud-skill/agentos/internal/platform/grpcx"
	"github.com/bian-cloud-skill/agentos/internal/platform/otel"
	"github.com/bian-cloud-skill/agentos/internal/platform/spiffe"
	runtimecontrol "github.com/bian-cloud-skill/agentos/internal/runtime/control"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
)

func main() {
	databaseURL := flag.String("database-url", os.Getenv("DATABASE_URL"), "PostgreSQL connection URL (or DATABASE_URL)")
	listenAddress := flag.String("listen", "127.0.0.1:9090", "gRPC listen address")
	devTenant := flag.String("dev-tenant", "", "fixed development tenant")
	maxLeaseTTL := flag.Duration("max-lease-ttl", 2*time.Minute, "maximum runtime-requested lease TTL")
	devMode := flag.Bool("dev-mode", false, "acknowledge plaintext loopback-only development transport")
	tlsCert := flag.String("tls-cert", "", "control plane X.509-SVID certificate (PEM)")
	tlsKey := flag.String("tls-key", "", "control plane X.509-SVID private key (PEM)")
	trustBundle := flag.String("trust-bundle", "", "SPIFFE trust bundle (PEM CA certificates)")
	spiffePattern := flag.String("spiffe-pattern", "spiffe://agentos.dev/ns/*/worker/*", "SPIFFE ID pattern authorized to call the Runtime Protocol")
	flag.Parse()

	if *databaseURL == "" || strings.TrimSpace(*devTenant) == "" || *maxLeaseTTL <= 0 || !*devMode || !loopback(*listenAddress) {
		slog.Error("database URL, dev tenant, positive max lease TTL, loopback listener, and explicit -dev-mode are required")
		os.Exit(2)
	}
	tlsConfigured := *tlsCert != "" || *tlsKey != "" || *trustBundle != ""
	if tlsConfigured && (*tlsCert == "" || *tlsKey == "" || *trustBundle == "") {
		slog.Error("-tls-cert, -tls-key and -trust-bundle must be provided together for the mTLS boundary")
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
	listener, err := net.Listen("tcp", *listenAddress)
	if err != nil {
		slog.Error("listen for Runtime Protocol", "error", err)
		os.Exit(1)
	}
	serverOptions := append([]grpc.ServerOption{}, grpcx.ServerOptions()...)
	var serviceOptions []runtimecontrol.Option
	if tlsConfigured {
		serverSVID, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			slog.Error("load control plane SVID", "error", err)
			os.Exit(1)
		}
		bundlePEM, err := os.ReadFile(*trustBundle)
		if err != nil {
			slog.Error("read trust bundle", "error", err)
			os.Exit(1)
		}
		pool, err := spiffe.TrustBundlePool([][]byte{bundlePEM})
		if err != nil {
			slog.Error("parse trust bundle", "error", err)
			os.Exit(1)
		}
		pattern, err := spiffe.ParsePattern(*spiffePattern)
		if err != nil {
			slog.Error("parse SPIFFE pattern", "error", err)
			os.Exit(1)
		}
		serverOptions = append(serverOptions, grpcx.ServerMTLSOptions(serverSVID, pool)...)
		serverOptions = append(serverOptions, grpcx.WithSpiffeIdentity(pattern))
		serviceOptions = append(serviceOptions, runtimecontrol.WithSpiffeClaimBinding(pattern.TrustDomain))
		slog.Info("Runtime Protocol identity boundary active (mTLS X.509-SVID)", "pattern", pattern.String())
	} else {
		slog.Warn("Runtime Protocol has NO transport identity boundary: workers connect in plaintext (dev mode only)")
	}
	server := grpc.NewServer(serverOptions...)
	runtimev1alpha1.RegisterRuntimeControlServiceServer(server,
		runtimecontrol.NewService(postgresstore.New(pool), *devTenant, *maxLeaseTTL, serviceOptions...))
	healthServer := health.NewServer()
	healthServer.SetServingStatus("agentos.runtime.v1alpha1.RuntimeControlService", grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(server, healthServer)
	go func() {
		<-ctx.Done()
		healthServer.Shutdown()
		done := make(chan struct{})
		go func() {
			server.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			server.Stop()
		}
	}()
	slog.Info("Agent OS development Runtime Protocol listening", "address", *listenAddress, "tenant", *devTenant)
	if err := server.Serve(listener); err != nil && ctx.Err() == nil {
		slog.Error("serve Runtime Protocol", "error", err)
		os.Exit(1)
	}
}

func loopback(address string) bool {
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
