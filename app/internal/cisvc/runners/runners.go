// Package runners registers runners, authenticates them by token and lets
// admins list, pause and delete them. It knows nothing about gRPC.
//
// Tokens are stored only as SHA-256 hashes (package token). A registration
// token works once and expires; a runner token works until the runner is
// deleted.
package runners

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/git2-project/git2-runner/app/internal/cisvc/apierr"
	"github.com/git2-project/git2-runner/app/internal/cisvc/db"
	"github.com/git2-project/git2-runner/app/internal/cisvc/page"
	"github.com/git2-project/git2-runner/app/internal/cisvc/token"
)

// Limits and defaults.
const (
	DefaultTokenTTL = time.Hour
	MaxTokenTTL     = 7 * 24 * time.Hour
	// OnlineWindow is how recently a runner must have asked for work to
	// count as online.
	OnlineWindow = 90 * time.Second
	// MaxLabels is the most labels a runner may have.
	MaxLabels = 20
	// Executors lists the executors ci-svc accepts.
	executorDocker = "docker"
)

// labelRE is the rule for labels, the same as for job tags in .git2-ci.yml.
var labelRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,63}$`)

// Runner is one registered runner.
type Runner struct {
	ID          uuid.UUID
	Name        string
	Description string
	Labels      []string
	Executor    string
	Version     string
	Platform    string
	Paused      bool
	LastSeenAt  *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Online reports whether the runner asked for work within OnlineWindow.
func (r Runner) Online(now time.Time) bool {
	return r.LastSeenAt != nil && now.Sub(*r.LastSeenAt) < OnlineWindow
}

// RegisterInput is what a runner sends to register.
type RegisterInput struct {
	RegistrationToken string
	Name              string
	Labels            []string
	Executor          string
	Version           string
	Platform          string
}

// Service is the runner registry.
type Service struct {
	pool *pgxpool.Pool
}

// NewService returns a Service on pool.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

const runnerColumns = `id, name, description, labels, executor, version, platform, paused, last_seen_at, created_at, updated_at`

func scanRunner(row pgx.Row) (Runner, error) {
	var r Runner
	err := row.Scan(&r.ID, &r.Name, &r.Description, &r.Labels, &r.Executor, &r.Version, &r.Platform,
		&r.Paused, &r.LastSeenAt, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// CreateRegistrationToken creates a one-time registration token valid for
// ttl (0 means DefaultTokenTTL). The token is returned only here.
func (s *Service) CreateRegistrationToken(ctx context.Context, description string, ttl time.Duration) (string, time.Time, error) {
	if err := checkText("description", description, 200); err != nil {
		return "", time.Time{}, err
	}
	switch {
	case ttl == 0:
		ttl = DefaultTokenTTL
	case ttl < time.Minute || ttl > MaxTokenTTL:
		return "", time.Time{}, apierr.InvalidField("expires_in", "must be between 1 minute and 7 days")
	}
	tok := token.New(token.RegistrationPrefix)
	var expiresAt time.Time
	err := s.pool.QueryRow(ctx, `
		INSERT INTO ci_svc.registration_tokens (id, token_sha256, description, expires_at)
		VALUES ($1, $2, $3, now() + make_interval(secs => $4::float8))
		RETURNING expires_at`,
		uuid.New(), token.Hash(tok), description, ttl.Seconds()).Scan(&expiresAt)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("create registration token: %w", err)
	}
	return tok, expiresAt, nil
}

// errBadRegistrationToken is the one answer for an unknown, used or
// expired registration token, so callers cannot tell them apart.
var errBadRegistrationToken = apierr.New(apierr.Unauthenticated, "the registration token is invalid, used or expired")

// Register uses up a registration token and creates a runner. It returns
// the runner and its token, which is never shown again.
func (s *Service) Register(ctx context.Context, in RegisterInput) (Runner, string, error) {
	labels, err := validateRegister(in)
	if err != nil {
		return Runner{}, "", err
	}
	if !token.HasPrefix(in.RegistrationToken, token.RegistrationPrefix) {
		return Runner{}, "", errBadRegistrationToken
	}

	runnerToken := token.New(token.RunnerPrefix)
	var r Runner
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var tokenID uuid.UUID
		err := tx.QueryRow(ctx, `
			UPDATE ci_svc.registration_tokens SET used_at = now()
			WHERE token_sha256 = $1 AND used_at IS NULL AND expires_at > now()
			RETURNING id`, token.Hash(in.RegistrationToken)).Scan(&tokenID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errBadRegistrationToken
		}
		if err != nil {
			return fmt.Errorf("use registration token: %w", err)
		}
		r, err = scanRunner(tx.QueryRow(ctx, `
			INSERT INTO ci_svc.runners (id, name, labels, executor, version, platform, token_sha256)
			VALUES ($1, $2, $3::text[], $4, $5, $6, $7)
			RETURNING `+runnerColumns,
			uuid.Must(uuid.NewV7()), in.Name, labels, in.Executor, in.Version, in.Platform, token.Hash(runnerToken)))
		if err != nil {
			return fmt.Errorf("insert runner: %w", err)
		}
		_, err = tx.Exec(ctx, `UPDATE ci_svc.registration_tokens SET runner_id = $1 WHERE id = $2`, r.ID, tokenID)
		return err
	})
	if err != nil {
		return Runner{}, "", err
	}
	return r, runnerToken, nil
}

func validateRegister(in RegisterInput) ([]string, error) {
	if in.Name == "" {
		return nil, apierr.InvalidField("name", "is required")
	}
	if err := checkText("name", in.Name, 100); err != nil {
		return nil, err
	}
	if in.Executor != executorDocker {
		return nil, apierr.InvalidField("executor", `must be "docker"`)
	}
	if err := checkText("version", in.Version, 50); err != nil {
		return nil, err
	}
	if err := checkText("platform", in.Platform, 50); err != nil {
		return nil, err
	}
	if len(in.Labels) > MaxLabels {
		return nil, apierr.InvalidField("labels", fmt.Sprintf("at most %d are allowed", MaxLabels))
	}
	labels := make([]string, 0, len(in.Labels))
	for _, l := range in.Labels {
		if !labelRE.MatchString(l) {
			return nil, apierr.InvalidField("labels", `each label is 1 to 63 characters from A-Z, a-z, 0-9, "_", "." and "-"`)
		}
		if !slices.Contains(labels, l) {
			labels = append(labels, l)
		}
	}
	return labels, nil
}

// checkText checks a free-text field: valid UTF-8, no control characters,
// at most max characters.
func checkText(field, s string, maxLen int) error {
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > maxLen || strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return apierr.InvalidField(field, fmt.Sprintf("must be at most %d characters, without control characters", maxLen))
	}
	return nil
}

// errBadRunnerToken is the answer for a missing, unknown or deleted runner
// token. Database faults are never reported this way: a runner that gets
// UNAUTHENTICATED throws its token away.
var errBadRunnerToken = apierr.New(apierr.Unauthenticated, "the runner token is invalid; register the runner again")

// Authenticate returns the runner a token belongs to.
func (s *Service) Authenticate(ctx context.Context, tok string) (Runner, error) {
	if !token.HasPrefix(tok, token.RunnerPrefix) {
		return Runner{}, errBadRunnerToken
	}
	r, err := scanRunner(s.pool.QueryRow(ctx,
		`SELECT `+runnerColumns+` FROM ci_svc.runners WHERE token_sha256 = $1`, token.Hash(tok)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Runner{}, errBadRunnerToken
	}
	if err != nil {
		return Runner{}, fmt.Errorf("authenticate runner: %w", err)
	}
	return r, nil
}

// Touch records that a runner asked for work just now.
func (s *Service) Touch(ctx context.Context, id uuid.UUID) error {
	if _, err := s.pool.Exec(ctx, `UPDATE ci_svc.runners SET last_seen_at = now() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("touch runner: %w", err)
	}
	return nil
}

