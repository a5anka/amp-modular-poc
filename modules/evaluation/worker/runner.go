package worker

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// RunSpec describes one isolated run for a Runner.
type RunSpec struct {
	RunID    string
	CoreURL  string
	Token    string // run token: scoped to this run, minutes long
	Deadline time.Duration
}

// Job states reported by a Runner.
const (
	StateRunning   = "running"
	StateSucceeded = "succeeded"
	StateFailed    = "failed"
)

// Runner is the EvalRunner seam: where an isolated run executes.
type Runner interface {
	Name() string
	Submit(ctx context.Context, spec RunSpec) (handle string, err error)
	Status(ctx context.Context, handle string) (state, message string, err error)
}

type jobState struct {
	state, msg string
}

type tracker struct {
	mu   sync.Mutex
	jobs map[string]*jobState
}

func (t *tracker) set(h, st, msg string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.jobs == nil {
		t.jobs = map[string]*jobState{}
	}
	t.jobs[h] = &jobState{st, msg}
}

func (t *tracker) get(h string) (string, string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	j, ok := t.jobs[h]
	if !ok {
		return "", "", fmt.Errorf("unknown job %s", h)
	}
	return j.state, j.msg, nil
}

// ---- in-process (dev only) ----

// InProcessRunner runs the launcher in a goroutine of the current process.
// Custom code still runs in a child interpreter with an empty environment,
// but there is no sandbox: use it for `amp serve --dev-workers` only.
type InProcessRunner struct {
	Log *slog.Logger
	t   tracker
}

// Name implements Runner.
func (r *InProcessRunner) Name() string { return "inprocess" }

// Submit implements Runner.
func (r *InProcessRunner) Submit(_ context.Context, s RunSpec) (string, error) {
	h := "inproc-" + s.RunID[:8]
	r.t.set(h, StateRunning, "")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), s.Deadline)
		defer cancel()
		if err := Launch(ctx, s.CoreURL, s.RunID, s.Token, r.Log); err != nil {
			r.t.set(h, StateFailed, err.Error())
			return
		}
		r.t.set(h, StateSucceeded, "")
	}()
	return h, nil
}

// Status implements Runner.
func (r *InProcessRunner) Status(_ context.Context, h string) (string, string, error) {
	return r.t.get(h)
}

// ---- local process ----

// ProcessRunner starts `amp-eval-worker launcher` as a separate OS process
// per run. It shows the process boundary on a laptop; it is not a sandbox.
type ProcessRunner struct {
	Launcher string // path to the amp-eval-worker binary
	t        tracker
}

// Name implements Runner.
func (r *ProcessRunner) Name() string { return "process" }

// Submit implements Runner.
func (r *ProcessRunner) Submit(_ context.Context, s RunSpec) (string, error) {
	cmd := exec.Command(r.Launcher, "launcher", "--core", s.CoreURL, "--run", s.RunID)
	// Only the launcher sees the run token. It never passes it to evaluator code.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "AMP_RUN_TOKEN=" + s.Token}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = os.Stdout, &stderr
	if err := cmd.Start(); err != nil {
		return "", err
	}
	h := fmt.Sprintf("pid-%d", cmd.Process.Pid)
	r.t.set(h, StateRunning, "")
	go func() {
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				r.t.set(h, StateFailed, fmt.Sprintf("%v: %s", err, tail(stderr.String(), 300)))
				return
			}
			r.t.set(h, StateSucceeded, "")
		case <-time.After(s.Deadline):
			_ = cmd.Process.Kill()
			r.t.set(h, StateFailed, "deadline exceeded")
		}
	}()
	return h, nil
}

