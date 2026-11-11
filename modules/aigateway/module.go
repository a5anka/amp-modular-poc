// Package aigateway is the AI Gateway module (control-plane side): LLM
// providers, agent-to-provider bindings, and the llm.proxy/v1 capability.
//
// It shows a module extending a core resource end to end: it adds a field to
// the core "create agent" form, validates that field in a validate hook,
// stores it when the core publishes agent.created, renders a section on the
// core agent page, and cleans up in a finalize hook when the agent is deleted.
package aigateway

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/a5anka/amp-modular-poc/modkit"
	"github.com/a5anka/amp-modular-poc/modkit/contracts/llmv1"
)

//go:embed migrations/*.sql
var migrations embed.FS

//go:embed ui
var ui embed.FS

func init() { modkit.Register(&Module{}) }

// Module implements modkit.Module and modkit.Installer.
type Module struct{}

// OnInstall implements modkit.Installer. It (re)creates the default providers.
// Migrations create structure; reference data belongs here, so it is restored
// when the module is installed again after a purge (which empties the tables
// but does not re-run migrations). 0001_init.sql still seeds them on a fresh
// database: applied migrations are never edited.
func (*Module) OnInstall(ctx context.Context, rt modkit.Runtime) error {
	_, err := rt.DB().Exec(ctx, `
		INSERT INTO llm_providers (name, kind, base_url, models) VALUES
		    ('openai',    'openai',    'https://api.openai.com/v1', ARRAY['gpt-4.1', 'gpt-4.1-mini']),
		    ('anthropic', 'anthropic', 'https://api.anthropic.com', ARRAY['claude-sonnet-4-5', 'claude-haiku-4-5'])
		ON CONFLICT (name) DO NOTHING`)
	return err
}

// Manifest implements modkit.Module.
func (*Module) Manifest() modkit.Manifest {
	return modkit.Manifest{
		Name:        "ai-gateway",
		Version:     "0.1.0",
		Description: "LLM providers and agent bindings routed through the AI Gateway.",
		CoreAPI:     modkit.CoreAPIVersion,
		Provides:    []string{llmv1.Capability},
		Permissions: []modkit.Permission{
			{Name: "providers.read", Description: "View LLM providers and bindings"},
			{Name: "providers.write", Description: "Add and remove LLM providers"},
			{Name: "bindings.write", Description: "Bind agents to LLM providers"},
		},
		RoleGrants: map[string][]string{
			"admin":     {"providers.read", "providers.write", "bindings.write"},
			"developer": {"providers.read", "bindings.write"},
			"viewer":    {"providers.read"},
		},
		Events: []string{"llm-provider.created"},
		UI: &modkit.UI{
			Nav: []modkit.NavItem{{Label: "LLM Providers", Page: "providers", Permission: "providers.read"}},
			Slots: []modkit.SlotContribution{
				{Slot: "agent.create.fields", Component: "CreateAgentField", Permission: "bindings.write"},
				{Slot: "agent.detail.sections", Component: "AgentLLMSection", Permission: "providers.read"},
			},
		},
	}
}

// Migrations implements modkit.Module.
func (*Module) Migrations() fs.FS { sub, _ := fs.Sub(migrations, "migrations"); return sub }

// UI implements modkit.Module.
func (*Module) UI() fs.FS { sub, _ := fs.Sub(ui, "ui"); return sub }

