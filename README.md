# Agent Manager: modular core PoC

A proof of concept for a modular Agent Manager. It is one Go binary that persists to Postgres and serves its own console. First-party modules are compiled into that binary but stay dormant until you install them. Installing a module:

1. creates the module's own database schema and role, and runs its migrations;
2. mounts its API under `/m/<module>`;
3. registers its hooks, event handlers, capabilities and permissions;
4. adds its pages and slot components to the console.

This happens on every replica, while they run, with no extra process, container or restart.

Some modules also have code that must not run inside the core. The evaluation module runs user-written evaluator code, so it is split in two:
- its **control part** (API, data, run queue, UI, hooks) is in the binary like any other module;
- its **workloads** run outside the binary: an `eval-controller` that launches one isolated job per custom-code run, and a `shared-worker` pool for built-in evaluators.

Workloads never touch the database. They lease work and post results over the module's internal API, with credentials and tokens the core issues. See [Workloads](#workloads-module-code-outside-the-binary).

> **Status:** exploratory PoC for design discussion, not product code. See [What is simplified](#what-is-simplified) before drawing conclusions.

![Modules page](docs/img/modules.png)

## What it demonstrates

| Design point | Where to see it |
|---|---|
| Single binary; core runs on its own | `amp-core` (no modules linked) and `amp` (all modules) both run on the same database |
| Install creates the module's DB | `mod_<name>` schema + `amp_mod_<name>` role per module; module SQL runs as that role |
| Data isolation | A module can read the `core_api` views. It cannot read `core.*` tables or another module's schema (enforced by the DB, covered by tests) |
| Atomic install | Schema, migrations and registration happen in one transaction. A failing migration leaves nothing behind |
| UI updates without a rebuild | The shell loads module ESM bundles at runtime through an import map. Nav, pages and slots appear and disappear live over SSE |
| Tight integration with the core | ai-gateway adds a field to the core create-agent form, validates it in a hook, stores it on `agent.created`, renders a section on the agent page, and cleans up in a finalize hook |
| Integration between modules | evaluation **requires** `traces.query/v1` (observability) and **optionally** uses `llm.proxy/v1` (ai-gateway). It fills a slot that observability owns (the Score column) |
| Dependency safety | Install refuses missing dependencies (or resolves them with `--with-deps`). Disable and uninstall refuse while a dependent needs the module |
| Multi-replica | One replica installs; the others notice within a second by polling `core.generation` (`--poll-interval`). An advisory lock serialises lifecycle changes and migrations |
| Upgrade on deploy | A new binary with a new migration upgrades the module on boot. Editing an applied migration is refused |
| Catch-up on late install | observability backfills traces for agents that existed before it was installed |
| Permissions from modules | Modules declare permissions and grant them to core roles. `/api/me`, the API and the UI all follow |
| Workloads outside the binary | evaluation declares `eval-controller`, `shared-worker` and a per-run `custom-evaluator` job. Without them it is active but **degraded**, and runs wait in the queue |
| Isolation of user code | Custom evaluator code only ever goes to the isolated lane. A trusted launcher holds the run token; the code runs in a child interpreter with an empty environment (covered by an e2e test) |
| Core-issued workload identity | Workload credentials (hashed, revocable) are exchanged on every heartbeat for 15-minute EdDSA tokens. Run tokens are scoped to one run. All of them verify against `/.well-known/jwks.json` |
| Cross-module tokens | A run token minted by evaluation is accepted by ai-gateway for judge calls, and only for those |
| Dev mode | `amp serve --dev-workers` runs the same workload code in-process (no isolation), so a laptop needs a single command |

## Quick start

You need Go 1.25+. No Docker is needed: `--embedded-db` starts a local Postgres. The first run downloads the Postgres binaries to `~/.amp`.

```bash
git clone https://github.com/a5anka/amp-modular-poc.git
cd amp-modular-poc
go build -o bin/ ./cmd/...

./bin/amp serve --embedded-db          # terminal 1, then open http://localhost:8080
```

In a second terminal, with the console open next to it:

```bash
./bin/amp module list
./bin/amp module install evaluation              # refused: requires traces.query/v1
./bin/amp module install evaluation --with-deps  # installs observability, then evaluation
./bin/amp module install ai-gateway
./bin/amp module disable ai-gateway              # LLM Providers page disappears; evaluation falls back to its heuristic judge
./bin/amp module enable ai-gateway
./bin/amp module uninstall ai-gateway            # data kept, reinstall resumes
./bin/amp module uninstall ai-gateway --purge    # schema dropped
```

### Evaluation workloads

Installing evaluation leaves it **degraded** until its workloads connect:

```bash
./bin/amp module health evaluation        # degraded: no eval-controller, no shared-worker

# Production shape: two separate processes with their own credentials
AMP_WORKLOAD_CREDENTIAL=$(./bin/amp module credential evaluation eval-controller | sed -n 2p | tr -d ' ') \
  ./bin/amp-eval-worker controller &      # launches one process per isolated run (--runner kubernetes creates Jobs)
AMP_WORKLOAD_CREDENTIAL=$(./bin/amp module credential evaluation shared-worker | sed -n 2p | tr -d ' ') \
  ./bin/amp-eval-worker shared-worker &

./bin/amp module health evaluation        # ok: 1 controller, 1 shared worker
```

For local development, skip all that: `./bin/amp serve --embedded-db --dev-workers` runs both workloads inside the core. It is the same code and protocol, with no process boundary and no sandbox.

Custom evaluators are Python and need `python3` wherever the launcher runs. On the **Evaluations** page, pick *Custom code (isolated lane)* and queue a run; the sample code reports how many credentials it can see (0).

![Evaluations page](docs/img/evaluations.png)

Or run the guided walkthrough, which covers every row of the table above:

```bash
./scripts/demo.sh            # PAUSE=1 ./scripts/demo.sh to step through it
```

Start the core without `--dev-workers` for the walkthrough: it starts the workloads itself, as separate processes.

Things to try in the console:
- **Agents:** create an agent. With ai-gateway installed, the form has an LLM field contributed by the module. Pick `openai/not-a-model` to see the module's validate hook reject the core request.
- **Agent page:** sections from ai-gateway, observability and evaluation, each tagged with the module it came from.
- **Traces:** an observability page. The Score column comes from evaluation; disable evaluation and the column disappears.
- **Role switcher** (top right): `viewer` loses the write buttons, because the module permissions decide what it may do.
- **Activity:** live feed of module changes and event deliveries.

![Agent page with module sections](docs/img/agent-detail.png)

### Two replicas

```bash
./bin/amp serve --embedded-db --replica r1
./bin/amp serve --addr :8081 --replica r2 \
  --db 'postgres://postgres:postgres@localhost:5439/postgres?sslmode=disable'

AMP_SERVER=http://localhost:8081 ./bin/amp module install ai-gateway   # install via r2...
# ...and the console on http://localhost:8080 (r1) updates in place.
```

Or with Docker (two replicas and Postgres 17):

```bash
docker compose up --build     # http://localhost:8080 and http://localhost:8081
```

## How it works

```
cmd/amp            full distribution: core + every module (imports modules/all)
cmd/amp-core       core only
cmd/amp-eval-worker evaluation workloads: controller | shared-worker | launcher
modkit/            the module contract: Module, Manifest, Runtime, Register()
modkit/contracts/  versioned capability contracts (tracesv1, llmv1)
internal/db        pools, per-module schema/role, SQL migrator, advisory lock
internal/modhost   registry, resolver, lifecycle, live registrations, UI manifest, SSE
internal/core      HTTP API, agent registry (core resource), hooks into agent create/delete
internal/cli       `amp serve` and `amp module ...`
web/static         console shell, @amp/ui-sdk, vendored Preact + htm (shared singletons)
modules/           ai-gateway, observability, evaluation
modules/evaluation/evalproto  protocol between the evaluation module and its workloads
modules/evaluation/worker     controller, shared worker, launcher, runners (in-process, process, Kubernetes Job)
deploy/k8s         illustrative manifests for the evaluation workloads (not applied in this PoC)
test/e2e           evaluation with real workloads over HTTP
```

### A module

A module is a Go package that registers itself at link time. Linking it does not install it.

```go
func init() { modkit.Register(&Module{}) }

func (*Module) Manifest() modkit.Manifest {
    return modkit.Manifest{
        Name: "evaluation", Version: "0.2.0", CoreAPI: modkit.CoreAPIVersion,
        Requires: []string{tracesv1.Capability},   // "traces.query/v1"
        Optional: []string{llmv1.Capability},      // "llm.proxy/v1"
        Permissions: []modkit.Permission{{Name: "evals.run"}},
        RoleGrants:  map[string][]string{"developer": {"evals.run"}},
        UI: &modkit.UI{
            Nav:   []modkit.NavItem{{Label: "Evaluations", Page: "evaluations"}},
            Slots: []modkit.SlotContribution{{Slot: "observability.trace.columns", Component: "ScoreColumn"}},
        },
    }
}

func (*Module) Migrations() fs.FS { ... } // embedded *.sql, run as the module role in its schema
func (*Module) UI() fs.FS         { ... } // embedded index.js (ESM), served at /modules/<name>/ui/<tag>/

func (*Module) Activate(ctx context.Context, rt modkit.Runtime) error {
    rt.Handle("POST", "/runs", "evals.run", s.createRun)  // -> POST /m/evaluation/runs (queues a run)
    rt.HandleInternal("POST", "/leases", "runs.lease", s.lease) // workloads only, core-issued token
    rt.Hook("agent", modkit.Finalize, s.cleanUp)          // synchronous, can block a delete
    rt.Subscribe("trace.recorded", s.onTrace)             // asynchronous
    traces, _ := modkit.Lookup[tracesv1.Query](rt, tracesv1.Capability) // resolved at call time
    ...
}
```

The UI bundle is plain ESM. It imports `preact/hooks` and `@amp/ui-sdk` by bare specifier; the import map in `index.html` resolves both to the single copy the shell loaded:

```js
import { html, api, useAsync } from "@amp/ui-sdk";
export const pages = { evaluations: Evaluations };
export const slots = { ScoreColumn: { header: "Score", Cell }, AgentScoreSection };
```

### Integration contracts

- **API:** module routes live under `/m/<module>`. The core authenticates the caller, and the route's declared permission is checked against the current permission set.
- **Hooks (synchronous):** `validate` runs before create, `finalize` before delete. A request body can carry `extensions.<module>` blocks. Each block goes to its module's hook, and a block addressed to an inactive module is rejected.
- **Events (asynchronous):** `agent.created`, `agent.deleted`, `trace.recorded`, and so on.
- **Capabilities:** versioned contracts in `modkit/contracts`. Consumers depend on the contract, never on the module that provides it.
- **Data:** the module's own schema, plus read-only `core_api` views. There are no foreign keys into core or into other modules; clean-up goes through finalize hooks.
- **UI:** nav items, pages and slot contributions. A module can own slots of its own (`ownSlots`) that other modules fill.

### Workloads: module code outside the binary

A module declares its workloads in the manifest. The control part never runs their code:

```go
Workloads: []modkit.Workload{
    {Name: "eval-controller", Kind: modkit.Service, Trusted: true, Required: true,
        Scopes: []string{"runs.lease", "runs.dispatch", "runs.status"}, Protocols: []string{"eval/v1"}},
    {Name: "shared-worker", Kind: modkit.Service, Trusted: true, Required: true,
        Scopes: []string{"runs.lease", "runs.read", "runs.report", "ai-gateway:llm.invoke"}, Protocols: []string{"eval/v1"}},
    {Name: "custom-evaluator", Kind: modkit.Job, Trusted: false, Isolation: "gvisor", Egress: "core+gateway"},
},
```

```
                 core binary                                     outside the binary
  ┌────────────────────────────────────────┐
  │ evaluation control part                │   lease, input,   ┌──────────────────────────┐
  │  /m/evaluation/*          (users)      │◄──results────────►│ shared-worker (trusted)  │── judge ──► ai-gateway
  │  /m/evaluation/internal/* (workloads)  │   workload token  └──────────────────────────┘
  │  runs queue in mod_evaluation          │                   ┌──────────────────────────┐
  │  traces via traces.query/v1            │◄──lease, token───►│ eval-controller (trusted)│
  │ core: credentials, tokens, JWKS,       │                   │  Runner: process | K8s Job│
  │       workload registry, health        │                   └────────────┬─────────────┘
  └────────────────────────────────────────┘                                │ one job per run
                      ▲                                         ┌────────────▼─────────────┐
                      └──────── input, results (run token) ─────│ launcher (holds token)   │── judge ──► ai-gateway
                                                                │  └ evaluator code: no    │
                                                                │    token, empty env      │
                                                                └──────────────────────────┘
```

1. **Lanes.** A run with only built-in evaluators goes to the shared lane. A run with any custom code goes to the isolated lane, which only `eval-controller` may lease.
2. **Identity.** `amp module credential <module> <workload>` mints a credential for a service workload. It is shown once and stored hashed. Each heartbeat (every 10 s) exchanges it for a 15-minute token carrying the workload's scopes. For each isolated run, the controller asks the module for a **run token**: 10 minutes, valid for that run only, and also accepted by ai-gateway for judge calls.
3. **No data access.** Workloads never connect to Postgres or the trace store. The module builds each run's input (it reads traces through `traces.query/v1`), fixes the run's trace set, and validates results before writing them: traces must belong to the run, scores must be in [0, 1].
4. **Leases.** Leases last 60 s and become 10 minutes once a job is running. Expired leases are requeued, and a run fails after 3 attempts. The queue uses `FOR UPDATE SKIP LOCKED`, so workers can scale out.
5. **Health.** Heartbeats feed a workload registry. A required service with no live instance makes the module `degraded`; `amp module health` and the console show it, with queue depth per lane. Disabling the module makes the core refuse heartbeats, and workers stand by until it is enabled again.
6. **Upgrades.** The module version pins the workload image and lists the protocol versions it accepts. A worker speaking another version is refused at heartbeat.
7. **Runners.** `inprocess` is used by `--dev-workers`, `process` (the default) starts one OS process per run, and `kubernetes` creates a `batch/v1` Job with the gVisor runtime class and no service-account token. Only `eval-controller` holds the RBAC to create Jobs; the core needs none.

### Lifecycle

```
available --install--> active <--enable/disable--> disabled
                          \                          /
                           +---uninstall (keep)---> uninstalled --install--> active (resumes)
                           +---uninstall --purge--> available (schema + role dropped)
```

Every change takes `pg_advisory_xact_lock`, applies DDL and the state change in one transaction, and bumps `core.generation`. The replica that made the change reloads at once. Every other replica reads `core.generation` once per poll interval (default 1 s, `--poll-interval` or `AMP_POLL_INTERVAL`) and reloads when it has moved. Polling is one single-row read per replica per interval, works on any database, and catches up after a restart or a missed change. On reload, a replica rebuilds an immutable snapshot of routes, hooks, subscriptions, capabilities and grants, and swaps it in atomically. Open consoles are told over SSE and re-render.

**Upgrading** means shipping a new binary whose module version has new migration files. On boot, under the same lock, the core applies any pending migrations for installed modules. Use expand/contract migrations, so that old and new replicas can run side by side during a rolling deploy. Applied files are checksummed and must never be edited.

### Distributions

`cmd/amp` and `cmd/amp-core` differ only in their imports. A distribution can install only the modules it links. If the database says a module is active but the binary does not include it, the module is skipped with a warning, and its data is left untouched.

## Tests

```bash
go test ./...        # resolver unit tests, lifecycle integration tests, evaluation e2e (embedded Postgres)
go test -short ./... # unit tests only
```

The integration tests register their own throwaway modules. They check:
- dependency resolution;
- that a second replica picks up changes;
- the DB permission boundaries;
- rollback of a failing migration;
- disable and enable keeping data;
- that purge drops the schema and role;
- upgrade on boot;
- that an edited migration is refused.

The evaluation e2e test (`test/e2e`) runs the real controller and shared worker against the core over HTTP. It checks:
- the module stays degraded until the workloads connect;
- user sessions, tampered tokens and old protocol versions are refused on workload routes;
- the controller may not lease the shared lane or read run inputs;
- a run token works for its own run only, and ai-gateway accepts it for judge calls;
- results with foreign traces or out-of-range scores are rejected;
- custom code runs in the isolated lane without seeing any credential (needs `python3`);
- the Kubernetes Job manifest uses gVisor, no service-account token and dropped capabilities.

A GitHub Actions workflow runs `gofmt`, `go vet`, the tests and a smoke test of the full binary. It lives in `ci/github-actions-ci.yml`; move it to `.github/workflows/` to turn it on. It was kept out of `.github/` only because the token that pushed this repo lacks the `workflow` scope.

## What is simplified

- **Auth** is a role picked with the `X-AMP-Role` header. In production this is the existing OIDC + RBAC.
- **Events** are delivered in-process on the replica that published them, with no retries. Production would put them on Agent Manager's existing outbox + NATS bus.
- **The module DB role** is a guard rail for first-party code, not a security boundary: a session can `RESET ROLE`. That is fine for compiled-in first-party modules. Untrusted code would need its own login role (or WASM / out-of-process).
- **UI stack:** the console uses Preact + htm with no build step. The same pattern (import map + shared singletons + ESM bundles) applies to React 19, Oxygen UI and TanStack Query; module bundles would then come from a Vite library build with those packages marked as externals.
- **No module configuration or secrets yet**, and no down-migrations.
- **OnInstall catch-up** runs best-effort after commit, on the installing replica.
- **The LLM judge is simulated.** ai-gateway's `/internal/judge` returns a deterministic score; it shows token checks and metering, not a real model call. In production the gateway runtime would verify run tokens against the core JWKS.
- **Workload credentials** are shared secrets. Production would bind workloads to a platform identity (projected service-account tokens, or ThunderID clients).
- **Isolation outside Kubernetes is process-level only.** The `process` and `inprocess` runners give the evaluator code an empty environment but no sandbox. The Kubernetes runner and `deploy/k8s` (gVisor, NetworkPolicy) are rendered and unit-tested, but not applied to a cluster. The run token is passed in the Job's env; production would use a per-run Secret or a token exchange.
- **Signing keys** are stored unencrypted in `core.signing_keys`, and there is no key rotation.
- **Docker artefacts** (Dockerfile, compose) have not been run.

## Mapping to Agent Manager today

| PoC | Agent Manager today |
|---|---|
| per-module SQL migrations tracked in `core.module_migrations` | one global Go migration list (`db_migrations/migration_list.go`) |
| event types declared in module manifests | fixed catalog in `events/catalog.go` |
| permissions declared by modules | fixed `FeatureAccessConfig` and `rbac/predefined_roles.go` |
| slots declared by the core and by modules | fixed `MountPoints` enum, page packages imported at build time |
| console served by the binary (`go:embed`) | separate nginx container |
| Polling `core.generation` | Nothing equivalent today; a NATS `modules.changed` message could shorten the delay later, with polling kept as the safety net |
| evaluation workloads (`eval-controller`, `shared-worker`, isolated jobs) with run tokens | `monitor_executor`/`monitor_scheduler` in the service, `evaluation-job` launched through OpenChoreo workflows; the eval execution redesign proposes the same split |

## License

Apache-2.0. Vendored: Preact (MIT) and htm (Apache-2.0); see `web/static/vendor/`.
