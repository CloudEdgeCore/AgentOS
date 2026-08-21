package capability

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/bian-cloud-skill/agentos/internal/kernel/agentversion"
	"github.com/bian-cloud-skill/agentos/internal/kernel/store"
	"github.com/google/uuid"
)

type versionStore struct {
	version store.AgentVersion
}

func (s versionStore) CreateAgentVersion(context.Context, store.CreateAgentVersionInput) (store.CreateAgentVersionResult, error) {
	return store.CreateAgentVersionResult{}, errors.New("not implemented")
}
func (s versionStore) GetAgentVersion(context.Context, string, uuid.UUID) (store.AgentVersion, error) {
	return store.AgentVersion{}, errors.New("not implemented")
}
func (s versionStore) GetAgentVersionByRef(_ context.Context, tenantID, ref string) (store.AgentVersion, error) {
	if tenantID != s.version.TenantID || ref != s.version.Ref() {
		return store.AgentVersion{}, store.ErrNotFound
	}
	return s.version, nil
}

func TestAuthorizerEnforcesExactAndWildcardGrants(t *testing.T) {
	spec := agentversion.Spec{
		Runtimes: []agentversion.RuntimeTarget{{Class: "remote"}},
		Capabilities: &agentversion.Capabilities{
			Tools: []string{"fs.read", "search.*"}, Models: []string{"model/quality"},
			Memory: []string{"project/*"}, Secrets: []string{},
		},
	}
	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	authorizer, err := NewAuthorizer(versionStore{version: store.AgentVersion{
		TenantID: "tenant-a", Name: "agent", Version: "1", Spec: encoded,
	}})
	if err != nil {
		t.Fatal(err)
	}
	for name, check := range map[string]struct {
		kind       Kind
		candidates []string
	}{
		"exact tool":     {Tool, []string{"fs.read"}},
		"versioned tool": {Tool, []string{"missing", "fs.read"}},
		"wildcard tool":  {Tool, []string{"search.web"}},
		"model":          {Model, []string{"model/quality"}},
		"memory":         {Memory, []string{"project/session"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := authorizer.Authorize(context.Background(), "tenant-a", "agent@1", check.kind, check.candidates...); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := authorizer.Authorize(context.Background(), "tenant-a", "agent@1", Secret, "database/prod"); !errors.Is(err, ErrDenied) {
		t.Fatalf("undeclared secret error=%v", err)
	}
	if err := authorizer.Authorize(context.Background(), "tenant-b", "agent@1", Tool, "fs.read"); !errors.Is(err, ErrDenied) {
		t.Fatalf("cross-tenant error=%v", err)
	}
}

func TestAuthorizerPreservesLegacyV1Alpha1Publications(t *testing.T) {
	authorizer, err := NewAuthorizer(versionStore{version: store.AgentVersion{
		TenantID: "tenant-a", Name: "legacy", Version: "1",
		Spec: json.RawMessage("{\"runtimeClassPolicy\":{\"allowed\":[\"oci\"]}}"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := authorizer.Authorize(context.Background(), "tenant-a", "legacy@1", Tool, "legacy.tool"); err != nil {
		t.Fatalf("legacy compatibility was broken: %v", err)
	}
}
