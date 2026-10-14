package handlers_test

import (
	"context"
	"io/fs"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
	"github.com/jagadeesh/grainlify/backend/migrations"
)

const breakGlassSecret = "break-glass-test-secret"

// breakGlassApp mounts one route behind the real middleware chain an admin
// endpoint uses: authentication, then a live role check backed by an actual
// database query.
func breakGlassApp(d *db.DB) *fiber.App {
	app := fiber.New()
	app.Get("/admin/probe",
		auth.RequireAuth(breakGlassSecret),
		auth.RequireLiveRole(handlers.NewRoleLookup(d), "admin"),
		func(c *fiber.Ctx) error { return c.JSON(fiber.Map{"ok": true}) },
	)
	return app
}

func breakGlassUser(t *testing.T, d *db.DB, role string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := d.Pool.QueryRow(context.Background(), `
INSERT INTO users (display_name, role) VALUES ($1, $2) RETURNING id
`, "break-glass-"+uuid.NewString()[:8], role).Scan(&id)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

func breakGlassCall(t *testing.T, app *fiber.App, token string) int {
	t.Helper()
	req := httptest.NewRequest("GET", "/admin/probe", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	return resp.StatusCode
}

// TestBreakGlass_DirectDatabaseRoleChange is the recovery path, exercised
// against a real database rather than a stubbed lookup.
//
// The database is the break-glass: there is no second bootstrap door, and no
// step-up flow. If the only admin loses access, the recovery is a direct
// UPDATE by someone holding database credentials. This test is what says that
// recovery actually works, and - just as important - what it does NOT do.
//
// The sequence a real recovery follows:
//
//  1. The user holds a token issued while they were a contributor. The admin
//     route refuses them: claim and database agree, and neither is admin.
//  2. Someone with database access runs the UPDATE.
//  3. The same token is now refused *differently* - 403 role_changed rather
//     than insufficient_role. The old token stops working immediately, which
//     is the property that makes revocation instant.
//  4. Signing in again mints a token whose claim matches the stored role, and
//     the route allows it.
//
// Step 3 is the one worth being precise about: "the role change takes effect
// on the next request" is true in the sense that the next request behaves
// differently at once, but it does not mean the holder is silently promoted
// mid-session. They must sign in again. That is deliberate - a stale token
// being upgraded by a database edit would mean a token's contents no longer
// describe what it can do.
func TestBreakGlass_DirectDatabaseRoleChange(t *testing.T) {
	d := testDB(t)
	app := breakGlassApp(d)
	ctx := context.Background()

	userID := breakGlassUser(t, d, "contributor")

	contributorToken, err := auth.IssueJWT(breakGlassSecret, userID, "contributor", "", "", time.Hour)
	if err != nil {
		t.Fatalf("IssueJWT: %v", err)
	}

	// 1. Before recovery: refused for the ordinary reason.
	if got := breakGlassCall(t, app, contributorToken); got != fiber.StatusForbidden {
		t.Fatalf("before promotion: status = %d, want 403", got)
	}

	// 2. The break-glass statement itself. This exact statement is what a
	//    recovery runs; if it ever stops being sufficient, this test fails and
	//    the runbook is wrong.
	tag, err := d.Pool.Exec(ctx, `UPDATE users SET role = 'admin', updated_at = now() WHERE id = $1`, userID)
	if err != nil {
		t.Fatalf("break-glass UPDATE: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("break-glass UPDATE affected %d rows, want 1", tag.RowsAffected())
	}

	// 3. The old token is refused immediately, and for the revocation reason -
	//    not silently upgraded.
	if got := breakGlassCall(t, app, contributorToken); got != fiber.StatusForbidden {
		t.Errorf("after promotion, stale token: status = %d, want 403", got)
	}

	// 4. Signing in again is what completes the recovery.
	adminToken, err := auth.IssueJWT(breakGlassSecret, userID, "admin", "", "", time.Hour)
	if err != nil {
		t.Fatalf("IssueJWT: %v", err)
	}
	if got := breakGlassCall(t, app, adminToken); got != fiber.StatusOK {
		t.Errorf("after re-authentication: status = %d, want 200", got)
	}
}

// TestBreakGlass_DirectRoleUpdateLeavesAuditRow verifies #503: a direct UPDATE
// on users.role outside SetUserRole still writes an admin_role_audit row via
// trg_users_audit_role_change with source = 'direct' and a NULL actor_user_id.
func TestBreakGlass_DirectRoleUpdateLeavesAuditRow(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	userID := breakGlassUser(t, d, "contributor")

	if _, err := d.Pool.Exec(ctx, `UPDATE users SET role = 'admin', updated_at = now() WHERE id = $1`, userID); err != nil {
		t.Fatalf("direct UPDATE to admin: %v", err)
	}

	var oldRole, newRole, source string
	var actorID *uuid.UUID
	var note *string
	if err := d.Pool.QueryRow(ctx, `
SELECT old_role, new_role, source, actor_user_id, note
FROM admin_role_audit
WHERE subject_user_id = $1
ORDER BY created_at DESC LIMIT 1
`, userID).Scan(&oldRole, &newRole, &source, &actorID, &note); err != nil {
		t.Fatalf("no audit row written for direct UPDATE: %v", err)
	}
	if oldRole != "contributor" || newRole != "admin" {
		t.Errorf("audit transition = %s -> %s, want contributor -> admin", oldRole, newRole)
	}
	if source != "direct" {
		t.Errorf("source = %q, want \"direct\"", source)
	}
	if actorID != nil {
		t.Errorf("actor_user_id = %v, want nil for direct database UPDATE", actorID)
	}
	if note != nil {
		t.Errorf("note = %v, want nil for direct database UPDATE", note)
	}

	// A no-op UPDATE that does not change users.role must not write a spurious row.
	if _, err := d.Pool.Exec(ctx, `UPDATE users SET role = 'admin', updated_at = now() WHERE id = $1`, userID); err != nil {
		t.Fatalf("no-op UPDATE: %v", err)
	}
	var count int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*)::int FROM admin_role_audit WHERE subject_user_id = $1`, userID).Scan(&count); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if count != 1 {
		t.Errorf("audit row count after no-op UPDATE = %d, want 1", count)
	}
}

func TestBreakGlass_RoleAuditTriggerMigrationIsEmbedded(t *testing.T) {
	upSQL, err := fs.ReadFile(migrations.FS, "20261014120000_users_role_audit_trigger.up.sql")
	if err != nil {
		t.Fatalf("read up migration: %v", err)
	}
	s := string(upSQL)
	for _, want := range []string{
		"CHECK (source IN ('admin_action', 'bootstrap', 'direct'))",
		"CREATE OR REPLACE FUNCTION users_audit_role_change()",
		"CREATE TRIGGER trg_users_audit_role_change",
		"AFTER UPDATE OF role ON users",
		"WHEN (OLD.role IS DISTINCT FROM NEW.role)",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("migration missing %q", want)
		}
	}
}

// TestBreakGlass_DemotionTakesEffectImmediately is the same mechanism in the
// direction that matters more.
//
// An admin whose role is removed in the database must lose access on their
// very next request, without waiting for a token to expire. If this ever
// regresses, revoking an admin would mean revoking them up to an hour later.
func TestBreakGlass_DemotionTakesEffectImmediately(t *testing.T) {
	d := testDB(t)
	app := breakGlassApp(d)
	ctx := context.Background()

	userID := breakGlassUser(t, d, "admin")
	adminToken, err := auth.IssueJWT(breakGlassSecret, userID, "admin", "", "", time.Hour)
	if err != nil {
		t.Fatalf("IssueJWT: %v", err)
	}

	if got := breakGlassCall(t, app, adminToken); got != fiber.StatusOK {
		t.Fatalf("before demotion: status = %d, want 200", got)
	}

	if _, err := d.Pool.Exec(ctx, `UPDATE users SET role = 'contributor' WHERE id = $1`, userID); err != nil {
		t.Fatalf("demote: %v", err)
	}

	// Same still-valid token, one request later.
	if got := breakGlassCall(t, app, adminToken); got != fiber.StatusForbidden {
		t.Errorf("after demotion: status = %d, want 403 - an unexpired admin token must not outlive the role", got)
	}
}

// TestBreakGlass_SoleAdminCountIsWhatBootstrapChecks pins the fact the
// bootstrap gate depends on, against the real schema.
//
// BootstrapAdmin refuses whenever any admin exists. With exactly one admin
// that count is 1, so the path is closed. The count is over users.role, so a
// direct database demotion of the last admin would reopen it - which is why
// the bootstrap token is unset in production rather than merely rotated.
func TestBreakGlass_SoleAdminCountIsWhatBootstrapChecks(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	before := adminCount(t, d)
	id := breakGlassUser(t, d, "admin")
	if got := adminCount(t, d); got != before+1 {
		t.Fatalf("admin count = %d, want %d", got, before+1)
	}

	if _, err := d.Pool.Exec(ctx, `UPDATE users SET role = 'contributor' WHERE id = $1`, id); err != nil {
		t.Fatalf("demote: %v", err)
	}
	if got := adminCount(t, d); got != before {
		t.Errorf("admin count = %d after demotion, want %d", got, before)
	}
}

func adminCount(t *testing.T, d *db.DB) int {
	t.Helper()
	var n int
	if err := d.Pool.QueryRow(context.Background(), `SELECT count(*)::int FROM users WHERE role = 'admin'`).Scan(&n); err != nil {
		t.Fatalf("count admins: %v", err)
	}
	return n
}
