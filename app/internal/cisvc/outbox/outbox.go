// Package outbox records domain events in ci_svc.outbox.
//
// An event is written in the same transaction as the change it describes,
// so it exists if and only if that change was committed. The row format is
// the team's, the same as git_svc.outbox (codeplane ADR 0007); a shared
// relay delivers pending rows at least once.
package outbox

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Aggregate types of ci-svc's events.
const (
	AggregatePipeline = "pipeline"
	AggregateJob      = "job"
)

// The payload is sent as text and cast to jsonb. A []byte argument would
// travel as bytea when pgx runs in exec mode, which it does behind a
// transaction pooler (see package db).
const insertSQL = `
INSERT INTO ci_svc.outbox (event_id, aggregate_type, aggregate_id, event_type, payload)
VALUES ($1, $2, $3, $4, $5::jsonb)`

// Write adds event to the outbox inside tx. It takes a pgx.Tx rather than
// a pool on purpose: an event written outside the data transaction could
// be published for a change that was rolled back.
func Write(ctx context.Context, tx pgx.Tx, aggregateType string, aggregateID uuid.UUID, event proto.Message) error {
	eventType, payload, err := encode(event)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, insertSQL, uuid.New(), aggregateType, aggregateID, eventType, payload); err != nil {
		return fmt.Errorf("insert %s into outbox: %w", eventType, err)
	}
	return nil
}

// encode returns the event type (the full protobuf message name, for
// example "ci.v1.PipelineFinished") and the payload (the message in
// canonical protobuf JSON).
func encode(event proto.Message) (eventType, payload string, err error) {
	eventType = string(proto.MessageName(event))
	b, err := protojson.Marshal(event)
	if err != nil {
		return "", "", fmt.Errorf("encode %s: %w", eventType, err)
	}
	return eventType, string(b), nil
}
