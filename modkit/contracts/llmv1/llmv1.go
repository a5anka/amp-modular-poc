// Package llmv1 is the "llm.proxy/v1" capability contract.
package llmv1

import "context"

// Capability is the capability ID.
const Capability = "llm.proxy/v1"

// Binding is the LLM provider and model an agent is bound to through the AI Gateway.
type Binding struct {
	AgentID  string `json:"agentId"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// Proxy exposes AI Gateway routing to other modules.
type Proxy interface {
	// BindingFor returns the agent's binding; ok is false when it has none.
	BindingFor(ctx context.Context, agentID string) (b Binding, ok bool, err error)
}
