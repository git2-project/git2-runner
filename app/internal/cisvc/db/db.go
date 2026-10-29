// Package db connects ci-svc to PostgreSQL.
//
// ci-svc owns the PostgreSQL schema ci_svc and nothing outside it. Every
// query names its tables with the schema (ci_svc.jobs) instead of relying
// on search_path.
//
// ci-svc never changes the schema itself. Migrations live in the git2
// repo; this repo keeps a copy in migrations/ for its tests.
//
// The pool settings and the transaction-pooler handling are the same as in
// git2's internal/database and git2-codeplane's db package, so every git2
// service behaves alike.
package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Schema is the PostgreSQL schema owned by ci-svc.
const Schema = "ci_svc"

// requiredTables are the tables ci-svc reads and writes.
var requiredTables = []string{
	Schema + ".runners", Schema + ".registration_tokens", Schema + ".pipelines",
	Schema + ".jobs", Schema + ".job_log_chunks", Schema + ".secrets", Schema + ".outbox",
}

// pingTimeout bounds the first connection attempt at startup.
const pingTimeout = 5 * time.Second

// errBadDSN is returned for a malformed DSN. pgx's own parse error quotes
// the DSN, password included on a best-effort basis, so it is not passed
// on: the message ends up in logs.
var errBadDSN = errors.New("parse POSTGRES_DSN: not a valid PostgreSQL connection string")

// Querier is what pgxpool.Pool and pgx.Tx have in common.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Open creates a connection pool of at most maxConns connections and checks
// that the database is reachable.
func Open(ctx context.Context, dsn string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errBadDSN
	}
	cfg.MaxConns = maxConns
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 30 * time.Minute
	cfg.HealthCheckPeriod = time.Minute

	// A transaction-pooling PgBouncer (Supabase's port 6543) hands each
	// transaction to any server connection, so server-side prepared
	// statements would collide on their names.
	if isTransactionPooler(dsn) {
		cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
		cfg.ConnConfig.StatementCacheCapacity = 0
		cfg.ConnConfig.DescriptionCacheCapacity = 0
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return pool, nil
}

// isTransactionPooler reports whether the DSN targets Supabase's
// transaction pooler (port 6543) or any host that advertises pgbouncer. It
// is the same rule as in git2 and git2-codeplane.
func isTransactionPooler(dsn string) bool {
	lower := strings.ToLower(dsn)
	return strings.Contains(dsn, ":6543/") ||
		strings.Contains(lower, "pgbouncer") ||
		strings.Contains(lower, "pooler.supabase")
}

// CheckSchema returns an error if a table ci-svc needs is missing, which
// means the git2 repo's migrations have not been applied to this database.
func CheckSchema(ctx context.Context, pool *pgxpool.Pool) error {
	var missing []string
	for _, table := range requiredTables {
		var found *string // to_regclass returns NULL for a missing table
		if err := pool.QueryRow(ctx, "SELECT to_regclass($1::text)::text", table).Scan(&found); err != nil {
			return fmt.Errorf("check table %s: %w", table, err)
		}
		if found == nil {
			missing = append(missing, table)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("database schema is not ready, missing %s: apply the git2 repo's migrations (migrations/*_ci_*.sql)",
			strings.Join(missing, ", "))
	}
	return nil
}

// txAttempts is how often InTx runs a transaction that PostgreSQL aborted
// because of a deadlock or a serialization failure.
const txAttempts = 3

// InTx runs fn in a transaction and commits it. When PostgreSQL aborts the
// transaction because of a deadlock or a serialization failure, fn runs
// again from the start, so fn must not have effects outside tx.
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	var err error
	for range txAttempts {
		err = pgx.BeginFunc(ctx, pool, fn)
		if !isRetryable(err) {
			return err
		}
	}
	return err
}

// isRetryable reports whether err is a deadlock (40P01) or a serialization
// failure (40001).
func isRetryable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40P01" || pgErr.Code == "40001")
}

// uniqueViolation is PostgreSQL's SQLSTATE for a unique constraint violation.
const uniqueViolation = "23505"

// IsUniqueViolation reports whether err is a violation of the unique index
// or constraint named constraint.
func IsUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == constraint
}
