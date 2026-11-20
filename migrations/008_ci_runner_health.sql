-- ci-svc: runner health and job container use, for monitoring in the console
-- The source of truth for this file is git2's migrations/. git2-runner
-- keeps an identical copy for its tests. Every statement is idempotent.

-- What a runner last reported about its build host and Docker daemon
-- (ci.v1.RunnerHealth in protobuf JSON), and when ci-svc received it.
-- Each report replaces the previous one; there is no history.
ALTER TABLE ci_svc.runners ADD COLUMN IF NOT EXISTS health JSONB;
ALTER TABLE ci_svc.runners ADD COLUMN IF NOT EXISTS health_at TIMESTAMPTZ;

-- The latest resource use of a running job's container
-- (ci.v1.ContainerStats in protobuf JSON), and when ci-svc received it.
-- Kept after the job ends, as its last sample.
ALTER TABLE ci_svc.jobs ADD COLUMN IF NOT EXISTS container_stats JSONB;
ALTER TABLE ci_svc.jobs ADD COLUMN IF NOT EXISTS container_stats_at TIMESTAMPTZ;
