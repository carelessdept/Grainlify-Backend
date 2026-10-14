package hackathon

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// A criterion with too little data is dropped and its weight redistributed,
// never scored as zero. "We could not measure this" and "this repo did badly"
// are different claims, and only one of them is supported.
func TestComputeMaintainerScore_DropsThinCriteriaAndRenormalises(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	pool := d.Pool
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	cfg := map[string]string{}

	// No clarity ratings and no review median: two of four criteria drop.
	score, err := ComputeMaintainerScore(ctx, pool, hackathonID, projectID, nil, cfg)
	if err != nil {
		t.Fatalf("ComputeMaintainerScore: %v", err)
	}

	var included, dropped int
	var weightSum float64
	for _, c := range score.Criteria {
		if c.Included {
			included++
			weightSum += c.Weight
		} else {
			dropped++
			if c.Reason == "" {
				t.Errorf("criterion %q was dropped with no reason recorded", c.Key)
			}
			if c.Weight != 0 {
				t.Errorf("dropped criterion %q kept weight %v", c.Key, c.Weight)
			}
		}
	}
	if dropped < 2 {
		t.Fatalf("expected clarity and review-median to drop, got %d dropped", dropped)
	}
	// The surviving weights must still sum to 1, or the score silently
	// shrinks toward zero for every repo missing data.
	if weightSum < 0.999 || weightSum > 1.001 {
		t.Errorf("included weights sum to %v, want 1 after renormalisation", weightSum)
	}

	// A dropped clarity criterion must not be indistinguishable from a
	// terrible one.
	for _, c := range score.Criteria {
		if c.Key == CriterionIssueClarity {
			if c.Included {
				t.Error("clarity was scored with zero ratings")
			}
			if !strings.Contains(c.Reason, "rather than scored as zero") {
				t.Errorf("clarity drop reason does not explain the renormalisation: %q", c.Reason)
			}
		}
	}
}

// One rating must not decide a quarter of someone's payout.
func TestComputeMaintainerScore_ClarityNeedsAMinimumSample(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	pool := d.Pool
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	cfg := map[string]string{"maintainer_clarity_min_ratings": "3"}

	clFxRated(t, pool, hackathonID, projectID, 1200, 5)
	score, err := ComputeMaintainerScore(ctx, pool, hackathonID, projectID, nil, cfg)
	if err != nil {
		t.Fatalf("ComputeMaintainerScore: %v", err)
	}
	for _, c := range score.Criteria {
		if c.Key == CriterionIssueClarity && c.Included {
			t.Fatal("clarity counted with a single rating")
		}
	}

	clFxRated(t, pool, hackathonID, projectID, 1201, 5)
	clFxRated(t, pool, hackathonID, projectID, 1202, 5)
	score, err = ComputeMaintainerScore(ctx, pool, hackathonID, projectID, nil, cfg)
	if err != nil {
		t.Fatalf("ComputeMaintainerScore: %v", err)
	}
	var found bool
	for _, c := range score.Criteria {
		if c.Key == CriterionIssueClarity {
			found = c.Included
			if c.Raw == nil || *c.Raw != 5 {
				t.Errorf("clarity raw = %v, want the 5-star mean snapshotted", c.Raw)
			}
		}
	}
	if !found {
		t.Error("clarity did not count once it reached the minimum sample")
	}
}

// Every criterion value is snapshotted, not just the non-reproducible one.
// First-timer counts and clarity aggregates drift as data syncs too, and a
// score that comes back different during an appeal turns the appeal into an
// argument about arithmetic.
func TestComputeMaintainerScore_SnapshotsEveryInput(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	pool := d.Pool
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)

	median := 12.0
	score, err := ComputeMaintainerScore(ctx, pool, hackathonID, projectID, &median, map[string]string{})
	if err != nil {
		t.Fatalf("ComputeMaintainerScore: %v", err)
	}
	if len(score.Criteria) != 4 {
		t.Fatalf("got %d criteria, want all 4 recorded even when some are dropped", len(score.Criteria))
	}
	for _, c := range score.Criteria {
		if c.Included && c.Raw == nil {
			t.Errorf("criterion %q was scored with no raw value snapshotted", c.Key)
		}
	}
}

