package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/CloudEdgeCore/AgentOS/cmd/mtlsutil"
	runtimev1 "github.com/CloudEdgeCore/AgentOS/gen/go/agentos/runtime/v1"
	"github.com/CloudEdgeCore/AgentOS/internal/platform/artifact"
	"github.com/CloudEdgeCore/AgentOS/internal/platform/grpcx"
	"github.com/CloudEdgeCore/AgentOS/internal/platform/otel"
	runtimeadapter "github.com/CloudEdgeCore/AgentOS/internal/runtime/adapter"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	controlAddress := flag.String("control-address", "127.0.0.1:9090", "Runtime Protocol control endpoint")
	endpoint := flag.String("adapter-endpoint", "", "loopback Agent Runtime Interface endpoint")
	tenantID := flag.String("tenant", "", "tenant assigned to this worker")
	runtimeInstanceID := flag.String("runtime-instance-id", "", "assigned runtime instance ID")
	artifactRoot := flag.String("artifact-root", "", "content-addressed artifact directory")
	pollInterval := flag.Duration("poll-interval", 250*time.Millisecond, "assignment polling interval")
	heartbeatTTL := flag.Duration("heartbeat-ttl", 30*time.Second, "requested runtime lease TTL")
	tlsCert := flag.String("tls-cert", "", "worker X.509-SVID certificate")
	tlsKey := flag.String("tls-key", "", "worker X.509-SVID private key")
	trustBundle := flag.String("trust-bundle", "", "SPIFFE trust bundle")
	devMode := flag.Bool("dev-mode", false, "allow plaintext Runtime Protocol for local development")
	flag.Parse()
	if strings.TrimSpace(*endpoint) == "" || strings.TrimSpace(*tenantID) == "" ||
		strings.TrimSpace(*runtimeInstanceID) == "" || strings.TrimSpace(*artifactRoot) == "" ||
		*pollInterval <= 0 || *heartbeatTTL <= 0 {
		slog.Error("adapter endpoint, tenant, runtime instance, artifact root and positive intervals are required")
		os.Exit(2)
	}
	parsed, err := url.Parse(*endpoint)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() == "" {
		slog.Error("adapter endpoint must be an absolute HTTP loopback URL")
		os.Exit(2)
	}
	ip := net.ParseIP(parsed.Hostname())
	if parsed.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		slog.Error("v1 adapter endpoint must be co-located on loopback", "endpoint", *endpoint)
		os.Exit(2)
	}
	tlsConfigured := *tlsCert != "" || *tlsKey != "" || *trustBundle != ""
	credentials, err := mtlsutil.ClientCredentials(tlsConfigured, *tlsCert, *tlsKey, *trustBundle)
	if err != nil {
		slog.Error("configure Runtime Protocol identity", "error", err)
		os.Exit(1)
	}
	if credentials == nil {
		if !*devMode {
			slog.Error("mTLS identity is required unless -dev-mode is explicit")
			os.Exit(2)
		}
		credentials = insecure.NewCredentials()
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
	connection, err := grpc.NewClient(*controlAddress,
		append([]grpc.DialOption{grpc.WithTransportCredentials(credentials)}, grpcx.ClientOptions()...)...)
	if err != nil {
		slog.Error("create Runtime Protocol client", "error", err)
		os.Exit(1)
	}
	defer connection.Close()
	artifacts, err := artifact.NewFilesystem(*artifactRoot, 64<<20)
	if err != nil {
		slog.Error("create artifact store", "error", err)
		os.Exit(1)
	}
	worker, err := runtimeadapter.NewWorker(runtimev1.NewRuntimeControlServiceClient(connection), artifacts,
		*endpoint, *tenantID, *runtimeInstanceID, *heartbeatTTL, nil)
	if err != nil {
		slog.Error("create adapter worker", "error", err)
		os.Exit(1)
	}
	ticker := time.NewTicker(*pollInterval)
	defer ticker.Stop()
	for {
		processed, err := worker.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			slog.Error("adapter runtime execution", "error", err)
		}
		if processed {
			slog.Info("adapter runtime completed assignment", "runtimeInstanceId", *runtimeInstanceID)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
