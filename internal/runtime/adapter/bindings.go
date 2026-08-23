// Runtime bindings decouple mutable deployment endpoints from immutable
// AgentVersions. A manifest's remote-class entrypoint is a logical
// reference (agentos-binding://agent-name/remote); operators map version
// refs or agent names to concrete Runtime Interface endpoints in a binding
// file, so promoting one AgentVersion digest across dev/staging/prod or
// across regions never requires re-signing or re-publishing it.
//
// The loader is also a security boundary (P0-02): every endpoint is
// validated against the EndpointPolicy before the process starts serving.
// Plaintext HTTP is tolerated only on loopback (a co-located runtime);
// a remote runtime must be HTTPS whose server certificate is verified -
// optionally pinned to a private CA and a client certificate (mTLS) declared
// on the binding. Binding files are mutable deployment state, so rotating a
// certificate never republishes the AgentVersion.
package adapter

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/CloudEdgeCore/AgentOS/sdk/agent"
)

// BindingScheme is the URI scheme of a logical (environment-independent)
// runtime entrypoint embedded in an immutable AgentVersion.
const BindingScheme = "agentos-binding://"

// EndpointPolicy is the transport-security boundary for Runtime Interface
// endpoints. The zero value is the production policy: plaintext HTTP is
// allowed only on loopback; remote endpoints must be HTTPS. The structural
// rules are shared with the runtime client (agent.ValidateBaseURL), so an
// endpoint rejected by the client is rejected at load time.
type EndpointPolicy struct {
	// AllowPlaintextRemote explicitly opts a development deployment into
	// plaintext HTTP to non-loopback runtimes. It must stay false in
	// production: a plaintext remote endpoint exposes the Runtime Interface
	// (workload, identity, results) to interception and spoofing.
	AllowPlaintextRemote bool
}

// Validate checks one endpoint against the policy.
func (p EndpointPolicy) Validate(endpoint string) error {
	if err := agent.ValidateBaseURL(endpoint); err != nil {
		return err
	}
	parsed, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil {
		return err
	}
	if parsed.Scheme == "http" && !p.AllowPlaintextRemote && !IsLoopbackHost(parsed.Hostname()) {
		return fmt.Errorf("plaintext HTTP runtime endpoint %q is remote: use HTTPS or an explicitly acknowledged development policy", endpoint)
	}
	return nil
}

// IsLoopbackHost reports whether a URL hostname (no port) is a loopback
// address: the literal "localhost" or a loopback IP.
func IsLoopbackHost(hostname string) bool {
	if strings.EqualFold(hostname, "localhost") {
		return true
	}
	ip := net.ParseIP(hostname)
	return ip != nil && ip.IsLoopback()
}

// RuntimeBinding maps one agent version reference - or every version of one
// agent name via the "name@*" wildcard - to a concrete Runtime Interface
// endpoint plus optional deployment metadata.
type RuntimeBinding struct {
	AgentVersionRef string `json:"agentVersionRef"`
	Endpoint        string `json:"endpoint"`
	// TLSServerName pins the TLS SNI/verification name when it differs from
	// the endpoint host (for example a SPIFFE-identified runtime behind a
	// load balancer).
	TLSServerName string `json:"tlsServerName,omitempty"`
	// RuntimePool and RuntimeClass bind the endpoint to the scheduler's pool
	// identity; Region and Weight are routing hints. They are metadata for
	// operators and audit - scheduling still flows through the kernel.
	RuntimePool  string `json:"runtimePool,omitempty"`
	RuntimeClass string `json:"runtimeClass,omitempty"`
	Region       string `json:"region,omitempty"`
	Weight       int    `json:"weight,omitempty"`
	// TLS optionally loads certificate material for this endpoint's client:
	// a private trust bundle (server verification), and a client certificate
	// pair (mutual TLS). Paths are read once at load.
	TLS *BindingTLS `json:"tls,omitempty"`
}

// BindingTLS names the certificate files of one binding endpoint.
type BindingTLS struct {
	// CAFile is the PEM trust bundle used to verify the runtime's server
	// certificate. Empty means the system roots.
	CAFile string `json:"caFile,omitempty"`
	// CertFile and KeyFile are the client certificate pair presented to the
	// runtime (mutual TLS). Both must be set together.
	CertFile string `json:"certFile,omitempty"`
	KeyFile  string `json:"keyFile,omitempty"`
}

// resolved closes over the load-time state of one binding.
type resolvedBinding struct {
	RuntimeBinding
	tlsConfig *tls.Config
}

type bindingFile struct {
	Bindings []RuntimeBinding `json:"bindings"`
}

// RuntimeBindings is the resolved, immutable view of a binding file.
type RuntimeBindings struct {
	exact    map[string]resolvedBinding
	wildcard map[string]resolvedBinding
	policy   EndpointPolicy
}

// LoadRuntimeBindings reads a binding file with the production endpoint
// policy. An empty path yields nil (no bindings configured). Endpoints must
// be absolute http(s) URLs (remote endpoints require HTTPS) and references
// must be name@version or name@* shapes; duplicate references are rejected
// instead of silently overriding each other.
func LoadRuntimeBindings(path string) (*RuntimeBindings, error) {
	return LoadRuntimeBindingsFor(path, EndpointPolicy{})
}