// Status implements Runner.
func (r *ProcessRunner) Status(_ context.Context, h string) (string, string, error) {
	return r.t.get(h)
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

// ---- Kubernetes Job ----

// KubernetesRunner creates one batch/v1 Job per run with the gVisor runtime
// class and no service-account token. It talks to the API server with the
// controller's in-cluster credentials, so only eval-controller needs RBAC to
// create Jobs; the core needs none. With DryRun it only logs the manifest.
type KubernetesRunner struct {
	Namespace    string
	Image        string // pinned by the module version
	RuntimeClass string // "gvisor"
	DryRun       bool
	Log          *slog.Logger
}

// Name implements Runner.
func (r *KubernetesRunner) Name() string { return "kubernetes" }

// JobManifest renders the Job for a run.
func (r *KubernetesRunner) JobManifest(s RunSpec) map[string]any {
	name := "amp-eval-run-" + strings.ToLower(s.RunID[:8])
	no := false
	return map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": name, "namespace": r.Namespace,
			"labels": map[string]string{"app.kubernetes.io/part-of": "amp", "amp.wso2.com/module": "evaluation", "amp.wso2.com/run": s.RunID}},
		"spec": map[string]any{
			"backoffLimit": 0, "activeDeadlineSeconds": int(s.Deadline.Seconds()), "ttlSecondsAfterFinished": 600,
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]string{"amp.wso2.com/lane": "isolated", "amp.wso2.com/run": s.RunID}},
				"spec": map[string]any{
					"restartPolicy": "Never", "runtimeClassName": r.RuntimeClass, "automountServiceAccountToken": no,
					"enableServiceLinks": no,
					"securityContext":    map[string]any{"runAsNonRoot": true, "seccompProfile": map[string]string{"type": "RuntimeDefault"}},
					"containers": []any{map[string]any{
						"name": "launcher", "image": r.Image,
						"args": []string{"launcher", "--core", s.CoreURL, "--run", s.RunID},
						// PoC: token in the pod spec. Production: a per-run Secret or a token-exchange init step.
						"env":       []any{map[string]string{"name": "AMP_RUN_TOKEN", "value": s.Token}},
						"resources": map[string]any{"limits": map[string]string{"cpu": "1", "memory": "512Mi"}},
						"securityContext": map[string]any{"allowPrivilegeEscalation": no, "readOnlyRootFilesystem": true,
							"capabilities": map[string]any{"drop": []string{"ALL"}}},
					}},
				},
			},
		},
	}
}

// Submit implements Runner.
func (r *KubernetesRunner) Submit(ctx context.Context, s RunSpec) (string, error) {
	job := r.JobManifest(s)
	name := job["metadata"].(map[string]any)["name"].(string)
	if r.DryRun {
		b, _ := json.MarshalIndent(job, "", "  ")
		r.Log.Info("dry run: would create Job", "name", name, "manifest", string(b))
		return "dry-run/" + name, nil
	}
	body, _ := json.Marshal(job)
	if err := r.api(ctx, "POST", "/apis/batch/v1/namespaces/"+r.Namespace+"/jobs", body, nil); err != nil {
		return "", err
	}
	return name, nil
}

// Status implements Runner.
func (r *KubernetesRunner) Status(ctx context.Context, h string) (string, string, error) {
	if strings.HasPrefix(h, "dry-run/") {
		return StateFailed, "dry run: Job not created", nil
	}
	var job struct {
		Status struct {
			Succeeded int `json:"succeeded"`
			Failed    int `json:"failed"`
		} `json:"status"`
	}
	if err := r.api(ctx, "GET", "/apis/batch/v1/namespaces/"+r.Namespace+"/jobs/"+h, nil, &job); err != nil {
		return "", "", err
	}
	switch {
	case job.Status.Succeeded > 0:
		return StateSucceeded, "", nil
	case job.Status.Failed > 0:
		return StateFailed, "job failed; see pod logs for " + h, nil
	}
	return StateRunning, "", nil
}

func (r *KubernetesRunner) api(ctx context.Context, method, path string, body []byte, out any) error {
	const sa = "/var/run/secrets/kubernetes.io/serviceaccount/"
	tok, err := os.ReadFile(sa + "token")
	if err != nil {
		return fmt.Errorf("not running in a cluster (no service-account token): %w", err)
	}
	ca, _ := os.ReadFile(sa + "ca.crt")
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	hc := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	host := os.Getenv("KUBERNETES_SERVICE_HOST") + ":" + os.Getenv("KUBERNETES_SERVICE_PORT")
	req, err := http.NewRequestWithContext(ctx, method, "https://"+host+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("kubernetes %s %s: %s %s", method, path, resp.Status, tail(string(b), 300))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}
