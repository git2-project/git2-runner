package source

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	gitv1 "github.com/git2-project/git2-codeplane/gen/go/git/v1"

	"github.com/git2-project/git2-runner/app/internal/ciyaml"
)

// fakeCommits is git-svc's CommitService with one programmable GetBlob.
type fakeCommits struct {
	gitv1.UnimplementedCommitServiceServer

	mu     sync.Mutex
	got    *gitv1.GetBlobRequest
	answer func(ctx context.Context) (*gitv1.GetBlobResponse, error)
}

func (f *fakeCommits) GetBlob(ctx context.Context, in *gitv1.GetBlobRequest) (*gitv1.GetBlobResponse, error) {
	f.mu.Lock()
	f.got = in
	f.mu.Unlock()
	return f.answer(ctx)
}

// dialFake serves f over an in-memory connection and returns a client.
func dialFake(t *testing.T, f *fakeCommits) gitv1.CommitServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	gitv1.RegisterCommitServiceServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return gitv1.NewCommitServiceClient(conn)
}

func TestGitSvcReadCIFile(t *testing.T) {
	const sha = "9fceb02d0ae598e95dc970b74767f19372d61af8"
	file := []byte("build:\n  image: alpine\n  script: [make]\n")
	ok := func(r *gitv1.GetBlobResponse) func(context.Context) (*gitv1.GetBlobResponse, error) {
		return func(context.Context) (*gitv1.GetBlobResponse, error) { return r, nil }
	}
	fail := func(c codes.Code, msg string) func(context.Context) (*gitv1.GetBlobResponse, error) {
		return func(context.Context) (*gitv1.GetBlobResponse, error) { return nil, status.Error(c, msg) }
	}
	regular := gitv1.TreeEntryType_TREE_ENTRY_TYPE_FILE

	for _, tc := range []struct {
		name    string
		answer  func(context.Context) (*gitv1.GetBlobResponse, error)
		want    []byte
		noFile  bool   // ErrNoCIFile
		invalid string // the InvalidFileError reason
	}{
		{name: "file", answer: ok(&gitv1.GetBlobResponse{Type: regular, Size: int64(len(file)), Content: file}), want: file},
		{name: "no file at this commit", answer: fail(codes.NotFound, "path not found"), noFile: true},
		{name: "repository deleted", answer: fail(codes.NotFound, "repository not found"), noFile: true},
		{name: "a directory", answer: fail(codes.InvalidArgument, "path is a directory"), invalid: "it is not a regular file"},
		{name: "symbolic link", answer: ok(&gitv1.GetBlobResponse{Type: gitv1.TreeEntryType_TREE_ENTRY_TYPE_SYMLINK, Content: []byte("other.yml")}), invalid: "it is a symbolic link"},
		{name: "LFS pointer", answer: ok(&gitv1.GetBlobResponse{Type: regular, Lfs: true, Content: []byte("version https://git-lfs.github.com/spec/v1\n")}), invalid: "it is stored in Git LFS"},
		{name: "binary", answer: ok(&gitv1.GetBlobResponse{Type: regular, Binary: true, Size: 100}), invalid: "it is a binary file"},
		{name: "cut at 1 MiB", answer: ok(&gitv1.GetBlobResponse{Type: regular, Truncated: true, Size: 2 << 20, Content: make([]byte, 1<<20)}), invalid: "the file is larger than 256 KiB"},
		{name: "over our limit", answer: ok(&gitv1.GetBlobResponse{Type: regular, Size: ciyaml.MaxFileSize + 1, Content: make([]byte, ciyaml.MaxFileSize+1)}), invalid: "the file is larger than 256 KiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeCommits{answer: tc.answer}
			repo := uuid.New()
			got, err := GitSvc{Client: dialFake(t, f)}.ReadCIFile(context.Background(), repo, sha)

			f.mu.Lock()
			req := f.got
			f.mu.Unlock()
			if req.GetRepositoryId() != repo.String() || req.GetRef() != sha || req.GetPath() != ciyaml.FileName {
				t.Errorf("asked git-svc for %v, want the repository, the commit SHA and %s", req, ciyaml.FileName)
			}
			var invalid *InvalidFileError
			switch {
			case tc.noFile:
				if !errors.Is(err, ErrNoCIFile) {
					t.Errorf("err = %v, want ErrNoCIFile", err)
				}
			case tc.invalid != "":
				if !errors.As(err, &invalid) || invalid.Reason != tc.invalid {
					t.Errorf("err = %v, want InvalidFileError %q", err, tc.invalid)
				}
			default:
				if err != nil || string(got) != string(tc.want) {
					t.Errorf("got %q, %v; want the file", got, err)
				}
			}
		})
	}
}

// TestGitSvcRetryableErrors checks that faults are neither "no CI" nor an
// invalid file, so the caller answers UNAVAILABLE and the push is retried.
func TestGitSvcRetryableErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		answer  func(ctx context.Context) (*gitv1.GetBlobResponse, error)
	}{
		{"git-svc unavailable", 0, func(context.Context) (*gitv1.GetBlobResponse, error) {
			return nil, status.Error(codes.Unavailable, "connection refused")
		}},
		{"internal error", 0, func(context.Context) (*gitv1.GetBlobResponse, error) {
			return nil, status.Error(codes.Internal, "internal")
		}},
		{"no answer in time", 20 * time.Millisecond, func(ctx context.Context) (*gitv1.GetBlobResponse, error) {
			<-ctx.Done()
			return nil, status.FromContextError(ctx.Err()).Err()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := GitSvc{Client: dialFake(t, &fakeCommits{answer: tc.answer}), Timeout: tc.timeout}
			_, err := src.ReadCIFile(context.Background(), uuid.New(), "9fceb02d0ae598e95dc970b74767f19372d61af8")
			var invalid *InvalidFileError
			if err == nil || errors.Is(err, ErrNoCIFile) || errors.As(err, &invalid) {
				t.Errorf("err = %v, want a retryable error", err)
			}
		})
	}
}
