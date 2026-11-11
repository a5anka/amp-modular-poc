-- evaluation 0.2.0: runs are queued and executed by workloads outside the
-- core binary. Expand-only change, so 0.1.0 replicas keep working during a rollout.

CREATE TABLE runs (
    id            text PRIMARY KEY DEFAULT gen_random_uuid()::text,
    agent_id      text NOT NULL,
    lane          text NOT NULL CHECK (lane IN ('shared', 'isolated')),
    evaluator     text NOT NULL CHECK (evaluator IN ('builtin', 'custom')),
    code          text,                 -- custom evaluator source; only ever sent to the isolated lane
    trace_ids     text[],               -- fixed when a worker fetches the input; results must match it
    status        text NOT NULL DEFAULT 'queued'
                  CHECK (status IN ('queued', 'leased', 'running', 'succeeded', 'failed')),
    attempts      int  NOT NULL DEFAULT 0,
    lease_owner   text,
    lease_expires timestamptz,
    message       text NOT NULL DEFAULT '',
    evaluated     int  NOT NULL DEFAULT 0,
    average       numeric(4, 2),
    evaluated_by  text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX runs_queue_idx ON runs (lane, status, created_at);
CREATE INDEX runs_agent_idx ON runs (agent_id, created_at DESC);

ALTER TABLE scores ADD COLUMN run_id text;
