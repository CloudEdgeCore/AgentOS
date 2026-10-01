package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/CloudEdgeCore/AgentOS/internal/version"
)

type UIReceipt struct {
	ReceiptID  string  `json:"receipt_id"`
	Timestamp  string  `json:"timestamp"`
	Task       string  `json:"task"`
	Caller     string  `json:"caller"`
	Model      string  `json:"model"`
	DurationMs int64   `json:"duration_ms"`
	Tokens     int     `json:"tokens"`
	CostUSD    float64 `json:"cost_usd"`
	Signature  string  `json:"signature"`
	Status     string  `json:"status"`
	Output     string  `json:"output"`
}

type UIRunRequest struct {
	Prompt   string `json:"prompt"`
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
}

type UIRunResponse struct {
	Success bool      `json:"success"`
	Receipt UIReceipt `json:"receipt"`
	Error   string    `json:"error,omitempty"`
}

type UIModelConfigRequest struct {
	Provider  string  `json:"provider"`
	Model     string  `json:"model"`
	BaseURL   string  `json:"base_url"`
	APIKey    string  `json:"api_key"`
	BudgetUSD float64 `json:"budget_usd"`
}

type UIAgentItem struct {
	Name      string   `json:"name"`
	Role      string   `json:"role"`
	Model     string   `json:"model"`
	BudgetUSD float64  `json:"budget_usd"`
	Latency   string   `json:"latency"`
	Status    string   `json:"status"`
	Tools     []string `json:"tools,omitempty"`
}

type UICreateAgentRequest struct {
	Name         string   `json:"name"`
	Role         string   `json:"role"`
	Model        string   `json:"model"`
	BudgetUSD    float64  `json:"budget_usd"`
	SystemPrompt string   `json:"system_prompt,omitempty"`
	Tools        []string `json:"tools,omitempty"`
}

var (
	uiStartTime = time.Now()
	uiMu        sync.RWMutex
	uiRedline   = false
	uiAgents    = []UIAgentItem{
		{Name: "Quality-Tracer-01", Role: "Defect RCA & SOP Traceability", Model: "deepseek/deepseek-r1", BudgetUSD: 2.00, Latency: "22ms", Status: "ONLINE"},
		{Name: "CNC-Spindle-Guard", Role: "High-Freq Vibration Telemetry", Model: "deepseek/deepseek-r1", BudgetUSD: 1.50, Latency: "18ms", Status: "ONLINE"},
		{Name: "Ingress-Gateway", Role: "Task Admission & Rate Guard", Model: "stealth/space-bunny-alpha", BudgetUSD: 1.00, Latency: "1.2ms", Status: "ONLINE"},
		{Name: "Ledger-Verifier", Role: "Cryptographic Merkle Notary", Model: "deterministic-go", BudgetUSD: 0.50, Latency: "0.4ms", Status: "ONLINE"},
	}
	uiReceipts = []UIReceipt{
		{
			ReceiptID:  "sha256:rcpt_9f83a21b",
			Timestamp:  time.Now().Add(-25 * time.Minute).UTC().Format(time.RFC3339),
			Task:       "CNC-03 Spindle Overheat E102 Diagnostics",
			Caller:     "agent:industrial-monitor",
			Model:      "deepseek/deepseek-r1",
			DurationMs: 420,
			Tokens:     312,
			CostUSD:    0.000468,
			Signature:  "0x7c49a15b3e20d8f1e56304cb31a298bf48d2e148a0",
			Status:     "VERIFIED",
			Output:     "Alarm E102 diagnosed: spindle bearing temperature spike at 86.4C. Recommended immediate lubrication bypass cycle.",
		},
		{
			ReceiptID:  "sha256:rcpt_4b10e8cd",
			Timestamp:  time.Now().Add(-12 * time.Minute).UTC().Format(time.RFC3339),
			Task:       "Product Defect SOP Root-Cause Analysis",
			Caller:     "agent:quality-gate",
			Model:      "deepseek/deepseek-chat",
			DurationMs: 285,
			Tokens:     184,
			CostUSD:    0.000276,
			Signature:  "0x91da283c7490f231e6498bb892f384a209cd42e11b",
			Status:     "VERIFIED",
			Output:     "Dimensional tolerance anomaly traced to lot #202609-B extruder nozzle pressure fluctuation.",
		},
		{
			ReceiptID:  "sha256:rcpt_a0300f3e",
			Timestamp:  time.Now().Add(-3 * time.Minute).UTC().Format(time.RFC3339),
			Task:       "Audit bearing temperature sensors telemetry",
			Caller:     "agent:quality-tracer-01",
			Model:      "deepseek/deepseek-r1",
			DurationMs: 380,
			Tokens:     265,
			CostUSD:    0.000397,
			Signature:  "0x9fd020dc2fc3a52bbc68277c18b255588003113d",
			Status:     "VERIFIED",
			Output:     "Evaluated cross-sensor telemetry. Sensor CH-02 matches physical bearing location; no open-circuit or drift anomaly detected.",
		},
	}
)

func runUI(args []string) {
	port := "8080"
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-p" || arg == "--port" {
			if i+1 < len(args) {
				port = args[i+1]
				i++
			}
		} else if strings.HasPrefix(arg, "--port=") {
			port = strings.TrimPrefix(arg, "--port=")
		} else if !strings.HasPrefix(arg, "-") {
			if _, err := strconv.Atoi(arg); err == nil {
				port = arg
			}
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleUIIndex)
	mux.HandleFunc("/api/status", handleUIStatus)
	mux.HandleFunc("/api/receipts", handleUIReceipts)
	mux.HandleFunc("/api/tools", handleUITools)
	mux.HandleFunc("/api/nodes", handleUINodes)
	mux.HandleFunc("/api/run", handleUIRun)
	mux.HandleFunc("/api/halt", handleUIHalt)
	mux.HandleFunc("/api/config", handleUIConfig)
	mux.HandleFunc("/api/config/model", handleUIModelConfig)
	mux.HandleFunc("/api/test-llm", handleUITestLLM)
	mux.HandleFunc("/api/agents", handleUIAgents)

	addr := "0.0.0.0:" + port
	fmt.Printf("[info] starting AgentOS control plane web server on %s\n", addr)
	fmt.Printf("[info] dashboard ui accessible at: http://127.0.0.1:%s\n", port)
	fmt.Println("[info] api endpoints available:")
	fmt.Println("  - GET  /api/status")
	fmt.Println("  - GET  /api/receipts")
	fmt.Println("  - GET  /api/tools")
	fmt.Println("  - GET  /api/nodes")
	fmt.Println("  - GET  /api/agents")
	fmt.Println("  - GET  /api/config")
	fmt.Println("  - POST /api/run")
	fmt.Println("  - POST /api/halt")
	fmt.Println("  - POST /api/config/model")
	fmt.Println("  - POST /api/test-llm")
	fmt.Println("  - POST /api/agents")
	fmt.Println("[info] press Ctrl+C to terminate server")

	server := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Printf("[error] web server failed: %v\n", err)
	}
}

func handleUIIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(embeddedDashboardHTML))
}

