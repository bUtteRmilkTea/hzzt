package investments

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JonathanRainee/hzzt/internal/currency"
	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
)

func d(s string) decimal.Decimal   { return decimal.RequireFromString(s) }
func dp(s string) *decimal.Decimal { v := d(s); return &v }

func nd(s string) decimal.NullDecimal {
	if s == "" {
		return decimal.NullDecimal{}
	}
	return decimal.NullDecimal{Decimal: d(s), Valid: true}
}

// near reports whether a and b agree to 12 decimal places.
func near(a, b decimal.Decimal) bool { return a.Sub(b).Abs().LessThan(d("0.000000000001")) }

var now = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

func ctx(base string, auto bool, live, manual map[string]string) *currency.ConversionContext {
	cc := &currency.ConversionContext{BaseCurrency: base, AutoFX: auto,
		Rates: map[string]decimal.Decimal{}, ManualRates: map[string]decimal.Decimal{}}
	for k, v := range live {
		cc.Rates[k] = d(v)
	}
	for k, v := range manual {
		cc.ManualRates[k] = d(v)
	}
	return cc
}

var usdIDR = map[string]string{"IDR": "16000"} // 1 USD = 16000 IDR

func inv(assetType, quote, qty, avg, price, fees string) sqlc.Investment {
	return sqlc.Investment{
		AssetType: assetType, PriceMode: PriceModeManual, Name: assetType, QuoteCurrency: quote,
		Quantity: d(qty), AvgBuyPrice: d(avg), ManualPrice: d(price), Fees: d(fees),
	}
}

func TestValueManualPriceFixtures(t *testing.T) {
	type want struct{ price, mv, cost, pl, pct string }
	cases := []struct {
		name string
		inv  sqlc.Investment
		want want
	}{
		{"BBCA, 2 lots = 200 shares (quantity already derived)", inv(TypeIDStock, "IDR", "200", "9200", "9500", "5000"),
			want{"9500", "1900000", "1845000", "55000", "2.9810298102981"}},
		{"BTC in profit", inv(TypeCrypto, "USD", "0.5", "60000", "75000", "0"),
			want{"75000", "37500", "30000", "7500", "25"}},
		{"US stock at a loss", inv(TypeUSStock, "USD", "10", "200", "150", "10"),
			want{"150", "1500", "2010", "-510", "-25.373134328358"}},
		{"mutual fund: units × NAV", inv(TypeMutualFund, "IDR", "1000.5", "1500", "1600", "0"),
			want{"1600", "1600800", "1500750", "100050", "6.666666666667"}},
		{"global ETF with fees", inv(TypeGlobalETF, "USD", "3", "100", "110", "1.5"),
			want{"110", "330", "301.5", "28.5", "9.452736318408"}},
		{"break-even", inv(TypeUSStock, "USD", "1", "10", "10", "0"), want{"10", "10", "10", "0", "0"}},
	}
	cc := ctx("USD", true, usdIDR, nil)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// value everything in its own currency by using a context whose base is the quote currency
			own := ctx(c.inv.QuoteCurrency, true, usdIDR, nil)
			v := Value(c.inv, nil, own, now)
			assert.True(t, d(c.want.price).Equal(v.Price))
			assert.True(t, d(c.want.mv).Equal(v.MarketValue), "mv %s", v.MarketValue)
			assert.True(t, d(c.want.cost).Equal(v.CostBasis), "cost %s", v.CostBasis)
			assert.True(t, d(c.want.pl).Equal(v.UnrealizedPL), "pl %s", v.UnrealizedPL)
			require.NotNil(t, v.UnrealizedPLPct)
			assert.True(t, near(d(c.want.pct), *v.UnrealizedPLPct) || d(c.want.pct).Sub(*v.UnrealizedPLPct).Abs().LessThan(d("0.000000001")),
				"pct want %s got %s", c.want.pct, v.UnrealizedPLPct)
			assert.Equal(t, SourceManual, v.PriceSource)
			assert.Nil(t, v.PriceAsOf)
			assert.False(t, v.IsStale)
			require.NotNil(t, v.Base)
			assert.True(t, d("1").Equal(v.Base.FxRate), "same currency needs no rate")
		})
	}
	_ = cc
}

