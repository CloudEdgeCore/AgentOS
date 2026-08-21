package otel

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestInitIsNoOpWithoutEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_SERVICE_NAME", "")
	t.Setenv("OTEL_SDK_DISABLED", "")
	shutdown, err := Init(context.Background())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Init must return a shutdown function")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("no-op shutdown: %v", err)
	}
	// Console slog must keep working after no-op init.
	slog.Info("log after no-op init", "k", "v")
}

func TestInitRespectsSDKDisabled(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "127.0.0.1:4317")
	shutdown, err := Init(context.Background())
	if err != nil {
		t.Fatalf("Init with disabled SDK: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestInitWithUnreachableEndpointStillServes(t *testing.T) {
	// Exporters are asynchronous: an unreachable endpoint must not fail Init
	// or break the slog pipeline; the collector being down is an operability
	// concern, not a startup failure.
	t.Setenv("OTEL_SDK_DISABLED", "")
	t.Setenv("OTEL_SERVICE_NAME", "test-service")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "127.0.0.1:1")
	shutdown, err := Init(context.Background())
	if err != nil {
		t.Fatalf("Init with unreachable endpoint: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Init must return a shutdown function")
	}
	var captured strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&captured, nil)))
	slog.Info("structured log still works")
	slog.SetDefault(old)
	if !strings.Contains(captured.String(), "structured log still works") {
		t.Fatalf("console log output missing: %q", captured.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = shutdown(ctx)
}
