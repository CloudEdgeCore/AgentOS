package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	runtimev1alpha1 "github.com/bian-cloud-skill/agentos/gen/go/agentos/runtime/v1alpha1"
	"github.com/bian-cloud-skill/agentos/internal/platform/artifact"
	"github.com/bian-cloud-skill/agentos/internal/runtime/reference"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	controlAddress := flag.String("control-address", "127.0.0.1:9090", "Runtime Protocol control endpoint")
	tenantID := flag.String("tenant", "", "development tenant")
	runtimeInstanceID := flag.String("runtime-instance-id", "", "assigned runtime instance ID")
	artifactRoot := flag.String("artifact-root", "", "development content-addressed artifact directory")
	pollInterval := flag.Duration("poll-interval", 250*time.Millisecond, "assignment polling interval")
	heartbeatTTL := flag.Duration("heartbeat-ttl", 30*time.Second, "requested runtime lease TTL")
	devMode := flag.Bool("dev-mode", false, "acknowledge deterministic non-sandboxed development provider")
	flag.Parse()
	if strings.TrimSpace(*tenantID) == "" || strings.TrimSpace(*runtimeInstanceID) == "" || strings.TrimSpace(*artifactRoot) == "" ||
		*pollInterval <= 0 || *heartbeatTTL <= 0 || !*devMode {
		slog.Error("tenant, runtime instance, artifact root, positive intervals, and explicit -dev-mode are required")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	connection, err := grpc.NewClient(*controlAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		slog.Error("create Runtime Protocol client", "error", err)
		os.Exit(1)
	}
	defer connection.Close()
	artifactStore, err := artifact.NewFilesystem(*artifactRoot, 64<<20)
	if err != nil {
		slog.Error("create development artifact store", "error", err)
		os.Exit(1)
	}
	worker := reference.NewWorker(runtimev1alpha1.NewRuntimeControlServiceClient(connection), artifactStore,
		*tenantID, *runtimeInstanceID, *heartbeatTTL)
	ticker := time.NewTicker(*pollInterval)
	defer ticker.Stop()
	for {
		processed, err := worker.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			slog.Error("reference runtime execution", "error", err)
		}
		if processed {
			slog.Info("reference runtime completed one assignment", "runtimeInstanceId", *runtimeInstanceID)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
