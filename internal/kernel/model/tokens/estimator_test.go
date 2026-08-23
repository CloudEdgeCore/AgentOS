package tokens

import "testing"

func legacyBytesPerFour(text string) int64 {
	return (int64(len(text)) + 3) / 4
}

// TestHeuristicDoesNotUnderestimateDenseScripts proves the estimator that
// replaced bytes/4 no longer under-reserves CJK prompts: Chinese, Japanese,
// and Korean text estimate at roughly one token per character instead of
// one token per four UTF-8 bytes.
func TestHeuristicDoesNotUnderestimateDenseScripts(t *testing.T) {
	samples := []string{
		"分析当前系统的可靠性与安全性，并给出改进建议。",
		"システムの信頼性を分析し、改善案を提示してください。",
		"시스템의 안정성을 분석하고 개선 방안을 제시하세요.",
	}
	for _, text := range samples {
		if got, legacy := Heuristic(text), legacyBytesPerFour(text); got < legacy {
			t.Fatalf("heuristic %d below legacy bytes/4 %d for %q", got, legacy, text)
		}
		// Interleaved ASCII (spaces, punctuation) stays cheaper than one
		// token per rune; the estimate must still cover ~the whole run.
		runes := int64(len([]rune(text)))
		if Heuristic(text) < runes*9/10 {
			t.Fatalf("dense script estimate %d well below the %d-rune prompt", Heuristic(text), runes)
		}
	}
}

// TestHeuristicKeepsLatinFloor proves ordinary Latin prompts keep at least
// the conventional bytes/4 reservation.
func TestHeuristicKeepsLatinFloor(t *testing.T) {
	text := "Analyze the current system reliability and propose improvements."
	if got, legacy := Heuristic(text), legacyBytesPerFour(text); got < legacy {
		t.Fatalf("latin heuristic %d below legacy %d", got, legacy)
	}
	if got := Heuristic(""); got != 0 {
		t.Fatalf("empty text estimate = %d, want 0", got)
	}
}

// TestHeuristicCoversCodeAndJson proves structured content (JSON, code with
// dense-script identifiers, mixed payloads) estimates at or above bytes/4.
func TestHeuristicCoversCodeAndJson(t *testing.T) {
	samples := []string{
		`{"messages":[{"role":"user","content":"总结结果"}],"max_tokens":1024}`,
		"func 分析() { return \"结果\" } // 混合代码",
		"SELECT * FROM 订单 WHERE 状态 = '已完成';",
	}
	for _, text := range samples {
		if got, legacy := Heuristic(text), legacyBytesPerFour(text); got < legacy {
			t.Fatalf("structured heuristic %d below legacy %d for %q", got, legacy, text)
		}
	}
}

// TestConservativeDoublesSafety proves unknown-tokenizer fail-safety: the
// conservative estimator never estimates below the heuristic.
func TestConservativeDoublesSafety(t *testing.T) {
	samples := []string{
		"plain ascii prompt",
		"中文提示词，包含大量表意字符。",
		`{"json":true,"nested":{"values":[1,2,3]}}`,
	}
	for _, text := range samples {
		if Conservative(text) < Heuristic(text) {
			t.Fatalf("conservative %d below heuristic %d for %q", Conservative(text), Heuristic(text), text)
		}
	}
}

// TestForNameResolvesAndRejects proves tokenizer names resolve to the
// documented estimators and unknown names fail closed.
func TestForNameResolvesAndRejects(t *testing.T) {
	if _, err := ForName(""); err != nil {
		t.Fatalf("default tokenizer rejected: %v", err)
	}
	if _, err := ForName("heuristic"); err != nil {
		t.Fatalf("heuristic rejected: %v", err)
	}
	if _, err := ForName("conservative"); err != nil {
		t.Fatalf("conservative rejected: %v", err)
	}
	if _, err := ForName("tiktoken-cl100k"); err == nil {
		t.Fatal("unknown tokenizer accepted, want rejection")
	}
}

// P1-05: the tokenizer plug-in seam — a deployment registers an exact
// per-provider tokenizer under a config-selectable name; built-in names are
// reserved and duplicates are rejected so link order can never swap an
// estimator silently.
func TestRegisterInstallsPluginTokenizer(t *testing.T) {
	exact := func(text string) int64 { return int64(len([]rune(text))) }
	if err := Register("test-exact-plugin", exact); err != nil {
		t.Fatalf("register plugin tokenizer: %v", err)
	}
	resolved, err := ForName("test-exact-plugin")
	if err != nil {
		t.Fatalf("resolve registered tokenizer: %v", err)
	}
	if resolved("abc中") != 4 {
		t.Fatalf("registered tokenizer not returned verbatim: %d", resolved("abc中"))
	}
	if err := Register("test-exact-plugin", exact); err == nil {
		t.Fatal("duplicate registration accepted")
	}
	if err := Register("heuristic", exact); err == nil {
		t.Fatal("built-in name override accepted")
	}
	if err := Register("", exact); err == nil {
		t.Fatal("empty name accepted")
	}
	if err := Register("nil-estimator", nil); err == nil {
		t.Fatal("nil estimator accepted")
	}
}
