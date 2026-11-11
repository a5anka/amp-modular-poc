// Package evaluation is the Evaluation module.
//
// It is split in two:
//   - the control part, here, runs inside the core binary: API, UI, data,
//     run queue, hooks, capability use. It never executes evaluator code.
//   - the workloads run outside it (package worker, cmd/amp-eval-worker):
//     eval-controller launches one isolated job per run with custom code,
//     and the shared-worker pool runs built-in evaluators.
//
// Workloads never connect to Postgres or the trace store. They lease runs,
// fetch inputs and post results over /m/evaluation/internal/*, with tokens
// the core issues. The module validates everything they send back.
package evaluation

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/a5anka/amp-modular-poc/modkit"
	"github.com/a5anka/amp-modular-poc/modkit/contracts/llmv1"
	"github.com/a5anka/amp-modular-poc/modkit/contracts/tracesv1"
	"github.com/a5anka/amp-modular-poc/modules/evaluation/evalproto"
	"github.com/a5anka/amp-modular-poc/modules/evaluation/worker"
)

//go:embed migrations/*.sql
var migrations embed.FS

//go:embed ui
var ui embed.FS

func init() { modkit.Register(&Module{}) }

// Module implements modkit.Module, modkit.DevWorkloads and modkit.HealthReporter.
type Module struct{}

const (
	leaseTTL   = 60 * time.Second
	runTTL     = 10 * time.Minute
	maxAttempt = 3
	// Pinned by this module version (an image digest in production), so
	// upgrading the binary upgrades the jobs eval-controller launches.
	workerImg = "ghcr.io/a5anka/amp-eval-worker:0.2.0"
)

