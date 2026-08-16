// Package grpcx centralizes the transport options every fenced gRPC boundary
// uses: message size ceilings above the largest payloads (assignments embed
// the full workload spec and receipt lists), keepalive so long-lived worker
// connections detect dead peers promptly instead of hanging on a half-open
// transport, bounded connect attempts so a down endpoint surfaces quickly
// instead of wedging the first call, and OpenTelemetry interceptors so every
// boundary participates in the reference observability stack.
package grpcx

import (
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

// MaxMessageBytes is the ceiling for both directions on every fenced
// boundary. The control-plane assignment embeds the workload spec (up to the
// API's 1 MiB cap) plus goal and receipt lists, so 1 MiB is too tight.
const MaxMessageBytes = 8 << 20

// ServerOptions configures a fenced gRPC server.
func ServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.MaxRecvMsgSize(MaxMessageBytes),
		grpc.MaxSendMsgSize(MaxMessageBytes),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time: 2 * time.Hour, Timeout: 20 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime: 10 * time.Second, PermitWithoutStream: true,
		}),
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
	}
}

// ClientOptions configures a fenced gRPC client: keepalive pings every 30s
// (idle workers have no streams), bounded connect attempts, a receive
// ceiling matching the servers, and OTel span propagation.
func ClientOptions() []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time: 30 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: true,
		}),
		grpc.WithConnectParams(grpc.ConnectParams{MinConnectTimeout: 5 * time.Second}),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(MaxMessageBytes)),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	}
}
