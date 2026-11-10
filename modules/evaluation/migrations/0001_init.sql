-- Runs as role amp_mod_evaluation with search_path = mod_evaluation.
-- trace_id refers to traces owned by observability. Evaluation never reads
-- mod_observability: it gets traces through the traces.query/v1 capability.

CREATE TABLE scores (
    trace_id   text PRIMARY KEY,
    agent_id   text NOT NULL,
    score      numeric(4, 2) NOT NULL,
    evaluator  text NOT NULL,
    reason     text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX scores_agent_idx ON scores (agent_id);
