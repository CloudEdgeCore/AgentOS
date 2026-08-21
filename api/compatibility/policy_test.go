package compatibility_test

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/bian-cloud-skill/agentos/internal/kernel/agentversion"
	"github.com/bian-cloud-skill/agentos/sdk/agent"
)

func TestV1PromotionPolicyIsMachineComplete(t *testing.T) {
	encoded, err := os.ReadFile("v1alpha1-to-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var policy struct {
		Kind    string
		Current struct {
			AgentManifest    string
			RuntimeInterface string
		}
		Policy struct {
			MinimumDeprecationDays             int
			NMinusOneReadCompatibility         bool
			AdditiveChangesOnlyBeforePromotion bool
			UnknownFieldsFailClosed            bool
		}
		FrozenRuntimeLifecycle []string
		PromotionGates         []string
	}
	if err := json.Unmarshal(encoded, &policy); err != nil {
		t.Fatal(err)
	}
	if policy.Kind != "CompatibilityPromotion" ||
		policy.Current.AgentManifest != agentversion.ManifestAPIVersion ||
		policy.Current.RuntimeInterface != agent.ProtocolVersion ||
		agentversion.RuntimeInterfaceV1Alpha1 != agent.ProtocolVersion {
		t.Fatalf("policy drifted from code constants: %+v", policy)
	}
	if policy.Policy.MinimumDeprecationDays < 180 || !policy.Policy.NMinusOneReadCompatibility ||
		!policy.Policy.AdditiveChangesOnlyBeforePromotion || !policy.Policy.UnknownFieldsFailClosed {
		t.Fatalf("unsafe promotion policy: %+v", policy.Policy)
	}
	for _, lifecycle := range []string{"health", "start", "stop", "event", "result", "checkpoint", "restore"} {
		if !slices.Contains(policy.FrozenRuntimeLifecycle, lifecycle) {
			t.Fatalf("runtime lifecycle %q is not frozen", lifecycle)
		}
	}
	if len(policy.PromotionGates) < 7 {
		t.Fatalf("promotion gates are incomplete: %v", policy.PromotionGates)
	}
}