// Manifest implements modkit.Module.
func (*Module) Manifest() modkit.Manifest {
	protocols := []string{evalproto.Protocol}
	return modkit.Manifest{
		Name:        "evaluation",
		Version:     "0.2.0",
		Description: "Scores agent traces. Control part in the core; evaluator code runs in workloads outside it.",
		CoreAPI:     modkit.CoreAPIVersion,
		Requires:    []string{tracesv1.Capability},
		Optional:    []string{llmv1.Capability},
		Provides:    []string{"eval.scores/v1"},
		Permissions: []modkit.Permission{
			{Name: "evals.read", Description: "View runs and scores"},
			{Name: "evals.run", Description: "Start evaluation runs"},
			{Name: "evals.custom", Description: "Run custom evaluator code"},
		},
		RoleGrants: map[string][]string{
			"admin":     {"evals.read", "evals.run", "evals.custom"},
			"developer": {"evals.read", "evals.run", "evals.custom"},
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
		Workloads: []modkit.Workload{
			{Name: "eval-controller", Kind: modkit.Service, Trusted: true, Required: true, Image: workerImg,
				Scopes: []string{"runs.lease", "runs.dispatch", "runs.status"}, Protocols: protocols,
				Description: "Leases isolated-lane runs and launches one isolated job per run (K8s Job, Cloud Run, local process). Holds the runtime permissions."},
			{Name: "shared-worker", Kind: modkit.Service, Trusted: true, Required: true, Image: workerImg,
				Scopes: []string{"runs.lease", "runs.read", "runs.report", "ai-gateway:llm.invoke"}, Protocols: protocols,
				Description: "Pool that scores shared-lane runs with built-in evaluators. Never runs custom code."},
			{Name: "custom-evaluator", Kind: modkit.Job, Trusted: false, Image: workerImg, Isolation: "gvisor", Egress: "core+gateway",
				Scopes:      []string{"runs.read", "runs.report", "ai-gateway:llm.invoke"},
				Description: "One job per run with custom code. A trusted launcher holds the run token; evaluator code gets no token and no network."},
		},
	}
}

// Migrations implements modkit.Module.
func (*Module) Migrations() fs.FS { sub, _ := fs.Sub(migrations, "migrations"); return sub }

// UI implements modkit.Module.
func (*Module) UI() fs.FS { sub, _ := fs.Sub(ui, "ui"); return sub }

// RunDevWorkloads implements modkit.DevWorkloads: both services run in this
// process and the isolated lane uses the in-process runner. Same protocol and
// credentials as production; no process boundary and no sandbox.
func (*Module) RunDevWorkloads(ctx context.Context, env modkit.DevEnv) error {
	ctrlCred, err := env.Credential(ctx, "eval-controller")
	if err != nil {
		return err
	}
	poolCred, err := env.Credential(ctx, "shared-worker")
	if err != nil {
		return err
	}
	ctrl := &worker.Controller{
		Session: &worker.Session{Core: env.CoreURL, Credential: ctrlCred, Instance: "dev-controller", Log: env.Logger},
		Runner:  &worker.InProcessRunner{Log: env.Logger}, Log: env.Logger,
	}
	pool := &worker.SharedWorker{
		Session: &worker.Session{Core: env.CoreURL, Credential: poolCred, Instance: "dev-pool", Log: env.Logger}, Log: env.Logger,
	}
	go ctrl.Run(ctx)
	pool.Run(ctx)
	return nil
}

type service struct{ rt modkit.Runtime }

// Activate implements modkit.Module.
func (*Module) Activate(_ context.Context, rt modkit.Runtime) error {
	s := &service{rt: rt}
	// User API.
	rt.Handle("POST", "/runs", "evals.run", s.createRun)
	rt.Handle("GET", "/runs", "evals.read", s.listRuns)
	rt.Handle("GET", "/scores", "evals.read", s.scores)
	rt.Handle("GET", "/judge", "evals.read", s.judgeInfo)
	// Workload API: core-issued bearer tokens only.
	rt.HandleInternal("POST", "/leases", "runs.lease", s.lease)
	rt.HandleInternal("POST", "/runs/{id}/token", "runs.dispatch", s.runToken)
	rt.HandleInternal("POST", "/runs/{id}/status", "runs.status", s.status)
	rt.HandleInternal("GET", "/runs/{id}/input", "runs.read", s.input)
	rt.HandleInternal("POST", "/runs/{id}/results", "runs.report", s.results)

	rt.Hook("agent", modkit.Finalize, s.finalizeAgent)
	// Online evaluation: every recorded trace becomes a small shared-lane run.
	rt.Subscribe("trace.recorded", s.onTrace)
	return nil
}

// Health implements modkit.HealthReporter: queue depth per lane.
func (*Module) Health(ctx context.Context, rt modkit.Runtime) map[string]any {
	out := map[string]any{"queued_shared": 0, "queued_isolated": 0}
	rows, err := rt.DB().Query(ctx, `SELECT lane, count(*), COALESCE(EXTRACT(EPOCH FROM now() - min(created_at))::int, 0)
		FROM runs WHERE status = 'queued' GROUP BY lane`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var lane string
		var n, age int
		if rows.Scan(&lane, &n, &age) == nil {
			out["queued_"+lane] = n
			out["oldest_"+lane+"_seconds"] = age
		}
	}
	return out
}

// ---- user API ----

type run struct {
	ID          string    `json:"id"`
	AgentID     string    `json:"agentId"`
	Lane        string    `json:"lane"`
	Evaluator   string    `json:"evaluator"`
	Status      string    `json:"status"`
	Attempts    int       `json:"attempts"`
	Message     string    `json:"message"`
	Evaluated   int       `json:"evaluated"`
	Average     *float64  `json:"average"`
	EvaluatedBy string    `json:"evaluatedBy"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

const runCols = `id, agent_id, lane, evaluator, status, attempts, message, evaluated, average::float8, evaluated_by, created_at, updated_at`

func scanRun(r pgx.CollectableRow) (run, error) {
	var x run
	err := r.Scan(&x.ID, &x.AgentID, &x.Lane, &x.Evaluator, &x.Status, &x.Attempts, &x.Message, &x.Evaluated, &x.Average, &x.EvaluatedBy, &x.CreatedAt, &x.UpdatedAt)
	return x, err
}

func (s *service) createRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentID   string `json:"agentId"`
		Evaluator string `json:"evaluator"`
		Code      string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AgentID == "" {
		modkit.WriteError(w, 400, "agentId is required")
		return
	}
	if _, err := s.rt.Core().GetAgent(r.Context(), req.AgentID); err != nil {
		modkit.WriteError(w, 404, "agent not found")
		return
	}
	lane := evalproto.LaneShared
	switch req.Evaluator {
	case "", evalproto.EvaluatorBuiltin:
		req.Evaluator, req.Code = evalproto.EvaluatorBuiltin, ""
	case evalproto.EvaluatorCustom:
		// Any custom code goes to the isolated lane, never to the shared pool.
		lane = evalproto.LaneIsolated
		if strings.TrimSpace(req.Code) == "" {
			modkit.WriteError(w, 400, "custom evaluator code is required")
			return
		}
		if !s.rt.Can(r.Context(), "evals.custom") {
			modkit.WriteError(w, 403, "missing permission evaluation:evals.custom")
			return
		}
	default:
		modkit.WriteError(w, 400, "evaluator must be builtin or custom")
		return
	}
	rows, err := s.rt.DB().Query(r.Context(), `INSERT INTO runs (agent_id, lane, evaluator, code) VALUES ($1, $2, $3, NULLIF($4, ''))
		RETURNING `+runCols, req.AgentID, lane, req.Evaluator, req.Code)
	if err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	x, err := pgx.CollectExactlyOneRow(rows, scanRun)
	if err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	modkit.WriteJSON(w, 202, x)
}

func (s *service) listRuns(w http.ResponseWriter, r *http.Request) {
	rows, err := s.rt.DB().Query(r.Context(), `SELECT `+runCols+` FROM runs
		WHERE ($1 = '' OR agent_id = $1) AND ($2 = '' OR evaluator = $2) ORDER BY created_at DESC LIMIT 50`,
		r.URL.Query().Get("agentId"), r.URL.Query().Get("evaluator"))
	if err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	out, err := pgx.CollectRows(rows, scanRun)
	if err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	if out == nil {
		out = []run{}
	}
	modkit.WriteJSON(w, 200, out)
}

type score struct {
	TraceID   string  `json:"traceId"`
	AgentID   string  `json:"agentId"`
	Score     float64 `json:"score"`
	Evaluator string  `json:"evaluator"`
	Reason    string  `json:"reason"`
	RunID     *string `json:"runId"`
}

func (s *service) scores(w http.ResponseWriter, r *http.Request) {
	rows, err := s.rt.DB().Query(r.Context(),
		`SELECT trace_id, agent_id, score::float8, evaluator, reason, run_id FROM scores
		 WHERE ($1 = '' OR agent_id = $1) ORDER BY created_at DESC LIMIT 500`, r.URL.Query().Get("agentId"))
	if err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (score, error) {
		var x score
		err := r.Scan(&x.TraceID, &x.AgentID, &x.Score, &x.Evaluator, &x.Reason, &x.RunID)
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

func (s *service) judgeFor(ctx context.Context, agentID string) (*evalproto.Judge, error) {
	proxy, ok := modkit.Lookup[llmv1.Proxy](s.rt, llmv1.Capability)
	if !ok || agentID == "" {
		return nil, nil
	}
	b, bound, err := proxy.BindingFor(ctx, agentID)
	if err != nil || !bound {
		return nil, err
	}
	return &evalproto.Judge{Provider: b.Provider, Model: b.Model}, nil
}

func (s *service) judgeInfo(w http.ResponseWriter, r *http.Request) {
	j, err := s.judgeFor(r.Context(), r.URL.Query().Get("agentId"))
	if err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	_, gw := s.rt.Capability(llmv1.Capability)
	ev := "built-in heuristic"
	if j != nil {
		ev = fmt.Sprintf("built-in LLM judge via ai-gateway (%s/%s)", j.Provider, j.Model)
	}
	modkit.WriteJSON(w, 200, map[string]any{"evaluator": ev, "aiGatewayActive": gw})
}

// ---- workload API ----

func (s *service) lease(w http.ResponseWriter, r *http.Request) {
	c, _ := modkit.ClaimsFrom(r.Context())
	var req evalproto.LeaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		modkit.WriteError(w, 400, "invalid JSON")
		return
	}
	// Each workload may only take its own lane.
	allowed := map[string]string{"shared-worker": evalproto.LaneShared, "eval-controller": evalproto.LaneIsolated}
	if allowed[c.Workload] != req.Lane {
		modkit.WriteError(w, 403, fmt.Sprintf("%s may not lease the %s lane", c.Workload, req.Lane))
		return
	}
	if req.Max <= 0 || req.Max > 20 {
		req.Max = 5
	}
	ctx := r.Context()
	tx, err := s.rt.DB().Begin(ctx)
	if err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	defer tx.Rollback(ctx)
	// Expired leases go back to the queue, or fail after maxAttempt tries.
	if _, err := tx.Exec(ctx, `UPDATE runs SET status = 'failed', message = 'gave up after '||attempts||' attempts (lease expired)', updated_at = now()
		WHERE status IN ('leased', 'running') AND lease_expires < now() AND attempts >= $1`, maxAttempt); err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET status = 'queued', lease_owner = NULL, message = 'lease expired, requeued', updated_at = now()
		WHERE status IN ('leased', 'running') AND lease_expires < now()`); err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	rows, err := tx.Query(ctx, `
		WITH next AS (
			SELECT id FROM runs WHERE lane = $1 AND status = 'queued' ORDER BY created_at LIMIT $2 FOR UPDATE SKIP LOCKED)
		UPDATE runs SET status = 'leased', attempts = attempts + 1, lease_owner = $3,
			lease_expires = now() + $4::interval, updated_at = now()
		FROM next WHERE runs.id = next.id
		RETURNING runs.id, runs.agent_id, runs.lane, runs.evaluator, runs.attempts, runs.lease_expires`,
		req.Lane, req.Max, c.Subject, fmt.Sprintf("%d seconds", int(leaseTTL.Seconds())))
	if err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	leases, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (evalproto.Lease, error) {
		var l evalproto.Lease
		err := r.Scan(&l.RunID, &l.AgentID, &l.Lane, &l.Evaluator, &l.Attempt, &l.LeaseExpires)
		return l, err
	})
	if err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	if err := tx.Commit(ctx); err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	if leases == nil {
		leases = []evalproto.Lease{}
	}
	modkit.WriteJSON(w, 200, leases)
}

