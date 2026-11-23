package allocation

import (
	"sort"

	"github.com/shopspring/decimal"
)

// SplitPct turns values into percentages of their sum at 2 decimal places using the
// largest-remainder method, so the parts add up to exactly 100.00 (ties go to the
// earlier value). Values at or below 0 get 0.00; all nil when nothing is positive.
func SplitPct(values []decimal.Decimal) []*decimal.Decimal {
	out := make([]*decimal.Decimal, len(values))
	total := decimal.Zero
	for _, v := range values {
		if v.IsPositive() {
			total = total.Add(v)
		}
	}
	if !total.IsPositive() {
		return out
	}
	const units = 10000 // 100.00 in hundredths
	floor := make([]int64, len(values))
	rem := make([]decimal.Decimal, len(values))
	assigned := int64(0)
	for i, v := range values {
		if !v.IsPositive() {
			continue
		}
		exact := v.Mul(decimal.NewFromInt(units)).DivRound(total, 18)
		f := exact.Floor()
		floor[i], rem[i] = f.IntPart(), exact.Sub(f)
		assigned += floor[i]
	}
	order := make([]int, len(values))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return rem[order[a]].Cmp(rem[order[b]]) > 0 })
	for k := int64(0); k < units-assigned && k < int64(len(order)); k++ {
		floor[order[k]]++
	}
	for i := range out {
		p := decimal.New(floor[i], -2)
		out[i] = &p
	}
	return out
}
