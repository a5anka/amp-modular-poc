package modhost_test

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/a5anka/amp-modular-poc/internal/core"
	"github.com/a5anka/amp-modular-poc/internal/db"
	"github.com/a5anka/amp-modular-poc/internal/modhost"
	"github.com/a5anka/amp-modular-poc/modkit"
)

// Test modules, registered only in this test binary.

type testModule struct {
	manifest   modkit.Manifest
	migrations fs.FS
	activate   func(rt modkit.Runtime)
}

func (m *testModule) Manifest() modkit.Manifest { return m.manifest }
func (m *testModule) Migrations() fs.FS         { return m.migrations }
func (m *testModule) UI() fs.FS                 { return nil }
func (m *testModule) Activate(_ context.Context, rt modkit.Runtime) error {
	if m.activate != nil {
		m.activate(rt)
	}
	return nil
}

func sqlFS(files map[string]string) fs.FS {
	m := fstest.MapFS{}
	for k, v := range files {
		m[k] = &fstest.MapFile{Data: []byte(v)}
	}
	return m
}

// upgradable stands in for "the same module in the next release of the binary".
var upgradable = &testModule{
	manifest:   modkit.Manifest{Name: "t-upgrade", Version: "1.0.0", CoreAPI: modkit.CoreAPIVersion},
	migrations: sqlFS(map[string]string{"0001.sql": "CREATE TABLE notes (body text); INSERT INTO notes VALUES ('kept');"}),
}

func init() {
	modkit.Register(upgradable)
	modkit.Register(&testModule{
		manifest: modkit.Manifest{Name: "t-store", Version: "1.0.0", CoreAPI: modkit.CoreAPIVersion, Provides: []string{"store/v1"}},
		migrations: sqlFS(map[string]string{
			"0001.sql": "CREATE TABLE items (id int PRIMARY KEY); INSERT INTO items VALUES (1), (2);",
		}),
		activate: func(rt modkit.Runtime) {
			rt.Provide("store/v1", "the store")
			rt.Handle("GET", "/items", "", func(w http.ResponseWriter, r *http.Request) {
				var n int
				if err := rt.DB().QueryRow(r.Context(), "SELECT count(*) FROM items").Scan(&n); err != nil {
					modkit.WriteError(w, 500, err.Error())
					return
				}
				modkit.WriteJSON(w, 200, map[string]int{"count": n})
			})
		},
	})
	modkit.Register(&testModule{
		manifest:   modkit.Manifest{Name: "t-consumer", Version: "1.0.0", CoreAPI: modkit.CoreAPIVersion, Requires: []string{"store/v1"}},
		migrations: sqlFS(map[string]string{"0001.sql": "CREATE TABLE secrets (v text); INSERT INTO secrets VALUES ('mine');"}),
	})
}

// broken is not registered: a binary containing it could never migrate.
// TestFailingMigrationAppliesNothing builds a host with it explicitly.
var broken = &testModule{
	manifest: modkit.Manifest{Name: "t-broken", Version: "1.0.0", CoreAPI: modkit.CoreAPIVersion},
	migrations: sqlFS(map[string]string{
		"0001.sql": "CREATE TABLE ok (id int);",
		"0002.sql": "CREATE TABLE oops (id int REFERENCES nowhere (id));",
	}),
}

func startPostgres(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: needs an embedded Postgres")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	dir := t.TempDir()
	cache := os.Getenv("AMP_TEST_PG_CACHE")
	if cache == "" {
		home, _ := os.UserHomeDir()
		cache = filepath.Join(home, ".amp", "test-cache")
	}
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V17).Port(uint32(port)).
		DataPath(filepath.Join(dir, "data")).RuntimePath(filepath.Join(dir, "rt")).
		BinariesPath(filepath.Join(dir, "bin")).CachePath(cache).Logger(io.Discard))
	if err := pg.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.Stop() })
	return fmt.Sprintf("postgres://postgres:postgres@localhost:%d/postgres?sslmode=disable", port)
}

