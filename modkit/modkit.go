// Package modkit is the contract between the Agent Manager core and a module.
//
// A module is a Go package compiled into the amp binary. It registers itself
// from init() (like a database/sql driver) and stays dormant until an operator
// installs it. Installing a module creates its database schema, runs its
// migrations, mounts its API routes, registers its hooks, event handlers,
// capabilities and permissions, and publishes its UI contributions to the
// console. No separate process or container is involved.
//
// Modules must only talk to the core through this package. They never import
// core internals, never query core tables (they read the core_api views), and
// never query another module's schema (they use capabilities instead). Keeping
// to that rule is what lets a module move out of process later if it has to.
package modkit

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CoreAPIVersion is the version of this contract. A module declares the
// version it was written against; the core refuses modules built for another.
const CoreAPIVersion = 1

// Manifest describes a module: what it needs, what it offers and what it adds
// to the API and the console.
type Manifest struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	CoreAPI     int    `json:"coreApi"`

	// Capabilities are versioned contracts such as "traces.query/v1".
	// Dependencies point at capabilities, not at module names, so a provider
	// can be replaced without touching its consumers.
	Requires []string `json:"requires,omitempty"`
	Optional []string `json:"optional,omitempty"`
	Provides []string `json:"provides,omitempty"`

	// Permissions are namespaced by the core as "<module>:<name>".
	Permissions []Permission `json:"permissions,omitempty"`
	// RoleGrants gives the module's permissions to the core roles
	// (admin, developer, viewer) when the module is installed.
	RoleGrants map[string][]string `json:"roleGrants,omitempty"`

	// Events lists the event types this module publishes.
	Events []string `json:"events,omitempty"`

	UI *UI `json:"ui,omitempty"`

	// Workloads are the parts of the module that run outside the core binary,
	// for code that must be isolated (evaluator code) or scaled separately.
	// The module's control part (API, data, UI, hooks) always stays in-process.
	Workloads []Workload `json:"workloads,omitempty"`
}

// WorkloadKind says how a workload runs.
type WorkloadKind string

const (
	// Service is long-running (a Deployment). Instances register, heartbeat
	// and are counted for module health.
	Service WorkloadKind = "service"
	// Job runs once per unit of work (one evaluation run), launched by
	// another workload. Jobs authenticate with run tokens, not credentials.
	Job WorkloadKind = "job"
)

// Workload declares one out-of-process part of a module.
type Workload struct {
	Name string       `json:"name"`
	Kind WorkloadKind `json:"kind"`
	// Trusted workloads run first-party code only. Untrusted ones run user
	// code and must be isolated.
	Trusted bool `json:"trusted"`
	// Image is pinned by the module version, so upgrading the binary
	// upgrades the workloads it launches.
	Image string `json:"image,omitempty"`
	// Isolation and Egress document the sandbox the platform must provide.
	Isolation string `json:"isolation,omitempty"` // e.g. "gvisor"
	Egress    string `json:"egress,omitempty"`    // e.g. "core+gateway"
	// Scopes the workload's tokens carry (module-local names).
	Scopes []string `json:"scopes,omitempty"`
	// Protocols this module version accepts from the workload, newest first.
	// Accepting N and N-1 lets old and new workers overlap during a rollout.
	Protocols []string `json:"protocols,omitempty"`
	// Required services make the module "degraded" when no instance is connected.
	Required    bool   `json:"required,omitempty"`
	Description string `json:"description,omitempty"`
}

// Permission is a module-defined permission.
type Permission struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// UI lists what the module adds to the console. Components are named exports
// of the module's ESM bundle (index.js): pages under `pages`, slot
// components under `slots`.
type UI struct {
	Nav   []NavItem          `json:"nav,omitempty"`
	Slots []SlotContribution `json:"slots,omitempty"`
	// OwnSlots lists extension points this module renders and other modules may fill.
	OwnSlots []string `json:"ownSlots,omitempty"`
}

// NavItem adds an entry to the console sidebar that opens one of the module's pages.
type NavItem struct {
	Label      string `json:"label"`
	Page       string `json:"page"`
	Permission string `json:"permission,omitempty"`
}

