-- Workloads: processes a module runs outside the core binary (controllers,
-- worker pools, per-run jobs). The core issues their credentials and tokens
-- and tracks which instances are connected.

-- Signing keys for workload and run tokens. Shared by every replica so a
-- token minted by one replica verifies on all of them and at the AI Gateway.
CREATE TABLE core.signing_keys (
    kid         text PRIMARY KEY,
    private_key bytea NOT NULL,          -- ed25519 seed (PoC: unencrypted; production: KMS / secrets API)
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- Long-lived credential a deployment of a workload uses to obtain short-lived
-- tokens. Stored hashed; shown once at creation; revocable.
CREATE TABLE core.workload_credentials (
    id          text PRIMARY KEY DEFAULT gen_random_uuid()::text,
    module      text NOT NULL,
    workload    text NOT NULL,
    secret_hash text NOT NULL UNIQUE,
    created_at  timestamptz NOT NULL DEFAULT now(),
    revoked_at  timestamptz
);

-- Connected instances, refreshed by heartbeats (each heartbeat also returns a fresh token).
CREATE TABLE core.workload_instances (
    module      text NOT NULL,
    workload    text NOT NULL,
    instance    text NOT NULL,
    protocol    text NOT NULL,
    version     text NOT NULL DEFAULT '',
    first_seen  timestamptz NOT NULL DEFAULT now(),
    last_seen   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (module, workload, instance)
);
