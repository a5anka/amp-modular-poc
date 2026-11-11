package modhost

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/a5anka/amp-modular-poc/internal/db"
	"github.com/a5anka/amp-modular-poc/modkit"
)

// Tokens for workloads and jobs are EdDSA JWTs signed with a key every
// replica shares (core.signing_keys). Anything holding the JWKS, such as the
// AI Gateway runtime, can verify them offline.

const (
	issuer          = "amp-core"
	workloadTTL     = 15 * time.Minute
	maxIssuedTTL    = 30 * time.Minute
	liveWindow      = 30 * time.Second
	credentialBytes = 24
)

type keyring struct {
	mu   sync.RWMutex
	kid  string
	priv ed25519.PrivateKey
	pubs map[string]ed25519.PublicKey
}

func (h *Host) loadKeys(ctx context.Context) error {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := db.Lock(ctx, tx); err != nil {
		return err
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM core.signing_keys`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		kid := randomHex(8)
		if _, err := tx.Exec(ctx, `INSERT INTO core.signing_keys (kid, private_key) VALUES ($1, $2)`, kid, []byte(priv.Seed())); err != nil {
			return err
		}
	}
	rows, err := tx.Query(ctx, `SELECT kid, private_key FROM core.signing_keys ORDER BY created_at DESC`)
	if err != nil {
		return err
	}
	k := &keyring{pubs: map[string]ed25519.PublicKey{}}
	for rows.Next() {
		var kid string
		var seed []byte
		if err := rows.Scan(&kid, &seed); err != nil {
			return err
		}
		priv := ed25519.NewKeyFromSeed(seed)
		if k.priv == nil {
			k.kid, k.priv = kid, priv
		}
		k.pubs[kid] = priv.Public().(ed25519.PublicKey)
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	h.keys = k
	return nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

type jwtClaims struct {
	Iss    string            `json:"iss"`
	Sub    string            `json:"sub"`
	Aud    []string          `json:"aud"`
	Exp    int64             `json:"exp"`
	Iat    int64             `json:"iat"`
	Jti    string            `json:"jti"`
	Mod    string            `json:"mod"`
	Wl     string            `json:"wl,omitempty"`
	Scope  string            `json:"scope"`
	Extras map[string]string `json:"ext,omitempty"`
}

func (h *Host) sign(c jwtClaims) (string, error) {
	h.keys.mu.RLock()
	defer h.keys.mu.RUnlock()
	head, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": h.keys.kid})
	body, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	unsigned := b64(head) + "." + b64(body)
	return unsigned + "." + b64(ed25519.Sign(h.keys.priv, []byte(unsigned))), nil
}

// VerifyToken checks a workload or run token and that its audience includes audience.
func (h *Host) VerifyToken(token, audience string) (modkit.Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return modkit.Claims{}, errors.New("malformed token")
	}
	hb, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	bb, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	sig, err3 := base64.RawURLEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return modkit.Claims{}, errors.New("malformed token")
	}
	var head struct{ Alg, Kid string }
	if err := json.Unmarshal(hb, &head); err != nil || head.Alg != "EdDSA" {
		return modkit.Claims{}, errors.New("unsupported token")
	}
	h.keys.mu.RLock()
	pub, ok := h.keys.pubs[head.Kid]
	h.keys.mu.RUnlock()
	if !ok || !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return modkit.Claims{}, errors.New("invalid token signature")
	}
	var c jwtClaims
	if err := json.Unmarshal(bb, &c); err != nil {
		return modkit.Claims{}, errors.New("malformed token")
	}
	if c.Iss != issuer || time.Now().Unix() >= c.Exp {
		return modkit.Claims{}, errors.New("token expired or not issued by this core")
	}
	if !slices.Contains(c.Aud, audience) {
		return modkit.Claims{}, fmt.Errorf("token is not meant for %s", audience)
	}
	return modkit.Claims{Subject: c.Sub, Module: c.Mod, Workload: c.Wl, Audience: c.Aud,
		Scopes: strings.Fields(c.Scope), Extra: c.Extras, Expiry: time.Unix(c.Exp, 0)}, nil
}

// audiences is the module plus the active providers of the capabilities it
// requires or optionally uses.
func (h *Host) audiences(module string) []string {
	aud := []string{module}
	v := h.current()
	mf := h.manifests[module]
	for _, c := range append(append([]string(nil), mf.Requires...), mf.Optional...) {
		if p, ok := v.capBy[c]; ok && !slices.Contains(aud, p) {
			aud = append(aud, p)
		}
	}
	return aud
}

// scopesFor qualifies module-local scopes. Scopes of other modules are only
// granted when they belong to an audience module (e.g. ai-gateway:llm.invoke).
func scopesFor(module string, scopes, aud []string) []string {
	var out []string
	for _, s := range scopes {
		if !strings.Contains(s, ":") {
			out = append(out, module+":"+s)
			continue
		}
		if slices.Contains(aud, strings.SplitN(s, ":", 2)[0]) {
			out = append(out, s)
		}
	}
	return out
}

func (h *Host) issue(module, workload, subject string, scopes []string, ttl time.Duration, extra map[string]string) (string, error) {
	if ttl <= 0 || ttl > maxIssuedTTL {
		ttl = maxIssuedTTL
	}
	aud := h.audiences(module)
	now := time.Now()
	return h.sign(jwtClaims{Iss: issuer, Sub: subject, Aud: aud, Iat: now.Unix(), Exp: now.Add(ttl).Unix(),
		Jti: randomHex(8), Mod: module, Wl: workload, Scope: strings.Join(scopesFor(module, scopes, aud), " "), Extras: extra})
}

// ServeJWKS publishes the verification keys.
func (h *Host) ServeJWKS(w http.ResponseWriter, r *http.Request) {
	h.keys.mu.RLock()
	defer h.keys.mu.RUnlock()
	var keys []map[string]string
	for kid, pub := range h.keys.pubs {
		keys = append(keys, map[string]string{"kty": "OKP", "crv": "Ed25519", "alg": "EdDSA", "use": "sig", "kid": kid, "x": b64(pub)})
	}
	modkit.WriteJSON(w, 200, map[string]any{"keys": keys})
}

// ---- workload credentials and heartbeats ----

func (h *Host) workload(module, name string) (modkit.Workload, error) {
	for _, wl := range h.manifests[module].Workloads {
		if wl.Name == name {
			return wl, nil
		}
	}
	return modkit.Workload{}, &ResolveError{Msg: fmt.Sprintf("module %s declares no workload %q", module, name)}
}

// CreateCredential mints a workload credential. The secret is returned once
// and stored hashed. Production would bind workloads to a platform identity
// (projected service-account token, ThunderID client) instead.
func (h *Host) CreateCredential(ctx context.Context, module, workload string) (string, error) {
	wl, err := h.workload(module, workload)
	if err != nil {
		return "", err
	}
	if wl.Kind != modkit.Service {
		return "", &ResolveError{Msg: fmt.Sprintf("%s is a %s workload; jobs use run tokens minted by the module", workload, wl.Kind)}
	}
	if !contains(h.current().active, module) {
		return "", &ResolveError{Msg: module + " is not active", Hint: "install or enable the module first"}
	}
	secret := "amw_" + randomHex(credentialBytes)
	sum := sha256.Sum256([]byte(secret))
	if _, err := h.pool.Exec(ctx, `INSERT INTO core.workload_credentials (module, workload, secret_hash) VALUES ($1, $2, $3)`,
		module, workload, hex.EncodeToString(sum[:])); err != nil {
		return "", err
	}
	return secret, nil
}

// HeartbeatRequest is sent by workload instances every few seconds.
type HeartbeatRequest struct {
	Credential string `json:"credential"`
	Instance   string `json:"instance"`
	Protocol   string `json:"protocol"`
	Version    string `json:"version"`
}

// HeartbeatResponse carries a fresh short-lived token.
type HeartbeatResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
	Module    string    `json:"module"`
	Workload  string    `json:"workload"`
}

// Heartbeat authenticates a workload instance, records it as connected and
// returns a fresh token.
func (h *Host) Heartbeat(ctx context.Context, req HeartbeatRequest) (HeartbeatResponse, error) {
	sum := sha256.Sum256([]byte(req.Credential))
	var module, workload string
	err := h.pool.QueryRow(ctx, `SELECT module, workload FROM core.workload_credentials
		WHERE secret_hash = $1 AND revoked_at IS NULL`, hex.EncodeToString(sum[:])).Scan(&module, &workload)
	if errors.Is(err, pgx.ErrNoRows) {
		return HeartbeatResponse{}, &ResolveError{Msg: "unknown or revoked workload credential"}
	}
	if err != nil {
		return HeartbeatResponse{}, err
	}
	if !contains(h.current().active, module) {
		return HeartbeatResponse{}, &ResolveError{Msg: module + " is not active; workload must stand by"}
	}
	wl, err := h.workload(module, workload)
	if err != nil {
		return HeartbeatResponse{}, err
	}
	if !slices.Contains(wl.Protocols, req.Protocol) {
		return HeartbeatResponse{}, &ResolveError{Msg: fmt.Sprintf("%s/%s speaks %q; %s %s accepts %s",
			module, workload, req.Protocol, module, h.manifests[module].Version, strings.Join(wl.Protocols, ", ")),
			Hint: "deploy the workload image pinned by this module version"}
	}
	if req.Instance == "" {
		req.Instance = "default"
	}
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO core.workload_instances (module, workload, instance, protocol, version) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (module, workload, instance) DO UPDATE SET last_seen = now(), protocol = EXCLUDED.protocol, version = EXCLUDED.version`,
		module, workload, req.Instance, req.Protocol, req.Version); err != nil {
		return HeartbeatResponse{}, err
	}
	tok, err := h.issue(module, workload, "workload:"+workload+"/"+req.Instance, wl.Scopes, workloadTTL, nil)
	if err != nil {
		return HeartbeatResponse{}, err
	}
	return HeartbeatResponse{Token: tok, ExpiresAt: time.Now().Add(workloadTTL), Module: module, Workload: workload}, nil
}

