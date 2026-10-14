package hackathon

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/settlement"
)

// AI-specs.md §7. Four criteria decide a maintainer's share; the fifth -
// "repo still active 60 days after the event" - is not scored here because it
// cannot be known at settle time. It is what the holdback release is
// conditional on, which is the only thing that makes the holdback do any work.

// Criterion keys. §7's table marks these safe precisely because a maintainer
// cannot move them alone: issues created, PRs merged and stars are all
// excluded for the opposite reason.
const (
	CriterionFirstTimeContributors = "first_time_contributors"
	CriterionTimeToFirstReview     = "median_time_to_first_review_hours"
	CriterionRepoPredatesEvent     = "repo_had_commits_before_announced_at"
	CriterionIssueClarity          = "issue_clarity_rating"
)

// defaultCriteriaWeights is used when maintainer_criteria_weights is unset.
// Deliberately even: nothing yet justifies asserting one of these matters
// more than another, and an invented weighting would be indistinguishable
// from a measured one once it is in production.
var defaultCriteriaWeights = map[string]float64{
	CriterionFirstTimeContributors: 0.25,
	CriterionTimeToFirstReview:     0.25,
	CriterionRepoPredatesEvent:     0.25,
	CriterionIssueClarity:          0.25,
}

// Criterion is one scored input, snapshotted.
type Criterion struct {
	Key string `json:"key"`
	// Included is false when the criterion was dropped - too little data to
	// be meaningful. Its weight is then redistributed across the rest rather
	// than scored as zero, because "we could not measure this" and "this repo
	// did badly" are different claims and only one of them is true.
	Included bool   `json:"included"`
	Reason   string `json:"reason,omitempty"`
	// Raw is the measured value in its own units, kept so a human reading a
	// snapshot months later can sanity-check the normalisation.
	Raw        *float64 `json:"raw"`
	Normalized float64  `json:"normalized"`
	// Weight after renormalisation - what actually multiplied Normalized.
	Weight float64 `json:"weight"`
}

// MaintainerScore is one repo's score with every input that produced it.
type MaintainerScore struct {
	ProjectID  uuid.UUID   `json:"project_id"`
	OrgLogin   string      `json:"org_login"`
	Score      float64     `json:"score"`
	Criteria   []Criterion `json:"criteria"`
	ComputedAt time.Time   `json:"computed_at"`
}

// criteriaWeights resolves the configured weights, falling back to the
// defaults for any key the config does not mention.
func criteriaWeights(cfg map[string]string) map[string]float64 {
	weights := map[string]float64{}
	for k, v := range defaultCriteriaWeights {
		weights[k] = v
	}
	raw := cfg["maintainer_criteria_weights"]
	if raw == "" || raw == "{}" {
		return weights
	}
	var parsed map[string]float64
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return weights
	}
	for k, v := range parsed {
		if _, known := weights[k]; known && v >= 0 {
			weights[k] = v
		}
	}
	return weights
}

// scoreFrom applies renormalisation and returns the weighted score.
//
// Dropping a criterion redistributes its weight proportionally across those
// that remain, so a repo with too few clarity ratings is scored on what could
// actually be measured rather than being handed a zero for the missing one. A
// zero would be a factual claim the data does not support - and it is the
// same argument as the anonymity floor, applied to money instead of privacy.
func scoreFrom(criteria []Criterion) (float64, []Criterion) {
	var totalIncluded float64
	for _, c := range criteria {
		if c.Included {
			totalIncluded += c.Weight
		}
	}
	if totalIncluded <= 0 {
		for i := range criteria {
			criteria[i].Weight = 0
		}
		return 0, criteria
	}

	var score float64
	for i := range criteria {
		if !criteria[i].Included {
			criteria[i].Weight = 0
			continue
		}
		criteria[i].Weight = criteria[i].Weight / totalIncluded
		score += criteria[i].Normalized * criteria[i].Weight
	}
	return score, criteria
}

func floatPtr(v float64) *float64 { return &v }