type provider struct {
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`
	BaseURL string   `json:"baseUrl"`
	Models  []string `json:"models"`
}

type binding struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type service struct{ rt modkit.Runtime }

// Activate implements modkit.Module.
func (*Module) Activate(_ context.Context, rt modkit.Runtime) error {
	s := &service{rt: rt}
	rt.Handle("GET", "/providers", "providers.read", s.listProviders)
	rt.Handle("POST", "/providers", "providers.write", s.createProvider)
	rt.Handle("DELETE", "/providers/{name}", "providers.write", s.deleteProvider)
	rt.Handle("GET", "/bindings/{agentId}", "providers.read", s.getBinding)
	rt.Handle("PUT", "/bindings/{agentId}", "bindings.write", s.putBinding)
	rt.Handle("DELETE", "/bindings/{agentId}", "bindings.write", s.deleteBinding)
	// Called by other modules' workloads (e.g. evaluation runs) with a
	// core-issued token whose audience includes ai-gateway. Stands in for the
	// gateway runtime's LLM route, which verifies the same tokens via JWKS.
	rt.HandleInternal("POST", "/judge", "llm.invoke", s.judge)

	rt.Hook("agent", modkit.Validate, s.validateAgent)
	rt.Hook("agent", modkit.Finalize, s.finalizeAgent)
	rt.Subscribe("agent.created", s.onAgentCreated)
	rt.Provide(llmv1.Capability, s)
	return nil
}

// BindingFor implements llmv1.Proxy for other modules.
func (s *service) BindingFor(ctx context.Context, agentID string) (llmv1.Binding, bool, error) {
	var b llmv1.Binding
	err := s.rt.DB().QueryRow(ctx, `SELECT agent_id, provider, model FROM agent_bindings WHERE agent_id = $1`, agentID).
		Scan(&b.AgentID, &b.Provider, &b.Model)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, false, nil
	}
	return b, err == nil, err
}

func (s *service) providers(ctx context.Context) ([]provider, error) {
	rows, err := s.rt.DB().Query(ctx, `SELECT name, kind, base_url, models FROM llm_providers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (provider, error) {
		var p provider
		err := r.Scan(&p.Name, &p.Kind, &p.BaseURL, &p.Models)
		return p, err
	})
}

// checkBinding validates a binding against the configured providers.
func (s *service) checkBinding(ctx context.Context, b binding) error {
	ps, err := s.providers(ctx)
	if err != nil {
		return err
	}
	for _, p := range ps {
		if p.Name == b.Provider {
			if !slices.Contains(p.Models, b.Model) {
				return fmt.Errorf("provider %s does not offer model %q (offers %s)", p.Name, b.Model, strings.Join(p.Models, ", "))
			}
			return nil
		}
	}
	return fmt.Errorf("unknown LLM provider %q", b.Provider)
}

func (s *service) listProviders(w http.ResponseWriter, r *http.Request) {
	ps, err := s.providers(r.Context())
	if err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	modkit.WriteJSON(w, 200, ps)
}

func (s *service) createProvider(w http.ResponseWriter, r *http.Request) {
	var p provider
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil || p.Name == "" || p.BaseURL == "" || len(p.Models) == 0 {
		modkit.WriteError(w, 400, "name, baseUrl and at least one model are required")
		return
	}
	if p.Kind == "" {
		p.Kind = "openai-compatible"
	}
	_, err := s.rt.DB().Exec(r.Context(), `INSERT INTO llm_providers (name, kind, base_url, models) VALUES ($1, $2, $3, $4)`,
		p.Name, p.Kind, p.BaseURL, p.Models)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		modkit.WriteError(w, 409, "provider exists")
		return
	}
	if err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	_ = s.rt.Publish(r.Context(), "llm-provider.created", p)
	modkit.WriteJSON(w, 201, p)
}

func (s *service) deleteProvider(w http.ResponseWriter, r *http.Request) {
	_, err := s.rt.DB().Exec(r.Context(), `DELETE FROM llm_providers WHERE name = $1`, r.PathValue("name"))
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23503" {
		modkit.WriteError(w, 409, "agents are still bound to this provider")
		return
	}
	if err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

func (s *service) getBinding(w http.ResponseWriter, r *http.Request) {
	b, ok, err := s.BindingFor(r.Context(), r.PathValue("agentId"))
	if err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	if !ok {
		modkit.WriteError(w, 404, "agent has no LLM binding")
		return
	}
	modkit.WriteJSON(w, 200, b)
}

