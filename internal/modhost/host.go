// Package modhost is the module host: it keeps track of which compiled-in
// modules are installed, runs their lifecycle (install, enable, disable,
// uninstall, upgrade on boot), and owns the live registrations of the active
// ones (routes, hooks, event handlers, capabilities, permissions, UI).
package modhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/a5anka/amp-modular-poc/internal/db"
	"github.com/a5anka/amp-modular-poc/modkit"
)

// Core permissions and the roles that hold them. Modules add their own
// permissions and grant them to these roles through their manifest.
var (
	CorePermissions = []modkit.Permission{
		{Name: "agents:read", Description: "View agents"},
		{Name: "agents:write", Description: "Create and delete agents"},
		{Name: "modules:manage", Description: "Install, enable, disable and uninstall modules"},
	}
	coreGrants = map[string][]string{
		"admin":     {"agents:read", "agents:write", "modules:manage"},
		"developer": {"agents:read", "agents:write"},
		"viewer":    {"agents:read"},
	}
	// Roles known to the core, in display order.
	Roles = []string{"admin", "developer", "viewer"}
)

const notifyChannel = "amp_modules"

// Host is the module host. One per process.
type Host struct {
	pool *pgxpool.Pool
	dsn  string
	log  *slog.Logger
	core modkit.CoreAPI

	mods      map[string]modkit.Module
	manifests map[string]modkit.Manifest

	poolMu sync.Mutex
	pools  map[string]*pgxpool.Pool

	mu     sync.RWMutex
	gen    int64
	states map[string]string
	dbVer  map[string]string
	v      *view

	tagMu sync.Mutex
	tags  map[string]string

	keys *keyring

	devMu   sync.Mutex
	devURL  string
	devRuns map[string]context.CancelFunc

	Bus *Broadcaster
}

// view is an immutable snapshot of what the active modules registered.
// Reload builds a new one and swaps it in, so requests never see a half-built set.
type view struct {
	mux    *http.ServeMux
	routes map[string]int // module -> route count
	hooks  map[string][]hookEntry
	subs   map[string][]subEntry
	caps   map[string]any
	capBy  map[string]string
	perms  map[string]string // full permission -> owning module ("" = core)
	grants map[string]map[string]bool
	active []string
}

type hookEntry struct {
	module string
	fn     modkit.HookFunc
}

type subEntry struct {
	module string
	fn     modkit.EventHandler
}

func newView() *view {
	v := &view{
		mux: http.NewServeMux(), routes: map[string]int{}, hooks: map[string][]hookEntry{},
		subs: map[string][]subEntry{}, caps: map[string]any{}, capBy: map[string]string{},
		perms: map[string]string{}, grants: map[string]map[string]bool{},
	}
	for _, p := range CorePermissions {
		v.perms[p.Name] = ""
	}
	for role, ps := range coreGrants {
		v.grants[role] = map[string]bool{}
		for _, p := range ps {
			v.grants[role][p] = true
		}
	}
	return v
}

// New creates the host for the modules compiled into this binary.
func New(pool *pgxpool.Pool, dsn string, core modkit.CoreAPI, log *slog.Logger) *Host {
	h := &Host{
		pool: pool, dsn: dsn, log: log, core: core,
		mods: map[string]modkit.Module{}, manifests: map[string]modkit.Manifest{},
		pools: map[string]*pgxpool.Pool{}, states: map[string]string{}, dbVer: map[string]string{},
		v: newView(), Bus: NewBroadcaster(), tags: map[string]string{}, devRuns: map[string]context.CancelFunc{},
	}
	for _, m := range modkit.Registered() {
		mf := m.Manifest()
		h.mods[mf.Name] = m
		h.manifests[mf.Name] = mf
	}
	return h
}

func (h *Host) current() *view {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.v
}

// Generation is the lifecycle generation this replica has loaded.
func (h *Host) Generation() int64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.gen
}

