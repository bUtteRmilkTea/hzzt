package investments

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
	"github.com/JonathanRainee/hzzt/internal/platform/apperror"
)

func leg(q, price, fee string) Leg { return Leg{Quantity: d(q), UnitPrice: d(price), Fee: d(fee)} }
func pos(q, avg, fees string) Position {
	return Position{Quantity: d(q), AvgBuyPrice: d(avg), Fees: d(fees)}
}

func TestBuyAveragesTheCost(t *testing.T) {
	cases := []struct {
		name  string
		start Position
		buy   Leg
		want  Position
	}{
		{"acceptance: 0.5 BTC at 60,000 then 0.5 at 70,000 → avg 65,000, quantity 1",
			pos("0.5", "60000", "0"), leg("0.5", "70000", "0"), pos("1", "65000", "0")},
		{"first purchase into an empty position", pos("0", "0", "0"), leg("2", "100", "1.5"), pos("2", "100", "1.5")},
		{"fees accumulate and stay out of the average", pos("10", "100", "5"), leg("10", "200", "3"), pos("20", "150", "8")},
		{"averaging down", pos("10", "100", "0"), leg("30", "50", "0"), pos("40", "62.5", "0")},
		{"an average that does not divide evenly is rounded to 8 places", pos("1", "100", "0"), leg("2", "100.00000001", "0"), pos("3", "100.00000001", "0")},
		{"a repeating decimal", pos("1", "10", "0"), leg("2", "10.5", "0"), pos("3", "10.33333333", "0")},
		{"tiny crypto quantities keep their precision", pos("0.000000000000000001", "50000", "0"), leg("0.000000000000000002", "50000", "0"), pos("0.000000000000000003", "50000", "0")},
		{"a position at zero quantity restarts at the buy price", pos("0", "999", "4"), leg("1", "10", "0"), pos("1", "10", "4")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.start.Buy(c.buy)
			assert.True(t, c.want.Equal(got), "want %+v got %+v", c.want, got)
		})
	}
}

func TestSellRealizesProfitAndKeepsTheAverage(t *testing.T) {
	t.Run("acceptance: selling 0.4 at 80,000 with fee 10 realizes 5,990", func(t *testing.T) {
		after, realized, err := pos("1", "65000", "0").Sell(leg("0.4", "80000", "10"))
		require.NoError(t, err)
		assert.True(t, d("5990").Equal(realized), realized.String())
		assert.True(t, pos("0.6", "65000", "0").Equal(after), "the average stays the same")
	})
	t.Run("a loss", func(t *testing.T) {
		_, realized, err := pos("10", "100", "0").Sell(leg("4", "90", "2"))
		require.NoError(t, err)
		assert.True(t, d("-42").Equal(realized), "(90−100)×4 − 2")
	})
	t.Run("break even minus the fee", func(t *testing.T) {
		_, realized, err := pos("10", "100", "0").Sell(leg("10", "100", "5"))
		require.NoError(t, err)
		assert.True(t, d("-5").Equal(realized))
	})
	t.Run("selling everything leaves zero quantity but keeps average and fees", func(t *testing.T) {
		after, _, err := pos("3", "10", "7").Sell(leg("3", "12", "0"))
		require.NoError(t, err)
		assert.True(t, pos("0", "10", "7").Equal(after))
	})
	t.Run("fees already paid are not touched by a sale", func(t *testing.T) {
		after, _, err := pos("10", "100", "25").Sell(leg("1", "110", "3"))
		require.NoError(t, err)
		assert.True(t, d("25").Equal(after.Fees))
	})
}

func TestSellCannotExceedWhatIsAvailable(t *testing.T) {
	expectInsufficient := func(t *testing.T, err error, available string) {
		t.Helper()
		require.Error(t, err)
		ae, ok := apperror.As(err)
		require.True(t, ok)
		assert.Equal(t, CodeInsufficientQuantity, ae.Code)
		assert.Equal(t, 422, ae.HTTPStatus)
		assert.Equal(t, available, ae.Details["available"])
	}
	_, _, err := pos("1", "100", "0").Sell(leg("1.000000000000000001", "100", "0"))
	expectInsufficient(t, err, "1")

	t.Run("the whole quantity can be sold", func(t *testing.T) {
		after, _, err := pos("2", "100", "0").Sell(leg("2", "100", "0"))
		require.NoError(t, err)
		assert.True(t, after.Quantity.IsZero())
	})
	t.Run("an empty position cannot be sold from", func(t *testing.T) {
		_, _, err := pos("0", "0", "0").Sell(leg("0.1", "1", "0"))
		expectInsufficient(t, err, "0")
	})
}

func TestBuyThenSellRoundTrip(t *testing.T) {
	start := pos("1", "65000", "10")
	bought := start.Buy(leg("2", "50000", "5"))
	assert.True(t, pos("3", "55000", "15").Equal(bought))
	sold, realized, err := bought.Sell(leg("2", "60000", "0"))
	require.NoError(t, err)
	assert.True(t, d("10000").Equal(realized), "(60000−55000)×2")
	assert.True(t, pos("1", "55000", "15").Equal(sold))
}

func TestPositionSnapshotRoundTrip(t *testing.T) {
	p := pos("0.6", "65000.12345678", "10.5")
	raw, err := p.MarshalJSON()
	require.NoError(t, err)
	assert.JSONEq(t, `{"quantity":"0.6","avgBuyPrice":"65000.12345678","fees":"10.5"}`, string(raw))
	back, err := ParsePosition(raw)
	require.NoError(t, err)
	assert.True(t, p.Equal(back))

	for _, bad := range []string{``, `{`, `{"quantity":"x","avgBuyPrice":"1","fees":"0"}`, `{"quantity":"1"}`} {
		_, err := ParsePosition([]byte(bad))
		assert.Error(t, err, bad)
	}
}

func TestPositionOfStoredRow(t *testing.T) {
	inv := sqlc.Investment{Quantity: d("2"), AvgBuyPrice: d("10"), Fees: d("1")}
	assert.True(t, pos("2", "10", "1").Equal(PositionOf(inv)))
	assert.False(t, pos("2", "10", "1").Equal(pos("2", "10", "2")))
	assert.False(t, pos("2", "10", "1").Equal(pos("3", "10", "1")))
	assert.False(t, pos("2", "10", "1").Equal(pos("2", "11", "1")))
	assert.True(t, d("1000").Equal(leg("10", "100", "0").Gross()))
}
