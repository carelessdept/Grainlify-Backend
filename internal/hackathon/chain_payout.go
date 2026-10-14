package hackathon

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/settlement"
)

// On-chain spec §2.2: payout arithmetic is per chain.
//
//	unit_value(chain) = contributor_pool(chain) / total_units(chain)
//
// Each chain is financially its own event. A sponsor funds a chain, and that
// money pays that chain's contributors and nobody else's.

// ChainPayout is one chain's computed plan.
type ChainPayout struct {
	ChainID string      `json:"chain_id"`
	Pool    float64     `json:"contributor_pool"`
	Plan    *PayoutPlan `json:"plan"`
}

// ComputePayoutsPerChain divides each chain's pool over only that chain's
// accepted PRs.
//
// The diminishing-returns curve applies **within a chain** - a contributor's
// Nth accepted PR *on that chain*. Deliberately not global: a global curve
// would discount one sponsor's pool because of activity in a pool they have
// no relationship to, which contradicts the separate-pool model the whole
// design rests on.
//
// The consequence, which is published rather than hidden: working across two
// chains restarts the curve on each. That is a known property, not a
// loophole - the global slot limits from §4 of the AI spec still cap how much
// work one person can hold at once, regardless of how it is spread.
func ComputePayoutsPerChain(
	prsByChain map[string][]JudgedPR,
	poolByChain map[string]float64,
	cfg map[string]string,
) ([]ChainPayout, error) {
	chains := make([]string, 0, len(poolByChain))
	for c := range poolByChain {
		chains = append(chains, c)
	}
	// Sorted so a recomputation months later produces byte-identical output.
	sort.Strings(chains)

	out := make([]ChainPayout, 0, len(chains))
	for _, chainID := range chains {
		plan, err := ComputePayout(prsByChain[chainID], poolByChain[chainID], cfg)
		if err != nil {
			return nil, fmt.Errorf("hackathon.ComputePayoutsPerChain: %s: %w", chainID, err)
		}
		out = append(out, ChainPayout{ChainID: chainID, Pool: poolByChain[chainID], Plan: plan})
	}
	return out, nil
}

// LoadJudgedPRsByChain groups a hackathon's judged PRs by the chain their
// issue was tagged with.
//
// A verdict with no chain lands under "" and is handled by the off-chain
// path, which is the correct behaviour for an event that has no chain pools
// at all - the great majority today.
func LoadJudgedPRsByChain(ctx context.Context, pool db.DBPool, hackathonID uuid.UUID) (map[string][]JudgedPR, error) {
	rows, err := pool.Query(ctx, `
SELECT v.id::text, v.github_login, v.final_bucket,
       COALESCE(pr.merged_at_github, v.created_at),
       COALESCE(v.chain_id, hi.chain_id, '')
FROM hackathon_verdicts v
LEFT JOIN hackathon_issues hi ON hi.id = v.hackathon_issue_id
LEFT JOIN github_pull_requests pr
       ON pr.project_id = v.project_id AND pr.number = v.pr_number
WHERE v.hackathon_id = $1 AND v.final_bucket IS NOT NULL
ORDER BY v.id
`, hackathonID)
	if err != nil {
		return nil, fmt.Errorf("hackathon.LoadJudgedPRsByChain: %w", err)
	}
	defer rows.Close()

	out := map[string][]JudgedPR{}
	for rows.Next() {
		var p JudgedPR
		var chainID string
		if err := rows.Scan(&p.VerdictID, &p.Login, &p.Bucket, &p.MergedAt, &chainID); err != nil {
			return nil, fmt.Errorf("hackathon.LoadJudgedPRsByChain: scan: %w", err)
		}
		out[chainID] = append(out[chainID], p)
	}
	return out, rows.Err()
}

// LoadChainPools reads each chain's contributor pool for an event, in whole
// units, from hackathon_chain_pools.
//
// Returns an empty map when the event runs no chains, which is what keeps the
// existing single-pool path unchanged for every event that is not on-chain.
func LoadChainPools(ctx context.Context, pool db.DBPool, hackathonID uuid.UUID) (map[string]float64, error) {
	rows, err := pool.Query(ctx, `
SELECT chain_id, (contributor_pool / (10 ^ asset_decimals))::float8
FROM hackathon_chain_pools
WHERE hackathon_id = $1
ORDER BY chain_id
`, hackathonID)
	if err != nil {
		return nil, fmt.Errorf("hackathon.LoadChainPools: %w", err)
	}
	defer rows.Close()

	out := map[string]float64{}
	for rows.Next() {
		var chainID string
		var amount float64
		if err := rows.Scan(&chainID, &amount); err != nil {
			return nil, fmt.Errorf("hackathon.LoadChainPools: scan: %w", err)
		}
		out[chainID] = amount
	}
	return out, rows.Err()
}

