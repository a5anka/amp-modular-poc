package worker

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/a5anka/amp-modular-poc/modules/evaluation/evalproto"
)

//go:embed harness.py
var harness string

// Launch runs one isolated evaluation run. It is the entry point of an
// isolated job (`amp-eval-worker launcher`), and of the in-process runner in
// dev mode.
//
// The launcher is trusted and holds the run token. Evaluator code is not: it
// runs in a child interpreter with an empty environment, gets its input on
// stdin and can ask for an LLM judgement through a line protocol. The
// launcher makes that call to the AI Gateway with the run token, so the code
// never sees a credential. In Kubernetes the whole pod is additionally
// sandboxed (gVisor) and its egress limited to the core and the gateway.
func Launch(ctx context.Context, coreURL, runID, token string, log *slog.Logger) error {
	c := &evalproto.Client{Base: coreURL, Token: func() string { return token }}
	var in evalproto.RunInput
	if err := c.Do(ctx, "GET", "/m/evaluation/internal/runs/"+runID+"/input", nil, &in); err != nil {
		return fmt.Errorf("fetch input: %w", err)
	}
	res := evalproto.Results{Evaluator: in.Evaluator}
	if in.Evaluator != evalproto.EvaluatorCustom {
		for _, t := range in.Traces {
			res.Scores = append(res.Scores, Heuristic(t))
		}
		res.Evaluator = "builtin:heuristic"
	} else {
		scores, err := runCustom(ctx, c, in)
		if err != nil {
			return err
		}
		res.Scores, res.Evaluator = scores, "custom (isolated lane)"
	}
	if err := c.Do(ctx, "POST", "/m/evaluation/internal/runs/"+runID+"/results", res, nil); err != nil {
		return fmt.Errorf("post results: %w", err)
	}
	log.Info("isolated run finished", "run", runID, "scores", len(res.Scores))
	return nil
}

func runCustom(ctx context.Context, c *evalproto.Client, in evalproto.RunInput) ([]evalproto.Score, error) {
	py, err := exec.LookPath("python3")
	if err != nil {
		return nil, ErrNoPython
	}
	cmd := exec.CommandContext(ctx, py, "-I", "-c", harness)
	cmd.Env = []string{} // no token, no PATH, nothing inherited
	cmd.Dir = os.TempDir()
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// Traces go in; the token does not.
	payload, _ := json.Marshal(map[string]any{"code": in.Code, "traces": in.Traces})
	if _, err := fmt.Fprintf(stdin, "%s\n", payload); err != nil {
		return nil, err
	}
	var scores []evalproto.Score
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "RPC "):
			var req struct {
				Op     string `json:"op"`
				Prompt string `json:"prompt"`
			}
			_ = json.Unmarshal([]byte(line[4:]), &req)
			reply := map[string]any{"score": nil, "error": "unsupported"}
			if req.Op == "judge" {
				if v, err := judge(ctx, c, in.AgentID, req.Prompt); err != nil {
					reply = map[string]any{"score": nil, "error": err.Error()}
				} else {
					reply = map[string]any{"score": v.Score, "provider": v.Provider, "model": v.Model}
				}
			}
			b, _ := json.Marshal(reply)
			if _, err := fmt.Fprintf(stdin, "%s\n", b); err != nil {
				return nil, err
			}
		case strings.HasPrefix(line, "RESULT "):
			if err := json.Unmarshal([]byte(line[7:]), &scores); err != nil {
				return nil, fmt.Errorf("evaluator output: %w", err)
			}
		}
	}
	_ = stdin.Close()
	_, _ = io.Copy(io.Discard, stdout)
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("evaluator code failed: %s", tail(stderr.String(), 400))
	}
	if scores == nil {
		return nil, fmt.Errorf("evaluator produced no result: %s", tail(stderr.String(), 400))
	}
	return scores, nil
}