func (h *Host) modulePool(ctx context.Context, name string) (*pgxpool.Pool, error) {
	h.poolMu.Lock()
	defer h.poolMu.Unlock()
	if p, ok := h.pools[name]; ok {
		return p, nil
	}
	p, err := db.ModulePool(ctx, h.dsn, name)
	if err != nil {
		return nil, err
	}
	h.pools[name] = p
	return p, nil
}

func (h *Host) closeModulePool(name string) {
	h.poolMu.Lock()
	defer h.poolMu.Unlock()
	if p, ok := h.pools[name]; ok {
		p.Close()
		delete(h.pools, name)
	}
}

type rowState struct{ state, version string }

func loadStates(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) (map[string]rowState, error) {
	rows, err := q.Query(ctx, `SELECT name, state, version FROM core.modules`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]rowState{}
	for rows.Next() {
		var n, s, v string
		if err := rows.Scan(&n, &s, &v); err != nil {
			return nil, err
		}
		out[n] = rowState{s, v}
	}
	return out, rows.Err()
}

func flatten(rs map[string]rowState) map[string]string {
	out := map[string]string{}
	for n, r := range rs {
		out[n] = r.state
	}
	return out
}

// Boot upgrades installed modules whose compiled version carries new
// migrations, loads the active set, and starts following lifecycle changes
// made by other replicas.
func (h *Host) Boot(ctx context.Context) error {
	if err := h.loadKeys(ctx); err != nil {
		return fmt.Errorf("signing keys: %w", err)
	}
	if err := h.upgradeInstalled(ctx); err != nil {
		return err
	}
	if err := h.Reload(ctx); err != nil {
		return err
	}
	go h.follow(ctx)
	return nil
}

func (h *Host) upgradeInstalled(ctx context.Context) error {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := db.Lock(ctx, tx); err != nil {
		return err
	}
	rs, err := loadStates(ctx, tx)
	if err != nil {
		return err
	}
	changed := false
	for name, r := range rs {
		m, ok := h.mods[name]
		if !ok || r.state == StateUninstalled {
			continue
		}
		n, err := h.migrate(ctx, tx, name, m)
		if err != nil {
			return err
		}
		mf := h.manifests[name]
		if n > 0 || r.version != mf.Version {
			h.log.Info("module upgraded", "module", name, "from", r.version, "to", mf.Version, "migrations", n)
			if _, err := tx.Exec(ctx, `UPDATE core.modules SET version = $2, updated_at = now() WHERE name = $1`, name, mf.Version); err != nil {
				return err
			}
			changed = true
		}
	}
	if changed {
		if _, err := bump(ctx, tx); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// migrate makes sure the module schema exists and applies pending migrations.
func (h *Host) migrate(ctx context.Context, tx pgx.Tx, name string, m modkit.Module) (int, error) {
	if err := db.EnsureModuleSchema(ctx, tx, name); err != nil {
		return 0, err
	}
	ms, err := db.Load(m.Migrations())
	if err != nil {
		return 0, err
	}
	pending, err := db.Pending(ctx, tx, name, ms)
	if err != nil {
		return 0, err
	}
	if err := db.Apply(ctx, tx, name, pending, db.RoleName(name)); err != nil {
		return 0, err
	}
	return len(pending), nil
}

func bump(ctx context.Context, tx pgx.Tx) (int64, error) {
	var g int64
	if err := tx.QueryRow(ctx, `UPDATE core.generation SET value = value + 1 RETURNING value`).Scan(&g); err != nil {
		return 0, err
	}
	// Delivered to every replica when the transaction commits.
	_, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, notifyChannel, strconv.FormatInt(g, 10))
	return g, err
}

// follow listens for lifecycle changes committed by any replica.
func (h *Host) follow(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := h.listenOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		h.log.Warn("module change listener stopped, retrying", "err", err, "in", backoff)
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (h *Host) listenOnce(ctx context.Context) error {
	conn, err := pgx.Connect(ctx, h.dsn)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
		return err
	}
	// Catch anything committed while we were not listening.
	if err := h.Reload(ctx); err != nil {
		h.log.Error("reload failed", "err", err)
	}
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		g, _ := strconv.ParseInt(n.Payload, 10, 64)
		if g <= h.Generation() {
			continue
		}
		h.log.Info("module set changed on another replica", "generation", g)
		if err := h.Reload(ctx); err != nil {
			h.log.Error("reload failed", "err", err)
		}
	}
}

