// Package inbox receives the events other services push to ci-svc and
// turns pushes into pipelines (ADR 0010, assumption A-02).
//
// It has two halves:
//
//   - Deliver, behind events.v1.EventSink, only stores an event in
//     ci_svc.inbox and answers. event_id is the table's primary key, so a
//     redelivered event changes nothing. A slow git-svc can never hold up
//     the producer's relay.
//   - A worker (Run) then handles each stored git.v1.RepositoryPushed: for
//     every branch or tag the push created or moved, it starts the
//     pipeline through pipeline.Service.Trigger, with the idempotency key
//     "<event_id>:<ref>". Handling an event twice therefore never runs a
//     pipeline twice.
//
// The worker claims due events with FOR UPDATE SKIP LOCKED and a lease: it
// pushes next_attempt_at forward by Options.Lease in a short transaction,
// then works outside any transaction, because reading .git2-ci.yml from
// git-svc can take seconds. A replica that dies mid-event leaves the lease
// to run out, and another replica takes the event again.
//
// An event whose refs could not all be handled for a reason that may pass
// (git-svc or the database unavailable) is retried with a growing pause
// and never dropped, like the relays do (ADR 0010).
package inbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/encoding/protojson"

	gitv1 "github.com/git2-project/git2-codeplane/gen/go/git/v1"

	"github.com/git2-project/git2-runner/app/internal/cisvc/apierr"
	"github.com/git2-project/git2-runner/app/internal/cisvc/notify"
	"github.com/git2-project/git2-runner/app/internal/cisvc/pipeline"
)

// TypeRepositoryPushed is the only event type ci-svc subscribes to.
const TypeRepositoryPushed = "git.v1.RepositoryPushed"

// maxRefBytes is the longest ref a pipeline can have; longer ones start
// nothing (ADR 0010).
const maxRefBytes = 255

// maxErrorBytes bounds last_error, so one odd error cannot bloat a row.
const maxErrorBytes = 1000

// unmarshal accepts fields this version does not know yet, so git-svc can
// add fields to RepositoryPushed without breaking ci-svc.
var unmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}

// Triggerer starts pipelines. pipeline.Service implements it.
type Triggerer interface {
	Trigger(ctx context.Context, in pipeline.TriggerInput) (pipeline.Pipeline, bool, error)
}

// Options tune the worker. DefaultOptions are the production values.
type Options struct {
	// Poll is how often the worker looks for due events without a signal.
	Poll time.Duration
	// Lease is how long a claimed event is hidden from other replicas.
	Lease time.Duration
	// Batch is how many events one claim takes.
	Batch int
	// MinBackoff and MaxBackoff bound the pause before a failed event is
	// tried again; it doubles with every attempt.
	MinBackoff, MaxBackoff time.Duration
	// KeepProcessed is how long handled events stay in the table. Pipelines
	// keep their idempotency keys forever, so a later redelivery still
	// starts nothing new.
	KeepProcessed time.Duration
}

// DefaultOptions returns the production settings.
func DefaultOptions() Options {
	return Options{
		Poll:          5 * time.Second,
		Lease:         2 * time.Minute,
		Batch:         20,
		MinBackoff:    5 * time.Second,
		MaxBackoff:    5 * time.Minute,
		KeepProcessed: 7 * 24 * time.Hour,
	}
}

// Service stores delivered events and handles them.
type Service struct {
	pool *pgxpool.Pool
	trig Triggerer
	hub  *notify.Hub
	log  *slog.Logger
	opts Options
}

// NewService returns a Service.
func NewService(pool *pgxpool.Pool, trig Triggerer, hub *notify.Hub, log *slog.Logger, opts Options) *Service {
	return &Service{pool: pool, trig: trig, hub: hub, log: log, opts: opts}
}

// Event is one delivered event, as events.v1.DeliverRequest carries it.
type Event struct {
	ID            string
	Type          string
	AggregateType string
	AggregateID   string
	OccurredAt    time.Time
	// Payload is the event message in protobuf JSON.
	Payload string
}

