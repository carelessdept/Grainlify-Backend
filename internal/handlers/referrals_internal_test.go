package handlers

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// referralsTestDB delegates to dbtest.DB so white-box tests in package handlers
// run against the package-scoped gl_test_handlers database.
func referralsTestDB(t *testing.T) *db.DB {
	t.Helper()
	return dbtest.DB(t)
}

// referralsCreateUser seeds a user row identified only by a UUID-suffixed
// display_name (no github_user_id) so repeated local test runs against a
// persistent Postgres instance never collide on a unique constraint.
func referralsCreateUser(t *testing.T, d *db.DB) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := d.Pool.QueryRow(context.Background(), `
INSERT INTO users (display_name) VALUES ($1) RETURNING id
`, "referrals-internal-test-user-"+uuid.NewString()).Scan(&id); err != nil {
		t.Fatalf("failed to seed user: %v", err)
	}
	return id
}

func TestGenerateReferralCode_LengthAndAlphabet(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		code := generateReferralCode()
		if len(code) != referralCodeLength {
			t.Fatalf("generateReferralCode() length = %d, want %d", len(code), referralCodeLength)
		}
		for _, r := range code {
			if !containsRune(referralCodeAlphabet, r) {
				t.Fatalf("generateReferralCode() = %q contains character %q outside referralCodeAlphabet", code, r)
			}
		}
		seen[code] = true
	}
	// Not a strict uniqueness guarantee (8 chars from a 32-char alphabet has
	// ~1e12 possibilities), but 200 draws colliding would indicate a broken
	// RNG, not bad luck.
	if len(seen) < 195 {
		t.Fatalf("generateReferralCode() produced only %d distinct values across 200 calls, suspiciously low", len(seen))
	}
}

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}

func TestEnsureReferralCode_PersistsAndIsStable(t *testing.T) {
	d := referralsTestDB(t)
	userID := referralsCreateUser(t, d)

	code1, err := ensureReferralCode(context.Background(), d, userID)
	if err != nil {
		t.Fatalf("ensureReferralCode() error = %v", err)
	}
	if code1 == "" {
		t.Fatal("ensureReferralCode() returned empty code")
	}

	code2, err := ensureReferralCode(context.Background(), d, userID)
	if err != nil {
		t.Fatalf("ensureReferralCode() second call error = %v", err)
	}
	if code2 != code1 {
		t.Errorf("ensureReferralCode() second call = %q, want same code %q as first call", code2, code1)
	}

	var stored *string
	if err := d.Pool.QueryRow(context.Background(), `SELECT referral_code FROM users WHERE id = $1`, userID).Scan(&stored); err != nil {
		t.Fatalf("failed to read back referral_code: %v", err)
	}
	if stored == nil || *stored != code1 {
		t.Errorf("users.referral_code = %v, want %q", stored, code1)
	}
}

func TestAttachReferral_CreatesPendingRow(t *testing.T) {
	d := referralsTestDB(t)
	referrerID := referralsCreateUser(t, d)
	referredID := referralsCreateUser(t, d)

	code, err := ensureReferralCode(context.Background(), d, referrerID)
	if err != nil {
		t.Fatalf("ensureReferralCode() error = %v", err)
	}

	attachReferral(context.Background(), d, code, referredID)

	var status string
	var referrer uuid.UUID
	var pointsAwarded int
	err = d.Pool.QueryRow(context.Background(), `
SELECT status, referrer_user_id, points_awarded FROM referrals WHERE referred_user_id = $1
`, referredID).Scan(&status, &referrer, &pointsAwarded)
	if err != nil {
		t.Fatalf("expected a referrals row for the referred user, query failed: %v", err)
	}
	if status != "pending" {
		t.Errorf("status = %q, want %q", status, "pending")
	}
	if referrer != referrerID {
		t.Errorf("referrer_user_id = %v, want %v", referrer, referrerID)
	}
	if pointsAwarded != 0 {
		t.Errorf("points_awarded = %d, want 0 before completion", pointsAwarded)
	}
}

