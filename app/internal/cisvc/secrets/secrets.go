// Package secrets stores CI secrets, encrypted with package secretbox, and
// hands the decrypted values to jobs. It knows nothing about gRPC.
//
// Values are write-only: nothing in this package returns a value, except
// ForRepository, which builds a job's environment while the job is leased.
package secrets

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/git2-project/git2-runner/app/internal/cisvc/apierr"
	"github.com/git2-project/git2-runner/app/internal/cisvc/db"
	"github.com/git2-project/git2-runner/app/internal/cisvc/page"
	"github.com/git2-project/git2-runner/app/internal/cisvc/secretbox"
)

// Scope says which jobs get a secret.
type Scope string

// Scopes, as stored in ci_svc.secrets.scope.
const (
	ScopeInstance   Scope = "instance"
	ScopeRepository Scope = "repository"
)

// MaxValueSize is the largest secret value.
const MaxValueSize = 32 << 10

// nameRE is the rule for secret names: environment variable names in
// upper case.
var nameRE = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,99}$`)

// Secret is a secret's metadata, never its value.
type Secret struct {
	Scope     Scope
	ScopeID   uuid.UUID // uuid.Nil for ScopeInstance
	Name      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Variable is a decrypted secret, ready for a job's environment.
type Variable struct {
	Name  string
	Value string
}

// Service stores and reads secrets.
type Service struct {
	pool *pgxpool.Pool
	box  *secretbox.Box
}

// NewService returns a Service that encrypts with box.
func NewService(pool *pgxpool.Pool, box *secretbox.Box) *Service {
	return &Service{pool: pool, box: box}
}

// aad binds a ciphertext to its row.
func aad(scope Scope, scopeID uuid.UUID, name string) []byte {
	return []byte("ci_svc.secrets\x00" + string(scope) + "\x00" + scopeID.String() + "\x00" + name)
}

// CheckKey returns an error if any stored secret was encrypted with
// another key than the configured one, for example after a restore that
// lost GIT2_CI_SECRETS_KEY. ci-svc refuses to start then, instead of
// failing every job that needs a secret.
func (s *Service) CheckKey(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT key_id FROM ci_svc.secrets WHERE key_id <> $1`, s.box.KeyID())
	if err != nil {
		return fmt.Errorf("check secrets key: %w", err)
	}
	others, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("check secrets key: %w", err)
	}
	if len(others) > 0 {
		return fmt.Errorf("GIT2_CI_SECRETS_KEY is not the key the stored CI secrets were encrypted with (configured %s, stored %s): restore the key from the backup",
			s.box.KeyID(), strings.Join(others, ", "))
	}
	return nil
}

// ValidateScope checks a scope and its ID as they arrive from a request.
func ValidateScope(scope Scope, scopeID string) (uuid.UUID, error) {
	switch scope {
	case ScopeInstance:
		if scopeID != "" {
			return uuid.Nil, apierr.InvalidField("scope_id", "must be empty for the instance scope")
		}
		return uuid.Nil, nil
	case ScopeRepository:
		return apierr.ParseID("scope_id", scopeID)
	}
	return uuid.Nil, apierr.InvalidField("scope", "is required")
}

func validateName(name string) error {
	if !nameRE.MatchString(name) {
		return apierr.InvalidField("name", `must be 1 to 100 characters from A-Z, 0-9 and "_", not starting with a digit`)
	}
	if strings.HasPrefix(name, "CI_") || strings.HasPrefix(name, "GIT2_") {
		return apierr.InvalidField("name", `names starting with "CI_" or "GIT2_" are reserved for git2`)
	}
	return nil
}

// Set creates a secret or replaces its value. It reports whether the
// secret was created.
func (s *Service) Set(ctx context.Context, scope Scope, scopeID uuid.UUID, name, value string) (Secret, bool, error) {
	if err := validateName(name); err != nil {
		return Secret{}, false, err
	}
	if value == "" || len(value) > MaxValueSize || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return Secret{}, false, apierr.InvalidField("value", "must be 1 byte to 32 KiB of UTF-8 text without NUL characters")
	}
	ciphertext, nonce := s.box.Seal([]byte(value), aad(scope, scopeID, name))
	sec := Secret{Scope: scope, ScopeID: scopeID, Name: name}
	var created bool
	// xmax is 0 for a row this statement inserted, and set for one it
	// updated.
	err := s.pool.QueryRow(ctx, `
		INSERT INTO ci_svc.secrets (id, scope, scope_id, name, ciphertext, nonce, key_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (scope, scope_id, name) DO UPDATE SET
			ciphertext = EXCLUDED.ciphertext, nonce = EXCLUDED.nonce,
			key_id = EXCLUDED.key_id, updated_at = now()
		RETURNING created_at, updated_at, (xmax = 0)`,
		uuid.New(), string(scope), scopeID, name, ciphertext, nonce, s.box.KeyID(),
	).Scan(&sec.CreatedAt, &sec.UpdatedAt, &created)
	if err != nil {
		return Secret{}, false, fmt.Errorf("set secret: %w", err)
	}
	return sec, created, nil
}