// Deliver stores ev, or does nothing if its ID was stored before. Events
// ci-svc can never handle are apierr.Invalid, which tells the relay not to
// send them again.
func (s *Service) Deliver(ctx context.Context, ev Event) error {
	id, err := apierr.ParseID("event_id", ev.ID)
	if err != nil {
		return err
	}
	if ev.Type != TypeRepositoryPushed {
		return apierr.InvalidField("event_type", "ci-svc handles only "+TypeRepositoryPushed)
	}
	if ev.AggregateType == "" {
		return apierr.InvalidField("aggregate_type", "is required")
	}
	aggregateID, err := apierr.ParseID("aggregate_id", ev.AggregateID)
	if err != nil {
		return err
	}
	if ev.OccurredAt.IsZero() {
		return apierr.InvalidField("occurred_at", "is required")
	}
	var pushed gitv1.RepositoryPushed
	if err := unmarshal.Unmarshal([]byte(ev.Payload), &pushed); err != nil {
		return apierr.InvalidField("payload", "is not a "+TypeRepositoryPushed+" in protobuf JSON")
	}
	if _, err := apierr.ParseID("payload.repository_id", pushed.GetRepositoryId()); err != nil {
		return err
	}

	// The payload is sent as text and cast, so it also works in pgx's exec
	// mode (see package db).
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO ci_svc.inbox (event_id, event_type, aggregate_type, aggregate_id, occurred_at, payload)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb)
		ON CONFLICT (event_id) DO NOTHING`,
		id, ev.Type, ev.AggregateType, aggregateID, ev.OccurredAt, ev.Payload)
	if isDataException(err) {
		// Valid protobuf JSON that jsonb still refuses, such as \u0000 in a
		// string. Storing it will never work.
		return apierr.InvalidField("payload", "cannot be stored as JSON")
	}
	if err != nil {
		return fmt.Errorf("insert inbox event: %w", err)
	}
	if tag.RowsAffected() > 0 {
		s.hub.Publish(ctx, notify.InboxTopic)
	}
	return nil
}

// isDataException reports whether err is a PostgreSQL data exception
// (SQLSTATE class 22), such as malformed JSON.
func isDataException(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "22")
}

// Run handles due events until ctx ends. It wakes on Deliver's signal,
// and polls every Options.Poll for retries and for signals that were lost.
func (s *Service) Run(ctx context.Context) {
	wake, unsubscribe := s.hub.Subscribe(notify.InboxTopic)
	defer unsubscribe()
	t := time.NewTicker(s.opts.Poll)
	defer t.Stop()
	var lastCleanup time.Time
	for {
		for {
			n, err := s.ProcessDue(ctx)
			if err != nil {
				if ctx.Err() == nil {
					s.log.ErrorContext(ctx, "handle inbox events", slog.String("err", err.Error()))
				}
				break
			}
			if n < s.opts.Batch {
				break
			}
		}
		if time.Since(lastCleanup) > time.Hour {
			lastCleanup = time.Now()
			if err := s.Cleanup(ctx); err != nil && ctx.Err() == nil {
				s.log.WarnContext(ctx, "clean up inbox", slog.String("err", err.Error()))
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-t.C:
		}
	}
}

// claimed is an event the worker has leased.
type claimed struct {
	id       uuid.UUID
	typ      string
	payload  string
	attempts int
}

// ProcessDue claims up to Options.Batch due events, handles them and
// returns how many it claimed.
func (s *Service) ProcessDue(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE ci_svc.inbox i
		SET attempts = i.attempts + 1,
		    next_attempt_at = now() + $1::bigint * interval '1 millisecond'
		FROM (
			SELECT event_id FROM ci_svc.inbox
			WHERE processed_at IS NULL AND next_attempt_at <= now()
			ORDER BY next_attempt_at, received_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		) due
		WHERE i.event_id = due.event_id
		RETURNING i.event_id, i.event_type, i.payload::text, i.attempts`,
		s.opts.Lease.Milliseconds(), s.opts.Batch)
	if err != nil {
		return 0, fmt.Errorf("claim inbox events: %w", err)
	}
	events, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (claimed, error) {
		var c claimed
		err := r.Scan(&c.id, &c.typ, &c.payload, &c.attempts)
		return c, err
	})
	if err != nil {
		return 0, fmt.Errorf("claim inbox events: %w", err)
	}
	for _, ev := range events {
		if err := s.handle(ctx, ev); err != nil {
			return len(events), err
		}
	}
	return len(events), nil
}

