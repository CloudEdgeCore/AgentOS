// Command agentos-conformance certifies a running third-party adapter through
// the public Runtime Interface only.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/CloudEdgeCore/AgentOS/internal/version"
	"github.com/CloudEdgeCore/AgentOS/sdk/agent"
	"github.com/CloudEdgeCore/AgentOS/sdk/conformance"
)

type certification struct {
	Schema         string             `json:"schema"`
	ProductVersion string             `json:"productVersion"`
	Endpoint       string             `json:"endpoint"`
	Passed         bool               `json:"passed"`
	StartedAt      time.Time          `json:"startedAt"`
	CompletedAt    time.Time          `json:"completedAt"`
	Report         conformance.Report `json:"report"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "agentos-conformance:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("agentos-conformance", flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("endpoint", "", "Runtime Interface base URL")
	timeout := flags.Duration("timeout", 2*time.Minute, "certification timeout")
	legacy := flags.Bool("legacy-v1alpha1", false, "certify the deprecated N-1 endpoint")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *endpoint == "" || *timeout <= 0 || *timeout > 10*time.Minute {
		return errors.New("-endpoint and a timeout between 1ns and 10m are required")
	}
	client, err := agent.NewClient(*endpoint, nil)
	if *legacy {
		client, err = agent.NewLegacyClient(*endpoint, nil)
	}
	if err != nil {
		return err
	}
	started := time.Now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	report, err := conformance.Run(ctx, client)
	result := certification{
		Schema: "agentos.conformance/v1", ProductVersion: version.ProductVersion,
		Endpoint: *endpoint, Passed: err == nil, StartedAt: started,
		CompletedAt: time.Now().UTC(), Report: report,
	}
	encoded, encodeErr := json.Marshal(result)
	if encodeErr != nil {
		return encodeErr
	}
	if _, encodeErr = fmt.Fprintln(stdout, string(encoded)); encodeErr != nil {
		return encodeErr
	}
	if err != nil {
		return fmt.Errorf("certification failed: %w", err)
	}
	return nil
}