// clampUnit keeps a normalised value inside [0,1].
func clampUnit(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// ComputeMaintainerScore builds one repo's score from stored data.
//
// reviewMedianHours is passed in rather than fetched here because it is
// sampled from the GitHub API (see docs/TESTING-DEBT.md) - keeping the call
// outside makes this function a pure-ish computation over stored rows, so the
// scoring rules are testable without a network.
//
// A nil reviewMedianHours drops that criterion rather than scoring it zero,
// for the same reason as the clarity floor.
func ComputeMaintainerScore(
	ctx context.Context,
	pool db.DBPool,
	hackathonID, projectID uuid.UUID,
	reviewMedianHours *float64,
	cfg map[string]string,
) (MaintainerScore, error) {
	out := MaintainerScore{ProjectID: projectID, ComputedAt: time.Now()}

	var orgLogin string
	var announcedAt *time.Time
	if err := pool.QueryRow(ctx, `
SELECT SPLIT_PART(p.github_full_name, '/', 1), h.announced_at
FROM projects p CROSS JOIN hackathons h
WHERE p.id = $1 AND h.id = $2
`, projectID, hackathonID).Scan(&orgLogin, &announcedAt); err != nil {
		return out, fmt.Errorf("hackathon.ComputeMaintainerScore: load context: %w", err)
	}
	out.OrgLogin = orgLogin

	weights := criteriaWeights(cfg)
	var criteria []Criterion

	// 1. Distinct first-time contributors to this repo during the event.
	// "First-time" means their earliest recorded contribution to this repo
	// falls inside the event, which is what makes it a growth signal rather
	// than a measure of existing traffic.
	var firstTimers int
	if err := pool.QueryRow(ctx, `
WITH firsts AS (
  SELECT LOWER(author_login) AS login, MIN(created_at_github) AS first_at
  FROM (
    SELECT author_login, created_at_github FROM github_pull_requests
    WHERE project_id = $1 AND author_login IS NOT NULL AND author_login <> ''
    UNION ALL
    SELECT author_login, created_at_github FROM github_issues
    WHERE project_id = $1 AND author_login IS NOT NULL AND author_login <> ''
  ) x
  WHERE created_at_github IS NOT NULL
  GROUP BY 1
)
SELECT count(*)::int FROM firsts
WHERE $2::timestamptz IS NULL OR first_at >= $2::timestamptz
`, projectID, announcedAt).Scan(&firstTimers); err != nil {
		return out, fmt.Errorf("hackathon.ComputeMaintainerScore: first-timers: %w", err)
	}
	// Log-scaled against a published reference, not linear.
	//
	// Chosen from real data rather than picked: across the projects on the
	// platform today the distinct-contributor count spans 2 to 246. A linear
	// scale with a small cap saturates - an earlier n/10 version gave a repo
	// with 19 newcomers exactly the same mark as one with 246, so the
	// criterion stopped discriminating across two thirds of the field. A
	// linear scale to the maximum has the opposite failure: the largest repo
	// takes the pool by sheer size, and the three largest here belong to one
	// org.
	//
	// A log scale discriminates across orders of magnitude without letting
	// scale alone decide. The reference is config so it is publishable in
	// advance alongside the draw weights - a maintainer planning around
	// "what counts as full marks" needs that number before the event.
	reference := float64(atoiOr(cfg["maintainer_first_timers_reference"], 200))
	if reference < 2 {
		reference = 2
	}
	criteria = append(criteria, Criterion{
		Key: CriterionFirstTimeContributors, Included: true,
		Raw:        floatPtr(float64(firstTimers)),
		Normalized: clampUnit(math.Log(1+float64(firstTimers)) / math.Log(1+reference)),
		Weight:     weights[CriterionFirstTimeContributors],
	})

	// 2. Median time to first review. Faster is better; 72h or worse scores
	// zero, immediate scores one.
	c2 := Criterion{Key: CriterionTimeToFirstReview, Weight: weights[CriterionTimeToFirstReview]}
	if reviewMedianHours == nil {
		c2.Reason = "No review data available for this repo, so the criterion is dropped and its weight redistributed."
	} else {
		c2.Included = true
		c2.Raw = reviewMedianHours
		c2.Normalized = clampUnit(1.0 - (*reviewMedianHours / 72.0))
	}
	criteria = append(criteria, c2)

	// 3. Repo predates the event. Binary, and the cheapest signal that the
	// repo was not spun up to farm it.
	var predates bool
	if announcedAt != nil {
		if err := pool.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM github_pull_requests WHERE project_id = $1 AND created_at_github < $2
  UNION ALL
  SELECT 1 FROM github_issues WHERE project_id = $1 AND created_at_github < $2
)
`, projectID, announcedAt).Scan(&predates); err != nil {
			return out, fmt.Errorf("hackathon.ComputeMaintainerScore: predates: %w", err)
		}
	}
	norm := 0.0
	if predates {
		norm = 1.0
	}
	criteria = append(criteria, Criterion{
		Key: CriterionRepoPredatesEvent, Included: announcedAt != nil,
		Raw: floatPtr(norm), Normalized: norm, Weight: weights[CriterionRepoPredatesEvent],
		Reason: func() string {
			if announcedAt == nil {
				return "The hackathon has no announced_at, so there is no boundary to test the repo against."
			}
			return ""
		}(),
	})

	// 4. Issue clarity, subject to a minimum sample. One rating deciding a
	// quarter of someone's payout is noise deciding money.
	minRatings := atoiOr(cfg["maintainer_clarity_min_ratings"], 3)
	if minRatings < 1 {
		minRatings = 1
	}
	count, mean, err := ClarityScore(ctx, pool, hackathonID, projectID)
	if err != nil {
		return out, err
	}
	c4 := Criterion{Key: CriterionIssueClarity, Weight: weights[CriterionIssueClarity]}
	if count < minRatings {
		c4.Reason = fmt.Sprintf("Only %d clarity rating(s); %d are needed before this counts. Dropped and its weight redistributed rather than scored as zero.", count, minRatings)
	} else {
		c4.Included = true
		c4.Raw = floatPtr(mean)
		// 1..5 stars onto 0..1.
		c4.Normalized = clampUnit((mean - 1.0) / 4.0)
	}
	criteria = append(criteria, c4)

	sort.Slice(criteria, func(i, j int) bool { return criteria[i].Key < criteria[j].Key })
	out.Score, out.Criteria = scoreFrom(criteria)
	return out, nil
}

// MaintainerPayout is one repo's allocation from the maintainer pool.
type MaintainerPayout struct {
	ProjectID       uuid.UUID `json:"project_id"`
	OrgLogin        string    `json:"org_login"`
	Score           float64   `json:"score"`
	GrossAmount     float64   `json:"gross_amount"`
	GrossMinor      *big.Int  `json:"gross_minor,omitempty"`
	HoldbackPct     int       `json:"holdback_pct"`
	HoldbackAmount  float64   `json:"holdback_amount"`
	HoldbackMinor   *big.Int  `json:"holdback_minor,omitempty"`
	ImmediateAmount float64   `json:"immediate_amount"`
	ImmediateMinor  *big.Int  `json:"immediate_minor,omitempty"`
	HoldbackDueAt   time.Time `json:"holdback_due_at"`
}

// scoreRat parses a repo's score into an exact rational at NUMERIC(10,6)
// precision - the same 6-decimal representation hackathon_maintainer_payouts.score
// stores in Postgres - so the weight handed to settlement.Apportion and the
// score persisted on the row are the same number.
func scoreRat(score float64) (*big.Rat, string) {
	if score <= 0 || math.IsNaN(score) || math.IsInf(score, 0) {
		return new(big.Rat), "0"
	}
	s := strconv.FormatFloat(score, 'f', 6, 64)
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() <= 0 {
		s = strconv.FormatFloat(score, 'f', -1, 64)
		r, ok = new(big.Rat).SetString(s)
		if !ok || r.Sign() <= 0 {
			return new(big.Rat), "0"
		}
	}
	return r, s
}

func minorToFloat(v *big.Int) float64 {
	if v == nil || v.Sign() == 0 {
		return 0
	}
	f, _ := new(big.Rat).SetFrac(v, big.NewInt(1_000_000)).Float64()
	return math.Round(f*1e6) / 1e6
}

func minorToDecimal(v *big.Int) string {
	if v == nil {
		return "0"
	}
	return settlement.RatToDecimalString(new(big.Rat).SetFrac(v, big.NewInt(1_000_000)), settlement.AssetDecimals)
}

func floatPoolToMinor(maintainerPool float64) (*big.Int, error) {
	if maintainerPool <= 0 || math.IsNaN(maintainerPool) || math.IsInf(maintainerPool, 0) {
		return new(big.Int), nil
	}
	s := strconv.FormatFloat(maintainerPool, 'f', settlement.AssetDecimals, 64)
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("hackathon.AllocateMaintainerPool: pool %q is not a number", s)
	}
	minor := new(big.Rat).Mul(r, new(big.Rat).SetInt64(1_000_000))
	if !minor.IsInt() {
		return nil, fmt.Errorf("hackathon.AllocateMaintainerPool: pool %q is finer than one minor unit", s)
	}
	return new(big.Int).Set(minor.Num()), nil
}

// AllocateMaintainerPoolMinor divides poolMinor (in settlement.AssetDecimals
// minor units) across scored repos in proportion to score using
// settlement.Apportion, asserts that the allocated minor units sum to the pool,
// and splits each repo's share into an immediate part and a holdback.
//
// Uses the same exact shape as the contributor settlement path: weights are
// parsed from decimal text into big.Rat, largest-remainder apportionment via
// settlement.Apportion converts rationals to integer minor units once with
// deterministic tie-breaking by project id, and the sum-to-pool invariant is
// checked against settlement.ErrAllocationMismatch.
func AllocateMaintainerPoolMinor(
	scores []MaintainerScore,
	poolMinor *big.Int,
	cfg map[string]string,
	settledAt time.Time,
) ([]MaintainerPayout, error) {
	holdbackPct := atoiOr(cfg["maintainer_holdback_pct"], 30)
	if holdbackPct < 0 {
		holdbackPct = 0
	}
	if holdbackPct > 100 {
		holdbackPct = 100
	}
	holdbackDays := atoiOr(cfg["maintainer_holdback_days"], 90)
	if holdbackDays < 0 {
		holdbackDays = 0
	}
	due := settledAt.AddDate(0, 0, holdbackDays)

	if poolMinor == nil {
		poolMinor = new(big.Int)
	}

	lines := make([]settlement.Line, len(scores))
	total := new(big.Rat)
	one := big.NewRat(1, 1)
	for i, s := range scores {
		w, _ := scoreRat(s.Score)
		lines[i] = settlement.Line{
			UserID:          s.ProjectID,
			RawWeight:       new(big.Rat).Set(w),
			Multiplier:      new(big.Rat).Set(one),
			EffectiveWeight: new(big.Rat).Set(w),
			AmountMinor:     new(big.Int),
		}
		if w.Sign() > 0 {
			total.Add(total, w)
		}
	}

	if poolMinor.Sign() > 0 && total.Sign() > 0 {
		if err := settlement.Apportion(lines, poolMinor, total); err != nil {
			return nil, fmt.Errorf("hackathon.AllocateMaintainerPoolMinor: %w", err)
		}
		res := &settlement.Result{
			Pool:           "maintainer",
			PoolMinor:      new(big.Int).Set(poolMinor),
			AssetDecimals:  settlement.AssetDecimals,
			TotalEffective: total,
			Lines:          lines,
		}
		if got := res.TotalAllocatedMinor(); got.Cmp(poolMinor) != 0 {
			return nil, fmt.Errorf("%w: allocated %s of %s minor units",
				settlement.ErrAllocationMismatch, got, poolMinor)
		}
	}

	pctBig := big.NewInt(int64(holdbackPct))
	hundred := big.NewInt(100)
	halfHundred := big.NewInt(50)

	out := make([]MaintainerPayout, 0, len(scores))
	for i, s := range scores {
		grossMinor := new(big.Int)
		if lines[i].AmountMinor != nil {
			grossMinor.Set(lines[i].AmountMinor)
		}
		holdbackMinor := new(big.Int).Mul(grossMinor, pctBig)
		if holdbackMinor.Sign() > 0 {
			holdbackMinor.Add(holdbackMinor, halfHundred)
		}
		holdbackMinor.Quo(holdbackMinor, hundred)
		immediateMinor := new(big.Int).Sub(grossMinor, holdbackMinor)

		out = append(out, MaintainerPayout{
			ProjectID:       s.ProjectID,
			OrgLogin:        s.OrgLogin,
			Score:           s.Score,
			GrossMinor:      grossMinor,
			GrossAmount:     minorToFloat(grossMinor),
			HoldbackPct:     holdbackPct,
			HoldbackMinor:   holdbackMinor,
			HoldbackAmount:  minorToFloat(holdbackMinor),
			ImmediateMinor:  immediateMinor,
			ImmediateAmount: minorToFloat(immediateMinor),
			HoldbackDueAt:   due,
		})
	}
	return out, nil
}

// AllocateMaintainerPool divides maintainerPool across scored repos in
// proportion to score, then splits each share into an immediate part and a
// holdback.
//
// maintainerPool is the *only* money that enters here. It is read from
// hackathons.maintainer_prize_pool by the caller and never from
// contributor_prize_pool - §7's opening requirement, and the reason the two
// are separate columns rather than one pot with a label.
func AllocateMaintainerPool(scores []MaintainerScore, maintainerPool float64, cfg map[string]string, settledAt time.Time) []MaintainerPayout {
	poolMinor, err := floatPoolToMinor(maintainerPool)
	if err != nil {
		return nil
	}
	out, err := AllocateMaintainerPoolMinor(scores, poolMinor, cfg, settledAt)
	if err != nil {
		return nil
	}
	return out
}

// RepoActivity is the evidence a holdback release is decided on.
type RepoActivity struct {
	WindowDays    int       `json:"window_days"`
	Since         time.Time `json:"since"`
	Commits       int       `json:"commits"`
	MergedPRs     int       `json:"merged_prs"`
	IssueActivity int       `json:"issue_activity"`
	// CommitsMeasured is false when the commit count could not be fetched.
	// Recorded because "no commits" and "we could not look" must not collapse
	// into the same decision when money is being withheld.
	CommitsMeasured bool `json:"commits_measured"`
}

// HoldbackDecision is what to do with a held-back amount.
type HoldbackDecision struct {
	Status      string       `json:"status"`
	Released    float64      `json:"released_amount"`
	Withheld    float64      `json:"withheld_amount"`
	Destination string       `json:"withheld_destination,omitempty"`
	Reason      string       `json:"reason"`
	Activity    RepoActivity `json:"activity"`
}

// DecideHoldback applies §7's fifth criterion - "repo still active 60 days
// after the event" - to a held-back amount.
//
// This is the whole point of the holdback. §7: "A maintainer farming an event
// is gone the next day. One genuinely growing a project is still there. This
// single mechanism does more anti-farming work than any classifier in this
// document." A holdback that releases on a timer alone tests none of that -
// the farmer waits out the clock and is paid in full, three months late.
//
// Three outcomes rather than two, because "one commit" and "nothing at all"
// are genuinely different and a binary would have to lump one of them in with
// the wrong neighbour.
//
// Withheld money goes to maintainer_withheld_destination, published in
// advance. Deciding afterwards where forfeited money lands is how a program
// ends up looking like it kept it.
func DecideHoldback(payout MaintainerPayout, activity RepoActivity, cfg map[string]string) HoldbackDecision {
	fullCommits := atoiOr(cfg["maintainer_activity_full_commits"], 5)
	fullPRs := atoiOr(cfg["maintainer_activity_full_prs"], 2)
	partialPct := atoiOr(cfg["maintainer_partial_release_pct"], 50)
	if partialPct < 0 {
		partialPct = 0
	}
	if partialPct > 100 {
		partialPct = 100
	}
	destination := cfg["maintainer_withheld_destination"]
	if destination == "" {
		destination = "next_event_pool"
	}

	d := HoldbackDecision{Activity: activity, Destination: destination}

	sustained := activity.Commits >= fullCommits || activity.MergedPRs >= fullPRs
	any := activity.Commits > 0 || activity.MergedPRs > 0 || activity.IssueActivity > 0

	switch {
	case sustained:
		d.Status = "released"
		d.Released = payout.HoldbackAmount
		d.Destination = ""
		d.Reason = fmt.Sprintf("Repo stayed active in the %d days after the event: %d commit(s) and %d merged PR(s).",
			activity.WindowDays, activity.Commits, activity.MergedPRs)
	case any:
		d.Status = "partially_released"
		d.Released = round2(payout.HoldbackAmount * float64(partialPct) / 100.0)
		d.Withheld = round2(payout.HoldbackAmount - d.Released)
		d.Reason = fmt.Sprintf("Some activity in the %d days after the event (%d commit(s), %d merged PR(s), %d issue event(s)) but below the bar of %d commits or %d merged PRs, so %d%% of the holdback is released.",
			activity.WindowDays, activity.Commits, activity.MergedPRs, activity.IssueActivity, fullCommits, fullPRs, partialPct)
	case !activity.CommitsMeasured:
		// Failing to look is not evidence of absence. Withholding someone's
		// money because a GitHub call failed would be a bug that reads as a
		// judgement, so this stays pending and is retried.
		d.Status = "pending"
		d.Reason = "Repo activity could not be measured, so the holdback stays pending rather than being withheld on missing evidence."
	default:
		d.Status = "withheld"
		d.Withheld = payout.HoldbackAmount
		d.Reason = fmt.Sprintf("No commits, merged PRs or issue activity in the %d days after the event.", activity.WindowDays)
	}
	return d
}

// ReviewMedianFn looks up a repo's median time to first review. Injected so
// the settle path decides whether to make network calls, and so the scoring
// rules stay testable without one. Returning nil drops that criterion.
type ReviewMedianFn func(projectID uuid.UUID, repoFullName string) *float64

// SettleMaintainerPool scores every participating repo, allocates the
// maintainer pool across them, and persists the result.
//
// Called on the transition into settled, next to the contributor recompute.
// Idempotent per hackathon: re-running replaces the rows rather than adding a
// second allocation, so a retry after a partial failure converges instead of
// paying twice.
//
// The money comes from hackathons.maintainer_prize_pool and nowhere else, read
// as text and parsed into exact minor units so no binary floating-point step
// enters the pool division.
func SettleMaintainerPool(
	ctx context.Context,
	pool db.DBPool,
	hackathonID uuid.UUID,
	medianFn ReviewMedianFn,
) ([]MaintainerPayout, error) {
	cfg, err := EffectiveValues(ctx, pool, &hackathonID)
	if err != nil {
		return nil, fmt.Errorf("hackathon.SettleMaintainerPool: config: %w", err)
	}

	poolMinor, err := poolMinorFor(ctx, pool, hackathonID, "maintainer")
	if err != nil {
		return nil, fmt.Errorf("hackathon.SettleMaintainerPool: pool: %w", err)
	}

	rows, err := pool.Query(ctx, `
SELECT DISTINCT p.id, p.github_full_name, p.owner_user_id
FROM hackathon_project_applications hpa
JOIN projects p ON p.id = hpa.project_id
WHERE hpa.hackathon_id = $1 AND hpa.status = 'accepted'
ORDER BY p.id
`, hackathonID)
	if err != nil {
		return nil, fmt.Errorf("hackathon.SettleMaintainerPool: participants: %w", err)
	}
	type participant struct {
		id       uuid.UUID
		fullName string
		owner    uuid.UUID
	}
	var participants []participant
	for rows.Next() {
		var p participant
		if err := rows.Scan(&p.id, &p.fullName, &p.owner); err != nil {
			rows.Close()
			return nil, err
		}
		participants = append(participants, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	scores := make([]MaintainerScore, 0, len(participants))
	owners := map[uuid.UUID]uuid.UUID{}
	for _, p := range participants {
		var median *float64
		if medianFn != nil {
			median = medianFn(p.id, p.fullName)
		}
		s, err := ComputeMaintainerScore(ctx, pool, hackathonID, p.id, median, cfg)
		if err != nil {
			return nil, err
		}
		scores = append(scores, s)
		owners[p.id] = p.owner
	}

	payouts, err := AllocateMaintainerPoolMinor(scores, poolMinor, cfg, time.Now())
	if err != nil {
		return nil, fmt.Errorf("hackathon.SettleMaintainerPool: allocate: %w", err)
	}
	byProject := map[uuid.UUID]MaintainerScore{}
	for _, s := range scores {
		byProject[s.ProjectID] = s
	}

	for _, p := range payouts {
		s := byProject[p.ProjectID]
		criteriaJSON, err := json.Marshal(s.Criteria)
		if err != nil {
			return nil, fmt.Errorf("hackathon.SettleMaintainerPool: marshal criteria: %w", err)
		}
		owner := owners[p.ProjectID]
		_, scoreDec := scoreRat(s.Score)
		if _, err := pool.Exec(ctx, `
INSERT INTO hackathon_maintainer_payouts
  (hackathon_id, project_id, org_login, maintainer_user_id, score, criteria,
   gross_amount, holdback_pct, holdback_amount, immediate_amount, holdback_due_at)
VALUES ($1,$2,$3,$4,$5::numeric,$6,$7::numeric,$8,$9::numeric,$10::numeric,$11)
ON CONFLICT (hackathon_id, project_id) DO UPDATE SET
  org_login = EXCLUDED.org_login,
  maintainer_user_id = EXCLUDED.maintainer_user_id,
  score = EXCLUDED.score,
  criteria = EXCLUDED.criteria,
  gross_amount = EXCLUDED.gross_amount,
  holdback_pct = EXCLUDED.holdback_pct,
  holdback_amount = EXCLUDED.holdback_amount,
  immediate_amount = EXCLUDED.immediate_amount,
  holdback_due_at = EXCLUDED.holdback_due_at,
  updated_at = now()
`, hackathonID, p.ProjectID, p.OrgLogin, owner, scoreDec, criteriaJSON,
			minorToDecimal(p.GrossMinor), p.HoldbackPct, minorToDecimal(p.HoldbackMinor), minorToDecimal(p.ImmediateMinor), p.HoldbackDueAt); err != nil {
			return nil, fmt.Errorf("hackathon.SettleMaintainerPool: persist: %w", err)
		}
	}
	return payouts, nil
}

// ActivityFn measures a repo's activity since a cutoff, for the holdback
// decision. Injected for the same reason as ReviewMedianFn.
type ActivityFn func(ctx context.Context, projectID uuid.UUID, repoFullName string, since time.Time, windowDays int) RepoActivity

// ReleaseDueHoldbacks resolves every holdback whose due date has passed.
//
// Convergent by construction, not by being run on time. The query selects on
// (status = 'pending' AND holdback_due_at <= now()), which carries no memory
// of whether a previous tick happened - so a service that is down across a
// due date resolves it on the next tick rather than skipping it, the same
// property sweepClosedEvents relies on. Nothing here records "last run at".
//
// A holdback whose activity could not be measured stays pending and is
// therefore retried on the following tick, which is the behaviour that makes
// a transient GitHub failure harmless rather than a silent forfeiture.
func ReleaseDueHoldbacks(ctx context.Context, pool db.DBPool, activityFn ActivityFn) (resolved int, err error) {
	rows, err := pool.Query(ctx, `
SELECT mp.id, mp.hackathon_id, mp.project_id, p.github_full_name, mp.holdback_amount,
       COALESCE(h.appeals_closed_at, h.results_published_at, mp.created_at)
FROM hackathon_maintainer_payouts mp
JOIN projects p ON p.id = mp.project_id
JOIN hackathons h ON h.id = mp.hackathon_id
WHERE mp.holdback_status = 'pending'
  AND mp.holdback_due_at IS NOT NULL
  AND mp.holdback_due_at <= now()
ORDER BY mp.holdback_due_at
LIMIT 200
`)
	if err != nil {
		return 0, fmt.Errorf("hackathon.ReleaseDueHoldbacks: %w", err)
	}
	type due struct {
		id          uuid.UUID
		hackathonID uuid.UUID
		projectID   uuid.UUID
		fullName    string
		amount      float64
		since       time.Time
	}
	var items []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.hackathonID, &d.projectID, &d.fullName, &d.amount, &d.since); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, d := range items {
		cfg, err := EffectiveValues(ctx, pool, &d.hackathonID)
		if err != nil {
			continue
		}
		windowDays := atoiOr(cfg["maintainer_activity_window_days"], 60)
		activity := RepoActivity{WindowDays: windowDays, Since: d.since}
		if activityFn != nil {
			activity = activityFn(ctx, d.projectID, d.fullName, d.since, windowDays)
		}

		decision := DecideHoldback(MaintainerPayout{HoldbackAmount: d.amount}, activity, cfg)
		activityJSON, _ := json.Marshal(decision.Activity)

		if decision.Status == "pending" {
			// Record that we looked and could not measure, without resolving.
			// The next tick retries because the selection predicate is
			// unchanged.
			if _, err := pool.Exec(ctx, `
UPDATE hackathon_maintainer_payouts
SET activity = $2, activity_checked_at = now(), holdback_reason = $3, updated_at = now()
WHERE id = $1
`, d.id, activityJSON, decision.Reason); err != nil {
				return resolved, err
			}
			continue
		}

		var destination any
		if decision.Destination != "" {
			destination = decision.Destination
		}
		if _, err := pool.Exec(ctx, `
UPDATE hackathon_maintainer_payouts
SET holdback_status = $2, released_amount = $3, withheld_amount = $4,
    withheld_destination = $5, activity = $6, activity_checked_at = now(),
    holdback_resolved_at = now(), holdback_reason = $7, updated_at = now()
WHERE id = $1 AND holdback_status = 'pending'
`, d.id, decision.Status, decision.Released, decision.Withheld,
			destination, activityJSON, decision.Reason); err != nil {
			return resolved, err
		}
		resolved++
	}
	return resolved, nil
}
