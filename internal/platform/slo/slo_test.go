package slo

import "testing"

func passingSample() Sample {
	return Sample{
		Schema: Schema, WindowSeconds: 2_592_000, ControlRequests: 1_000_000, ControlFailures: 100,
		AcceptedTasks: 10_000, SchedulingMeasurements: 10_000, SchedulingDecisionP99Seconds: 1.2,
		RecoveryMeasurements: 100, RuntimeRecoveryP99Seconds: 30, BudgetStopMeasurements: 100,
		BudgetHardStopP99Seconds: 2, ExpectedAuditEvents: 100_000, RecordedAuditEvents: 100_000,
		TenantStateRPOSeconds: 0, ControlPlaneRTOSeconds: 300,
	}
}

func TestEvaluateRequiresEvidenceAndEveryObjective(t *testing.T) {
	report, err := Evaluate(passingSample())
	if err != nil || !report.Passed || len(report.Checks) != 9 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	failing := passingSample()
	failing.RuntimeRecoveryP99Seconds = 60
	report, err = Evaluate(failing)
	if err != nil || report.Passed {
		t.Fatalf("failing report=%+v err=%v", report, err)
	}
	if _, err := Evaluate(Sample{Schema: Schema}); err == nil {
		t.Fatal("empty evidence was accepted")
	}
}
