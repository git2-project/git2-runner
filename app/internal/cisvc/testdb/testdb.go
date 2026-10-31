// Package testdb gives Postgres integration tests their database: the
// throwaway one named by TEST_POSTGRES_DSN, with the ci_svc schema created
// from this repo's copy of the migrations.
//
// Only *_integration_test.go files import it. TEST_POSTGRES_DSN must never
// point at the shared dev database: the tests write and delete rows.
//
// It is the same helper as git2-codeplane's testdb (codeplane ADR 0011).
package testdb

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/git2-project/git2-runner/app/internal/cisvc/db"
)

// EnvDSN names the variable that points at the throwaway test database.
const EnvDSN = "TEST_POSTGRES_DSN"

// maxConns leaves room for tests that run many requests at once.
const maxConns = 20

// Mode is how pgx sends queries to PostgreSQL.
type Mode string

const (
	// Direct is pgx's default: cached server-side prepared statements, as on
	// a direct connection.
	Direct Mode = "direct"
	// Pooler is exec mode, without prepared statements, which db.Open
	// selects behind a transaction pooler such as Supabase's port 6543.
	Pooler Mode = "pooler"
)

// Modes lists every Mode, for tests that run once per mode.
var Modes = []Mode{Direct, Pooler}

// Open returns a pool on the test database in the given mode, with the
// schema in place. It skips the test when TEST_POSTGRES_DSN is not set, and
// closes the pool when the test ends.
func Open(t testing.TB, mode Mode) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv(EnvDSN)
	if dsn == "" {
		t.Skip(EnvDSN + " is not set; the devcontainer sets it")
	}
	if mode == Pooler {
		dsn = withParam(dsn, "default_query_exec_mode", "exec")
	}

	ctx := t.Context()
	pool, err := db.Open(ctx, dsn, maxConns)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	if err := db.CheckSchema(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

// migrationLock is an arbitrary advisory lock key. go test runs packages in
// parallel, and two concurrent CREATE TABLE IF NOT EXISTS can still fail on
// PostgreSQL's catalog, so Migrate takes this lock first.
const migrationLock = 0x63695f737663 // "ci_svc"

// Migrate applies every file in the repo's migrations/ directory in name
// order, each in its own transaction, as git2's runner does. The files are
// idempotent, so applying them again changes nothing.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	dir, err := migrationsDir()
	if err != nil {
		return err
	}
	fsys := os.DirFS(dir)
	files, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	if len(files) == 0 {
		return fmt.Errorf("no migrations in %s", dir)
	}
	slices.Sort(files)

	for _, name := range files {
		sql, err := fs.ReadFile(fsys, name)
		if err != nil {
			return fmt.Errorf("read migration: %w", err)
		}
		err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			// A transaction-scoped lock, so it also works through a pooler.
			if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(migrationLock)); err != nil {
				return fmt.Errorf("lock: %w", err)
			}
			// Without arguments pgx uses the simple protocol, which accepts
			// several statements in one string.
			_, err := tx.Exec(ctx, string(sql))
			return err
		})
		if err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
	}
	return nil
}

// migrationsDir finds migrations/ at the repo root: the parent of the
// directory that holds go.mod (app/).
func migrationsDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("find migrations: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(filepath.Dir(dir), "migrations"), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("find migrations: no go.mod above the working directory")
		}
		dir = parent
	}
}

// withParam adds a connection parameter to a DSN in URL or key=value form.
func withParam(dsn, key, value string) string {
	if !strings.Contains(dsn, "://") {
		return dsn + " " + key + "=" + value
	}
	if strings.Contains(dsn, "?") {
		return dsn + "&" + key + "=" + value
	}
	return dsn + "?" + key + "=" + value
}
