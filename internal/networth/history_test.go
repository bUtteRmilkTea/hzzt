package networth

import (
	"fmt"
	"github.com/shopspring/decimal"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JonathanRainee/hzzt/internal/platform/apperror"
)

func series(n int) []Point {
	out := make([]Point, n)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range out {
		out[i] = Point{Date: start.AddDate(0, 0, i).Format("2006-01-02"), NetWorth: decimal.NewFromInt(int64(i))}
	}
	return out
}

func TestDownsample(t *testing.T) {
	t.Run("short series are returned untouched", func(t *testing.T) {
		for _, n := range []int{0, 1, 2, 50, 120} {
			assert.Equal(t, series(n), Downsample(series(n), 120), n)
		}
	})

	for _, n := range []int{121, 150, 365, 400, 1000, 5000} {
		t.Run(fmt.Sprintf("%d points", n), func(t *testing.T) {
			in := series(n)
			out := Downsample(in, 120)
			assert.LessOrEqual(t, len(out), 120)
			assert.GreaterOrEqual(t, len(out), 115, "close to the limit, not much below it")
			assert.Equal(t, in[0], out[0], "the first point is kept")
			assert.Equal(t, in[n-1], out[len(out)-1], "the last (live) point is kept")
			for i := 1; i < len(out); i++ {
				assert.True(t, out[i].Date > out[i-1].Date, "strictly increasing dates, no duplicates")
			}
			// evenly spread: no gap more than about twice the average
			avg := float64(n) / float64(len(out)-1)
			for i := 1; i < len(out); i++ {
				a, _ := time.Parse("2006-01-02", out[i-1].Date)
				b, _ := time.Parse("2006-01-02", out[i].Date)
				assert.LessOrEqual(t, float64(b.Sub(a).Hours()/24), 2*avg+1)
			}
		})
	}

	t.Run("a tiny limit still keeps both ends", func(t *testing.T) {
		out := Downsample(series(100), 2)
		require.Len(t, out, 2)
		assert.Equal(t, "2026-01-01", out[0].Date)
		assert.Equal(t, series(100)[99], out[1])
		assert.Len(t, Downsample(series(100), 3), 3)
	})
	t.Run("a limit below two is a no-op", func(t *testing.T) {
		assert.Len(t, Downsample(series(10), 1), 10)
	})
}

func TestRangeStart(t *testing.T) {
	today := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	cases := map[string]string{"1M": "2026-10-18", "3M": "2026-08-18", "6M": "2026-05-18", "1Y": "2025-11-18", "1W": "2026-11-11", "1w": "2026-11-11", "YTD": "2026-01-01", "ytd": "2026-01-01", "1m": "2026-10-18", " 3m ": "2026-08-18"}
	for in, want := range cases {
		from, ok, err := rangeStart(in, today)
		require.NoError(t, err, in)
		require.True(t, ok)
		assert.Equal(t, want, from.Format("2006-01-02"), in)
	}
	_, ok, err := rangeStart("ALL", today)
	require.NoError(t, err)
	assert.False(t, ok, "ALL has no lower bound")

	for _, bad := range []string{"", "2M", "2W", "YEAR", "0", "YTDX"} {
		_, _, err := rangeStart(bad, today)
		require.Error(t, err, bad)
		ae, ok := apperror.As(err)
		require.True(t, ok)
		assert.Equal(t, apperror.CodeValidationFailed, ae.Code)
	}
	t.Run("a month end clamps like Go dates do", func(t *testing.T) {
		from, _, _ := rangeStart("1M", time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC))
		assert.Equal(t, "2026-03-03", from.Format("2006-01-02"), "31 Mar − 1 month normalizes through 31 Feb")
	})
}

func TestUTCDate(t *testing.T) {
	jkt, _ := time.LoadLocation("Asia/Jakarta")
	assert.Equal(t, "2026-11-17", utcDate(time.Date(2026, 11, 18, 3, 0, 0, 0, jkt)).Format("2006-01-02"), "03:00 Jakarta is still the 17th in UTC")
	assert.Equal(t, "2026-11-18", utcDate(time.Date(2026, 11, 18, 23, 59, 0, 0, time.UTC)).Format("2006-01-02"))
}

