// Package cli is the amp command line: `amp serve` runs the core, and
// `amp module ...` manages modules on a running core through its API.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"

	"github.com/a5anka/amp-modular-poc/internal/core"
	"github.com/a5anka/amp-modular-poc/internal/db"
	"github.com/a5anka/amp-modular-poc/internal/modhost"
	"github.com/a5anka/amp-modular-poc/modkit"
	"github.com/a5anka/amp-modular-poc/web"
)

// Main runs the CLI. distribution names the build (which modules it links).
func Main(distribution string) {
	if len(os.Args) < 2 {
		usage(distribution)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "module", "modules", "mod":
		err = moduleCmd(os.Args[2:])
	case "version":
		fmt.Printf("%s, core API v%d, modules compiled in:", distribution, modkit.CoreAPIVersion)
		for _, m := range modkit.Registered() {
			fmt.Printf(" %s@%s", m.Manifest().Name, m.Manifest().Version)
		}
		fmt.Println()
	default:
		usage(distribution)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage(dist string) {
	fmt.Fprintf(os.Stderr, `%s

Usage:
  amp serve [--addr :8080] [--db postgres://...] [--embedded-db] [--replica name]
  amp module list
  amp module info <name>
  amp module install <name> [--with-deps]
  amp module disable <name>
  amp module enable <name>
  amp module uninstall <name> [--purge]
  amp version

Module commands talk to a running core: --server (or AMP_SERVER), default http://localhost:8080.
`, dist)
}

// ---- serve ----

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", envOr("AMP_ADDR", ":8080"), "listen address")
	dsn := fs.String("db", os.Getenv("AMP_DB_URL"), "Postgres URL")
	embedded := fs.Bool("embedded-db", false, "start a local Postgres for development (no Docker needed)")
	dataDir := fs.String("data-dir", envOr("AMP_DATA_DIR", defaultDataDir()), "embedded Postgres data directory")
	replica := fs.String("replica", os.Getenv("AMP_REPLICA"), "replica name shown in logs and the console")
	_ = fs.Parse(args)

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *replica == "" {
		h, _ := os.Hostname()
		*replica = h + *addr
	}
	if *embedded {
		pg, url, err := startEmbedded(*dataDir, log)
		if err != nil {
			return err
		}
		defer pg.Stop()
		*dsn = url
	}
	if *dsn == "" {
		return fmt.Errorf("set --db (or AMP_DB_URL), or use --embedded-db")
	}

	pool, err := db.Connect(ctx, *dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := waitForDB(ctx, pool.Ping); err != nil {
		return err
	}
	if err := db.Bootstrap(ctx, pool); err != nil {
		return fmt.Errorf("core migrations: %w", err)
	}
	agents := core.NewAgents(pool)
	host := modhost.New(pool, *dsn, agents, log)
	if err := host.Boot(ctx); err != nil {
		return fmt.Errorf("module host: %w", err)
	}

	srv := &http.Server{Addr: *addr, Handler: core.NewServer(host, agents, web.FS(), log, *replica).Handler(),
		ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(c)
	}()
	var names []string
	for _, m := range modkit.Registered() {
		names = append(names, m.Manifest().Name)
	}
	log.Info("amp core listening", "addr", *addr, "replica", *replica, "compiledIn", names)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func startEmbedded(dir string, log *slog.Logger) (*embeddedpostgres.EmbeddedPostgres, string, error) {
	const port = 5439
	if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond); err == nil {
		c.Close()
		return nil, "", fmt.Errorf("port %d is in use: another replica already runs the embedded database; start this one with --db %s", port, embeddedURL(port))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", err
	}
	log.Info("starting embedded Postgres (first run downloads the binaries)", "dir", dir, "port", port)
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V17).Port(port).
		DataPath(filepath.Join(dir, "data")).RuntimePath(filepath.Join(dir, "runtime")).
		BinariesPath(filepath.Join(dir, "bin")).CachePath(filepath.Join(dir, "cache")).
		Logger(io.Discard))
	if err := pg.Start(); err != nil {
		return nil, "", fmt.Errorf("embedded postgres: %w", err)
	}
	return pg, embeddedURL(port), nil
}

func embeddedURL(port int) string {
	return fmt.Sprintf("postgres://postgres:postgres@localhost:%d/postgres?sslmode=disable", port)
}

func defaultDataDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ".amp"
	}
	return filepath.Join(h, ".amp")
}

