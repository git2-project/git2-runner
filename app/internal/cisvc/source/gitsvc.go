package source

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gitv1 "github.com/git2-project/git2-codeplane/gen/go/git/v1"

	"github.com/git2-project/git2-runner/app/internal/ciyaml"
)

// DefaultGitSvcTimeout bounds one GetBlob call to git-svc.
const DefaultGitSvcTimeout = 10 * time.Second

// InvalidFileError means the commit has a .git2-ci.yml that cannot be a
// pipeline definition: not a regular file, a symbolic link, a Git LFS
// pointer, binary, or too large. The pipeline is created as failed with
// Reason, like a file that does not parse.
type InvalidFileError struct {
	// Reason says what is wrong, for the pipeline's error message.
	Reason string
}

func (e *InvalidFileError) Error() string { return ciyaml.FileName + ": " + e.Reason }

// GitSvc reads .git2-ci.yml from git-svc with CommitService.GetBlob at the
// pushed commit (git2-codeplane v0.2.0, assumption A-03).
type GitSvc struct {
	Client gitv1.CommitServiceClient
	// Timeout bounds one call; 0 means DefaultGitSvcTimeout.
	Timeout time.Duration
}

// ReadCIFile implements Source.
func (g GitSvc) ReadCIFile(ctx context.Context, repositoryID uuid.UUID, commitSHA string) ([]byte, error) {
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = DefaultGitSvcTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resp, err := g.Client.GetBlob(ctx, &gitv1.GetBlobRequest{
		RepositoryId: repositoryID.String(),
		Ref:          commitSHA,
		Path:         ciyaml.FileName,
	})
	switch status.Code(err) {
	case codes.OK:
	case codes.NotFound:
		// The repository, the commit or the file does not exist: either
		// way there is nothing to run.
		return nil, ErrNoCIFile
	case codes.InvalidArgument:
		// The ID and the SHA are valid, so git-svc refused the path: a
		// directory or a submodule called .git2-ci.yml.
		return nil, &InvalidFileError{Reason: "it is not a regular file"}
	default:
		return nil, fmt.Errorf("git-svc GetBlob: %w", err)
	}

	switch {
	case resp.GetType() == gitv1.TreeEntryType_TREE_ENTRY_TYPE_SYMLINK:
		return nil, &InvalidFileError{Reason: "it is a symbolic link"}
	case resp.GetLfs():
		return nil, &InvalidFileError{Reason: "it is stored in Git LFS"}
	case resp.GetBinary():
		return nil, &InvalidFileError{Reason: "it is a binary file"}
	case resp.GetTruncated() || resp.GetSize() > ciyaml.MaxFileSize:
		return nil, &InvalidFileError{Reason: fmt.Sprintf("the file is larger than %d KiB", ciyaml.MaxFileSize>>10)}
	}
	return resp.GetContent(), nil
}
