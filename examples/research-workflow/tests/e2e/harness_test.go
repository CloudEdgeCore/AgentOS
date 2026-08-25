//go:build integration

// End-to-end harness for the multi-agent research workflow: it assembles the
// real kernel stack (gateway gRPC services, brokered MCP incl. the dynamic
// spawn service, runtime control, admission/scheduling/recovery/workflow
// controllers, adapter workers) plus the example's own surfaces (multi-role
// agent runtime, webhook tool endpoint) and drives complete research
// workflows against a scripted OpenAI-compatible provider.
package research_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	research "github.com/CloudEdgeCore/AgentOS/examples/research-workflow/runtime"
	webtools "github.com/CloudEdgeCore/AgentOS/examples/research-workflow/tools/webtools"
	gatewayv1 "github.com/CloudEdgeCore/AgentOS/gen/go/agentos/gateway/v1"
	modelv1 "github.com/CloudEdgeCore/AgentOS/gen/go/agentos/model/v1"
	runtimev1 "github.com/CloudEdgeCore/AgentOS/gen/go/agentos/runtime/v1"
	"github.com/CloudEdgeCore/AgentOS/internal/gateway"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/admission"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/capability"
	kernelmemory "github.com/CloudEdgeCore/AgentOS/internal/kernel/memory"
	kernelmodel "github.com/CloudEdgeCore/AgentOS/internal/kernel/model"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/model/provider"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/money"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/policy"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/recovery"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/scheduler"
	kernelstore "github.com/CloudEdgeCore/AgentOS/internal/kernel/store"
	postgresstore "github.com/CloudEdgeCore/AgentOS/internal/kernel/store/postgres"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/tool"
	workflowkernel "github.com/CloudEdgeCore/AgentOS/internal/kernel/workflow"
	"github.com/CloudEdgeCore/AgentOS/internal/mcp"
	"github.com/CloudEdgeCore/AgentOS/internal/platform/artifact"
	"github.com/CloudEdgeCore/AgentOS/internal/platform/migrate"
	runtimeadapter "github.com/CloudEdgeCore/AgentOS/internal/runtime/adapter"
	"github.com/CloudEdgeCore/AgentOS/internal/runtime/control"
	"github.com/CloudEdgeCore/AgentOS/internal/runtime/reference"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	researchTenant = "research-tenant"
	fakeModelRef   = "fake/research-model"
)

// scenario tunes the scripted provider per test.
type scenario struct {
	criticRound1NeedsMore bool
	writerBadCitations    bool
	criticAlwaysNeedsMore bool
	writerUnknownEvidence bool
}

type fetchCounter struct {
	mu    sync.Mutex
	count map[string]int
}

// responseRecorder captures status and body for diagnostics.
type responseRecorder struct {
	http.ResponseWriter
	status int
	body   strings.Builder
}

func (r *responseRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = 200
	}
	r.body.Write(data)
	return r.ResponseWriter.Write(data)
}

func (f *fetchCounter) add(url string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.count == nil {
		f.count = map[string]int{}
	}
	f.count[url]++
}

func (f *fetchCounter) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for _, count := range f.count {
		total += count
	}
	return total
}

type harness struct {
	t         *testing.T
	pool      *pgxpool.Pool
	store     *postgresstore.Store
	provider  *scriptedProvider
	webtools  *webtools.Server
	fetches   *fetchCounter
	cancelCtx context.CancelFunc
	loopWG    sync.WaitGroup
	mcpURL    string
	agentURL  string
	bindings  *runtimeadapter.RuntimeBindings
	registry  *mcp.ExecutionRegistry
	artifacts *artifact.Filesystem
	listener  net.Listener // control-plane listener, kept for extra workers
	loopCtx   context.Context
	workerMu  sync.Mutex
	workers   map[string]context.CancelFunc
	models    research.Models // logical model refs active for this run
	liveModel bool
	liveWeb   bool
	schema    string
}

