package workload

import (
	"encoding/json"
	"testing"
)

func TestImagePinValidation(t *testing.T) {
	valid := Image{Ref: "example.com/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid pin rejected: %v", err)
	}
	if !valid.Pinned() {
		t.Fatal("digest-pinned image must report Pinned")
	}

	for name, image := range map[string]Image{
		"empty ref":     {Ref: "", Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		"bad digest alg": {Ref: "example.com/agent", Digest: "md5:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		"short digest":   {Ref: "example.com/agent", Digest: "sha256:abc"},
		"uppercase hex":  {Ref: "example.com/agent", Digest: "sha256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
	} {
		if err := image.Validate(); err == nil {
			t.Fatalf("%s must fail validation", name)
		}
	}

	// A mutable reference is legal in dev but not pinned.
	mutable := Image{Ref: "example.com/agent:latest"}
	if err := mutable.Validate(); err != nil {
		t.Fatalf("mutable dev ref rejected: %v", err)
	}
	if mutable.Pinned() {
		t.Fatal("mutable ref must not report Pinned")
	}
}

func TestImageCanonicalAndEqual(t *testing.T) {
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pinned := Image{Ref: "example.com/agent", Digest: digest}
	if pinned.Canonical() != "example.com/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("canonical = %q", pinned.Canonical())
	}
	if !pinned.Equal(Image{Ref: "example.com/agent", Digest: digest}) {
		t.Fatal("identical pins must be equal")
	}
	if pinned.Equal(Image{Ref: "example.com/other", Digest: digest}) {
		t.Fatal("different refs must not be equal")
	}
	if pinned.Equal(Image{Ref: "example.com/agent"}) {
		t.Fatal("pinned vs mutable must not be equal")
	}
}

func TestSpecDecodesImagePin(t *testing.T) {
	raw := json.RawMessage(`{"placement":{"runtimeClasses":["oci"]},"image":{"ref":"example.com/agent","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`)
	spec, err := Decode(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if spec.Image == nil || spec.Image.Ref != "example.com/agent" || !spec.Image.Pinned() {
		t.Fatalf("decoded image = %+v", spec.Image)
	}
}