type runRow struct {
	lane, status, agentID, evaluator, code string
	traceIDs                               []string
}

func (s *service) getRun(ctx context.Context, id string) (runRow, error) {
	var x runRow
	var code *string
	err := s.rt.DB().QueryRow(ctx, `SELECT lane, status, agent_id, evaluator, code, trace_ids FROM runs WHERE id = $1`, id).
		Scan(&x.lane, &x.status, &x.agentID, &x.evaluator, &code, &x.traceIDs)
	if code != nil {
		x.code = *code
	}
	return x, err
}

// authorizeRun: an isolated-lane run is only reachable with a run token for
// that run; a shared-lane run only with a shared-worker token.
func authorizeRun(ctx context.Context, id, lane string) error {
	c, _ := modkit.ClaimsFrom(ctx)
	if lane == evalproto.LaneIsolated {
		if c.Extra["run"] != id {
			return errors.New("isolated runs need a run token for this run")
		}
		return nil
	}
	if c.Workload != "shared-worker" {
		return errors.New("shared-lane runs are served by shared-worker only")
	}
	return nil
}

func (s *service) loadRun(w http.ResponseWriter, r *http.Request) (runRow, bool) {
	x, err := s.getRun(r.Context(), r.PathValue("id"))
	if errors.Is(err, pgx.ErrNoRows) {
		modkit.WriteError(w, 404, "run not found")
		return x, false
	}
	if err != nil {
		modkit.WriteError(w, 500, err.Error())
		return x, false
	}
	return x, true
}

