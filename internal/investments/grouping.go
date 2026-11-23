package investments

import (
	"sort"

	"github.com/shopspring/decimal"

	"github.com/JonathanRainee/hzzt/internal/currency"
	"github.com/JonathanRainee/hzzt/internal/platform/allocation"
)

// UnassignedKey is the key of the group of investments without a wrapper.
const UnassignedKey = "_unassigned"

// UnassignedLabel is the display label of that group.
const UnassignedLabel = "Unassigned"

// Totals are base-currency sums of a group of investments.
type Totals struct {
	Value        decimal.Decimal
	Cost         decimal.Decimal
	UnrealizedPL decimal.Decimal
	// UnrealizedPLPct is a percentage; nil when the cost is 0.
	UnrealizedPLPct *decimal.Decimal
}

func (t *Totals) add(v *BaseValues) {
	t.Value = t.Value.Add(v.MarketValue)
	t.Cost = t.Cost.Add(v.CostBasis)
	t.UnrealizedPL = t.UnrealizedPL.Add(v.UnrealizedPL)
}

func (t *Totals) finish() {
	if !t.Cost.IsZero() {
		pct := t.UnrealizedPL.Div(t.Cost).Mul(hundred)
		t.UnrealizedPLPct = &pct
	}
}

// WrapperGroup is the investments of one asset type held on one exchange, broker or platform.
type WrapperGroup struct {
	// Wrapper is the display name (the most recently written casing); nil for the unassigned group.
	Wrapper *string
	Key     string
	Totals
	// PctOfCategory and PctOfTotal are nil when the denominator is 0.
	PctOfCategory *decimal.Decimal
	PctOfTotal    *decimal.Decimal
	Items         []Item
}

// CategoryGroup is the investments of one asset type.
type CategoryGroup struct {
	AssetType string
	Label     string
	Totals
	PctOfTotal *decimal.Decimal
	Wrappers   []WrapperGroup
}

// Grouped is a portfolio grouped by asset type, then wrapper.
type Grouped struct {
	Currency   string
	Total      Totals
	Categories []CategoryGroup
	Warnings   []currency.Warning
}

// Group groups the items by asset type and then by wrapper (case and whitespace
// insensitive; no wrapper falls under the unassigned group). Sums use the
// viewer's base-currency figures only, so they equal Summarize over the same items;
// items without a usable FX rate are listed but left out of every sum and reported
// in Warnings. The percentages of the slices at each level add up to exactly 100.00.
func Group(baseCurrency string, items []Item) Grouped {
	vals := make([]Valuation, 0, len(items))
	for _, it := range items {
		vals = append(vals, it.Valuation)
	}
	sum := Summarize(baseCurrency, vals)
	out := Grouped{
		Currency:   sum.Currency,
		Total:      Totals{Value: sum.TotalValue, Cost: sum.TotalCost, UnrealizedPL: sum.UnrealizedPL, UnrealizedPLPct: sum.UnrealizedPLPct},
		Categories: []CategoryGroup{},
		Warnings:   sum.Warnings,
	}

	type bucket struct {
		g      WrapperGroup
		latest int64 // unix nano of the item whose casing is shown
	}
	for _, meta := range Metas {
		wrappers := map[string]*bucket{}
		cat := CategoryGroup{AssetType: meta.AssetType, Label: meta.Label, Wrappers: []WrapperGroup{}}
		for _, it := range items {
			if it.Investment.AssetType != meta.AssetType {
				continue
			}
			key, label := UnassignedKey, (*string)(nil)
			if w := it.Investment.Wrapper; w != nil && WrapperKey(*w) != "" {
				key = WrapperKey(*w)
				n := NormalizeWrapper(*w)
				label = &n
			}
			b := wrappers[key]
			if b == nil {
				b = &bucket{g: WrapperGroup{Key: key}}
				wrappers[key] = b
			}
			if label != nil {
				if ts := it.Investment.UpdatedAt.UnixNano(); b.g.Wrapper == nil || ts >= b.latest {
					b.g.Wrapper, b.latest = label, ts
				}
			}
			b.g.Items = append(b.g.Items, it)
			if v := it.Valuation.Base; v != nil {
				b.g.add(v)
				cat.add(v)
			}
		}
		if len(wrappers) == 0 {
			continue
		}
		for _, b := range wrappers {
			b.g.finish()
			cat.Wrappers = append(cat.Wrappers, b.g)
		}
		sort.SliceStable(cat.Wrappers, func(i, j int) bool {
			a, b := cat.Wrappers[i], cat.Wrappers[j]
			if (a.Key == UnassignedKey) != (b.Key == UnassignedKey) {
				return b.Key == UnassignedKey
			}
			if c := a.Value.Cmp(b.Value); c != 0 {
				return c > 0
			}
			return a.Key < b.Key
		})
		cat.finish()
		out.Categories = append(out.Categories, cat)
	}

	// percentages: wrappers within their category, and wrappers/categories of the grand total
	catVals := make([]decimal.Decimal, len(out.Categories))
	var allWrappers []*WrapperGroup
	var allVals []decimal.Decimal
	for ci := range out.Categories {
		cat := &out.Categories[ci]
		catVals[ci] = cat.Value
		in := make([]decimal.Decimal, len(cat.Wrappers))
		for wi := range cat.Wrappers {
			in[wi] = cat.Wrappers[wi].Value
			allWrappers = append(allWrappers, &cat.Wrappers[wi])
			allVals = append(allVals, cat.Wrappers[wi].Value)
		}
		for wi, p := range allocation.SplitPct(in) {
			cat.Wrappers[wi].PctOfCategory = p
		}
	}
	for ci, p := range allocation.SplitPct(catVals) {
		out.Categories[ci].PctOfTotal = p
	}
	for wi, p := range allocation.SplitPct(allVals) {
		allWrappers[wi].PctOfTotal = p
	}
	return out
}

// ItemPcts returns each item's share of the portfolio's base-currency value as
// percentages summing to exactly 100.00; nil for an item without a base value, or
// for all when the total is 0.
func ItemPcts(items []Item) []*decimal.Decimal {
	idx := make([]int, 0, len(items))
	vals := make([]decimal.Decimal, 0, len(items))
	for i, it := range items {
		if it.Valuation.Base != nil {
			idx = append(idx, i)
			vals = append(vals, it.Valuation.Base.MarketValue)
		}
	}
	out := make([]*decimal.Decimal, len(items))
	for k, p := range allocation.SplitPct(vals) {
		out[idx[k]] = p
	}
	return out
}

// groupLabel is the wrapper's display label, or "Unassigned".
func groupLabel(g WrapperGroup) string {
	if g.Wrapper != nil {
		return *g.Wrapper
	}
	return UnassignedLabel
}
