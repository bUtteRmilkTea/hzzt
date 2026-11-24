package investments

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
)

func TestParseNAV(t *testing.T) {
	for in, want := range map[string]string{"1534.20": "1534.2", "1,534.20": "1534.2", "1,234,567": "1234567", "0": "0", " 12.5 ": "12.5"} {
		got, err := parseNAV(in)
		require.NoError(t, err, in)
		assert.True(t, d(want).Equal(got), in)
	}
	for _, in := range []string{"", "abc", "1,53", "1,5000", "1e3", "-1,000", ",100"} {
		if v, err := parseNAV(in); err == nil {
			t.Errorf("%q should fail, got %s", in, v)
		}
	}
}

func TestNavAgeDays(t *testing.T) {
	now := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)
	fund := func(details string) sqlc.Investment {
		return sqlc.Investment{AssetType: TypeMutualFund, Details: []byte(details)}
	}
	got := navAgeDays(fund(`{"fundType":"EQUITY","navDate":"2026-10-14"}`), now)
	require.NotNil(t, got)
	assert.Equal(t, 41, *got)
	assert.Nil(t, navAgeDays(fund(`{"fundType":"EQUITY"}`), now), "no NAV date")
	assert.Nil(t, navAgeDays(fund(`{"fundType":"EQUITY","navDate":"soon"}`), now), "not a date")
	assert.Nil(t, navAgeDays(sqlc.Investment{AssetType: TypeCrypto, Details: []byte(`{"navDate":"2026-10-14"}`)}, now), "only mutual funds")
	future := navAgeDays(fund(`{"fundType":"EQUITY","navDate":"2026-12-01"}`), now)
	require.NotNil(t, future)
	assert.Zero(t, *future, "never negative")
}

func TestBuildRowsPnlAndChangeExcludeThePositionChange(t *testing.T) {
	list := []sqlc.InvestmentValuation{ // newest first
		{Nav: d("130"), Quantity: d("20"), CostBasis: d("2200"), Value: d("2600")},
		{Nav: d("110"), Quantity: d("10"), CostBasis: d("1000"), Value: d("1100")},
		{Nav: d("0"), Quantity: d("10"), CostBasis: d("0"), Value: d("0")},
	}
	rows := buildRows(list)
	assert.True(t, rows[0].IsLatest)
	assert.True(t, d("400").Equal(rows[0].PnL))
	assert.True(t, d("400").Equal(rows[0].ChangeValue), "(130-110) x 20")
	require.NotNil(t, rows[0].ChangePct)
	assert.Equal(t, "18.18", rows[0].ChangePct.StringFixed(2))
	assert.True(t, d("1100").Equal(rows[1].ChangeValue), "(110-0) x 10")
	assert.Nil(t, rows[1].ChangePct, "previous NAV is 0")
	assert.False(t, rows[2].HasChange)
	assert.Nil(t, rows[2].PnLPct, "cost 0")
}
