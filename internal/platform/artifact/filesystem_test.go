package artifact

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestFilesystemRoundTripAndDeduplication(t *testing.T) {
	storage, err := NewFilesystem(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	first, err := storage.Put(context.Background(), "tenant-a", "text/plain", strings.NewReader("durable state"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := storage.Put(context.Background(), "tenant-a", "text/plain", strings.NewReader("durable state"))
	if err != nil {
		t.Fatal(err)
	}
	if first.URI != second.URI || first.SHA256 != second.SHA256 {
		t.Fatalf("content addressing is not deterministic: %+v %+v", first, second)
	}
	reader, err := storage.Open(context.Background(), "tenant-a", first)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if string(data) != "durable state" {
		t.Fatalf("artifact = %q", data)
	}
}

func TestFilesystemRejectsOversizeArtifact(t *testing.T) {
	storage, err := NewFilesystem(t.TempDir(), 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Put(context.Background(), "tenant-a", "text/plain", strings.NewReader("12345")); err == nil {
		t.Fatal("oversize artifact was accepted")
	}
}