func waitForDB(ctx context.Context, ping func(context.Context) error) error {
	var err error
	for i := 0; i < 30; i++ {
		if err = ping(ctx); err == nil {
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("database not reachable: %w", err)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ---- module commands ----

func moduleCmd(args []string) error {
	fs := flag.NewFlagSet("module", flag.ExitOnError)
	server := fs.String("server", envOr("AMP_SERVER", "http://localhost:8080"), "core URL")
	role := fs.String("role", envOr("AMP_ROLE", "admin"), "role to act as")
	withDeps := fs.Bool("with-deps", false, "also install modules that provide missing required capabilities")
	purge := fs.Bool("purge", false, "uninstall: drop the module schema and its data")
	if len(args) == 0 {
		return fmt.Errorf("module: missing subcommand (list, info, install, enable, disable, uninstall)")
	}
	sub := args[0]
	// Accept flags before or after the module name.
	var pos []string
	rest := args[1:]
	for len(rest) > 0 {
		_ = fs.Parse(rest)
		rest = fs.Args()
		if len(rest) > 0 {
			pos = append(pos, rest[0])
			rest = rest[1:]
		}
	}
	c := &client{base: strings.TrimRight(*server, "/"), role: *role}
	needName := func() (string, error) {
		if len(pos) != 1 {
			return "", fmt.Errorf("module %s: expected one module name", sub)
		}
		return pos[0], nil
	}
	switch sub {
	case "list", "ls":
		var mods []modhost.ModuleInfo
		if err := c.do("GET", "/api/modules", nil, &mods); err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tVERSION\tSTATE\tPROVIDES\tREQUIRES\tREQUIRED BY")
		for _, m := range mods {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", m.Name, m.Version, m.State,
				dash(m.Provides), dash(m.Requires), dash(m.RequiredBy))
		}
		return tw.Flush()
	case "info":
		name, err := needName()
		if err != nil {
			return err
		}
		var m json.RawMessage
		if err := c.do("GET", "/api/modules/"+name, nil, &m); err != nil {
			return err
		}
		var out bytes.Buffer
		_ = json.Indent(&out, m, "", "  ")
		fmt.Println(out.String())
		return nil
	case "install":
		name, err := needName()
		if err != nil {
			return err
		}
		var res struct {
			Steps      []modhost.Step `json:"steps"`
			Generation int64          `json:"generation"`
		}
		q := ""
		if *withDeps {
			q = "?withDeps=true"
		}
		start := time.Now()
		if err := c.do("POST", "/api/modules/"+name+"/install"+q, nil, &res); err != nil {
			return err
		}
		for _, s := range res.Steps {
			var m modhost.ModuleInfo
			_ = c.do("GET", "/api/modules/"+s.Module, nil, &m)
			fmt.Printf("✔ %s %s@%s: schema %s, %d migration(s), %d route(s), %d permission(s), %d nav item(s), %d slot(s)\n",
				s.Action, m.Name, m.Version, m.Schema, len(m.Migrations), m.Routes, len(m.Permissions), navCount(m), slotCount(m))
		}
		fmt.Printf("active on all replicas at generation %d (%s); open consoles update without a reload\n",
			res.Generation, time.Since(start).Round(time.Millisecond))
		return nil
	case "enable", "disable":
		name, err := needName()
		if err != nil {
			return err
		}
		if err := c.do("POST", "/api/modules/"+name+"/"+sub, nil, nil); err != nil {
			return err
		}
		fmt.Printf("✔ %sd %s\n", sub, name)
		return nil
	case "uninstall", "remove":
		name, err := needName()
		if err != nil {
			return err
		}
		q := ""
		if *purge {
			q = "?purge=true"
		}
		if err := c.do("DELETE", "/api/modules/"+name+q, nil, nil); err != nil {
			return err
		}
		if *purge {
			fmt.Printf("✔ uninstalled %s and dropped schema %s\n", name, db.SchemaName(name))
		} else {
			fmt.Printf("✔ uninstalled %s (schema %s kept; reinstall resumes with its data, --purge drops it)\n", name, db.SchemaName(name))
		}
		return nil
	}
	return fmt.Errorf("module: unknown subcommand %q", sub)
}

func navCount(m modhost.ModuleInfo) int {
	if m.UI == nil {
		return 0
	}
	return len(m.UI.Nav)
}

func slotCount(m modhost.ModuleInfo) int {
	if m.UI == nil {
		return 0
	}
	return len(m.UI.Slots)
}

func dash(xs []string) string {
	if len(xs) == 0 {
		return "-"
	}
	return strings.Join(xs, ",")
}

type client struct{ base, role string }

func (c *client) do(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("X-AMP-Role", c.role)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach the core at %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
			Hint  string `json:"hint"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			if e.Hint != "" {
				return fmt.Errorf("%s\n  hint: %s", e.Error, e.Hint)
			}
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}
