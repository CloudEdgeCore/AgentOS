package easy

import (
	"context"
	"strings"
	"testing"
)

func TestEasyAgentRun(t *testing.T) {
	agent := New("test-agent").
		WithTool("echo", func(ctx context.Context, input string) (string, error) {
			return "echo: " + input, nil
		})

	res, err := agent.Run(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(res.Receipts) != 1 {
		t.Fatalf("expected 1 receipt, got %d", len(res.Receipts))
	}

	if !strings.HasPrefix(res.Receipts[0].Hash, "sha256:rcpt_") {
		t.Errorf("expected valid hash, got %s", res.Receipts[0].Hash)
	}

	if res.CostUSD <= 0 {
		t.Errorf("expected positive cost, got %f", res.CostUSD)
	}
}
