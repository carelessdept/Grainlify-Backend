package handlers

import (
	"fmt"
	"testing"
	"time"

	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// The SQL predicate and the Go function must return the same answer.
//
// supportUndeliveredPredicate is duplicated in
// migrations/000064_support_delivery_semantics.up.sql, because a partial index
// cannot call a Go function. Two copies of a rule is exactly how the
// leaderboard and the profile came to disagree about the same contributor, so
// this runs both against every combination of column states and compares them
// rather than trusting the copies to stay in step.
func TestSupportDelivered_SQLAndGoAgree(t *testing.T) {
	pool := dbtest.DB(t).Pool

	now := time.Now()
	stamps := []*time.Time{nil, &now}
	categories := []string{"bug", "kyc", "idea", "help", "other"}

	checked := 0
	for _, cat := range categories {
		for _, discord := range stamps {
			for _, topic := range stamps {
				for _, dm := range stamps {
					// Evaluate the SAME predicate text the index uses, against
					// these values, in Postgres.
					sql := fmt.Sprintf(`
SELECT %s
FROM (SELECT $1::text AS category,
             $2::timestamptz AS discord_delivered_at,
             $3::timestamptz AS telegram_delivered_at,
             $4::timestamptz AS telegram_admin_dm_delivered_at) AS support_requests`,
						supportUndeliveredPredicate)

					var sqlUndelivered bool
					if err := pool.QueryRow(t.Context(), sql, cat, discord, topic, dm).Scan(&sqlUndelivered); err != nil {
						t.Fatalf("evaluate predicate: %v", err)
					}
					goUndelivered := !supportFullyDelivered(cat, discord, topic, dm)

					if sqlUndelivered != goUndelivered {
						t.Errorf("DISAGREE category=%s discord=%v topic=%v dm=%v: sql=%v go=%v",
							cat, discord != nil, topic != nil, dm != nil, sqlUndelivered, goUndelivered)
					}
					checked++
				}
			}
		}
	}
	if checked != len(categories)*8 {
		t.Fatalf("checked %d combinations, expected %d", checked, len(categories)*8)
	}
}
