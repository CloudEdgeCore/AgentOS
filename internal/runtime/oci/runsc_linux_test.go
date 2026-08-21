//go:build linux

package oci

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunscExecutorUsesFailClosedCtrArguments(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ctr.log")
	ctrPath := filepath.Join(dir, "ctr")
	const fakeCtr = `#!/bin/sh
printf '%s\n' "$*" >> "$AGENTOS_CTR_TEST_LOG"
exit 0
`
	if err := os.WriteFile(ctrPath, []byte(fakeCtr), 0o700); err != nil {
		t.Fatalf("write fake ctr: %v", err)
	}
	t.Setenv("AGENTOS_CTR_TEST_LOG", logPath)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	executor, err := NewRunscExecutor(WithSkipPull())
	if err != nil {
		t.Fatalf("new runsc executor: %v", err)
	}
	execution, err := executor.Prepare(context.Background(), ExecutionSpec{
		TenantID:         "tenant-a",
		AttemptID:        "attempt-a",
		AgentVersionRef:  "echo@1.0.0",
		WorkloadSpecJSON: []byte(`{"kind":"Task"}`),
		ImageRef:         "example.invalid/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		WorkspaceBytes:   64 << 20,
		CPUQuotaMillis:   250,
		MemoryLimitMiB:   128,
	})
	if err != nil {
		t.Fatalf("prepare sandbox: %v", err)
	}
	if _, err := execution.Wait(context.Background()); err != nil {
		t.Fatalf("wait for sandbox: %v", err)
	}
	if err := executor.Destroy(context.Background(), execution); err != nil {
		t.Fatalf("destroy sandbox: %v", err)
	}

	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake ctr log: %v", err)
	}
	var runArgs []string
	for _, line := range strings.Split(strings.TrimSpace(string(logged)), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "-n" && fields[2] == "run" {
			runArgs = fields
			break
		}
	}
	if len(runArgs) == 0 {
		t.Fatalf("ctr run invocation not recorded: %s", logged)
	}

	for _, want := range [][]string{
		{"--runtime", "io.containerd.runsc.v1"},
		{"--runtime-config-path", "/etc/containerd/runsc.toml"},
		{"--snapshotter", "overlayfs"},
		{"--read-only"},
		{"--cap-drop", "ALL"},
		{"--seccomp"},
		{"--cpu-quota", "250000"},
		{"--memory-limit", "134217728"},
	} {
		if !containsSequence(runArgs, want) {
			t.Errorf("ctr run arguments missing %q: %q", want, runArgs)
		}
	}
	if containsSequence(runArgs, []string{"--memory"}) {
		t.Errorf("unsupported ctr --memory flag present: %q", runArgs)
	}
	if containsSequence(runArgs, []string{"--allow-new-privs"}) {
		t.Errorf("sandbox must retain no-new-privileges: %q", runArgs)
	}
}

func containsSequence(values, sequence []string) bool {
	if len(sequence) == 0 || len(sequence) > len(values) {
		return false
	}
	for start := 0; start <= len(values)-len(sequence); start++ {
		match := true
		for index := range sequence {
			if values[start+index] != sequence[index] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
