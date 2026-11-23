package allocation

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func dd(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func TestSplitPctAddsUpToExactly100(t *testing.T) {
	cases := [][]string{
		{"1", "1", "1"},
		{"600000", "300000", "100000"},
		{"1", "2", "4", "8", "16", "32", "64"},
		{"0.01", "99999999.99"},
		{"1", "0", "1", "-5"},
		{"33.333", "33.333", "33.334"},
	}
	for _, c := range cases {
		vals := make([]decimal.Decimal, len(c))
		for i, s := range c {
			vals[i] = dd(s)
		}
		got := SplitPct(vals)
		sum := decimal.Zero
		for i, p := range got {
			require.NotNil(t, p, "%v[%d]", c, i)
			assert.Equal(t, int32(-2), p.Exponent(), "two places")
			sum = sum.Add(*p)
		}
		assert.True(t, sum.Equal(dd("100")), "%v sums to %s", c, sum)
	}
	got := SplitPct([]decimal.Decimal{dd("1"), dd("1"), dd("1")})
	assert.Equal(t, "33.34", got[0].StringFixed(2), "the leftover unit goes to the earliest of the tied")
	assert.Equal(t, "33.33", got[1].StringFixed(2))
	neg := SplitPct([]decimal.Decimal{dd("1"), dd("-5")})
	assert.Equal(t, "0.00", neg[1].StringFixed(2))
}

func TestSplitPctWithNothingPositiveIsNull(t *testing.T) {
	for _, in := range [][]decimal.Decimal{nil, {dd("0"), dd("0")}, {dd("-1")}} {
		for _, p := range SplitPct(in) {
			assert.Nil(t, p)
		}
	}
}