// §7's structural guarantee: maintainer payouts never draw on the contributor
// pool. If someone later "simplifies" the two pools into one, this must fail.
func TestAllocateMaintainerPool_NeverDrawsFromTheContributorPool(t *testing.T) {
	scores := []MaintainerScore{
		{ProjectID: uuid.New(), OrgLogin: "acme", Score: 0.9},
		{ProjectID: uuid.New(), OrgLogin: "globex", Score: 0.1},
	}
	cfg := map[string]string{}
	now := time.Now()

	// An empty maintainer pool pays nothing, no matter how large the
	// contributor pool is - there is no input to this function that could
	// carry contributor money into it.
	for _, pool := range []float64{0} {
		payouts := AllocateMaintainerPool(scores, pool, cfg, now)
		for _, p := range payouts {
			if p.GrossAmount != 0 || p.ImmediateAmount != 0 || p.HoldbackAmount != 0 {
				t.Fatalf("maintainer paid %v from an empty maintainer pool", p.GrossAmount)
			}
		}
	}

	// And with a real maintainer pool, the total never exceeds it.
	const maintainerPool = 1000.0
	payouts := AllocateMaintainerPool(scores, maintainerPool, cfg, now)
	var total float64
	for _, p := range payouts {
		total += p.GrossAmount
		if p.ImmediateAmount+p.HoldbackAmount > p.GrossAmount+0.01 {
			t.Errorf("%s: immediate+holdback %v exceeds gross %v", p.OrgLogin, p.ImmediateAmount+p.HoldbackAmount, p.GrossAmount)
		}
	}
	if total > maintainerPool+0.01 {
		t.Fatalf("maintainer payouts total %v, which exceeds the maintainer pool of %v", total, maintainerPool)
	}
}

func TestAllocateMaintainerPool_HoldsBackTheConfiguredShare(t *testing.T) {
	scores := []MaintainerScore{{ProjectID: uuid.New(), OrgLogin: "acme", Score: 1}}
	settled := time.Now()
	payouts := AllocateMaintainerPool(scores, 1000, map[string]string{}, settled)
	p := payouts[0]

	if p.HoldbackPct != 30 {
		t.Errorf("holdback_pct = %d, want the shipped default of 30", p.HoldbackPct)
	}
	if p.HoldbackAmount != 300 {
		t.Errorf("holdback = %v, want 300", p.HoldbackAmount)
	}
	if p.ImmediateAmount != 700 {
		t.Errorf("immediate = %v, want 700", p.ImmediateAmount)
	}
	wantDue := settled.AddDate(0, 0, 90)
	if p.HoldbackDueAt.Sub(wantDue) > time.Minute || wantDue.Sub(p.HoldbackDueAt) > time.Minute {
		t.Errorf("holdback due %v, want ~%v (90 days)", p.HoldbackDueAt, wantDue)
	}
}

// Three equal-score repos dividing 1000 USDC cannot be represented exactly in
// binary float or independent cent rounding (333.33 x 3 = 999.99). Apportioning
// in minor units via settlement.Apportion conserves every minor unit of the
// pool and breaks remainder ties deterministically by lowest project id.
func TestAllocateMaintainerPool_ConservesPoolExactlyAcrossTiesAndUnevenShares(t *testing.T) {
	idLow := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	idMid := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	idHigh := uuid.MustParse("00000000-0000-0000-0000-000000000003")

	scores := []MaintainerScore{
		{ProjectID: idHigh, OrgLogin: "high", Score: 1},
		{ProjectID: idLow, OrgLogin: "low", Score: 1},
		{ProjectID: idMid, OrgLogin: "mid", Score: 1},
	}
	poolMinor := big.NewInt(1_000_000_000) // 1000 USDC in minor units
	payouts, err := AllocateMaintainerPoolMinor(scores, poolMinor, map[string]string{}, time.Now())
	if err != nil {
		t.Fatalf("AllocateMaintainerPoolMinor: %v", err)
	}

	totalMinor := new(big.Int)
	byProject := map[uuid.UUID]MaintainerPayout{}
	for _, p := range payouts {
		totalMinor.Add(totalMinor, p.GrossMinor)
		byProject[p.ProjectID] = p

		splitSum := new(big.Int).Add(p.ImmediateMinor, p.HoldbackMinor)
		if splitSum.Cmp(p.GrossMinor) != 0 {
			t.Errorf("%s: immediate (%s) + holdback (%s) != gross (%s)",
				p.OrgLogin, p.ImmediateMinor, p.HoldbackMinor, p.GrossMinor)
		}
	}
	if totalMinor.Cmp(poolMinor) != 0 {
		t.Fatalf("allocated %s minor units, want exact pool %s", totalMinor, poolMinor)
	}
	// Lowest project ID receives the single leftover minor unit (333_333_334 vs 333_333_333).
	if got := byProject[idLow].GrossMinor; got.Cmp(big.NewInt(333_333_334)) != 0 {
		t.Errorf("lowest project id got %s minor units, want 333333334", got)
	}
	if got := byProject[idMid].GrossMinor; got.Cmp(big.NewInt(333_333_333)) != 0 {
		t.Errorf("middle project id got %s minor units, want 333333333", got)
	}
	if got := byProject[idHigh].GrossMinor; got.Cmp(big.NewInt(333_333_333)) != 0 {
		t.Errorf("highest project id got %s minor units, want 333333333", got)
	}
}

