// Runtime bindings decouple mutable deployment endpoints from immutable
// AgentVersions. A manifest's remote-class entrypoint is a logical
// reference (agentos-binding://agent-name/remote); operators map version
// refs or agent names to concrete Runtime Interface endpoints in a binding
// file, so promoting one AgentVersion digest across dev/staging/prod or
// across regions never requires re-signing or re-publishing it.
package adapter

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// BindingScheme is the URI scheme of a logical (environment-independent)
// runtime entrypoint embedded in an immutable AgentVersion.
const BindingScheme = "agentos-binding://"

// RuntimeBinding maps one agent version reference — or every version of one
// agent name via the "name@*" wildcard — to a concrete Runtime Interface
// endpoint.
type RuntimeBinding struct {
	AgentVersionRef string `json:"agentVersionRef"`
	Endpoint        string `json:"endpoint"`
}

type bindingFile struct {
	Bindings []RuntimeBinding `json:"bindings"`
}

// RuntimeBindings is the resolved, immutable view of a binding file.
type RuntimeBindings struct {
	exact    map[string]string
	wildcard map[string]string
}

// LoadRuntimeBindings reads a binding file. An empty path yields nil (no
// bindings configured). Endpoints must be absolute http(s) URLs and
// references must be name@version or name@* shapes.
func LoadRuntimeBindings(path string) (*RuntimeBindings, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read runtime bindings file: %w", err)
	}
	var file bindingFile
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("decode runtime bindings file: %w", err)
	}
	bindings := &RuntimeBindings{exact: map[string]string{}, wildcard: map[string]string{}}
	for _, binding := range file.Bindings {
		name, version, err := parseVersionRef(binding.AgentVersionRef)
		if err != nil {
			return nil, fmt.Errorf("runtime binding %q: %w", binding.AgentVersionRef, err)
		}
		parsed, err := url.Parse(binding.Endpoint)
		if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return nil, fmt.Errorf("runtime binding %q: endpoint must be an absolute http(s) URL", binding.AgentVersionRef)
		}
		if version == "*" {
			bindings.wildcard[name] = strings.TrimRight(binding.Endpoint, "/")
			continue
		}
		bindings.exact[binding.AgentVersionRef] = strings.TrimRight(binding.Endpoint, "/")
	}
	return bindings, nil
}

// Resolve returns the bound endpoint for one agent version reference: an
// exact ref match first, then the agent name's wildcard binding.
func (b *RuntimeBindings) Resolve(agentVersionRef string) (string, bool) {
	if b == nil {
		return "", false
	}
	if endpoint, ok := b.exact[agentVersionRef]; ok {
		return endpoint, true
	}
	name, _, err := parseVersionRef(agentVersionRef)
	if err != nil {
		return "", false
	}
	endpoint, ok := b.wildcard[name]
	return endpoint, ok
}

func parseVersionRef(ref string) (name, version string, err error) {
	name, version, ok := strings.Cut(strings.TrimSpace(ref), "@")
	if !ok || strings.TrimSpace(name) == "" || strings.TrimSpace(version) == "" {
		return "", "", fmt.Errorf("reference must be name@version or name@*")
	}
	return name, version, nil
}

// IsLogicalEntrypoint reports whether a manifest entrypoint is a logical
// binding reference that must be resolved through runtime bindings (as
// opposed to an environment-embedded concrete URL).
func IsLogicalEntrypoint(entrypoint string) bool {
	return strings.HasPrefix(entrypoint, BindingScheme)
}
