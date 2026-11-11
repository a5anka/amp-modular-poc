// Package e2e exercises a module whose workloads run outside the core:
// the evaluation control part runs in the core, and the eval-controller and
// shared-worker talk to it over HTTP with core-issued credentials and tokens.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"

	"github.com/a5anka/amp-modular-poc/internal/core"
	"github.com/a5anka/amp-modular-poc/internal/db"
	"github.com/a5anka/amp-modular-poc/internal/modhost"
	_ "github.com/a5anka/amp-modular-poc/modules/all"
	"github.com/a5anka/amp-modular-poc/modules/evaluation/evalproto"
	"github.com/a5anka/amp-modular-poc/modules/evaluation/worker"
	"github.com/a5anka/amp-modular-poc/web"
)

func startPostgres(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: needs an embedded Postgres")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	dir := t.TempDir()
	cache := os.Getenv("AMP_TEST_PG_CACHE")
	if cache == "" {
		home, _ := os.UserHomeDir()
		cache = filepath.Join(home, ".amp", "test-cache")
	}
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V17).Port(uint32(port)).
		DataPath(filepath.Join(dir, "data")).RuntimePath(filepath.Join(dir, "rt")).
		BinariesPath(filepath.Join(dir, "bin")).CachePath(cache).Logger(io.Discard))
	if err := pg.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.Stop() })
	return fmt.Sprintf("postgres://postgres:postgres@localhost:%d/postgres?sslmode=disable", port)
}

type env struct {
	t    *testing.T
	url  string
	host *modhost.Host
}

func (e *env) call(method, path, token string, in, out any) int {
	e.t.Helper()
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.url+path, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil && len(b) > 0 {
		_ = json.Unmarshal(b, out)
	}
	return resp.StatusCode
}

func (e *env) heartbeat(cred string) string {
	e.t.Helper()
	var hb modhost.HeartbeatResponse
	if code := e.call("POST", "/api/workloads/heartbeat", "", map[string]string{"credential": cred, "instance": "t", "protocol": evalproto.Protocol}, &hb); code != 200 {
		e.t.Fatalf("heartbeat: %d", code)
	}
	return hb.Token
}

type runView struct {
	ID, Lane, Status, Message, EvaluatedBy string
	Evaluated                              int
}

