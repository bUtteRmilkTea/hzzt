package investments

import (
	"math/rand"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JonathanRainee/hzzt/internal/ledger"
	"github.com/JonathanRainee/hzzt/internal/platform/apperror"
)

func dFromInt(n int) decimal.Decimal { return decimal.NewFromInt(int64(n)) }

func detailsOf(t *testing.T, err error) map[string]any {
	t.Helper()
	ae, ok := apperror.As(err)
	require.True(t, ok)
	return ae.Details
}

func step(action, qty, price, fee string) TradeStep {
	return TradeStep{ID: uuid.New(), Action: action, Leg: leg(qty, price, fee)}
}

func TestReplayPositions(t *testing.T) {
	t.Run("nothing to replay", func(t *testing.T) {
		out, err := ReplayPositions(pos("2", "100", "1"), nil)
		require.NoError(t, err)
		assert.Empty(t, out)
	})

	t.Run("buys and sells recompute average, fees and realised profit", func(t *testing.T) {
		start := pos("0", "0", "0")
		steps := []TradeStep{
			step(ledger.ActionBuy, "10", "100", "5"),
			step(ledger.ActionBuy, "10", "200", "5"),
			step(ledger.ActionSell, "5", "250", "2"),
		}
		out, err := ReplayPositions(start, steps)
		require.NoError(t, err)
		require.Len(t, out, 3)
		assert.True(t, d("150").Equal(out[1].After.AvgBuyPrice))
		assert.True(t, d("20").Equal(out[1].After.Quantity))
		assert.True(t, d("10").Equal(out[1].After.Fees))
		assert.Nil(t, out[0].Realized)
		require.NotNil(t, out[2].Realized)
		assert.True(t, d("498").Equal(*out[2].Realized), "(250-150)*5-2: %s", out[2].Realized)
		assert.True(t, out[2].Before.Equal(out[1].After), "each step starts where the one before ended")
		assert.True(t, d("15").Equal(out[2].After.Quantity))
	})

	t.Run("a sell that no longer fits is reported with its id and changes nothing", func(t *testing.T) {
		sell := step(ledger.ActionSell, "8", "100", "0")
		_, err := ReplayPositions(pos("0", "0", "0"), []TradeStep{step(ledger.ActionBuy, "5", "100", "0"), sell})
		require.Error(t, err)
		assert.Equal(t, CodeTradeDeleteBreaksLaterSell, codeOf(t, err))
		assert.Equal(t, sell.ID.String(), detailsOf(t, err)["tradeId"])
	})

	t.Run("replaying from the opening position without one trade equals a fresh build", func(t *testing.T) {
		b1, b2, s1 := step(ledger.ActionBuy, "10", "100", "1"), step(ledger.ActionBuy, "5", "130", "1"), step(ledger.ActionSell, "4", "150", "1")
		full, err := ReplayPositions(pos("3", "90", "0"), []TradeStep{b1, b2, s1})
		require.NoError(t, err)
		// delete b2: replay from b2's position before, over the later steps
		after, err := ReplayPositions(full[1].Before, []TradeStep{s1})
		require.NoError(t, err)
		fresh, err := ReplayPositions(pos("3", "90", "0"), []TradeStep{b1, s1})
		require.NoError(t, err)
		assert.True(t, fresh[1].After.Equal(after[0].After))
		assert.True(t, fresh[1].Realized.Equal(*after[0].Realized))
	})
}

// Property: build a random history, delete a random trade, and compare the replay with a fresh build of
// the remaining trades. A delete that would break a later sell must be refused by both.
func TestReplayPropertyDeleteEqualsFreshBuild(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	refused, applied := 0, 0
	for n := 0; n < 200; n++ {
		opening := pos("0", "0", "0")
		if rng.Intn(2) == 0 {
			opening = pos("5", "50", "2")
		}
		var steps []TradeStep
		held := opening.Quantity
		for i, count := 0, 2+rng.Intn(8); i < count; i++ {
			price := 10 + rng.Intn(190)
			fee := rng.Intn(5)
			if held.IsPositive() && rng.Intn(3) == 0 {
				q := held.Mul(d("0.5")).Round(2)
				if !q.IsPositive() {
					q = held
				}
				steps = append(steps, TradeStep{ID: uuid.New(), Action: ledger.ActionSell, Leg: leg(q.String(), d("1").Mul(dFromInt(price)).String(), dFromInt(fee).String())})
				held = held.Sub(q)
			} else {
				q := dFromInt(1 + rng.Intn(20))
				steps = append(steps, TradeStep{ID: uuid.New(), Action: ledger.ActionBuy, Leg: leg(q.String(), dFromInt(price).String(), dFromInt(fee).String())})
				held = held.Add(q)
			}
		}
		full, err := ReplayPositions(opening, steps)
		require.NoError(t, err, "the generated history is valid")

		k := rng.Intn(len(steps))
		remaining := append(append([]TradeStep{}, steps[:k]...), steps[k+1:]...)
		fresh, freshErr := ReplayPositions(opening, remaining)
		got, gotErr := ReplayPositions(full[k].Before, steps[k+1:])
		if freshErr != nil {
			require.Error(t, gotErr, "both refuse")
			assert.Equal(t, CodeTradeDeleteBreaksLaterSell, codeOf(t, gotErr))
			refused++
			continue
		}
		require.NoError(t, gotErr)
		applied++
		finalFresh := opening
		if len(fresh) > 0 {
			finalFresh = fresh[len(fresh)-1].After
		}
		finalGot := full[k].Before
		if len(got) > 0 {
			finalGot = got[len(got)-1].After
		}
		assert.True(t, finalFresh.Equal(finalGot), "final position, run %d", n)
		for i := range got {
			assert.True(t, fresh[k+i].After.Equal(got[i].After), "run %d trade %d", n, k+i)
			if got[i].Realized != nil {
				require.NotNil(t, fresh[k+i].Realized)
				assert.True(t, fresh[k+i].Realized.Equal(*got[i].Realized), "realised, run %d trade %d", n, k+i)
			}
		}
	}
	t.Logf("applied %d, refused %d", applied, refused)
	assert.Positive(t, applied)
}