// LoadRuntimeBindingsFor reads a binding file under an explicit endpoint
// policy (used by deployments that acknowledge plaintext remote runtimes).
func LoadRuntimeBindingsFor(path string, policy EndpointPolicy) (*RuntimeBindings, error) {
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
	bindings := &RuntimeBindings{exact: map[string]resolvedBinding{}, wildcard: map[string]resolvedBinding{}, policy: policy}
	seenExact := map[string]string{}
	seenWildcard := map[string]string{}
	for _, binding := range file.Bindings {
		name, version, err := parseVersionRef(binding.AgentVersionRef)
		if err != nil {
			return nil, fmt.Errorf("runtime binding %q: %w", binding.AgentVersionRef, err)
		}
		if err := policy.Validate(binding.Endpoint); err != nil {
			return nil, fmt.Errorf("runtime binding %q: %w", binding.AgentVersionRef, err)
		}
		if err := binding.validateMetadata(); err != nil {
			return nil, fmt.Errorf("runtime binding %q: %w", binding.AgentVersionRef, err)
		}
		tlsConfig, err := binding.TLS.load()
		if err != nil {
			return nil, fmt.Errorf("runtime binding %q: %w", binding.AgentVersionRef, err)
		}
		resolved := resolvedBinding{RuntimeBinding: binding, tlsConfig: tlsConfig}
		resolved.Endpoint = strings.TrimRight(binding.Endpoint, "/")
		if version == "*" {
			if previous, duplicate := seenWildcard[name]; duplicate {
				return nil, fmt.Errorf("runtime binding %q duplicates the wildcard binding for %q (previously %q): remove one instead of relying on file order",
					binding.AgentVersionRef, name, previous)
			}
			seenWildcard[name] = binding.AgentVersionRef
			bindings.wildcard[name] = resolved
			continue
		}
		if previous, duplicate := seenExact[binding.AgentVersionRef]; duplicate {
			return nil, fmt.Errorf("runtime binding %q is declared twice (previously %q): remove one instead of relying on file order",
				binding.AgentVersionRef, previous)
		}
		seenExact[binding.AgentVersionRef] = binding.AgentVersionRef
		bindings.exact[binding.AgentVersionRef] = resolved
	}
	return bindings, nil
}

// validateMetadata bounds the optional deployment metadata.
func (b RuntimeBinding) validateMetadata() error {
	for name, value := range map[string]string{
		"tlsServerName": b.TLSServerName, "runtimePool": b.RuntimePool,
		"runtimeClass": b.RuntimeClass, "region": b.Region,
	} {
		if len(value) > 256 {
			return fmt.Errorf("%s must not exceed 256 bytes", name)
		}
	}
	if b.Weight < 0 || b.Weight > 1000 {
		return fmt.Errorf("weight must be between 0 and 1000")
	}
	if b.TLS == nil {
		if b.TLSServerName != "" {
			return fmt.Errorf("tlsServerName requires the tls section")
		}
		return nil
	}
	if (b.TLS.CertFile == "") != (b.TLS.KeyFile == "") {
		return fmt.Errorf("tls certFile and keyFile must be set together")
	}
	if b.TLS.CertFile == "" && b.TLS.CAFile == "" && b.TLSServerName == "" {
		return fmt.Errorf("tls section requires caFile, certFile/keyFile, or tlsServerName")
	}
	return nil
}

// load reads the binding's certificate material into a TLS configuration.
func (t *BindingTLS) load() (*tls.Config, error) {
	if t == nil {
		return nil, nil
	}
	config := &tls.Config{MinVersion: tls.VersionTLS12}
	if t.CAFile != "" {
		bundle, err := os.ReadFile(t.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read TLS trust bundle: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(bundle) {
			return nil, fmt.Errorf("TLS trust bundle %s contains no PEM certificates", t.CAFile)
		}
		config.RootCAs = pool
	}
	if t.CertFile != "" && t.KeyFile != "" {
		certificate, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load TLS client certificate: %w", err)
		}
		config.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return &certificate, nil
		}
	}
	return config, nil
}

// Resolve returns the bound endpoint for one agent version reference: an
// exact ref match first, then the agent name's wildcard binding.
func (b *RuntimeBindings) Resolve(agentVersionRef string) (string, bool) {
	binding, ok := b.ResolveBinding(agentVersionRef)
	if !ok {
		return "", false
	}
	return binding.Endpoint, true
}

// ResolveBinding returns the full binding (endpoint plus deployment
// metadata and TLS configuration) for one agent version reference.
func (b *RuntimeBindings) ResolveBinding(agentVersionRef string) (RuntimeBinding, bool) {
	if b == nil {
		return RuntimeBinding{}, false
	}
	if binding, ok := b.exact[agentVersionRef]; ok {
		return binding.RuntimeBinding, true
	}
	name, _, err := parseVersionRef(agentVersionRef)
	if err != nil {
		return RuntimeBinding{}, false
	}
	if binding, ok := b.wildcard[name]; ok {
		return binding.RuntimeBinding, true
	}
	return RuntimeBinding{}, false
}

// tlsConfigFor returns the load-time TLS configuration of the binding that
// covers the reference (nil when the endpoint uses ambient TLS).
func (b *RuntimeBindings) tlsConfigFor(agentVersionRef string) *tls.Config {
	if b == nil {
		return nil
	}
	if binding, ok := b.exact[agentVersionRef]; ok {
		return binding.tlsConfig
	}
	name, _, err := parseVersionRef(agentVersionRef)
	if err != nil {
		return nil
	}
	if binding, ok := b.wildcard[name]; ok {
		return binding.tlsConfig
	}
	return nil
}

// ValidateEndpointOverride guards the explicit worker endpoint against
// routing every assignment of a multi-version (shared) worker to one
// endpoint: an explicit endpoint plus runtime bindings is a dedicated-worker
// configuration that must be acknowledged as such.
func ValidateEndpointOverride(endpoint string, bindings *RuntimeBindings, dedicated bool) error {
	if endpoint == "" || bindings == nil {
		return nil
	}
	if dedicated {
		return nil
	}
	return fmt.Errorf("an explicit worker endpoint together with runtime bindings routes every assignment to one endpoint; pass dedicated=true only for single-agent-version workers")
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
