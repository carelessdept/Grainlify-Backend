package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// authTestDB returns a live *pgxpool.Pool connected to this package's isolated
// test database (gl_test_auth), migrated to the current schema via dbtest.DB.
func authTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return dbtest.DB(t).Pool.(*pgxpool.Pool)
}

// repoTestUniqueAddress returns an arbitrary-but-unique wallet address
// string so repeated test runs against a persistent Postgres don't collide
// on the wallets(wallet_type, address) or auth_nonces(wallet_type, address,
// nonce) unique constraints. repo.go stores addresses as opaque text with
// no format validation (that happens one layer up, in NormalizeAddress), so
// the exact shape doesn't matter here.
func repoTestUniqueAddress() string {
	return "0xrepo-test-" + uuid.NewString()
}

func TestCreateNonce(t *testing.T) {
	pool := authTestDB(t)
	ctx := context.Background()
	address := repoTestUniqueAddress()

	n, err := CreateNonce(ctx, pool, WalletTypeEVM, address, 10*time.Minute)
	if err != nil {
		t.Fatalf("CreateNonce: %v", err)
	}
	if n.Nonce == "" {
		t.Error("nonce is empty")
	}
	if !n.ExpiresAt.After(time.Now()) {
		t.Errorf("expires_at = %v, want in the future", n.ExpiresAt)
	}
}

func TestCreateNonce_DefaultTTLWhenNonPositive(t *testing.T) {
	pool := authTestDB(t)
	ctx := context.Background()
	address := repoTestUniqueAddress()

	before := time.Now()
	n, err := CreateNonce(ctx, pool, WalletTypeEVM, address, 0) // ttl <= 0 -> defaults to 10m
	if err != nil {
		t.Fatalf("CreateNonce: %v", err)
	}
	wantMin := before.Add(9 * time.Minute)
	wantMax := before.Add(11 * time.Minute)
	if n.ExpiresAt.Before(wantMin) || n.ExpiresAt.After(wantMax) {
		t.Errorf("expires_at = %v, want between %v and %v", n.ExpiresAt, wantMin, wantMax)
	}
}

func TestCreateNonce_NilPool(t *testing.T) {
	_, err := CreateNonce(context.Background(), nil, WalletTypeEVM, "0xabc", time.Minute)
	if err == nil {
		t.Fatal("expected error for nil pool, got nil")
	}
}

func TestConsumeNonceAndUpsertUser_NewUser(t *testing.T) {
	pool := authTestDB(t)
	ctx := context.Background()
	address := repoTestUniqueAddress()

	n, err := CreateNonce(ctx, pool, WalletTypeEVM, address, 10*time.Minute)
	if err != nil {
		t.Fatalf("CreateNonce: %v", err)
	}

	res, err := ConsumeNonceAndUpsertUser(ctx, pool, WalletTypeEVM, address, n.Nonce, "pubkey-abc")
	if err != nil {
		t.Fatalf("ConsumeNonceAndUpsertUser: %v", err)
	}
	if res.User.ID == uuid.Nil {
		t.Error("user id is the zero UUID")
	}
	if res.User.Role != "contributor" {
		t.Errorf("role = %v, want contributor (the schema default)", res.User.Role)
	}
	if res.Wallet.Address != address {
		t.Errorf("wallet address = %v, want %v", res.Wallet.Address, address)
	}
	if res.Wallet.WalletType != WalletTypeEVM {
		t.Errorf("wallet type = %v, want %v", res.Wallet.WalletType, WalletTypeEVM)
	}
}

func TestConsumeNonceAndUpsertUser_RepeatLoginReturnsSameUser(t *testing.T) {
	pool := authTestDB(t)
	ctx := context.Background()
	address := repoTestUniqueAddress()

	n1, err := CreateNonce(ctx, pool, WalletTypeEVM, address, 10*time.Minute)
	if err != nil {
		t.Fatalf("CreateNonce (1st): %v", err)
	}
	res1, err := ConsumeNonceAndUpsertUser(ctx, pool, WalletTypeEVM, address, n1.Nonce, "")
	if err != nil {
		t.Fatalf("ConsumeNonceAndUpsertUser (1st): %v", err)
	}

	n2, err := CreateNonce(ctx, pool, WalletTypeEVM, address, 10*time.Minute)
	if err != nil {
		t.Fatalf("CreateNonce (2nd): %v", err)
	}
	res2, err := ConsumeNonceAndUpsertUser(ctx, pool, WalletTypeEVM, address, n2.Nonce, "")
	if err != nil {
		t.Fatalf("ConsumeNonceAndUpsertUser (2nd): %v", err)
	}

	if res1.User.ID != res2.User.ID {
		t.Errorf("expected the same user id on repeat login for the same wallet, got %v then %v", res1.User.ID, res2.User.ID)
	}
}

func TestConsumeNonceAndUpsertUser_WrongNonce(t *testing.T) {
	pool := authTestDB(t)
	ctx := context.Background()
	address := repoTestUniqueAddress()

	if _, err := CreateNonce(ctx, pool, WalletTypeEVM, address, 10*time.Minute); err != nil {
		t.Fatalf("CreateNonce: %v", err)
	}

	_, err := ConsumeNonceAndUpsertUser(ctx, pool, WalletTypeEVM, address, "totally-wrong-nonce-value", "")
	if err == nil {
		t.Fatal("expected error for a wrong nonce, got nil")
	}
}

