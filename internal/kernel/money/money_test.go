package money

import (
	"encoding/json"
	"testing"
)

func TestFixedPointConversionsAndTokenCost(t *testing.T) {
	amount, err := FromUSD(0.1234564)
	if err != nil || amount != 123456 {
		t.Fatalf("amount=%d err=%v", amount, err)
	}
	cost, err := TokenCost(101, MustFromUSD(3))
	if err != nil || cost != 303 {
		t.Fatalf("cost=%d err=%v", cost, err)
	}
}

func TestJSONDollarContractIsExact(t *testing.T) {
	var amount MicroUSD
	if err := json.Unmarshal([]byte(`1.234567`), &amount); err != nil || amount != 1_234_567 {
		t.Fatalf("amount=%d err=%v", amount, err)
	}
	if err := json.Unmarshal([]byte(`0.0000001`), &amount); err == nil {
		t.Fatal("sub-microUSD precision was accepted")
	}
	encoded, err := json.Marshal(MicroUSD(1_230_000))
	if err != nil || string(encoded) != "1.23" {
		t.Fatalf("encoded=%s err=%v", encoded, err)
	}
}
