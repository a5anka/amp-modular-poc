// Package worker holds the evaluation module's workloads: eval-controller,
// the shared-worker pool and the isolated-run launcher. In production they
// run as separate processes (cmd/amp-eval-worker); with
// `amp serve --dev-workers` the same code runs in-process for local use.
package worker

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math"
	"os"
	"sync"
	"time"

	"github.com/a5anka/amp-modular-poc/modkit/contracts/tracesv1"
	"github.com/a5anka/amp-modular-poc/modules/evaluation/evalproto"
)

// Version is reported in heartbeats.
const Version = "0.2.0"

// Session keeps a workload instance registered: it heartbeats every few
// seconds with its credential and keeps the short-lived token it gets back.
// While the module is disabled the core refuses heartbeats and the session
// stands by with no token, so the loops idle.
type Session struct {
	Core       string
	Credential string
	Instance   string
	Log        *slog.Logger

	mu    sync.RWMutex
	token string
	kick  chan struct{}
}

// Standby drops the token after the core refused a call (module disabled,
// token revoked) and asks for an immediate heartbeat, so loops go quiet at
// once instead of failing until the next scheduled heartbeat.
func (s *Session) Standby(err error) {
	var ae *evalproto.APIError
	if !errors.As(err, &ae) || (ae.Status != 401 && ae.Status != 404) {
		return
	}
	s.mu.Lock()
	had := s.token != ""
	s.token = ""
	kick := s.kick
	s.mu.Unlock()
	if had {
		s.Log.Warn("core refused the call, standing by", "err", err)
	}
	select {
	case kick <- struct{}{}:
	default:
	}
}

// Token returns the current token, or "" while standing by.
func (s *Session) Token() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.token
}

// Client returns a core client that uses the session token.
func (s *Session) Client() *evalproto.Client {
	return &evalproto.Client{Base: s.Core, Token: s.Token}
}

func (s *Session) beat(ctx context.Context) error {
	var res struct {
		Token string `json:"token"`
	}
	err := (&evalproto.Client{Base: s.Core}).Do(ctx, "POST", "/api/workloads/heartbeat", map[string]string{
		"credential": s.Credential, "instance": s.Instance, "protocol": evalproto.Protocol, "version": Version,
	}, &res)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		if s.token != "" {
			s.Log.Warn("heartbeat refused, standing by", "err", err)
		}
		s.token = ""
		return err
	}
	if s.token == "" {
		s.Log.Info("connected to core", "core", s.Core, "instance", s.Instance)
	}
	s.token = res.Token
	return nil
}

// Run heartbeats until ctx ends.
func (s *Session) Run(ctx context.Context) {
	s.mu.Lock()
	if s.kick == nil {
		s.kick = make(chan struct{}, 1)
	}
	kick := s.kick
	s.mu.Unlock()
	if s.Instance == "" {
		h, _ := os.Hostname()
		s.Instance = fmt.Sprintf("%s-%d", h, os.Getpid())
	}
	for {
		err := s.beat(ctx)
		wait := 10 * time.Second
		if err != nil {
			wait = 3 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-kick:
			time.Sleep(time.Second)
		case <-time.After(wait):
		}
	}
}

// ---- built-in evaluators (trusted code) ----

// Heuristic is the built-in evaluator. It is first-party code, so it may run
// on the shared lane.
func Heuristic(t tracesv1.Trace) evalproto.Score {
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
	return evalproto.Score{TraceID: t.ID, Score: math.Round(s*100) / 100, Reason: reason}
}

// judge asks the AI Gateway for a verdict with the caller's token.
func judge(ctx context.Context, c *evalproto.Client, agentID, prompt string) (evalproto.JudgeResponse, error) {
	var v evalproto.JudgeResponse
	err := c.Do(ctx, "POST", "/m/ai-gateway/internal/judge", evalproto.JudgeRequest{AgentID: agentID, Prompt: prompt}, &v)
	return v, err
}

// ---- shared-worker ----

// SharedWorker leases shared-lane runs and scores them with built-in
// evaluators. It never runs custom code.
type SharedWorker struct {
	Session *Session
	Log     *slog.Logger
}

