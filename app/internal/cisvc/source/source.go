// Package source reads .git2-ci.yml from a repository at a commit.
//
// The real source is git-svc's gRPC API (assumption A-03): codeplane plans
// a CommitService that returns a file at a commit. Until it exists, ci-svc
// runs with Unavailable, or in development with Dir.
package source

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/git2-project/git2-runner/app/internal/ciyaml"
)

// ErrNoCIFile means the commit has no .git2-ci.yml.
var ErrNoCIFile = errors.New("no " + ciyaml.FileName + " at this commit")

// ErrUnavailable means the file could not be read right now.
var ErrUnavailable = errors.New("reading files from git-svc is not available yet (assumption A-03)")

// Source reads a repository's CI file at a commit.
type Source interface {
	// ReadCIFile returns the content of .git2-ci.yml at commitSHA, or
	// ErrNoCIFile when there is none. Any other error means "retry later".
	ReadCIFile(ctx context.Context, repositoryID uuid.UUID, commitSHA string) ([]byte, error)
}

// Unavailable is the source until git-svc can serve files: every read
// fails with ErrUnavailable.
type Unavailable struct{}

// ReadCIFile implements Source.
func (Unavailable) ReadCIFile(context.Context, uuid.UUID, string) ([]byte, error) {
	return nil, ErrUnavailable
}

// Dir is a development source: it reads <Root>/<repository_id>/.git2-ci.yml
// and ignores the commit. Never use it in production.
type Dir struct {
	Root string
}

// ReadCIFile implements Source.
func (d Dir) ReadCIFile(_ context.Context, repositoryID uuid.UUID, _ string) ([]byte, error) {
	// The ID is a parsed UUID, so it cannot leave Root.
	p := filepath.Join(d.Root, repositoryID.String(), ciyaml.FileName)
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoCIFile
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	return b, nil
}

// Static is a source for tests: one file for every repository and commit.
// A nil Content means "no file".
type Static struct {
	Content []byte
}

// ReadCIFile implements Source.
func (s Static) ReadCIFile(context.Context, uuid.UUID, string) ([]byte, error) {
	if s.Content == nil {
		return nil, ErrNoCIFile
	}
	return s.Content, nil
}