// SettleMaintainerPoolPerChain scores and allocates the maintainer pool
// independently for each chain (§2.2: "the maintainer pool is likewise scored
// and split per chain, over the repos whose issues were tagged with that
// chain").
//
// A repo that tagged issues on two chains is scored in both, from each
// chain's own budget. That is the same separate-pool logic as the contributor
// side: a sponsor's maintainer budget rewards the repos that brought work to
// *their* chain, not to someone else's.
//
// Falls through to the single-pool path when the event runs no chains, so
// every existing event settles exactly as before.
func SettleMaintainerPoolPerChain(
	ctx context.Context,
	pool db.DBPool,
	hackathonID uuid.UUID,
	medianFn ReviewMedianFn,
) (map[string][]MaintainerPayout, error) {
	chains, err := EventChains(ctx, pool, hackathonID)
	if err != nil {
		return nil, err
	}
	if len(chains) == 0 {
		payouts, err := SettleMaintainerPool(ctx, pool, hackathonID, medianFn)
		if err != nil {
			return nil, err
		}
		return map[string][]MaintainerPayout{"": payouts}, nil
	}

	cfg, err := EffectiveValues(ctx, pool, &hackathonID)
	if err != nil {
		return nil, fmt.Errorf("hackathon.SettleMaintainerPoolPerChain: config: %w", err)
	}

	out := map[string][]MaintainerPayout{}
	for _, chainID := range chains {
		poolMinor, err := chainMaintainerPoolMinorFor(ctx, pool, hackathonID, chainID)
		if err != nil {
			return nil, fmt.Errorf("hackathon.SettleMaintainerPoolPerChain: %s pool: %w", chainID, err)
		}

		// Only repos that tagged issues onto this chain compete for it.
		rows, err := pool.Query(ctx, `
SELECT DISTINCT p.id, p.github_full_name
FROM hackathon_issues hi
JOIN projects p ON p.id = hi.project_id
WHERE hi.hackathon_id = $1 AND hi.chain_id = $2
ORDER BY p.id
`, hackathonID, chainID)
		if err != nil {
			return nil, fmt.Errorf("hackathon.SettleMaintainerPoolPerChain: %s repos: %w", chainID, err)
		}
		type repo struct {
			id       uuid.UUID
			fullName string
		}
		var repos []repo
		for rows.Next() {
			var r repo
			if err := rows.Scan(&r.id, &r.fullName); err != nil {
				rows.Close()
				return nil, err
			}
			repos = append(repos, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}

		scores := make([]MaintainerScore, 0, len(repos))
		for _, r := range repos {
			var median *float64
			if medianFn != nil {
				median = medianFn(r.id, r.fullName)
			}
			s, err := ComputeMaintainerScore(ctx, pool, hackathonID, r.id, median, cfg)
			if err != nil {
				return nil, err
			}
			scores = append(scores, s)
		}
		payouts, err := AllocateMaintainerPoolMinor(scores, poolMinor, cfg, timeNow())
		if err != nil {
			return nil, fmt.Errorf("hackathon.SettleMaintainerPoolPerChain: %s allocate: %w", chainID, err)
		}
		out[chainID] = payouts
	}
	return out, nil
}

// chainMaintainerPoolMinorFor reads a chain's maintainer_pool as text and
// converts it to exact settlement.AssetDecimals minor units.
func chainMaintainerPoolMinorFor(ctx context.Context, pool db.DBPool, hackathonID uuid.UUID, chainID string) (*big.Int, error) {
	var raw string
	var decimals int
	if err := pool.QueryRow(ctx, `
SELECT COALESCE(maintainer_pool, 0)::text, asset_decimals
FROM hackathon_chain_pools WHERE hackathon_id = $1 AND chain_id = $2
`, hackathonID, chainID).Scan(&raw, &decimals); err != nil {
		return nil, err
	}
	if raw == "" {
		return new(big.Int), nil
	}
	r, ok := new(big.Rat).SetString(raw)
	if !ok {
		return nil, fmt.Errorf("maintainer_pool %q is not a number", raw)
	}
	if decimals < 0 {
		return nil, fmt.Errorf("asset_decimals %d is negative", decimals)
	}
	chainScale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	settleScale := new(big.Int).Exp(big.NewInt(10), big.NewInt(settlement.AssetDecimals), nil)
	minor := new(big.Rat).Mul(r, new(big.Rat).SetFrac(settleScale, chainScale))
	if !minor.IsInt() {
		return nil, fmt.Errorf("maintainer_pool %q at %d decimals is finer than one minor unit", raw, decimals)
	}
	return new(big.Int).Set(minor.Num()), nil
}

// timeNow is a seam so a settle time can be pinned in a test without
// threading a clock through every caller.
var timeNow = func() time.Time { return time.Now() }
