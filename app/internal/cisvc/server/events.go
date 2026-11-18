package server

import (
	"context"

	eventsv1 "github.com/git2-project/git2-codeplane/gen/go/events/v1"

	"github.com/git2-project/git2-runner/app/internal/cisvc/inbox"
)

// eventSinkServer implements eventsv1.EventSinkServer: other services'
// relays deliver their events here (ADR 0010).
//
// The answer tells the relay what to do: OK means stored (or seen before),
// INVALID_ARGUMENT means "never send this again", and anything else, such
// as INTERNAL when the database is down, is retried by the relay.
type eventSinkServer struct {
	eventsv1.UnimplementedEventSinkServer
	d Deps
}

func (s *eventSinkServer) Deliver(ctx context.Context, req *eventsv1.DeliverRequest) (*eventsv1.DeliverResponse, error) {
	ev := inbox.Event{
		ID:            req.GetEventId(),
		Type:          req.GetEventType(),
		AggregateType: req.GetAggregateType(),
		AggregateID:   req.GetAggregateId(),
		Payload:       req.GetPayload(),
	}
	if t := req.GetOccurredAt(); t.IsValid() {
		ev.OccurredAt = t.AsTime()
	}
	if err := s.d.Inbox.Deliver(ctx, ev); err != nil {
		return nil, toStatus(ctx, s.d.Log, err)
	}
	return &eventsv1.DeliverResponse{}, nil
}
