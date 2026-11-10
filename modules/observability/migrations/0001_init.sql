-- Runs as role amp_mod_observability with search_path = mod_observability.

CREATE TABLE traces (
    id          text PRIMARY KEY DEFAULT gen_random_uuid()::text,
    agent_id    text NOT NULL,          -- core_api.agents.id, cleaned up by the finalize hook
    name        text NOT NULL,
    status      text NOT NULL CHECK (status IN ('ok', 'error')),
    duration_ms int  NOT NULL DEFAULT 0,
    input       text NOT NULL DEFAULT '',
    output      text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX traces_agent_idx ON traces (agent_id, created_at DESC);
