package tools

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// SensorDataPoint represents one industrial sensor reading.
type SensorDataPoint struct {
	Timestamp   string  `json:"timestamp"`
	Temperature float64 `json:"temperature"` // Celsius
	CoolantFlow float64 `json:"coolantFlow"` // L/min (normal: 40-50)
	SpindleRPM  int     `json:"spindleRpm"`  // RPM
	Vibration   float64 `json:"vibration"`   // mm/s (normal: < 2.5)
}

// SensorQueryResult is returned by industrial.sensor.query.
type SensorQueryResult struct {
	EquipmentID      string            `json:"equipmentId"`
	EquipmentName    string            `json:"equipmentName"`
	EquipmentType    string            `json:"equipmentType"`
	TimeRangeMinutes int               `json:"timeRangeMinutes"`
	CurrentStatus    string            `json:"currentStatus"`
	Readings         []SensorDataPoint `json:"readings"`
	Anomalies        []string          `json:"anomalies"`
}

// AlarmInfo is returned by industrial.alarm.lookup.
type AlarmInfo struct {
	AlarmCode         string   `json:"alarmCode"`
	AlarmTitle        string   `json:"alarmTitle"`
	Severity          string   `json:"severity"` // CRITICAL, WARNING, INFO
	Component         string   `json:"component"`
	TriggerCondition  string   `json:"triggerCondition"`
	HistoricalStats   string   `json:"historicalStats"`
	PossibleCauses    []string `json:"possibleCauses"`
}

// SOPGuideline is returned by industrial.sop.search.
type SOPGuideline struct {
	DocID        string   `json:"docId"`
	DocTitle     string   `json:"docTitle"`
	ApplicableTo string   `json:"applicableTo"`
	Steps        []string `json:"steps"`
	SafetyNotice string   `json:"safetyNotice"`
}

// IndustrialToolServer serves industrial tools as webhooks for AgentOS Tool Gateway.
type IndustrialToolServer struct{}

func NewIndustrialToolServer() *IndustrialToolServer {
	return &IndustrialToolServer{}
}