// envOr returns the environment value or the fallback when unset/empty.
func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func mustUSD(t *testing.T, value string) float64 {
	t.Helper()
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		t.Fatalf("parse usd %q: %v", value, err)
	}
	return parsed
}

func newHarness(t *testing.T, name string, tune func(*scenario)) *harness {
	t.Helper()
	databaseURL := os.Getenv("AGENTOS_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AGENTOS_TEST_DATABASE_URL is not set")
	}
	normalizedName := strings.Map(func(character rune) rune {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '_' {
			return character
		}
		return '_'
	}, name)
	schema := "agentos_research_" + normalizedName
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	admin, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	if _, err := admin.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS `+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	_ = admin.Close(ctx)
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	config.MaxConns = 24
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	migrations, err := filepath.Abs(filepath.Join("..", "..", "..", "..", "db", "migrations"))
	if err != nil {
		t.Fatalf("migrations path: %v", err)
	}
	if _, err := migrate.Apply(ctx, pool, migrations); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE TABLE task_usage_reservations,
		runtime_capacity_reservations, runtime_pool_capacities, runtime_pool_tenant_grants, runtime_pools,
		provider_circuit_breakers, workflow_usage_ledgers,
		model_calls, model_descriptors, tool_approvals, tool_calls, tool_descriptors,
		runtime_operation_receipts, checkpoints, artifacts, memory_records,
		task_budget_settlements, task_budget_ledgers, agent_versions, inbox_receipts, outbox_events,
		runtime_leases, attempts, runs, tasks, workflows, workflow_steps RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	store := postgresstore.New(pool)

	scenarioState := &scenario{}
	if tune != nil {
		tune(scenarioState)
	}
	h := &harness{t: t, pool: pool, store: store, provider: &scriptedProvider{scenario: scenarioState}, schema: schema}

	// Live-mode detection (roadmap P0/P1): AGENTOS_RESEARCH_LIVE=1 routes
	// model calls to a real OpenAI-compatible provider; AGENTOS_RESEARCH_LIVE_WEB=1
	// swaps the deterministic corpus for real internet search + fetch.
	h.models = research.Models{Fast: fakeModelRef, Reader: fakeModelRef, Reasoning: fakeModelRef}
	liveModel := os.Getenv("AGENTOS_RESEARCH_LIVE") == "1"
	if liveModel {
		if strings.TrimSpace(os.Getenv("AGENTOS_RESEARCH_MODEL_BASE_URL")) == "" {
			t.Fatalf("AGENTOS_RESEARCH_LIVE=1 requires AGENTOS_RESEARCH_MODEL_BASE_URL")
		}
		h.liveModel = true
		h.models = research.Models{
			Fast:      "research/fast",
			Reader:    "research/reader",
			Reasoning: "research/reasoning",
		}
	}
	h.liveWeb = os.Getenv("AGENTOS_RESEARCH_LIVE_WEB") == "1"

	providerListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("provider listen: %v", err)
	}
	providerServer := &http.Server{Handler: h.provider}
	go func() { _ = providerServer.Serve(providerListener) }()
	t.Cleanup(func() { _ = providerServer.Close() })

	h.webtools = webtools.New(webtools.Corpus())
	if h.liveWeb {
		searchKey := os.Getenv("AGENTOS_RESEARCH_SEARCH_KEY")
		var search webtools.SearchProvider
		switch strings.ToLower(envOr("AGENTOS_RESEARCH_SEARCH_PROVIDER", "brave")) {
		case "doubao", "volcengine":
			search = &webtools.DoubaoSearch{APIKey: searchKey}
		case "brave":
			search = &webtools.BraveSearch{APIKey: searchKey}
		case "bing":
			search = &webtools.BingSearch{APIKey: searchKey}
		default:
			t.Fatalf("unknown AGENTOS_RESEARCH_SEARCH_PROVIDER %q (want doubao, brave or bing)",
				os.Getenv("AGENTOS_RESEARCH_SEARCH_PROVIDER"))
		}
		h.webtools = h.webtools.WithBackend(&webtools.CompositeBackend{
			SearchProvider: search,
			FetchProvider:  &webtools.LiveFetch{},
		})
	}
	toolListener, toolClient, toolEndpoint, err := webtools.SelfSignedTLSListener(h.webtools)
	if err != nil {
		t.Fatalf("webtools listener: %v", err)
	}
	toolServer := &http.Server{Handler: h.webtools}
	go func() { _ = toolServer.Serve(toolListener) }()
	t.Cleanup(func() { _ = toolListener.Close() })
	h.fetches = &fetchCounter{}
	h.webtools.CountFetches(h.fetches.add)

	policyEngine, err := policy.New(policy.TenantPolicies{researchTenant: {
		MaxPriority:  100,
		AllowedTools: []string{"web.search", "web.fetch"},
		AllowedModels: []string{
			h.models.Fast, h.models.Reader, h.models.Reasoning,
		},
	}})
	if err != nil {
		t.Fatalf("policy engine: %v", err)
	}
	webhookExecutor, err := gateway.NewWebhookExecutor(map[string]string{
		"web.search@1.0.0": toolEndpoint,
		"web.fetch@1.0.0":  toolEndpoint,
	}, toolClient)
	if err != nil {
		t.Fatalf("webhook executor: %v", err)
	}
	toolGateway := tool.NewGateway(policyEngine, store, store, store, webhookExecutor, &gateway.DevSecretBroker{})
	modelGateway := kernelmodel.NewGateway(policyEngine, store, store, store)
	providerRegistry := provider.NewRegistry()
	if err := providerRegistry.Register(provider.Config{
		Name: "fake", BaseURL: "http://" + providerListener.Addr().String(),
		APIKey: "research-provider-key", TimeoutMs: 30000, MaxAttempts: 2,
	}); err != nil {
		t.Fatalf("register provider: %v", err)
	}
	if h.liveModel {
		baseURL := strings.TrimSpace(os.Getenv("AGENTOS_RESEARCH_MODEL_BASE_URL"))
		providerName := envOr("AGENTOS_RESEARCH_MODEL_PROVIDER", "openai")
		if err := providerRegistry.Register(provider.Config{
			Name: providerName, BaseURL: baseURL, APIKey: os.Getenv("AGENTOS_RESEARCH_MODEL_KEY"),
			TimeoutMs: 180000, MaxAttempts: 2,
		}); err != nil {
			t.Fatalf("register live provider: %v", err)
		}
		routes := map[string]string{
			"research/fast":      envOr("AGENTOS_RESEARCH_MODEL_FAST", "gpt-4o-mini"),
			"research/reader":    envOr("AGENTOS_RESEARCH_MODEL_READER", envOr("AGENTOS_RESEARCH_MODEL_FAST", "gpt-4o-mini")),
			"research/reasoning": envOr("AGENTOS_RESEARCH_MODEL_REASONING", "gpt-4o"),
		}
		for route, wireModel := range routes {
			if err := providerRegistry.RegisterRoute(provider.Route{
				ModelRef: route, Provider: providerName, Model: wireModel,
			}); err != nil {
				t.Fatalf("register live route %s: %v", route, err)
			}
		}
	}
	authorizer, err := capability.NewAuthorizer(store)
	if err != nil {
		t.Fatalf("authorizer: %v", err)
	}

	gatewayListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("gateway listen: %v", err)
	}
	gatewayServer := grpc.NewServer(grpc.MaxRecvMsgSize(4<<20), grpc.MaxSendMsgSize(4<<20))
	gatewayv1.RegisterToolGatewayServiceServer(gatewayServer, gateway.NewService(toolGateway, researchTenant, authorizer))
	gatewayv1.RegisterMemoryGatewayServiceServer(gatewayServer, gateway.NewMemoryService(
		kernelmemory.NewGateway(kernelmemory.DevEmbedder{}, store), store, researchTenant, authorizer))
	modelv1.RegisterModelGatewayServiceServer(gatewayServer, gateway.NewModelService(modelGateway, researchTenant, authorizer))
	modelv1.RegisterModelInvocationServiceServer(gatewayServer, gateway.NewModelInvocationService(
		kernelmodel.NewInvoker(modelGateway, providerRegistry), researchTenant, authorizer))
	runtimev1.RegisterWorkflowSpawnServiceServer(gatewayServer, newSpawnFacade(store))
	go func() { _ = gatewayServer.Serve(gatewayListener) }()
	t.Cleanup(gatewayServer.Stop)

	gatewayConn, err := grpc.NewClient(gatewayListener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("gateway client: %v", err)
	}
	t.Cleanup(func() { _ = gatewayConn.Close() })

	controlListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("control listen: %v", err)
	}
	h.listener = controlListener
	controlServer := grpc.NewServer(grpc.MaxRecvMsgSize(4<<20), grpc.MaxSendMsgSize(4<<20))
	runtimev1.RegisterRuntimeControlServiceServer(controlServer, control.NewService(store, researchTenant, 2*time.Minute))
	go func() { _ = controlServer.Serve(controlListener) }()
	t.Cleanup(controlServer.Stop)

	artifacts, err := artifact.NewFilesystem(t.TempDir(), 64<<20)
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	h.artifacts = artifacts

	if _, err := store.RegisterModelDescriptor(ctx, kernelstore.RegisterModelDescriptorInput{
		TenantID: researchTenant, Provider: "fake", ModelName: "research-model", SupportsStreaming: true,
		InputPriceMicroUSDPerMillion: money.MustFromUSD(3), OutputPriceMicroUSDPerMillion: money.MustFromUSD(6), PriceRevision: "p1",
	}); err != nil {
		t.Fatalf("model descriptor: %v", err)
	}
	if h.liveModel {
		// Logical-tier descriptors: the gateway resolves the descriptor from
		// the logical ref ("research/fast" → provider=research model=fast).
		for tier, descriptor := range map[string]struct {
			input, output string
		}{
			"fast":      {"0.5", "1.5"},
			"reader":    {"0.5", "1.5"},
			"reasoning": {"3", "12"},
		} {
			if _, err := store.RegisterModelDescriptor(ctx, kernelstore.RegisterModelDescriptorInput{
				TenantID: researchTenant, Provider: "research", ModelName: tier, SupportsStreaming: true,
				InputPriceMicroUSDPerMillion:  money.MustFromUSD(mustUSD(t, descriptor.input)),
				OutputPriceMicroUSDPerMillion: money.MustFromUSD(mustUSD(t, descriptor.output)),
				PriceRevision:                 "live-p1",
			}); err != nil {
				t.Fatalf("live model descriptor %s: %v", tier, err)
			}
		}
	}
	for _, descriptor := range []struct {
		name    string
		pattern string
		params  string
	}{
		{"web.search", "web:search:*", `{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}},"required":["query"]}`},
		{"web.fetch", "web:fetch:*", `{"type":"object","properties":{"url":{"type":"string","format":"uri"}},"required":["url"]}`},
	} {
		if _, err := store.RegisterToolDescriptor(ctx, kernelstore.RegisterToolDescriptorInput{
			TenantID: researchTenant, Name: descriptor.name, Version: "1.0.0", SideEffectRisk: kernelstore.ToolRiskLow,
			Actions: []string{"invoke"}, ResourcePatterns: []string{descriptor.pattern},
			ParamsSchema: []byte(descriptor.params),
		}); err != nil {
			t.Fatalf("tool descriptor %s: %v", descriptor.name, err)
		}
	}

	registry := mcp.NewExecutionRegistry()
	h.registry = registry
	toolsAdapter := mcp.NewToolAdapter(reference.NewGrpcToolInvoker(gatewayv1.NewToolGatewayServiceClient(gatewayConn)), registry)
	broker := mcp.NewBroker(toolsAdapter,
		runtimeadapter.NewGrpcModelBroker(modelv1.NewModelInvocationServiceClient(gatewayConn)),
		runtimeadapter.NewGrpcMemoryBroker(gatewayv1.NewMemoryGatewayServiceClient(gatewayConn)),
		runtimeadapter.NewGrpcWorkflowSpawner(runtimev1.NewWorkflowSpawnServiceClient(gatewayConn)),
		registry)
	mcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("mcp listen: %v", err)
	}
	mcpHTTP := &http.Server{Handler: mcp.NewServer("research-e2e", "v1.1.0", broker)}
	go func() { _ = mcpHTTP.Serve(mcpListener) }()
	t.Cleanup(func() { _ = mcpListener.Close() })
	h.mcpURL = "http://" + mcpListener.Addr().String()

	agentListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("agent listen: %v", err)
	}
	agentHandler, err := newAgentHandler(h.mcpURL, h.models)
	if err != nil {
		t.Fatalf("agent host: %v", err)
	}
	observedAgent := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := &responseRecorder{ResponseWriter: w}
		agentHandler.ServeHTTP(recorder, r)
		if strings.HasSuffix(r.URL.Path, "/result") {
			fmt.Fprintf(os.Stderr, "[research-e2e] agent %s -> %d: %.160s\n", r.URL.Path, recorder.status, recorder.body.String())
			return
		}
		if recorder.status >= 400 {
			fmt.Fprintf(os.Stderr, "[research-e2e] agent %s -> %d: %.600s\n", r.URL.Path, recorder.status, recorder.body.String())
		}
	})
	go func() { _ = (&http.Server{Handler: observedAgent}).Serve(agentListener) }()
	t.Cleanup(func() { _ = agentListener.Close() })
	h.agentURL = "http://" + agentListener.Addr().String()

	bindingEntries := make([]runtimeadapter.RuntimeBinding, 0, len(agentRefs()))
	for _, ref := range agentRefs() {
		bindingEntries = append(bindingEntries, runtimeadapter.RuntimeBinding{
			AgentVersionRef: ref, Endpoint: h.agentURL,
		})
	}
	bindingsFile := filepath.Join(t.TempDir(), "bindings.json")
	encodedBindings, _ := json.Marshal(map[string]any{"bindings": bindingEntries})
	if err := os.WriteFile(bindingsFile, encodedBindings, 0o644); err != nil {
		t.Fatalf("write bindings: %v", err)
	}
	bindings, err := runtimeadapter.LoadRuntimeBindings(bindingsFile)
	if err != nil {
		t.Fatalf("load bindings: %v", err)
	}
	h.bindings = bindings

	h.publishAgents()

	loopCtx, cancel := context.WithCancel(context.Background())
	h.cancelCtx = cancel
	h.loopCtx = loopCtx
	instances := []string{"research-worker-a", "research-worker-b"}
	if os.Getenv("AGENTOS_RESEARCH_SCALE") == "1" {
		// The scale scenario needs real dispatch parallelism: one poller per
		// runtime instance, so widen the fleet under the scale gate.
		instances = instances[:0]
		for index := 0; index < 8; index++ {
			instances = append(instances, fmt.Sprintf("research-worker-%02d", index))
		}
	}
	pools := make(staticPools, 0, len(instances))
	for index, instance := range instances {
		pools = append(pools, scheduler.RuntimePool{
			ID:        fmt.Sprintf("pool-research-%c", 'a'+rune(index%26)) + fmt.Sprintf("%02d", index),
			TenantIDs: []string{researchTenant}, RuntimeClass: "adapter",
			RuntimeInstanceID: instance, Region: "cn-east", DataResidency: "cn", Ready: true,
			AvailableCPU: 8000, AvailableMemory: 16384, AvailableLLMSlots: 32,
		})
	}
	admissionController := admission.NewController(store, admission.New(admission.Limits{
		RuntimeClasses: []string{"adapter"}, MaxTokens: 2000000, MaxCostMicroUSD: money.MustFromUSD(5000),
		MaxToolCalls: 100000, MaxWallSeconds: 36000, MaxCPU: 16000, MaxMemory: 32768, MaxLLMConcurrency: 64,
	}), policyEngine, "research-admission", 50, time.Minute)
	schedulerController := scheduler.NewController(store, pools, "research-scheduler", 50, time.Minute, 2*time.Minute)
	if err := store.RegisterRuntimePools(loopCtx, pools); err != nil {
		t.Fatalf("register pools: %v", err)
	}
	workflowController := workflowkernel.NewController(store, store, artifacts, "research-orchestrator", 100)
	recoveryController := recovery.NewController(store, 128, 30*time.Second)
	h.loopWG.Add(1)
	go func() {
		defer h.loopWG.Done()
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		logf := func(stage string, err error) {
			if err != nil {
				fmt.Fprintf(os.Stderr, "[research-e2e] %s reconcile: %v\n", stage, err)
			}
		}
		logged := map[string]bool{}
		logOnce := func(stage string, err error) {
			if err != nil && !logged[stage+"|"+err.Error()] {
				logged[stage+"|"+err.Error()] = true
				logf(stage, err)
			}
		}
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
				_, err := admissionController.Reconcile(loopCtx)
				logOnce("admission", err)
				_, err = schedulerController.Reconcile(loopCtx)
				logOnce("scheduler", err)
				_, err = workflowController.Reconcile(loopCtx)
				logOnce("workflow", err)
				_, err = recoveryController.Reconcile(loopCtx)
				logOnce("recovery", err)
			}
		}
	}()
	for _, instance := range instances {
		h.startWorker(loopCtx, instance)
	}
	t.Cleanup(func() {
		cancel()
		h.loopWG.Wait()
	})
	return h
}

func (h *harness) startWorker(loopCtx context.Context, instance string) {
	h.t.Helper()
	controlConn, err := grpc.NewClient(h.listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		h.t.Fatalf("control client: %v", err)
	}
	worker, err := runtimeadapter.NewWorker(runtimev1.NewRuntimeControlServiceClient(controlConn),
		h.artifacts, "", researchTenant, instance, 15*time.Second, nil)
	if err != nil {
		h.t.Fatalf("worker %s: %v", instance, err)
	}
	worker = worker.WithExecutionWindow(h.registry).WithRuntimeBindings(h.bindings)
	workerCtx, cancelWorker := context.WithCancel(loopCtx)
	h.workerMu.Lock()
	if h.workers == nil {
		h.workers = map[string]context.CancelFunc{}
	}
	if previous, exists := h.workers[instance]; exists {
		previous() // a live replacement replaces the crashed loop
	}
	h.workers[instance] = cancelWorker
	h.workerMu.Unlock()
	h.loopWG.Add(1)
	go func() {
		defer h.loopWG.Done()
		defer func() { _ = controlConn.Close() }()
		for {
			processed, err := worker.RunOnce(workerCtx)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[research-e2e] worker %s: %v\n", instance, err)
			}
			if !processed {
				if workerCtx.Err() != nil {
					return
				}
				select {
				case <-workerCtx.Done():
					return
				case <-time.After(15 * time.Millisecond):
				}
			}
		}
	}()
}

// KillWorker simulates a runtime crash: the poll loop stops dead without
// releasing its leases, so recovery must reclaim them after expiry.
func (h *harness) KillWorker(instance string) {
	h.workerMu.Lock()
	cancel, ok := h.workers[instance]
	h.workerMu.Unlock()
	if ok {
		cancel()
	}
}
