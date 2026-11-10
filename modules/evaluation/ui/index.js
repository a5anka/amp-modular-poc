// evaluation console bundle.
import { useState } from "preact/hooks";
import { html, api, useAsync, ErrorNote, can } from "@amp/ui-sdk";

const BASE = "/m/evaluation";

// Tiny shared cache so a table of N rows makes one request per agent.
const cache = new Map();
function scoresFor(agentId) {
  const hit = cache.get(agentId);
  if (hit && Date.now() - hit.at < 1500) return hit.p;
  const p = api(`${BASE}/scores?agentId=${agentId}`).then((xs) => new Map(xs.map((s) => [s.traceId, s])));
  cache.set(agentId, { at: Date.now(), p });
  return p;
}

// Fills the "observability.trace.columns" slot owned by the observability module.
const ScoreColumn = {
  header: "Score",
  Cell({ trace }) {
    const s = useAsync(() => scoresFor(trace.agentId), [trace.id]);
    const x = s.data?.get(trace.id);
    if (!x) return html`<span class="muted">-</span>`;
    const color = x.score >= 0.7 ? "#166534" : x.score >= 0.4 ? "#92400e" : "#991b1b";
    return html`<span class="score" style=${{ color }} title=${`${x.evaluator}: ${x.reason}`}>${x.score.toFixed(2)}</span>`;
  },
};

function RunPanel({ agentId, onDone }) {
  const judge = useAsync(() => api(`${BASE}/judge?agentId=${agentId || ""}`), [agentId]);
  const [result, setResult] = useState(null);
  const [error, setError] = useState(null);
  const run = async () => {
    setError(null);
    try {
      const r = await api(`${BASE}/runs`, { method: "POST", body: { agentId } });
      cache.clear();
      setResult(r);
      onDone && onDone();
    } catch (err) { setError(err); }
  };
  return html`<div>
    <p>Evaluator: <b>${judge.data?.evaluator || "…"}</b>
      ${judge.data && !judge.data.aiGatewayActive && html` <span class="muted">(install ai-gateway and bind the agent to use an LLM judge)</span>`}</p>
    ${can("evaluation:evals.run") && agentId && html`<button onClick=${run}>Run evaluation</button>`}
    ${result && html`<div class="ok">Scored ${result.evaluated} trace(s), average <b>${result.average}</b> (${result.evaluator})</div>`}
    <${ErrorNote} error=${error} />
  </div>`;
}

function Evaluations() {
  const agents = useAsync(() => api("/api/agents"), []);
  const [agentId, setAgentId] = useState("");
  const scores = useAsync(() => api(`${BASE}/scores?agentId=${agentId}`), [agentId]);
  return html`
    <h1>Evaluations</h1>
    <p class="muted">Page contributed by <b>evaluation</b>. It reads traces through the <code>traces.query/v1</code> capability and scores new traces as observability records them.</p>
    <label>Agent
      <select value=${agentId} onChange=${(e) => setAgentId(e.target.value)}>
        <option value="">all agents</option>
        ${(agents.data || []).map((a) => html`<option value=${a.id}>${a.name}</option>`)}
      </select>
    </label>
    <div class="card"><${RunPanel} agentId=${agentId} onDone=${scores.reload} /></div>
    <${ErrorNote} error=${scores.error} />
    <table>
      <thead><tr><th>Trace</th><th>Score</th><th>Evaluator</th><th>Reason</th></tr></thead>
      <tbody>
        ${(scores.data || []).map((s) => html`<tr><td><code>${s.traceId.slice(0, 8)}</code></td><td>${s.score.toFixed(2)}</td><td>${s.evaluator}</td><td>${s.reason}</td></tr>`)}
        ${scores.data?.length === 0 && html`<tr><td colspan="4" class="empty">No scores yet</td></tr>`}
      </tbody>
    </table>`;
}

function AgentScoreSection({ agent }) {
  const scores = useAsync(() => api(`${BASE}/scores?agentId=${agent.id}`), [agent.id]);
  const xs = scores.data || [];
  const avg = xs.length ? (xs.reduce((a, s) => a + s.score, 0) / xs.length).toFixed(2) : "-";
  return html`
    <h3>Quality</h3>
    <p>Average score <b>${avg}</b> over ${xs.length} trace(s).</p>
    <${RunPanel} agentId=${agent.id} onDone=${scores.reload} />`;
}

export const pages = { evaluations: Evaluations };
export const slots = { ScoreColumn, AgentScoreSection };