// Reload rebuilds the registrations of the active modules and swaps them in.
func (h *Host) Reload(ctx context.Context) error {
	rs, err := loadStates(ctx, h.pool)
	if err != nil {
		return err
	}
	var gen int64
	if err := h.pool.QueryRow(ctx, `SELECT value FROM core.generation`).Scan(&gen); err != nil {
		return err
	}
	states := flatten(rs)
	var active []string
	for name, s := range states {
		if s != StateActive {
			continue
		}
		if _, ok := h.mods[name]; !ok {
			h.log.Warn("module is active in the database but not compiled into this binary; skipping", "module", name)
			continue
		}
		active = append(active, name)
	}
	v := newView()
	for _, name := range ActivationOrder(active, h.manifests) {
		rt, err := h.runtime(ctx, name, v)
		if err != nil {
			return err
		}
		mf := h.manifests[name]
		for _, p := range mf.Permissions {
			v.perms[name+":"+p.Name] = name
		}
		for role, ps := range mf.RoleGrants {
			if v.grants[role] == nil {
				v.grants[role] = map[string]bool{}
			}
			for _, p := range ps {
				v.grants[role][qualify(name, p)] = true
			}
		}
		if err := h.mods[name].Activate(ctx, rt); err != nil {
			h.log.Error("module failed to activate", "module", name, "err", err)
			continue
		}
		v.active = append(v.active, name)
	}
	h.mu.Lock()
	changed := gen != h.gen || len(h.v.active) != len(v.active)
	h.v, h.gen, h.states = v, gen, states
	h.dbVer = map[string]string{}
	for n, r := range rs {
		h.dbVer[n] = r.version
	}
	h.mu.Unlock()
	h.log.Info("modules loaded", "generation", gen, "active", v.active)
	h.syncDevWorkloads(v.active)
	if changed {
		h.Bus.Send("modules.changed", map[string]any{"generation": gen, "active": v.active})
	}
	return nil
}

func qualify(module, perm string) string {
	if strings.Contains(perm, ":") {
		return perm
	}
	return module + ":" + perm
}

// ---- lifecycle ----

