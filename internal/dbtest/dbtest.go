// Package dbtest provides a real-Postgres test database helper that isolates
// each Go package in its own migrated database (gl_test_<package>). Not a
// _test.go file so it can be imported as a normal package dependency from each
// package's own _test.go files.
package dbtest

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/migrate"
)

const (
	packageDBPrefix = "gl_test_"
	// maxPostgresIdentLen is NAMEDATALEN-1 in standard PostgreSQL builds.
	maxPostgresIdentLen = 63
)

var (
	pkgDBMu     sync.Mutex
	migratedDBs = map[string]bool{}
)

// PackageDBName derives a deterministic PostgreSQL database identifier
// (gl_test_<package>) from a caller source file path.
//
// Isolating test databases per package removes cross-package table contention
// when `go test ./...` runs packages in parallel (for example,
// internal/founding truncating settlements while internal/payout or
// internal/handlers reads and writes settlement rows).
func PackageDBName(callerFile string) string {
	slash := filepath.ToSlash(callerFile)
	dir := path.Dir(slash)

	var rel string
	switch {
	case strings.Contains(dir, "/internal/"):
		_, after, _ := strings.Cut(dir, "/internal/")
		rel = after
	case strings.HasPrefix(dir, "internal/"):
		rel = strings.TrimPrefix(dir, "internal/")
	case strings.Contains(dir, "/cmd/"):
		_, after, _ := strings.Cut(dir, "/cmd/")
		rel = "cmd_" + after
	case strings.HasPrefix(dir, "cmd/"):
		rel = "cmd_" + strings.TrimPrefix(dir, "cmd/")
	default:
		rel = path.Base(dir)
	}

	var b strings.Builder
	lastUnderscore := false
	for _, r := range strings.ToLower(rel) {
		isAlphaNum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if isAlphaNum {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore && b.Len() > 0 {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	slug := strings.Trim(b.String(), "_")
	if slug == "" {
		slug = "default"
	}

	name := packageDBPrefix + slug
	if len(name) > maxPostgresIdentLen {
		name = strings.TrimRight(name[:maxPostgresIdentLen], "_")
	}
	return name
}

func callerPackageDBName() string {
	for skip := 1; skip < 16; skip++ {
		_, file, _, ok := runtime.Caller(skip)
		if !ok {
			break
		}
		slash := filepath.ToSlash(file)
		// Skip frames inside internal/dbtest itself unless the caller is a
		// test within internal/dbtest (*_test.go).
		if (strings.Contains(slash, "/internal/dbtest/") || strings.HasPrefix(slash, "internal/dbtest/")) &&
			!strings.HasSuffix(slash, "_test.go") {
			continue
		}
		return PackageDBName(file)
	}
	return packageDBPrefix + "default"
}

func ensurePackageDB(t *testing.T, baseDSN, dbName string) *pgxpool.Config {
	t.Helper()
	ctx := context.Background()

	pkgCfg, err := pgxpool.ParseConfig(baseDSN)
	if err != nil {
		t.Fatalf("parse TEST_DB_URL: %v", err)
	}
	pkgCfg.ConnConfig.Database = dbName

	cacheKey := baseDSN + "\x00" + dbName

	pkgDBMu.Lock()
	defer pkgDBMu.Unlock()

	if migratedDBs[cacheKey] {
		return pkgCfg
	}

	adminCfg, err := pgxpool.ParseConfig(baseDSN)
	if err != nil {
		t.Fatalf("parse TEST_DB_URL: %v", err)
	}
	adminPool, err := pgxpool.NewWithConfig(ctx, adminCfg)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	var exists bool
	if err := adminPool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, dbName).Scan(&exists); err != nil {
		// If the database named in TEST_DB_URL does not exist yet on the server,
		// fall back to the maintenance "postgres" database to create gl_test_<package>.
		adminPool.Close()
		adminCfg.ConnConfig.Database = "postgres"
		adminPool, err = pgxpool.NewWithConfig(ctx, adminCfg)
		if err != nil {
			t.Fatalf("pgxpool.New(postgres): %v", err)
		}
		if err := adminPool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, dbName).Scan(&exists); err != nil {
			adminPool.Close()
			t.Fatalf("check database %s: %v", dbName, err)
		}
	}

	if !exists {
		if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "42P04" {
				adminPool.Close()
				t.Fatalf("create database %s: %v", dbName, err)
			}
		}
	}
	adminPool.Close()

	migratePool, err := pgxpool.NewWithConfig(ctx, pkgCfg)
	if err != nil {
		t.Fatalf("pgxpool.New(%s): %v", dbName, err)
	}
	defer migratePool.Close()

	if err := migrate.Up(ctx, migratePool); err != nil {
		t.Fatalf("migrate.Up(%s): %v", dbName, err)
	}

	migratedDBs[cacheKey] = true
	return pkgCfg
}

// DB returns a db.DB backed by an isolated per-package database
// (gl_test_<package>) derived from TEST_DB_URL and migrated to the current
// schema on first use. Tests that need a real Postgres call this first and get
// skipped (not failed) when TEST_DB_URL isn't set, so `go test ./...` stays
// green on a machine with no local Postgres running.
func DB(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DB_URL")
	if dsn == "" {
		t.Skip("TEST_DB_URL not set; skipping test that needs a live database")
	}

	dbName := callerPackageDBName()
	pkgCfg := ensurePackageDB(t, dsn, dbName)

	pool, err := pgxpool.NewWithConfig(context.Background(), pkgCfg)
	if err != nil {
		t.Fatalf("pgxpool.New(%s): %v", dbName, err)
	}
	t.Cleanup(pool.Close)

	return &db.DB{Pool: pool}
}

// FreshDB is DB, but in a database of its own, created for this test and
// dropped after it. For tests whose subject reads the whole of a table - a
// queue worker claiming "any pending job" - and so would act on rows other
// tests left in the package database.
func FreshDB(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DB_URL")
	if dsn == "" {
		t.Skip("TEST_DB_URL not set; skipping test that needs a live database")
	}
	ctx := context.Background()

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse TEST_DB_URL: %v", err)
	}
	admin, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer admin.Close()

	name := "fresh_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}

	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pgxpool.New(%s): %v", name, err)
	}
	t.Cleanup(func() {
		pool.Close()
		if a, err := pgxpool.New(context.Background(), dsn); err == nil {
			_, _ = a.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			a.Close()
		}
	})
	if err := migrate.Up(ctx, pool); err != nil {
		t.Fatalf("migrate.Up: %v", err)
	}
	return &db.DB{Pool: pool}
}