func (s *service) runToken(w http.ResponseWriter, r *http.Request) {
	x, ok := s.loadRun(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if x.lane != evalproto.LaneIsolated || x.status != "leased" {
		modkit.WriteError(w, 409, "run tokens are minted for leased isolated-lane runs only")
		return
	}
	tok, err := s.rt.IssueToken(r.Context(), modkit.TokenRequest{
		Subject: "run:" + id, TTL: runTTL, Claims: map[string]string{"run": id},
		Scopes: []string{"runs.read", "runs.report", "ai-gateway:llm.invoke"},
	})
	if err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	modkit.WriteJSON(w, 200, evalproto.RunToken{Token: tok, ExpiresAt: time.Now().Add(runTTL)})
}

func (s *service) status(w http.ResponseWriter, r *http.Request) {
	var u evalproto.StatusUpdate
	if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
		modkit.WriteError(w, 400, "invalid JSON")
		return
	}
	var q string
	switch u.Status {
	case "running":
		q = `UPDATE runs SET status = 'running', message = $2, lease_expires = now() + interval '10 minutes', updated_at = now()
			WHERE id = $1 AND lane = 'isolated' AND status IN ('leased', 'running')`
	case "failed":
		q = `UPDATE runs SET status = 'failed', message = $2, updated_at = now()
			WHERE id = $1 AND lane = 'isolated' AND status IN ('leased', 'running')`
	default:
		modkit.WriteError(w, 400, "status must be running or failed")
		return
	}
	if _, err := s.rt.DB().Exec(r.Context(), q, r.PathValue("id"), u.Message); err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

func (s *service) input(w http.ResponseWriter, r *http.Request) {
	x, ok := s.loadRun(w, r)
	if !ok {
		return
	}
	ctx, id := r.Context(), r.PathValue("id")
	if err := authorizeRun(ctx, id, x.lane); err != nil {
		modkit.WriteError(w, 403, err.Error())
		return
	}
	if x.status != "leased" && x.status != "running" {
		modkit.WriteError(w, 409, "run is "+x.status)
		return
	}
	q, ok := modkit.Lookup[tracesv1.Query](s.rt, tracesv1.Capability)
	if !ok {
		modkit.WriteError(w, 503, "traces.query/v1 is not available")
		return
	}
	all, err := q.ListTraces(ctx, x.agentID, 100)
	if err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	traces := []tracesv1.Trace{}
	if x.traceIDs == nil {
		// First fetch fixes the run's trace set; results must match it.
		traces = append(traces, all...)
		ids := []string{}
		for _, t := range all {
			ids = append(ids, t.ID)
		}
		if _, err := s.rt.DB().Exec(ctx, `UPDATE runs SET trace_ids = $2 WHERE id = $1`, id, ids); err != nil {
			modkit.WriteError(w, 500, err.Error())
			return
		}
	} else {
		for _, t := range all {
			if slices.Contains(x.traceIDs, t.ID) {
				traces = append(traces, t)
			}
		}
	}
	j, _ := s.judgeFor(ctx, x.agentID)
	modkit.WriteJSON(w, 200, evalproto.RunInput{RunID: id, AgentID: x.agentID, Lane: x.lane, Evaluator: x.evaluator,
		Code: x.code, Traces: traces, Judge: j})
}

// results validates what a workload reports before anything is written:
// the run must be in flight, every score must belong to the run's input,
// and scores must be in [0, 1].
func (s *service) results(w http.ResponseWriter, r *http.Request) {
	x, ok := s.loadRun(w, r)
	if !ok {
		return
	}
	ctx, id := r.Context(), r.PathValue("id")
	if err := authorizeRun(ctx, id, x.lane); err != nil {
		modkit.WriteError(w, 403, err.Error())
		return
	}
	if x.status != "leased" && x.status != "running" {
		modkit.WriteError(w, 409, "run is "+x.status)
		return
	}
	var res evalproto.Results
	if err := json.NewDecoder(r.Body).Decode(&res); err != nil {
		modkit.WriteError(w, 400, "invalid JSON")
		return
	}
	for _, sc := range res.Scores {
		if !slices.Contains(x.traceIDs, sc.TraceID) {
			modkit.WriteError(w, 422, "score for a trace outside this run: "+sc.TraceID)
			return
		}
		if sc.Score < 0 || sc.Score > 1 || math.IsNaN(sc.Score) {
			modkit.WriteError(w, 422, "scores must be between 0 and 1")
			return
		}
	}
	tx, err := s.rt.DB().Begin(ctx)
	if err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	defer tx.Rollback(ctx)
	total := 0.0
	for _, sc := range res.Scores {
		total += sc.Score
		if _, err := tx.Exec(ctx, `
			INSERT INTO scores (trace_id, agent_id, score, evaluator, reason, run_id) VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (trace_id) DO UPDATE SET score = EXCLUDED.score, evaluator = EXCLUDED.evaluator,
				reason = EXCLUDED.reason, run_id = EXCLUDED.run_id, created_at = now()`,
			sc.TraceID, x.agentID, sc.Score, res.Evaluator, sc.Reason, id); err != nil {
			modkit.WriteError(w, 500, err.Error())
			return
		}
	}
	var avg *float64
	if n := len(res.Scores); n > 0 {
		a := math.Round(total/float64(n)*100) / 100
		avg = &a
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET status = 'succeeded', evaluated = $2, average = $3, evaluated_by = $4,
		message = '', updated_at = now() WHERE id = $1`, id, len(res.Scores), avg, res.Evaluator); err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	if err := tx.Commit(ctx); err != nil {
		modkit.WriteError(w, 500, err.Error())
		return
	}
	_ = s.rt.Publish(ctx, "evaluation.completed", map[string]any{"runId": id, "agentId": x.agentID, "lane": x.lane,
		"evaluated": len(res.Scores), "average": avg, "evaluator": res.Evaluator})
	w.WriteHeader(204)
}

// ---- events and hooks ----

func (s *service) onTrace(ctx context.Context, e modkit.Event) error {
	var t tracesv1.Trace
	if err := json.Unmarshal(e.Data, &t); err != nil {
		return err
	}
	_, err := s.rt.DB().Exec(ctx, `INSERT INTO runs (agent_id, lane, evaluator, trace_ids) VALUES ($1, 'shared', 'builtin', $2)`,
		t.AgentID, []string{t.ID})
	return err
}

func (s *service) finalizeAgent(ctx context.Context, req modkit.HookRequest) error {
	if _, err := s.rt.DB().Exec(ctx, `DELETE FROM scores WHERE agent_id = $1`, req.ID); err != nil {
		return err
	}
	_, err := s.rt.DB().Exec(ctx, `DELETE FROM runs WHERE agent_id = $1`, req.ID)
	return err
}