func TestAttachReferral_UnknownCodeIsNoOp(t *testing.T) {
	d := referralsTestDB(t)
	referredID := referralsCreateUser(t, d)

	attachReferral(context.Background(), d, "NOSUCHCODE", referredID)

	var count int
	if err := d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM referrals WHERE referred_user_id = $1`, referredID).Scan(&count); err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	if count != 0 {
		t.Errorf("referrals row count = %d, want 0 for an unknown referral code", count)
	}
}

func TestAttachReferral_SelfReferralIsNoOp(t *testing.T) {
	d := referralsTestDB(t)
	userID := referralsCreateUser(t, d)
	code, err := ensureReferralCode(context.Background(), d, userID)
	if err != nil {
		t.Fatalf("ensureReferralCode() error = %v", err)
	}

	// Same user as both referrer and referred - defensive guard, should
	// never legitimately happen via the OAuth flow but must not panic or
	// insert a self-loop row if it somehow did.
	attachReferral(context.Background(), d, code, userID)

	var count int
	if err := d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM referrals WHERE referred_user_id = $1`, userID).Scan(&count); err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	if count != 0 {
		t.Errorf("referrals row count = %d, want 0 for a self-referral attempt", count)
	}
}

func TestMaybeCompleteReferral_CompletesPendingAndAwardsPoints(t *testing.T) {
	// Covers the award arithmetic itself, which must keep working if the
	// freeze is lifted. The frozen behaviour has its own tests.
	UnfreezePointsProgrammeForTest(t)
	d := referralsTestDB(t)
	referrerID := referralsCreateUser(t, d)
	referredID := referralsCreateUser(t, d)
	code, err := ensureReferralCode(context.Background(), d, referrerID)
	if err != nil {
		t.Fatalf("ensureReferralCode() error = %v", err)
	}
	attachReferral(context.Background(), d, code, referredID)

	// notify=nil: Service.Notify nil-checks itself (see service.go), so this
	// exercises maybeCompleteReferral's DB side effects without needing a
	// live mailer/notifications setup.
	maybeCompleteReferral(context.Background(), d, nil, referredID)

	var status string
	var pointsAwarded int
	var completedAt *string
	err = d.Pool.QueryRow(context.Background(), `
SELECT status, points_awarded, completed_at::text FROM referrals WHERE referred_user_id = $1
`, referredID).Scan(&status, &pointsAwarded, &completedAt)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if status != "completed" {
		t.Errorf("status = %q, want %q", status, "completed")
	}
	if pointsAwarded != referralPointsPerCompletion {
		t.Errorf("points_awarded = %d, want %d", pointsAwarded, referralPointsPerCompletion)
	}
	if completedAt == nil {
		t.Error("completed_at is nil, want a timestamp")
	}
}

func TestMaybeCompleteReferral_NoPendingReferralIsNoOp(t *testing.T) {
	d := referralsTestDB(t)
	userID := referralsCreateUser(t, d)

	// Must not panic or error when the user was never referred at all.
	maybeCompleteReferral(context.Background(), d, nil, userID)
}

func TestMaybeCompleteReferral_SecondCallIsIdempotent(t *testing.T) {
	// Covers the award arithmetic itself, which must keep working if the
	// freeze is lifted. The frozen behaviour has its own tests.
	UnfreezePointsProgrammeForTest(t)
	d := referralsTestDB(t)
	referrerID := referralsCreateUser(t, d)
	referredID := referralsCreateUser(t, d)
	code, err := ensureReferralCode(context.Background(), d, referrerID)
	if err != nil {
		t.Fatalf("ensureReferralCode() error = %v", err)
	}
	attachReferral(context.Background(), d, code, referredID)

	maybeCompleteReferral(context.Background(), d, nil, referredID)
	maybeCompleteReferral(context.Background(), d, nil, referredID) // duplicate KYC webhook delivery

	var pointsAwarded int
	if err := d.Pool.QueryRow(context.Background(), `
SELECT points_awarded FROM referrals WHERE referred_user_id = $1
`, referredID).Scan(&pointsAwarded); err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if pointsAwarded != referralPointsPerCompletion {
		t.Errorf("points_awarded after two completion calls = %d, want %d (must not double-award)", pointsAwarded, referralPointsPerCompletion)
	}
}
