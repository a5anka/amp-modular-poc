-- Runs as role amp_mod_ai_gateway with search_path = mod_ai_gateway.
-- Unqualified names land in the module schema.

CREATE TABLE llm_providers (
    name       text PRIMARY KEY,
    kind       text NOT NULL,
    base_url   text NOT NULL,
    models     text[] NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- agent_id refers to core_api.agents.id. No foreign key: the core schema is
-- not visible to this role. The finalize hook on agent delete cleans up.
CREATE TABLE agent_bindings (
    agent_id   text PRIMARY KEY,
    provider   text NOT NULL REFERENCES llm_providers (name),
    model      text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO llm_providers (name, kind, base_url, models) VALUES
    ('openai',    'openai',    'https://api.openai.com/v1',  ARRAY['gpt-4.1', 'gpt-4.1-mini']),
    ('anthropic', 'anthropic', 'https://api.anthropic.com',  ARRAY['claude-sonnet-4-5', 'claude-haiku-4-5']);
