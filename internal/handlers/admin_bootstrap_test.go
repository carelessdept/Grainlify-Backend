package handlers_test

import (
	"context"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

const bootstrapSecret = "bootstrap-suite-secret"

func bootstrapApp(d *db.DB) *fiber.App {
	cfg := config.Config{JWTSecret: bootstrapSecret}
	h := handlers.NewAdminHandler(cfg, d)
	app := fiber.New()
	// No bootstrap route: it was removed. Granting a role is an attributed
	// action by an existing admin, and first-admin recovery is a database
	// write - see break_glass_test.go.
	app.Put("/admin/users/:id/role", auth.RequireAuth(bootstrapSecret),
		auth.RequireLiveRole(handlers.NewRoleLookup(d), "admin"), h.SetUserRole())
	return app
}

func bootstrapToken4(t *testing.T, userID uuid.UUID, role string) string {
	t.Helper()
	tok, err := auth.IssueJWT(bootstrapSecret, userID, role, "", "", time.Hour)
	if err != nil {
		t.Fatalf("IssueJWT: %v", err)
	}
	return tok
}

func clearAdmins(t *testing.T, d *db.DB) {
	t.Helper()
	if _, err := d.Pool.Exec(context.Background(), `UPDATE users SET role = 'contributor' WHERE role = 'admin'`); err != nil {
		t.Fatalf("clear admins: %v", err)
	}
	if _, err := d.Pool.Exec(context.Background(), `DELETE FROM admin_role_audit`); err != nil {
		t.Fatalf("clear audit: %v", err)
	}
}

func doWithBootstrapToken(t *testing.T, app *fiber.App, jwt, token string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest("POST", "/admin/bootstrap", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	if token != "" {
		req.Header.Set("X-Admin-Bootstrap-Token", token)
	}
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// TestBootstrap_GrantsTheFirstAdminAndRecordsItsOrigin. A first admin with no
// origin record is the same gap one step earlier - the one grant nobody can
// trace.
func TestSetUserRole_RecordsBeforeAndAfter(t *testing.T) {
	d := testDB(t)
	clearAdmins(t, d)
	app := bootstrapApp(d)

	actor := adminSuiteInsertUser(t, d, "admin")
	subject := adminSuiteInsertUser(t, d, "contributor")
	jwt := bootstrapToken4(t, actor, "admin")

	for i, tc := range []struct{ to, wantOld string }{
		{"admin", "contributor"}, // promotion
		{"contributor", "admin"}, // demotion matters as much
	} {
		resp, respBody := notifSuiteDo(t, app, "PUT", "/admin/users/"+subject.String()+"/role", jwt,
			[]byte(`{"role":"`+tc.to+`","reason":"test"}`))
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("status = %d, want 200 setting role to %s; body = %s",
				resp.StatusCode, tc.to, respBody)
		}

		var oldRole, newRole, source, note string
		var gotActor uuid.UUID
		if err := d.Pool.QueryRow(context.Background(), `
SELECT old_role, new_role, source, actor_user_id, COALESCE(note, '') FROM admin_role_audit
WHERE subject_user_id = $1 ORDER BY created_at DESC LIMIT 1
`, subject).Scan(&oldRole, &newRole, &source, &gotActor, &note); err != nil {
			t.Fatalf("no audit row for role change to %s: %v", tc.to, err)
		}
		if oldRole != tc.wantOld || newRole != tc.to {
			t.Errorf("audit = %s -> %s, want %s -> %s", oldRole, newRole, tc.wantOld, tc.to)
		}
		if source != "admin_action" || gotActor != actor || note != "test" {
			t.Errorf("source/actor/note = %s/%s/%q, want admin_action/%s/\"test\"", source, gotActor, note, actor)
		}

		var rowCount int
		if err := d.Pool.QueryRow(context.Background(), `
SELECT count(*)::int FROM admin_role_audit WHERE subject_user_id = $1
`, subject).Scan(&rowCount); err != nil {
			t.Fatalf("count audit rows: %v", err)
		}
		if rowCount != i+1 {
			t.Errorf("audit row count after transition %d = %d, want %d (must not double-write)", i+1, rowCount, i+1)
		}
	}
}
