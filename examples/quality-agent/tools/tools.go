package tools

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type ToolServer struct {
	server *http.Server
	port   int
}

func NewToolServer(port int) *ToolServer {
	return &ToolServer{port: port}
}

type GenericRequest struct {
	Action   string          `json:"action"`
	Resource string          `json:"resource"`
	Args     json.RawMessage `json:"args"`
}

type GenericResponse struct {
	Output    any    `json:"output"`
	ReceiptID string `json:"receiptId,omitempty"`
	Error     string `json:"error,omitempty"`
}

func (s *ToolServer) Start() error {
	mux := http.NewServeMux()

	// 1. Tool: custom.quality.metrics@1.0.0
	mux.HandleFunc("/api/v1/quality-metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req GenericRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			json.NewEncoder(w).Encode(GenericResponse{Error: err.Error()})
			return
		}

		resp := map[string]any{
			"product":         "Product-A (Precision Injection Molded Structural Component)",
			"time_window":     "last 72 hours (3 days)",
			"total_inspected": 12500,
			"overall_trend": []map[string]any{
				{"day": "Day-1 (-3d)", "inspected": 4100, "defects": 74, "defect_rate": "1.80%"},
				{"day": "Day-2 (-2d)", "inspected": 4150, "defects": 133, "defect_rate": "3.20%"},
				{"day": "Day-3 (Yesterday)", "inspected": 4250, "defects": 196, "defect_rate": "4.61%"},
			},
			"pareto_defect_types": []map[string]any{
				{"defect_type": "Surface micro-cracking and warpage deformation", "count": 316, "percentage": "78.4%", "cumulative": "78.4%"},
				{"defect_type": "Key assembly dimensional out-of-tolerance", "count": 55, "percentage": "13.6%", "cumulative": "92.0%"},
				{"defect_type": "Surface silver streaking / gas splay", "count": 32, "percentage": "8.0%", "cumulative": "100.0%"},
			},
			"cross_tabulation_by_line_and_shift": map[string]any{
				"Line-01 (Injection Machine IM-01)": map[string]string{
					"Day shift (08:00-20:00)":   "defect_rate: 1.62% (nominal)",
					"Night shift (20:00-08:00)": "defect_rate: 1.75% (nominal)",
				},
				"Line-02 (Injection Machine IM-02)": map[string]string{
					"Day shift (08:00-20:00)":   "defect_rate: 2.05% (slight increase)",
					"Night shift (20:00-08:00)": "defect_rate: 8.92% (critical degradation, accounts for 81.3% of warpage defects)",
				},
			},
		}

		receiptID := fmt.Sprintf("rcpt_qm_%x", time.Now().UnixNano())
		json.NewEncoder(w).Encode(GenericResponse{Output: resp, ReceiptID: receiptID})
	})

	// 2. Tool: custom.process.telemetry@1.0.0
	mux.HandleFunc("/api/v1/process-telemetry", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req GenericRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			json.NewEncoder(w).Encode(GenericResponse{Error: err.Error()})
			return
		}

		resp := map[string]any{
			"equipment_id": "Line-02 / IM-02 (Injection Machine #2)",
			"parameters_analyzed": map[string]any{
				"mold_temperature": map[string]any{
					"target_sop":                       "65.0 C (window: 62.0C - 68.0C)",
					"im01_baseline_night":              "mean 64.9 C (stddev sigma=0.7C, highly stable)",
					"im02_actual_night":                "mean 49.1 C (minimum dip 44.8C, stddev sigma=6.8C, severe instability)",
					"abnormal_time_range":              "night 01:15 - 05:40 mold temp continuously below 52C",
					"pearson_correlation_with_warpage": 0.942,
				},
				"injection_pressure": map[string]any{
					"target_sop":        "8.50 MPa (window: 8.2 - 8.8 MPa)",
					"im02_actual_night": "mean 8.48 MPa (fluctuation < 0.15 MPa, normal)",
				},
				"cooling_time": map[string]any{
					"target_sop":        "18.0 s",
					"im02_actual_night": "18.0 s (PLC logic verified, unchanged)",
				},
			},
			"raw_material_batch": "Polycarbonate PC-1100 Batch LOT-20260925-B (shared across Line-01 and Line-02; Line-01 nominal, raw material contamination ruled out)",
		}

		receiptID := fmt.Sprintf("rcpt_pt_%x", time.Now().UnixNano())
		json.NewEncoder(w).Encode(GenericResponse{Output: resp, ReceiptID: receiptID})
	})

	// 3. Tool: custom.knowledge.cases@1.0.0
	mux.HandleFunc("/api/v1/knowledge-case-search", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req GenericRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			json.NewEncoder(w).Encode(GenericResponse{Error: err.Error()})
			return
		}

		resp := map[string]any{
			"query": "IM-02 low mold temperature night shift warpage cracking mold temperature controller case and SOP",
			"retrieved_cases": []map[string]any{
				{
					"case_id":          "CASE-QA-2025-081",
					"title":            "Machine #2 Night Shift Mold Temperature Loss Causing Structural Warpage and Micro-cracking Root Cause Report",
					"incident_date":    "2025-11-14",
					"similarity_score": 0.965,
					"case_summary":     "When ambient workshop temperature drops below 10C at night, IM-02 mold temperature controller heating proportional bypass valve spool mechanically jammed at small opening position, restricting thermal oil circulation. Daytime ambient temperatures allowed passive thermal stabilization at 60C, while nighttime temperature drops failed to compensate, causing mold temperature to plunge to 48C and resulting in residual stress-induced warpage and micro-cracks.",
					"verified_fix":     "Disassembled and flushed proportional bypass valve spool, cleared scale deposit, and replaced high-temperature O-ring seals.",
				},
			},
			"retrieved_sop": map[string]any{
				"sop_id": "SOP-QA-MOLD-04",
				"title":  "Injection Molding Temperature Anomaly Emergency Troubleshooting Protocol (2026 Revision)",
				"critical_rules": []string{
					"When mold temperature falls below 55C, melt flow crystallization occurs prematurely; continued production is strictly forbidden and automated interlock hold must trigger.",
					"Diagnostic sequence must first measure chiller/controller pump inlet/outlet delta-P, followed by heating element resistance and proportional valve drive voltage (0-10V).",
					"All parts produced on IM-02 night shift must be tagged with yellow quarantine labels and isolated for dimensional verification and residual stress inspection.",
				},
			},
		}

		receiptID := fmt.Sprintf("rcpt_kc_%x", time.Now().UnixNano())
		json.NewEncoder(w).Encode(GenericResponse{Output: resp, ReceiptID: receiptID})
	})

	s.server = &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", s.port),
		Handler: mux,
	}

	go func() {
		_ = s.server.ListenAndServe()
	}()

	return nil
}

func (s *ToolServer) Stop() {
	if s.server != nil {
		_ = s.server.Close()
	}
}