// WorkloadStatus is the API view of a declared workload.
type WorkloadStatus struct {
	modkit.Workload
	Live      int      `json:"live"`
	Instances []string `json:"instances,omitempty"`
}

// Health of one module: its workloads and anything the module reports.
type Health struct {
	Status    string           `json:"status"` // ok | degraded | inactive
	Reasons   []string         `json:"reasons,omitempty"`
	Workloads []WorkloadStatus `json:"workloads,omitempty"`
	Detail    map[string]any   `json:"detail,omitempty"`
}

// ModuleHealth reports live workload instances (heartbeat within 30s).
func (h *Host) ModuleHealth(ctx context.Context, module string) Health {
	if !contains(h.current().active, module) {
		return Health{Status: "inactive"}
	}
	out := Health{Status: "ok"}
	live := map[string][]string{}
	rows, err := h.pool.Query(ctx, `SELECT workload, instance FROM core.workload_instances
		WHERE module = $1 AND last_seen > now() - $2::interval ORDER BY instance`, module, fmt.Sprintf("%d seconds", int(liveWindow.Seconds())))
	if err == nil {
		for rows.Next() {
			var wl, inst string
			if rows.Scan(&wl, &inst) == nil {
				live[wl] = append(live[wl], inst)
			}
		}
		rows.Close()
	}
	for _, wl := range h.manifests[module].Workloads {
		st := WorkloadStatus{Workload: wl, Live: len(live[wl.Name]), Instances: live[wl.Name]}
		if wl.Kind == modkit.Service && wl.Required && st.Live == 0 {
			out.Status = "degraded"
			out.Reasons = append(out.Reasons, "no "+wl.Name+" connected")
		}
		out.Workloads = append(out.Workloads, st)
	}
	if hr, ok := h.mods[module].(modkit.HealthReporter); ok {
		if rt, err := h.runtime(ctx, module, h.current()); err == nil {
			out.Detail = hr.Health(ctx, rt)
		}
	}
	return out
}