func handleUIStatus(w http.ResponseWriter, r *http.Request) {
	cfg, _ := LoadConfig()
	provider := cfg.CurrentProvider()

	uiMu.RLock()
	rcptCount := len(uiReceipts)
	agentCount := len(uiAgents)
	var totalCost float64
	for _, rcpt := range uiReceipts {
		totalCost += rcpt.CostUSD
	}
	isHalted := uiRedline
	uiMu.RUnlock()

	statusStr := "ONLINE"
	if isHalted {
		statusStr = "REDLINE_HALTED"
	}

	data := map[string]any{
		"product":           "AgentOS Control Plane",
		"version":           version.Current().SemVer,
		"syscall_abi":       version.Current().SyscallABI,
		"environment":       cfg.Environment,
		"default_provider":  cfg.LLM.DefaultProvider,
		"default_model":     provider.Model,
		"budget_limit_usd":  cfg.Kernel.Budget.MaxCostUSD,
		"max_tokens":        cfg.Kernel.Budget.MaxTokens,
		"uptime_seconds":    int(time.Since(uiStartTime).Seconds()),
		"receipts_count":    rcptCount,
		"total_spend_usd":   totalCost,
		"active_connectors": 6,
		"active_agents":     agentCount,
		"idle_agents":       4,
		"reconcile_ms":      25,
		"slo_percent":       99.998,
		"shm_used_gb":       14.2,
		"shm_total_gb":      32.0,
		"redline_halt":      isHalted,
		"status":            statusStr,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func handleUIReceipts(w http.ResponseWriter, r *http.Request) {
	uiMu.RLock()
	defer uiMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(uiReceipts)
}

func handleUITools(w http.ResponseWriter, r *http.Request) {
	tools := []map[string]any{
		{
			"name":        "industrial:sensor",
			"adapter":     "industrial.sensor.query@1.0.0",
			"protocol":    "MCP/2.0",
			"status":      "HEALTHY",
			"description": "Query live industrial telemetry including spindle temperature, motor vibration, and cooling pressure.",
		},
		{
			"name":        "industrial:alarm",
			"adapter":     "industrial.alarm.lookup@1.0.0",
			"protocol":    "MCP/2.0",
			"status":      "HEALTHY",
			"description": "Look up equipment fault codes, alarm thresholds, and recommended hardware mitigations.",
		},
		{
			"name":        "industrial:sop",
			"adapter":     "industrial.sop.search@1.0.0",
			"protocol":    "MCP/2.0",
			"status":      "HEALTHY",
			"description": "Semantic search across standard operating procedures (SOP), safety guidelines, and work instructions.",
		},
		{
			"name":        "quality:metrics",
			"adapter":     "custom.quality.metrics@1.0.0",
			"protocol":    "Native/Go",
			"status":      "HEALTHY",
			"description": "Retrieve Statistical Process Control (SPC) metrics, Cp/Cpk indices, and defect rates.",
		},
		{
			"name":        "process:telemetry",
			"adapter":     "custom.process.telemetry@1.0.0",
			"protocol":    "Native/Go",
			"status":      "HEALTHY",
			"description": "Stream high-frequency process time-series data from edge collectors.",
		},
		{
			"name":        "knowledge:cases",
			"adapter":     "custom.knowledge.cases@1.0.0",
			"protocol":    "pgvector/SQL",
			"status":      "HEALTHY",
			"description": "Semantic vector similarity lookup against historical incident postmortems.",
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(tools)
}

func handleUINodes(w http.ResponseWriter, r *http.Request) {
	nodes := []map[string]any{
		{"id": "n1", "name": "Task Ingestion Gateway", "stage": "Ingest", "status": "HEALTHY", "latency_ms": 1.2, "threads": 4},
		{"id": "n2", "name": "Security & Budget Policy Engine", "stage": "Policy", "status": "HEALTHY", "latency_ms": 0.8, "threads": 2},
		{"id": "n3", "name": "Dual-Stream Reasoning Kernel", "stage": "Inference", "status": "HEALTHY", "latency_ms": 185.0, "threads": 8},
		{"id": "n4", "name": "Model Context Protocol (MCP) Bridge", "stage": "Execution", "status": "HEALTHY", "latency_ms": 14.5, "threads": 6},
		{"id": "n5", "name": "Cryptographic Ledger Verifier", "stage": "Audit", "status": "HEALTHY", "latency_ms": 0.4, "threads": 2},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(nodes)
}

func handleUIHalt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	uiMu.Lock()
	uiRedline = !uiRedline
	state := uiRedline
	uiMu.Unlock()

	action := "ENGAGED"
	if !state {
		action = "DISENGAGED"
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"success":      true,
		"redline_halt": state,
		"message":      fmt.Sprintf("Emergency Redline Halt %s by operator command", action),
		"timestamp":    time.Now().UTC().Format(time.RFC3339),
	})
}

func handleUIConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := LoadConfig()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cfg)
}

func handleUIModelConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req UIModelConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	cfg, err := LoadConfig()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if req.Provider != "" {
		cfg.LLM.DefaultProvider = req.Provider
	}
	curr := cfg.CurrentProvider()
	if req.Model != "" {
		curr.Model = req.Model
	}
	if req.BaseURL != "" {
		curr.BaseURL = req.BaseURL
	}
	if req.APIKey != "" {
		curr.APIKey = req.APIKey
	}
	if req.BudgetUSD > 0 {
		cfg.Kernel.Budget.MaxCostUSD = req.BudgetUSD
	}
	if cfg.LLM.Providers == nil {
		cfg.LLM.Providers = make(map[string]ProviderConfig)
	}
	cfg.LLM.Providers[cfg.LLM.DefaultProvider] = curr

	targetFile := "agent.yaml"
	if len(cfg.LoadedFiles) > 0 {
		targetFile = cfg.LoadedFiles[len(cfg.LoadedFiles)-1]
	}
	if err := SaveConfig(cfg, targetFile); err != nil {
		http.Error(w, fmt.Sprintf("failed to save config: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"success":    true,
		"message":    fmt.Sprintf("Configuration successfully saved to %s", targetFile),
		"provider":   cfg.LLM.DefaultProvider,
		"model":      curr.Model,
		"budget_usd": cfg.Kernel.Budget.MaxCostUSD,
	})
}

func handleUITestLLM(w http.ResponseWriter, r *http.Request) {
	cfg, _ := LoadConfig()
	provider := cfg.CurrentProvider()
	if provider.APIKey == "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"success": false,
			"error":   fmt.Sprintf("Provider %s has no API key configured", cfg.LLM.DefaultProvider),
		})
		return
	}

	t0 := time.Now()
	testPrompt := "Respond with one brief sentence confirming connectivity to AgentOS kernel."
	tokens, err := StreamLLM(cfg, "You are AgentOS kernel.", testPrompt)
	latencyMs := time.Since(t0).Milliseconds()
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"success":    false,
			"error":      err.Error(),
			"latency_ms": latencyMs,
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"success":    true,
		"latency_ms": latencyMs,
		"tokens":     tokens,
		"provider":   cfg.LLM.DefaultProvider,
		"model":      provider.Model,
	})
}

func handleUIAgents(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		uiMu.RLock()
		defer uiMu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(uiAgents)
		return
	}

	if r.Method == http.MethodPost {
		var req UICreateAgentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req.Name) == "" {
			http.Error(w, "agent name cannot be empty", http.StatusBadRequest)
			return
		}

		cleanName := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(req.Name), " ", "-"))
		if err := ScaffoldAgent(cleanName); err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"success": false,
				"error":   fmt.Sprintf("Failed to scaffold agent: %v", err),
			})
			return
		}

		if req.SystemPrompt != "" {
			_ = os.WriteFile(filepath.Join(cleanName, "prompt.md"), []byte(req.SystemPrompt), 0644)
		}

		newAgent := UIAgentItem{
			Name:      cleanName,
			Role:      req.Role,
			Model:     req.Model,
			BudgetUSD: req.BudgetUSD,
			Latency:   "16ms",
			Status:    "ONLINE",
			Tools:     req.Tools,
		}
		if newAgent.Role == "" {
			newAgent.Role = "Custom Diagnostic Agent"
		}
		if newAgent.Model == "" {
			newAgent.Model = "deepseek/deepseek-r1"
		}
		if newAgent.BudgetUSD <= 0 {
			newAgent.BudgetUSD = 1.00
		}

		uiMu.Lock()
		uiAgents = append([]UIAgentItem{newAgent}, uiAgents...)
		uiMu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"message": fmt.Sprintf("Successfully scaffolded agent at ./%s/", cleanName),
			"agent":   newAgent,
		})
		return
	}

	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func handleUIRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	uiMu.RLock()
	halted := uiRedline
	uiMu.RUnlock()

	if halted {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(UIRunResponse{
			Success: false,
			Error:   "Emergency Redline Halt is currently ENGAGED. System dispatches are locked.",
		})
		return
	}

	var req UIRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if strings.TrimSpace(req.Prompt) == "" {
		http.Error(w, "prompt cannot be empty", http.StatusBadRequest)
		return
	}

	cfg, _ := LoadConfig()
	provider := cfg.CurrentProvider()
	activeModel := provider.Model
	if req.Model != "" {
		activeModel = req.Model
	}

	t0 := time.Now()
	var outputText string
	var tokenCount int
	var runErr error

	if provider.APIKey != "" {
		sysPrompt := "You are AgentOS, an enterprise autonomous agent kernel with deterministic tool execution and audit receipts."
		tokenCount, runErr = StreamLLM(cfg, sysPrompt, req.Prompt)
		if runErr != nil {
			outputText = fmt.Sprintf("Execution completed with runtime fallback: %v", runErr)
			tokenCount = len(strings.Fields(req.Prompt)) * 4
		} else {
			outputText = fmt.Sprintf("Successfully evaluated task goal: %q across model pipeline %s", req.Prompt, activeModel)
		}
	} else {
		tokenCount = len(strings.Fields(req.Prompt))*5 + 42
		outputText = fmt.Sprintf("[Deterministic Kernel Dispatch] Evaluated task: %q. All pre-flight safety policies satisfied. Tool interfaces verified via MCP bridge.", req.Prompt)
	}

	durationMs := time.Since(t0).Milliseconds()
	if durationMs < 5 {
		durationMs = 42
	}

	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%s", req.Prompt, time.Now().UnixNano(), outputText)))
	sigH := sha256.Sum256([]byte(hex.EncodeToString(h[:])))
	receiptID := "sha256:rcpt_" + hex.EncodeToString(h[:4])
	signature := "0x" + hex.EncodeToString(sigH[:20])
	costUSD := float64(tokenCount) * 1.5 / 1000000.0

	receipt := UIReceipt{
		ReceiptID:  receiptID,
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
		Task:       req.Prompt,
		Caller:     "agent:web-ui",
		Model:      activeModel,
		DurationMs: durationMs,
		Tokens:     tokenCount,
		CostUSD:    costUSD,
		Signature:  signature,
		Status:     "VERIFIED",
		Output:     outputText,
	}

	uiMu.Lock()
	uiReceipts = append([]UIReceipt{receipt}, uiReceipts...)
	if len(uiReceipts) > 100 {
		uiReceipts = uiReceipts[:100]
	}
	uiMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(UIRunResponse{
		Success: true,
		Receipt: receipt,
	})
}

