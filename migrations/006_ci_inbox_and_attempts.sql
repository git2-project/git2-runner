-- ci-svc: job attempts for RetryJob and the inbox of delivered events (ADR 0010)
-- The source of truth for this file is git2's migrations/. git2-runner
-- keeps an identical copy for its tests. Every statement is idempotent.

-- RetryJob runs a finished job again as a new row in the same pipeline:
-- attempt N+1, whose retry_of is attempt N. Earlier attempts keep their
-- status and their log. A pipeline shows the newest attempt of each job.
--   idempotency_key: set on the attempt RetryJob created, so a repeated
--                    call returns it instead of making another one
--   triggered_by:    ID of the user who retried (from auth-svc), if known
ALTER TABLE ci_svc.jobs ADD COLUMN IF NOT EXISTS attempt INTEGER NOT NULL DEFAULT 1;
ALTER TABLE ci_svc.jobs ADD COLUMN IF NOT EXISTS retry_of UUID REFERENCES ci_svc.jobs (id) ON DELETE SET NULL;
ALTER TABLE ci_svc.jobs ADD COLUMN IF NOT EXISTS idempotency_key TEXT;
ALTER TABLE ci_svc.jobs ADD COLUMN IF NOT EXISTS triggered_by UUID;

-- A job name is unique per attempt now, not per pipeline.
ALTER TABLE ci_svc.jobs DROP CONSTRAINT IF EXISTS jobs_pipeline_name_key;
CREATE UNIQUE INDEX IF NOT EXISTS jobs_pipeline_name_attempt_key
    ON ci_svc.jobs (pipeline_id, name, attempt);
CREATE UNIQUE INDEX IF NOT EXISTS jobs_idempotency_key_key
    ON ci_svc.jobs (idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- Events that other services' relays delivered to ci-svc's
-- events.v1.EventSink (ADR 0010). Deliver only stores the event; a worker
-- handles it afterwards.
--   event_id:        unique, so a redelivered event changes nothing
--   payload:         the event message in protobuf JSON, as delivered
--   processed_at:    NULL until the worker has handled the event
--   attempts, last_error, next_attempt_at: the worker's retries
CREATE TABLE IF NOT EXISTS ci_svc.inbox (
    event_id        UUID        PRIMARY KEY,
    event_type      TEXT        NOT NULL,
    aggregate_type  TEXT        NOT NULL,
    aggregate_id    UUID        NOT NULL,
    occurred_at     TIMESTAMPTZ NOT NULL,
    payload         JSONB       NOT NULL,
    received_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at    TIMESTAMPTZ,
    attempts        INTEGER     NOT NULL DEFAULT 0,
    last_error      TEXT        NOT NULL DEFAULT '',
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Lets the worker find unhandled events in the order they are due.
CREATE INDEX IF NOT EXISTS inbox_unprocessed_idx
    ON ci_svc.inbox (next_attempt_at, received_at)
    WHERE processed_at IS NULL;
