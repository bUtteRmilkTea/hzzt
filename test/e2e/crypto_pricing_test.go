//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JonathanRainee/hzzt/internal/marketdata/coinmarketcap"
)

func cryptoNoMode(sym, qty string, extra ...any) map[string]any {
	return clone(map[string]any{"assetType": "CRYPTO", "name": sym, "symbol": sym, "quantity": qty,
		"avgBuyPrice": "1000", "manualPrice": "1000"}, extra...)
}

func warnCodes(ws []struct{ Code, Message, Label string }) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = w.Code
	}
	return out
}

// G-T1/T2/T3: a crypto holding created with the defaults is priced by the market (CMC), not by the
// price typed once; what cannot be linked falls back to MANUAL with a warning.
func TestCryptoDefaultsToAutoPricing(t *testing.T) {
	s := NewTestServer(t)
	e := newInvEnv(s)
	e.setSpace(map[string]any{"baseCurrency": "USD"})

	t.Run("a crypto without priceMode is AUTO and linked to its coin", func(t *testing.T) {
		got := e.must(cryptoNoMode("BTC", "0.5"))
		assert.Equal(t, "AUTO", got.PriceMode)
		require.NotNil(t, got.InstrumentID)
		assert.Equal(t, "MARKET", got.Valuation.PriceSource)
		assert.True(t, dec("31500").Equal(dec(got.Valuation.MarketValue)), "0.5 x 63000, not 0.5 x the typed 1000")
		assert.True(t, dec("1000").Equal(dec(got.AvgBuyPrice)), "the bought price is never overwritten")
		assert.True(t, dec("1000").Equal(dec(got.ManualPrice)), "the typed price stays as the fallback")
		assert.Empty(t, got.Valuation.Warnings)
	})

	t.Run("an unknown symbol falls back to MANUAL with PRICE_NOT_LINKED instead of failing", func(t *testing.T) {
		got := e.must(cryptoNoMode("NOPE", "2"))
		assert.Equal(t, "MANUAL", got.PriceMode)
		assert.Nil(t, got.InstrumentID)
		assert.Equal(t, "MANUAL", got.Valuation.PriceSource)
		assert.True(t, dec("2000").Equal(dec(got.Valuation.MarketValue)))
		require.Len(t, got.Valuation.Warnings, 1)
		assert.Equal(t, "PRICE_NOT_LINKED", got.Valuation.Warnings[0].Code)
	})

	t.Run("an ambiguous symbol falls back to MANUAL with the warning", func(t *testing.T) {
		s.CMC.Coins = append(s.CMC.Coins,
			coinmarketcap.Coin{ID: 98001, Symbol: "DUP", Name: "Dup One", Rank: 800, Active: true},
			coinmarketcap.Coin{ID: 98002, Symbol: "DUP", Name: "Dup Two", Rank: 900, Active: true})
		_, err := s.Market.SyncCoinMap(context.Background())
		require.NoError(t, err)
		got := e.must(cryptoNoMode("DUP", "1"))
		assert.Equal(t, "MANUAL", got.PriceMode)
		require.NotEmpty(t, got.Valuation.Warnings)
		assert.Equal(t, "PRICE_NOT_LINKED", got.Valuation.Warnings[0].Code)
		// BTC shares its symbol with a clone but is clearly #1: not ambiguous
		assert.Equal(t, "AUTO", e.must(cryptoNoMode("BTC", "1")).PriceMode)
	})

	t.Run("an explicit MANUAL stays MANUAL even for a known coin", func(t *testing.T) {
		got := e.must(cryptoNoMode("ETH", "1", "priceMode", "MANUAL"))
		assert.Equal(t, "MANUAL", got.PriceMode)
		assert.Nil(t, got.InstrumentID)
		assert.True(t, dec("1000").Equal(dec(got.Valuation.Price)))
	})

	t.Run("an explicit AUTO with an unknown symbol still fails", func(t *testing.T) {
		res := e.post(cryptoNoMode("NOPE", "1", "priceMode", "AUTO"))
		require.Equal(t, http.StatusUnprocessableEntity, res.Status)
		assert.Equal(t, "INSTRUMENT_NOT_FOUND", errCode(t, res))
	})

	t.Run("the type metadata advertises AUTO for crypto only", func(t *testing.T) {
		res := s.Get("/api/v1/investment-types", bearer(e.u.AccessToken))
		require.Equal(t, http.StatusOK, res.Status)
		var types []struct {
			AssetType string `json:"assetType"`
			Defaults  struct {
				PriceMode string `json:"priceMode"`
			} `json:"defaults"`
		}
		res.JSON(t, &types)
		for _, ty := range types {
			if ty.AssetType == "CRYPTO" {
				assert.Equal(t, "AUTO", ty.Defaults.PriceMode)
			} else {
				assert.Equal(t, "MANUAL", ty.Defaults.PriceMode, ty.AssetType)
			}
		}
	})
}

