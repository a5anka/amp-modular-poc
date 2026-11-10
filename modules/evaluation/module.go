// Package evaluation is the Evaluation module. It REQUIRES traces.query/v1
// (provided by observability) and can OPTIONALLY use llm.proxy/v1 (provided by
// ai-gateway) as an LLM judge.
//
// It shows the module-to-module side: it reads traces only through the
// capability (never from mod_observability), reacts to observability's
// trace.recorded event, fills the slot observability owns on its traces table,
// and changes behaviour when an optional dependency comes and goes.
package evaluation

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"math"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/a5anka/amp-modular-poc/modkit"
	"github.com/a5anka/amp-modular-poc/modkit/contracts/llmv1"
	"github.com/a5anka/amp-modular-poc/modkit/contracts/tracesv1"
)

//go:embed migrations/*.sql
var migrations embed.FS

//go:embed ui
var ui embed.FS

func init() { modkit.Register(&Module{}) }

// Module implements modkit.Module.
type Module struct{}

// Manifest implements modkit.Module.
func (*Module) Manifest() modkit.Manifest {
	return modkit.Manifest{
		Name:        "evaluation",
		Version:     "0.1.0",
		Description: "Scores agent traces. Uses the AI Gateway as an LLM judge when it is installed.",
		CoreAPI:     modkit.CoreAPIVersion,
		Requires:    []string{tracesv1.Capability},
		Optional:    []string{llmv1.Capability},
		Provides:    []string{"eval.scores/v1"},
		Permissions: []modkit.Permission{
			{Name: "evals.read", Description: "View scores"},
			{Name: "evals.run", Description: "Run evaluations"},
		},
		RoleGrants: map[string][]string{
			"admin":     {"evals.read", "evals.run"},
			"developer": {"evals.read", "evals.run"},
			"viewer":    {"evals.read"},
		},
		Events: []string{"evaluation.completed"},
		UI: &modkit.UI{
			Nav: []modkit.NavItem{{Label: "Evaluations", Page: "evaluations", Permission: "evals.read"}},
			Slots: []modkit.SlotContribution{
				{Slot: "observability.trace.columns", Component: "ScoreColumn", Permission: "evals.read"},
				{Slot: "agent.detail.sections", Component: "AgentScoreSection", Permission: "evals.read"},
			},
		},
	}
}

// Migrations implements modkit.Module.
func (*Module) Migrations() fs.FS { sub, _ := fs.Sub(migrations, "migrations"); return sub }

// UI implements modkit.Module.
func (*Module) UI() fs.FS { sub, _ := fs.Sub(ui, "ui"); return sub }

type score struct {
	TraceID   string  `json:"traceId"`
	AgentID   string  `json:"agentId"`
	Score     float64 `json:"score"`
	Evaluator string  `json:"evaluator"`
	Reason    string  `json:"reason"`
}

type service struct{ rt modkit.Runtime }

// Activate implements modkit.Module.
func (*Module) Activate(_ context.Context, rt modkit.Runtime) error {
	s := &service{rt: rt}
	rt.Handle("POST", "/runs", "evals.run", s.run)
	rt.Handle("GET", "/scores", "evals.read", s.scores)
	rt.Handle("GET", "/judge", "evals.read", s.judgeInfo)
	rt.Hook("agent", modkit.Finalize, s.finalizeAgent)
	// Continuous evaluation: score every trace observability records.
	rt.Subscribe("trace.recorded", s.onTrace)
	return nil
}

// judge picks the evaluator: an LLM judge through the AI Gateway when the
// optional llm.proxy capability is active and the agent is bound, otherwise
// a heuristic. Resolved per call, so installing or disabling ai-gateway
// changes behaviour immediately.
func (s *service) judge(ctx context.Context, agentID string) (string, error) {
	proxy, ok := modkit.Lookup[llmv1.Proxy](s.rt, llmv1.Capability)
	if !ok {
		return "heuristic", nil
	}
	b, bound, err := proxy.BindingFor(ctx, agentID)
	if err != nil {
		return "", err
	}
	if !bound {
		return "heuristic (agent has no LLM binding)", nil
	}
	return fmt.Sprintf("llm-judge via ai-gateway (%s/%s)", b.Provider, b.Model), nil
}

