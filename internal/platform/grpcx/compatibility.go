package grpcx

import (
	"fmt"
	"strings"

	"google.golang.org/grpc"
)

// RegisterLegacyServiceAlias exposes a stable protobuf implementation under
// an N-1 fully-qualified service name. This is safe only while v1 and alpha
// message field numbers remain wire-identical; compatibility tests enforce
// that invariant. The implementation and decoder stay stable-v1 types.
func RegisterLegacyServiceAlias(registrar grpc.ServiceRegistrar, stable grpc.ServiceDesc, implementation any, legacyServiceName string) error {
	if registrar == nil || implementation == nil {
		return fmt.Errorf("gRPC registrar and implementation are required")
	}
	if strings.TrimSpace(stable.ServiceName) == "" || strings.TrimSpace(legacyServiceName) == "" || stable.ServiceName == legacyServiceName {
		return fmt.Errorf("stable and distinct legacy gRPC service names are required")
	}
	stable.ServiceName = legacyServiceName
	registrar.RegisterService(&stable, implementation)
	return nil
}