func TestManualCurrentValueOverridesQuantityTimesPrice(t *testing.T) {
	i := inv(TypeIDStock, "IDR", "200", "9200", "9500", "0")
	i.ManualCurrentValue = nd("2000000")
	v := Value(i, nil, ctx("IDR", true, nil, nil), now)
	assert.True(t, d("2000000").Equal(v.MarketValue), "not 200 × 9500 = 1,900,000")
	assert.Equal(t, SourceManualValue, v.PriceSource)
	assert.True(t, d("9500").Equal(v.Price), "the unit price is still reported")
	assert.True(t, d("160000").Equal(v.UnrealizedPL))
	require.NotNil(t, v.UnrealizedPLPct)
	assert.True(t, near(d("160000").Div(d("1840000")).Mul(d("100")), *v.UnrealizedPLPct))

	i.ManualCurrentValue = nd("0")
	v = Value(i, nil, ctx("IDR", true, nil, nil), now)
	assert.True(t, v.MarketValue.IsZero(), "an explicit 0 value counts as set")
	assert.Equal(t, SourceManualValue, v.PriceSource)
}

func TestZeroCostBasisHasNoPercentage(t *testing.T) {
	i := inv(TypeCrypto, "USD", "2", "0", "100", "0") // airdrop
	v := Value(i, nil, ctx("USD", true, nil, nil), now)
	assert.True(t, d("200").Equal(v.UnrealizedPL))
	assert.Nil(t, v.UnrealizedPLPct)
	i.Quantity = d("0")
	v = Value(i, nil, ctx("USD", true, nil, nil), now)
	assert.True(t, v.MarketValue.IsZero())
	assert.Nil(t, v.UnrealizedPLPct)
}

func TestAutoPriceUsesMarketThenFallsBack(t *testing.T) {
	cc := ctx("USD", true, nil, nil)
	btc := inv(TypeCrypto, "USD", "0.5", "60000", "65000", "0")
	btc.PriceMode = PriceModeAuto

	t.Run("market price wins", func(t *testing.T) {
		mp := &MarketPrice{Price: d("70000"), AsOf: now.Add(-5 * time.Minute)}
		v := Value(btc, mp, cc, now)
		assert.True(t, d("70000").Equal(v.Price))
		assert.True(t, d("35000").Equal(v.MarketValue))
		assert.True(t, d("5000").Equal(v.UnrealizedPL))
		assert.Equal(t, SourceMarket, v.PriceSource)
		require.NotNil(t, v.PriceAsOf)
		assert.True(t, mp.AsOf.Equal(*v.PriceAsOf))
		assert.False(t, v.IsStale)
	})
	t.Run("stale after 30 minutes", func(t *testing.T) {
		for age, stale := range map[time.Duration]bool{29 * time.Minute: false, 30 * time.Minute: false, 31 * time.Minute: true, 5 * time.Hour: true} {
			v := Value(btc, &MarketPrice{Price: d("70000"), AsOf: now.Add(-age)}, cc, now)
			assert.Equal(t, stale, v.IsStale, "age %s", age)
			assert.True(t, d("70000").Equal(v.Price), "a stale price is still used (last known price)")
		}
	})
	t.Run("no market price falls back to the manual price", func(t *testing.T) {
		v := Value(btc, nil, cc, now)
		assert.True(t, d("65000").Equal(v.Price))
		assert.Equal(t, SourceManual, v.PriceSource)
		assert.Nil(t, v.PriceAsOf)
		assert.False(t, v.IsStale)
	})
	t.Run("MANUAL mode ignores the market price", func(t *testing.T) {
		m := btc
		m.PriceMode = PriceModeManual
		v := Value(m, &MarketPrice{Price: d("70000"), AsOf: now}, cc, now)
		assert.True(t, d("65000").Equal(v.Price))
		assert.Equal(t, SourceManual, v.PriceSource)
	})
	t.Run("manual current value beats the market price", func(t *testing.T) {
		m := btc
		m.ManualCurrentValue = nd("40000")
		v := Value(m, &MarketPrice{Price: d("70000"), AsOf: now}, cc, now)
		assert.True(t, d("40000").Equal(v.MarketValue))
		assert.Equal(t, SourceManualValue, v.PriceSource)
	})
}

