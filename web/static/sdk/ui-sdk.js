// @amp/ui-sdk: the contract between the console shell and module UIs.
//
// Modules import from here and from "preact/hooks"; the import map resolves
// both to the copies the shell loaded, so there is one store, one router and
// one component tree.
import { h, render as preactRender } from "preact";
import { useEffect, useState } from "preact/hooks";
import htm from "htm";

export const html = htm.bind(h);
export { h };
export const render = preactRender;

// ---- API client ----

export function currentRole() {
  return localStorage.getItem("amp.role") || "admin";
}

export function setRole(role) {
  localStorage.setItem("amp.role", role);
  return reloadManifest();
}

export class ApiError extends Error {
  constructor(status, body) {
    super(body?.error || `HTTP ${status}`);
    this.status = status;
    this.body = body;
  }
}

export async function api(path, { method = "GET", body } = {}) {
  const res = await fetch(path, {
    method,
    headers: { "X-AMP-Role": currentRole(), "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (res.status === 204) return null;
  const text = await res.text();
  const data = text ? JSON.parse(text) : null;
  if (!res.ok) throw new ApiError(res.status, data);
  return data;
}

// ---- store: UI manifest + loaded module bundles ----

const store = { manifest: { generation: 0, modules: [] }, me: null, bundles: new Map(), version: 0 };
const listeners = new Set();

function emit() {
  store.version++;
  listeners.forEach((l) => l(store.version));
}

/** Re-reads /api/ui-manifest and /api/me and imports bundles not loaded yet. */
export async function reloadManifest() {
  const [manifest, me] = await Promise.all([api("/api/ui-manifest"), api("/api/me")]);
  await Promise.all(
    manifest.modules.map(async (m) => {
      if (!store.bundles.has(m.entry)) {
        try {
          store.bundles.set(m.entry, await import(m.entry));
        } catch (e) {
          console.error(`failed to load UI of ${m.name}`, e);
        }
      }
    }),
  );
  store.manifest = manifest;
  store.me = me;
  emit();
}

export function useStore() {
  const [, setV] = useState(store.version);
  useEffect(() => {
    listeners.add(setV);
    return () => listeners.delete(setV);
  }, []);
  return store;
}

export function can(perm) {
  return !!store.me?.permissions?.includes(perm);
}

export function isModuleActive(name) {
  return store.manifest.modules.some((m) => m.name === name);
}

/** Pages of a module: the bundle's `pages` export. */
export function modulePage(name, page) {
  const m = store.manifest.modules.find((x) => x.name === name);
  return m ? store.bundles.get(m.entry)?.pages?.[page] : undefined;
}

/**
 * Everything contributed to a slot, in module order:
 * [{ module, component, export }]. Slot owners decide how to render them
 * (a section, a table column, a form field...).
 */
export function slotItems(slot) {
  const out = [];
  for (const m of store.manifest.modules) {
    for (const s of m.slots || []) {
      if (s.slot !== slot) continue;
      const exp = store.bundles.get(m.entry)?.slots?.[s.component];
      if (exp) out.push({ module: m.name, component: s.component, export: exp });
    }
  }
  return out;
}

/** Renders every component contributed to `name`, passing props through. */
export function Slot({ name, ...props }) {
  useStore();
  return html`${slotItems(name).map(
    (it) => html`<div class="slot-item" data-module=${it.module} key=${it.module + it.component}>
      <div class="slot-tag">from ${it.module}</div>
      <${it.export} ...${props} />
    </div>`,
  )}`;
}

// ---- small shared components ----

export function useAsync(fn, deps) {
  const [state, setState] = useState({ loading: true });
  const [tick, setTick] = useState(0);
  useEffect(() => {
    let live = true;
    setState((s) => ({ ...s, loading: true }));
    fn()
      .then((data) => live && setState({ data }))
      .catch((error) => live && setState({ error }));
    return () => (live = false);
  }, [...deps, tick]);
  return { ...state, reload: () => setTick((t) => t + 1) };
}

export function ErrorNote({ error }) {
  if (!error) return null;
  const mod = error.body?.module ? html`<b>${error.body.module}</b>: ` : "";
  const hint = error.body?.hint ? html`<div class="hint">${error.body.hint}</div>` : "";
  return html`<div class="error">${mod}${error.message}${hint}</div>`;
}

export function navigate(hash) {
  location.hash = hash;
}
