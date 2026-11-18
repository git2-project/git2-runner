package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventsv1 "github.com/git2-project/git2-codeplane/gen/go/events/v1"
	gitv1 "github.com/git2-project/git2-codeplane/gen/go/git/v1"

	civ1 "github.com/git2-project/git2-runner/app/gen/go/ci/v1"
	"github.com/git2-project/git2-runner/app/internal/cisvc/inbox"
)

// testInboxOptions make a failed event wait long enough that the test
// decides when it is due again.
var testInboxOptions = inbox.Options{
	Poll:          50 * time.Millisecond,
	Lease:         time.Minute,
	Batch:         20,
	MinBackoff:    time.Minute,
	MaxBackoff:    time.Hour,
	KeepProcessed: time.Hour,
}

const zeroSHA = "0000000000000000000000000000000000000000"

// pushEvent is a delivered git.v1.RepositoryPushed, as codeplane's relay
// sends it.
func pushEvent(t *testing.T, repo uuid.UUID, pusher string, updates ...*gitv1.RefUpdate) *eventsv1.DeliverRequest {
	t.Helper()
	payload, err := protojson.Marshal(&gitv1.RepositoryPushed{
		RepositoryId: repo.String(), PusherId: pusher, OccurredAt: timestamppb.Now(), RefUpdates: updates,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &eventsv1.DeliverRequest{
		EventId: uuid.NewString(), EventType: inbox.TypeRepositoryPushed, AggregateType: "repository",
		AggregateId: repo.String(), OccurredAt: timestamppb.Now(), Payload: string(payload),
	}
}

func (e *env) deliver(t *testing.T, req *eventsv1.DeliverRequest) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), `DELETE FROM ci_svc.inbox WHERE event_id = $1`, req.GetEventId())
	})
	if _, err := e.events.Deliver(t.Context(), req); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
}

// inboxRow is what the test reads back about one event.
type inboxRow struct {
	processed bool
	attempts  int
	lastError string
	dueIn     time.Duration
}

func (e *env) inboxRow(t *testing.T, eventID string) (inboxRow, bool) {
	t.Helper()
	var r inboxRow
	var dueInMS float64
	err := e.pool.QueryRow(t.Context(), `
		SELECT processed_at IS NOT NULL, attempts, last_error,
		       EXTRACT(EPOCH FROM next_attempt_at - now()) * 1000
		FROM ci_svc.inbox WHERE event_id = $1`, eventID).Scan(&r.processed, &r.attempts, &r.lastError, &dueInMS)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return r, false
		}
		t.Fatal(err)
	}
	r.dueIn = time.Duration(dueInMS) * time.Millisecond
	return r, true
}

