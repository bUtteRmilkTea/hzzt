package investments

import (
	"context"
	"errors"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/JonathanRainee/hzzt/internal/currency"
	"github.com/JonathanRainee/hzzt/internal/db"
	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
	"github.com/JonathanRainee/hzzt/internal/platform/apperror"
)

// Error codes owned by wrapper allocations (see plan Appendix B).
const (
	CodeAllocationExceeds100 = "ALLOCATION_EXCEEDS_100"
	CodeWrapperNotFound      = "WRAPPER_NOT_FOUND"
)

// bpsPct renders basis points as a percentage with 2 places ("70.00").
func bpsToPct(bps int) decimal.Decimal { return decimal.New(int64(bps), -2) }

// pctToBps converts a percentage with at most 2 decimals in [0, 100] to basis points.
func pctToBps(pct decimal.Decimal) (int, bool) {
	if !pct.Round(2).Equal(pct) || pct.IsNegative() || pct.GreaterThan(decimal.NewFromInt(100)) {
		return 0, false
	}
	return int(pct.Mul(decimal.NewFromInt(100)).IntPart()), true
}

// WrapperAlloc is one wrapper's planned and actual share of its asset type.
type WrapperAlloc struct {
	AssetType string
	// Wrapper is the display name; nil for the unassigned group.
	Wrapper *string
	Label   string
	Key     string
	// TargetPct is nil when no percentage is set. ActualPct is the wrapper's share of the asset
	// type's base-currency value, nil when the asset type is worth 0. DriftPct = actual - target
	// (nil unless both are known).
	TargetPct *decimal.Decimal
	ActualPct *decimal.Decimal
	DriftPct  *decimal.Decimal
}

// CategoryAlloc is the planned split of one asset type.
type CategoryAlloc struct {
	AssetType string
	Label     string
	// AssignedPct is the sum of the percentages set in the asset type; UnallocatedPct = 100 - assigned.
	AssignedPct    decimal.Decimal
	UnallocatedPct decimal.Decimal
	Wrappers       []WrapperAlloc
}

// Allocations is every wrapper's target and actual share, by asset type.
type Allocations struct {
	Currency   string
	Categories []CategoryAlloc
	Warnings   []currency.Warning
}

// BuildAllocations merges the stored targets into a grouped portfolio. A target whose wrapper no
// longer holds any investment (renamed or sold out) is kept and listed with an actual share of
// 0.00 (nil when the asset type is worth nothing).
func BuildAllocations(g Grouped, rows []sqlc.InvestmentWrapperAllocation) Allocations {
	type rk struct{ asset, key string }
	targets := map[rk]sqlc.InvestmentWrapperAllocation{}
	assigned := map[string]int{}
	for _, r := range rows {
		targets[rk{r.AssetType, r.WrapperKey}] = r
		assigned[r.AssetType] += int(r.PctBps)
	}
	groups := map[string]CategoryGroup{}
	for _, c := range g.Categories {
		groups[c.AssetType] = c
	}
	out := Allocations{Currency: g.Currency, Warnings: g.Warnings, Categories: []CategoryAlloc{}}
	for _, meta := range Metas {
		cg, hasGroup := groups[meta.AssetType]
		cat := CategoryAlloc{AssetType: meta.AssetType, Label: meta.Label,
			AssignedPct: bpsToPct(assigned[meta.AssetType]), UnallocatedPct: bpsToPct(10000 - assigned[meta.AssetType]), Wrappers: []WrapperAlloc{}}
		seen := map[string]bool{}
		for _, w := range cg.Wrappers {
			seen[w.Key] = true
			a := WrapperAlloc{AssetType: meta.AssetType, Wrapper: w.Wrapper, Label: groupLabel(w), Key: w.Key, ActualPct: w.PctOfCategory}
			if t, ok := targets[rk{meta.AssetType, w.Key}]; ok {
				v := bpsToPct(int(t.PctBps))
				a.TargetPct = &v
				if a.Wrapper == nil || WrapperKey(*a.Wrapper) != w.Key {
					lbl := t.Label
					a.Wrapper, a.Label = &lbl, lbl
				}
			}
			a.drift()
			cat.Wrappers = append(cat.Wrappers, a)
		}
		var orphans []WrapperAlloc
		for k, t := range targets {
			if k.asset != meta.AssetType || seen[k.key] {
				continue
			}
			lbl, v := t.Label, bpsToPct(int(t.PctBps))
			a := WrapperAlloc{AssetType: meta.AssetType, Wrapper: &lbl, Label: lbl, Key: k.key, TargetPct: &v}
			if hasGroup && cg.Value.IsPositive() {
				zero := decimal.New(0, -2)
				a.ActualPct = &zero
			}
			a.drift()
			orphans = append(orphans, a)
		}
		sort.Slice(orphans, func(i, j int) bool { return orphans[i].Key < orphans[j].Key })
		cat.Wrappers = append(cat.Wrappers, orphans...)
		if len(cat.Wrappers) > 0 {
			out.Categories = append(out.Categories, cat)
		}
	}
	return out
}