// The holdback must be conditional on continued activity. A timer alone pays
// the farmer three months late, which is the behaviour it exists to catch.
func TestDecideHoldback_ReleaseDependsOnContinuedActivity(t *testing.T) {
	payout := MaintainerPayout{HoldbackAmount: 300}
	cfg := map[string]string{}

	sustained := DecideHoldback(payout, RepoActivity{WindowDays: 60, Commits: 12, MergedPRs: 3, CommitsMeasured: true}, cfg)
	if sustained.Status != "released" || sustained.Released != 300 || sustained.Withheld != 0 {
		t.Errorf("sustained activity: %+v, want full release", sustained)
	}

	some := DecideHoldback(payout, RepoActivity{WindowDays: 60, Commits: 1, CommitsMeasured: true}, cfg)
	if some.Status != "partially_released" {
		t.Errorf("some activity: status = %q, want partially_released", some.Status)
	}
	if some.Released != 150 || some.Withheld != 150 {
		t.Errorf("some activity: released %v / withheld %v, want 150/150 at the default 50%%", some.Released, some.Withheld)
	}

	none := DecideHoldback(payout, RepoActivity{WindowDays: 60, CommitsMeasured: true}, cfg)
	if none.Status != "withheld" || none.Withheld != 300 {
		t.Errorf("no activity: %+v, want the holdback withheld", none)
	}
	if none.Destination != "next_event_pool" {
		t.Errorf("withheld destination = %q, want the published default", none.Destination)
	}

	// Failing to look is not evidence of absence.
	unmeasured := DecideHoldback(payout, RepoActivity{WindowDays: 60, CommitsMeasured: false}, cfg)
	if unmeasured.Status != "pending" {
		t.Errorf("unmeasured activity: status = %q, want pending - money must not be withheld because a GitHub call failed", unmeasured.Status)
	}
	if unmeasured.Withheld != 0 {
		t.Errorf("unmeasured activity withheld %v", unmeasured.Withheld)
	}
}

// A holdback whose due date passed while the service was down must still
// resolve on the next tick. The selection predicate carries no memory of
// previous runs, which is what makes that true - so this test drives it by
// backdating the due date rather than by simulating a schedule.
func TestReleaseDueHoldbacks_SurvivesAMissedRun(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	pool := d.Pool
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)

	if _, err := pool.Exec(ctx, `
INSERT INTO hackathon_maintainer_payouts
  (hackathon_id, project_id, org_login, score, gross_amount, holdback_pct,
   holdback_amount, immediate_amount, holdback_due_at)
VALUES ($1,$2,'acme',1,1000,30,300,700, now() - interval '14 days')
`, hackathonID, projectID); err != nil {
		t.Fatalf("seed payout: %v", err)
	}

	active := func(_ context.Context, _ uuid.UUID, _ string, since time.Time, windowDays int) RepoActivity {
		return RepoActivity{WindowDays: windowDays, Since: since, Commits: 9, MergedPRs: 3, CommitsMeasured: true}
	}

	n, err := ReleaseDueHoldbacks(ctx, pool, active)
	if err != nil {
		t.Fatalf("ReleaseDueHoldbacks: %v", err)
	}
	if n != 1 {
		t.Fatalf("resolved %d holdbacks, want 1 - a due date passed 14 days ago must still be picked up", n)
	}

	var status string
	var released float64
	if err := pool.QueryRow(ctx, `
SELECT holdback_status, released_amount FROM hackathon_maintainer_payouts
WHERE hackathon_id = $1 AND project_id = $2
`, hackathonID, projectID).Scan(&status, &released); err != nil {
		t.Fatalf("read: %v", err)
	}
	if status != "released" || released != 300 {
		t.Errorf("status=%q released=%v, want released/300", status, released)
	}

	// Already resolved: a second pass must not double-release.
	n, err = ReleaseDueHoldbacks(ctx, pool, active)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if n != 0 {
		t.Errorf("second pass resolved %d, want 0", n)
	}
}