func TestConversionToBaseCurrency(t *testing.T) {
	usd := func() sqlc.Investment { return inv(TypeUSStock, "USD", "10", "100", "110", "0") } // cost 1000, value 1100

	t.Run("live rates convert through the context", func(t *testing.T) {
		v := Value(usd(), nil, ctx("IDR", true, usdIDR, nil), now)
		require.NotNil(t, v.Base)
		assert.Equal(t, "IDR", v.Base.Currency)
		assert.True(t, d("17600000").Equal(v.Base.MarketValue))
		assert.True(t, d("16000000").Equal(v.Base.CostBasis))
		assert.True(t, d("1600000").Equal(v.Base.UnrealizedPL))
		assert.True(t, d("16000").Equal(v.Base.FxRate))
		assert.True(t, d("1100").Equal(v.MarketValue), "native values stay native")
		assert.Empty(t, v.Warnings)
	})

	t.Run("acceptance: auto FX off + manual_fx_rate 16,000 converts with 16,000", func(t *testing.T) {
		i := usd()
		i.ManualFxRate = nd("16000")
		// the space's own manual rate says 15000; the per-investment rate must win
		v := Value(i, nil, ctx("IDR", false, map[string]string{"IDR": "17000"}, map[string]string{"USD/IDR": "15000"}), now)
		require.NotNil(t, v.Base)
		assert.True(t, d("16000").Equal(v.Base.FxRate))
		assert.True(t, d("17600000").Equal(v.Base.MarketValue))
	})

	t.Run("with auto FX on the per-investment rate is ignored", func(t *testing.T) {
		i := usd()
		i.ManualFxRate = nd("12345")
		v := Value(i, nil, ctx("IDR", true, usdIDR, nil), now)
		assert.True(t, d("16000").Equal(v.Base.FxRate))
	})

	t.Run("manual FX mode without a per-investment rate uses the space's manual rate", func(t *testing.T) {
		v := Value(usd(), nil, ctx("IDR", false, usdIDR, map[string]string{"USD/IDR": "15000"}), now)
		require.NotNil(t, v.Base)
		assert.True(t, d("15000").Equal(v.Base.FxRate))
	})

	t.Run("a same-currency investment never needs a rate", func(t *testing.T) {
		i := inv(TypeIDStock, "IDR", "100", "100", "110", "0")
		i.ManualFxRate = nd("99")
		v := Value(i, nil, ctx("IDR", false, nil, nil), now)
		require.NotNil(t, v.Base)
		assert.True(t, d("1").Equal(v.Base.FxRate))
	})

	t.Run("an inverse manual rate works for IDR → USD", func(t *testing.T) {
		i := inv(TypeIDStock, "IDR", "100", "100", "110", "0")
		v := Value(i, nil, ctx("USD", false, nil, map[string]string{"USD/IDR": "16000"}), now)
		require.NotNil(t, v.Base)
		assert.True(t, near(d("1").Div(d("16000")), v.Base.FxRate))
	})

	t.Run("a missing rate yields no base values and a warning, never an error", func(t *testing.T) {
		v := Value(usd(), nil, ctx("EUR", true, usdIDR, nil), now)
		assert.Nil(t, v.Base)
		require.Len(t, v.Warnings, 1)
		assert.Equal(t, currency.CodeFXRateUnavailable, v.Warnings[0].Code)
		assert.Contains(t, v.Warnings[0].Message, "USD→EUR")
		assert.True(t, d("1100").Equal(v.MarketValue), "native figures are still there")
	})
}