// pushPipelines returns "ref key triggered_by" for every pipeline of repo.
func (e *env) pushPipelines(t *testing.T, repo uuid.UUID) []string {
	t.Helper()
	rows, err := e.pool.Query(t.Context(), `
		SELECT ref || ' ' || idempotency_key || ' ' || trigger || ' ' || COALESCE(triggered_by::text, '-')
		FROM ci_svc.pipelines WHERE repository_id = $1 ORDER BY ref`, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func (e *env) processDue(t *testing.T) {
	t.Helper()
	if _, err := e.inbox.ProcessDue(t.Context()); err != nil {
		t.Fatalf("ProcessDue: %v", err)
	}
}

func TestEventSinkPushStartsPipelines(t *testing.T) {
	forEachMode(t, func(t *testing.T, e *env) {
		ctx := t.Context()
		repo := e.newRepo(t, threeStages)
		pusher := uuid.NewString()
		mainSHA, tagSHA := sha(), sha()
		longRef := "refs/heads/" + strings.Repeat("x", 250)
		ev := pushEvent(t, repo, pusher,
			&gitv1.RefUpdate{Ref: "refs/heads/main", OldSha: zeroSHA, NewSha: mainSHA},
			&gitv1.RefUpdate{Ref: "refs/tags/v1.0.0", OldSha: zeroSHA, NewSha: tagSHA},
			&gitv1.RefUpdate{Ref: "refs/heads/old", OldSha: sha(), NewSha: zeroSHA}, // deleted
			&gitv1.RefUpdate{Ref: "refs/notes/commits", OldSha: zeroSHA, NewSha: sha()},
			&gitv1.RefUpdate{Ref: longRef, OldSha: zeroSHA, NewSha: sha()},
		)

		// Deliver only stores; a repeat changes nothing.
		e.deliver(t, ev)
		e.deliver(t, ev)
		if row, ok := e.inboxRow(t, ev.GetEventId()); !ok || row.processed || row.attempts != 0 {
			t.Fatalf("after Deliver: %+v, found %v", row, ok)
		}
		if got := e.pushPipelines(t, repo); len(got) != 0 {
			t.Fatalf("Deliver started pipelines itself: %v", got)
		}

		// The worker starts one pipeline per created or moved branch or tag.
		e.processDue(t)
		want := []string{
			"refs/heads/main " + ev.GetEventId() + ":refs/heads/main push " + pusher,
			"refs/tags/v1.0.0 " + ev.GetEventId() + ":refs/tags/v1.0.0 push " + pusher,
		}
		if got := e.pushPipelines(t, repo); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("pipelines:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		if row, _ := e.inboxRow(t, ev.GetEventId()); !row.processed || row.attempts != 1 || row.lastError != "" {
			t.Fatalf("after the worker: %+v", row)
		}
		resp, err := e.ps.ListPipelines(ctx, &civ1.ListPipelinesRequest{RepositoryId: repo.String()})
		if err != nil || len(resp.GetPipelines()) != 2 {
			t.Fatalf("ListPipelines: %v, %d pipelines", err, len(resp.GetPipelines()))
		}
		for _, p := range resp.GetPipelines() {
			if p.GetCommitSha() != mainSHA && p.GetCommitSha() != tagSHA {
				t.Errorf("pipeline %s has commit %s", p.GetRef(), p.GetCommitSha())
			}
		}

		// A redelivery after the event was handled starts nothing either,
		// even once the handled event has been cleaned up.
		e.deliver(t, ev)
		e.processDue(t)
		if _, err := e.pool.Exec(ctx, `UPDATE ci_svc.inbox SET processed_at = now() - interval '2 hours' WHERE event_id = $1`, ev.GetEventId()); err != nil {
			t.Fatal(err)
		}
		if err := e.inbox.Cleanup(ctx); err != nil {
			t.Fatal(err)
		}
		if _, ok := e.inboxRow(t, ev.GetEventId()); ok {
			t.Fatal("Cleanup kept an event handled two hours ago")
		}
		e.deliver(t, ev)
		e.processDue(t)
		if got := e.pushPipelines(t, repo); len(got) != 2 {
			t.Fatalf("a redelivery started more pipelines: %v", got)
		}
	})
}

func TestEventSinkRetriesAndSkips(t *testing.T) {
	forEachMode(t, func(t *testing.T, e *env) {
		ctx := t.Context()
		main := func() *gitv1.RefUpdate {
			return &gitv1.RefUpdate{Ref: "refs/heads/main", OldSha: sha(), NewSha: sha()}
		}

		// git-svc down: the event stays, with the error and a later due time.
		down := e.newRepo(t, threeStages)
		e.src.setErr(down, errors.New("git-svc unavailable"))
		ev := pushEvent(t, down, "", main())
		e.deliver(t, ev)
		e.processDue(t)
		row, _ := e.inboxRow(t, ev.GetEventId())
		if row.processed || row.attempts != 1 || !strings.Contains(row.lastError, "refs/heads/main") ||
			row.dueIn < 50*time.Second || row.dueIn > 70*time.Second {
			t.Fatalf("after a failure: %+v", row)
		}
		if got := e.pushPipelines(t, down); len(got) != 0 {
			t.Fatalf("pipelines while git-svc is down: %v", got)
		}
		// Not due yet: the worker leaves it alone.
		e.processDue(t)
		if row, _ := e.inboxRow(t, ev.GetEventId()); row.attempts != 1 {
			t.Fatalf("the worker took an event before it was due: %+v", row)
		}
		// git-svc is back and the pause is over: the pipeline starts, without
		// a user because the event named none. The second pause doubles.
		e.src.setErr(down, errors.New("still down"))
		if _, err := e.pool.Exec(ctx, `UPDATE ci_svc.inbox SET next_attempt_at = now() WHERE event_id = $1`, ev.GetEventId()); err != nil {
			t.Fatal(err)
		}
		e.processDue(t)
		if row, _ := e.inboxRow(t, ev.GetEventId()); row.attempts != 2 || row.dueIn < 110*time.Second {
			t.Fatalf("the second pause did not double: %+v", row)
		}
		e.src.mu.Lock()
		delete(e.src.errs, down)
		e.src.mu.Unlock()
		if _, err := e.pool.Exec(ctx, `UPDATE ci_svc.inbox SET next_attempt_at = now() WHERE event_id = $1`, ev.GetEventId()); err != nil {
			t.Fatal(err)
		}
		e.processDue(t)
		if got := e.pushPipelines(t, down); len(got) != 1 || !strings.HasSuffix(got[0], " push -") {
			t.Fatalf("after git-svc came back: %v", got)
		}
		if row, _ := e.inboxRow(t, ev.GetEventId()); !row.processed || row.lastError != "" {
			t.Fatalf("after git-svc came back: %+v", row)
		}

		// No .git2-ci.yml: handled, and no pipeline.
		noCI := e.newRepo(t, "")
		ev = pushEvent(t, noCI, uuid.NewString(), main())
		e.deliver(t, ev)
		e.processDue(t)
		if row, _ := e.inboxRow(t, ev.GetEventId()); !row.processed {
			t.Fatalf("a push without CI is not handled: %+v", row)
		}
		if got := e.pushPipelines(t, noCI); len(got) != 0 {
			t.Fatalf("a push without CI started %v", got)
		}
	})
}

func TestEventSinkRefusesBadEvents(t *testing.T) {
	forEachMode(t, func(t *testing.T, e *env) {
		repo := uuid.New()
		good := func() *eventsv1.DeliverRequest {
			return pushEvent(t, repo, "", &gitv1.RefUpdate{Ref: "refs/heads/main", OldSha: zeroSHA, NewSha: sha()})
		}
		for _, c := range []struct {
			name   string
			change func(*eventsv1.DeliverRequest)
		}{
			{"another type", func(r *eventsv1.DeliverRequest) { r.EventType = "git.v1.RepositoryCreated" }},
			{"no event ID", func(r *eventsv1.DeliverRequest) { r.EventId = "" }},
			{"event ID not a UUID", func(r *eventsv1.DeliverRequest) { r.EventId = "42" }},
			{"no aggregate type", func(r *eventsv1.DeliverRequest) { r.AggregateType = "" }},
			{"aggregate ID not a UUID", func(r *eventsv1.DeliverRequest) { r.AggregateId = "repo" }},
			{"no time", func(r *eventsv1.DeliverRequest) { r.OccurredAt = nil }},
			{"payload not JSON", func(r *eventsv1.DeliverRequest) { r.Payload = "not json" }},
			{"payload of another message", func(r *eventsv1.DeliverRequest) { r.Payload = `{"refUpdates": 3}` }},
			{"no repository in the payload", func(r *eventsv1.DeliverRequest) { r.Payload = `{"pusherId": "x"}` }},
			{"payload jsonb cannot hold", func(r *eventsv1.DeliverRequest) {
				r.Payload = `{"repositoryId": "` + repo.String() + `", "pusherId": "a\u0000b"}`
			}},
		} {
			req := good()
			c.change(req)
			_, err := e.events.Deliver(t.Context(), req)
			wantCode(t, c.name, err, codes.InvalidArgument)
			if _, err := uuid.Parse(req.GetEventId()); err == nil {
				if _, ok := e.inboxRow(t, req.GetEventId()); ok {
					t.Errorf("%s: the event was stored", c.name)
				}
			}
		}

		// Unknown fields are fine: git-svc may add some before we update.
		req := good()
		req.Payload = strings.Replace(req.GetPayload(), "{", `{"someNewField": [1, 2],`, 1)
		e.deliver(t, req)
	})
}
