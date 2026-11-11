// Package core is the always-installed part of Agent Manager: the HTTP API,
// the agent registry, the console shell, and the wiring to the module host.
package core

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/a5anka/amp-modular-poc/internal/modhost"
	"github.com/a5anka/amp-modular-poc/modkit"
)

// Agents implements the agent registry and the modkit.CoreAPI read API.
type Agents struct{ pool *pgxpool.Pool }

// NewAgents creates the agent registry.
func NewAgents(pool *pgxpool.Pool) *Agents { return &Agents{pool: pool} }

// ListAgents implements modkit.CoreAPI.
func (a *Agents) ListAgents(ctx context.Context) ([]modkit.Agent, error) {
	rows, err := a.pool.Query(ctx, `SELECT id::text, name, description, created_at FROM core.agents ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanAgent)
}

// GetAgent implements modkit.CoreAPI.
func (a *Agents) GetAgent(ctx context.Context, id string) (modkit.Agent, error) {
	rows, err := a.pool.Query(ctx, `SELECT id::text, name, description, created_at FROM core.agents WHERE id::text = $1`, id)
	if err != nil {
		return modkit.Agent{}, err
	}
	return pgx.CollectExactlyOneRow(rows, scanAgent)
}

func scanAgent(r pgx.CollectableRow) (modkit.Agent, error) {
	var x modkit.Agent
	err := r.Scan(&x.ID, &x.Name, &x.Description, &x.CreatedAt)
	return x, err
}

// Server is the core HTTP server.
type Server struct {
	host   *modhost.Host
	agents *Agents
	web    fs.FS
	log    *slog.Logger
	name   string
}

// NewServer wires the core API, the module host and the console shell.
func NewServer(host *modhost.Host, agents *Agents, web fs.FS, log *slog.Logger, replica string) *Server {
	return &Server{host: host, agents: agents, web: web, log: log, name: replica}
}

// Handler returns the root handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /api/me", s.me)
	mux.HandleFunc("GET /api/ui-manifest", s.uiManifest)
	mux.Handle("GET /api/stream", s.host.Bus)

	mux.HandleFunc("GET /api/modules", s.need("", s.listModules))
	mux.HandleFunc("GET /api/modules/{name}", s.need("", s.getModule))
	mux.HandleFunc("POST /api/modules/{name}/install", s.need("modules:manage", s.installModule))
	mux.HandleFunc("POST /api/modules/{name}/enable", s.need("modules:manage", s.enableModule))
	mux.HandleFunc("POST /api/modules/{name}/disable", s.need("modules:manage", s.disableModule))
	mux.HandleFunc("DELETE /api/modules/{name}", s.need("modules:manage", s.uninstallModule))
	mux.HandleFunc("GET /api/modules/{name}/health", s.need("", s.moduleHealth))
	mux.HandleFunc("POST /api/modules/{name}/workloads/{workload}/credentials", s.need("modules:manage", s.createCredential))

	// Workload plane: authenticated by workload credentials and core-issued tokens, not user sessions.
	mux.HandleFunc("POST /api/workloads/heartbeat", s.heartbeat)
	mux.HandleFunc("GET /.well-known/jwks.json", s.host.ServeJWKS)

	mux.HandleFunc("GET /api/agents", s.need("agents:read", s.listAgents))
	mux.HandleFunc("POST /api/agents", s.need("agents:write", s.createAgent))
	mux.HandleFunc("GET /api/agents/{id}", s.need("agents:read", s.getAgent))
	mux.HandleFunc("DELETE /api/agents/{id}", s.need("agents:write", s.deleteAgent))

	mux.HandleFunc("/m/", s.host.ServeModule)
	mux.HandleFunc("GET /modules/", s.host.ServeUI)
	mux.Handle("/", http.FileServerFS(s.web))
	return s.authn(mux)
}

// authn is a stand-in for real authentication: the caller picks a role with
// the X-AMP-Role header (the console sends it from the role switcher).
func (s *Server) authn(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		role := r.Header.Get("X-AMP-Role")
		if role == "" {
			role = r.URL.Query().Get("role")
		}
		if !slices.Contains(modhost.Roles, role) {
			role = "admin"
		}
		w.Header().Set("X-AMP-Replica", s.name)
		next.ServeHTTP(w, r.WithContext(modkit.WithPrincipal(r.Context(), modkit.Principal{User: "demo", Role: role})))
	})
}

func (s *Server) need(perm string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if perm != "" && !s.host.Can(modkit.PrincipalFrom(r.Context()).Role, perm) {
			modkit.WriteError(w, http.StatusForbidden, "missing permission "+perm)
			return
		}
		h(w, r)
	}
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	modkit.WriteJSON(w, 200, map[string]any{"replica": s.name, "generation": s.host.Generation()})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	p := modkit.PrincipalFrom(r.Context())
	modkit.WriteJSON(w, 200, map[string]any{
		"user": p.User, "role": p.Role, "roles": modhost.Roles,
		"permissions": s.host.Permissions(p.Role), "replica": s.name,
	})
}

func (s *Server) uiManifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	modkit.WriteJSON(w, 200, s.host.UIManifest(modkit.PrincipalFrom(r.Context()).Role))
}

// ---- modules ----

func (s *Server) listModules(w http.ResponseWriter, r *http.Request) {
	modkit.WriteJSON(w, 200, s.host.WithHealth(r.Context(), s.host.List()))
}

func (s *Server) moduleHealth(w http.ResponseWriter, r *http.Request) {
	modkit.WriteJSON(w, 200, s.host.ModuleHealth(r.Context(), r.PathValue("name")))
}

func (s *Server) createCredential(w http.ResponseWriter, r *http.Request) {
	secret, err := s.host.CreateCredential(r.Context(), r.PathValue("name"), r.PathValue("workload"))
	if err != nil {
		s.lifecycleError(w, err)
		return
	}
	modkit.WriteJSON(w, 201, map[string]string{"module": r.PathValue("name"), "workload": r.PathValue("workload"), "credential": secret})
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	var req modhost.HeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Credential == "" {
		modkit.WriteError(w, 400, "credential is required")
		return
	}
	res, err := s.host.Heartbeat(r.Context(), req)
	var re *modhost.ResolveError
	if errors.As(err, &re) {
		modkit.WriteJSON(w, http.StatusUnauthorized, re)
		return
	}
	if err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	modkit.WriteJSON(w, 200, res)
}

func (s *Server) getModule(w http.ResponseWriter, r *http.Request) {
	m, ok := s.host.Get(r.PathValue("name"))
	if ok && m.State == modhost.StateActive {
		hl := s.host.ModuleHealth(r.Context(), m.Name)
		m.Health = &hl
	}
	if !ok {
		modkit.WriteError(w, 404, "module "+r.PathValue("name")+" is not compiled into this distribution")
		return
	}
	modkit.WriteJSON(w, 200, m)
}

func (s *Server) installModule(w http.ResponseWriter, r *http.Request) {
	withDeps := r.URL.Query().Get("withDeps") == "true"
	steps, err := s.host.Install(r.Context(), r.PathValue("name"), withDeps)
	if err != nil {
		s.lifecycleError(w, err)
		return
	}
	modkit.WriteJSON(w, 200, map[string]any{"steps": steps, "generation": s.host.Generation()})
}

func (s *Server) enableModule(w http.ResponseWriter, r *http.Request) {
	if err := s.host.Enable(r.Context(), r.PathValue("name")); err != nil {
		s.lifecycleError(w, err)
		return
	}
	modkit.WriteJSON(w, 200, map[string]any{"generation": s.host.Generation()})
}

func (s *Server) disableModule(w http.ResponseWriter, r *http.Request) {
	if err := s.host.Disable(r.Context(), r.PathValue("name")); err != nil {
		s.lifecycleError(w, err)
		return
	}
	modkit.WriteJSON(w, 200, map[string]any{"generation": s.host.Generation()})
}

func (s *Server) uninstallModule(w http.ResponseWriter, r *http.Request) {
	purge := r.URL.Query().Get("purge") == "true"
	if err := s.host.Uninstall(r.Context(), r.PathValue("name"), purge); err != nil {
		s.lifecycleError(w, err)
		return
	}
	modkit.WriteJSON(w, 200, map[string]any{"generation": s.host.Generation(), "purged": purge})
}

func (s *Server) lifecycleError(w http.ResponseWriter, err error) {
	var re *modhost.ResolveError
	if errors.As(err, &re) {
		modkit.WriteJSON(w, http.StatusConflict, re)
		return
	}
	s.log.Error("module lifecycle failed", "err", err)
	modkit.WriteError(w, 500, err.Error())
}

// ---- agents ----

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	xs, err := s.agents.ListAgents(r.Context())
	if err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	if xs == nil {
		xs = []modkit.Agent{}
	}
	modkit.WriteJSON(w, 200, xs)
}

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request) {
	a, err := s.agents.GetAgent(r.Context(), r.PathValue("id"))
	if errors.Is(err, pgx.ErrNoRows) {
		modkit.WriteError(w, 404, "agent not found")
		return
	}
	if err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	modkit.WriteJSON(w, 200, a)
}

type createAgentReq struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Extensions carries per-module fields, e.g. {"ai-gateway": {"provider": "openai"}}.
	// The core does not store them: each module validates its own block in a
	// validate hook and persists it when it handles agent.created.
	Extensions map[string]json.RawMessage `json:"extensions,omitempty"`
}

func (s *Server) createAgent(w http.ResponseWriter, r *http.Request) {
	var req createAgentReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		modkit.WriteError(w, 400, "invalid JSON")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		modkit.WriteError(w, 400, "name is required")
		return
	}
	obj := map[string]any{"name": req.Name, "description": req.Description}
	if err := s.host.RunHooks(r.Context(), "agent", modkit.Validate,
		modkit.HookRequest{Resource: "agent", Action: "create", Object: obj}, req.Extensions); err != nil {
		s.hookError(w, err)
		return
	}
	var a modkit.Agent
	err := s.agents.pool.QueryRow(r.Context(),
		`INSERT INTO core.agents (name, description) VALUES ($1, $2) RETURNING id::text, name, description, created_at`,
		req.Name, req.Description).Scan(&a.ID, &a.Name, &a.Description, &a.CreatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		modkit.WriteError(w, 409, "an agent with that name exists")
		return
	}
	if err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	_ = s.host.Publish(r.Context(), "core", "agent.created", modkit.AgentCreated{Agent: a, Extensions: req.Extensions})
	modkit.WriteJSON(w, 201, a)
}

func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request) {
	a, err := s.agents.GetAgent(r.Context(), r.PathValue("id"))
	if errors.Is(err, pgx.ErrNoRows) {
		modkit.WriteError(w, 404, "agent not found")
		return
	}
	if err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	// Finalize hooks: each active module that attached data to the agent
	// cleans it up, or blocks the delete.
	if err := s.host.RunHooks(r.Context(), "agent", modkit.Finalize,
		modkit.HookRequest{Resource: "agent", Action: "delete", ID: a.ID,
			Object: map[string]any{"name": a.Name}}, nil); err != nil {
		s.hookError(w, err)
		return
	}
	if _, err := s.agents.pool.Exec(r.Context(), `DELETE FROM core.agents WHERE id::text = $1`, a.ID); err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	_ = s.host.Publish(r.Context(), "core", "agent.deleted", map[string]any{"agent": a, "deletedAt": time.Now().UTC()})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) hookError(w http.ResponseWriter, err error) {
	var he *modhost.HookError
	if errors.As(err, &he) {
		modkit.WriteJSON(w, http.StatusUnprocessableEntity, he)
		return
	}
	modkit.WriteError(w, 500, err.Error())
}
