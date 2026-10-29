-- ci-svc: runners, pipelines, jobs, job logs, secrets and the outbox in the ci_svc schema
-- Postgres is the source of truth and the job queue; Redis only carries
-- wake-up signals. The source of truth for this file is git2's
-- migrations/. git2-runner keeps an identical copy for its tests.
CREATE SCHEMA IF NOT EXISTS ci_svc;

-- A runner registered with a one-time token. token_sha256 is the hex
-- SHA-256 of the runner token; the token itself is never stored.
-- last_seen_at is updated when the runner asks for work, which is what
-- "online" means in the console.
CREATE TABLE IF NOT EXISTS ci_svc.runners (
    id           UUID        PRIMARY KEY,
    name         TEXT        NOT NULL,
    description  TEXT        NOT NULL DEFAULT '',
    labels       TEXT[]      NOT NULL DEFAULT '{}',
    executor     TEXT        NOT NULL,
    version      TEXT        NOT NULL DEFAULT '',
    platform     TEXT        NOT NULL DEFAULT '',
    token_sha256 TEXT        NOT NULL,
    paused       BOOLEAN     NOT NULL DEFAULT FALSE,
    last_seen_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT runners_token_sha256_key UNIQUE (token_sha256)
);

-- One-time tokens that register a runner. Only the hash is stored.
-- used_at is set when a runner registers with the token.
CREATE TABLE IF NOT EXISTS ci_svc.registration_tokens (
    id           UUID        PRIMARY KEY,
    token_sha256 TEXT        NOT NULL,
    description  TEXT        NOT NULL DEFAULT '',
    expires_at   TIMESTAMPTZ NOT NULL,
    used_at      TIMESTAMPTZ,
    runner_id    UUID,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT registration_tokens_token_sha256_key UNIQUE (token_sha256)
);

-- One run of .git2-ci.yml for one commit. id is a UUIDv7, so ordering by
-- id is ordering by creation time. idempotency_key makes TriggerPipeline
-- safe under at-least-once event delivery. repository_id and
-- triggered_by are IDs from git-svc and auth-svc; there are no foreign
-- keys across schemas.
CREATE TABLE IF NOT EXISTS ci_svc.pipelines (
    id              UUID        PRIMARY KEY,
    repository_id   UUID        NOT NULL,
    ref             TEXT        NOT NULL,
    commit_sha      TEXT        NOT NULL,
    trigger         TEXT        NOT NULL,
    triggered_by    UUID,
    idempotency_key TEXT        NOT NULL,
    status          TEXT        NOT NULL,
    error_message   TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at      TIMESTAMPTZ,
    finished_at     TIMESTAMPTZ,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT pipelines_idempotency_key_key UNIQUE (idempotency_key),
    CONSTRAINT pipelines_trigger_check CHECK (trigger IN ('push', 'manual')),
    CONSTRAINT pipelines_status_check
        CHECK (status IN ('pending', 'running', 'success', 'failed', 'canceled'))
);

-- ListPipelines: newest first, per repository and optionally per ref.
CREATE INDEX IF NOT EXISTS pipelines_repository_idx
    ON ci_svc.pipelines (repository_id, id DESC);
CREATE INDEX IF NOT EXISTS pipelines_repository_ref_idx
    ON ci_svc.pipelines (repository_id, ref, id DESC);

