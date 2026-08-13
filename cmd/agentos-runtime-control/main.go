package main

import (
	"context"
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
	flag.Parse()

	if *databaseURL == "" || strings.TrimSpace(*devTenant) == "" || *maxLeaseTTL <= 0 || !*devMode || !loopback(*listenAddress) {
		slog.Error("database URL, dev tenant, positive max lease TTL, loopback listener, and explicit -dev-mode are required")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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
	server := grpc.NewServer(
		grpc.MaxRecvMsgSize(1<<20),
		grpc.MaxSendMsgSize(1<<20),
	)
	runtimev1alpha1.RegisterRuntimeControlServiceServer(server,
		runtimecontrol.NewService(postgresstore.New(pool), *devTenant, *maxLeaseTTL))
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
