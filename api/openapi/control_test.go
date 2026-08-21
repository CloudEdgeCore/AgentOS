package openapi_test

import (
	"context"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func TestControlContractIsValidOpenAPI(t *testing.T) {
	for _, filename := range []string{"control-v1alpha1.yaml", "runtime-interface-v1alpha1.yaml"} {
		t.Run(filename, func(t *testing.T) {
			loader := openapi3.NewLoader()
			document, err := loader.LoadFromFile(filename)
			if err != nil {
				t.Fatalf("load OpenAPI contract: %v", err)
			}
			if err := document.Validate(context.Background()); err != nil {
				t.Fatalf("validate OpenAPI contract: %v", err)
			}
		})
	}
}