-- One job of a pipeline. The table is also the job queue:
--   created   waits for an earlier stage
--   pending   queued; a runner leases it with FOR UPDATE SKIP LOCKED
--   running   leased by runner_id until lease_expires_at; every heartbeat
--             extends the lease, and a reaper fails expired leases
-- spec is everything the runner needs (image, script, variables, timeout)
-- except secrets, which are added when the job is leased.
-- tags must all be among a runner's labels for it to take the job.
-- log_size is the length of the job's log in job_log_chunks.
CREATE TABLE IF NOT EXISTS ci_svc.jobs (
    id               UUID        PRIMARY KEY,
    pipeline_id      UUID        NOT NULL REFERENCES ci_svc.pipelines (id) ON DELETE CASCADE,
    stage            TEXT        NOT NULL,
    stage_index      INTEGER     NOT NULL,
    name             TEXT        NOT NULL,
    spec             JSONB       NOT NULL,
    tags             TEXT[]      NOT NULL DEFAULT '{}',
    status           TEXT        NOT NULL,
    runner_id        UUID        REFERENCES ci_svc.runners (id) ON DELETE SET NULL,
    lease_expires_at TIMESTAMPTZ,
    cancel_requested BOOLEAN     NOT NULL DEFAULT FALSE,
    exit_code        INTEGER,
    failure_reason   TEXT        NOT NULL DEFAULT '',
    log_size         BIGINT      NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    queued_at        TIMESTAMPTZ,
    started_at       TIMESTAMPTZ,
    finished_at      TIMESTAMPTZ,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT jobs_pipeline_name_key UNIQUE (pipeline_id, name),
    CONSTRAINT jobs_status_check CHECK (status IN
        ('created', 'pending', 'running', 'success', 'failed', 'canceled', 'skipped')),
    CONSTRAINT jobs_failure_reason_check CHECK (failure_reason IN
        ('', 'script_failure', 'timeout', 'runner_lost', 'image_pull_failure', 'runner_system_failure'))
);

-- The queue: pending jobs in the order they were queued.
CREATE INDEX IF NOT EXISTS jobs_pending_idx
    ON ci_svc.jobs (queued_at, id)
    WHERE status = 'pending';

-- The reaper: running jobs by lease expiry.
CREATE INDEX IF NOT EXISTS jobs_running_lease_idx
    ON ci_svc.jobs (lease_expires_at)
    WHERE status = 'running';

-- A job's log as the runner sent it, one row per AppendLog call.
-- "offset" is the position of data's first byte in the log; chunks do not
-- overlap and leave no gaps.
CREATE TABLE IF NOT EXISTS ci_svc.job_log_chunks (
    job_id     UUID        NOT NULL REFERENCES ci_svc.jobs (id) ON DELETE CASCADE,
    "offset"   BIGINT      NOT NULL,
    data       BYTEA       NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (job_id, "offset")
);

-- CI secrets, encrypted with AES-256-GCM by ci-svc. The key is not in the
-- database (GIT2_CI_SECRETS_KEY); key_id names the key a row was
-- encrypted with. scope_id is the repository ID, or the nil UUID for the
-- instance scope, so one unique constraint covers both.
CREATE TABLE IF NOT EXISTS ci_svc.secrets (
    id         UUID        PRIMARY KEY,
    scope      TEXT        NOT NULL,
    scope_id   UUID        NOT NULL,
    name       TEXT        NOT NULL,
    ciphertext BYTEA       NOT NULL,
    nonce      BYTEA       NOT NULL,
    key_id     TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT secrets_scope_name_key UNIQUE (scope, scope_id, name),
    CONSTRAINT secrets_scope_check CHECK (scope IN ('instance', 'repository'))
);

-- Domain events, in the same format as git_svc.outbox, written in the same
-- transaction as the change they describe. A relay delivers pending rows
-- at least once.
--   event_id:     lets consumers drop duplicates
--   event_type:   full protobuf message name, e.g. 'ci.v1.PipelineFinished'
--   payload:      the event message in canonical protobuf JSON
--   published_at: NULL until the relay has delivered the event
CREATE TABLE IF NOT EXISTS ci_svc.outbox (
    id             BIGSERIAL   PRIMARY KEY,
    event_id       UUID        NOT NULL,
    aggregate_type TEXT        NOT NULL,
    aggregate_id   UUID        NOT NULL,
    event_type     TEXT        NOT NULL,
    payload        JSONB       NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at   TIMESTAMPTZ,
    CONSTRAINT outbox_event_id_key UNIQUE (event_id)
);

-- Lets the relay find pending events without scanning delivered ones.
CREATE INDEX IF NOT EXISTS outbox_unpublished_idx
    ON ci_svc.outbox (id)
    WHERE published_at IS NULL;
