package server

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	civ1 "github.com/git2-project/git2-runner/app/gen/go/ci/v1"
	"github.com/git2-project/git2-runner/app/internal/cisvc/apierr"
	"github.com/git2-project/git2-runner/app/internal/cisvc/runners"
)

// errInternal is the only message clients see for unexpected failures.
var errInternal = status.Error(codes.Internal, "internal error")

// toStatus turns a domain error into a gRPC status error. An *apierr.Error
// carries a client-safe message; any other error can contain SQL or paths,
// so it is logged here and the client sees a generic INTERNAL error.
func toStatus(ctx context.Context, log *slog.Logger, err error) error {
	var e *apierr.Error
	switch {
	case errors.As(err, &e):
		return status.Error(codeOf(e.Kind), e.Message)
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "deadline exceeded")
	}
	method, _ := grpc.Method(ctx)
	log.ErrorContext(ctx, "request failed", slog.String("method", method), slog.String("err", err.Error()))
	return errInternal
}

func codeOf(k apierr.Kind) codes.Code {
	switch k {
	case apierr.Invalid:
		return codes.InvalidArgument
	case apierr.NotFound:
		return codes.NotFound
	case apierr.AlreadyExists:
		return codes.AlreadyExists
	case apierr.FailedPrecondition:
		return codes.FailedPrecondition
	case apierr.Unauthenticated:
		return codes.Unauthenticated
	case apierr.ResourceExhausted:
		return codes.ResourceExhausted
	case apierr.Unavailable:
		return codes.Unavailable
	}
	return codes.Internal
}

// quietMethods are called every few seconds by every runner or probe.
// They are logged at debug level unless they fail on our side.
var quietMethods = map[string]bool{
	civ1.RunnerService_RequestJob_FullMethodName: true,
	civ1.RunnerService_UpdateJob_FullMethodName:  true,
	civ1.RunnerService_AppendLog_FullMethodName:  true,
}

func logRPC(ctx context.Context, log *slog.Logger, method string, start time.Time, err error) {
	code := status.Code(err)
	level := slog.LevelInfo
	switch {
	case isServerFault(code):
		level = slog.LevelError
	case quietMethods[method] || strings.HasPrefix(method, "/grpc.health."):
		level = slog.LevelDebug
	}
	attrs := []slog.Attr{
		slog.String("method", method),
		slog.String("code", code.String()),
		slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
	}
	if err != nil {
		attrs = append(attrs, slog.String("err", err.Error()))
	}
	log.LogAttrs(ctx, level, "rpc", attrs...)
}

// logUnary logs one line per RPC.
func logUnary(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		logRPC(ctx, log, info.FullMethod, start, err)
		return resp, err
	}
}

// logStream logs one line per streaming RPC, when it ends.
func logStream(log *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		start := time.Now()
		err := handler(srv, ss)
		logRPC(ss.Context(), log, info.FullMethod, start, err)
		return err
	}
}

// recoverUnary turns a panicking handler into an INTERNAL error, so one bad
// request cannot crash the process.
func recoverUnary(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if p := recover(); p != nil {
				logPanic(ctx, log, info.FullMethod, p)
				err = errInternal
			}
		}()
		return handler(ctx, req)
	}
}

// recoverStream is recoverUnary for streaming RPCs.
func recoverStream(log *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			if p := recover(); p != nil {
				logPanic(ss.Context(), log, info.FullMethod, p)
				err = errInternal
			}
		}()
		return handler(srv, ss)
	}
}

func logPanic(ctx context.Context, log *slog.Logger, method string, p any) {
	log.ErrorContext(ctx, "panic in gRPC handler",
		slog.String("method", method),
		slog.Any("panic", p),
		slog.String("stack", string(debug.Stack())),
	)
}

// isServerFault reports whether code means ci-svc failed, as opposed to
// the caller sending a bad or impossible request.
func isServerFault(code codes.Code) bool {
	switch code {
	case codes.Internal, codes.Unknown, codes.DataLoss, codes.Unavailable, codes.Unimplemented:
		return true
	}
	return false
}

// runnerKey is the context key of the authenticated runner.
type runnerKey struct{}

// authRunner authenticates every RunnerService call except RegisterRunner
// by the bearer token in the "authorization" metadata, and puts the
// runner into the context. A database fault is INTERNAL, never
// UNAUTHENTICATED: a runner throws its token away on UNAUTHENTICATED.
func authRunner(rs *runners.Service, log *slog.Logger) grpc.UnaryServerInterceptor {
	prefix := "/" + civ1.RunnerService_ServiceDesc.ServiceName + "/"
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !strings.HasPrefix(info.FullMethod, prefix) || info.FullMethod == civ1.RunnerService_RegisterRunner_FullMethodName {
			return handler(ctx, req)
		}
		r, err := rs.Authenticate(ctx, bearerToken(ctx))
		if err != nil {
			return nil, toStatus(ctx, log, err)
		}
		return handler(context.WithValue(ctx, runnerKey{}, r), req)
	}
}

// bearerToken returns the token of an "authorization: Bearer <token>"
// header, or "".
func bearerToken(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	for _, v := range md.Get("authorization") {
		if tok, ok := strings.CutPrefix(v, "Bearer "); ok {
			return strings.TrimSpace(tok)
		}
	}
	return ""
}

// runnerFrom returns the runner authRunner put into ctx.
func runnerFrom(ctx context.Context) runners.Runner {
	r, _ := ctx.Value(runnerKey{}).(runners.Runner)
	return r
}
