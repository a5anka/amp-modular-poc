// Package db holds the database plumbing shared by the core and the module host:
// connection pools, the per-module role and schema, and the SQL migrator.
package db

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// lockKey is the advisory lock that serialises migrations (and signing-key
// creation) across replicas started at the same time. Module lifecycle
// changes do not use it: they run no DDL and lock the generation row instead.
const lockKey int64 = 0x616d70 // "amp"

//go:embed migrations/*.sql
var coreMigrations embed.FS

// Connect opens the core pool.
func Connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	return pgxpool.NewWithConfig(ctx, cfg)
}

// SchemaName is the schema that holds a module's tables.
func SchemaName(module string) string { return "mod_" + strings.ReplaceAll(module, "-", "_") }

// RoleName is the database role a module's queries run as.
func RoleName(module string) string { return "amp_mod_" + strings.ReplaceAll(module, "-", "_") }

// ModulePool opens a pool whose sessions run as the module role, with the
// module schema on the search path. The core_api views stay readable; core
// tables and other modules' schemas are not.
//
// This is a guard rail for first-party code, not a security boundary: a
// session can RESET ROLE. Untrusted code would need its own login role.
func ModulePool(ctx context.Context, dsn, module string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 4
	role, schema := pgx.Identifier{RoleName(module)}.Sanitize(), pgx.Identifier{SchemaName(module)}.Sanitize()
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, fmt.Sprintf("SET ROLE %s; SET search_path = %s", role, schema))
		return err
	}
	return pgxpool.NewWithConfig(ctx, cfg)
}

// Lock takes the cluster-wide lifecycle lock for the rest of the transaction.
func Lock(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", lockKey)
	return err
}

// Bootstrap applies the core's own migrations. Every replica calls it at
// start-up; the advisory lock makes sure only one of them does the work.
func Bootstrap(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := Lock(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		CREATE SCHEMA IF NOT EXISTS core;
		CREATE TABLE IF NOT EXISTS core.module_migrations (
			module     text NOT NULL,
			file       text NOT NULL,
			checksum   text NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now(),
			PRIMARY KEY (module, file)
		)`); err != nil {
		return err
	}
	sub, _ := fs.Sub(coreMigrations, "migrations")
	ms, err := Load(sub)
	if err != nil {
		return err
	}
	pending, err := Pending(ctx, tx, "core", ms)
	if err != nil {
		return err
	}
	if err := Apply(ctx, tx, "core", pending, ""); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Querier is a pool or a transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// CorePending reports how many of the core's own migrations are not applied.
func CorePending(ctx context.Context, q Querier) (int, error) {
	sub, _ := fs.Sub(coreMigrations, "migrations")
	ms, err := Load(sub)
	if err != nil {
		return 0, err
	}
	var exists bool
	if err := q.QueryRow(ctx, `SELECT to_regclass('core.module_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return 0, err
	}
	if !exists {
		return len(ms), nil
	}
	p, err := Pending(ctx, q, "core", ms)
	return len(p), err
}

// Migration is one SQL file.
type Migration struct {
	File     string
	SQL      string
	Checksum string
}

// Load reads *.sql files from fsys in lexical order.
func Load(fsys fs.FS) ([]Migration, error) {
	if fsys == nil {
		return nil, nil
	}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	var out []Migration
	for _, e := range entries {
		if e.IsDir() || path.Ext(e.Name()) != ".sql" {
			continue
		}
		b, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		out = append(out, Migration{File: e.Name(), SQL: string(b), Checksum: hex.EncodeToString(sum[:8])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out, nil
}

// Pending returns the migrations not applied yet. An applied file whose
// content changed is an error: migrations are append-only.
func Pending(ctx context.Context, q Querier, module string, ms []Migration) ([]Migration, error) {
	rows, err := q.Query(ctx, `SELECT file, checksum FROM core.module_migrations WHERE module = $1`, module)
	if err != nil {
		return nil, err
	}
	applied, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) ([2]string, error) {
		var f, c string
		err := r.Scan(&f, &c)
		return [2]string{f, c}, err
	})
	if err != nil {
		return nil, err
	}
	done := map[string]string{}
	for _, a := range applied {
		done[a[0]] = a[1]
	}
	var out []Migration
	for _, m := range ms {
		sum, ok := done[m.File]
		switch {
		case !ok:
			out = append(out, m)
		case sum != m.Checksum:
			return nil, fmt.Errorf("%s: migration %s was changed after it was applied", module, m.File)
		}
	}
	return out, nil
}

// Apply runs migrations inside tx. For a module, role is the module role:
// the SQL runs as that role with search_path set to the module schema, so
// every object it creates lands in the module schema and is owned by the
// module role. Postgres DDL is transactional, so a failing migration rolls
// back the whole install.
func Apply(ctx context.Context, tx pgx.Tx, module string, ms []Migration, role string) error {
	if len(ms) == 0 {
		return nil
	}
	if role != "" {
		if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL ROLE %s; SET LOCAL search_path = %s",
			pgx.Identifier{role}.Sanitize(), pgx.Identifier{SchemaName(module)}.Sanitize())); err != nil {
			return err
		}
	}
	for _, m := range ms {
		if _, err := tx.Exec(ctx, m.SQL); err != nil {
			return fmt.Errorf("%s: migration %s: %w", module, m.File, err)
		}
	}
	if role != "" {
		if _, err := tx.Exec(ctx, "RESET ROLE; SET LOCAL search_path = DEFAULT"); err != nil {
			return err
		}
	}
	for _, m := range ms {
		if _, err := tx.Exec(ctx, `INSERT INTO core.module_migrations (module, file, checksum) VALUES ($1, $2, $3)`,
			module, m.File, m.Checksum); err != nil {
			return err
		}
	}
	return nil
}

