// Console shell. Owns layout, routing and the core pages; everything else
// comes from module bundles listed in /api/ui-manifest.
import { useEffect, useState } from "preact/hooks";
import {
  html, render, api, useStore, reloadManifest, setRole, currentRole, can,
  modulePage, Slot, slotItems, useAsync, ErrorNote, navigate,
} from "@amp/ui-sdk";

// ---- routing ----

function useRoute() {
  const [hash, setHash] = useState(location.hash || "#/agents");
  useEffect(() => {
    const on = () => setHash(location.hash || "#/agents");
    addEventListener("hashchange", on);
    return () => removeEventListener("hashchange", on);
  }, []);
  return hash.slice(2).split("/");
}

// ---- live updates ----

const activity = [];
const activityListeners = new Set();
function pushActivity(item) {
  activity.unshift({ ...item, at: new Date().toLocaleTimeString() });
  activity.length = Math.min(activity.length, 40);
  activityListeners.forEach((l) => l(activity.length + Math.random()));
}

function connectStream() {
  const es = new EventSource("/api/stream");
  es.addEventListener("modules.changed", async (e) => {
    const d = JSON.parse(e.data);
    await reloadManifest();
    pushActivity({ kind: "modules", text: `module set changed (generation ${d.generation}); UI updated in place. Active: ${d.active.join(", ") || "none"}` });
  });
  es.addEventListener("event", (e) => {
    const d = JSON.parse(e.data);
    pushActivity({ kind: "event", text: `${d.type} from ${d.source} delivered to ${d.deliveredTo?.join(", ") || "no subscribers"}` });
  });
}

// ---- layout ----

function App() {
  const store = useStore();
  const route = useRoute();
  if (!store.me) return html`<div class="loading">Loading…</div>`;
  const nav = [
    { label: "Agents", href: "#/agents", show: can("agents:read") },
    { label: "Modules", href: "#/modules", show: true },
    { label: "Activity", href: "#/activity", show: true },
  ];
  return html`
    <div class="layout">
      <header>
        <div class="brand">Agent Manager <span class="muted">modular PoC</span></div>
        <div class="right">
          <span class="muted">replica ${store.me.replica} · generation ${store.manifest.generation}</span>
          <label>role
            <select value=${currentRole()} onChange=${(e) => setRole(e.target.value)}>
              ${store.me.roles.map((r) => html`<option value=${r}>${r}</option>`)}
            </select>
          </label>
        </div>
      </header>
      <nav>
        <div class="nav-group">Core</div>
        ${nav.filter((n) => n.show).map((n) => html`<a href=${n.href} class=${location.hash.startsWith(n.href) ? "active" : ""}>${n.label}</a>`)}
        ${store.manifest.modules.filter((m) => m.nav.length).map((m) => html`
          <div class="nav-group">${m.name}</div>
          ${m.nav.map((n) => {
            const href = `#/m/${m.name}/${n.page}`;
            return html`<a href=${href} class=${location.hash === href ? "active" : ""}>${n.label}</a>`;
          })}`)}
      </nav>
      <main>${page(route)}</main>
    </div>`;
}

function page(route) {
  const [first, a, b] = route;
  if (first === "agents" && a) return html`<${AgentDetail} id=${a} />`;
  if (first === "agents") return html`<${Agents} />`;
  if (first === "modules") return html`<${Modules} />`;
  if (first === "activity") return html`<${Activity} />`;
  if (first === "m") {
    const P = modulePage(a, b);
    return P ? html`<${P} />` : html`<div class="empty">Module page ${a}/${b} is not available. The module may have been disabled.</div>`;
  }
  return html`<div class="empty">Not found</div>`;
}

// ---- core pages ----