// SlotContribution fills an extension point owned by the core or another module.
type SlotContribution struct {
	Slot       string `json:"slot"`
	Component  string `json:"component"`
	Permission string `json:"permission,omitempty"`
}

// Module is implemented by every module.
type Module interface {
	Manifest() Manifest
	// Migrations returns *.sql files applied in lexical order inside the
	// module's own schema, or nil.
	Migrations() fs.FS
	// UI returns the module's prebuilt console bundle (index.js and assets), or nil.
	UI() fs.FS
	// Activate registers routes, hooks, event handlers and capabilities. The
	// core calls it on every replica each time the set of active modules
	// changes, with a fresh Runtime, so it must be cheap and side-effect free.
	Activate(ctx context.Context, rt Runtime) error
}

// Installer is optionally implemented by modules that must catch up with
// existing core state when they are installed (a module may be installed long
// after the core started, so it cannot rely on events alone).
type Installer interface {
	OnInstall(ctx context.Context, rt Runtime) error
}

// Phase of a synchronous resource hook.
type Phase string

const (
	// Validate runs before a resource is created or updated. Returning an error rejects the request.
	Validate Phase = "validate"
	// Finalize runs before a resource is deleted. Returning an error blocks the delete;
	// returning nil means the module has cleaned up its own data for that resource.
	Finalize Phase = "finalize"
)

// HookRequest is passed to a hook.
type HookRequest struct {
	Resource string `json:"resource"` // e.g. "agent"
	Action   string `json:"action"`   // create, update, delete
	ID       string `json:"id,omitempty"`
	// Object is the resource as submitted or stored.
	Object map[string]any `json:"object,omitempty"`
	// Extension is the part of the request body addressed to this module
	// (body.extensions["<module>"]), or nil.
	Extension json.RawMessage `json:"extension,omitempty"`
}

// HookFunc handles a resource hook.
type HookFunc func(ctx context.Context, req HookRequest) error

// Event is an asynchronous notification.
type Event struct {
	Type   string          `json:"type"`
	Source string          `json:"source"`
	Time   time.Time       `json:"time"`
	Data   json.RawMessage `json:"data"`
}

// EventHandler handles an event. Handlers run asynchronously.
type EventHandler func(ctx context.Context, e Event) error