// EnsureModuleSchema creates the module role and schema if they do not exist
// and grants the role read access to the core_api views.
func EnsureModuleSchema(ctx context.Context, tx pgx.Tx, module string) error {
	role := pgx.Identifier{RoleName(module)}.Sanitize()
	schema := pgx.Identifier{SchemaName(module)}.Sanitize()
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, RoleName(module)).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err := tx.Exec(ctx, "CREATE ROLE "+role+" NOLOGIN"); err != nil {
			return fmt.Errorf("create role %s (the core's DB user needs CREATEROLE): %w", role, err)
		}
	}
	stmts := []string{
		// Lets the core's login role SET ROLE to the module role.
		"GRANT " + role + " TO CURRENT_USER",
		"CREATE SCHEMA IF NOT EXISTS " + schema + " AUTHORIZATION " + role,
		"GRANT USAGE ON SCHEMA core_api TO " + role,
		"GRANT SELECT ON ALL TABLES IN SCHEMA core_api TO " + role,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	return nil
}

// PurgeModuleData empties every table in a module's schema and revokes the
// module's workload credentials. It runs no DDL that changes the schema: the
// tables, the schema and the role stay, as created by Migrate, so the module
// can be installed again without migrating. TRUNCATE runs as the module role,
// which owns the tables.
func PurgeModuleData(ctx context.Context, tx pgx.Tx, module string) error {
	rows, err := tx.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname = $1 ORDER BY tablename`, SchemaName(module))
	if err != nil {
		return err
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	if len(tables) > 0 {
		var ids []string
		for _, t := range tables {
			ids = append(ids, pgx.Identifier{SchemaName(module), t}.Sanitize())
		}
		stmts := []string{
			"SET LOCAL ROLE " + pgx.Identifier{RoleName(module)}.Sanitize(),
			"TRUNCATE " + strings.Join(ids, ", ") + " RESTART IDENTITY",
			"RESET ROLE",
		}
		for _, st := range stmts {
			if _, err := tx.Exec(ctx, st); err != nil {
				return fmt.Errorf("%s: %w", st, err)
			}
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE core.workload_credentials SET revoked_at = now() WHERE module = $1 AND revoked_at IS NULL`, module); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `DELETE FROM core.workload_instances WHERE module = $1`, module)
	return err
}
