package modhost

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/a5anka/amp-modular-poc/modkit"
)

// UIModule is one module's entry in the UI manifest.
type UIModule struct {
	Name     string                    `json:"name"`
	Version  string                    `json:"version"`
	Entry    string                    `json:"entry"`
	Nav      []modkit.NavItem          `json:"nav"`
	Slots    []modkit.SlotContribution `json:"slots"`
	OwnSlots []string                  `json:"ownSlots,omitempty"`
}

// UIManifest is what the console shell renders from.
type UIManifest struct {
	Generation int64      `json:"generation"`
	Modules    []UIModule `json:"modules"`
}

// UIManifest merges the UI contributions of the active modules, filtered by
// what the role may see. The shell fetches it at start-up and again whenever
// the module set changes.
func (h *Host) UIManifest(role string) UIManifest {
	v := h.current()
	out := UIManifest{Generation: h.Generation(), Modules: []UIModule{}}
	for _, name := range v.active {
		m := h.mods[name]
		mf := h.manifests[name]
		if mf.UI == nil || m.UI() == nil {
			continue
		}
		u := UIModule{Name: name, Version: mf.Version, Entry: "/modules/" + name + "/ui/" + h.uiTag(name) + "/index.js",
			Nav: []modkit.NavItem{}, Slots: []modkit.SlotContribution{}, OwnSlots: mf.UI.OwnSlots}
		for _, n := range mf.UI.Nav {
			if n.Permission == "" || h.Can(role, qualify(name, n.Permission)) {
				u.Nav = append(u.Nav, n)
			}
		}
		for _, s := range mf.UI.Slots {
			if s.Permission == "" || h.Can(role, qualify(name, s.Permission)) {
				u.Slots = append(u.Slots, s)
			}
		}
		out.Modules = append(out.Modules, u)
	}
	return out
}

// uiTag returns "<version>-<content hash>" for a bundle, so a rebuilt bundle
// gets a new URL even when the module version did not change. Computed once
// per module: embedded bundles never change while the process runs.
func (h *Host) uiTag(name string) string {
	h.tagMu.Lock()
	defer h.tagMu.Unlock()
	if t, ok := h.tags[name]; ok {
		return t
	}
	ui := h.mods[name].UI()
	sum := sha256.New()
	_ = fs.WalkDir(ui, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(ui, p)
		if err != nil {
			return err
		}
		sum.Write([]byte(p))
		sum.Write(b)
		return nil
	})
	t := h.manifests[name].Version + "-" + hex.EncodeToString(sum.Sum(nil))[:10]
	h.tags[name] = t
	return t
}

// ServeUI serves /modules/<name>/ui/<tag>/<file> from the module's
// embedded bundle. Only active modules serve assets, and the version in the
// path makes the URLs safe to cache forever.
func (h *Host) ServeUI(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/modules/"), "/", 4)
	if len(parts) != 4 || parts[1] != "ui" {
		http.NotFound(w, r)
		return
	}
	name, version, file := parts[0], parts[2], parts[3]
	if !contains(h.current().active, name) {
		http.NotFound(w, r)
		return
	}
	ui := h.mods[name].UI()
	if ui == nil || h.uiTag(name) != version {
		http.NotFound(w, r)
		return
	}
	b, err := fs.ReadFile(ui, path.Clean(file))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ct := mime.TypeByExtension(path.Ext(file))
	if path.Ext(file) == ".js" {
		ct = "text/javascript; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = w.Write(b)
}
