package domain

import "testing"

func TestTaskTransitions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		from    TaskPhase
		to      TaskPhase
		wantErr bool
	}{
		{name: "admit queued task", from: TaskQueued, to: TaskAdmitted},
		{name: "cancel before scheduling", from: TaskQueued, to: TaskCancelled},
		{name: "start admitted task", from: TaskAdmitted, to: TaskRunning},
		{name: "commit success", from: TaskRunning, to: TaskSucceeded},
		{name: "terminal is immutable", from: TaskSucceeded, to: TaskRunning, wantErr: true},
		{name: "cannot skip admission", from: TaskQueued, to: TaskRunning, wantErr: true},
		{name: "same phase is not a transition", from: TaskRunning, to: TaskRunning, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateTaskTransition(tt.from, tt.to)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateTaskTransition(%q, %q) error = %v, wantErr %v", tt.from, tt.to, err, tt.wantErr)
			}
		})
	}
}

func TestRunTransitions(t *testing.T) {
	t.Parallel()

	if err := ValidateRunTransition(RunPending, RunRunning); err != nil {
		t.Fatalf("pending run should start: %v", err)
	}
	if err := ValidateRunTransition(RunRunning, RunCompleted); err != nil {
		t.Fatalf("running run should complete: %v", err)
	}
	if err := ValidateRunTransition(RunCompleted, RunRunning); err == nil {
		t.Fatal("completed run must be immutable")
	}
}

func TestAttemptTransitions(t *testing.T) {
	t.Parallel()

	path := []AttemptPhase{
		AttemptPending,
		AttemptPlaced,
		AttemptStarting,
		AttemptRunning,
		AttemptWaitingTool,
		AttemptRunning,
		AttemptCheckpointing,
		AttemptRunning,
		AttemptCompleted,
	}
	for i := 1; i < len(path); i++ {
		if err := ValidateAttemptTransition(path[i-1], path[i]); err != nil {
			t.Fatalf("valid path step %s -> %s failed: %v", path[i-1], path[i], err)
		}
	}

	if err := ValidateAttemptTransition(AttemptCompleted, AttemptRunning); err == nil {
		t.Fatal("completed attempt must be immutable")
	}
	if !AttemptFailed.Terminal() || !AttemptCancelled.Terminal() || AttemptRunning.Terminal() {
		t.Fatal("attempt terminal classification is incorrect")
	}
}