func newHost(t *testing.T, ctx context.Context, dsn string) (*modhost.Host, *pgxpool.Pool) {
	t.Helper()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	h := modhost.New(pool, dsn, core.NewAgents(pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	// Same as `amp serve` with the default --migrate=true.
	if _, err := h.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	return h, pool
}

func schemaExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema string) bool {
	var ok bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, schema).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestModuleLifecycle(t *testing.T) {
	dsn := startPostgres(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h1, pool := newHost(t, ctx, dsn)
	h2, _ := newHost(t, ctx, dsn) // a second replica on the same database

	if _, err := pool.Exec(ctx, `INSERT INTO core.agents (name) VALUES ('a1')`); err != nil {
		t.Fatal(err)
	}

	migrationRows := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM core.module_migrations`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("migrate prepares every compiled-in module before any install", func(t *testing.T) {
		for _, s := range []string{"mod_t_store", "mod_t_consumer", "mod_t_upgrade"} {
			if !schemaExists(t, ctx, pool, s) {
				t.Fatalf("schema %s should exist after migrate", s)
			}
		}
		var n int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM core.modules`).Scan(&n)
		if n != 0 {
			t.Fatalf("migrate must not install anything, %d module rows", n)
		}
		rec := httptest.NewRecorder()
		h1.ServeModule(rec, httptest.NewRequest("GET", "/m/t-store/items", nil))
		if rec.Code != 404 {
			t.Fatalf("a migrated but not installed module must not serve, got %d", rec.Code)
		}
	})

	t.Run("required capability must be present", func(t *testing.T) {
		_, err := h1.Install(ctx, "t-consumer", false)
		if !modhost.IsResolveError(err) || !strings.Contains(err.Error(), "store/v1") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("install with deps activates and mounts routes, with no migrations", func(t *testing.T) {
		before := migrationRows()
		steps, err := h1.Install(ctx, "t-consumer", true)
		if err != nil {
			t.Fatal(err)
		}
		if len(steps) != 2 || steps[0].Module != "t-store" {
			t.Fatalf("steps %v", steps)
		}
		for _, s := range []string{"mod_t_store", "mod_t_consumer"} {
			if !schemaExists(t, ctx, pool, s) {
				t.Fatalf("schema %s missing", s)
			}
		}
		if after := migrationRows(); after != before {
			t.Fatalf("install applied migrations: %d -> %d rows", before, after)
		}
		rec := httptest.NewRecorder()
		h1.ServeModule(rec, httptest.NewRequest("GET", "/m/t-store/items", nil))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"count":2`) {
			t.Fatalf("route: %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("other replica picks up the change without a restart", func(t *testing.T) {
		deadline := time.Now().Add(5 * time.Second)
		for h2.Generation() < h1.Generation() {
			if time.Now().After(deadline) {
				t.Fatalf("replica 2 stuck at generation %d, replica 1 at %d", h2.Generation(), h1.Generation())
			}
			time.Sleep(50 * time.Millisecond)
		}
		rec := httptest.NewRecorder()
		h2.ServeModule(rec, httptest.NewRequest("GET", "/m/t-store/items", nil))
		if rec.Code != 200 {
			t.Fatalf("replica 2 route: %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("replicas poll: a change with no notification still propagates", func(t *testing.T) {
		// Change the module state straight in the database, the way a restore
		// or an operator script would. Nothing tells the replicas; polling must.
		set := func(state string) {
			t.Helper()
			if _, err := pool.Exec(ctx, `UPDATE core.modules SET state = $1 WHERE name = 't-store'`, state); err != nil {
				t.Fatal(err)
			}
			var g int64
			if err := pool.QueryRow(ctx, `UPDATE core.generation SET value = value + 1 RETURNING value`).Scan(&g); err != nil {
				t.Fatal(err)
			}
			for _, h := range []*modhost.Host{h1, h2} {
				deadline := time.Now().Add(5 * time.Second)
				for h.Generation() < g {
					if time.Now().After(deadline) {
						t.Fatalf("replica stuck at generation %d, database at %d", h.Generation(), g)
					}
					time.Sleep(50 * time.Millisecond)
				}
			}
		}
		code := func(h *modhost.Host) int {
			rec := httptest.NewRecorder()
			h.ServeModule(rec, httptest.NewRequest("GET", "/m/t-store/items", nil))
			return rec.Code
		}
		set("disabled")
		if c1, c2 := code(h1), code(h2); c1 != 404 || c2 != 404 {
			t.Fatalf("disabled module still served: replica1=%d replica2=%d", c1, c2)
		}
		set("active")
		if c1, c2 := code(h1), code(h2); c1 != 200 || c2 != 200 {
			t.Fatalf("re-enabled module not served: replica1=%d replica2=%d", c1, c2)
		}
	})

	t.Run("module role sees its schema and core_api only", func(t *testing.T) {
		mp, err := db.ModulePool(ctx, dsn, "t-store")
		if err != nil {
			t.Fatal(err)
		}
		defer mp.Close()
		var n int
		if err := mp.QueryRow(ctx, `SELECT count(*) FROM core_api.agents`).Scan(&n); err != nil || n != 1 {
			t.Fatalf("core_api.agents: n=%d err=%v", n, err)
		}
		if err := mp.QueryRow(ctx, `SELECT count(*) FROM core.agents`).Scan(&n); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("core.agents must be denied, got %v", err)
		}
		if err := mp.QueryRow(ctx, `SELECT count(*) FROM mod_t_consumer.secrets`).Scan(&n); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("another module's schema must be denied, got %v", err)
		}
	})

	t.Run("a provider cannot go while a consumer needs it", func(t *testing.T) {
		if err := h1.Disable(ctx, "t-store"); !modhost.IsResolveError(err) {
			t.Fatalf("disable: %v", err)
		}
		if err := h1.Uninstall(ctx, "t-store", true); !modhost.IsResolveError(err) {
			t.Fatalf("uninstall: %v", err)
		}
	})

	t.Run("disabled module stops serving and keeps data", func(t *testing.T) {
		if err := h1.Disable(ctx, "t-consumer"); err != nil {
			t.Fatal(err)
		}
		if err := h1.Disable(ctx, "t-store"); err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		h1.ServeModule(rec, httptest.NewRequest("GET", "/m/t-store/items", nil))
		if rec.Code != 404 {
			t.Fatalf("disabled module served %d", rec.Code)
		}
		if !schemaExists(t, ctx, pool, "mod_t_store") {
			t.Fatal("disable must keep the schema")
		}
		// Installing a disabled module with --with-deps re-enables it and its providers.
		steps, err := h1.Install(ctx, "t-consumer", true)
		if err != nil {
			t.Fatal(err)
		}
		if len(steps) != 2 || steps[0].Action != "enable" || steps[1].Action != "enable" {
			t.Fatalf("steps %v", steps)
		}
		rec = httptest.NewRecorder()
		h1.ServeModule(rec, httptest.NewRequest("GET", "/m/t-store/items", nil))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"count":2`) {
			t.Fatalf("data must survive disable/enable: %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("purge empties the tables and keeps the schema", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `INSERT INTO core.workload_credentials (module, workload, secret_hash) VALUES ('t-consumer', 'w', 'h')`); err != nil {
			t.Fatal(err)
		}
		before := migrationRows()
		if err := h1.Uninstall(ctx, "t-consumer", true); err != nil {
			t.Fatal(err)
		}
		if !schemaExists(t, ctx, pool, "mod_t_consumer") {
			t.Fatal("purge must keep the schema: it belongs to the binary, not the install")
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM mod_t_consumer.secrets`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("purge must empty the tables: n=%d err=%v", n, err)
		}
		var live int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM core.workload_credentials WHERE module = 't-consumer' AND revoked_at IS NULL`).Scan(&live)
		if live != 0 {
			t.Fatal("purge must revoke the module's workload credentials")
		}
		if after := migrationRows(); after != before {
			t.Fatalf("purge must keep the migration history: %d -> %d", before, after)
		}
		// Installing again needs no migration.
		steps, err := h1.Install(ctx, "t-consumer", false)
		if err != nil || len(steps) != 1 || steps[0].Action != "install" {
			t.Fatalf("reinstall after purge: %v %v", steps, err)
		}
	})
}

func TestFailingMigrationAppliesNothing(t *testing.T) {
	dsn := startPostgres(t)
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	h := modhost.NewWith(pool, dsn, core.NewAgents(pool), slog.New(slog.NewTextHandler(io.Discard, nil)), []modkit.Module{broken})
	if _, err := h.Migrate(ctx); err == nil {
		t.Fatal("expected migration failure")
	}
	if schemaExists(t, ctx, pool, "mod_t_broken") {
		t.Fatal("a module whose migration failed must leave no schema behind")
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM core.module_migrations WHERE module = 't-broken'`).Scan(&n)
	if n != 0 {
		t.Fatalf("no migration of the failed module may be recorded, got %d", n)
	}
	// A binary whose schema is not migrated refuses to serve.
	if err := h.Boot(ctx); err == nil || !strings.Contains(err.Error(), "amp migrate") {
		t.Fatalf("Boot must refuse an unmigrated schema, got %v", err)
	}
}

func TestUpgradeOnBoot(t *testing.T) {
	// The test mutates the registered module to simulate a new release; reset it so -count=N works.
	upgradable.manifest.Version = "1.0.0"
	upgradable.migrations = sqlFS(map[string]string{"0001.sql": "CREATE TABLE notes (body text); INSERT INTO notes VALUES ('kept');"})
	dsn := startPostgres(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, pool := newHost(t, ctx, dsn)
	if _, err := h.Install(ctx, "t-upgrade", false); err != nil {
		t.Fatal(err)
	}

	// Ship v1.1.0: same module, one more migration (expand: add a column).
	upgradable.manifest.Version = "1.1.0"
	upgradable.migrations = sqlFS(map[string]string{
		"0001.sql": "CREATE TABLE notes (body text); INSERT INTO notes VALUES ('kept');",
		"0002.sql": "ALTER TABLE notes ADD COLUMN author text NOT NULL DEFAULT 'unknown';",
	})
	// A replica of the new binary that skips migrate (serve --migrate=false,
	// before the migrate Job ran) refuses to start.
	p1, _ := db.Connect(ctx, dsn)
	defer p1.Close()
	stale := modhost.New(p1, dsn, core.NewAgents(p1), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := stale.Boot(ctx); err == nil || !strings.Contains(err.Error(), "t-upgrade: 1") {
		t.Fatalf("want schema-behind error, got %v", err)
	}
	newHost(t, ctx, dsn) // the migrate step runs, then a replica of the new binary boots

	var v, author, body string
	if err := pool.QueryRow(ctx, `SELECT version FROM core.modules WHERE name = 't-upgrade'`).Scan(&v); err != nil || v != "1.1.0" {
		t.Fatalf("version %q, %v", v, err)
	}
	if err := pool.QueryRow(ctx, `SELECT body, author FROM mod_t_upgrade.notes`).Scan(&body, &author); err != nil || body != "kept" || author != "unknown" {
		t.Fatalf("got %q %q %v", body, author, err)
	}

	// Editing an applied migration is refused.
	upgradable.migrations = sqlFS(map[string]string{
		"0001.sql": "CREATE TABLE notes (body text);",
		"0002.sql": "ALTER TABLE notes ADD COLUMN author text NOT NULL DEFAULT 'unknown';",
	})
	p2, _ := db.Connect(ctx, dsn)
	defer p2.Close()
	h3 := modhost.New(p2, dsn, core.NewAgents(p2), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := h3.Boot(ctx); err == nil || !strings.Contains(err.Error(), "changed after it was applied") {
		t.Fatalf("want checksum error, got %v", err)
	}
}