// change runs fn under the cluster-wide lifecycle lock, bumps the generation,
// commits, and reloads this replica. Other replicas reload on the notification.
func (h *Host) change(ctx context.Context, fn func(tx pgx.Tx, states map[string]string) error) error {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := db.Lock(ctx, tx); err != nil {
		return err
	}
	rs, err := loadStates(ctx, tx)
	if err != nil {
		return err
	}
	if err := fn(tx, flatten(rs)); err != nil {
		return err
	}
	if _, err := bump(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return h.Reload(ctx)
}

// Install installs a module (and, with withDeps, the providers of its missing
// required capabilities). It returns the steps it carried out.
func (h *Host) Install(ctx context.Context, name string, withDeps bool) ([]Step, error) {
	var steps []Step
	err := h.change(ctx, func(tx pgx.Tx, states map[string]string) error {
		if states[name] == StateActive {
			return &ResolveError{Msg: name + " is already installed and active"}
		}
		var err error
		if steps, err = Plan(name, h.manifests, states, withDeps); err != nil {
			return err
		}
		for _, s := range steps {
			if _, err := h.migrate(ctx, tx, s.Module, h.mods[s.Module]); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO core.modules (name, version, state) VALUES ($1, $2, 'active')
				ON CONFLICT (name) DO UPDATE SET version = EXCLUDED.version, state = 'active', updated_at = now()`,
				s.Module, h.manifests[s.Module].Version); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Catch-up: a module installed late rebuilds its view of existing core state.
	for _, s := range steps {
		if inst, ok := h.mods[s.Module].(modkit.Installer); ok && s.Action == "install" {
			rt, err := h.runtime(ctx, s.Module, h.current())
			if err == nil {
				err = inst.OnInstall(ctx, rt)
			}
			if err != nil {
				h.log.Error("module catch-up failed", "module", s.Module, "err", err)
			}
		}
	}
	return steps, nil
}

// Enable re-activates a disabled module, applying any migrations added since.
func (h *Host) Enable(ctx context.Context, name string) error {
	return h.change(ctx, func(tx pgx.Tx, states map[string]string) error {
		if states[name] != StateDisabled {
			return &ResolveError{Msg: fmt.Sprintf("%s is %s, not disabled", name, stateOf(name, states))}
		}
		if _, err := Plan(name, h.manifests, states, false); err != nil {
			return err
		}
		if _, err := h.migrate(ctx, tx, name, h.mods[name]); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE core.modules SET state = 'active', version = $2, updated_at = now() WHERE name = $1`,
			name, h.manifests[name].Version)
		return err
	})
}

// Disable deactivates a module and keeps its data.
func (h *Host) Disable(ctx context.Context, name string) error {
	return h.change(ctx, func(tx pgx.Tx, states map[string]string) error {
		if states[name] != StateActive {
			return &ResolveError{Msg: fmt.Sprintf("%s is %s, not active", name, stateOf(name, states))}
		}
		if deps := Dependents(name, h.manifests, states, false); len(deps) > 0 {
			return &ResolveError{Msg: fmt.Sprintf("%s is required by %s", name, strings.Join(deps, ", ")), Hint: "disable those first"}
		}
		_, err := tx.Exec(ctx, `UPDATE core.modules SET state = 'disabled', updated_at = now() WHERE name = $1`, name)
		return err
	})
}

// Uninstall removes a module. Without purge its schema and data stay, so a
// later install picks up where it left off; with purge they are dropped.
func (h *Host) Uninstall(ctx context.Context, name string, purge bool) error {
	err := h.change(ctx, func(tx pgx.Tx, states map[string]string) error {
		st := stateOf(name, states)
		if st == StateAvailable || (st == StateUninstalled && !purge) {
			return &ResolveError{Msg: name + " is not installed"}
		}
		if deps := Dependents(name, h.manifests, states, true); len(deps) > 0 {
			return &ResolveError{Msg: fmt.Sprintf("%s is required by %s", name, strings.Join(deps, ", ")), Hint: "uninstall those first"}
		}
		if !purge {
			_, err := tx.Exec(ctx, `UPDATE core.modules SET state = 'uninstalled', updated_at = now() WHERE name = $1`, name)
			return err
		}
		if err := db.DropModuleSchema(ctx, tx, name); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM core.modules WHERE name = $1`, name)
		return err
	})
	if err == nil && purge {
		h.closeModulePool(name)
	}
	return err
}

// ---- runtime used by modules ----

type runtime struct {
	h    *Host
	name string
	v    *view
	pool *pgxpool.Pool
	log  *slog.Logger
}

func (h *Host) runtime(ctx context.Context, name string, v *view) (*runtime, error) {
	p, err := h.modulePool(ctx, name)
	if err != nil {
		return nil, err
	}
	return &runtime{h: h, name: name, v: v, pool: p, log: h.log.With("module", name)}, nil
}

func (r *runtime) Name() string         { return r.name }
func (r *runtime) DB() *pgxpool.Pool    { return r.pool }
func (r *runtime) Core() modkit.CoreAPI { return r.h.core }
func (r *runtime) Logger() *slog.Logger { return r.log }
func (r *runtime) Capability(c string) (any, bool) {
	impl, ok := r.h.current().caps[c]
	return impl, ok
}

func (r *runtime) Handle(method, path, perm string, fn http.HandlerFunc) {
	full := ""
	if perm != "" {
		full = qualify(r.name, perm)
	}
	pattern := fmt.Sprintf("%s /m/%s%s", method, r.name, path)
	r.v.routes[r.name]++
	r.v.mux.HandleFunc(pattern, func(w http.ResponseWriter, req *http.Request) {
		if full != "" && !r.h.Can(modkit.PrincipalFrom(req.Context()).Role, full) {
			modkit.WriteError(w, http.StatusForbidden, "missing permission "+full)
			return
		}
		fn(w, req)
	})
}

func (r *runtime) HandleInternal(method, path, scope string, fn http.HandlerFunc) {
	full := qualify(r.name, scope)
	pattern := fmt.Sprintf("%s /m/%s/internal%s", method, r.name, path)
	r.v.routes[r.name]++
	r.v.mux.HandleFunc(pattern, func(w http.ResponseWriter, req *http.Request) {
		tok, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
		if !ok {
			modkit.WriteError(w, http.StatusUnauthorized, "internal route: bearer token required")
			return
		}
		c, err := r.h.VerifyToken(tok, r.name)
		if err != nil {
			modkit.WriteError(w, http.StatusUnauthorized, err.Error())
			return
		}
		if !c.HasScope(full) {
			modkit.WriteError(w, http.StatusForbidden, "token lacks scope "+full)
			return
		}
		fn(w, req.WithContext(modkit.WithClaims(req.Context(), c)))
	})
}

func (r *runtime) Can(ctx context.Context, perm string) bool {
	return r.h.Can(modkit.PrincipalFrom(ctx).Role, qualify(r.name, perm))
}

func (r *runtime) IssueToken(_ context.Context, t modkit.TokenRequest) (string, error) {
	return r.h.issue(r.name, "", t.Subject, t.Scopes, t.TTL, t.Claims)
}

func (r *runtime) Hook(resource string, phase modkit.Phase, fn modkit.HookFunc) {
	k := resource + "/" + string(phase)
	r.v.hooks[k] = append(r.v.hooks[k], hookEntry{r.name, fn})
}

func (r *runtime) Subscribe(eventType string, fn modkit.EventHandler) {
	r.v.subs[eventType] = append(r.v.subs[eventType], subEntry{r.name, fn})
}

func (r *runtime) Publish(ctx context.Context, eventType string, data any) error {
	return r.h.Publish(ctx, r.name, eventType, data)
}

func (r *runtime) Provide(capability string, impl any) {
	r.v.caps[capability] = impl
	r.v.capBy[capability] = r.name
}

// ---- used by the core ----

// Can reports whether role holds perm under the current module set.
func (h *Host) Can(role, perm string) bool {
	return h.current().grants[role][perm]
}

// Permissions lists the permissions a role holds.
func (h *Host) Permissions(role string) []string {
	var out []string
	for p, ok := range h.current().grants[role] {
		if ok {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// HookError is returned when a module hook rejects a request.
type HookError struct {
	Module string `json:"module"`
	Msg    string `json:"error"`
}

func (e *HookError) Error() string { return e.Module + ": " + e.Msg }

// RunHooks runs the hooks registered for resource/phase. extensions are the
// request's per-module extension blocks; one addressed to a module that is
// not active is rejected, so clients cannot silently lose data.
func (h *Host) RunHooks(ctx context.Context, resource string, phase modkit.Phase, req modkit.HookRequest, extensions map[string]json.RawMessage) error {
	v := h.current()
	for mod := range extensions {
		if !contains(v.active, mod) {
			return &HookError{Module: mod, Msg: "request carries an extension for a module that is not active"}
		}
	}
	for _, e := range v.hooks[resource+"/"+string(phase)] {
		r := req
		r.Extension = extensions[e.module]
		if err := e.fn(ctx, r); err != nil {
			return &HookError{Module: e.module, Msg: err.Error()}
		}
	}
	return nil
}

// Publish delivers an event to subscribers asynchronously. In this PoC the
// delivery is in-process on the replica that published; production would put
// it on the existing outbox + NATS bus so every replica and retries are covered.
func (h *Host) Publish(ctx context.Context, source, eventType string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	e := modkit.Event{Type: eventType, Source: source, Time: time.Now().UTC(), Data: b}
	v := h.current()
	var to []string
	for _, s := range append(append([]subEntry(nil), v.subs[eventType]...), v.subs["*"]...) {
		to = append(to, s.module)
		go func(s subEntry) {
			c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := s.fn(c, e); err != nil {
				h.log.Error("event handler failed", "module", s.module, "event", eventType, "err", err)
			}
		}(s)
	}
	h.Bus.Send("event", map[string]any{"type": eventType, "source": source, "deliveredTo": to})
	return nil
}

// ServeModule dispatches /m/<module>/... to the active module's routes.
func (h *Host) ServeModule(w http.ResponseWriter, r *http.Request) {
	name := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/m/"), "/", 2)[0]
	v := h.current()
	if !contains(v.active, name) {
		modkit.WriteError(w, http.StatusNotFound, fmt.Sprintf("module %s is not active", name))
		return
	}
	v.mux.ServeHTTP(w, r)
}

// ModuleInfo is the API view of a module.
type ModuleInfo struct {
	modkit.Manifest
	State            string   `json:"state"`
	InstalledVersion string   `json:"installedVersion,omitempty"`
	RequiredBy       []string `json:"requiredBy,omitempty"`
	Routes           int      `json:"routes"`
	Migrations       []string `json:"migrations"`
	Schema           string   `json:"schema"`
	Health           *Health  `json:"health,omitempty"`
}

// List describes every module compiled into the binary.
func (h *Host) List() []ModuleInfo {
	h.mu.RLock()
	states, dbVer, v := h.states, h.dbVer, h.v
	h.mu.RUnlock()
	var out []ModuleInfo
	for _, m := range modkit.Registered() {
		mf := m.Manifest()
		ms, _ := db.Load(m.Migrations())
		var files []string
		for _, x := range ms {
			files = append(files, x.File)
		}
		out = append(out, ModuleInfo{
			Manifest: mf, State: stateOf(mf.Name, states), InstalledVersion: dbVer[mf.Name],
			RequiredBy: Dependents(mf.Name, h.manifests, states, true), Routes: v.routes[mf.Name],
			Migrations: files, Schema: db.SchemaName(mf.Name),
		})
	}
	return out
}

// Get returns one module.
func (h *Host) Get(name string) (ModuleInfo, bool) {
	for _, m := range h.List() {
		if m.Name == name {
			return m, true
		}
	}
	return ModuleInfo{}, false
}

// WithHealth adds live health to module infos.
func (h *Host) WithHealth(ctx context.Context, ms []ModuleInfo) []ModuleInfo {
	for i := range ms {
		if ms[i].State == StateActive {
			hl := h.ModuleHealth(ctx, ms[i].Name)
			ms[i].Health = &hl
		}
	}
	return ms
}

// EnableDevWorkloads makes the host run the in-process dev workloads of
// active modules (`amp serve --dev-workers`). They talk to coreURL over HTTP.
func (h *Host) EnableDevWorkloads(coreURL string) {
	h.devMu.Lock()
	h.devURL = coreURL
	h.devMu.Unlock()
	h.syncDevWorkloads(h.current().active)
}

func (h *Host) syncDevWorkloads(active []string) {
	h.devMu.Lock()
	defer h.devMu.Unlock()
	if h.devURL == "" {
		return
	}
	for name, cancel := range h.devRuns {
		if !contains(active, name) {
			cancel()
			delete(h.devRuns, name)
			h.log.Info("dev workloads stopped", "module", name)
		}
	}
	for _, name := range active {
		dw, ok := h.mods[name].(modkit.DevWorkloads)
		if !ok || h.devRuns[name] != nil {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		h.devRuns[name] = cancel
		env := modkit.DevEnv{CoreURL: h.devURL, Logger: h.log.With("module", name, "dev-workload", true),
			Credential: func(ctx context.Context, wl string) (string, error) { return h.CreateCredential(ctx, name, wl) }}
		go func() {
			if err := dw.RunDevWorkloads(ctx, env); err != nil && ctx.Err() == nil {
				h.log.Error("dev workloads stopped", "module", name, "err", err)
			}
		}()
		h.log.Info("dev workloads started in-process (no isolation: dev only)", "module", name)
	}
}

// IsResolveError reports whether err is a user-facing lifecycle error.
func IsResolveError(err error) bool {
	var re *ResolveError
	return errors.As(err, &re)
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
