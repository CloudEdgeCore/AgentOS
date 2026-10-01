package tools

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type ToolServer struct {
	server *http.Server
	port   int
	mu     sync.Mutex
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

	// 1. 自定义质检分析工具：custom.quality.metrics@1.0.0
	mux.HandleFunc("/api/v1/quality-metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req GenericRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			json.NewEncoder(w).Encode(GenericResponse{Error: err.Error()})
			return
		}

		resp := map[string]any{
			"product":         "Product-A (高精度注塑结构件)",
			"time_window":     "过去 72 小时 (3天)",
			"total_inspected": 12500,
			"overall_trend": []map[string]any{
				{"day": "Day-1 (-3d)", "inspected": 4100, "defects": 74, "defect_rate": "1.80%"},
				{"day": "Day-2 (-2d)", "inspected": 4150, "defects": 133, "defect_rate": "3.20%"},
				{"day": "Day-3 (Yesterday)", "inspected": 4250, "defects": 196, "defect_rate": "4.61%"},
			},
			"pareto_defect_types": []map[string]any{
				{"defect_type": "表面微裂纹与翘曲形变 (Surface Cracking & Warpage)", "count": 316, "percentage": "78.4%", "cumulative": "78.4%"},
				{"defect_type": "装配关键尺寸超差 (Dimensional Out-of-Tolerance)", "count": 55, "percentage": "13.6%", "cumulative": "92.0%"},
				{"defect_type": "表面银丝/气斑 (Silver Streaking)", "count": 32, "percentage": "8.0%", "cumulative": "100.0%"},
			},
			"cross_tabulation_by_line_and_shift": map[string]any{
				"Line-01 (1号注塑机 IM-01)": map[string]string{
					"白班 (Day Shift 08:00-20:00)":   "不良率 1.62% (正常)",
					"夜班 (Night Shift 20:00-08:00)": "不良率 1.75% (正常)",
				},
				"Line-02 (2号注塑机 IM-02)": map[string]string{
					"白班 (Day Shift 08:00-20:00)":   "不良率 2.05% (轻微上升)",
					"夜班 (Night Shift 20:00-08:00)": "不良率 8.92% (严重恶化！贡献全厂 81.3% 的翘曲缺陷)",
				},
			},
		}

		receiptID := fmt.Sprintf("rcpt_qm_%x", time.Now().UnixNano())
		json.NewEncoder(w).Encode(GenericResponse{Output: resp, ReceiptID: receiptID})
	})

	// 2. 自定义工艺时序与相关性分析工具：custom.process.telemetry@1.0.0
	mux.HandleFunc("/api/v1/process-telemetry", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req GenericRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			json.NewEncoder(w).Encode(GenericResponse{Error: err.Error()})
			return
		}

		resp := map[string]any{
			"equipment_id": "Line-02 / IM-02 (2号注塑机)",
			"parameters_analyzed": map[string]any{
				"mold_temperature (模具温度)": map[string]any{
					"target_sop":                       "65.0 ℃ (工艺窗口: 62.0℃ ~ 68.0℃)",
					"im01_baseline_night":              "平均 64.9 ℃ (标准差 σ=0.7℃, 极其平稳)",
					"im02_actual_night":                "平均 49.1 ℃ (最低跌至 44.8℃, 标准差 σ=6.8℃, 严重失稳！)",
					"abnormal_time_range":              "夜间 01:15 ~ 05:40 模温连续跌破 52℃ 以下",
					"pearson_correlation_with_warpage": 0.942, // 与翘曲缺陷呈极强负相关(温度越低翘曲越高)
				},
				"injection_pressure (注塑压力)": map[string]any{
					"target_sop":        "8.50 MPa (窗口 8.2 ~ 8.8 MPa)",
					"im02_actual_night": "平均 8.48 MPa (波动 < 0.15 MPa, 完全正常)",
				},
				"cooling_time (保压冷却时长)": map[string]any{
					"target_sop":        "18.0 s",
					"im02_actual_night": "18.0 s (PLC程序受控，无改动)",
				},
			},
			"raw_material_batch": "聚碳酸酯 PC-1100 批次 LOT-20260925-B (全厂Line-01与02共用同一批原料，Line-01正常，排除原料批次污染)",
		}

		receiptID := fmt.Sprintf("rcpt_pt_%x", time.Now().UnixNano())
		json.NewEncoder(w).Encode(GenericResponse{Output: resp, ReceiptID: receiptID})
	})

	// 3. 自定义企业私有案例与知识库检索工具：custom.knowledge.cases@1.0.0
	mux.HandleFunc("/api/v1/knowledge-case-search", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req GenericRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			json.NewEncoder(w).Encode(GenericResponse{Error: err.Error()})
			return
		}

		resp := map[string]any{
			"query": "IM-02 模温过低 夜班 翘曲 裂纹 模温机 案例与SOP",
			"retrieved_cases": []map[string]any{
				{
					"case_id":          "CASE-QA-2025-081",
					"title":            "《2号注塑机夜间模温失控导致结构件翘曲与微裂纹故障溯源报告》",
					"incident_date":    "2025-11-14",
					"similarity_score": 0.965,
					"case_summary":     "车间夜间环境温度降至10℃以下时，IM-02配套的模温机2号加热旁通比例阀阀芯机械卡死在小开度位置，热介质循环受阻。白天环境气温高时模温能被动维持在60℃，夜间气温骤降无法有效补热，模温跌至48℃，导致成型内应力急剧增大，产生大批量翘曲和微裂纹缺陷。",
					"verified_fix":     "拆洗模温机电磁比例阀阀芯，清理水垢异物，更换高温密封圈后恢复正常。",
				},
			},
			"retrieved_sop": map[string]any{
				"sop_id": "SOP-QA-MOLD-04",
				"title":  "《注塑成型模温异常应急排查与处置规程 (2026修订版)》",
				"critical_rules": []string{
					"模温低于 55℃ 时熔体流动前沿结晶过快，严禁继续生产，必须触发防呆停机 (Auto-Hold)",
					"排查顺序必须先查模温机循环泵进出口压差，再测加热管阻值及比例阀驱动电压 (0-10V)",
					"所有已生产的 IM-02 夜班批次必须加贴黄色不合格标签，隔离抽检尺寸与应力测试",
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