func evaluate(t tracesv1.Trace, evaluator string) score {
	h := fnv.New32a()
	h.Write([]byte(t.ID))
	jitter := float64(h.Sum32()%10) / 100
	s, reason := 0.0, ""
	switch {
	case t.Status != "ok":
		s, reason = 0.1+jitter, "the call failed"
	case t.Output == "":
		s, reason = 0.3+jitter, "empty answer"
	default:
		s = 0.95 - math.Min(float64(t.DurationMs)/10000, 0.3) - jitter
		reason = fmt.Sprintf("answered in %d ms", t.DurationMs)
	}
	return score{TraceID: t.ID, AgentID: t.AgentID, Score: math.Round(s*100) / 100, Evaluator: evaluator, Reason: reason}
}

func (s *service) save(ctx context.Context, sc score) error {
	_, err := s.rt.DB().Exec(ctx, `
		INSERT INTO scores (trace_id, agent_id, score, evaluator, reason) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (trace_id) DO UPDATE SET score = EXCLUDED.score, evaluator = EXCLUDED.evaluator,
			reason = EXCLUDED.reason, created_at = now()`,
		sc.TraceID, sc.AgentID, sc.Score, sc.Evaluator, sc.Reason)
	return err
}

func (s *service) run(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentID string `json:"agentId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AgentID == "" {
		modkit.WriteError(w, 400, "agentId is required")
		return
	}
	traces, ok := modkit.Lookup[tracesv1.Query](s.rt, tracesv1.Capability)
	if !ok {
		// Cannot happen while the resolver keeps required capabilities active.
		modkit.WriteError(w, 503, "traces.query/v1 is not available")
		return
	}
	ts, err := traces.ListTraces(r.Context(), req.AgentID, 100)
	if err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	evaluator, err := s.judge(r.Context(), req.AgentID)
	if err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	total := 0.0
	for _, t := range ts {
		sc := evaluate(t, evaluator)
		total += sc.Score
		if err := s.save(r.Context(), sc); err != nil {
			modkit.WriteError(w, 500, err.Error())
			return
		}
	}
	avg := 0.0
	if len(ts) > 0 {
		avg = math.Round(total/float64(len(ts))*100) / 100
	}
	res := map[string]any{"agentId": req.AgentID, "evaluated": len(ts), "average": avg, "evaluator": evaluator}
	_ = s.rt.Publish(r.Context(), "evaluation.completed", res)
	modkit.WriteJSON(w, 200, res)
}

func (s *service) scores(w http.ResponseWriter, r *http.Request) {
	rows, err := s.rt.DB().Query(r.Context(),
		`SELECT trace_id, agent_id, score::float8, evaluator, reason FROM scores
		 WHERE ($1 = '' OR agent_id = $1) ORDER BY created_at DESC LIMIT 500`, r.URL.Query().Get("agentId"))
	if err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (score, error) {
		var x score
		err := r.Scan(&x.TraceID, &x.AgentID, &x.Score, &x.Evaluator, &x.Reason)
		return x, err
	})
	if err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	if out == nil {
		out = []score{}
	}
	modkit.WriteJSON(w, 200, out)
}

func (s *service) judgeInfo(w http.ResponseWriter, r *http.Request) {
	ev, err := s.judge(r.Context(), r.URL.Query().Get("agentId"))
	if err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	_, gw := s.rt.Capability(llmv1.Capability)
	modkit.WriteJSON(w, 200, map[string]any{"evaluator": ev, "aiGatewayActive": gw})
}

func (s *service) onTrace(ctx context.Context, e modkit.Event) error {
	var t tracesv1.Trace
	if err := json.Unmarshal(e.Data, &t); err != nil {
		return err
	}
	ev, err := s.judge(ctx, t.AgentID)
	if err != nil {
		return err
	}
	return s.save(ctx, evaluate(t, ev))
}

func (s *service) finalizeAgent(ctx context.Context, req modkit.HookRequest) error {
	_, err := s.rt.DB().Exec(ctx, `DELETE FROM scores WHERE agent_id = $1`, req.ID)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