// Agent is the core resource modules most often attach data to.
type Agent struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"createdAt"`
}

// AgentCreated is the payload of the core "agent.created" event.
type AgentCreated struct {
	Agent      Agent                      `json:"agent"`
	Extensions map[string]json.RawMessage `json:"extensions,omitempty"`
}

// CoreAPI is the read API the core offers to modules.
type CoreAPI interface {
	ListAgents(ctx context.Context) ([]Agent, error)
	GetAgent(ctx context.Context, id string) (Agent, error)
}

// Runtime is what the core gives a module when it activates it.
type Runtime interface {
	Name() string
	// DB is a pool that runs as the module's own database role with
	// search_path set to the module's schema. It can read the core_api views.
	DB() *pgxpool.Pool
	Core() CoreAPI
	Logger() *slog.Logger

	// Handle mounts an API route under /m/<module>. perm is a module
	// permission name (without the module prefix) or a core permission such
	// as "agents:read"; "" means any signed-in user.
	Handle(method, path, perm string, h http.HandlerFunc)
	// Hook registers a synchronous hook on a core resource.
	Hook(resource string, phase Phase, h HookFunc)
	// Subscribe registers an asynchronous event handler.
	Subscribe(eventType string, h EventHandler)
	// Publish emits an event declared in the manifest.
	Publish(ctx context.Context, eventType string, data any) error

	// Can reports whether the caller of the current request holds perm (a
	// module-local or "<module>:<name>" permission).
	Can(ctx context.Context, perm string) bool

	// HandleInternal mounts a route under /m/<module>/internal for the
	// module's workloads. Callers present a core-issued bearer token whose
	// audience includes this module and whose scopes include scope.
	HandleInternal(method, path, scope string, h http.HandlerFunc)
	// IssueToken mints a short-lived token for a workload or job, e.g. a
	// run token for one evaluation run. The audience is this module plus the
	// providers of capabilities it requires or optionally uses (so a run
	// token can call the AI Gateway). Scopes are module-local names.
	IssueToken(ctx context.Context, req TokenRequest) (string, error)

	// Provide registers the implementation of a capability listed in Provides.
	Provide(capability string, impl any)
	// Capability resolves a capability at call time. ok is false when no
	// active module provides it (always possible for optional ones).
	Capability(name string) (impl any, ok bool)
}

// TokenRequest describes a token to mint.
type TokenRequest struct {
	Subject string            // e.g. "run:<id>"
	Scopes  []string          // module-local scope names
	TTL     time.Duration     // capped by the core
	Claims  map[string]string // extra string claims, e.g. {"run": id}
}

// Claims are the verified claims of a workload or run token.
type Claims struct {
	Subject  string            `json:"sub"`
	Module   string            `json:"mod"`
	Workload string            `json:"wl,omitempty"`
	Audience []string          `json:"aud"`
	Scopes   []string          `json:"scopes"`
	Extra    map[string]string `json:"ext,omitempty"`
	Expiry   time.Time         `json:"-"`
}

// HasScope reports whether the token carries scope (qualified "<module>:<name>").
func (c Claims) HasScope(scope string) bool {
	for _, s := range c.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

type claimsKey struct{}

// WithClaims stores verified token claims in a context (used by the core).
func WithClaims(ctx context.Context, c Claims) context.Context {
	return context.WithValue(ctx, claimsKey{}, c)
}

// ClaimsFrom returns the token claims of an internal request.
func ClaimsFrom(ctx context.Context) (Claims, bool) {
	c, ok := ctx.Value(claimsKey{}).(Claims)
	return c, ok
}

// DevWorkloads is optionally implemented by modules whose workloads can run
// in-process for local development (`amp serve --dev-workers`). They still
// talk to the core over HTTP with real credentials, so the protocol is the
// same as in production; only the process boundary is missing.
type DevWorkloads interface {
	RunDevWorkloads(ctx context.Context, env DevEnv) error
}

// DevEnv is what the core gives dev workloads.
type DevEnv struct {
	CoreURL string
	// Credential mints a workload credential for one of the module's workloads.
	Credential func(ctx context.Context, workload string) (string, error)
	Logger     *slog.Logger
}

// HealthReporter is optionally implemented by modules that add detail to
// their health, e.g. queue depth per lane.
type HealthReporter interface {
	Health(ctx context.Context, rt Runtime) map[string]any
}

// Lookup resolves a capability and asserts its contract type.
func Lookup[T any](rt Runtime, capability string) (T, bool) {
	var zero T
	impl, ok := rt.Capability(capability)
	if !ok {
		return zero, false
	}
	t, ok := impl.(T)
	return t, ok
}

// Principal is the caller of an API request.
type Principal struct {
	User string `json:"user"`
	Role string `json:"role"`
}

type principalKey struct{}

// WithPrincipal stores the caller in a context (used by the core).
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the caller of the current request.
func PrincipalFrom(ctx context.Context) Principal {
	p, _ := ctx.Value(principalKey{}).(Principal)
	return p
}

var (
	regMu    sync.Mutex
	registry = map[string]Module{}
)

// Register makes a module available in this binary. Call it from init().
// Being registered does not install the module.
func Register(m Module) {
	regMu.Lock()
	defer regMu.Unlock()
	name := m.Manifest().Name
	if _, dup := registry[name]; dup {
		panic("modkit: module registered twice: " + name)
	}
	registry[name] = m
}

// Registered returns the modules compiled into this binary, sorted by name.
func Registered() []Module {
	regMu.Lock()
	defer regMu.Unlock()
	out := make([]Module, 0, len(registry))
	for _, m := range registry {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Manifest().Name < out[j].Manifest().Name })
	return out
}

// WriteJSON is a small helper for module handlers.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes {"error": msg}.
func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]string{"error": msg})
}
