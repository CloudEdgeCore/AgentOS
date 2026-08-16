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

	"github.com/bian-cloud-skill/agentos/cmd/mtlsutil"
	runtimev1alpha1 "github.com/bian-cloud-skill/agentos/gen/go/agentos/runtime/v1alpha1"
	"github.com/bian-cloud-skill/agentos/internal/platform/artifact"
	"github.com/bian-cloud-skill/agentos/internal/platform/grpcx"
	"github.com/bian-cloud-skill/agentos/internal/platform/otel"
	"github.com/bian-cloud-skill/agentos/internal/runtime/oci"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	controlAddress := flag.String("control-address", "127.0.0.1:9090", "Runtime Protocol control endpoint")
	tenantID := flag.String("tenant", "", "development tenant")
	runtimeInstanceID := flag.String("runtime-instance-id", "", "assigned runtime instance ID")
	artifactRoot := flag.String("artifact-root", "", "development content-addressed artifact directory")
	imageRef := flag.String("image-ref", "", "digest-pinned OCI image that runs the published AgentVersion")
	namespace := flag.String("containerd-namespace", "agentos", "containerd namespace for sandboxed containers")
	runtimeName := flag.String("containerd-runtime", "io.containerd.runsc.v1", "containerd runtime (runsc by default)")
	skipImagePull := flag.Bool("skip-image-pull", false, "assume the image is already loaded into containerd")
	cpuQuotaMillis := flag.Int64("cpu-quota-millis", 0, "cgroup CPU quota in milliseconds (0 = Admission decides)")
	memoryLimitMiB := flag.Int64("memory-limit-mib", 0, "cgroup memory limit in MiB (0 = Admission decides)")
	workspaceBytes := flag.Int64("workspace-bytes", 0, "tmpfs workspace size in bytes (0 = Admission decides)")
	pollInterval := flag.Duration("poll-interval", 250*time.Millisecond, "assignment polling interval")
	heartbeatTTL := flag.Duration("heartbeat-ttl", 30*time.Second, "requested runtime lease TTL")
	devMode := flag.Bool("dev-mode", false, "acknowledge the v0.1 engineering-baseline provider (not a hardened sandbox)")
	tlsCert := flag.String("tls-cert", "", "worker X.509-SVID certificate (PEM)")
	tlsKey := flag.String("tls-key", "", "worker X.509-SVID private key (PEM)")
	trustBundle := flag.String("trust-bundle", "", "SPIFFE trust bundle (PEM CA certificates)")
	flag.Parse()
	if strings.TrimSpace(*tenantID) == "" || strings.TrimSpace(*runtimeInstanceID) == "" || strings.TrimSpace(*artifactRoot) == "" ||
		strings.TrimSpace(*imageRef) == "" || *pollInterval <= 0 || *heartbeatTTL <= 0 || !*devMode {
		slog.Error("tenant, runtime instance, artifact root, digest-pinned image, positive intervals, and explicit -dev-mode are required")
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

	options := []oci.RunscOption{oci.WithNamespace(*namespace), oci.WithRuntime(*runtimeName)}
	if *skipImagePull {
		options = append(options, oci.WithSkipPull())
	}
	executor, err := oci.NewRunscExecutor(options...)
	if err != nil {
		slog.Error("create OCI/gVisor executor", "error", err)
		os.Exit(1)
	}
	tlsConfigured := *tlsCert != "" || *tlsKey != "" || *trustBundle != ""
	controlCredentials, err := mtlsutil.ClientCredentials(tlsConfigured, *tlsCert, *tlsKey, *trustBundle)
	if err != nil {
		slog.Error("configure worker mTLS credentials", "error", err)
		os.Exit(1)
	}
	if controlCredentials == nil {
		controlCredentials = insecure.NewCredentials()
		slog.Warn("Runtime Protocol client has NO transport identity (plaintext dev mode)")
	} else {
		slog.Info("Runtime Protocol client authenticating with X.509-SVID", "instance", *runtimeInstanceID)
	}
	connection, err := grpc.NewClient(*controlAddress,
		append([]grpc.DialOption{grpc.WithTransportCredentials(controlCredentials)}, grpcx.ClientOptions()...)...)
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
	worker := oci.NewWorker(runtimev1alpha1.NewRuntimeControlServiceClient(connection), artifactStore, executor,
		*tenantID, *runtimeInstanceID, *heartbeatTTL, *imageRef)
	worker.WithResourceLimits(*cpuQuotaMillis, *memoryLimitMiB, *workspaceBytes)
	ticker := time.NewTicker(*pollInterval)
	defer ticker.Stop()
	for {
		processed, err := worker.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			slog.Error("OCI/gVisor runtime execution", "error", err)
		}
		if processed {
			slog.Info("OCI/gVisor runtime completed one assignment", "runtimeInstanceId", *runtimeInstanceID)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