func TestConsumeNonceAndUpsertUser_ExpiredNonce(t *testing.T) {
	pool := authTestDB(t)
	ctx := context.Background()
	address := repoTestUniqueAddress()

	// TTL of a few milliseconds so it's reliably expired by the time we
	// try to consume it below.
	n, err := CreateNonce(ctx, pool, WalletTypeEVM, address, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("CreateNonce: %v", err)
	}
	time.Sleep(250 * time.Millisecond)

	_, err = ConsumeNonceAndUpsertUser(ctx, pool, WalletTypeEVM, address, n.Nonce, "")
	if err == nil {
		t.Fatal("expected error for an expired nonce, got nil")
	}
}

func TestConsumeNonceAndUpsertUser_NonceIsSingleUse(t *testing.T) {
	pool := authTestDB(t)
	ctx := context.Background()
	address := repoTestUniqueAddress()

	n, err := CreateNonce(ctx, pool, WalletTypeEVM, address, 10*time.Minute)
	if err != nil {
		t.Fatalf("CreateNonce: %v", err)
	}

	if _, err := ConsumeNonceAndUpsertUser(ctx, pool, WalletTypeEVM, address, n.Nonce, ""); err != nil {
		t.Fatalf("first consume: unexpected error: %v", err)
	}

	if _, err := ConsumeNonceAndUpsertUser(ctx, pool, WalletTypeEVM, address, n.Nonce, ""); err == nil {
		t.Fatal("expected error consuming the same nonce a second time, got nil (nonce should be single-use)")
	}
}

func TestConsumeNonceAndUpsertUser_NilPool(t *testing.T) {
	_, err := ConsumeNonceAndUpsertUser(context.Background(), nil, WalletTypeEVM, "0xabc", "nonce", "")
	if err == nil {
		t.Fatal("expected error for nil pool, got nil")
	}
}

// A nonce issued for payout-address verification must not be redeemable at the
// sign-in endpoint. Today the two challenge texts differ, so a signature made
// for one will not verify for the other -- but that is a single accidental
// overlap away from being the only separation between the two flows, and the
// nonce table has carried a `purpose` column since migration 000080 precisely
// so the separation does not rest on message wording.
func TestConsumeNonceAndUpsertUser_RefusesNonceIssuedForAnotherPurpose(t *testing.T) {
	pool := authTestDB(t)
	ctx := context.Background()
	address := repoTestUniqueAddress()

	n, err := CreateNonceForPurpose(ctx, pool, WalletTypeEVM, address, PurposePayoutAddress, 10*time.Minute)
	if err != nil {
		t.Fatalf("CreateNonceForPurpose: %v", err)
	}

	_, err = ConsumeNonceAndUpsertUser(ctx, pool, WalletTypeEVM, address, n.Nonce, "")
	if err == nil {
		t.Fatal("sign-in consumed a payout-address nonce; want invalid_or_expired_nonce")
	}
	// Deliberately the same error a wholly unknown nonce gets: the sign-in
	// endpoint should not tell a caller that some other flow issued this one.
	if err.Error() != "invalid_or_expired_nonce" {
		t.Fatalf("err = %q, want %q", err.Error(), "invalid_or_expired_nonce")
	}

	// The refused attempt must not burn the nonce. It still belongs to the
	// payout flow, and the person who asked for it has not used it yet.
	if err := ConsumeNonceForPurpose(ctx, pool, WalletTypeEVM, address, n.Nonce, PurposePayoutAddress); err != nil {
		t.Fatalf("payout nonce unusable after a refused sign-in: %v", err)
	}
}

// CreateNonce writes the purpose explicitly rather than relying on the column
// default, so that changing the default cannot silently widen what a sign-in
// nonce is accepted for.
func TestCreateNonce_RecordsSignInPurpose(t *testing.T) {
	pool := authTestDB(t)
	ctx := context.Background()
	address := repoTestUniqueAddress()

	n, err := CreateNonce(ctx, pool, WalletTypeEVM, address, 10*time.Minute)
	if err != nil {
		t.Fatalf("CreateNonce: %v", err)
	}

	var purpose string
	err = pool.QueryRow(ctx, `
SELECT purpose FROM auth_nonces
WHERE wallet_type = $1 AND address = $2 AND nonce = $3
`, string(WalletTypeEVM), address, n.Nonce).Scan(&purpose)
	if err != nil {
		t.Fatalf("reading purpose back: %v", err)
	}
	if purpose != string(PurposeSignIn) {
		t.Errorf("purpose = %q, want %q", purpose, string(PurposeSignIn))
	}
}

// The mirror of the refusal above: a sign-in nonce is not redeemable at the
// payout-address endpoint either. That direction was already scoped; this
// pins it so both halves of the separation are covered by tests.
func TestConsumeNonceForPurpose_RefusesSignInNonceForPayout(t *testing.T) {
	pool := authTestDB(t)
	ctx := context.Background()
	address := repoTestUniqueAddress()

	n, err := CreateNonce(ctx, pool, WalletTypeEVM, address, 10*time.Minute)
	if err != nil {
		t.Fatalf("CreateNonce: %v", err)
	}

	err = ConsumeNonceForPurpose(ctx, pool, WalletTypeEVM, address, n.Nonce, PurposePayoutAddress)
	if !errors.Is(err, ErrNonceWrongPurpose) {
		t.Fatalf("err = %v, want ErrNonceWrongPurpose", err)
	}

	// Still usable for the flow that issued it.
	if _, err := ConsumeNonceAndUpsertUser(ctx, pool, WalletTypeEVM, address, n.Nonce, ""); err != nil {
		t.Fatalf("sign-in nonce unusable after a refused payout consume: %v", err)
	}
}