func TestDirectionOf(t *testing.T) {
	pct := func(s string) *decimal.Decimal { v := decimal.RequireFromString(s); return &v }
	cases := []struct {
		name   string
		change string
		pct    *decimal.Decimal
		want   string
	}{
		{"clearly up", "10", pct("5"), DirectionRising},
		{"clearly down", "-10", pct("-5"), DirectionFalling},
		{"inside the band is flat", "0.4", pct("0.04"), DirectionFlat},
		{"inside the negative band is flat", "-0.4", pct("-0.04"), DirectionFlat},
		{"the band edge counts as a move", "0.5", pct("0.05"), DirectionRising},
		{"the negative band edge counts as a move", "-0.5", pct("-0.05"), DirectionFalling},
		{"no percentage: the sign of the change", "3", nil, DirectionRising},
		{"no percentage and a loss", "-3", nil, DirectionFalling},
		{"no percentage and no change", "0", nil, DirectionFlat},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, directionOf(decimal.RequireFromString(c.change), c.pct), c.name)
	}
}

func TestStatOf(t *testing.T) {
	d := decimal.RequireFromString
	t.Run("a deposit is not performance", func(t *testing.T) {
		st := statOf(d("1000"), d("1500"), d("500"))
		assert.Equal(t, d("500"), st.Change)
		assert.Equal(t, "50", st.ChangePct.String())
		assert.True(t, st.MarketChange.IsZero())
		assert.Equal(t, "0", st.PerformancePct.String())
		assert.Equal(t, DirectionRising, st.Direction, "the value rose even though the return is 0")
	})
	t.Run("a price move is performance", func(t *testing.T) {
		st := statOf(d("1000"), d("900"), d("0"))
		assert.Equal(t, d("-100"), st.MarketChange)
		assert.Equal(t, "-10", st.PerformancePct.String())
		assert.Equal(t, DirectionFalling, st.Direction)
	})
	t.Run("money added and lost on the market", func(t *testing.T) {
		st := statOf(d("1000"), d("1400"), d("500")) // 1000 + 500 put in, worth 1400: lost 100 of 1500
		assert.Equal(t, d("-100"), st.MarketChange)
		assert.Equal(t, "-6.67", st.PerformancePct.String())
		assert.Equal(t, DirectionRising, st.Direction)
	})
	t.Run("a first value of 0 has no percentages", func(t *testing.T) {
		st := statOf(d("0"), d("100"), d("0"))
		assert.Nil(t, st.ChangePct)
		assert.Nil(t, st.PerformancePct, "the base is not positive")
		assert.Equal(t, DirectionRising, st.Direction)
	})
	t.Run("a base that withdrawals made negative has no performance", func(t *testing.T) {
		st := statOf(d("100"), d("0"), d("-500"))
		assert.Nil(t, st.PerformancePct)
	})
}

func TestBuildSeriesAndCoverage(t *testing.T) {
	d := decimal.RequireFromString
	first := newPoint("2026-11-01", d("1000"), d("500"), d("300"), d("200"), d("50"), d("50"), false)
	last := newPoint("2026-11-18", d("1300"), d("600"), d("450"), d("200"), d("50"), d("0"), true)
	assert.Equal(t, d("1050"), first.Assets, "500 + 300 + 200 + 50, the debt is not subtracted")

	s := buildSeries(first, last, flows{cash: d("100"), investments: d("100"), fixed: d("0"), unfunded: d("0")})
	assert.Equal(t, d("100"), s.NetWorth.NetContributions)
	assert.Equal(t, d("100"), s.Investments.NetContributions)
	assert.Equal(t, d("50"), s.Investments.MarketChange, "450 - 300 - 100")
	assert.Equal(t, d("100"), s.Assets.NetContributions, "trades are neutral for everything owned")
	assert.Equal(t, DirectionFlat, s.FixedAssets.Direction)

	one := buildSeries(last, last, flows{})
	assert.Equal(t, DirectionFlat, one.Investments.Direction)
	assert.Nil(t, one.Investments.ChangePct)

	from := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	c := coverage("2026-11-02", from, true)
	assert.True(t, c.Short)
	assert.Equal(t, "2026-08-18", *c.RequestedFrom)
	assert.False(t, coverage("2026-08-18", from, true).Short)
	all := coverage("2026-01-01", time.Time{}, false)
	assert.Nil(t, all.RequestedFrom)
	assert.False(t, all.Short)
}