// Acceptance: two crypto holdings are worth sum(quantity x CMC price) in the base currency, to the cent,
// and the total follows the provider price at the next refresh.
func TestCryptoPortfolioFollowsCMC(t *testing.T) {
	ctx := context.Background()
	s := NewTestServer(t)
	e := newInvEnv(s)
	e.setSpace(map[string]any{"baseCurrency": "EUR"}) // 1 USD = 0.8 EUR
	require.NoError(t, s.Currency.Refresh(ctx))
	e.must(cryptoNoMode("BTC", "0.5"))
	e.must(cryptoNoMode("ETH", "2"))

	total := func() string { return e.list("").Summary.TotalValue }
	assert.True(t, dec("28080").Equal(dec(total())), "(0.5 x 63000 + 2 x 1800) x 0.8 = %s", total())

	s.CMC.Quotes[1] = "70000.55"
	calls := s.CMC.PriceCalls
	res, err := s.Market.RefreshCryptoPrices(ctx)
	require.NoError(t, err)
	assert.Equal(t, "SUCCESS", res.Status)
	assert.Equal(t, calls+1, s.CMC.PriceCalls, "one batched CMC call for all holdings")
	want := dec("0.5").Mul(dec("70000.55")).Add(dec("3600")).Mul(dec("0.8")).Round(2)
	assert.True(t, want.Equal(dec(total())), "total %s want %s", total(), want)

	t.Run("a second holding of the same coin and a third coin still cost one call", func(t *testing.T) {
		e.must(cryptoNoMode("BTC", "1", "name", "Cold BTC"))
		e.must(cryptoNoMode("SOL", "10"))
		before := s.CMC.PriceCalls
		_, err := s.Market.RefreshCryptoPrices(ctx)
		require.NoError(t, err)
		assert.Equal(t, before+1, s.CMC.PriceCalls)
		require.NotEmpty(t, s.CMC.IDsAsked)
		assert.Len(t, s.CMC.IDsAsked[len(s.CMC.IDsAsked)-1], 3, "BTC, ETH and SOL, once each")
	})
}

// A stale market price still counts in the total (better than the typed price) and is flagged.
func TestStaleCryptoPriceStillCounts(t *testing.T) {
	ctx := context.Background()
	s := NewTestServer(t)
	e := newInvEnv(s)
	e.setSpace(map[string]any{"baseCurrency": "USD"})
	inv := e.must(cryptoNoMode("BTC", "1"))

	s.CMC.AsOf = s.Clock.Now().Add(-3 * time.Hour)
	s.CMC.Quotes[1] = "64000"
	_, err := s.Market.RefreshCryptoPrices(ctx)
	require.NoError(t, err)

	got := e.get(inv.ID)
	assert.True(t, got.Valuation.IsStale)
	assert.Equal(t, "MARKET", got.Valuation.PriceSource)
	assert.True(t, dec("64000").Equal(dec(got.Valuation.MarketValue)), "the stale CMC price, not the typed 1000")
	assert.Contains(t, warnCodesV(got.Valuation.Warnings), "PRICE_STALE")
	assert.True(t, dec("64000").Equal(dec(e.list("").Summary.TotalValue)))
}

func warnCodesV(ws []struct{ Code, Message, Label string }) []string { return warnCodes(ws) }

// A manual current value beats the market price (existing rule) but the client is told.
func TestManualValueOverridesMarketWarning(t *testing.T) {
	s := NewTestServer(t)
	e := newInvEnv(s)
	e.setSpace(map[string]any{"baseCurrency": "USD"})
	got := e.must(cryptoNoMode("BTC", "1", "manualCurrentValue", "40000"))
	assert.Equal(t, "AUTO", got.PriceMode)
	assert.Equal(t, "MANUAL_VALUE", got.Valuation.PriceSource)
	assert.True(t, dec("40000").Equal(dec(got.Valuation.MarketValue)))
	assert.Equal(t, []string{"MANUAL_VALUE_OVERRIDES_MARKET"}, warnCodesV(got.Valuation.Warnings))
}

