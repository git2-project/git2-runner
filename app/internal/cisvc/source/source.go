// Package source reads .git2-ci.yml from a repository at a commit.
//
// In production the source is GitSvc, which asks git-svc's
// CommitService.GetBlob (assumption A-03). Without a git-svc address ci-svc
// runs with Unavailable, and in development it can use Dir.
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

// ErrUnavailable means ci-svc has no way to read files: no git-svc address
// is configured.
var ErrUnavailable = errors.New("no git-svc address is configured")

// File is a .git2-ci.yml read at a commit.
type File struct {
	Content []byte
	// CommitSHA is the commit the file was read from. It differs from the
	// requested SHA when that names an annotated tag object, which git-svc
	// peels to its commit. Empty when the source cannot tell.
	CommitSHA string
}

// Source reads a repository's CI file at a commit.
type Source interface {
	// ReadCIFile returns .git2-ci.yml at commitSHA. It returns ErrNoCIFile
	// when there is none, and an *InvalidFileError when the file cannot be
	// a pipeline definition. Any other error means "retry later".
	ReadCIFile(ctx context.Context, repositoryID uuid.UUID, commitSHA string) (File, error)
}

// Unavailable is the source when no git-svc address is configured: every
// read fails with ErrUnavailable.
type Unavailable struct{}

// ReadCIFile implements Source.
func (Unavailable) ReadCIFile(context.Context, uuid.UUID, string) (File, error) {
	return File{}, ErrUnavailable
}

// Dir is a development source: it reads <Root>/<repository_id>/.git2-ci.yml
// and ignores the commit. Never use it in production.
type Dir struct {
	Root string
}

// ReadCIFile implements Source.
func (d Dir) ReadCIFile(_ context.Context, repositoryID uuid.UUID, commitSHA string) (File, error) {
	// The ID is a parsed UUID, so it cannot leave Root.
	p := filepath.Join(d.Root, repositoryID.String(), ciyaml.FileName)
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return File{}, ErrNoCIFile
	}
	if err != nil {
		return File{}, fmt.Errorf("read %s: %w", p, err)
	}
	return File{Content: b, CommitSHA: commitSHA}, nil
}

// Static is a source for tests: one file for every repository and commit.
// A nil Content means "no file".
type Static struct {
	Content []byte
}

// ReadCIFile implements Source.
func (s Static) ReadCIFile(_ context.Context, _ uuid.UUID, commitSHA string) (File, error) {
	if s.Content == nil {
		return File{}, ErrNoCIFile
	}
	return File{Content: s.Content, CommitSHA: commitSHA}, nil
}