// An unmeasurable repo stays pending and is retried, rather than forfeiting
// the money because one GitHub call failed.
func TestReleaseDueHoldbacks_UnmeasurableStaysPendingAndRetries(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	pool := d.Pool
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)

	if _, err := pool.Exec(ctx, `
INSERT INTO hackathon_maintainer_payouts
  (hackathon_id, project_id, org_login, score, gross_amount, holdback_pct,
   holdback_amount, immediate_amount, holdback_due_at)
VALUES ($1,$2,'acme',1,1000,30,300,700, now() - interval '1 day')
`, hackathonID, projectID); err != nil {
		t.Fatalf("seed payout: %v", err)
	}

	unmeasurable := func(_ context.Context, _ uuid.UUID, _ string, since time.Time, windowDays int) RepoActivity {
		return RepoActivity{WindowDays: windowDays, Since: since, CommitsMeasured: false}
	}

	if _, err := ReleaseDueHoldbacks(ctx, pool, unmeasurable); err != nil {
		t.Fatalf("ReleaseDueHoldbacks: %v", err)
	}
	var status string
	var withheld float64
	var checkedAt *time.Time
	if err := pool.QueryRow(ctx, `
SELECT holdback_status, withheld_amount, activity_checked_at
FROM hackathon_maintainer_payouts WHERE hackathon_id = $1 AND project_id = $2
`, hackathonID, projectID).Scan(&status, &withheld, &checkedAt); err != nil {
		t.Fatalf("read: %v", err)
	}
	if status != "pending" {
		t.Errorf("status = %q, want pending - money must not be forfeited on missing evidence", status)
	}
	if withheld != 0 {
		t.Errorf("withheld %v while unmeasurable", withheld)
	}
	if checkedAt == nil {
		t.Error("no activity_checked_at recorded; the attempt should be visible")
	}

	// Still selected on the next pass, and resolves once measurable.
	measured := func(_ context.Context, _ uuid.UUID, _ string, since time.Time, windowDays int) RepoActivity {
		return RepoActivity{WindowDays: windowDays, Since: since, CommitsMeasured: true}
	}
	n, err := ReleaseDueHoldbacks(ctx, pool, measured)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if n != 1 {
		t.Fatalf("retry resolved %d, want 1 - a pending holdback must remain selectable", n)
	}
	if err := pool.QueryRow(ctx, `
SELECT holdback_status, withheld_amount FROM hackathon_maintainer_payouts
WHERE hackathon_id = $1 AND project_id = $2
`, hackathonID, projectID).Scan(&status, &withheld); err != nil {
		t.Fatalf("read: %v", err)
	}
	if status != "withheld" || withheld != 300 {
		t.Errorf("after measuring no activity: status=%q withheld=%v, want withheld/300", status, withheld)
	}
}

// Settling persists a row per participating repo, with the criteria snapshot,
// and re-running replaces rather than duplicates.
func TestSettleMaintainerPool_PersistsAndIsIdempotent(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	pool := d.Pool
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE hackathons SET maintainer_prize_pool = 500 WHERE id = $1`, hackathonID); err != nil {
		t.Fatalf("set pool: %v", err)
	}

	for i := 0; i < 2; i++ {
		if _, err := SettleMaintainerPool(ctx, pool, hackathonID, nil); err != nil {
			t.Fatalf("SettleMaintainerPool run %d: %v", i, err)
		}
	}

	var rows int
	var criteria []byte
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int, MIN(criteria::text)::jsonb
FROM hackathon_maintainer_payouts WHERE hackathon_id = $1 AND project_id = $2
`, hackathonID, projectID).Scan(&rows, &criteria); err != nil {
		t.Fatalf("read: %v", err)
	}
	if rows != 1 {
		t.Errorf("got %d rows after settling twice, want 1", rows)
	}
	if len(criteria) < 10 {
		t.Errorf("criteria snapshot looks empty: %s", criteria)
	}
}

