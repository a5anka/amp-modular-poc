// observability console bundle.
import { useState } from "preact/hooks";
import { html, api, useAsync, useStore, slotItems, ErrorNote, can } from "@amp/ui-sdk";

const BASE = "/m/observability";

// Extension point owned by this module. Contributions export
// { header: string, Cell: ({ trace }) => vnode }.
const COLUMNS = "observability.trace.columns";

function TraceTable({ traces }) {
  useStore(); // re-render when modules filling our slot come or go
  const extra = slotItems(COLUMNS);
  return html`<table>
    <thead><tr>
      <th>When</th><th>Span</th><th>Status</th><th>Duration</th><th>Input</th>
      ${extra.map((c) => html`<th title=${"column from " + c.module}>${c.export.header} <span class="muted small">(${c.module})</span></th>`)}
    </tr></thead>
    <tbody>
      ${traces.map((t) => html`<tr>
        <td class="nowrap muted">${new Date(t.createdAt).toLocaleTimeString()}</td>
        <td><code>${t.name}</code></td>
        <td><span class=${"badge " + (t.status === "ok" ? "active" : "uninstalled")}>${t.status}</span></td>
        <td>${t.durationMs} ms</td>
        <td>${t.input}</td>
        ${extra.map((c) => html`<td><${c.export.Cell} trace=${t} /></td>`)}
      </tr>`)}
      ${traces.length === 0 && html`<tr><td colspan="9" class="empty">No traces</td></tr>`}
    </tbody>
  </table>`;
}

function Traces() {
  const agents = useAsync(() => api("/api/agents"), []);
  const [agentId, setAgentId] = useState("");
  const traces = useAsync(() => api(`${BASE}/traces?agentId=${agentId}&limit=50`), [agentId]);
  const [error, setError] = useState(null);
  const simulate = async () => {
    const list = agents.data || [];
    const a = list.find((x) => x.id === agentId) || list[Math.floor(Math.random() * list.length)];
    if (!a) return;
    setError(null);
    try {
      const ok = Math.random() > 0.25;
      await api(`${BASE}/traces`, { method: "POST", body: {
        agentId: a.id, name: `${a.name}.invoke`, status: ok ? "ok" : "error",
        durationMs: 200 + Math.floor(Math.random() * 2500), input: "Simulated request", output: ok ? "Simulated answer" : "",
      } });
      setTimeout(traces.reload, 300);
    } catch (err) { setError(err); }
  };
  return html`
    <h1>Traces</h1>
    <p class="muted">Page contributed by <b>observability</b>. Extra columns come from other modules through the <code>${COLUMNS}</code> slot; install <b>evaluation</b> and a Score column appears here.</p>
    <div class="row">
      <label>Agent
        <select value=${agentId} onChange=${(e) => setAgentId(e.target.value)}>
          <option value="">all agents</option>
          ${(agents.data || []).map((a) => html`<option value=${a.id}>${a.name}</option>`)}
        </select>
      </label>
      ${can("observability:traces.write") && html`<div><button onClick=${simulate}>Simulate a trace</button></div>`}
      <div><button class="secondary" onClick=${traces.reload}>Refresh</button></div>
    </div>
    <${ErrorNote} error=${traces.error || error} />
    <${TraceTable} traces=${traces.data || []} />`;
}

function AgentTracesSection({ agent }) {
  const traces = useAsync(() => api(`${BASE}/traces?agentId=${agent.id}&limit=5`), [agent.id]);
  return html`
    <h3>Recent traces</h3>
    <${ErrorNote} error=${traces.error} />
    <${TraceTable} traces=${traces.data || []} />`;
}

export const pages = { traces: Traces };
export const slots = { AgentTracesSection };
