// Package capability enforces immutable AgentVersion grants at Gateway
// boundaries without coupling the Agent manifest vocabulary to persistence.
package capability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/CloudEdgeCore/AgentOS/internal/kernel/agentversion"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/store"
)

type Kind string

const (
	Tool   Kind = "tool"
	Model  Kind = "model"
	Memory Kind = "memory"
	Secret Kind = "secret"
)

var ErrDenied = errors.New("agent version capability denied")

// Authorizer resolves the immutable AgentVersion and enforces its symbolic
// grants. Legacy publications (no runtimes declaration) retain their
// tenant-policy behavior during the v1alpha1 compatibility window.
type Authorizer struct {
	versions store.AgentVersionStore
}

func NewAuthorizer(versions store.AgentVersionStore) (*Authorizer, error) {
	if versions == nil {
		return nil, errors.New("agent version store is required")
	}
	return &Authorizer{versions: versions}, nil
}

func (a *Authorizer) Authorize(
	ctx context.Context,
	tenantID, agentVersionRef string,
	kind Kind,
	candidates ...string,
) error {
	if a == nil || a.versions == nil {
		return fmt.Errorf("%w: capability authorizer is not configured", ErrDenied)
	}
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(agentVersionRef) == "" {
		return fmt.Errorf("%w: tenant and agent version reference are required", ErrDenied)
	}
	version, err := a.versions.GetAgentVersionByRef(ctx, tenantID, agentVersionRef)
	if err != nil {
		return fmt.Errorf("%w: resolve immutable agent version: %v", ErrDenied, err)
	}
	var spec agentversion.Spec
	if err := json.Unmarshal(version.Spec, &spec); err != nil {
		return fmt.Errorf("%w: decode immutable agent version: %v", ErrDenied, err)
	}
	if len(spec.Runtimes) == 0 {
		return nil
	}
	if spec.Capabilities == nil {
		return fmt.Errorf("%w: portable publication has no capability declaration", ErrDenied)
	}
	var grants []string
	switch kind {
	case Tool:
		grants = spec.Capabilities.Tools
	case Model:
		grants = spec.Capabilities.Models
	case Memory:
		grants = spec.Capabilities.Memory
	case Secret:
		grants = spec.Capabilities.Secrets
	default:
		return fmt.Errorf("%w: unknown capability kind %q", ErrDenied, kind)
	}
	for _, grant := range grants {
		for _, candidate := range candidates {
			if matches(grant, candidate) {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: %s candidates %q are not granted to %s", ErrDenied, kind, candidates, agentVersionRef)
}

func matches(pattern, value string) bool {
	if pattern == value {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return false
	}
	quoted := regexp.QuoteMeta(pattern)
	expression := "^" + strings.ReplaceAll(quoted, "\\*", ".*") + "$"
	matched, err := regexp.MatchString(expression, value)
	return err == nil && matched
}