function Agents() {
  const list = useAsync(() => api("/api/agents"), []);
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [extensions, setExtensions] = useState({});
  const [error, setError] = useState(null);
  const setExtension = (module, value) =>
    setExtensions((x) => {
      const n = { ...x };
      if (value === undefined) delete n[module];
      else n[module] = value;
      return n;
    });
  const create = async (e) => {
    e.preventDefault();
    setError(null);
    try {
      await api("/api/agents", { method: "POST", body: { name, description, extensions } });
      setName(""); setDescription(""); setExtensions({});
      list.reload();
    } catch (err) { setError(err); }
  };
  return html`
    <h1>Agents</h1>
    <p class="muted">Agents are a core resource. Modules attach their own data to them through
      create-form fields, validate and finalize hooks, and detail-page sections.</p>
    ${can("agents:write") && html`
      <form class="card" onSubmit=${create}>
        <h3>Register an agent</h3>
        <div class="row">
          <label>Name <input value=${name} onInput=${(e) => setName(e.target.value)} placeholder="support-bot" required /></label>
          <label>Description <input value=${description} onInput=${(e) => setDescription(e.target.value)} placeholder="Answers support tickets" /></label>
        </div>
        <${Slot} name="agent.create.fields" setExtension=${setExtension} extensions=${extensions} />
        <${ErrorNote} error=${error} />
        <button type="submit">Create</button>
        ${Object.keys(extensions).length > 0 && html`<pre class="muted small">extensions: ${JSON.stringify(extensions)}</pre>`}
      </form>`}
    <${ErrorNote} error=${list.error} />
    <table>
      <thead><tr><th>Name</th><th>Description</th><th>Created</th></tr></thead>
      <tbody>
        ${(list.data || []).map((a) => html`
          <tr onClick=${() => navigate(`#/agents/${a.id}`)} class="click">
            <td>${a.name}</td><td>${a.description}</td><td>${new Date(a.createdAt).toLocaleString()}</td>
          </tr>`)}
        ${list.data?.length === 0 && html`<tr><td colspan="3" class="empty">No agents yet</td></tr>`}
      </tbody>
    </table>`;
}

function AgentDetail({ id }) {
  const agent = useAsync(() => api(`/api/agents/${id}`), [id]);
  const [error, setError] = useState(null);
  const del = async () => {
    setError(null);
    try {
      await api(`/api/agents/${id}`, { method: "DELETE" });
      navigate("#/agents");
    } catch (err) { setError(err); }
  };
  if (agent.error) return html`<${ErrorNote} error=${agent.error} />`;
  if (!agent.data) return html`<div class="loading">Loading…</div>`;
  const a = agent.data;
  return html`
    <a href="#/agents" class="muted">← Agents</a>
    <h1>${a.name}</h1>
    <div class="card">
      <div><span class="muted">ID</span> ${a.id}</div>
      <div><span class="muted">Description</span> ${a.description || "-"}</div>
      ${can("agents:write") && html`<button class="danger" onClick=${del}>Delete agent</button>`}
      <${ErrorNote} error=${error} />
    </div>
    <${Slot} name="agent.detail.sections" agent=${a} />
    ${slotItems("agent.detail.sections").length === 0 &&
      html`<div class="empty">No module adds anything to this page yet. Install ai-gateway or observability.</div>`}`;
}

function Modules() {
  const store = useStore();
  const list = useAsync(() => api("/api/modules"), [store.manifest.generation]);
  const [busy, setBusy] = useState(null);
  const [error, setError] = useState(null);
  const [message, setMessage] = useState(null);
  const act = async (name, action, opts = {}) => {
    setBusy(name); setError(null); setMessage(null);
    try {
      const path = {
        install: `/api/modules/${name}/install${opts.withDeps ? "?withDeps=true" : ""}`,
        enable: `/api/modules/${name}/enable`,
        disable: `/api/modules/${name}/disable`,
        uninstall: `/api/modules/${name}${opts.purge ? "?purge=true" : ""}`,
      }[action];
      const res = await api(path, { method: action === "uninstall" ? "DELETE" : "POST" });
      setMessage(res?.steps ? `Done: ${res.steps.map((s) => `${s.action} ${s.module}`).join(", ")}` : `Done: ${action} ${name}`);
      list.reload();
    } catch (err) { setError({ err, name, action }); }
    setBusy(null);
  };
  const manage = can("modules:manage");
  return html`
    <h1>Modules</h1>
    <p class="muted">Every module below is compiled into this binary. Installing one creates its database
      schema, runs its migrations, mounts its API under ${"/m/<module>"} and adds its UI, on every replica, without a restart.
      ${!manage && html` <b>Your role cannot manage modules.</b>`}</p>
    ${message && html`<div class="ok">${message}</div>`}
    ${error && html`<div>
      <${ErrorNote} error=${error.err} />
      ${error.action === "install" && error.err.body?.hint?.includes("with-deps") &&
        html`<button onClick=${() => act(error.name, "install", { withDeps: true })}>Install ${error.name} with dependencies</button>`}
    </div>`}
    <div class="modules">
      ${(list.data || []).map((m) => html`
        <div class=${"card module " + m.state}>
          <div class="module-head">
            <h3>${m.name} <span class="muted">${m.version}</span></h3>
            <span class=${"badge " + m.state}>${m.state}</span>
          </div>
          <p>${m.description}</p>
          <dl>
            <dt>Provides</dt><dd>${m.provides?.join(", ") || "-"}</dd>
            <dt>Requires</dt><dd>${m.requires?.join(", ") || "-"}</dd>
            <dt>Optional</dt><dd>${m.optional?.join(", ") || "-"}</dd>
            <dt>Required by</dt><dd>${m.requiredBy?.join(", ") || "-"}</dd>
            <dt>Schema</dt><dd><code>${m.schema}</code> · ${m.migrations?.length || 0} migration(s)</dd>
            <dt>Adds</dt><dd>${`${m.routes} route(s) when active, ${m.permissions?.length || 0} permission(s), ${m.ui?.nav?.length || 0} page(s), ${m.ui?.slots?.length || 0} slot contribution(s)`}</dd>
          </dl>
          ${m.workloads?.length > 0 && html`<${Workloads} m=${m} manage=${manage} />`}
          ${manage && html`<div class="actions">
            ${(m.state === "available" || m.state === "uninstalled") && html`<button disabled=${busy} onClick=${() => act(m.name, "install")}>Install</button>`}
            ${m.state === "active" && html`<button class="secondary" disabled=${busy} onClick=${() => act(m.name, "disable")}>Disable</button>`}
            ${m.state === "disabled" && html`<button disabled=${busy} onClick=${() => act(m.name, "enable")}>Enable</button>`}
            ${(m.state === "active" || m.state === "disabled") && html`<button class="secondary" disabled=${busy} onClick=${() => act(m.name, "uninstall")}>Uninstall (keep data)</button>`}
            ${m.state !== "available" && html`<button class="danger" disabled=${busy} onClick=${() => act(m.name, "uninstall", { purge: true })}>Purge</button>`}
          </div>`}
        </div>`)}
    </div>`;
}

