package investments

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func wrapped(assetType, quote, qty, price string, wrapper *string, updated time.Time) Item {
	i := inv(assetType, quote, qty, "0", price, "0")
	i.Wrapper = wrapper
	i.UpdatedAt = updated
	return Item{Investment: i, Valuation: Value(i, nil, ctx("USD", true, usdIDR, nil), now)}
}

func TestGroupMergesWrappersCaseAndWhitespaceInsensitively(t *testing.T) {
	t0 := now
	items := []Item{
		wrapped(TypeCrypto, "USD", "1", "300", strp("Binance"), t0),
		wrapped(TypeCrypto, "USD", "1", "100", strp("  binance "), t0.Add(time.Hour)), // newer casing wins the label
		wrapped(TypeCrypto, "USD", "1", "100", strp("Ledger  Nano"), t0),
		wrapped(TypeCrypto, "USD", "1", "50", nil, t0),
		wrapped(TypeUSStock, "USD", "1", "450", strp("Robinhood"), t0),
	}
	g := Group("USD", items)
	require.Len(t, g.Categories, 2)
	crypto := g.Categories[0]
	assert.Equal(t, TypeCrypto, crypto.AssetType)
	require.Len(t, crypto.Wrappers, 3)
	bin := crypto.Wrappers[0]
	assert.Equal(t, "binance", bin.Key)
	assert.Equal(t, "binance", *bin.Wrapper, "the most recently written casing, whitespace trimmed")
	assert.Equal(t, 2, len(bin.Items))
	assert.True(t, d("400").Equal(bin.Value))
	assert.Equal(t, "ledger nano", crypto.Wrappers[1].Key, "inner whitespace collapsed")
	last := crypto.Wrappers[2]
	assert.Equal(t, UnassignedKey, last.Key, "unassigned sorts last")
	assert.Nil(t, last.Wrapper)
	assert.Equal(t, UnassignedLabel, groupLabel(last))
}

func TestGroupSumsEqualTheSummaryAndPercentagesAddUp(t *testing.T) {
	items := []Item{
		wrapped(TypeCrypto, "USD", "1", "300", strp("Binance"), now),
		wrapped(TypeCrypto, "USD", "1", "100", strp("Ledger"), now),
		wrapped(TypeCrypto, "USD", "1", "100", nil, now),
		wrapped(TypeUSStock, "USD", "1", "500", strp("Robinhood"), now),
		wrapped(TypeGlobalETF, "USD", "1", "0.01", strp("IBKR"), now),
	}
	var vals []Valuation
	for _, it := range items {
		vals = append(vals, it.Valuation)
	}
	sum := Summarize("USD", vals)
	g := Group("USD", items)
	assert.True(t, sum.TotalValue.Equal(g.Total.Value))
	assert.True(t, sum.TotalCost.Equal(g.Total.Cost))

	catSum, catPct := decimal.Zero, decimal.Zero
	wrapperPct := decimal.Zero
	for _, c := range g.Categories {
		catSum = catSum.Add(c.Value)
		catPct = catPct.Add(*c.PctOfTotal)
		inCat, inCatPct := decimal.Zero, decimal.Zero
		for _, w := range c.Wrappers {
			inCat = inCat.Add(w.Value)
			inCatPct = inCatPct.Add(*w.PctOfCategory)
			wrapperPct = wrapperPct.Add(*w.PctOfTotal)
		}
		assert.True(t, inCat.Equal(c.Value), "wrappers add up to the category")
		assert.True(t, inCatPct.Equal(d("100")), "%s pctOfCategory: %s", c.AssetType, inCatPct)
	}
	assert.True(t, catSum.Equal(g.Total.Value))
	assert.True(t, catPct.Equal(d("100")), catPct.String())
	assert.True(t, wrapperPct.Equal(d("100")), wrapperPct.String())
}

func TestGroupLeavesOutItemsWithoutFXAndWarns(t *testing.T) {
	good := wrapped(TypeMutualFund, "USD", "1", "100", strp("Bibit"), now)
	i := inv(TypeMutualFund, "EUR", "1", "0", "50", "0") // no EUR rate in the context
	i.Wrapper = strp("Bibit")
	noFX := Item{Investment: i, Valuation: Value(i, nil, ctx("USD", true, usdIDR, nil), now)}
	require.Nil(t, noFX.Valuation.Base)
	g := Group("USD", []Item{good, noFX})
	require.Len(t, g.Warnings, 1)
	w := g.Categories[0].Wrappers[0]
	assert.Equal(t, 2, len(w.Items), "listed")
	assert.True(t, d("100").Equal(w.Value), "but not summed")
	assert.True(t, d("100").Equal(g.Total.Value))
}