func (e *env) runs(agentID string) []runView {
	var rs []runView
	e.call("GET", "/m/evaluation/runs?agentId="+agentID, "", nil, &rs)
	return rs
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestEvaluationWorkloads(t *testing.T) {
	dsn := startPostgres(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	agents := core.NewAgents(pool)
	host := modhost.New(pool, dsn, agents, log)
	if _, err := host.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := host.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(core.NewServer(host, agents, web.FS(), log, "test").Handler())
	defer srv.Close()
	e := &env{t: t, url: srv.URL, host: host}

	for _, m := range []string{"evaluation", "ai-gateway"} {
		if _, err := host.Install(ctx, m, true); err != nil {
			t.Fatal(err)
		}
	}
	if h := host.ModuleHealth(ctx, "evaluation"); h.Status != "degraded" {
		t.Fatalf("no workloads connected: want degraded, got %s", h.Status)
	}

	var agent struct{ ID string }
	e.call("POST", "/api/agents", "", map[string]any{"name": "qa-bot",
		"extensions": map[string]any{"ai-gateway": map[string]string{"provider": "openai", "model": "gpt-4.1-mini"}}}, &agent)
	// observability seeds 3 traces; each becomes a shared-lane run.
	waitFor(t, "shared runs queued", func() bool { return len(e.runs(agent.ID)) == 3 })

	ctrlCred, err := host.CreateCredential(ctx, "evaluation", "eval-controller")
	if err != nil {
		t.Fatal(err)
	}
	poolCred, err := host.CreateCredential(ctx, "evaluation", "shared-worker")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("workload routes reject users and wrong tokens", func(t *testing.T) {
		if code := e.call("POST", "/m/evaluation/internal/leases", "", evalproto.LeaseRequest{Lane: "shared"}, nil); code != 401 {
			t.Fatalf("no token: %d", code)
		}
		ctrl := e.heartbeat(ctrlCred)
		if code := e.call("POST", "/m/evaluation/internal/leases", ctrl, evalproto.LeaseRequest{Lane: "shared"}, nil); code != 403 {
			t.Fatalf("controller leasing the shared lane: %d", code)
		}
		tampered := ctrl[:len(ctrl)-4] + "AAAA"
		if code := e.call("POST", "/m/evaluation/internal/leases", tampered, evalproto.LeaseRequest{Lane: "isolated"}, nil); code != 401 {
			t.Fatalf("tampered token: %d", code)
		}
		bad := map[string]string{"credential": ctrlCred, "instance": "old", "protocol": "eval/v0"}
		if code := e.call("POST", "/api/workloads/heartbeat", "", bad, nil); code != 401 {
			t.Fatalf("old protocol accepted: %d", code)
		}
	})

	t.Run("run tokens are scoped to one run and results are validated", func(t *testing.T) {
		code := "def evaluate(t):\n    return {'score': 0.5}\n"
		var a, b struct{ ID string }
		e.call("POST", "/m/evaluation/runs", "", map[string]string{"agentId": agent.ID, "evaluator": "custom", "code": code}, &a)
		e.call("POST", "/m/evaluation/runs", "", map[string]string{"agentId": agent.ID, "evaluator": "custom", "code": code}, &b)
		ctrl := e.heartbeat(ctrlCred)
		var leases []evalproto.Lease
		e.call("POST", "/m/evaluation/internal/leases", ctrl, evalproto.LeaseRequest{Lane: "isolated", Max: 5}, &leases)
		if len(leases) != 2 {
			t.Fatalf("leases %v", leases)
		}
		if code := e.call("GET", "/m/evaluation/internal/runs/"+a.ID+"/input", ctrl, nil, nil); code != 403 {
			t.Fatalf("controller reading run input: %d", code)
		}
		var tok evalproto.RunToken
		e.call("POST", "/m/evaluation/internal/runs/"+a.ID+"/token", ctrl, nil, &tok)
		if code := e.call("GET", "/m/evaluation/internal/runs/"+b.ID+"/input", tok.Token, nil, nil); code != 403 {
			t.Fatalf("run token of A used for B: %d", code)
		}
		var in evalproto.RunInput
		if code := e.call("GET", "/m/evaluation/internal/runs/"+a.ID+"/input", tok.Token, nil, &in); code != 200 || len(in.Traces) != 3 || in.Code == "" {
			t.Fatalf("input: %d %+v", code, in)
		}
		if code := e.call("POST", "/m/evaluation/internal/runs/"+a.ID+"/results", tok.Token,
			evalproto.Results{Scores: []evalproto.Score{{TraceID: "not-in-run", Score: 0.5}}}, nil); code != 422 {
			t.Fatalf("foreign trace accepted: %d", code)
		}
		if code := e.call("POST", "/m/evaluation/internal/runs/"+a.ID+"/results", tok.Token,
			evalproto.Results{Scores: []evalproto.Score{{TraceID: in.Traces[0].ID, Score: 7}}}, nil); code != 422 {
			t.Fatalf("score out of range accepted: %d", code)
		}
		// The run token also works at the AI Gateway (another module), and only for judge calls.
		var v evalproto.JudgeResponse
		if code := e.call("POST", "/m/ai-gateway/internal/judge", tok.Token, evalproto.JudgeRequest{AgentID: agent.ID, Prompt: "x"}, &v); code != 200 || v.Model != "gpt-4.1-mini" {
			t.Fatalf("judge with run token: %d %+v", code, v)
		}
		if code := e.call("POST", "/m/ai-gateway/internal/judge", ctrl, evalproto.JudgeRequest{AgentID: agent.ID, Prompt: "x"}, nil); code != 403 {
			t.Fatalf("controller token at the gateway: %d", code)
		}
		if code := e.call("POST", "/m/evaluation/internal/runs/"+a.ID+"/results", tok.Token,
			evalproto.Results{Evaluator: "custom", Scores: []evalproto.Score{{TraceID: in.Traces[0].ID, Score: 0.5}}}, nil); code != 204 {
			t.Fatalf("valid results: %d", code)
		}
		e.call("POST", "/m/evaluation/internal/runs/"+b.ID+"/status", ctrl, evalproto.StatusUpdate{Status: "failed", Message: "test"}, nil)
	})

	// Now run the real workloads (in goroutines; same HTTP protocol as the separate processes).
	wctx, stop := context.WithCancel(ctx)
	defer stop()
	go (&worker.Controller{Session: &worker.Session{Core: srv.URL, Credential: ctrlCred, Instance: "c1", Log: log},
		Runner: &worker.InProcessRunner{Log: log}, Log: log}).Run(wctx)
	go (&worker.SharedWorker{Session: &worker.Session{Core: srv.URL, Credential: poolCred, Instance: "p1", Log: log}, Log: log}).Run(wctx)

	t.Run("workloads connect and the module becomes healthy", func(t *testing.T) {
		waitFor(t, "health ok", func() bool { return host.ModuleHealth(ctx, "evaluation").Status == "ok" })
	})

	t.Run("shared lane scores with built-in evaluators and the gateway judge", func(t *testing.T) {
		waitFor(t, "shared runs done", func() bool {
			for _, r := range e.runs(agent.ID) {
				if r.Lane == "shared" && r.Status != "succeeded" {
					return false
				}
			}
			return true
		})
		for _, r := range e.runs(agent.ID) {
			if r.Lane == "shared" && !strings.Contains(r.EvaluatedBy, "llm-judge via ai-gateway") {
				t.Fatalf("shared run not judged through the gateway: %+v", r)
			}
		}
	})

	t.Run("custom code runs in the isolated lane without credentials", func(t *testing.T) {
		if _, err := exec.LookPath("python3"); err != nil {
			t.Skip("python3 not installed")
		}
		code := "import os\ndef evaluate(t):\n    v = judge('rate: ' + t['output']) if t['output'] else {'score': 0}\n" +
			"    leaked = [k for k in os.environ if 'TOKEN' in k or 'CRED' in k or k.startswith('AMP')]\n" +
			"    return {'score': v['score'] or 0, 'reason': 'leaked=%d' % len(leaked)}\n"
		var r struct{ ID string }
		e.call("POST", "/m/evaluation/runs", "", map[string]string{"agentId": agent.ID, "evaluator": "custom", "code": code}, &r)
		waitFor(t, "custom run done", func() bool {
			for _, x := range e.runs(agent.ID) {
				if x.ID == r.ID {
					if x.Status == "failed" {
						t.Fatalf("custom run failed: %s", x.Message)
					}
					return x.Status == "succeeded"
				}
			}
			return false
		})
		var scores []struct {
			Reason string  `json:"reason"`
			RunID  *string `json:"runId"`
		}
		e.call("GET", "/m/evaluation/scores?agentId="+agent.ID, "", nil, &scores)
		for _, s := range scores {
			if s.RunID != nil && *s.RunID == r.ID && !strings.HasPrefix(s.Reason, "leaked=0") {
				t.Fatalf("evaluator code saw credentials in its environment: %s", s.Reason)
			}
		}
	})

	t.Run("disabling the module turns workloads away", func(t *testing.T) {
		if err := host.Disable(ctx, "evaluation"); err != nil {
			t.Fatal(err)
		}
		bad := map[string]string{"credential": poolCred, "instance": "p1", "protocol": evalproto.Protocol}
		if code := e.call("POST", "/api/workloads/heartbeat", "", bad, nil); code != 401 {
			t.Fatalf("heartbeat while disabled: %d", code)
		}
		if err := host.Enable(ctx, "evaluation"); err != nil {
			t.Fatal(err)
		}
	})
}

func TestKubernetesJobManifest(t *testing.T) {
	r := &worker.KubernetesRunner{Namespace: "amp-eval", Image: "img@sha256:abc", RuntimeClass: "gvisor"}
	job := r.JobManifest(worker.RunSpec{RunID: "0123456789abcdef", CoreURL: "http://amp", Token: "t", Deadline: 5 * time.Minute})
	b, _ := json.Marshal(job)
	for _, want := range []string{`"runtimeClassName":"gvisor"`, `"automountServiceAccountToken":false`, `"backoffLimit":0`,
		`"name":"amp-eval-run-01234567"`, `"image":"img@sha256:abc"`, `"drop":["ALL"]`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("manifest misses %s: %s", want, b)
		}
	}
}