func TestSummarize(t *testing.T) {
	cc := ctx("USD", true, usdIDR, nil)
	a := Value(inv(TypeUSStock, "USD", "10", "100", "110", "0"), nil, cc, now)     // value 1100, cost 1000
	b := Value(inv(TypeIDStock, "IDR", "100", "16000", "8000", "0"), nil, cc, now) // value 800000 IDR = 50 USD, cost 1,600,000 IDR = 100 USD
	ci, c2i := inv(TypeMutualFund, "EUR", "5", "10", "12", "0"), inv(TypeMutualFund, "EUR", "7", "10", "12", "0")
	ci.Name, c2i.Name = "Fund one", "Fund two"
	c, c2 := Value(ci, nil, cc, now), Value(c2i, nil, cc, now) // no EUR rate for either

	s := Summarize("USD", []Valuation{a, b, c, c2})
	assert.Equal(t, "USD", s.Currency)
	assert.True(t, d("1150").Equal(s.TotalValue), s.TotalValue.String())
	assert.True(t, d("1100").Equal(s.TotalCost))
	assert.True(t, d("50").Equal(s.UnrealizedPL))
	require.NotNil(t, s.UnrealizedPLPct)
	assert.True(t, near(d("50").Div(d("1100")).Mul(d("100")), *s.UnrealizedPLPct))
	assert.Len(t, s.Warnings, 2, "one warning per left-out investment")

	empty := Summarize("IDR", nil)
	assert.True(t, empty.TotalValue.IsZero())
	assert.Nil(t, empty.UnrealizedPLPct)
	assert.NotNil(t, empty.Warnings)
	assert.Equal(t, "IDR", empty.Currency)
}

func TestOwnershipFactor(t *testing.T) {
	assert.True(t, d("1").Equal(OwnershipFactor(nil)), "an unshared investment is fully owned")
	assert.True(t, d("0.3").Equal(OwnershipFactor(&Share{MyBps: 3000, AllBps: []int{7000, 3000}, MyIndex: 1})))
	assert.True(t, d("0.0001").Equal(OwnershipFactor(&Share{MyBps: 1})))
}

func TestAllocateSplitsTheBaseValueExactly(t *testing.T) {
	// 1,000.01 USD of value shared 50/30/20: the parts must add up exactly
	cc := ctx("USD", true, nil, nil)
	i := inv(TypeUSStock, "USD", "1", "0", "1000.01", "0")
	total := Value(i, nil, cc, now)
	require.NotNil(t, total.Base)

	all := []int{5000, 3000, 2000}
	var parts []Valuation
	for idx, bps := range all {
		parts = append(parts, Allocate(total, &Share{MyBps: bps, AllBps: all, MyIndex: idx}, "USD"))
	}
	assert.True(t, d("500.01").Equal(parts[0].Base.MarketValue), parts[0].Base.MarketValue.String())
	assert.True(t, d("300").Equal(parts[1].Base.MarketValue))
	assert.True(t, d("200").Equal(parts[2].Base.MarketValue))
	assert.True(t, d("1000.01").Equal(parts[0].Base.MarketValue.Add(parts[1].Base.MarketValue).Add(parts[2].Base.MarketValue)))

	t.Run("native figures scale by the factor and the total is untouched", func(t *testing.T) {
		assert.True(t, d("300.003").Equal(parts[1].MarketValue))
		assert.True(t, d("1000.01").Equal(total.MarketValue))
		assert.Equal(t, total.Price, parts[1].Price)
	})
	t.Run("profit and loss add up too", func(t *testing.T) {
		withCost := inv(TypeUSStock, "USD", "1", "333.33", "1000.01", "0")
		whole := Value(withCost, nil, cc, now)
		sumPL, sumCost := decimal.Zero, decimal.Zero
		for idx, bps := range all {
			a := Allocate(whole, &Share{MyBps: bps, AllBps: all, MyIndex: idx}, "USD")
			assert.True(t, a.Base.MarketValue.Sub(a.Base.CostBasis).Equal(a.Base.UnrealizedPL), "pl = value − cost per share")
			sumPL, sumCost = sumPL.Add(a.Base.UnrealizedPL), sumCost.Add(a.Base.CostBasis)
		}
		assert.True(t, whole.Base.UnrealizedPL.Round(2).Equal(sumPL), "%s vs %s", whole.Base.UnrealizedPL, sumPL)
		assert.True(t, whole.Base.CostBasis.Round(2).Equal(sumCost))
	})
	t.Run("IDR has no minor units", func(t *testing.T) {
		idr := inv(TypeIDStock, "IDR", "100", "0", "1001", "0") // 100,100 IDR
		whole := Value(idr, nil, ctx("IDR", true, nil, nil), now)
		a := Allocate(whole, &Share{MyBps: 3333, AllBps: []int{3333, 3333, 3334}, MyIndex: 0}, "IDR")
		assert.True(t, a.Base.MarketValue.Round(0).Equal(a.Base.MarketValue), "whole rupiah: %s", a.Base.MarketValue)
	})
	t.Run("a nil share returns the whole valuation", func(t *testing.T) {
		assert.Equal(t, total, Allocate(total, nil, "USD"))
	})
	t.Run("without a base valuation only the native figures scale", func(t *testing.T) {
		noRate := Value(inv(TypeMutualFund, "EUR", "1", "0", "10", "0"), nil, cc, now)
		require.Nil(t, noRate.Base)
		a := Allocate(noRate, &Share{MyBps: 5000, AllBps: []int{5000, 5000}, MyIndex: 0}, "USD")
		assert.Nil(t, a.Base)
		assert.True(t, d("5").Equal(a.MarketValue))
	})
}

