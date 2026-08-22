package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/CloudEdgeCore/AgentOS/cmd/mtlsutil"
	gatewayv1 "github.com/CloudEdgeCore/AgentOS/gen/go/agentos/gateway/v1"
	modelv1 "github.com/CloudEdgeCore/AgentOS/gen/go/agentos/model/v1"
	runtimev1 "github.com/CloudEdgeCore/AgentOS/gen/go/agentos/runtime/v1"
	"github.com/CloudEdgeCore/AgentOS/internal/mcp"
	"github.com/CloudEdgeCore/AgentOS/internal/platform/artifact"
	"github.com/CloudEdgeCore/AgentOS/internal/platform/grpcx"
	"github.com/CloudEdgeCore/AgentOS/internal/platform/otel"
	runtimeadapter "github.com/CloudEdgeCore/AgentOS/internal/runtime/adapter"
	"github.com/CloudEdgeCore/AgentOS/internal/runtime/reference"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	controlAddress := flag.String("control-address", "127.0.0.1:9090", "Runtime Protocol control endpoint")
	endpoint := flag.String("adapter-endpoint", "", "loopback Agent Runtime Interface endpoint")
	tenantID := flag.String("tenant", "", "tenant assigned to this worker")
	runtimeInstanceID := flag.String("runtime-instance-id", "", "assigned runtime instance ID")
	artifactRoot := flag.String("artifact-root", "", "content-addressed artifact directory")
	mcpListen := flag.String("mcp-listen", "", "optional loopback address for the sandbox Agent MCP endpoint (e.g. 127.0.0.1:9093)")
	gatewayAddress := flag.String("gateway-address", "", "Gateway Protocol address for brokered model/memory/tools (required by -mcp-listen)")
	pollInterval := flag.Duration("poll-interval", 250*time.Millisecond, "assignment polling interval")
	heartbeatTTL := flag.Duration("heartbeat-ttl", 30*time.Second, "requested runtime lease TTL")
	tlsCert := flag.String("tls-cert", "", "worker X.509-SVID certificate")
	tlsKey := flag.String("tls-key", "", "worker X.509-SVID private key")
	trustBundle := flag.String("trust-bundle", "", "SPIFFE trust bundle")
	devMode := flag.Bool("dev-mode", false, "allow plaintext Runtime Protocol for local development")
	spawnAddress := flag.String("spawn-address", "", "orchestrator WorkflowSpawnService address (dynamic step spawn; empty disables)")
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
		for name, address := range map[string]string{"control": *controlAddress, "gateway": *gatewayAddress, "spawn": *spawnAddress, "mcp": *mcpListen} {
			if strings.TrimSpace(address) != "" && !loopbackRPCAddress(address) {
				slog.Error("plaintext development endpoint must be loopback-only", "endpoint", name, "address", address)
				os.Exit(2)
			}
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
	if strings.TrimSpace(*mcpListen) != "" {
		if strings.TrimSpace(*gatewayAddress) == "" {
			slog.Error("-mcp-listen requires -gateway-address so the sandbox Agent reaches the gateway services")
			os.Exit(2)
		}
		gatewayConnection, dialErr := grpc.NewClient(*gatewayAddress,
			append([]grpc.DialOption{grpc.WithTransportCredentials(credentials)}, grpcx.ClientOptions()...)...)
		if dialErr != nil {
			slog.Error("connect to Gateway Protocol", "error", dialErr)
			os.Exit(1)
		}
		defer gatewayConnection.Close()
		slot := mcp.NewExecutionRegistry()
		tools := mcp.NewToolAdapter(reference.NewGrpcToolInvoker(gatewayv1.NewToolGatewayServiceClient(gatewayConnection)), slot)
		var spawner mcp.WorkflowSpawner
		if strings.TrimSpace(*spawnAddress) != "" {
			spawnConnection, dialErr := grpc.NewClient(*spawnAddress,
				append([]grpc.DialOption{grpc.WithTransportCredentials(credentials)}, grpcx.ClientOptions()...)...)
			if dialErr != nil {
				slog.Error("connect to orchestrator spawn service", "error", dialErr)
				os.Exit(1)
			}
			defer spawnConnection.Close()
			spawner = runtimeadapter.NewGrpcWorkflowSpawner(runtimev1.NewWorkflowSpawnServiceClient(spawnConnection))
		}
		broker := mcp.NewBroker(tools,
			runtimeadapter.NewGrpcModelBroker(modelv1.NewModelInvocationServiceClient(gatewayConnection)),
			runtimeadapter.NewGrpcMemoryBroker(gatewayv1.NewMemoryGatewayServiceClient(gatewayConnection)),
			spawner, slot)
		worker.WithExecutionWindow(slot)
		mcpServer := mcp.NewServer("agentos-adapter", "v1.1.0", broker)
		listener, listenErr := net.Listen("tcp", *mcpListen)
		if listenErr != nil {
			slog.Error("listen for sandbox Agent MCP endpoint", "error", listenErr)
			os.Exit(1)
		}
		httpServer := &http.Server{
			Handler: mcpServer,
			// ReadTimeout bounds slow body reads (Slowloris); the MCP SSE
			// long-poll writes are bounded by the handler concurrency cap.
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 10 * time.Minute,
			IdleTimeout:  60 * time.Second,
		}
		go func() {
			if serveErr := httpServer.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				slog.Error("sandbox Agent MCP endpoint", "error", serveErr)
			}
		}()
		defer func() { _ = httpServer.Close() }()
		slog.Info("sandbox Agent MCP endpoint listening", "address", *mcpListen)
	}
	idleDelay := *pollInterval
	const maxIdlePoll = 5 * time.Second
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		processed, err := worker.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			slog.Error("adapter runtime execution", "error", err)
		}
		if processed {
			slog.Info("adapter runtime completed assignment", "runtimeInstanceId", *runtimeInstanceID)
			idleDelay = *pollInterval
			timer.Reset(0)
			continue
		}
		if err != nil {
			idleDelay = *pollInterval
		} else if idleDelay < maxIdlePoll {
			idleDelay *= 2
			if idleDelay > maxIdlePoll {
				idleDelay = maxIdlePoll
			}
		}
		timer.Reset(idleDelay)
	}
}

func loopbackRPCAddress(address string) bool {
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