func (s *service) putBinding(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("agentId")
	if _, err := s.rt.Core().GetAgent(r.Context(), id); err != nil {
		modkit.WriteError(w, 404, "agent not found")
		return
	}
	var b binding
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		modkit.WriteError(w, 400, "invalid JSON")
		return
	}
	if err := s.checkBinding(r.Context(), b); err != nil {
		modkit.WriteError(w, 422, err.Error())
		return
	}
	if err := s.store(r.Context(), id, b); err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	modkit.WriteJSON(w, 200, llmv1.Binding{AgentID: id, Provider: b.Provider, Model: b.Model})
}

func (s *service) deleteBinding(w http.ResponseWriter, r *http.Request) {
	if _, err := s.rt.DB().Exec(r.Context(), `DELETE FROM agent_bindings WHERE agent_id = $1`, r.PathValue("agentId")); err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

func (s *service) store(ctx context.Context, agentID string, b binding) error {
	_, err := s.rt.DB().Exec(ctx, `
		INSERT INTO agent_bindings (agent_id, provider, model) VALUES ($1, $2, $3)
		ON CONFLICT (agent_id) DO UPDATE SET provider = EXCLUDED.provider, model = EXCLUDED.model, updated_at = now()`,
		agentID, b.Provider, b.Model)
	return err
}

// validateAgent checks the "ai-gateway" block of a create-agent request
// before the core stores the agent.
func (s *service) validateAgent(ctx context.Context, req modkit.HookRequest) error {
	if req.Action != "create" || req.Extension == nil {
		return nil
	}
	var b binding
	if err := json.Unmarshal(req.Extension, &b); err != nil {
		return fmt.Errorf("invalid ai-gateway extension: %w", err)
	}
	return s.checkBinding(ctx, b)
}

// onAgentCreated stores the binding validated above.
func (s *service) onAgentCreated(ctx context.Context, e modkit.Event) error {
	var d modkit.AgentCreated
	if err := json.Unmarshal(e.Data, &d); err != nil {
		return err
	}
	ext, ok := d.Extensions["ai-gateway"]
	if !ok {
		return nil
	}
	var b binding
	if err := json.Unmarshal(ext, &b); err != nil {
		return err
	}
	s.rt.Logger().Info("binding agent to LLM provider", "agent", d.Agent.Name, "provider", b.Provider, "model", b.Model)
	return s.store(ctx, d.Agent.ID, b)
}

// judge simulates an LLM-judge call routed through the AI Gateway. It shows
// the gateway accepting a run token minted by another module; it does not
// call a real model.
func (s *service) judge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentID string `json:"agentId"`
		Prompt  string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		modkit.WriteError(w, 400, "invalid JSON")
		return
	}
	b, ok, err := s.BindingFor(r.Context(), req.AgentID)
	if err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	if !ok {
		modkit.WriteError(w, 422, "agent has no LLM binding")
		return
	}
	c, _ := modkit.ClaimsFrom(r.Context())
	h := fnv.New32a()
	h.Write([]byte(req.Prompt))
	score := 0.6 + float64(h.Sum32()%36)/100
	s.rt.Logger().Info("judge call metered", "caller", c.Subject, "issuedBy", c.Module, "provider", b.Provider, "model", b.Model)
	modkit.WriteJSON(w, 200, map[string]any{"score": score, "provider": b.Provider, "model": b.Model})
}

// finalizeAgent removes the module's data for an agent that is being
// deleted. Module tables cannot hold foreign keys to core tables (they live in
// another schema the module cannot read), so clean-up is the module's job.
func (s *service) finalizeAgent(ctx context.Context, req modkit.HookRequest) error {
	_, err := s.rt.DB().Exec(ctx, `DELETE FROM agent_bindings WHERE agent_id = $1`, req.ID)
	return err
}