func TestValueWarnings(t *testing.T) {
	cc := ctx("USD", true, usdIDR, nil)
	codes := func(v Valuation) []string {
		out := []string{}
		for _, w := range v.Warnings {
			out = append(out, w.Code)
		}
		return out
	}

	t.Run("a stale market price counts and is flagged PRICE_STALE", func(t *testing.T) {
		i := inv(TypeCrypto, "USD", "1", "1000", "1000", "0")
		i.PriceMode = PriceModeAuto
		v := Value(i, &MarketPrice{Price: d("64000"), AsOf: now.Add(-3 * time.Hour)}, cc, now)
		assert.True(t, v.IsStale)
		assert.Equal(t, d("64000"), v.MarketValue, "the stale price still counts")
		assert.Equal(t, []string{CodePriceStale}, codes(v))
	})

	t.Run("a fresh market price has no warning", func(t *testing.T) {
		i := inv(TypeCrypto, "USD", "1", "1000", "1000", "0")
		i.PriceMode = PriceModeAuto
		v := Value(i, &MarketPrice{Price: d("64000"), AsOf: now.Add(-time.Minute)}, cc, now)
		assert.Empty(t, v.Warnings)
	})

	t.Run("a manual value on an AUTO holding wins and says so", func(t *testing.T) {
		i := inv(TypeCrypto, "USD", "1", "1000", "1000", "0")
		i.PriceMode = PriceModeAuto
		i.ManualCurrentValue = nd("40000")
		v := Value(i, &MarketPrice{Price: d("64000"), AsOf: now}, cc, now)
		assert.Equal(t, d("40000"), v.MarketValue)
		assert.Equal(t, SourceManualValue, v.PriceSource)
		assert.Equal(t, []string{CodeManualValueOver}, codes(v))
	})

	t.Run("a manual value on a MANUAL holding has no warning", func(t *testing.T) {
		i := inv(TypeCrypto, "USD", "1", "1000", "1000", "0")
		i.ManualCurrentValue = nd("40000")
		assert.Empty(t, Value(i, nil, cc, now).Warnings)
	})

	t.Run("only FX warnings reach the portfolio summary", func(t *testing.T) {
		i := inv(TypeCrypto, "IDR", "1", "1", "1", "0")
		i.PriceMode = PriceModeAuto
		v := Value(i, &MarketPrice{Price: d("1"), AsOf: now.Add(-3 * time.Hour)}, ctx("EUR", true, nil, nil), now)
		require.Nil(t, v.Base)
		s := Summarize("EUR", []Valuation{v})
		require.Len(t, s.Warnings, 1)
		assert.Equal(t, currency.CodeFXRateUnavailable, s.Warnings[0].Code)
	})
}