var errNotFound = apierr.New(apierr.NotFound, "runner not found")

// Get returns a runner by ID.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Runner, error) {
	r, err := scanRunner(s.pool.QueryRow(ctx, `SELECT `+runnerColumns+` FROM ci_svc.runners WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Runner{}, errNotFound
	}
	if err != nil {
		return Runner{}, fmt.Errorf("get runner: %w", err)
	}
	return r, nil
}

// listCursor is the position after the last runner of a page. Runner IDs
// are UUIDv7, so ID order is registration order.
type listCursor struct {
	After uuid.UUID `json:"a"`
}

// List returns one page of runners, oldest first.
func (s *Service) List(ctx context.Context, pageSize int32, pageToken string) ([]Runner, string, error) {
	size, err := page.Size(pageSize, 50)
	if err != nil {
		return nil, "", err
	}
	var c listCursor
	if _, err := page.Decode(pageToken, &c); err != nil {
		return nil, "", err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+runnerColumns+` FROM ci_svc.runners
		WHERE id > $1 ORDER BY id LIMIT $2`, c.After, size+1)
	if err != nil {
		return nil, "", fmt.Errorf("list runners: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Runner, error) { return scanRunner(row) })
	if err != nil {
		return nil, "", fmt.Errorf("list runners: %w", err)
	}
	next := ""
	if len(list) > size {
		list = list[:size]
		next = page.Encode(listCursor{After: list[size-1].ID})
	}
	return list, next, nil
}

// Update changes the fields that are not nil.
func (s *Service) Update(ctx context.Context, id uuid.UUID, paused *bool, description *string) (Runner, error) {
	if description != nil {
		if err := checkText("description", *description, 200); err != nil {
			return Runner{}, err
		}
	}
	r, err := scanRunner(s.pool.QueryRow(ctx, `
		UPDATE ci_svc.runners SET
			paused = COALESCE($2::boolean, paused),
			description = COALESCE($3::text, description),
			updated_at = now()
		WHERE id = $1
		RETURNING `+runnerColumns, id, paused, description))
	if errors.Is(err, pgx.ErrNoRows) {
		return Runner{}, errNotFound
	}
	if err != nil {
		return Runner{}, fmt.Errorf("update runner: %w", err)
	}
	return r, nil
}

// Delete removes a runner. Its token stops working at once; a job it was
// running keeps its runner_id as NULL and fails when its lease runs out.
func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM ci_svc.runners WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete runner: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errNotFound
	}
	return nil
}