// handle starts the pipelines of one claimed event and records the
// outcome. Its error is about recording only; an event that could not be
// handled is scheduled again, which is not an error of ProcessDue.
func (s *Service) handle(ctx context.Context, ev claimed) error {
	log := s.log.With(slog.String("event_id", ev.id.String()))
	retryErr := s.triggerAll(ctx, log, ev)
	if ctx.Err() != nil {
		// Shutting down: give the event back at once instead of waiting for
		// the lease, and do not count this attempt.
		release, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_, err := s.pool.Exec(release, `
			UPDATE ci_svc.inbox SET next_attempt_at = now(), attempts = attempts - 1
			WHERE event_id = $1 AND processed_at IS NULL`, ev.id)
		return err
	}
	if retryErr == nil {
		_, err := s.pool.Exec(ctx, `
			UPDATE ci_svc.inbox SET processed_at = now(), last_error = '' WHERE event_id = $1`, ev.id)
		if err != nil {
			return fmt.Errorf("mark inbox event processed: %w", err)
		}
		return nil
	}

	pause := s.backoff(ev.attempts)
	msg := retryErr.Error()
	if len(msg) > maxErrorBytes {
		msg = msg[:maxErrorBytes]
	}
	log.WarnContext(ctx, "push not handled yet; will retry",
		slog.Int("attempts", ev.attempts), slog.String("retry_in", pause.String()), slog.String("err", msg))
	_, err := s.pool.Exec(ctx, `
		UPDATE ci_svc.inbox
		SET last_error = $2, next_attempt_at = now() + $3::bigint * interval '1 millisecond'
		WHERE event_id = $1`, ev.id, msg, pause.Milliseconds())
	if err != nil {
		return fmt.Errorf("schedule inbox retry: %w", err)
	}
	return nil
}

// triggerAll runs the trigger for every ref of the event that can start a
// pipeline. It returns an error only if the event must be tried again:
// outcomes that can never change (no .git2-ci.yml, an unusable ref) are
// logged and count as handled.
func (s *Service) triggerAll(ctx context.Context, log *slog.Logger, ev claimed) error {
	if ev.typ != TypeRepositoryPushed {
		// Deliver refuses other types, so only a row written some other way
		// gets here. Retrying cannot help.
		log.ErrorContext(ctx, "inbox event of a type ci-svc does not handle", slog.String("event_type", ev.typ))
		return nil
	}
	var pushed gitv1.RepositoryPushed
	if err := unmarshal.Unmarshal([]byte(ev.payload), &pushed); err != nil {
		log.ErrorContext(ctx, "inbox event with an unreadable payload", slog.String("err", err.Error()))
		return nil
	}
	pusher := pushed.GetPusherId()
	if _, err := uuid.Parse(pusher); err != nil || len(pusher) != 36 {
		pusher = "" // the pipeline is still started, without a user
	}

	var retry []string
	for _, u := range pushed.GetRefUpdates() {
		if !startsPipeline(u) {
			continue
		}
		p, created, err := s.trig.Trigger(ctx, pipeline.TriggerInput{
			RepositoryID:   pushed.GetRepositoryId(),
			Ref:            u.GetRef(),
			CommitSHA:      u.GetNewSha(),
			Trigger:        pipeline.TriggerPush,
			TriggeredByID:  pusher,
			IdempotencyKey: ev.id.String() + ":" + u.GetRef(),
		})
		refLog := log.With(slog.String("ref", u.GetRef()), slog.String("commit_sha", u.GetNewSha()))
		switch kind := apierr.KindOf(err); {
		case err == nil:
			if created {
				refLog.InfoContext(ctx, "push started a pipeline", slog.String("pipeline_id", p.ID.String()))
			}
		case kind == apierr.NotFound:
			// No .git2-ci.yml at this commit: no CI, which is normal.
		case kind == apierr.Invalid || kind == apierr.AlreadyExists:
			refLog.WarnContext(ctx, "push cannot start a pipeline", slog.String("err", err.Error()))
		case ctx.Err() != nil:
			return ctx.Err()
		default:
			retry = append(retry, u.GetRef()+": "+err.Error())
		}
	}
	if len(retry) > 0 {
		return errors.New(strings.Join(retry, "; "))
	}
	return nil
}

// startsPipeline reports whether a ref update can start a pipeline: a
// branch or tag that was created or moved, with a name a pipeline can
// carry. Deleted refs, refs/notes/* and other namespaces start nothing.
func startsPipeline(u *gitv1.RefUpdate) bool {
	ref := u.GetRef()
	if !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/") {
		return false
	}
	if len(ref) > maxRefBytes {
		return false
	}
	return strings.Trim(u.GetNewSha(), "0") != ""
}

// backoff is the pause before attempt n+1: MinBackoff doubled for every
// attempt after the first, at most MaxBackoff.
func (s *Service) backoff(attempts int) time.Duration {
	d := s.opts.MinBackoff
	for i := 1; i < attempts && d < s.opts.MaxBackoff; i++ {
		d *= 2
	}
	return min(d, s.opts.MaxBackoff)
}

// Cleanup deletes events handled more than Options.KeepProcessed ago.
func (s *Service) Cleanup(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM ci_svc.inbox
		WHERE processed_at < now() - $1::bigint * interval '1 millisecond'`,
		s.opts.KeepProcessed.Milliseconds())
	if err != nil {
		return fmt.Errorf("delete old inbox events: %w", err)
	}
	return nil
}
