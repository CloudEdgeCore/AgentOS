package compatibility_test

import (
	"testing"

	gatewayv1 "github.com/CloudEdgeCore/AgentOS/gen/go/agentos/gateway/v1"
	gatewayv1alpha1 "github.com/CloudEdgeCore/AgentOS/gen/go/agentos/gateway/v1alpha1"
	modelv1 "github.com/CloudEdgeCore/AgentOS/gen/go/agentos/model/v1"
	modelv1alpha1 "github.com/CloudEdgeCore/AgentOS/gen/go/agentos/model/v1alpha1"
	runtimev1 "github.com/CloudEdgeCore/AgentOS/gen/go/agentos/runtime/v1"
	runtimev1alpha1 "github.com/CloudEdgeCore/AgentOS/gen/go/agentos/runtime/v1alpha1"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestPromotedProtobufsRemainAlphaWireCompatible(t *testing.T) {
	assertWireCompatible(t, runtimev1.File_agentos_runtime_v1_runtime_proto, runtimev1alpha1.File_agentos_runtime_v1alpha1_runtime_proto)
	assertWireCompatible(t, gatewayv1.File_agentos_gateway_v1_gateway_proto, gatewayv1alpha1.File_agentos_gateway_v1alpha1_gateway_proto)
	assertWireCompatible(t, modelv1.File_agentos_model_v1_model_proto, modelv1alpha1.File_agentos_model_v1alpha1_model_proto)
}

func assertWireCompatible(t *testing.T, stable, legacy protoreflect.FileDescriptor) {
	t.Helper()
	if stable.Messages().Len() != legacy.Messages().Len() || stable.Services().Len() != legacy.Services().Len() || stable.Enums().Len() != legacy.Enums().Len() {
		t.Fatalf("descriptor shape drift: stable=%s legacy=%s", stable.Path(), legacy.Path())
	}
	for index := 0; index < stable.Messages().Len(); index++ {
		current := stable.Messages().Get(index)
		previous := legacy.Messages().ByName(current.Name())
		if previous == nil || current.Fields().Len() != previous.Fields().Len() {
			t.Fatalf("message %s shape drift", current.Name())
		}
		for fieldIndex := 0; fieldIndex < current.Fields().Len(); fieldIndex++ {
			field := current.Fields().Get(fieldIndex)
			old := previous.Fields().ByNumber(field.Number())
			if old == nil || field.Name() != old.Name() || field.Kind() != old.Kind() || field.Cardinality() != old.Cardinality() {
				t.Fatalf("message %s field %d wire drift", current.Name(), field.Number())
			}
		}
	}
	for index := 0; index < stable.Services().Len(); index++ {
		service := stable.Services().Get(index)
		old := legacy.Services().ByName(service.Name())
		if old == nil || service.Methods().Len() != old.Methods().Len() {
			t.Fatalf("service %s drift", service.Name())
		}
		for methodIndex := 0; methodIndex < service.Methods().Len(); methodIndex++ {
			method := service.Methods().Get(methodIndex)
			previous := old.Methods().ByName(method.Name())
			if previous == nil || method.Input().Name() != previous.Input().Name() || method.Output().Name() != previous.Output().Name() {
				t.Fatalf("service %s method %s drift", service.Name(), method.Name())
			}
		}
	}
	for index := 0; index < stable.Enums().Len(); index++ {
		enum := stable.Enums().Get(index)
		old := legacy.Enums().ByName(enum.Name())
		if old == nil || enum.Values().Len() != old.Values().Len() {
			t.Fatalf("enum %s drift", enum.Name())
		}
		for valueIndex := 0; valueIndex < enum.Values().Len(); valueIndex++ {
			value := enum.Values().Get(valueIndex)
			previous := old.Values().ByNumber(value.Number())
			if previous == nil || value.Name() != previous.Name() {
				t.Fatalf("enum %s value %d drift", enum.Name(), value.Number())
			}
		}
	}
}
