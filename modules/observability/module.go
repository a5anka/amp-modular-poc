// Package observability is the Observability module: it stores agent traces
// and provides the traces.query/v1 capability other modules build on.
//
// It shows catch-up on install (OnInstall backfills traces for agents that
// existed before the module was installed), an extension point of its own
// (the "observability.trace.columns" slot, filled by evaluation), and
// cleanup through a finalize hook.
package observability

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io/fs"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/a5anka/amp-modular-poc/modkit"
	"github.com/a5anka/amp-modular-poc/modkit/contracts/tracesv1"
)

//go:embed migrations/*.sql
var migrations embed.FS

//go:embed ui
var ui embed.FS

func init() { modkit.Register(&Module{}) }

// Module implements modkit.Module and modkit.Installer.
type Module struct{}

// Manifest implements modkit.Module.
func (*Module) Manifest() modkit.Manifest {
	return modkit.Manifest{
		Name:        "observability",
		Version:     "0.1.0",
		Description: "Agent traces, and the traces.query capability for other modules.",
		CoreAPI:     modkit.CoreAPIVersion,
		Provides:    []string{tracesv1.Capability},
		Permissions: []modkit.Permission{
			{Name: "traces.read", Description: "View traces"},
			{Name: "traces.write", Description: "Ingest traces"},
		},
		RoleGrants: map[string][]string{
			"admin":     {"traces.read", "traces.write"},
			"developer": {"traces.read", "traces.write"},
			"viewer":    {"traces.read"},
		},
		Events: []string{"trace.recorded"},
		UI: &modkit.UI{
			Nav:      []modkit.NavItem{{Label: "Traces", Page: "traces", Permission: "traces.read"}},
			Slots:    []modkit.SlotContribution{{Slot: "agent.detail.sections", Component: "AgentTracesSection", Permission: "traces.read"}},
			OwnSlots: []string{"observability.trace.columns"},
		},
	}
}

// Migrations implements modkit.Module.
func (*Module) Migrations() fs.FS { sub, _ := fs.Sub(migrations, "migrations"); return sub }

// UI implements modkit.Module.
func (*Module) UI() fs.FS { sub, _ := fs.Sub(ui, "ui"); return sub }

type service struct{ rt modkit.Runtime }

// Activate implements modkit.Module.
func (*Module) Activate(_ context.Context, rt modkit.Runtime) error {
	s := &service{rt: rt}
	rt.Handle("GET", "/traces", "traces.read", s.list)
	rt.Handle("POST", "/traces", "traces.write", s.ingest)
	rt.Hook("agent", modkit.Finalize, s.finalizeAgent)
	rt.Subscribe("agent.created", s.onAgentCreated)
	rt.Provide(tracesv1.Capability, s)
	return nil
}

// OnInstall implements modkit.Installer: agents registered before the module
// was installed get their history too. Idempotent.
func (*Module) OnInstall(ctx context.Context, rt modkit.Runtime) error {
	s := &service{rt: rt}
	agents, err := rt.Core().ListAgents(ctx)
	if err != nil {
		return err
	}
	n := 0
	for _, a := range agents {
		var exists bool
		if err := rt.DB().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM traces WHERE agent_id = $1)`, a.ID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			if err := s.seed(ctx, a); err != nil {
				return err
			}
			n++
		}
	}
	rt.Logger().Info("catch-up complete: backfilled traces for existing agents", "agents", n)
	return nil
}

// ListTraces implements tracesv1.Query.
func (s *service) ListTraces(ctx context.Context, agentID string, limit int) ([]tracesv1.Trace, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT id, agent_id, name, status, duration_ms, input, output, created_at FROM traces`
	args := []any{limit}
	if agentID != "" {
		q += ` WHERE agent_id = $2`
		args = append(args, agentID)
	}
	rows, err := s.rt.DB().Query(ctx, q+` ORDER BY created_at DESC LIMIT $1`, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (tracesv1.Trace, error) {
		var t tracesv1.Trace
		err := r.Scan(&t.ID, &t.AgentID, &t.Name, &t.Status, &t.DurationMs, &t.Input, &t.Output, &t.CreatedAt)
		return t, err
	})
}

func (s *service) list(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	ts, err := s.ListTraces(r.Context(), r.URL.Query().Get("agentId"), limit)
	if err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	if ts == nil {
		ts = []tracesv1.Trace{}
	}
	modkit.WriteJSON(w, 200, ts)
}

func (s *service) ingest(w http.ResponseWriter, r *http.Request) {
	var t tracesv1.Trace
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil || t.AgentID == "" {
		modkit.WriteError(w, 400, "agentId is required")
		return
	}
	if _, err := s.rt.Core().GetAgent(r.Context(), t.AgentID); err != nil {
		modkit.WriteError(w, 404, "agent not found")
		return
	}
	if t.Name == "" {
		t.Name = "invoke"
	}
	if t.Status == "" {
		t.Status = "ok"
	}
	saved, err := s.insert(r.Context(), t)
	if err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	modkit.WriteJSON(w, 201, saved)
}

func (s *service) insert(ctx context.Context, t tracesv1.Trace) (tracesv1.Trace, error) {
	err := s.rt.DB().QueryRow(ctx, `
		INSERT INTO traces (agent_id, name, status, duration_ms, input, output)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id, created_at`,
		t.AgentID, t.Name, t.Status, t.DurationMs, t.Input, t.Output).Scan(&t.ID, &t.CreatedAt)
	if err != nil {
		return t, err
	}
	return t, s.rt.Publish(ctx, "trace.recorded", t)
}

// seed records a few sample traces so the demo has data to look at.
func (s *service) seed(ctx context.Context, a modkit.Agent) error {
	h := fnv.New32a()
	h.Write([]byte(a.Name))
	base := int(h.Sum32() % 900)
	samples := []struct{ in, out, status string }{
		{"How do I reset my password?", "Go to Settings > Security and choose Reset.", "ok"},
		{"Summarise ticket #4821", "Customer reports a failed payment on renewal.", "ok"},
		{"What is our refund policy?", "", "error"},
	}
	for i, x := range samples {
		if _, err := s.insert(ctx, tracesv1.Trace{
			AgentID: a.ID, Name: fmt.Sprintf("%s.invoke", a.Name), Status: x.status,
			DurationMs: 300 + base + i*450, Input: x.in, Output: x.out,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *service) onAgentCreated(ctx context.Context, e modkit.Event) error {
	var d modkit.AgentCreated
	if err := json.Unmarshal(e.Data, &d); err != nil {
		return err
	}
	return s.seed(ctx, d.Agent)
}

func (s *service) finalizeAgent(ctx context.Context, req modkit.HookRequest) error {
	_, err := s.rt.DB().Exec(ctx, `DELETE FROM traces WHERE agent_id = $1`, req.ID)
	return err
}
