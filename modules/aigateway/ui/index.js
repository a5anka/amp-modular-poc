// ai-gateway console bundle. Plain ESM: imports the shared singletons by bare
// specifier and bundles nothing. The core serves it from the binary at
// /modules/ai-gateway/ui/<version>/index.js once the module is installed.
import { useState } from "preact/hooks";
import { html, api, useAsync, ErrorNote, can } from "@amp/ui-sdk";

const BASE = "/m/ai-gateway";

function Providers() {
  const list = useAsync(() => api(`${BASE}/providers`), []);
  const [form, setForm] = useState({ name: "", baseUrl: "", models: "" });
  const [error, setError] = useState(null);
  const add = async (e) => {
    e.preventDefault();
    setError(null);
    try {
      await api(`${BASE}/providers`, {
        method: "POST",
        body: { name: form.name, baseUrl: form.baseUrl, models: form.models.split(",").map((m) => m.trim()).filter(Boolean) },
      });
      setForm({ name: "", baseUrl: "", models: "" });
      list.reload();
    } catch (err) { setError(err); }
  };
  const del = async (name) => {
    setError(null);
    try { await api(`${BASE}/providers/${name}`, { method: "DELETE" }); list.reload(); } catch (err) { setError(err); }
  };
  return html`
    <h1>LLM Providers</h1>
    <p class="muted">Page contributed by the <b>ai-gateway</b> module. Data lives in schema <code>mod_ai_gateway</code>.</p>
    <${ErrorNote} error=${list.error || error} />
    <table>
      <thead><tr><th>Name</th><th>Kind</th><th>Base URL</th><th>Models</th><th></th></tr></thead>
      <tbody>
        ${(list.data || []).map((p) => html`<tr>
          <td>${p.name}</td><td>${p.kind}</td><td><code>${p.baseUrl}</code></td><td>${p.models.join(", ")}</td>
          <td>${can("ai-gateway:providers.write") && html`<button class="danger" onClick=${() => del(p.name)}>Remove</button>`}</td>
        </tr>`)}
      </tbody>
    </table>
    ${can("ai-gateway:providers.write") && html`
      <form class="card" onSubmit=${add}>
        <h3>Add a provider</h3>
        <div class="row">
          <label>Name <input value=${form.name} onInput=${(e) => setForm({ ...form, name: e.target.value })} placeholder="azure-openai" required /></label>
          <label>Base URL <input value=${form.baseUrl} onInput=${(e) => setForm({ ...form, baseUrl: e.target.value })} placeholder="https://..." required /></label>
          <label>Models (comma separated) <input value=${form.models} onInput=${(e) => setForm({ ...form, models: e.target.value })} placeholder="gpt-4.1" required /></label>
        </div>
        <button type="submit">Add</button>
      </form>`}`;
}

// Slot "agent.create.fields": adds a field to the core create-agent form.
// The value travels in the request as extensions["ai-gateway"]; the module's
// validate hook checks it before the core stores the agent.
function CreateAgentField({ setExtension, extensions }) {
  const list = useAsync(() => api(`${BASE}/providers`), []);
  const current = extensions["ai-gateway"] || {};
  const options = (list.data || []).flatMap((p) => p.models.map((m) => ({ value: `${p.name}/${m}`, label: `${p.name}/${m}`, provider: p.name, model: m })));
  // A deliberately invalid choice, to show the module's validate hook rejecting the core request.
  options.push({ value: "openai/not-a-model", label: "openai/not-a-model (rejected by the validate hook)", provider: "openai", model: "not-a-model" });
  const value = current.provider ? `${current.provider}/${current.model}` : "";
  return html`<label>LLM (via AI Gateway)
    <select value=${value} onChange=${(e) => {
      const o = options.find((x) => x.value === e.target.value);
      setExtension("ai-gateway", o ? { provider: o.provider, model: o.model } : undefined);
    }}>
      <option value="">none</option>
      ${options.map((o) => html`<option value=${o.value}>${o.label}</option>`)}
    </select>
  </label>`;
}

// Slot "agent.detail.sections": a section on the core agent page.
function AgentLLMSection({ agent }) {
  const b = useAsync(() => api(`${BASE}/bindings/${agent.id}`).catch((e) => (e.status === 404 ? null : Promise.reject(e))), [agent.id]);
  const providers = useAsync(() => api(`${BASE}/providers`), []);
  const [error, setError] = useState(null);
  const options = (providers.data || []).flatMap((p) => p.models.map((m) => `${p.name}/${m}`));
  const bind = async (v) => {
    setError(null);
    try {
      if (!v) await api(`${BASE}/bindings/${agent.id}`, { method: "DELETE" });
      else {
        const [provider, model] = v.split("/");
        await api(`${BASE}/bindings/${agent.id}`, { method: "PUT", body: { provider, model } });
      }
      b.reload();
    } catch (err) { setError(err); }
  };
  return html`
    <h3>LLM binding</h3>
    ${b.data ? html`<p>Routed to <b>${b.data.provider}</b> / <code>${b.data.model}</code> through the AI Gateway.</p>`
             : html`<p class="muted">Not bound to an LLM provider.</p>`}
    ${can("ai-gateway:bindings.write") && html`<label>Change
      <select value=${b.data ? `${b.data.provider}/${b.data.model}` : ""} onChange=${(e) => bind(e.target.value)}>
        <option value="">none</option>
        ${options.map((o) => html`<option value=${o}>${o}</option>`)}
      </select></label>`}
    <${ErrorNote} error=${error || b.error} />`;
}

export const pages = { providers: Providers };
export const slots = { CreateAgentField, AgentLLMSection };
