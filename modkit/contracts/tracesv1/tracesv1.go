// Package tracesv1 is the "traces.query/v1" capability contract.
//
// Capability contracts live in modkit, not in the providing module, so a
// consumer depends on the contract and never on the module that implements it.
// A breaking change ships as tracesv1 -> tracesv2, and a provider can offer
// both while consumers move.
package tracesv1

import (
	"context"
	"time"
)

// Capability is the capability ID.
const Capability = "traces.query/v1"

// Trace is one agent invocation.
type Trace struct {
	ID         string    `json:"id"`
	AgentID    string    `json:"agentId"`
	Name       string    `json:"name"`
	Status     string    `json:"status"` // ok | error
	DurationMs int       `json:"durationMs"`
	Input      string    `json:"input"`
	Output     string    `json:"output"`
	CreatedAt  time.Time `json:"createdAt"`
}

// Query reads traces.
type Query interface {
	ListTraces(ctx context.Context, agentID string, limit int) ([]Trace, error)
}