// listCursor is the name after which the next page starts, and the scope
// the token belongs to.
type listCursor struct {
	Scope   Scope     `json:"s"`
	ScopeID uuid.UUID `json:"i"`
	After   string    `json:"a"`
}

// List returns one page of a scope's secrets, sorted by name.
func (s *Service) List(ctx context.Context, scope Scope, scopeID uuid.UUID, pageSize int32, pageToken string) ([]Secret, string, error) {
	size, err := page.Size(pageSize, 50)
	if err != nil {
		return nil, "", err
	}
	var c listCursor
	ok, err := page.Decode(pageToken, &c)
	if err != nil {
		return nil, "", err
	}
	if ok && (c.Scope != scope || c.ScopeID != scopeID) {
		return nil, "", apierr.InvalidField("page_token", "was issued for another scope")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT name, created_at, updated_at FROM ci_svc.secrets
		WHERE scope = $1 AND scope_id = $2 AND name > $3
		ORDER BY name LIMIT $4`, string(scope), scopeID, c.After, size+1)
	if err != nil {
		return nil, "", fmt.Errorf("list secrets: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Secret, error) {
		sec := Secret{Scope: scope, ScopeID: scopeID}
		err := row.Scan(&sec.Name, &sec.CreatedAt, &sec.UpdatedAt)
		return sec, err
	})
	if err != nil {
		return nil, "", fmt.Errorf("list secrets: %w", err)
	}
	next := ""
	if len(list) > size {
		list = list[:size]
		next = page.Encode(listCursor{Scope: scope, ScopeID: scopeID, After: list[size-1].Name})
	}
	return list, next, nil
}

// Delete deletes a secret.
func (s *Service) Delete(ctx context.Context, scope Scope, scopeID uuid.UUID, name string) error {
	if err := validateName(name); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM ci_svc.secrets WHERE scope = $1 AND scope_id = $2 AND name = $3`,
		string(scope), scopeID, name)
	if err != nil {
		return fmt.Errorf("delete secret: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return apierr.New(apierr.NotFound, "secret not found")
	}
	return nil
}

// ForRepository returns the decrypted secrets a job of the repository
// gets: the instance's and the repository's, the repository's winning on
// equal names, sorted by name. q is usually the transaction that leases
// the job.
func (s *Service) ForRepository(ctx context.Context, q db.Querier, repositoryID uuid.UUID) ([]Variable, error) {
	// Ordering by scope puts "instance" before "repository", so the
	// repository's value overwrites the instance's below.
	rows, err := q.Query(ctx, `
		SELECT scope, scope_id, name, ciphertext, nonce, key_id FROM ci_svc.secrets
		WHERE (scope = 'instance' AND scope_id = $1) OR (scope = 'repository' AND scope_id = $2)
		ORDER BY name, scope`, uuid.Nil, repositoryID)
	if err != nil {
		return nil, fmt.Errorf("read secrets: %w", err)
	}
	defer rows.Close()
	var out []Variable
	for rows.Next() {
		var (
			scope, name, keyID string
			scopeID            uuid.UUID
			ciphertext, nonce  []byte
		)
		if err := rows.Scan(&scope, &scopeID, &name, &ciphertext, &nonce, &keyID); err != nil {
			return nil, fmt.Errorf("read secrets: %w", err)
		}
		value, err := s.box.Open(ciphertext, nonce, aad(Scope(scope), scopeID, name), keyID)
		if err != nil {
			return nil, fmt.Errorf("secret %s of scope %s: %w", name, scope, err)
		}
		v := Variable{Name: name, Value: string(value)}
		if n := len(out); n > 0 && out[n-1].Name == name {
			out[n-1] = v
		} else {
			out = append(out, v)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read secrets: %w", err)
	}
	return out, nil
}