func (s *IndustrialToolServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Action   string          `json:"action"`
		Resource string          `json:"resource"`
		Args     json.RawMessage `json:"args"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}

	action := req.Action
	if action == "invoke" {
		switch {
		case strings.HasPrefix(req.Resource, "industrial:sensor"):
			action = "sensor-query"
		case strings.HasPrefix(req.Resource, "industrial:alarm"):
			action = "alarm-lookup"
		case strings.HasPrefix(req.Resource, "industrial:sop"):
			action = "sop-search"
		case strings.HasPrefix(req.Resource, "industrial:stop"):
			action = "emergency-stop"
		default:
			http.Error(w, "unknown resource: "+req.Resource, http.StatusBadRequest)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	switch action {
	case "sensor-query":
		s.handleSensorQuery(w, req.Args)
	case "alarm-lookup":
		s.handleAlarmLookup(w, req.Args)
	case "sop-search":
		s.handleSOPSearch(w, req.Args)
	case "emergency-stop":
		s.handleEmergencyStop(w, req.Args)
	default:
		http.Error(w, "unknown action: "+req.Action, http.StatusBadRequest)
	}
}

func (s *IndustrialToolServer) handleSensorQuery(w http.ResponseWriter, raw json.RawMessage) {
	var args struct {
		EquipmentID      string `json:"equipmentId"`
		TimeRangeMinutes int    `json:"timeRangeMinutes"`
	}
	_ = json.Unmarshal(raw, &args)
	if args.EquipmentID == "" {
		args.EquipmentID = "CNC-03"
	}
	if args.TimeRangeMinutes <= 0 {
		args.TimeRangeMinutes = 20
	}

	now := time.Now()
	readings := make([]SensorDataPoint, 0, 5)
	// Simulate 20 minutes of data showing temperature climb from 74°C to 89.4°C and coolant drop
	readings = append(readings,
		SensorDataPoint{Timestamp: now.Add(-20 * time.Minute).Format("15:04:05"), Temperature: 74.2, CoolantFlow: 46.5, SpindleRPM: 12000, Vibration: 1.1},
		SensorDataPoint{Timestamp: now.Add(-15 * time.Minute).Format("15:04:05"), Temperature: 79.5, CoolantFlow: 42.0, SpindleRPM: 12000, Vibration: 1.3},
		SensorDataPoint{Timestamp: now.Add(-10 * time.Minute).Format("15:04:05"), Temperature: 84.8, CoolantFlow: 37.5, SpindleRPM: 12000, Vibration: 1.6},
		SensorDataPoint{Timestamp: now.Add(-5 * time.Minute).Format("15:04:05"), Temperature: 87.6, CoolantFlow: 35.8, SpindleRPM: 12000, Vibration: 1.9},
		SensorDataPoint{Timestamp: now.Format("15:04:05"), Temperature: 89.4, CoolantFlow: 35.1, SpindleRPM: 12000, Vibration: 2.1},
	)

	result := SensorQueryResult{
		EquipmentID:      args.EquipmentID,
		EquipmentName:    "3号五轴联动数控加工中心 (CNC-03)",
		EquipmentType:    "五轴加工中心",
		TimeRangeMinutes: args.TimeRangeMinutes,
		CurrentStatus:    "ALARM_CRITICAL",
		Readings:         readings,
		Anomalies: []string{
			"主轴温度超限：过去20分钟内从 74.2℃ 持续飙升至 89.4℃（告警阈值 85.0℃，当前超标 +4.4℃）",
			"冷却液流量骤降：流量由正常基线 46.5 L/min 降至 35.1 L/min（衰减 -24.5%）",
			"主轴振动值呈微弱正相关上升（1.1 mm/s -> 2.1 mm/s），仍在机械振动警戒线（2.8 mm/s）以内",
		},
	}
	_ = json.NewEncoder(w).Encode(result)
}

func (s *IndustrialToolServer) handleAlarmLookup(w http.ResponseWriter, raw json.RawMessage) {
	var args struct {
		AlarmCode string `json:"alarmCode"`
	}
	_ = json.Unmarshal(raw, &args)
	if args.AlarmCode == "" {
		args.AlarmCode = "E102"
	}

	info := AlarmInfo{
		AlarmCode:        args.AlarmCode,
		AlarmTitle:       "主轴电机与冷却回路温度过热报警 (Spindle Overheat / Cooling Circuit Fault)",
		Severity:         "CRITICAL",
		Component:        "主轴高速电机单元 / 恒温循环冷却夹套",
		TriggerCondition: "主轴定子热敏电阻测得温度连续 > 85.0℃ 超过 300 秒，或热保护触点断开",
		HistoricalStats:  "车间同类故障大数据统计：63% 为冷却管路结垢或滤网堵塞导致流量不足；27% 为循环水泵叶轮磨损或温控阀失效；10% 为主轴轴承润滑不良或定子线圈局部短路",
		PossibleCauses: []string{
			"冷却系统滤网被金属细屑或油泥严重堵塞，导致冷却回路循环流速受阻",
			"主轴油冷机/水冷机循环泵出水压力不足，流量跌破 38 L/min 下限",
			"主轴高负载切削工况下轴承润滑油气供应不足，产生额外摩擦热",
		},
	}
	_ = json.NewEncoder(w).Encode(info)
}

func (s *IndustrialToolServer) handleSOPSearch(w http.ResponseWriter, raw json.RawMessage) {
	var args struct {
		Query string `json:"query"`
	}
	_ = json.Unmarshal(raw, &args)

	sop := SOPGuideline{
		DocID:        "SOP-CNC-MNT-2026-E102",
		DocTitle:     "CNC 主轴过热（E102）标准应急处置与检修规程",
		ApplicableTo: "CNC-01 至 CNC-12 五轴数控加工系列",
		Steps: []string{
			"第 1 步：立即暂停当前切削进给（Cycle Pause），维持主轴空载低速运转（500 RPM）3分钟进行风冷过渡，严禁直接紧急断电导致动轴承抱死。",
			"第 2 步：检查主轴油冷机储液箱液位视窗（正常应高于绿色刻度线），检查回液管路滤网是否被铝屑/切削液杂质堵塞，必要时执行快速反冲洗。",
			"第 3 步：观察油冷机压力表（正常工作区间：0.25 - 0.40 MPa），若读数低于 0.18 MPa，需检修循环泵电磁阀及泵体联轴节。",
			"第 4 步：待主轴温度自然回落至 65℃ 以下后，手动点动主轴并用测振仪检测自由端与驱动端振动值，确认无明显机械刮擦异响。",
		},
		SafetyNotice: "严禁在主轴温度高于 80℃ 时直接用冷水喷淋降温，主轴套筒急冷会导致热应力裂纹与动平衡永久报废！",
	}
	_ = json.NewEncoder(w).Encode(sop)
}

func (s *IndustrialToolServer) handleEmergencyStop(w http.ResponseWriter, raw json.RawMessage) {
	var args struct {
		EquipmentID string `json:"equipmentId"`
		Reason      string `json:"reason"`
	}
	_ = json.Unmarshal(raw, &args)

	res := map[string]any{
		"equipmentId": args.EquipmentID,
		"status":      "INTERLOCKED_SAFE_STOP",
		"timestamp":   time.Now().Format(time.RFC3339),
		"message":     fmt.Sprintf("Equipment %s safe deceleration interlock triggered. Reason: %s", args.EquipmentID, args.Reason),
		"requiresAck": true,
	}
	_ = json.NewEncoder(w).Encode(res)
}