// The first-timer criterion must keep discriminating across the range real
// repos actually occupy.
//
// Pinned from a simulation against live data: distinct-contributor counts on
// the platform span 2 to 246. An earlier linear n/10 normalisation gave 19
// and 246 the identical mark, so four of six repos tied at full marks and the
// criterion decided nothing. This asserts the shape rather than exact values,
// so tuning the reference stays possible but flattening it does not.
func TestComputeMaintainerScore_FirstTimerScaleDoesNotSaturate(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	pool := d.Pool
	cfg := map[string]string{}

	// Representative of the real spread, smallest to largest.
	counts := []int{2, 5, 19, 135}
	var norms []float64

	for _, n := range counts {
		hackathonID, projectID, _ := fxLiveHackathon(t, pool)
		// announced_at is in the past on this fixture, so contributions
		// seeded now count as first-timers within the event.
		for i := 0; i < n; i++ {
			if _, err := pool.Exec(ctx, `
INSERT INTO github_pull_requests (project_id, github_pr_id, number, state, author_login, created_at_github)
VALUES ($1, $2, $3, 'open', $4, now())
`, projectID, uuid.New().ID(), 10000+i, fmt.Sprintf("newcomer-%d-%s", i, uuid.New().String()[:6])); err != nil {
				t.Fatalf("seed contributor: %v", err)
			}
		}
		score, err := ComputeMaintainerScore(ctx, pool, hackathonID, projectID, nil, cfg)
		if err != nil {
			t.Fatalf("ComputeMaintainerScore(n=%d): %v", n, err)
		}
		for _, c := range score.Criteria {
			if c.Key == CriterionFirstTimeContributors {
				norms = append(norms, c.Normalized)
			}
		}
	}

	if len(norms) != len(counts) {
		t.Fatalf("got %d normalised values, want %d", len(norms), len(counts))
	}
	for i := 1; i < len(norms); i++ {
		if norms[i] <= norms[i-1] {
			t.Errorf("n=%d scored %.3f, not above n=%d at %.3f - the scale has saturated and stopped discriminating",
				counts[i], norms[i], counts[i-1], norms[i-1])
		}
	}
	// And the bottom of the real range must not be pinned at zero either.
	if norms[0] <= 0 {
		t.Errorf("the smallest real repo normalised to %.3f; a participating repo should not score zero on contributor growth", norms[0])
	}
}

// maintainer_criteria_weights must actually override the Go defaults. A key
// visible in the admin UI that silently changes nothing is the same failure
// as auto_revert_oob_assignment: an admin sets it, sees it set, and believes
// it took effect.
func TestCriteriaWeights_ConfigOverridesTheDefaults(t *testing.T) {
	defaults := criteriaWeights(map[string]string{})
	if defaults[CriterionFirstTimeContributors] != 0.25 {
		t.Fatalf("default weight = %v, want 0.25", defaults[CriterionFirstTimeContributors])
	}

	overridden := criteriaWeights(map[string]string{
		"maintainer_criteria_weights": `{"first_time_contributors":0.7,"issue_clarity_rating":0.1}`,
	})
	if overridden[CriterionFirstTimeContributors] != 0.7 {
		t.Errorf("configured weight did not take effect: got %v, want 0.7", overridden[CriterionFirstTimeContributors])
	}
	if overridden[CriterionIssueClarity] != 0.1 {
		t.Errorf("configured clarity weight = %v, want 0.1", overridden[CriterionIssueClarity])
	}
	// Keys the config does not mention keep their default rather than
	// dropping to zero, so a partial override is not a silent deletion.
	if overridden[CriterionRepoPredatesEvent] != 0.25 {
		t.Errorf("unmentioned criterion = %v, want its 0.25 default kept", overridden[CriterionRepoPredatesEvent])
	}
	// Malformed config falls back rather than zeroing every weight, which
	// would make every repo score 0 and split the pool evenly by accident.
	garbage := criteriaWeights(map[string]string{"maintainer_criteria_weights": `{not json`})
	if garbage[CriterionFirstTimeContributors] != 0.25 {
		t.Errorf("malformed weights did not fall back to defaults: %v", garbage)
	}
	// An unknown key is ignored rather than inflating the denominator.
	unknown := criteriaWeights(map[string]string{"maintainer_criteria_weights": `{"made_up_criterion":9}`})
	if _, ok := unknown["made_up_criterion"]; ok {
		t.Error("an unknown criterion key was accepted into the weight set")
	}
}