func TestGroupOfNothingAndOfZeroValues(t *testing.T) {
	g := Group("USD", nil)
	assert.Empty(t, g.Categories)
	assert.True(t, g.Total.Value.IsZero())
	assert.Nil(t, g.Total.UnrealizedPLPct)

	z := Group("USD", []Item{wrapped(TypeCrypto, "USD", "0", "10", strp("Binance"), now)})
	require.Len(t, z.Categories, 1)
	assert.Nil(t, z.Categories[0].PctOfTotal, "null when the denominator is 0")
	assert.Nil(t, z.Categories[0].Wrappers[0].PctOfCategory)
}

func TestItemPcts(t *testing.T) {
	items := []Item{
		wrapped(TypeCrypto, "USD", "1", "1", nil, now),
		wrapped(TypeCrypto, "USD", "1", "1", nil, now),
		wrapped(TypeCrypto, "USD", "1", "1", nil, now),
	}
	sum := decimal.Zero
	for _, p := range ItemPcts(items) {
		sum = sum.Add(*p)
	}
	assert.True(t, sum.Equal(d("100")))
}

func TestWrapperNormalizationAndDerivation(t *testing.T) {
	assert.Equal(t, "binance us", WrapperKey("  Binance \t US "))
	assert.Equal(t, "Binance US", NormalizeWrapper("  Binance \t US "))
	assert.Equal(t, "Binance", *DetailsWrapper(cryptoDetails{ExchangeOrWallet: strp(" Binance ")}))
	assert.Equal(t, "Stockbit", *DetailsWrapper(idStockDetails{Broker: strp("Stockbit")}))
	assert.Equal(t, "Robinhood", *DetailsWrapper(usStockDetails{Exchange: "NASDAQ", Platform: strp("Robinhood")}), "platform, not the listing venue")
	assert.Nil(t, DetailsWrapper(usStockDetails{Exchange: "NASDAQ"}))
	assert.Nil(t, DetailsWrapper(globalETFDetails{Exchange: "NYSE", Platform: strp("  ")}))
	assert.Nil(t, DetailsWrapper(mutualFundDetails{FundManager: strp("Manulife"), FundType: "EQUITY"}))
}

func TestGroupWrapperPnlAddsUpToTheCategoryAndTotal(t *testing.T) {
	mk := func(w *string, qty, avg, price string) Item {
		i := inv(TypeUSStock, "USD", qty, avg, price, "0")
		i.Wrapper = w
		i.UpdatedAt = now
		return Item{Investment: i, Valuation: Value(i, nil, ctx("USD", true, usdIDR, nil), now)}
	}
	items := []Item{
		mk(strp("Robinhood"), "3", "10.01", "12.337"),
		mk(strp("robinhood "), "7", "3.33", "2.111"),
		mk(strp("IBKR"), "1", "100", "90.005"),
		mk(nil, "2", "1.1", "1.7"),
	}
	g := Group("USD", items)
	require.Len(t, g.Categories, 1)
	cat := g.Categories[0]
	pl, cost := decimal.Zero, decimal.Zero
	for _, w := range cat.Wrappers {
		pl = pl.Add(w.UnrealizedPL)
		cost = cost.Add(w.Cost)
		var itemPL decimal.Decimal
		for _, it := range w.Items {
			itemPL = itemPL.Add(it.Valuation.Base.UnrealizedPL)
		}
		assert.True(t, itemPL.Equal(w.UnrealizedPL), "wrapper pnl equals the sum of its items")
		if !w.Cost.IsZero() {
			require.NotNil(t, w.UnrealizedPLPct)
		}
	}
	assert.True(t, pl.Equal(cat.UnrealizedPL))
	assert.True(t, cost.Equal(cat.Cost))
	assert.True(t, cat.UnrealizedPL.Equal(g.Total.UnrealizedPL))
}