// Run polls for work until ctx ends.
func (w *SharedWorker) Run(ctx context.Context) {
	go w.Session.Run(ctx)
	c := w.Session.Client()
	for ctx.Err() == nil {
		n := 0
		if w.Session.Token() != "" {
			var leases []evalproto.Lease
			if err := c.Do(ctx, "POST", "/m/evaluation/internal/leases", evalproto.LeaseRequest{Lane: evalproto.LaneShared, Max: 10}, &leases); err != nil {
				w.Session.Standby(err)
			}
			for _, l := range leases {
				if err := w.runOne(ctx, c, l); err != nil {
					w.Log.Error("shared run failed", "run", l.RunID, "err", err)
				}
			}
			n = len(leases)
		}
		if n == 0 {
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}
}

func (w *SharedWorker) runOne(ctx context.Context, c *evalproto.Client, l evalproto.Lease) error {
	var in evalproto.RunInput
	if err := c.Do(ctx, "GET", "/m/evaluation/internal/runs/"+l.RunID+"/input", nil, &in); err != nil {
		return err
	}
	res := evalproto.Results{Evaluator: "builtin:heuristic"}
	useJudge := in.Judge != nil
	if useJudge {
		res.Evaluator = fmt.Sprintf("builtin:llm-judge via ai-gateway (%s/%s)", in.Judge.Provider, in.Judge.Model)
	}
	for _, t := range in.Traces {
		s := Heuristic(t)
		if useJudge && t.Output != "" {
			v, err := judge(ctx, c, in.AgentID, "Rate this answer: "+t.Output)
			if err != nil {
				// The gateway may be disabled mid-run: fall back rather than fail.
				res.Evaluator, useJudge = "builtin:heuristic (judge unavailable)", false
			} else {
				s.Score = math.Round((s.Score+v.Score)/2*100) / 100
				s.Reason += fmt.Sprintf("; judge %.2f", v.Score)
			}
		}
		res.Scores = append(res.Scores, s)
	}
	return c.Do(ctx, "POST", "/m/evaluation/internal/runs/"+l.RunID+"/results", res, nil)
}

// ---- eval-controller ----

// Controller leases isolated-lane runs, mints a run token for each through
// the module, and launches one isolated job per run through a Runner. It
// holds the runtime permissions (create Jobs) so the core binary needs none.
type Controller struct {
	Session  *Session
	Runner   Runner
	Capacity int
	Log      *slog.Logger
}

type inflight struct {
	lease  evalproto.Lease
	handle string
}

// Run polls for work until ctx ends.
func (k *Controller) Run(ctx context.Context) {
	go k.Session.Run(ctx)
	if k.Capacity <= 0 {
		k.Capacity = 4
	}
	c := k.Session.Client()
	var running []inflight
	for ctx.Err() == nil {
		running = k.reap(ctx, c, running)
		if free := k.Capacity - len(running); free > 0 && k.Session.Token() != "" {
			var leases []evalproto.Lease
			if err := c.Do(ctx, "POST", "/m/evaluation/internal/leases", evalproto.LeaseRequest{Lane: evalproto.LaneIsolated, Max: free}, &leases); err != nil {
				k.Session.Standby(err)
			}
			for _, l := range leases {
				if h, err := k.launch(ctx, c, l); err != nil {
					k.Log.Error("launch failed", "run", l.RunID, "err", err)
					_ = c.Do(ctx, "POST", "/m/evaluation/internal/runs/"+l.RunID+"/status", evalproto.StatusUpdate{Status: "failed", Message: "launch failed: " + err.Error()}, nil)
				} else {
					running = append(running, inflight{l, h})
				}
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

func (k *Controller) launch(ctx context.Context, c *evalproto.Client, l evalproto.Lease) (string, error) {
	var tok evalproto.RunToken
	if err := c.Do(ctx, "POST", "/m/evaluation/internal/runs/"+l.RunID+"/token", nil, &tok); err != nil {
		return "", fmt.Errorf("run token: %w", err)
	}
	h, err := k.Runner.Submit(ctx, RunSpec{RunID: l.RunID, CoreURL: k.Session.Core, Token: tok.Token, Deadline: 5 * time.Minute})
	if err != nil {
		return "", err
	}
	k.Log.Info("isolated run launched", "run", l.RunID, "runner", k.Runner.Name(), "handle", h)
	_ = c.Do(ctx, "POST", "/m/evaluation/internal/runs/"+l.RunID+"/status", evalproto.StatusUpdate{Status: "running", Message: k.Runner.Name() + ": " + h}, nil)
	return h, nil
}

func (k *Controller) reap(ctx context.Context, c *evalproto.Client, running []inflight) []inflight {
	var keep []inflight
	for _, r := range running {
		st, msg, err := k.Runner.Status(ctx, r.handle)
		switch {
		case err != nil:
			k.Log.Warn("status failed", "run", r.lease.RunID, "err", err)
			keep = append(keep, r)
		case st == StateRunning:
			keep = append(keep, r)
		case st == StateFailed:
			k.Log.Warn("isolated run failed", "run", r.lease.RunID, "msg", msg)
			_ = c.Do(ctx, "POST", "/m/evaluation/internal/runs/"+r.lease.RunID+"/status", evalproto.StatusUpdate{Status: "failed", Message: msg}, nil)
		default: // succeeded: the launcher posted results itself
		}
	}
	return keep
}

// ErrNoPython is returned when the custom-evaluator sandbox has no interpreter.
var ErrNoPython = errors.New("python3 not found: custom evaluators need it")
