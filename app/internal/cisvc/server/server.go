// Package server wires ci-svc's gRPC services, interceptors and the
// mapping from domain errors to gRPC status codes.
//
// ci-svc serves two ports (plan §3.1):
//   - the internal port, with PipelineService, RunnerAdminService and
//     SecretService, reachable only inside the instance network;
//   - the runner port, with RunnerService only, reachable from build hosts.
//     Every call except RegisterRunner needs a runner token.
//
// Both ports also serve the standard gRPC health service.
package server

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	civ1 "github.com/git2-project/git2-runner/app/gen/go/ci/v1"
	"github.com/git2-project/git2-runner/app/internal/cisvc/pipeline"
	"github.com/git2-project/git2-runner/app/internal/cisvc/ratelimit"
	"github.com/git2-project/git2-runner/app/internal/cisvc/runners"
	"github.com/git2-project/git2-runner/app/internal/cisvc/secrets"
)

// Deps are the domain services behind the gRPC API.
type Deps struct {
	Pipelines *pipeline.Service
	Runners   *runners.Service
	Secrets   *secrets.Service
	// RegisterLimit limits RegisterRunner attempts per client address. May
	// be nil.
	RegisterLimit *ratelimit.Limiter
	Log           *slog.Logger
}

// Server is one gRPC server with its health service.
type Server struct {
	grpc     *grpc.Server
	health   *health.Server
	services []string
}

// NewInternal builds the internal server. reflection registers the gRPC
// reflection service, for grpcurl; dev only.
func NewInternal(d Deps, reflect bool) *Server {
	gs := grpc.NewServer(
		grpc.ChainUnaryInterceptor(logUnary(d.Log), recoverUnary(d.Log)),
		grpc.ChainStreamInterceptor(logStream(d.Log), recoverStream(d.Log)),
	)
	civ1.RegisterPipelineServiceServer(gs, &pipelineServer{d: d})
	civ1.RegisterRunnerAdminServiceServer(gs, &adminServer{d: d})
	civ1.RegisterSecretServiceServer(gs, &secretServer{d: d})
	if reflect {
		reflection.Register(gs)
	}
	return newServer(gs,
		civ1.PipelineService_ServiceDesc.ServiceName,
		civ1.RunnerAdminService_ServiceDesc.ServiceName,
		civ1.SecretService_ServiceDesc.ServiceName)
}

// NewRunner builds the runner server. With tlsConfig nil it serves
// plaintext, for development or behind a proxy that terminates TLS.
func NewRunner(d Deps, tlsConfig *tls.Config) *Server {
	opts := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(logUnary(d.Log), recoverUnary(d.Log), authRunner(d.Runners, d.Log)),
		grpc.ChainStreamInterceptor(logStream(d.Log), recoverStream(d.Log)),
	}
	if tlsConfig != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(tlsConfig)))
	}
	gs := grpc.NewServer(opts...)
	civ1.RegisterRunnerServiceServer(gs, &runnerServer{d: d})
	return newServer(gs, civ1.RunnerService_ServiceDesc.ServiceName)
}

func newServer(gs *grpc.Server, services ...string) *Server {
	hs := health.NewServer()
	services = append([]string{""}, services...)
	for _, name := range services {
		hs.SetServingStatus(name, healthpb.HealthCheckResponse_NOT_SERVING)
	}
	healthpb.RegisterHealthServer(gs, hs)
	return &Server{grpc: gs, health: hs, services: services}
}

// Serve marks the server healthy and serves on lis until Shutdown.
func (s *Server) Serve(lis net.Listener) error {
	for _, name := range s.services {
		s.health.SetServingStatus(name, healthpb.HealthCheckResponse_SERVING)
	}
	return s.grpc.Serve(lis)
}

// Shutdown marks the server unhealthy, then waits for in-flight RPCs to
// finish. If ctx ends first, the remaining RPCs are canceled. Long polls
// and log streams end at once: their contexts are canceled by Stop.
func (s *Server) Shutdown(ctx context.Context) {
	s.health.Shutdown()
	done := make(chan struct{})
	go func() {
		s.grpc.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		s.grpc.Stop()
		<-done
	}
}