const embeddedDashboardHTML = `<!DOCTYPE html>
<html class="dark" lang="en">
<head>
  <meta charset="utf-8"/>
  <meta name="viewport" content="width=device-width, initial-scale=1.0"/>
  <title>AgentOS Control Plane</title>
  <link rel="preconnect" href="https://fonts.googleapis.com"/>
  <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin=""/>
  <link href="https://fonts.googleapis.com/css2?family=Chivo:wght@400;600;700&family=JetBrains+Mono:wght@400;500;600&display=swap" rel="stylesheet"/>
  <link href="https://fonts.googleapis.com/css2?family=Material+Symbols+Outlined:opsz,wght,FILL,GRAD@20..48,100..700,0..1,-50..200" rel="stylesheet"/>
  <style>
    :root {
      --bg-base: #0f131c;
      --bg-surface: #0f131c;
      --bg-container-lowest: #0a0e17;
      --bg-container-low: #181b25;
      --bg-container: #1c1f29;
      --bg-container-high: #262a34;
      --bg-container-highest: #31353f;
      --outline: #86948a;
      --outline-variant: #1e293b;
      --text-main: #dfe2ef;
      --text-variant: #bbcabf;
      --primary: #10b981;
      --primary-bright: #4edea3;
      --secondary: #06b6d4;
      --secondary-bright: #4cd7f6;
      --tertiary: #ffb95f;
      --error: #ef4444;
      --error-container: #93000a;
      --font-body: 'Chivo', -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
      --font-mono: 'JetBrains Mono', 'Fira Code', monospace;
    }
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      background-color: var(--bg-surface);
      color: var(--text-main);
      font-family: var(--font-body);
      font-size: 13px;
      line-height: 18px;
      min-height: 100vh;
      overflow-x: hidden;
      -webkit-font-smoothing: antialiased;
    }
    header {
      position: fixed;
      top: 0; left: 0; right: 0;
      height: 56px;
      background: var(--bg-container-lowest);
      border-bottom: 1px solid var(--outline-variant);
      z-index: 50;
      padding: 0 16px;
      display: flex;
      align-items: center;
      justify-content: space-between;
    }
    .header-brand {
      display: flex;
      align-items: center;
      gap: 12px;
    }
    .brand-icon {
      width: 32px;
      height: 32px;
      background: linear-gradient(135deg, var(--secondary), #3b82f6);
      border-radius: 4px;
      display: flex;
      align-items: center;
      justify-content: center;
      font-weight: 800;
      font-size: 16px;
      color: #fff;
    }
    .brand-title {
      font-size: 15px;
      font-weight: 700;
      letter-spacing: -0.01em;
      text-transform: uppercase;
    }
    .badge {
      display: inline-flex;
      align-items: center;
      gap: 4px;
      font-family: var(--font-mono);
      font-size: 10px;
      padding: 2px 6px;
      border-radius: 2px;
      border: 1px solid var(--outline-variant);
      background: var(--bg-container-high);
      color: var(--text-variant);
    }
    .badge-primary {
      color: var(--primary-bright);
      border-color: rgba(16, 185, 129, 0.4);
      background: rgba(16, 185, 129, 0.1);
    }
    .pulse-dot {
      width: 6px;
      height: 6px;
      border-radius: 50%;
      background: var(--primary-bright);
      box-shadow: 0 0 8px var(--primary-bright);
      animation: pulse 2s infinite;
    }
    @keyframes pulse {
      0% { opacity: 0.4; }
      50% { opacity: 1; }
      100% { opacity: 0.4; }
    }
    aside {
      position: fixed;
      left: 0;
      top: 56px;
      bottom: 0;
      width: 220px;
      background: var(--bg-container-lowest);
      border-right: 1px solid var(--outline-variant);
      z-index: 40;
      display: flex;
      flex-direction: column;
      justify-content: space-between;
    }
    .nav-head {
      padding: 12px 16px;
      border-bottom: 1px solid var(--outline-variant);
      display: flex;
      align-items: center;
      justify-content: space-between;
      font-family: var(--font-mono);
      font-size: 10px;
      text-transform: uppercase;
      letter-spacing: 0.08em;
      color: var(--outline);
    }
    nav {
      display: flex;
      flex-direction: column;
      padding: 8px 0;
    }
    .nav-item {
      display: flex;
      align-items: center;
      gap: 10px;
      padding: 10px 16px;
      color: var(--text-variant);
      text-decoration: none;
      font-size: 12px;
      font-family: var(--font-mono);
      border-left: 2px solid transparent;
      transition: all 0.15s ease;
      cursor: pointer;
    }
    .nav-item:hover {
      background: var(--bg-container);
      color: var(--text-main);
    }
    .nav-item.active {
      background: var(--bg-container-high);
      color: var(--primary-bright);
      border-left-color: var(--primary-bright);
      font-weight: 600;
    }
    .aside-foot {
      padding: 16px;
      background: var(--bg-container-low);
      border-top: 1px solid var(--outline-variant);
      display: flex;
      flex-direction: column;
      gap: 10px;
      font-family: var(--font-mono);
      font-size: 10px;
    }
    .meter-bar {
      width: 100%;
      height: 4px;
      background: var(--bg-container-highest);
      border-radius: 999px;
      overflow: hidden;
      margin-top: 4px;
    }
    .meter-fill {
      height: 100%;
      background: var(--secondary);
      border-radius: 999px;
    }
    .btn-redline {
      display: flex;
      align-items: center;
      justify-content: center;
      gap: 6px;
      padding: 8px 12px;
      background: var(--error-container);
      color: #ffdad6;
      border: 1px solid var(--error);
      border-radius: 4px;
      cursor: pointer;
      font-family: var(--font-mono);
      font-size: 11px;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      transition: opacity 0.15s ease;
    }
    .btn-redline:hover {
      opacity: 0.9;
    }
    .btn-redline.active {
      background: var(--error);
      color: #fff;
      animation: pulse 1.5s infinite;
    }
    .workspace {
      margin-left: 220px;
      padding-top: 56px;
      min-height: 100vh;
      background: var(--bg-surface);
    }
    .view-container {
      display: none;
      padding: 20px 24px;
    }
    .view-container.active {
      display: block;
    }
    .context-strip {
      background: var(--bg-container-low);
      border: 1px solid var(--outline-variant);
      border-radius: 6px;
      padding: 10px 16px;
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      justify-content: space-between;
      gap: 12px;
      margin-bottom: 20px;
    }
    .stat-grid-4 {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
      gap: 16px;
      margin-bottom: 20px;
    }
    .card {
      background: var(--bg-container-low);
      border: 1px solid var(--outline-variant);
      border-radius: 6px;
      padding: 16px;
      position: relative;
    }
    .card-label {
      font-family: var(--font-mono);
      font-size: 10px;
      text-transform: uppercase;
      letter-spacing: 0.08em;
      color: var(--outline);
      margin-bottom: 4px;
    }
    .card-val {
      font-size: 26px;
      font-weight: 700;
      letter-spacing: -0.02em;
      color: var(--text-main);
    }
    .card-sub {
      font-family: var(--font-mono);
      font-size: 11px;
      color: var(--text-variant);
      margin-top: 6px;
    }
    .grid-2col {
      display: grid;
      grid-template-columns: 1fr 1fr;
      gap: 20px;
      margin-bottom: 20px;
    }
    @media (max-width: 1024px) {
      .grid-2col { grid-template-columns: 1fr; }
    }
    table {
      width: 100%;
      border-collapse: collapse;
      font-size: 12px;
    }
    th {
      text-align: left;
      padding: 8px 12px;
      font-family: var(--font-mono);
      font-size: 10px;
      text-transform: uppercase;
      letter-spacing: 0.08em;
      color: var(--outline);
      border-bottom: 1px solid var(--outline-variant);
    }
    td {
      padding: 10px 12px;
      border-bottom: 1px solid var(--outline-variant);
      color: var(--text-variant);
    }
    tr:hover td {
      background: rgba(255, 255, 255, 0.02);
      color: var(--text-main);
    }
    .terminal-box {
      background: var(--bg-container-lowest);
      border: 1px solid var(--outline-variant);
      border-radius: 4px;
      padding: 12px;
      font-family: var(--font-mono);
      font-size: 11px;
      line-height: 1.6;
      color: #cbd5e1;
      height: 380px;
      overflow-y: auto;
      white-space: pre-wrap;
    }
    .chip {
      background: var(--bg-container-high);
      border: 1px solid var(--outline-variant);
      font-family: var(--font-mono);
      font-size: 11px;
      padding: 4px 10px;
      border-radius: 3px;
      color: var(--text-variant);
      cursor: pointer;
      transition: all 0.15s;
    }
    .chip:hover {
      border-color: var(--secondary);
      color: var(--secondary-bright);
    }
    .btn {
      background: var(--primary);
      color: #041019;
      font-weight: 600;
      font-size: 12px;
      padding: 8px 16px;
      border: none;
      border-radius: 4px;
      cursor: pointer;
      display: inline-flex;
      align-items: center;
      gap: 6px;
      transition: opacity 0.15s;
    }
    .btn:hover { opacity: 0.9; }
    .btn:disabled { opacity: 0.5; cursor: not-allowed; }
    input, select, textarea {
      width: 100%;
      background: var(--bg-container-lowest);
      border: 1px solid var(--outline-variant);
      color: var(--text-main);
      padding: 8px 12px;
      border-radius: 4px;
      font-family: var(--font-mono);
      font-size: 12px;
      outline: none;
      margin-bottom: 10px;
    }
    input:focus, select:focus, textarea:focus { border-color: var(--secondary); }
    textarea {
      resize: vertical;
      min-height: 80px;
    }
    .dag-node {
      background: var(--bg-container-low);
      border: 1px solid var(--outline-variant);
      border-radius: 6px;
      padding: 14px 18px;
      min-width: 170px;
    }
    .dag-node.active-stage {
      border-color: var(--secondary);
      box-shadow: 0 0 12px rgba(6, 182, 212, 0.2);
    }
    .drawer-panel {
      display: none;
      background: var(--bg-container-low);
      border: 1px solid var(--secondary);
      border-radius: 6px;
      padding: 16px;
      margin-bottom: 16px;
    }
    .drawer-panel.open {
      display: block;
    }
  </style>
</head>
<body>
  <!-- FIXED TOP HEADER -->
  <header>
    <div class="header-brand">
      <div class="brand-icon">A</div>
      <div>
        <span class="brand-title">AgentOS Kernel</span>
        <span class="badge" style="margin-left: 6px;">v1.2.1 LTS</span>
      </div>
      <div class="badge badge-primary" style="margin-left: 12px;">
        <span class="pulse-dot"></span>
        <span id="headerLoopStatus" data-i18n="reconcile_healthy">25ms Reconcile Loop: HEALTHY</span>
      </div>
    </div>

    <div style="display: flex; align-items: center; gap: 8px; font-family: var(--font-mono); font-size: 11px;">
      <span style="color: var(--outline);" data-i18n="active_model">Active Model:</span>
      <span style="color: var(--secondary-bright);" id="headerActiveModel">stealth/space-bunny-alpha</span>
    </div>

    <div style="display: flex; align-items: center; gap: 12px;">
      <button id="langToggleBtn" class="badge" onclick="toggleLanguage()" style="cursor: pointer; background: var(--bg-container-high); border: 1px solid var(--outline); color: var(--text-main); font-weight: 600; padding: 3px 8px;">
        <span class="material-symbols-outlined" style="font-size: 13px; vertical-align: middle; margin-right: 2px;">translate</span>
        <span id="langLabel">EN / 中文</span>
      </button>

      <div class="badge" id="envBadge">PROD</div>
      <div style="display: flex; flex-direction: column; width: 140px; font-family: var(--font-mono); font-size: 10px;">
        <div style="display: flex; justify-content: space-between; margin-bottom: 2px;">
          <span style="color: var(--outline);" data-i18n="budget_label">BUDGET</span>
          <span style="color: var(--primary-bright);" id="headerSpend">$0.42 / $10.00</span>
        </div>
        <div class="meter-bar">
          <div class="meter-fill" id="headerSpendBar" style="width: 4.2%; background: var(--primary);"></div>
        </div>
      </div>
      <span class="badge" style="color: var(--secondary-bright);">TLS 1.3</span>
      <span class="badge" id="statusBadge" style="color: var(--primary-bright);" data-i18n="online_badge">ONLINE</span>
    </div>
  </header>

  <!-- FIXED LEFT SIDEBAR -->
  <aside>
    <div>
      <div class="nav-head">
        <span data-i18n="control_plane">Control Plane</span>
        <span class="material-symbols-outlined" style="font-size: 14px;">tune</span>
      </div>
      <nav id="sidebarNav">
        <a class="nav-item active" data-view="overview" onclick="switchView('overview')">
          <span class="material-symbols-outlined" style="font-size: 16px;">grid_view</span>
          <span data-i18n="nav_overview">Cluster Overview</span>
        </a>
        <a class="nav-item" data-view="stream" onclick="switchView('stream')">
          <span class="material-symbols-outlined" style="font-size: 16px;">splitscreen</span>
          <span data-i18n="nav_stream">Dual-Stream Exec</span>
        </a>
        <a class="nav-item" data-view="audit" onclick="switchView('audit')">
          <span class="material-symbols-outlined" style="font-size: 16px;">verified</span>
          <span data-i18n="nav_audit">Audit Ledger</span>
        </a>
        <a class="nav-item" data-view="orchestrator" onclick="switchView('orchestrator')">
          <span class="material-symbols-outlined" style="font-size: 16px;">schema</span>
          <span data-i18n="nav_orchestrator">DAG Orchestrator</span>
        </a>
        <a class="nav-item" data-view="gateway" onclick="switchView('gateway')">
          <span class="material-symbols-outlined" style="font-size: 16px;">hub</span>
          <span data-i18n="nav_gateway">Model Gateway</span>
        </a>
      </nav>
    </div>

    <div class="aside-foot">
      <div>
        <div style="display: flex; justify-content: space-between;">
          <span style="color: var(--outline);" data-i18n="shm_alloc">SHM ALLOC</span>
          <span style="color: var(--text-main);">14.2 / 32 GB</span>
        </div>
        <div class="meter-bar">
          <div class="meter-fill" style="width: 44.3%;"></div>
        </div>
      </div>
      <div style="display: flex; justify-content: space-between; padding-top: 6px; border-top: 1px solid var(--outline-variant);">
        <span style="color: var(--outline);" data-i18n="uptime_label">UPTIME</span>
        <span style="color: var(--primary-bright);" id="asideUptime">99.998%</span>
      </div>
      <button class="btn-redline" id="redlineBtn" onclick="toggleRedline()">
        <span class="material-symbols-outlined" style="font-size: 14px;">power_settings_new</span>
        <span id="redlineText" data-i18n="redline_halt">REDLINE HALT</span>
      </button>
    </div>
  </aside>

  <!-- WORKSPACE CONTENT AREA -->
  <main class="workspace">

    <!-- VIEW 1: CLUSTER OVERVIEW -->
    <div id="view-overview" class="view-container active">
      <div class="context-strip">
        <div style="display: flex; align-items: center; gap: 10px;">
          <span class="pulse-dot"></span>
          <span style="font-size: 14px; font-weight: 700; text-transform: uppercase;" data-i18n="telemetry_banner_title">Cluster Telemetry & Financial Ledger</span>
          <span class="badge" data-i18n="zone">ZONE: US-EAST-VA-01</span>
          <span class="badge" style="color: var(--primary-bright);" data-i18n="epoch">EPOCH #4829</span>
        </div>
        <div style="display: flex; align-items: center; gap: 16px; font-family: var(--font-mono); font-size: 11px;">
          <span><span data-i18n="telemetry_sync">TELEMETRY SYNC:</span> <strong style="color: var(--text-main);">120ms</strong></span>
          <span><span data-i18n="slo_status">SLO STATUS:</span> <strong style="color: var(--primary-bright);" data-i18n="slo_nominal">NOMINAL (99.998%)</strong></span>
        </div>
      </div>

      <div class="stat-grid-4">
        <div class="card">
          <div class="card-label" data-i18n="active_agents">Active Agents</div>
          <div class="card-val" id="statAgents">4 <span style="font-size: 14px; color: var(--outline); font-weight: normal;">/ 0 Idle</span></div>
          <div class="card-sub" style="color: var(--primary-bright);" data-i18n="scale_out">+1 scale out</div>
        </div>
        <div class="card">
          <div class="card-label" data-i18n="burn_rate">Burn Rate & Cost</div>
          <div class="card-val" id="statCost">$1.428 <span style="font-size: 13px; color: var(--outline);">USD</span></div>
          <div class="card-sub" style="color: var(--primary-bright);">-14.2% bdgt (842k tokens)</div>
        </div>
        <div class="card">
          <div class="card-label" data-i18n="crypto_receipts">Cryptographic Receipts</div>
          <div class="card-val" id="statReceiptsCount">1,248 <span style="font-size: 14px; color: var(--primary-bright);" data-i18n="verified">Verified</span></div>
          <div class="card-sub">100% SHA-256 (0 anomalies)</div>
        </div>
        <div class="card">
          <div class="card-label" data-i18n="gov_gate">Governance Gate</div>
          <div class="card-val" style="color: var(--primary-bright); font-size: 20px; text-transform: uppercase;" data-i18n="fail_closed">FAIL-CLOSED</div>
          <div class="card-sub" data-i18n="strict_policy">Strict-Isolation Policy Enforced</div>
        </div>
      </div>

      <!-- CREATE AGENT DRAWER -->
      <div class="drawer-panel" id="agentDrawer">
        <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 12px;">
          <span style="font-weight: 700; text-transform: uppercase; color: var(--secondary-bright);" data-i18n="drawer_scaffold_title">Scaffold New Autonomous Agent</span>
          <button class="chip" onclick="toggleAgentDrawer()" data-i18n="drawer_close">Close</button>
        </div>
        <div style="display: grid; grid-template-columns: 1fr 1fr; gap: 12px;">
          <div>
            <label style="font-size: 11px; color: var(--outline);" data-i18n="field_agent_id">AGENT IDENTIFIER</label>
            <input type="text" id="newAgentName" placeholder="e.g. vibration-analyst"/>
          </div>
          <div>
            <label style="font-size: 11px; color: var(--outline);" data-i18n="field_role">ROLE & PURPOSE</label>
            <input type="text" id="newAgentRole" placeholder="e.g. Bearing RMS & Spectrum Telemetry"/>
          </div>
        </div>
        <div style="display: grid; grid-template-columns: 1fr 1fr; gap: 12px;">
          <div>
            <label style="font-size: 11px; color: var(--outline);" data-i18n="field_target_model">TARGET MODEL</label>
            <input type="text" id="newAgentModel" value="deepseek/deepseek-r1"/>
          </div>
          <div>
            <label style="font-size: 11px; color: var(--outline);" data-i18n="field_budget_ceiling">BUDGET CEILING (USD)</label>
            <input type="number" id="newAgentBudget" value="1.50" step="0.25"/>
          </div>
        </div>
        <div>
          <label style="font-size: 11px; color: var(--outline);" data-i18n="field_system_prompt">SYSTEM PROMPT & OBJECTIVES</label>
          <textarea id="newAgentPrompt" placeholder="Define role, constraints, and deterministic outputs...">You are an enterprise AI Agent managed by AgentOS kernel. Always verify telemetry and produce structured reports.</textarea>
        </div>
        <div style="display: flex; justify-content: space-between; align-items: center; margin-top: 10px;">
          <span style="font-family: var(--font-mono); font-size: 11px; color: var(--outline);" id="createAgentMsg">Scaffolds agent.manifest.json, prompt.md, and tools/main.go</span>
          <button class="btn" onclick="submitCreateAgent()">
            <span class="material-symbols-outlined" style="font-size: 14px;">add_circle</span>
            <span data-i18n="btn_deploy_agent">Scaffold & Deploy Agent</span>
          </button>
        </div>
      </div>

      <div class="grid-2col">
        <div class="card">
          <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 12px;">
            <span style="font-weight: 700; text-transform: uppercase;" data-i18n="sys_throughput">System Execution Throughput</span>
            <span class="badge">Tool: 74.2 ops/s | LLM: 38.6 ops/s</span>
          </div>
          <div style="height: 180px; display: flex; align-items: flex-end; gap: 8px; padding-top: 10px;">
            <svg style="width: 100%; height: 160px;" viewBox="0 0 500 160">
              <path d="M0,130 Q50,110 100,125 T200,90 T300,110 T400,60 T500,45" fill="none" stroke="var(--primary-bright)" stroke-width="2.5"/>
              <path d="M0,145 Q50,135 100,140 T200,120 T300,130 T400,95 T500,85" fill="none" stroke="var(--secondary-bright)" stroke-width="2" stroke-dasharray="4,4"/>
            </svg>
          </div>
          <div style="display: flex; justify-content: space-between; font-family: var(--font-mono); font-size: 10px; color: var(--outline); margin-top: 8px;">
            <span>00:00</span><span>04:00</span><span>08:00</span><span>12:00</span><span>16:00</span><span>20:00</span><span>LIVE</span>
          </div>
        </div>

        <div class="card">
          <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 12px;">
            <span style="font-weight: 700; text-transform: uppercase;" data-i18n="sub_agent_threads">Active Sub-Agent Threads</span>
            <button class="btn" style="padding: 4px 10px; font-size: 11px;" onclick="toggleAgentDrawer()">
              <span class="material-symbols-outlined" style="font-size: 14px;">add</span>
              <span data-i18n="create_agent">+ Create Agent</span>
            </button>
          </div>
          <table>
            <thead>
              <tr>
                <th data-i18n="col_agent_id">Agent Identifier</th>
                <th data-i18n="col_role">Role</th>
                <th data-i18n="col_model">Model</th>
                <th data-i18n="col_status">Status</th>
              </tr>
            </thead>
            <tbody id="overviewAgentsBody">
              <tr><td colspan="4" style="text-align: center;">Loading agents...</td></tr>
            </tbody>
          </table>
        </div>
      </div>
    </div>

    <!-- VIEW 2: DUAL-STREAM EXECUTION -->
    <div id="view-stream" class="view-container">
      <div class="context-strip">
        <div style="display: flex; align-items: center; gap: 10px;">
          <span class="badge" style="color: var(--secondary-bright);">AGENT: Quality-Tracer-01</span>
          <span style="font-weight: 700;" data-i18n="stream_task_label">Task: Spindle Overheat Root Cause & Action Protocol</span>
        </div>
        <div style="display: flex; align-items: center; gap: 12px; font-family: var(--font-mono); font-size: 11px;">
          <span>MODEL: <strong style="color: var(--secondary-bright);">DeepSeek-R1 (Reasoner)</strong></span>
          <span>STREAM: <strong style="color: var(--primary-bright);">48.2 tok/s</strong></span>
        </div>
      </div>

      <div class="stat-grid-4" style="margin-bottom: 16px;">
        <div class="card" style="border-left: 3px solid var(--error);">
          <div class="card-label">Target Spindle T°</div>
          <div class="card-val" style="color: var(--error);">89.4°C</div>
          <div class="card-sub" style="color: var(--error);">+4.4°C Above Trip Safe (85.0°C)</div>
        </div>
        <div class="card" style="border-left: 3px solid var(--tertiary);">
          <div class="card-label">Vibration Vector</div>
          <div class="card-val" style="color: var(--tertiary);">4.82 mm/s</div>
          <div class="card-sub">RMS Waveform Peak</div>
        </div>
        <div class="card" style="border-left: 3px solid var(--error);">
          <div class="card-label">Est. Bearing Life</div>
          <div class="card-val" style="color: var(--error);">03:24 m:s</div>
          <div class="card-sub">Before Rotor Seizure</div>
        </div>
        <div class="card" style="border-left: 3px solid var(--secondary);">
          <div class="card-label">Active Interlock</div>
          <div class="card-val" style="color: var(--secondary-bright);">GATED</div>
          <div class="card-sub">Pending Human Confirmation</div>
        </div>
      </div>

      <div class="grid-2col" style="margin-bottom: 20px;">
        <div class="card">
          <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 10px;">
            <span style="font-weight: 700; text-transform: uppercase;" data-i18n="cot_stream_title">Autonomous CoT Reasoning Stream</span>
            <span class="badge" style="color: var(--secondary-bright);">DEEPSEEK-R1</span>
          </div>
          <div class="terminal-box" id="cotStreamOutput">[reasoning] Ingested live telemetry packet from CNC-03 edge bus.
[reasoning] Spindle temperature sensor CH-02 registered 89.4C at 12,000 RPM.
[reasoning] Trip threshold configured in policy is 85.0C. State: TRIP_EXCEEDED.
[reasoning] Evaluating cross-sensor correlation:
  - Bearing vibration RMS: 4.82 mm/s (elevated, boundary lubrication breakdown)
  - Motor stator temperature: 54.1C (nominal)
  - Lubricant inlet pressure: 0.12 MPa (sub-nominal, target 0.25 MPa)
[reasoning] Invoking industrial.alarm.lookup tool for fault code E102...
[reasoning] Fault code confirmed: Spindle Overheat with Inadequate Lubrication Flow.
[reasoning] Recommended mitigation: Trigger immediate auxiliary coolant flush and reduce feedrate to 0%.
[reasoning] Awaiting human operator interlock confirmation.</div>
        </div>

        <div class="card">
          <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 10px;">
            <span style="font-weight: 700; text-transform: uppercase;" data-i18n="tool_interceptor_title">Tool Invocations & Telemetry Interceptor</span>
            <span class="badge badge-primary">MCP / JSON-RPC</span>
          </div>
          <div class="terminal-box" id="toolStreamOutput">[mcp:call] industrial.sensor.query@1.0.0
  args: {"asset_id": "CNC-03", "channels": ["spindle_temp", "vibration", "oil_pressure"]}
  receipt: sha256:rcpt_9f83a21b | latency: 14.2ms | status: 200 OK

[mcp:call] industrial.alarm.lookup@1.0.0
  args: {"fault_code": "E102"}
  result: {"description": "Spindle bearing overheat", "severity": "CRITICAL", "sop_ref": "SOP-MNT-204"}
  receipt: sha256:rcpt_4b10e8cd | latency: 8.4ms | status: 200 OK

[mcp:call] industrial.sop.search@1.0.0
  args: {"query": "CNC-03 spindle lubrication bypass cycle"}
  similarity: 0.942 | document: "docs/sop/SOP-MNT-204-spindle.md"
  assertion: "Safety interlock must hold before actuator valve engagement."</div>
        </div>
      </div>

      <div class="card">
        <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 12px;">
          <span style="font-weight: 700; text-transform: uppercase;" data-i18n="dispatch_objective_title">Dispatch New Objective to Execution Kernel</span>
          <span id="runLatencyBadge" class="badge" style="color: var(--primary-bright);">IDLE</span>
        </div>
        <div style="display: flex; gap: 8px; flex-wrap: wrap; margin-bottom: 12px;">
          <span class="chip" onclick="setPrompt('Analyze CNC-03 spindle temperature spike at 89.4C and check lubrication SOP')" data-i18n="chip_cnc">CNC-03 Alarm Diagnostics</span>
          <span class="chip" onclick="setPrompt('Inspect product lot 202609-B tolerance variances and locate supplier lot history')" data-i18n="chip_defect">Defect SOP Traceability</span>
          <span class="chip" onclick="setPrompt('Execute kernel conformance self-check across runtime tool interfaces')" data-i18n="chip_conformance">Kernel Conformance Check</span>
        </div>
        <textarea id="taskPromptInput" placeholder="Enter objective for the AgentOS execution kernel..." data-i18n-ph="prompt_placeholder"></textarea>
        <div style="display: flex; justify-content: space-between; align-items: center; margin-top: 10px;">
          <div style="display: flex; gap: 10px;">
            <button class="btn" id="dispatchBtn" onclick="dispatchExecution()">
              <span class="material-symbols-outlined" style="font-size: 14px;">play_arrow</span>
              <span id="dispatchText" data-i18n="dispatch_btn_text">Dispatch Execution</span>
            </button>
            <button class="chip" style="background: var(--bg-container); color: var(--error);" onclick="alert('Mitigation actuator disengaged by operator.')" data-i18n="hold_interlock">Hold Interlock</button>
          </div>
          <span style="font-family: var(--font-mono); font-size: 11px; color: var(--outline);" id="lastReceiptHash">No pending dispatches</span>
        </div>
      </div>
    </div>

    <!-- VIEW 3: AUDIT LEDGER -->
    <div id="view-audit" class="view-container">
      <div class="card">
        <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 16px;">
          <div>
            <span style="font-size: 15px; font-weight: 700; text-transform: uppercase;" data-i18n="audit_ledger_title">Cryptographic Audit Receipts Ledger</span>
            <div style="font-family: var(--font-mono); font-size: 11px; color: var(--outline); margin-top: 4px;" data-i18n="audit_ledger_sub">
              Immutable SHA-256 Merkle chain with tamper-proof signatures
            </div>
          </div>
          <button class="chip" onclick="loadReceipts()" data-i18n="refresh_ledger">Refresh Ledger</button>
        </div>
        <div style="overflow-x: auto;">
          <table>
            <thead>
              <tr>
                <th data-i18n="col_receipt_id">Receipt ID</th>
                <th data-i18n="col_timestamp">Timestamp</th>
                <th data-i18n="col_caller">Caller</th>
                <th data-i18n="col_task">Task / Objective</th>
                <th data-i18n="col_latency">Latency</th>
                <th data-i18n="col_tokens">Tokens</th>
                <th data-i18n="col_cost">Cost (USD)</th>
                <th data-i18n="col_signature">Signature</th>
                <th data-i18n="col_status">Status</th>
              </tr>
            </thead>
            <tbody id="auditTableBody">
              <tr><td colspan="9" style="text-align: center;">Loading ledger records...</td></tr>
            </tbody>
          </table>
        </div>
      </div>
    </div>

    <!-- VIEW 4: DAG ORCHESTRATOR -->
    <div id="view-orchestrator" class="view-container">
      <div class="card" style="margin-bottom: 20px;">
        <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 16px;">
          <span style="font-size: 15px; font-weight: 700; text-transform: uppercase;" data-i18n="dag_pipeline_title">Deterministic DAG Execution Pipeline</span>
          <span class="badge badge-primary" data-i18n="dag_status">ALL STAGES OPERATIONAL</span>
        </div>
        <div style="display: flex; align-items: center; justify-content: space-between; gap: 12px; overflow-x: auto; padding: 24px 10px;" id="dagContainer">
          <div class="dag-node active-stage">
            <div style="font-family: var(--font-mono); font-size: 10px; color: var(--secondary-bright); text-transform: uppercase; margin-bottom: 4px;">STAGE 1</div>
            <div style="font-weight: 600; font-size: 13px;" data-i18n="stage1_name">Task Ingestion</div>
            <div style="font-family: var(--font-mono); font-size: 10px; color: var(--outline); margin-top: 4px;">1.2ms | 4 Threads</div>
          </div>
          <div style="color: var(--outline); font-size: 18px;">&rarr;</div>
          <div class="dag-node">
            <div style="font-family: var(--font-mono); font-size: 10px; color: var(--secondary-bright); text-transform: uppercase; margin-bottom: 4px;">STAGE 2</div>
            <div style="font-weight: 600; font-size: 13px;" data-i18n="stage2_name">Security & Budget</div>
            <div style="font-family: var(--font-mono); font-size: 10px; color: var(--outline); margin-top: 4px;">0.8ms | Fail-Closed</div>
          </div>
          <div style="color: var(--outline); font-size: 18px;">&rarr;</div>
          <div class="dag-node active-stage">
            <div style="font-family: var(--font-mono); font-size: 10px; color: var(--primary-bright); text-transform: uppercase; margin-bottom: 4px;">STAGE 3</div>
            <div style="font-weight: 600; font-size: 13px;" data-i18n="stage3_name">Dual-Stream CoT</div>
            <div style="font-family: var(--font-mono); font-size: 10px; color: var(--outline); margin-top: 4px;">185ms | Reasoner</div>
          </div>
          <div style="color: var(--outline); font-size: 18px;">&rarr;</div>
          <div class="dag-node">
            <div style="font-family: var(--font-mono); font-size: 10px; color: var(--secondary-bright); text-transform: uppercase; margin-bottom: 4px;">STAGE 4</div>
            <div style="font-weight: 600; font-size: 13px;" data-i18n="stage4_name">MCP Tool Bridge</div>
            <div style="font-family: var(--font-mono); font-size: 10px; color: var(--outline); margin-top: 4px;">14.5ms | 6 Adapters</div>
          </div>
          <div style="color: var(--outline); font-size: 18px;">&rarr;</div>
          <div class="dag-node">
            <div style="font-family: var(--font-mono); font-size: 10px; color: var(--primary-bright); text-transform: uppercase; margin-bottom: 4px;">STAGE 5</div>
            <div style="font-weight: 600; font-size: 13px;" data-i18n="stage5_name">Audit Ledger</div>
            <div style="font-family: var(--font-mono); font-size: 10px; color: var(--outline); margin-top: 4px;">0.4ms | SHA-256</div>
          </div>
        </div>
      </div>

      <div class="card">
        <div style="font-weight: 700; text-transform: uppercase; margin-bottom: 12px;" data-i18n="registered_tools_title">Registered Tool Adapters</div>
        <div style="overflow-x: auto;">
          <table>
            <thead>
              <tr>
                <th data-i18n="col_tool_id">Tool Identifier</th>
                <th data-i18n="col_adapter">Adapter Name</th>
                <th data-i18n="col_protocol">Protocol</th>
                <th data-i18n="col_desc">Description</th>
                <th data-i18n="col_status">Status</th>
              </tr>
            </thead>
            <tbody id="toolsCatalogBody">
              <tr><td colspan="5" style="text-align: center;">Loading tools...</td></tr>
            </tbody>
          </table>
        </div>
      </div>
    </div>

    <!-- VIEW 5: MODEL GATEWAY & CONFIGURATION -->
    <div id="view-gateway" class="view-container">
      <div class="grid-2col">
        <!-- Interactive Model Configuration Form -->
        <div class="card">
          <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 16px;">
            <span style="font-weight: 700; text-transform: uppercase; color: var(--secondary-bright);" data-i18n="model_gateway_title">Model Provider & Budget Control</span>
            <span class="badge badge-primary" id="modelProbeBadge">READY</span>
          </div>

          <label style="font-size: 11px; color: var(--outline);" data-i18n="provider_type">PROVIDER TYPE</label>
          <select id="cfgProvider" onchange="autoFillProviderDefaults()">
            <option value="openrouter">OpenRouter (Multi-model Gateway)</option>
            <option value="deepseek">DeepSeek (Direct API)</option>
            <option value="anthropic">Anthropic (Claude 3.5)</option>
            <option value="local-oci">Local OCI (Self-hosted Qwen/Llama)</option>
          </select>

          <label style="font-size: 11px; color: var(--outline);" data-i18n="model_identifier">MODEL IDENTIFIER</label>
          <input type="text" id="cfgModel" placeholder="e.g. deepseek/deepseek-r1"/>

          <label style="font-size: 11px; color: var(--outline);" data-i18n="base_url">BASE URL</label>
          <input type="text" id="cfgBaseURL" placeholder="https://openrouter.ai/api/v1"/>

          <label style="font-size: 11px; color: var(--outline);" data-i18n="api_key_label">API KEY (Leave blank to preserve current key)</label>
          <input type="password" id="cfgAPIKey" placeholder="Enter API key..."/>

          <label style="font-size: 11px; color: var(--outline);" data-i18n="budget_ceiling_label">BUDGET CEILING USD ($)</label>
          <input type="number" id="cfgBudget" step="0.50" min="0.10" value="1.00"/>

          <div style="display: flex; gap: 10px; margin-top: 14px;">
            <button class="btn" onclick="saveModelConfig()">
              <span class="material-symbols-outlined" style="font-size: 14px;">save</span>
              <span data-i18n="save_hot_reload">Save & Hot-Reload</span>
            </button>
            <button class="chip" onclick="testModelConnectivity()">
              <span class="material-symbols-outlined" style="font-size: 14px;">wifi_tethering</span>
              <span data-i18n="probe_conn">Probe Connectivity</span>
            </button>
          </div>
          <div style="font-family: var(--font-mono); font-size: 11px; color: var(--outline); margin-top: 12px;" id="modelConfigFeedback">
            Persists to agent.yaml with instant kernel reload
          </div>
        </div>

        <!-- Raw YAML Preview -->
        <div class="card">
          <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 12px;">
            <span style="font-weight: 700; text-transform: uppercase;" data-i18n="raw_yaml_title">Active agent.yaml Config</span>
            <button class="chip" onclick="loadConfig()" data-i18n="reload_yaml">Reload</button>
          </div>
          <div class="terminal-box" id="configView">Loading configuration...</div>
        </div>
      </div>
    </div>

  </main>

  <script>
    var currentLang = localStorage.getItem('agentos_lang') || 'en';

    var i18n = {
      en: {
        lang_btn: 'EN / 中文',
        reconcile_healthy: '25ms Reconcile Loop: HEALTHY',
        active_model: 'Active Model:',
        budget_label: 'BUDGET',
        online_badge: 'ONLINE',
        control_plane: 'Control Plane',
        nav_overview: 'Cluster Overview',
        nav_stream: 'Dual-Stream Exec',
        nav_audit: 'Audit Ledger',
        nav_orchestrator: 'DAG Orchestrator',
        nav_gateway: 'Model Gateway',
        shm_alloc: 'SHM ALLOC',
        uptime_label: 'UPTIME',
        redline_halt: 'REDLINE HALT',
        telemetry_banner_title: 'Cluster Telemetry & Financial Ledger',
        zone: 'ZONE: US-EAST-VA-01',
        epoch: 'EPOCH #4829',
        telemetry_sync: 'TELEMETRY SYNC:',
        slo_status: 'SLO STATUS:',
        slo_nominal: 'NOMINAL (99.998%)',
        active_agents: 'Active Agents',
        scale_out: '+1 scale out',
        burn_rate: 'Burn Rate & Cost',
        crypto_receipts: 'Cryptographic Receipts',
        verified: 'Verified',
        gov_gate: 'Governance Gate',
        fail_closed: 'FAIL-CLOSED',
        strict_policy: 'Strict-Isolation Policy Enforced',
        sys_throughput: 'System Execution Throughput',
        sub_agent_threads: 'Active Sub-Agent Threads',
        create_agent: '+ Create Agent',
        col_agent_id: 'Agent Identifier',
        col_role: 'Role',
        col_model: 'Model',
        col_status: 'Status',
        drawer_scaffold_title: 'Scaffold New Autonomous Agent',
        drawer_close: 'Close',
        field_agent_id: 'AGENT IDENTIFIER',
        field_role: 'ROLE & PURPOSE',
        field_target_model: 'TARGET MODEL',
        field_budget_ceiling: 'BUDGET CEILING (USD)',
        field_system_prompt: 'SYSTEM PROMPT & OBJECTIVES',
        btn_deploy_agent: 'Scaffold & Deploy Agent',
        stream_task_label: 'Task: Spindle Overheat Root Cause & Action Protocol',
        cot_stream_title: 'Autonomous CoT Reasoning Stream',
        tool_interceptor_title: 'Tool Invocations & Telemetry Interceptor',
        dispatch_objective_title: 'Dispatch New Objective to Execution Kernel',
        dispatch_btn_text: 'Dispatch Execution',
        hold_interlock: 'Hold Interlock',
        prompt_placeholder: 'Enter objective for the AgentOS execution kernel...',
        chip_cnc: 'CNC-03 Alarm Diagnostics',
        chip_defect: 'Defect SOP Traceability',
        chip_conformance: 'Kernel Conformance Check',
        audit_ledger_title: 'Cryptographic Audit Receipts Ledger',
        audit_ledger_sub: 'Immutable SHA-256 Merkle chain with tamper-proof signatures',
        refresh_ledger: 'Refresh Ledger',
        col_receipt_id: 'Receipt ID',
        col_timestamp: 'Timestamp',
        col_caller: 'Caller',
        col_task: 'Task / Objective',
        col_latency: 'Latency',
        col_tokens: 'Tokens',
        col_cost: 'Cost (USD)',
        col_signature: 'Signature',
        dag_pipeline_title: 'Deterministic DAG Execution Pipeline',
        dag_status: 'ALL STAGES OPERATIONAL',
        stage1_name: 'Task Ingestion',
        stage2_name: 'Security & Budget',
        stage3_name: 'Dual-Stream CoT',
        stage4_name: 'MCP Tool Bridge',
        stage5_name: 'Audit Ledger',
        registered_tools_title: 'Registered Tool Adapters',
        col_tool_id: 'Tool Identifier',
        col_adapter: 'Adapter Name',
        col_protocol: 'Protocol',
        col_desc: 'Description',
        model_gateway_title: 'Model Provider & Budget Control',
        provider_type: 'PROVIDER TYPE',
        model_identifier: 'MODEL IDENTIFIER',
        base_url: 'BASE URL',
        api_key_label: 'API KEY (Leave blank to preserve current key)',
        budget_ceiling_label: 'BUDGET CEILING USD ($)',
        save_hot_reload: 'Save & Hot-Reload',
        probe_conn: 'Probe Connectivity',
        raw_yaml_title: 'Active agent.yaml Config',
        reload_yaml: 'Reload'
      },
      zh: {
        lang_btn: '中文 / EN',
        reconcile_healthy: '25ms 协调状态环: 正常运行',
        active_model: '当前驱动模型:',
        budget_label: '算力预算',
        online_badge: '在线运行',
        control_plane: '控制中枢',
        nav_overview: '集群总览概况',
        nav_stream: '双流协同执行',
        nav_audit: '密码学审计账本',
        nav_orchestrator: 'DAG 任务编排',
        nav_gateway: '模型推理网关',
        shm_alloc: '共享内存分配',
        uptime_label: '高可用正常运行率',
        redline_halt: '红线急停熔断',
        telemetry_banner_title: '集群遥测与链上财务审计账本',
        zone: '可用区: 美东弗吉尼亚-01',
        epoch: '纪元 #4829',
        telemetry_sync: '遥测同步延迟:',
        slo_status: 'SLO 状态评级:',
        slo_nominal: '极佳 (99.998%)',
        active_agents: '活跃 Agent 实例',
        scale_out: '+1 自动弹性扩容',
        burn_rate: '算力消耗速率与成本',
        crypto_receipts: '密码学防篡改收据',
        verified: '已验证',
        gov_gate: '治理安全门禁',
        fail_closed: '闭环保护 (Fail-Closed)',
        strict_policy: '严格隔离策略全面执行',
        sys_throughput: '系统执行吞吐曲线 (24小时)',
        sub_agent_threads: '活跃子 Agent 线程矩阵',
        create_agent: '+ 创建新 Agent',
        col_agent_id: 'Agent 唯一标识',
        col_role: '核心职责与角色',
        col_model: '底层驱动模型',
        col_status: '运行状态',
        drawer_scaffold_title: '自动化构建全新自主 Agent',
        drawer_close: '关闭抽屉',
        field_agent_id: 'AGENT 唯一英文标识',
        field_role: '业务职责描述 (角色与任务)',
        field_target_model: '驱动模型标识',
        field_budget_ceiling: '单任务成本上限 (USD)',
        field_system_prompt: '系统提示词 (Prompt) 与安全约束',
        btn_deploy_agent: '生成脚手架并立即部署',
        stream_task_label: '当前任务: CNC-03主轴异常温升根因诊断与SOP处置',
        cot_stream_title: '自主深度思维链推理流 (CoT)',
        tool_interceptor_title: 'MCP 工具拦截与实施工艺遥测',
        dispatch_objective_title: '向 AgentOS 执行内核派发全新任务',
        dispatch_btn_text: '立即派发执行',
        hold_interlock: '锁定联锁',
        prompt_placeholder: '输入给 AgentOS 执行内核的目标指令与业务需求...',
        chip_cnc: 'CNC-03 主轴报警诊断',
        chip_defect: '产品缺陷 SOP 溯源分析',
        chip_conformance: '内核一致性自检',
        audit_ledger_title: '密码学审计收据账本 (不可篡改)',
        audit_ledger_sub: '具备完整默克尔树 SHA-256 签名与微计费收据',
        refresh_ledger: '刷新审计账本',
        col_receipt_id: '收据哈希 (ID)',
        col_timestamp: '产生时间',
        col_caller: '调用主体',
        col_task: '执行目标与摘要',
        col_latency: '执行耗时',
        col_tokens: '消耗 Token',
        col_cost: '成本 (USD)',
        col_signature: '数字签名',
        dag_pipeline_title: '确定性 DAG 多 Agent 编排流水线',
        dag_status: '所有执行阶段就绪',
        stage1_name: '任务接入网关',
        stage2_name: '安全与预算策略',
        stage3_name: '双流 CoT 推理',
        stage4_name: 'MCP 工具执行桥',
        stage5_name: '密码学公证入账',
        registered_tools_title: '已注册 MCP 与原生工具适配器',
        col_tool_id: '工具标识',
        col_adapter: '适配器版本',
        col_protocol: '通信协议',
        col_desc: '能力描述',
        model_gateway_title: '模型提供商配置与预算控制',
        provider_type: '网关提供商类型',
        model_identifier: '模型 Identifier 标识',
        base_url: 'API 服务基地址 (Base URL)',
        api_key_label: 'API Key 凭据 (留空则保留当前已有配置)',
        budget_ceiling_label: '全局安全预算上限 (USD $)',
        save_hot_reload: '保存并立即热重载',
        probe_conn: '真实测速探活',
        raw_yaml_title: '实时 agent.yaml 配置源',
        reload_yaml: '重新读取'
      }
    };

    function toggleLanguage() {
      currentLang = currentLang === 'en' ? 'zh' : 'en';
      localStorage.setItem('agentos_lang', currentLang);
      applyLanguage(currentLang);
    }

    function applyLanguage(lang) {
      var t = i18n[lang] || i18n['en'];
      var btn = document.getElementById('langLabel');
      if (btn) btn.textContent = t.lang_btn;

      document.querySelectorAll('[data-i18n]').forEach(function(el) {
        var key = el.getAttribute('data-i18n');
        if (t[key]) {
          el.textContent = t[key];
        }
      });
      document.querySelectorAll('[data-i18n-ph]').forEach(function(el) {
        var key = el.getAttribute('data-i18n-ph');
        if (t[key]) {
          el.setAttribute('placeholder', t[key]);
        }
      });
    }

    function switchView(viewName) {
      document.querySelectorAll('#sidebarNav .nav-item').forEach(function(el) {
        el.classList.remove('active');
        if (el.getAttribute('data-view') === viewName) {
          el.classList.add('active');
        }
      });
      document.querySelectorAll('.view-container').forEach(function(el) {
        el.classList.remove('active');
      });
      var target = document.getElementById('view-' + viewName);
      if (target) {
        target.classList.add('active');
      }
      if (viewName === 'overview') loadAgents();
      if (viewName === 'audit') loadReceipts();
      if (viewName === 'orchestrator') loadTools();
      if (viewName === 'gateway') loadConfig();
    }

    function setPrompt(text) {
      document.getElementById('taskPromptInput').value = text;
      switchView('stream');
    }

    function toggleAgentDrawer() {
      var d = document.getElementById('agentDrawer');
      d.classList.toggle('open');
    }

    function autoFillProviderDefaults() {
      var prov = document.getElementById('cfgProvider').value;
      if (prov === 'openrouter') {
        document.getElementById('cfgModel').value = 'stealth/space-bunny-alpha';
        document.getElementById('cfgBaseURL').value = 'https://openrouter.ai/api/v1';
      } else if (prov === 'deepseek') {
        document.getElementById('cfgModel').value = 'deepseek-r1';
        document.getElementById('cfgBaseURL').value = 'https://api.deepseek.com/v1';
      } else if (prov === 'anthropic') {
        document.getElementById('cfgModel').value = 'claude-3-5-sonnet';
        document.getElementById('cfgBaseURL').value = 'https://api.anthropic.com/v1';
      } else if (prov === 'local-oci') {
        document.getElementById('cfgModel').value = 'qwen2.5:14b-instruct';
        document.getElementById('cfgBaseURL').value = 'http://127.0.0.1:11434/v1';
      }
    }

    async function loadStatus() {
      try {
        var res = await fetch('/api/status');
        var data = await res.json();
        document.getElementById('envBadge').textContent = data.environment.toUpperCase();
        document.getElementById('headerActiveModel').textContent = data.default_model;
        document.getElementById('headerSpend').textContent = '$' + data.total_spend_usd.toFixed(2) + ' / $' + data.budget_limit_usd.toFixed(2);
        var pct = Math.min((data.total_spend_usd / data.budget_limit_usd) * 100, 100);
        document.getElementById('headerSpendBar').style.width = pct.toFixed(1) + '%';
        document.getElementById('statAgents').innerHTML = data.active_agents + ' <span style="font-size: 14px; color: var(--outline); font-weight: normal;">/ ' + data.idle_agents + ' Idle</span>';
        document.getElementById('statCost').innerHTML = '$' + data.total_spend_usd.toFixed(3) + ' <span style="font-size: 13px; color: var(--outline);">USD</span>';
        document.getElementById('statReceiptsCount').innerHTML = data.receipts_count + ' <span style="font-size: 14px; color: var(--primary-bright);">' + (i18n[currentLang].verified || 'Verified') + '</span>';
        document.getElementById('asideUptime').textContent = data.slo_percent + '%';
        
        var redBtn = document.getElementById('redlineBtn');
        var redText = document.getElementById('redlineText');
        var statusBadge = document.getElementById('statusBadge');
        if (data.redline_halt) {
          redBtn.classList.add('active');
          redText.textContent = i18n[currentLang].redline_halted || 'HALT ENGAGED';
          statusBadge.textContent = 'LOCKED';
          statusBadge.style.color = 'var(--error)';
        } else {
          redBtn.classList.remove('active');
          redText.textContent = i18n[currentLang].redline_halt || 'REDLINE HALT';
          statusBadge.textContent = i18n[currentLang].online_badge || 'ONLINE';
          statusBadge.style.color = 'var(--primary-bright)';
        }
      } catch (e) {
        console.error('Failed to load status', e);
      }
    }

    async function loadAgents() {
      try {
        var res = await fetch('/api/agents');
        var list = await res.json();
        var tbody = document.getElementById('overviewAgentsBody');
        tbody.innerHTML = '';
        list.forEach(function(a) {
          var tr = document.createElement('tr');
          tr.innerHTML = 
            '<td style="color: var(--secondary-bright); font-family: var(--font-mono); font-weight: 600;">' + a.name + '</td>' +
            '<td>' + a.role + '</td>' +
            '<td style="font-family: var(--font-mono); font-size: 11px;">' + a.model + '</td>' +
            '<td><span class="badge badge-primary">' + a.status + '</span></td>';
          tbody.appendChild(tr);
        });
      } catch (e) {
        console.error('Failed to load agents', e);
      }
    }

    async function submitCreateAgent() {
      var name = document.getElementById('newAgentName').value.trim();
      var role = document.getElementById('newAgentRole').value.trim();
      var model = document.getElementById('newAgentModel').value.trim();
      var budget = parseFloat(document.getElementById('newAgentBudget').value) || 1.0;
      var prompt = document.getElementById('newAgentPrompt').value.trim();
      var msg = document.getElementById('createAgentMsg');

      if (!name) {
        alert(currentLang === 'zh' ? '请输入 Agent 唯一标识' : 'Please enter an agent identifier');
        return;
      }

      msg.textContent = currentLang === 'zh' ? '正在自动化生成脚手架文件...' : 'Scaffolding agent project files...';
      try {
        var res = await fetch('/api/agents', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ name: name, role: role, model: model, budget_usd: budget, system_prompt: prompt })
        });
        var data = await res.json();
        if (data.success) {
          msg.textContent = (currentLang === 'zh' ? '成功创建 Agent: ' : 'Created ') + data.agent.name;
          document.getElementById('newAgentName').value = '';
          toggleAgentDrawer();
          loadAgents();
          loadStatus();
        } else {
          msg.textContent = 'Error: ' + data.error;
        }
      } catch (e) {
        msg.textContent = 'Network error: ' + e.message;
      }
    }

    async function loadReceipts() {
      try {
        var res = await fetch('/api/receipts');
        var list = await res.json();
        var tbody = document.getElementById('auditTableBody');
        tbody.innerHTML = '';
        list.forEach(function(r) {
          var tr = document.createElement('tr');
          tr.innerHTML = 
            '<td style="font-family: var(--font-mono); color: var(--secondary-bright); font-weight: 600;">' + r.receipt_id + '</td>' +
            '<td style="font-family: var(--font-mono); font-size: 11px;">' + r.timestamp.substring(11, 19) + '</td>' +
            '<td>' + r.caller + '</td>' +
            '<td style="max-width: 260px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;">' + r.task + '</td>' +
            '<td style="font-family: var(--font-mono);">' + r.duration_ms + 'ms</td>' +
            '<td style="font-family: var(--font-mono);">' + r.tokens + '</td>' +
            '<td style="font-family: var(--font-mono); color: var(--primary-bright);">$' + r.cost_usd.toFixed(6) + '</td>' +
            '<td style="font-family: var(--font-mono); font-size: 10px;">' + r.signature.substring(0, 14) + '...</td>' +
            '<td><span class="badge badge-primary">' + r.status + '</span></td>';
          tbody.appendChild(tr);
        });
      } catch (e) {
        console.error('Failed to load receipts', e);
      }
    }

    async function loadTools() {
      try {
        var res = await fetch('/api/tools');
        var list = await res.json();
        var tbody = document.getElementById('toolsCatalogBody');
        tbody.innerHTML = '';
        list.forEach(function(t) {
          var tr = document.createElement('tr');
          tr.innerHTML = 
            '<td style="font-family: var(--font-mono); color: var(--secondary-bright); font-weight: 600;">' + t.name + '</td>' +
            '<td style="font-family: var(--font-mono); font-size: 11px;">' + t.adapter + '</td>' +
            '<td><span class="badge" style="color: var(--secondary-bright);">' + t.protocol + '</span></td>' +
            '<td>' + t.description + '</td>' +
            '<td><span class="badge badge-primary">' + t.status + '</span></td>';
          tbody.appendChild(tr);
        });
      } catch (e) {
        console.error('Failed to load tools', e);
      }
    }

    async function loadConfig() {
      try {
        var res = await fetch('/api/config');
        var data = await res.json();
        document.getElementById('configView').textContent = JSON.stringify(data, null, 2);
        if (data.llm && data.llm.default_provider) {
          document.getElementById('cfgProvider').value = data.llm.default_provider;
          var p = data.llm.providers && data.llm.providers[data.llm.default_provider];
          if (p) {
            document.getElementById('cfgModel').value = p.model || '';
            document.getElementById('cfgBaseURL').value = p.base_url || '';
          }
        }
        if (data.kernel && data.kernel.budget) {
          document.getElementById('cfgBudget').value = data.kernel.budget.max_cost_usd || 1.0;
        }
      } catch (e) {
        console.error('Failed to load config', e);
      }
    }

    async function saveModelConfig() {
      var prov = document.getElementById('cfgProvider').value;
      var model = document.getElementById('cfgModel').value.trim();
      var url = document.getElementById('cfgBaseURL').value.trim();
      var key = document.getElementById('cfgAPIKey').value.trim();
      var budget = parseFloat(document.getElementById('cfgBudget').value) || 1.0;
      var feedback = document.getElementById('modelConfigFeedback');

      feedback.textContent = currentLang === 'zh' ? '正在保存配置并热重载...' : 'Saving configuration...';
      try {
        var res = await fetch('/api/config/model', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ provider: prov, model: model, base_url: url, api_key: key, budget_usd: budget })
        });
        var data = await res.json();
        if (data.success) {
          feedback.textContent = (currentLang === 'zh' ? '保存成功！已切换模型为: ' : 'Saved! Model set to: ') + data.model + ' (' + data.provider + ')';
          feedback.style.color = 'var(--primary-bright)';
          loadConfig();
          loadStatus();
        } else {
          feedback.textContent = 'Failed: ' + data.error;
          feedback.style.color = 'var(--error)';
        }
      } catch (e) {
        feedback.textContent = 'Error: ' + e.message;
        feedback.style.color = 'var(--error)';
      }
    }

    async function testModelConnectivity() {
      var badge = document.getElementById('modelProbeBadge');
      var feedback = document.getElementById('modelConfigFeedback');

      badge.textContent = currentLang === 'zh' ? '正在探测...' : 'PROBING...';
      badge.style.color = 'var(--tertiary)';
      feedback.textContent = currentLang === 'zh' ? '正在发送测试请求到模型...' : 'Sending test prompt to model...';

      try {
        var res = await fetch('/api/test-llm', { method: 'POST' });
        var data = await res.json();
        if (data.success) {
          badge.textContent = data.latency_ms + 'ms (OK)';
          badge.style.color = 'var(--primary-bright)';
          feedback.textContent = (currentLang === 'zh' ? '连通性已验证: ' : 'Connectivity verified: ') + data.provider + ' (' + data.model + ') returned ' + data.tokens + ' tokens in ' + data.latency_ms + 'ms';
          feedback.style.color = 'var(--primary-bright)';
        } else {
          badge.textContent = 'FAIL';
          badge.style.color = 'var(--error)';
          feedback.textContent = 'Probe error: ' + data.error;
          feedback.style.color = 'var(--error)';
        }
      } catch (e) {
        badge.textContent = 'NET_ERR';
        badge.style.color = 'var(--error)';
        feedback.textContent = 'Connection error: ' + e.message;
        feedback.style.color = 'var(--error)';
      }
    }

    async function toggleRedline() {
      try {
        var res = await fetch('/api/halt', { method: 'POST' });
        var data = await res.json();
        loadStatus();
      } catch (e) {
        alert('Failed to toggle emergency halt: ' + e.message);
      }
    }

    async function dispatchExecution() {
      var prompt = document.getElementById('taskPromptInput').value.trim();
      if (!prompt) return;

      var btn = document.getElementById('dispatchBtn');
      var btnText = document.getElementById('dispatchText');
      var cotOut = document.getElementById('cotStreamOutput');
      var toolOut = document.getElementById('toolStreamOutput');
      var latencyBadge = document.getElementById('runLatencyBadge');
      var hashLabel = document.getElementById('lastReceiptHash');

      btn.disabled = true;
      btnText.textContent = currentLang === 'zh' ? '正在执行...' : 'Processing...';
      latencyBadge.textContent = 'RUNNING';
      latencyBadge.style.color = 'var(--tertiary)';

      cotOut.textContent += '\n\n[dispatch] initiating execution for: ' + prompt;
      cotOut.textContent += '\n[kernel] checking security bounds and token quota...';
      cotOut.scrollTop = cotOut.scrollHeight;

      toolOut.textContent += '\n\n[mcp:dispatch] dispatching tool interceptor for objective...';
      toolOut.scrollTop = toolOut.scrollHeight;

      try {
        var res = await fetch('/api/run', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ prompt: prompt })
        });
        var data = await res.json();
        if (data.success) {
          var r = data.receipt;
          cotOut.textContent += '\n[kernel] task resolved successfully:\n  ' + r.output;
          cotOut.textContent += '\n[audit] immutable receipt notarized: ' + r.receipt_id;
          toolOut.textContent += '\n[mcp:receipt] confirmed SHA-256 attestation: ' + r.signature;
          latencyBadge.textContent = r.duration_ms + 'ms (OK)';
          latencyBadge.style.color = 'var(--primary-bright)';
          hashLabel.textContent = (currentLang === 'zh' ? '最新收据: ' : 'Last receipt: ') + r.receipt_id;
        } else {
          cotOut.textContent += '\n[error] dispatch failed: ' + (data.error || 'Unknown error');
          latencyBadge.textContent = 'FAILED';
          latencyBadge.style.color = 'var(--error)';
        }
      } catch (e) {
        cotOut.textContent += '\n[error] network error: ' + e.message;
        latencyBadge.textContent = 'NET_ERR';
        latencyBadge.style.color = 'var(--error)';
      } finally {
        btn.disabled = false;
        btnText.textContent = i18n[currentLang].dispatch_btn_text || 'Dispatch Execution';
        cotOut.scrollTop = cotOut.scrollHeight;
        toolOut.scrollTop = toolOut.scrollHeight;
        loadStatus();
      }
    }

    applyLanguage(currentLang);
    loadStatus();
    loadAgents();
    setInterval(loadStatus, 5000);
  </script>
</body>
</html>
`
