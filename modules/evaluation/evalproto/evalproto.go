// Package evalproto is the protocol between the evaluation module (inside
// the core binary) and its workloads (outside it). Workloads import only this
// package and the capability contracts, never the module itself.
package evalproto

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/a5anka/amp-modular-poc/modkit/contracts/tracesv1"
)

// Protocol is the version this build speaks. The module accepts the versions
// listed in its manifest (current and previous), so workers can be rolled
// after the core during an upgrade.
const Protocol = "eval/v1"

// Lanes.
const (
	LaneShared   = "shared"   // built-in evaluators only, served by the shared-worker pool
	LaneIsolated = "isolated" // any custom code: one isolated job per run, launched by eval-controller
)

// Evaluator kinds.
const (
	EvaluatorBuiltin = "builtin"
	EvaluatorCustom  = "custom"
)

// LeaseRequest asks for queued runs of one lane.
type LeaseRequest struct {
	Lane string `json:"lane"`
	Max  int    `json:"max"`
}

// Lease is one run handed to a workload until LeaseExpires.
type Lease struct {
	RunID        string    `json:"runId"`
	AgentID      string    `json:"agentId"`
	Lane         string    `json:"lane"`
	Evaluator    string    `json:"evaluator"`
	Attempt      int       `json:"attempt"`
	LeaseExpires time.Time `json:"leaseExpires"`
}

// Judge is the LLM binding an LLM-judge call would use, when ai-gateway is active.
type Judge struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// RunInput is everything a worker needs. The module builds it (it reads
// traces through traces.query/v1), so workers never touch the trace store.
type RunInput struct {
	RunID     string           `json:"runId"`
	AgentID   string           `json:"agentId"`
	Lane      string           `json:"lane"`
	Evaluator string           `json:"evaluator"`
	Code      string           `json:"code,omitempty"`
	Traces    []tracesv1.Trace `json:"traces"`
	Judge     *Judge           `json:"judge,omitempty"`
}

// Score for one trace.
type Score struct {
	TraceID string  `json:"traceId"`
	Score   float64 `json:"score"`
	Reason  string  `json:"reason"`
}

// Results are posted once per run. The module validates them before writing.
type Results struct {
	Evaluator string  `json:"evaluator"`
	Scores    []Score `json:"scores"`
}

// StatusUpdate is posted by eval-controller as a job progresses.
type StatusUpdate struct {
	Status  string `json:"status"` // running | failed
	Message string `json:"message,omitempty"`
}

// RunToken is minted by the module for one isolated run.
type RunToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// JudgeRequest / JudgeResponse are the AI Gateway judge call (simulated in the PoC).
type JudgeRequest struct {
	AgentID string `json:"agentId"`
	Prompt  string `json:"prompt"`
}

// JudgeResponse is the judge verdict.
type JudgeResponse struct {
	Score    float64 `json:"score"`
	Provider string  `json:"provider"`
	Model    string  `json:"model"`
}

// Client calls the core with a bearer token.
type Client struct {
	Base  string
	Token func() string
	HTTP  *http.Client
}

// APIError is a non-2xx answer.
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string { return fmt.Sprintf("%d: %s", e.Status, e.Msg) }

// Do sends a JSON request and decodes a JSON answer.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Base, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != nil {
		if t := c.Token(); t != "" {
			req.Header.Set("Authorization", "Bearer "+t)
		}
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
			Hint  string `json:"hint"`
		}
		msg := strings.TrimSpace(string(b))
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			msg = e.Error
			if e.Hint != "" {
				msg += " (" + e.Hint + ")"
			}
		}
		return &APIError{Status: resp.StatusCode, Msg: msg}
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}