func (a *WrapperAlloc) drift() {
	if a.TargetPct != nil && a.ActualPct != nil {
		d := a.ActualPct.Sub(*a.TargetPct)
		a.DriftPct = &d
	}
}

// find returns the wrapper entry and its category.
func (a Allocations) find(assetType, key string) (CategoryAlloc, WrapperAlloc, bool) {
	for _, c := range a.Categories {
		if c.AssetType != assetType {
			continue
		}
		for _, w := range c.Wrappers {
			if w.Key == key {
				return c, w, true
			}
		}
	}
	return CategoryAlloc{}, WrapperAlloc{}, false
}

// category returns the asset type's totals (zero assigned when it has no entry).
func (a Allocations) category(assetType string) CategoryAlloc {
	for _, c := range a.Categories {
		if c.AssetType == assetType {
			return c
		}
	}
	return CategoryAlloc{AssetType: assetType, AssignedPct: decimal.New(0, -2), UnallocatedPct: decimal.New(10000, -2)}
}

// Allocations returns the targets and actual shares of the viewer's portfolio.
func (s *Service) Allocations(ctx context.Context, spaceID uuid.UUID, includeShared bool, viewer uuid.UUID) (Allocations, error) {
	res, err := s.List(ctx, spaceID, "", includeShared, viewer)
	if err != nil {
		return Allocations{}, err
	}
	rows, err := s.q.ListWrapperAllocations(ctx, spaceID)
	if err != nil {
		return Allocations{}, err
	}
	return BuildAllocations(Group(res.Summary.Currency, res.Items), rows), nil
}

// SetWrapperTarget sets (target != nil) or clears (nil) the planned percentage of one wrapper of an
// asset type. The percentages set in an asset type may add up to at most 100.00 (422
// ALLOCATION_EXCEEDS_100, with the already assigned and the available percentage); the rest stays
// unallocated. The wrapper must hold a live investment or already have a percentage (404
// WRAPPER_NOT_FOUND). Concurrent edits of an asset type are serialised.
func (s *Service) SetWrapperTarget(ctx context.Context, spaceID, userID uuid.UUID, assetType, key string, target *decimal.Decimal) (CategoryAlloc, WrapperAlloc, error) {
	bps := 0
	if target != nil {
		var ok bool
		if bps, ok = pctToBps(*target); !ok {
			return CategoryAlloc{}, WrapperAlloc{}, validationField("targetPct", "must be a percentage from 0 to 100 with at most 2 decimal places")
		}
	}
	err := db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if err := q.LockWrapperCategory(ctx, spaceID.String()+"/"+assetType); err != nil {
			return err
		}
		label, err := q.LatestWrapperLabel(ctx, sqlc.LatestWrapperLabelParams{SpaceID: spaceID, AssetType: assetType, Column3: key})
		live := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		cur, err := q.GetWrapperAllocation(ctx, sqlc.GetWrapperAllocationParams{SpaceID: spaceID, AssetType: assetType, WrapperKey: key})
		has := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if !live && !has {
			return apperror.New(404, CodeWrapperNotFound, "No investment is held on this wrapper")
		}
		if target == nil {
			_, err := q.DeleteWrapperAllocation(ctx, sqlc.DeleteWrapperAllocationParams{SpaceID: spaceID, AssetType: assetType, WrapperKey: key})
			return err
		}
		others, err := q.SumWrapperAllocations(ctx, sqlc.SumWrapperAllocationsParams{SpaceID: spaceID, AssetType: assetType, WrapperKey: key})
		if err != nil {
			return err
		}
		if int(others)+bps > 10000 {
			return apperror.Unprocessable(CodeAllocationExceeds100, "The percentages of this asset type would add up to more than 100").WithDetails(map[string]any{
				"assigned": bpsToPct(int(others)).StringFixed(2), "available": bpsToPct(10000 - int(others)).StringFixed(2),
			})
		}
		if !live {
			label = cur.Label
		}
		_, err = q.UpsertWrapperAllocation(ctx, sqlc.UpsertWrapperAllocationParams{
			SpaceID: spaceID, AssetType: assetType, WrapperKey: key, Label: label, PctBps: int32(bps), UpdatedByID: userID,
		})
		return err
	})
	if err != nil {
		return CategoryAlloc{}, WrapperAlloc{}, err
	}
	all, err := s.Allocations(ctx, spaceID, false, userID)
	if err != nil {
		return CategoryAlloc{}, WrapperAlloc{}, err
	}
	cat, w, ok := all.find(assetType, key)
	if !ok { // an orphaned percentage was cleared: nothing of it is left to list
		sum, err := s.q.SumWrapperAllocations(ctx, sqlc.SumWrapperAllocationsParams{SpaceID: spaceID, AssetType: assetType, WrapperKey: ""})
		if err != nil {
			return CategoryAlloc{}, WrapperAlloc{}, err
		}
		meta, _ := MetaFor(assetType)
		return CategoryAlloc{AssetType: assetType, Label: meta.Label, AssignedPct: bpsToPct(int(sum)), UnallocatedPct: bpsToPct(10000 - int(sum))},
			WrapperAlloc{AssetType: assetType, Key: key, Label: key}, nil
	}
	return cat, w, nil
}
