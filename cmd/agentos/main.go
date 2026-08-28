// Command agentos is the stable developer and operator workflow.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/CloudEdgeCore/AgentOS/internal/kernel/agentpkg"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/agentversion"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/money"
	runtimeadapter "github.com/CloudEdgeCore/AgentOS/internal/runtime/adapter"
	"github.com/CloudEdgeCore/AgentOS/internal/version"
	"github.com/CloudEdgeCore/AgentOS/sdk/agent"
	"github.com/CloudEdgeCore/AgentOS/sdk/conformance"
	"github.com/google/uuid"
)

const maxResponseBytes = 2 << 20

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "agentos:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: agentos <version|init|migrate|validate|package|sign|publish|run|logs|workflow|research|metrics|runtime|conformance> [flags]")
	}
	switch args[0] {
	case "version":
		return runVersion(args[1:], stdout, stderr)
	case "init":
		return runInit(args[1:], stdout, stderr)
	case "migrate":
		return runMigrate(args[1:], stdout, stderr)
	case "validate":
		return runValidate(args[1:], stdout, stderr)
	case "package":
		return runPackage(args[1:], stdout, stderr)
	case "sign":
		return runSignPackage(args[1:], stdout, stderr)
	case "publish":
		return runPublish(args[1:], stdout, stderr)
	case "run":
		return runSubmit(args[1:], stdout, stderr)
	case "logs":
		return runLogs(args[1:], stdout, stderr)
	case "workflow":
		return runWorkflow(args[1:], stdout, stderr)
	case "research":
		return runResearch(args[1:], stdout, stderr)
	case "metrics":
		return runMetrics(args[1:], stdout, stderr)
	case "runtime":
		return runRuntime(args[1:], stdout, stderr)
	case "conformance":
		return runConformance(args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runConformance(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("conformance", flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("endpoint", "http://127.0.0.1:8088", "Runtime Interface endpoint")
	timeout := flags.Duration("timeout", 2*time.Minute, "conformance timeout (maximum 10m)")
	legacy := flags.Bool("legacy-v1alpha1", false, "test the deprecated N-1 Runtime Interface")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *timeout <= 0 || *timeout > 10*time.Minute {
		return errors.New("-timeout must be between 1ns and 10m")
	}
	client, err := agent.NewClient(*endpoint, nil)
	if *legacy {
		client, err = agent.NewLegacyClient(*endpoint, nil)
	}
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	report, err := conformance.Run(ctx, client)
	result := map[string]any{
		"schema": "agentos.conformance/v1", "endpoint": *endpoint,
		"passed": err == nil, "report": report,
	}
	encoded, encodeErr := json.Marshal(result)
	if encodeErr != nil {
		return encodeErr
	}
	if _, encodeErr = fmt.Fprintln(stdout, string(encoded)); encodeErr != nil {
		return encodeErr
	}
	if err != nil {
		return fmt.Errorf("conformance failed: %w", err)
	}
	return nil
}

func runVersion(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("version", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "emit machine-readable release information")
	if err := flags.Parse(args); err != nil {
		return err
	}
	info := version.Current()
	if *jsonOutput {
		encoded, err := json.Marshal(info)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, string(encoded))
		return err
	}
	_, err := fmt.Fprintf(stdout, "%s %s (semver v%s, %s)\n", info.Product, info.ProductVersion, info.SemVer, info.ReleaseStage)
	return err
}

func runMigrate(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("migrate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("manifest", "agent.json", "legacy or stable Agent Manifest file")
	out := flags.String("out", "agent.v1.json", "stable v1 Agent Manifest output")
	if err := flags.Parse(args); err != nil {
		return err
	}
	manifest, _, _, err := loadManifest(*path)
	if err != nil {
		return err
	}
	promoted, err := manifest.PromoteToV1()
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(promoted, "", "  ")
	if err != nil {
		return err
	}
	if err := writeExclusive(*out, append(encoded, '\n')); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "stable manifest written to %s ref=%s\n", *out, promoted.Ref())
	return err
}

func runInit(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	flags.SetOutput(stderr)
	directory := flags.String("dir", ".", "new Agent project directory")
	name := flags.String("name", "", "Agent name (defaults to directory name)")
	language := flags.String("adapter", "go", "go, python, langgraph, or a2a")
	endpoint := flags.String("endpoint", "", "embed a concrete Runtime Interface endpoint in the manifest (legacy single-environment flow; empty writes an environment-independent logical binding reference)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	agentName := *name
	if agentName == "" {
		agentName = filepath.Base(filepath.Clean(*directory))
	}
	if err := agentversion.ValidateName(agentName); err != nil {
		return err
	}
	// The default entrypoint is a logical binding reference, not a network
	// address: deployment endpoints live in the operator's runtime binding
	// file (see agentos-runtime-adapter -runtime-bindings), so one
	// AgentVersion digest deploys unchanged across dev/staging/prod and
	// regions. -endpoint preserves the legacy single-environment flow.
	entrypoint := runtimeadapter.BindingScheme + agentName + "/remote"
	if strings.TrimSpace(*endpoint) != "" {
		entrypoint = strings.TrimRight(*endpoint, "/")
	}
	var runtimeABI, sourceName, source string
	switch *language {
	case "go":
		runtimeABI, sourceName, source = "agentos.go-native/v1", "main.go", goTemplate
	case "python":
		runtimeABI, sourceName, source = "agentos.python-remote/v1", "server.py", pythonTemplate
	case "langgraph":
		runtimeABI, sourceName, source = "agentos.langgraph/v1", "server.py", langGraphTemplate
	case "a2a":
		runtimeABI, sourceName, source = "agentos.a2a/v1", "server.py", a2aTemplate
	default:
		return errors.New("-adapter must be go, python, langgraph, or a2a")
	}
	manifest := agentversion.Manifest{
		APIVersion: agentversion.ManifestAPIVersion, Kind: agentversion.ManifestKind,
		Metadata: agentversion.Metadata{Name: agentName, Version: "0.1.0", Namespace: "default"},
		Spec: agentversion.Spec{
			RuntimeClassPolicy: agentversion.RuntimeClassPolicy{Allowed: []string{"remote"}, Preferred: "remote"},
			Runtimes: []agentversion.RuntimeTarget{{
				Class: "remote", Interface: agentversion.RuntimeInterfaceV1,
				RuntimeABI: runtimeABI, Entrypoint: []string{entrypoint},
			}},
			Capabilities: &agentversion.Capabilities{
				Tools: []string{}, Models: []string{}, Memory: []string{}, Secrets: []string{},
			},
			Resources:  &agentversion.ResourceLimits{CPUMillis: 250, MemoryMiB: 256},
			Budget:     &agentversion.Budget{Tokens: 10_000, CostMicroUSD: money.MustFromUSD(1), ToolCalls: 20, WallSeconds: 300},
			Checkpoint: &agentversion.CheckpointPolicy{Mode: agentversion.CheckpointLogical, SchemaVersion: agentName + "/v1"},
		},
	}
	if err := manifest.Validate(); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*directory, 0o750); err != nil {
		return err
	}
	if err := writeExclusive(filepath.Join(*directory, "agent.json"), append(encoded, '\n')); err != nil {
		return err
	}
	if err := writeExclusive(filepath.Join(*directory, sourceName), []byte(source)); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "initialized %s adapter project at %s\n", *language, *directory)
	return nil
}

func runValidate(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("manifest", "agent.json", "Agent Manifest file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	manifest, _, digest, err := loadManifest(*path)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "manifest OK ref=%s digest=%x runtimes=%d\n", manifest.Ref(), digest, len(manifest.Spec.Runtimes))
	return nil
}

func runPackage(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("package", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("manifest", "agent.json", "Agent Manifest file")
	out := flags.String("out", "package-manifest.json", "unsigned package manifest output")
	builder := flags.String("builder", "", "builder identity")
	workflow := flags.String("workflow", "", "workflow identity")
	commit := flags.String("git-commit", "", "source commit")
	builtAtText := flags.String("built-at", "", "RFC3339 build timestamp")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *builder == "" || *workflow == "" || *commit == "" || *builtAtText == "" {
		return errors.New("-builder, -workflow, -git-commit and -built-at are required")
	}
	builtAt, err := time.Parse(time.RFC3339, *builtAtText)
	if err != nil {
		return err
	}
	manifest, _, _, err := loadManifest(*path)
	if err != nil {
		return err
	}
	unsigned, err := agentpkg.FromAgentManifest(manifest, agentpkg.Provenance{
		Builder: *builder, BuildWorkflow: *workflow, GitCommit: *commit, BuiltAt: builtAt.UTC(),
	})
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(unsigned)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, encoded, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "package manifest written to %s\n", *out)
	return nil
}

func runSignPackage(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("sign", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("package-manifest", "package-manifest.json", "unsigned package manifest")
	keyID := flags.String("key-id", "", "trusted signing key identity")
	privateKeyFile := flags.String("private-key-file", "", "file containing base64 raw ed25519 private key")
	out := flags.String("out", "package.json", "signed package output")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *keyID == "" || *privateKeyFile == "" {
		return errors.New("-key-id and -private-key-file are required")
	}
	var manifest agentpkg.Manifest
	if err := decodeFileStrict(*path, &manifest); err != nil {
		return err
	}
	keyText, err := os.ReadFile(*privateKeyFile)
	if err != nil {
		return err
	}
	privateKey, err := agentpkg.DecodePrivateKey(strings.TrimSpace(string(keyText)))
	if err != nil {
		return err
	}
	pkg, err := agentpkg.Sign(manifest, &agentpkg.SigningKey{ID: *keyID, PrivateKey: privateKey})
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(pkg)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, encoded, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "signed package written to %s\n", *out)
	return nil
}

func runPublish(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("publish", flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("endpoint", "http://127.0.0.1:8080", "Control API endpoint")
	manifestPath := flags.String("manifest", "agent.json", "Agent Manifest file")
	packagePath := flags.String("package", "", "optional signed package")
	idempotency := flags.String("idempotency-key", "", "safe publish idempotency key")
	if err := flags.Parse(args); err != nil {
		return err
	}
	manifest, _, _, err := loadManifest(*manifestPath)
	if err != nil {
		return err
	}
	body := map[string]any{"manifest": manifest}
	if *packagePath != "" {
		var pkg agentpkg.Package
		if err := decodeFileStrict(*packagePath, &pkg); err != nil {
			return err
		}
		body["package"] = pkg
	}
	key := *idempotency
	if key == "" {
		key, err = randomKey("publish")
		if err != nil {
			return err
		}
	}
	return controlRequest(context.Background(), http.MethodPost, *endpoint, "/v1/agent-versions", key, body, stdout)
}

func runSubmit(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("endpoint", "http://127.0.0.1:8080", "Control API endpoint")
	agentRef := flags.String("agent", "", "published name@version")
	goal := flags.String("goal", "", "task goal")
	namespace := flags.String("namespace", "default", "task namespace")
	specPath := flags.String("spec", "", "optional workload spec JSON file")
	idempotency := flags.String("idempotency-key", "", "safe task idempotency key")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if _, _, _, err := agentversion.ParseRef(*agentRef); err != nil {
		return err
	}
	if strings.TrimSpace(*goal) == "" {
		return errors.New("-goal is required")
	}
	spec := json.RawMessage("{}")
	if *specPath != "" {
		encoded, err := os.ReadFile(*specPath)
		if err != nil {
			return err
		}
		if !json.Valid(encoded) {
			return errors.New("workload spec is not valid JSON")
		}
		spec = encoded
	}
	key := *idempotency
	var err error
	if key == "" {
		key, err = randomKey("task")
		if err != nil {
			return err
		}
	}
	body := map[string]any{
		"agentVersionRef": *agentRef, "goal": *goal, "namespace": *namespace, "spec": spec,
	}
	return controlRequest(context.Background(), http.MethodPost, *endpoint, "/v1/tasks", key, body, stdout)
}

func runLogs(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("logs", flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("endpoint", "http://127.0.0.1:8080", "Control API endpoint")
	taskID := flags.String("task", "", "task UUID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*taskID) == "" {
		return errors.New("-task is required")
	}
	if _, err := uuid.Parse(*taskID); err != nil {
		return errors.New("-task must be a UUID")
	}
	base, err := normalizeEndpoint(*endpoint)
	if err != nil {
		return err
	}
	request, err := http.NewRequest(http.MethodGet, base+"/v1/tasks/"+url.PathEscape(*taskID)+"/events", nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "text/event-stream")
	setBearer(request)
	response, err := (&http.Client{}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return responseError(response)
	}
	_, err = io.Copy(stdout, io.LimitReader(response.Body, 64<<20))
	return err
}

func controlRequest(ctx context.Context, method, endpoint, path, idempotency string, body any, stdout io.Writer) error {
	headers := map[string]string{}
	if idempotency != "" {
		headers["Idempotency-Key"] = idempotency
	}
	reply, err := controlRequestBytes(ctx, method, endpoint, path, headers, body)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, string(reply))
	return err
}

func controlRequestBytes(ctx context.Context, method, endpoint, path string, headers map[string]string, body any) ([]byte, error) {
	base, err := normalizeEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	var requestBody io.Reader
	if body != nil {
		encoded, encodeErr := json.Marshal(body)
		if encodeErr != nil {
			return nil, encodeErr
		}
		requestBody = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, base+path, requestBody)
	if err != nil {
		return nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	setBearer(request)
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, responseError(response)
	}
	reply, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(reply) > maxResponseBytes {
		return nil, errors.New("control API response exceeds 2 MiB")
	}
	if !json.Valid(reply) {
		return nil, errors.New("control API returned invalid JSON")
	}
	return reply, nil
}

func loadManifest(path string) (agentversion.Manifest, json.RawMessage, [32]byte, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return agentversion.Manifest{}, nil, [32]byte{}, err
	}
	return agentversion.DecodeManifest(encoded)
}

func decodeFileStrict(path string, target any) error {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("file must contain exactly one JSON document")
	}
	return nil
}

func normalizeEndpoint(endpoint string) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("endpoint must be an absolute HTTP(S) URL")
	}
	return parsed.String(), nil
}