// The Home number, the Assets number, the grouped view, the investments list and the history graph all use
// one valuation path and must show the same investments total for the same space and price.
func TestInvestmentTotalsAgreeEverywhere(t *testing.T) {
	ctx := context.Background()
	s := NewTestServer(t)
	e := newInvEnv(s)
	e.setSpace(map[string]any{"baseCurrency": "USD"})
	e.must(cryptoNoMode("BTC", "0.5"))
	e.must(cryptoNoMode("ETH", "3"))
	e.must(clone(aapl))
	s.CMC.Quotes[1] = "64123.45"
	_, err := s.Market.RefreshCryptoPrices(ctx)
	require.NoError(t, err)

	list := e.list("").Summary.TotalValue
	grouped := s.grouped(e.u, e.sid, "").Total.Value
	nw := s.networth(e.u, e.sid)
	require.NotNil(t, nw.Investments)
	assets := s.assetSummary(e.u, e.sid)
	require.NotNil(t, assets.Investments)
	hist := s.history(e.u, e.sid, "")

	want := dec("0.5").Mul(dec("64123.45")).Add(dec("5400")).Add(dec("0")) // BTC + ETH; AAPL added below
	_ = want
	assert.Equal(t, list, grouped, "grouped")
	assert.Equal(t, list, nw.Investments.Value, "net-worth summary")
	assert.Equal(t, list, assets.Investments.Total, "assets summary")
	require.NotEmpty(t, hist.Points)
	assert.True(t, hist.Points[len(hist.Points)-1].Live)
	_, err = s.Networth.SnapshotAll(ctx, zerologNop())
	require.NoError(t, err)
	var snap string
	require.NoError(t, s.Tx.QueryRow(ctx, `SELECT investments::text FROM net_worth_snapshots WHERE space_id = $1`, e.sid).Scan(&snap))
	assert.True(t, dec(list).Equal(dec(snap).Round(2)), "the snapshot job stores the market value: %s vs %s", snap, list)
	assert.True(t, dec(list).GreaterThan(dec("32000")), "BTC at the CMC price, not the typed 1000: %s", list)
}

// G-T7: the backfill flips only price_mode, only for linked crypto without a manual value, and never
// touches a price or cost column.
func TestCryptoAutoBackfillMigration(t *testing.T) {
	ctx := context.Background()
	s := NewTestServer(t)
	e := newInvEnv(s)
	e.setSpace(map[string]any{"baseCurrency": "USD"})
	var btc string
	for _, c := range s.searchCoins(e.u, "?q=btc") {
		if c.Rank != nil && *c.Rank == 1 {
			btc = c.InstrumentID
		}
	}
	require.NotEmpty(t, btc)

	ins := func(name, mode string, instrument any, manualValue any, deleted bool) string {
		var id string
		require.NoError(t, s.Tx.QueryRow(ctx, `
			INSERT INTO investments (space_id, asset_type, price_mode, name, symbol, icon, quote_currency, quantity, avg_buy_price,
			                         manual_price, fees, manual_current_value, created_by_id, instrument_id, deleted_at)
			VALUES ($1, 'CRYPTO', $2, $3, 'BTC', '🪙', 'USD', 2, 41234.5, 43000.25, 17.5, $4, $5, $6, CASE WHEN $7 THEN now() END)
			RETURNING id::text`, e.sid, mode, name, manualValue, e.u.User.ID, instrument, deleted).Scan(&id))
		return id
	}
	flip := ins("linked", "MANUAL", btc, nil, false)
	withValue := ins("has manual value", "MANUAL", btc, "50000", false)
	unlinked := ins("no instrument", "MANUAL", nil, nil, false)
	gone := ins("deleted", "MANUAL", btc, nil, true)
	already := ins("already auto", "AUTO", btc, nil, false)

	type row struct{ mode, avg, price, fees, cost string }
	snap := func() map[string]row {
		out := map[string]row{}
		rows, err := s.Tx.Query(ctx, `SELECT id::text, price_mode, avg_buy_price::text, manual_price::text, fees::text, quantity::text || '/' || coalesce(manual_current_value::text, '-') FROM investments WHERE space_id = $1`, e.sid)
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			var id string
			var r row
			require.NoError(t, rows.Scan(&id, &r.mode, &r.avg, &r.price, &r.fees, &r.cost))
			out[id] = r
		}
		return out
	}
	before := snap()
	s.runMigrationUp("00019_crypto_auto_price.sql")
	after := snap()

	for id, b := range before {
		a := after[id]
		assert.Equal(t, b.avg, a.avg, "avg_buy_price is never touched")
		assert.Equal(t, b.price, a.price, "manual_price is never touched")
		assert.Equal(t, b.fees, a.fees, "fees are never touched")
		assert.Equal(t, b.cost, a.cost, "quantity and manual value are never touched")
	}
	assert.Equal(t, "AUTO", after[flip].mode)
	assert.Equal(t, "MANUAL", after[withValue].mode, "a manual current value keeps it manual")
	assert.Equal(t, "MANUAL", after[unlinked].mode, "no instrument: listed, not changed")
	assert.Equal(t, "MANUAL", after[gone].mode, "deleted rows are left alone")
	assert.Equal(t, "AUTO", after[already].mode)
}