// Out-of-process workloads of a module: live instances, and credentials to start them.
function Workloads({ m, manage }) {
  const [secret, setSecret] = useState(null);
  const [error, setError] = useState(null);
  const health = m.health;
  const live = Object.fromEntries((health?.workloads || []).map((w) => [w.name, w]));
  const mint = async (wl) => {
    setError(null);
    try {
      const r = await api(`/api/modules/${m.name}/workloads/${wl}/credentials`, { method: "POST" });
      setSecret({ wl, value: r.credential });
    } catch (err) { setError(err); }
  };
  return html`<div class="workloads">
    <div class="wl-head">Runs outside the core
      ${health && html` <span class=${"badge " + (health.status === "ok" ? "active" : "uninstalled")}>${health.status}</span>`}</div>
    ${m.workloads.map((w) => html`<div class="wl">
      <code>${w.name}</code> <span class="muted small">${w.kind}${w.trusted ? ", trusted" : ", untrusted"}${w.isolation ? ", " + w.isolation : ""}</span>
      ${w.kind === "service" && m.state === "active" && html` <span class="small">${live[w.name]?.live ?? 0} live</span>`}
      ${w.kind === "service" && m.state === "active" && manage && html` <a href="#" class="small" onClick=${(e) => { e.preventDefault(); mint(w.name); }}>credential</a>`}
      ${w.kind === "job" && html` <span class="small muted">one per run, launched by eval-controller</span>`}
    </div>`)}
    ${secret && html`<pre class="small">AMP_WORKLOAD_CREDENTIAL=${secret.value} \
  amp-eval-worker ${secret.wl.replace("eval-", "")} --core ${location.origin}</pre>`}
    <${ErrorNote} error=${error} />
  </div>`;
}

function Activity() {
  const [, setV] = useState(0);
  useEffect(() => {
    activityListeners.add(setV);
    return () => activityListeners.delete(setV);
  }, []);
  return html`
    <h1>Activity</h1>
    <p class="muted">Live feed from this replica: module set changes (from any replica) and events delivered to modules.</p>
    <table><tbody>
      ${activity.map((a) => html`<tr><td class="muted nowrap">${a.at}</td><td><span class=${"badge " + a.kind}>${a.kind}</span></td><td>${a.text}</td></tr>`)}
      ${activity.length === 0 && html`<tr><td class="empty">Nothing yet. Install a module or create an agent.</td></tr>`}
    </tbody></table>`;
}

// ---- boot ----

reloadManifest().then(() => {
  render(html`<${App} />`, document.getElementById("app"));
  connectStream();
});