func responseError(response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	return fmt.Errorf("control API returned HTTP %d: %s", response.StatusCode, body)
}

func setBearer(request *http.Request) {
	if token := strings.TrimSpace(os.Getenv("AGENTOS_TOKEN")); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
}

func randomKey(prefix string) (string, error) {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(value), nil
}

func writeExclusive(path string, content []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

var goTemplate = `package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/CloudEdgeCore/AgentOS/sdk/agent"
)

type runtime struct{ states sync.Map }

func (r *runtime) Run(ctx context.Context, request agent.StartRequest, emit agent.Emitter) (json.RawMessage, error) {
	if err := emit("agent.started", json.RawMessage("{}")); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	output, err := json.Marshal(map[string]any{"goal": request.Goal, "input": request.Input})
	if err == nil {
		r.states.Store(request.ExecutionID, json.RawMessage(output))
	}
	return output, err
}

func (r *runtime) Checkpoint(_ context.Context, id string) (agent.Checkpoint, error) {
	state, ok := r.states.Load(id)
	if !ok {
		return agent.Checkpoint{}, fmt.Errorf("execution %s has no state", id)
	}
	return agent.Checkpoint{SchemaVersion: "agent/v1", State: state.(json.RawMessage), CreatedAt: time.Now().UTC()}, nil
}

func (r *runtime) Restore(_ context.Context, request agent.RestoreRequest) error {
	r.states.Store(request.ExecutionID, request.Checkpoint.State)
	return nil
}

func main() {
	host, err := agent.NewHost(&runtime{}, agent.HostOptions{Adapter: "go-native"})
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(http.ListenAndServe("127.0.0.1:8088", host))
}
`

var pythonTemplate = `import threading
import time
from agentos_runtime import serve

class Runtime:
    def __init__(self): self.states = {}
    def run(self, request, emit, stop_event: threading.Event):
        emit("agent.started", {})
        if stop_event.is_set(): raise RuntimeError("execution cancelled")
        output = {"goal": request["goal"], "input": request["input"]}
        self.states[request["executionId"]] = output
        return output
    def checkpoint(self, execution_id):
        return {"schemaVersion": "agent/v1", "state": self.states[execution_id], "createdAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}
    def restore(self, execution_id, checkpoint): self.states[execution_id] = checkpoint["state"]

serve(Runtime(), "python-remote").serve_forever()
`

var langGraphTemplate = `from agentos_runtime import serve
from agentos_langgraph import LangGraphRuntime

class Graph:
    def invoke(self, value, *, config):
        return {"input": value, "thread": config["configurable"]["thread_id"]}

serve(LangGraphRuntime(Graph()), "langgraph").serve_forever()
`

var a2aTemplate = `from agentos_runtime import serve
from agentos_a2a import A2ARuntime

class Client:
    def send_message(self, message):
        return {"id": message["messageId"], "status": {"state": "completed"}, "artifacts": [{"parts": message["parts"]}]}

serve(A2ARuntime(Client()), "a2a").serve_forever()
`
