-- Core schema. Private to the core: modules never read these tables directly.

CREATE TABLE core.modules (
    name         text PRIMARY KEY,
    version      text NOT NULL,
    state        text NOT NULL CHECK (state IN ('active', 'disabled', 'uninstalled')),
    installed_at timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- Bumped on every module lifecycle change. Replicas compare it to their own
-- copy and reload when it moves (LISTEN amp_modules carries the new value).
CREATE TABLE core.generation (
    id    int PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    value bigint NOT NULL
);
INSERT INTO core.generation (value) VALUES (0);

CREATE TABLE core.agents (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL UNIQUE,
    description text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- core_api is the read contract for modules. Views run with the privileges of
-- their owner (the core), so a module role can read them without any access
-- to the core schema. Columns here are a public API: add, never rename.
CREATE SCHEMA IF NOT EXISTS core_api;

CREATE VIEW core_api.agents AS
    SELECT id::text AS id, name, description, created_at FROM core.agents;
