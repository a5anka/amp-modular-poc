// evaluation console bundle.
import { useEffect, useState } from "preact/hooks";
import { html, api, useAsync, ErrorNote, can } from "@amp/ui-sdk";

const BASE = "/m/evaluation";

const SAMPLE = `import os

def evaluate(trace):
    # Runs in the isolated lane, outside the core binary.
    # The environment is empty: no run token, no credentials.
    leaked = [k for k in os.environ if "TOKEN" in k or "CRED" in k]
    if not trace["output"]:
        return {"score": 0.0, "reason": "no answer"}
    # judge() is answered by the trusted launcher, which calls the AI Gateway
    # with the run token on this code's behalf.
    v = judge("Is this a helpful answer? " + trace["output"])
    return {"score": v["score"] or 0.0,
            "reason": "judge %s/%s, credentials visible: %d" % (v.get("provider"), v.get("model"), len(leaked))}
`;

// Tiny shared cache so a table of N rows makes one request per agent.
const cache = new Map();
function scoresFor(agentId) {
  const hit = cache.get(agentId);
  if (hit && Date.now() - hit.at < 1500) return hit.p;
  const p = api(`${BASE}/scores?agentId=${agentId}`).then((xs) => new Map(xs.map((s) => [s.traceId, s])));
  cache.set(agentId, { at: Date.now(), p });
  return p;
}

function usePoll(fn, deps, ms = 2000) {
  const r = useAsync(fn, deps);
  useEffect(() => {
    const t = setInterval(r.reload, ms);
    return () => clearInterval(t);
  }, deps);
  return r;
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

// Health of the module's out-of-process workloads.
function WorkloadBanner() {
  const h = usePoll(() => api("/api/modules/evaluation/health"), [], 3000);
  if (!h.data) return null;
  const d = h.data.detail || {};
  const ok = h.data.status === "ok";
  return html`<div class=${ok ? "ok" : "error"}>
    <b>Workloads: ${h.data.status}</b>${!ok && h.data.reasons ? ` (${h.data.reasons.join("; ")})` : ""}.
    ${(h.data.workloads || []).filter((w) => w.kind === "service").map((w) =>
      html` <span class="badge ${w.live ? "active" : "uninstalled"}">${w.name}: ${w.live} live</span>`)}
    <span class="muted"> · queued: shared ${d.queued_shared ?? 0}, isolated ${d.queued_isolated ?? 0}</span>
    ${!ok && html`<div class="hint">Runs wait in the queue until the workloads connect. Start them with
      <code>amp module credential evaluation &lt;workload&gt;</code> and <code>amp-eval-worker</code>, or run the core with <code>--dev-workers</code>.</div>`}
  </div>`;
}

function RunForm({ agentId, onDone }) {
  const judge = useAsync(() => api(`${BASE}/judge?agentId=${agentId || ""}`), [agentId]);
  const [kind, setKind] = useState("builtin");
  const [code, setCode] = useState(SAMPLE);
  const [result, setResult] = useState(null);
  const [error, setError] = useState(null);
  const run = async () => {
    setError(null);
    try {
      const r = await api(`${BASE}/runs`, { method: "POST", body: { agentId, evaluator: kind, code: kind === "custom" ? code : "" } });
      cache.clear();
      setResult(r);
      onDone && onDone();
    } catch (err) { setError(err); }
  };
  if (!can("evaluation:evals.run")) return html`<p class="muted">Your role can view scores but not start runs.</p>`;
  if (!agentId) return html`<p class="muted">Pick an agent to start a run.</p>`;
  return html`<div>
    <div class="row">
      <label>Evaluator
        <select value=${kind} onChange=${(e) => setKind(e.target.value)}>
          <option value="builtin">Built-in (shared lane)</option>
          ${can("evaluation:evals.custom") && html`<option value="custom">Custom code (isolated lane)</option>`}
        </select>
      </label>
    </div>
    ${kind === "builtin"
      ? html`<p class="small muted">Scored by the shared-worker pool with ${judge.data?.evaluator || "…"}.</p>`
      : html`<label>Evaluator code (Python, defines evaluate(trace))
          <textarea rows="13" class="code" value=${code} onInput=${(e) => setCode(e.target.value)}></textarea></label>
        <p class="small muted">Queued on the isolated lane. eval-controller launches one isolated job for this run; the code never runs in the core.</p>`}
    <button onClick=${run}>Queue run</button>
    ${result && html`<div class="ok">Run <code>${result.id.slice(0, 8)}</code> queued on the ${result.lane} lane.</div>`}
    <${ErrorNote} error=${error} />
  </div>`;
}

function RunsTable({ agentId }) {
  const runs = usePoll(() => api(`${BASE}/runs?agentId=${agentId || ""}`), [agentId]);
  return html`<table>
    <thead><tr><th>Run</th><th>Lane</th><th>Evaluator</th><th>Status</th><th>Attempts</th><th>Scored</th><th>Average</th><th>By / message</th></tr></thead>
    <tbody>
      ${(runs.data || []).map((r) => html`<tr>
        <td><code>${r.id.slice(0, 8)}</code></td>
        <td><span class=${"badge " + (r.lane === "isolated" ? "event" : "modules")}>${r.lane}</span></td>
        <td>${r.evaluator}</td>
        <td><span class=${"badge " + ({ succeeded: "active", failed: "uninstalled", queued: "", leased: "disabled", running: "disabled" }[r.status] || "")}>${r.status}</span></td>
        <td>${r.attempts}</td><td>${r.evaluated}</td><td>${r.average ?? "-"}</td>
        <td class="small" title=${r.message || r.evaluatedBy}>${(r.message || r.evaluatedBy).replace(/\s+/g, " ").slice(-120)}</td>
      </tr>`)}
      ${runs.data?.length === 0 && html`<tr><td colspan="8" class="empty">No runs yet</td></tr>`}
    </tbody>
  </table>`;
}

function Evaluations() {
  const agents = useAsync(() => api("/api/agents"), []);
  const [agentId, setAgentId] = useState("");
  return html`
    <h1>Evaluations</h1>
    <p class="muted">Page contributed by <b>evaluation</b>. The control part (this API, the queue, the data) runs in the core; evaluator code runs in workloads outside it.</p>
    <${WorkloadBanner} />
    <label>Agent
      <select value=${agentId} onChange=${(e) => setAgentId(e.target.value)}>
        <option value="">all agents</option>
        ${(agents.data || []).map((a) => html`<option value=${a.id}>${a.name}</option>`)}
      </select>
    </label>
    <div class="card"><${RunForm} agentId=${agentId} /></div>
    <h3>Runs</h3>
    <${RunsTable} agentId=${agentId} />`;
}

function AgentScoreSection({ agent }) {
  const scores = usePoll(() => api(`${BASE}/scores?agentId=${agent.id}`), [agent.id], 3000);
  const xs = scores.data || [];
  const avg = xs.length ? (xs.reduce((a, s) => a + s.score, 0) / xs.length).toFixed(2) : "-";
  return html`
    <h3>Quality</h3>
    <p>Average score <b>${avg}</b> over ${xs.length} trace(s). <a href="#/m/evaluation/evaluations">Runs and custom evaluators</a></p>
    <${RunForm} agentId=${agent.id} />`;
}

export const pages = { evaluations: Evaluations };
export const slots = { ScoreColumn, AgentScoreSection };
