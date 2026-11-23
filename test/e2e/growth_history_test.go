//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type seriesBody struct {
	First            string  `json:"first"`
	Last             string  `json:"last"`
	Change           string  `json:"change"`
	ChangePct        *string `json:"changePct"`
	NetContributions string  `json:"netContributions"`
	MarketChange     string  `json:"marketChange"`
	PerformancePct   *string `json:"performancePct"`
	Direction        string  `json:"direction"`
}

type growthBody struct {
	Currency string `json:"currency"`
	Range    string `json:"range"`
	Points   []struct {
		Date        string `json:"date"`
		NetWorth    string `json:"netWorth"`
		Cash        string `json:"cash"`
		Investments string `json:"investments"`
		FixedAssets string `json:"fixedAssets"`
		Receivables string `json:"receivables"`
		Liabilities string `json:"liabilities"`
		Assets      string `json:"assets"`
		Live        bool   `json:"live"`
	} `json:"points"`
	Series struct {
		NetWorth    seriesBody `json:"netWorth"`
		Investments seriesBody `json:"investments"`
		FixedAssets seriesBody `json:"fixedAssets"`
		Assets      seriesBody `json:"assets"`
	} `json:"series"`
	Coverage struct {
		FirstPointDate string  `json:"firstPointDate"`
		RequestedFrom  *string `json:"requestedFrom"`
		Short          bool    `json:"short"`
	} `json:"coverage"`
	Warnings []struct{ Code, Message, Label string } `json:"warnings"`
}

func (s *TestServer) growth(u session, sid, query string) growthBody {
	s.t.Helper()
	res := s.Get("/api/v1/spaces/"+sid+"/net-worth/history"+query, bearer(u.AccessToken))
	require.Equal(s.t, http.StatusOK, res.Status, string(res.Body))
	var h growthBody
	res.JSON(s.t, &h)
	return h
}

// seedFullSnapshot stores a snapshot with every component.
func (s *TestServer) seedFullSnapshot(sid, date string, cash, inv, fixed, recv, liab string) {
	s.t.Helper()
	_, err := s.Tx.Exec(context.Background(), `
		INSERT INTO net_worth_snapshots (space_id, date, currency, cash, investments, fixed_assets, receivables, liabilities, net_worth)
		VALUES ($1, $2::date, 'USD', $3::numeric, $4::numeric, $5::numeric, $6::numeric, $7::numeric, $3::numeric + $4::numeric + $5::numeric + $6::numeric - $7::numeric)
		ON CONFLICT (space_id, date) DO UPDATE SET cash = EXCLUDED.cash, investments = EXCLUDED.investments, fixed_assets = EXCLUDED.fixed_assets,
		  receivables = EXCLUDED.receivables, liabilities = EXCLUDED.liabilities, net_worth = EXCLUDED.net_worth`,
		sid, date, cash, inv, fixed, recv, liab)
	require.NoError(s.t, err)
}

// K-T1: the history used to carry only netWorth and investments per point and no verdict per series.
func TestHistoryExposesEveryComponentAndASeriesVerdict(t *testing.T) {
	s := NewTestServer(t)
	s.Clock.Set(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	w := newWorld(t, s) // 1500 USD cash
	s.seedFullSnapshot(w.sid, "2026-11-11", "1500", "100", "5000", "200", "300")
	s.Clock.Set(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC))

	h := s.growth(w.u, w.sid, "?range=1M")
	require.Len(t, h.Points, 2)
	first := h.Points[0]
	assert.Equal(t, "2026-11-11", first.Date)
	assert.True(t, dec("1500").Equal(dec(first.Cash)))
	assert.True(t, dec("5000").Equal(dec(first.FixedAssets)))
	assert.True(t, dec("200").Equal(dec(first.Receivables)))
	assert.True(t, dec("300").Equal(dec(first.Liabilities)))
	assert.True(t, dec("6800").Equal(dec(first.Assets)), "cash + investments + fixed assets + receivables, debt not subtracted: %s", first.Assets)
	assert.True(t, dec("6500").Equal(dec(first.NetWorth)), first.NetWorth)
	assert.NotEmpty(t, h.Series.Investments.Direction, "a verdict for the investments line")
}

// K-T4: buying with money from a wallet is a contribution to the investments line, not a market gain, and is
// neutral for everything owned (cash becomes an investment).
func TestSeriesContributionsOfATrade(t *testing.T) {
	s := NewTestServer(t)
	s.Clock.Set(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	w := newWorld(t, s) // 1500 USD
	e := &invEnv{s: s, u: w.u, sid: w.sid}
	btc := e.must(map[string]any{"assetType": "CRYPTO", "name": "Bitcoin", "symbol": "BTC", "quantity": "0",
		"avgBuyPrice": "0", "manualPrice": "1200", "priceMode": "MANUAL"})
	s.seedFullSnapshot(w.sid, "2026-11-11", "1500", "0", "0", "0", "0")
	s.Clock.Set(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC))

	res := s.Do(http.MethodPost, trURL(w.sid), map[string]any{"from": walletEnd(w.a), "to": leg(btc.ID, "1", "1000", "10")}, bearer(w.u.AccessToken))
	require.Equal(t, http.StatusCreated, res.Status, string(res.Body))

	h := s.growth(w.u, w.sid, "?range=1M")
	inv := h.Series.Investments
	assert.True(t, dec("0").Equal(dec(inv.First)))
	assert.True(t, dec("1200").Equal(dec(inv.Last)), "1 x the manual price 1200")
	assert.True(t, dec("1010").Equal(dec(inv.NetContributions)), "the buy: 1000 + 10 fee: %s", inv.NetContributions)
	assert.True(t, dec("190").Equal(dec(inv.MarketChange)), inv.MarketChange)
	assert.Nil(t, inv.ChangePct, "the first value is 0")
	require.NotNil(t, inv.PerformancePct)
	assert.True(t, dec("18.81").Equal(dec(*inv.PerformancePct)), "190 on 1010: %s", *inv.PerformancePct)
	assert.Equal(t, "RISING", inv.Direction)

	as := h.Series.Assets
	assert.True(t, dec("1500").Equal(dec(as.First)))
	assert.True(t, dec("1690").Equal(dec(as.Last)), "490 cash + 1200 investment")
	assert.True(t, dec("0").Equal(dec(as.NetContributions)), "a purchase from a wallet is neutral for everything owned: %s", as.NetContributions)
	assert.True(t, dec("190").Equal(dec(as.MarketChange)))
	assert.Equal(t, "RISING", as.Direction)
	assert.True(t, dec("190").Equal(dec(h.Series.NetWorth.MarketChange)))

	t.Run("a sale takes money out of the investments line, a fee is a cost", func(t *testing.T) {
		res := s.Do(http.MethodPost, trURL(w.sid), map[string]any{"from": leg(btc.ID, "1", "1300", "20"), "to": walletEnd(w.a)}, bearer(w.u.AccessToken))
		require.Equal(t, http.StatusCreated, res.Status, string(res.Body))
		h := s.growth(w.u, w.sid, "?range=1M")
		inv := h.Series.Investments
		assert.True(t, dec("0").Equal(dec(inv.Last)), "nothing is held any more")
		assert.True(t, dec("-270").Equal(dec(inv.NetContributions)), "+1010 for the buy, -(1300 - 20) for the sale: %s", inv.NetContributions)
		assert.True(t, dec("270").Equal(dec(inv.MarketChange)), "value 0 - 0 - (-270)")
		assert.True(t, dec("0").Equal(dec(h.Series.Assets.NetContributions)))
		assert.True(t, dec("1500").Add(dec("270")).Equal(dec(h.Series.Assets.Last)), "cash 1500 - 1010 + 1280: %s", h.Series.Assets.Last)
	})
}

// A price move on a holding that was already there is market change, with no contribution.
func TestSeriesPriceMoveOfAnAutoCrypto(t *testing.T) {
	ctx := context.Background()
	s := NewTestServer(t)
	s.Clock.Set(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	w := newWorld(t, s)
	e := &invEnv{s: s, u: w.u, sid: w.sid}
	e.setSpace(map[string]any{"baseCurrency": "USD"})
	e.must(cryptoNoMode("BTC", "0.5")) // 0.5 x 63000 = 31500 (the holding is older than the baseline)
	s.seedFullSnapshot(w.sid, "2026-11-11", "1500", "31500", "0", "0", "0")
	s.Clock.Set(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC))

	s.CMC.Quotes[1] = "64000"
	_, err := s.Market.RefreshCryptoPrices(ctx)
	require.NoError(t, err)

	h := s.growth(w.u, w.sid, "?range=1M")
	inv := h.Series.Investments
	assert.True(t, dec("32000").Equal(dec(inv.Last)), "the CMC price, not the typed one: %s", inv.Last)
	assert.True(t, dec("500").Equal(dec(inv.Change)))
	assert.True(t, dec("0").Equal(dec(inv.NetContributions)))
	assert.True(t, dec("500").Equal(dec(inv.MarketChange)))
	require.NotNil(t, inv.ChangePct)
	assert.True(t, dec("1.59").Equal(dec(*inv.ChangePct)), *inv.ChangePct)
	require.NotNil(t, inv.PerformancePct)
	assert.True(t, dec("1.59").Equal(dec(*inv.PerformancePct)))
	assert.Equal(t, "RISING", inv.Direction)

	t.Run("a falling price is FALLING", func(t *testing.T) {
		s.CMC.Quotes[1] = "60000"
		_, err := s.Market.RefreshCryptoPrices(ctx)
		require.NoError(t, err)
		inv := s.growth(w.u, w.sid, "?range=1M").Series.Investments
		assert.Equal(t, "FALLING", inv.Direction)
		assert.True(t, dec("-1500").Equal(dec(inv.MarketChange)))
	})
}

// A holding added by hand (no funding wallet) costs money put in, not a market gain.
func TestSeriesUnfundedInvestmentAndFixedAssets(t *testing.T) {
	ctx := context.Background()
	s := NewTestServer(t)
	s.Clock.Set(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	w := newWorld(t, s)
	e := &invEnv{s: s, u: w.u, sid: w.sid}
	e.setSpace(map[string]any{"baseCurrency": "USD"})
	s.seedFullSnapshot(w.sid, "2026-11-11", "1500", "0", "8000", "0", "0")
	s.Clock.Set(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC))

	inv := e.must(map[string]any{"assetType": "CRYPTO", "name": "Bitcoin", "symbol": "BTC", "quantity": "0.5", "avgBuyPrice": "60000", "manualPrice": "60000", "priceMode": "MANUAL"})
	_, err := s.Tx.Exec(ctx, `UPDATE investments SET created_at = $2::timestamptz WHERE id = $1::uuid`, inv.ID, time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC))
	require.NoError(t, err)

	car := s.Do(http.MethodPost, assetURL(w.sid), map[string]any{"kind": "FIXED_ASSET", "name": "Bike", "currentValue": "500"}, bearer(w.u.AccessToken))
	require.Equal(t, http.StatusCreated, car.Status, string(car.Body))
	_, err = s.Tx.Exec(ctx, `UPDATE manual_asset_value_changes SET changed_at = $1::timestamptz WHERE asset_id IN (SELECT id FROM manual_assets WHERE name = 'Bike')`, time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	// the 8000 house of the baseline is sold
	_, err = s.Tx.Exec(ctx, `INSERT INTO manual_assets (space_id, kind, name, icon, currency, current_value, created_by_id, deleted_at)
		VALUES ($1::uuid, 'FIXED_ASSET', 'House', 'x', 'USD', 8000, $2::uuid, $3::timestamptz)`, w.sid, w.u.User.ID, time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC))
	require.NoError(t, err)

	h := s.growth(w.u, w.sid, "?range=1M")
	i := h.Series.Investments
	assert.True(t, dec("30000").Equal(dec(i.NetContributions)), "0.5 x the 60000 paid: %s", i.NetContributions)
	assert.True(t, dec("0").Equal(dec(i.MarketChange)), "worth 30000, cost 30000: %s", i.MarketChange)

	f := h.Series.FixedAssets
	assert.True(t, dec("8000").Equal(dec(f.First)))
	assert.True(t, dec("500").Equal(dec(f.Last)), "only the bike is left: %s", f.Last)
	assert.True(t, dec("-7500").Equal(dec(f.NetContributions)), "+500 bought, -8000 sold: %s", f.NetContributions)
	assert.True(t, dec("0").Equal(dec(f.MarketChange)), f.MarketChange)
	assert.Equal(t, "FALLING", f.Direction)
	assert.True(t, dec("-93.75").Equal(dec(*f.ChangePct)), *f.ChangePct)
}

// K-T5: the range names and what the history really covers.
func TestHistoryRangesAndCoverage(t *testing.T) {
	s := NewTestServer(t)
	s.Clock.Set(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC))
	w := newWorld(t, s)
	for _, d := range []string{"2026-01-05", "2026-11-01", "2026-11-12", "2026-11-17"} {
		s.seedFullSnapshot(w.sid, d, "1000", "0", "0", "0", "0")
	}
	dates := func(h growthBody) []string {
		var out []string
		for _, p := range h.Points {
			out = append(out, p.Date)
		}
		return out
	}

	h := s.growth(w.u, w.sid, "?range=1W")
	assert.Equal(t, "1W", h.Range)
	assert.Equal(t, []string{"2026-11-12", "2026-11-17", "2026-11-18"}, dates(h))
	require.NotNil(t, h.Coverage.RequestedFrom)
	assert.Equal(t, "2026-11-11", *h.Coverage.RequestedFrom)
	assert.Equal(t, "2026-11-12", h.Coverage.FirstPointDate)
	assert.True(t, h.Coverage.Short, "the first snapshot is later than the start of the week")

	h = s.growth(w.u, w.sid, "?range=YTD")
	assert.Equal(t, "YTD", h.Range)
	assert.Equal(t, []string{"2026-01-05", "2026-11-01", "2026-11-12", "2026-11-17", "2026-11-18"}, dates(h))
	assert.Equal(t, "2026-01-01", *h.Coverage.RequestedFrom)
	assert.True(t, h.Coverage.Short, "5 Jan is after 1 Jan")

	s.seedFullSnapshot(w.sid, "2026-11-11", "1000", "0", "0", "0", "0")
	assert.False(t, s.growth(w.u, w.sid, "?range=1W").Coverage.Short, "a snapshot on the first day of the range covers it")

	h = s.growth(w.u, w.sid, "?range=ALL")
	assert.Nil(t, h.Coverage.RequestedFrom)
	assert.False(t, h.Coverage.Short)
	assert.Equal(t, "2026-01-05", h.Coverage.FirstPointDate)

	t.Run("a new space has one live point: nothing to compare, honest coverage", func(t *testing.T) {
		fresh := s.register("fresh@example.com")
		h := s.growth(fresh, spaceIDOf(fresh), "?range=3M")
		require.Len(t, h.Points, 1)
		assert.Equal(t, "FLAT", h.Series.Investments.Direction)
		assert.Nil(t, h.Series.NetWorth.ChangePct)
		assert.Nil(t, h.Series.NetWorth.PerformancePct)
		assert.True(t, h.Coverage.Short)
		assert.Equal(t, "2026-11-18", h.Coverage.FirstPointDate)
	})

	t.Run("an unknown range lists the new names", func(t *testing.T) {
		res := s.Get("/api/v1/spaces/"+w.sid+"/net-worth/history?range=2W", bearer(w.u.AccessToken))
		require.Equal(t, http.StatusBadRequest, res.Status)
		assert.Contains(t, string(res.Body), "1W")
		assert.Contains(t, string(res.Body), "YTD")
	})
}

// K-T6: the last point of the history is the Home number: it equals the net-worth summary and the assets summary
// to the cent, component by component, and the snapshot job stores the same market values.
func TestHistoryLastPointEqualsHomeAndAssets(t *testing.T) {
	ctx := context.Background()
	s := NewTestServer(t)
	e, inv := buildFixture(t, s)
	_ = inv
	nw := s.networth(e.u, e.sid)
	a := s.assetSummary(e.u, e.sid)
	h := s.growth(e.u, e.sid, "")
	require.NotEmpty(t, h.Points)
	last := h.Points[len(h.Points)-1]
	require.True(t, last.Live)
	assert.Equal(t, nw.NetWorth, last.NetWorth)
	assert.Equal(t, nw.Cash, last.Cash)
	assert.Equal(t, nw.Investments.Value, last.Investments)
	assert.Equal(t, *nw.FixedAssets, last.FixedAssets)
	assert.Equal(t, *nw.Receivables, last.Receivables)
	assert.Equal(t, *nw.Liabilities, last.Liabilities)
	assert.Equal(t, a.Investments.Total, last.Investments)
	assert.True(t, dec(last.Cash).Add(dec(last.Investments)).Add(dec(last.FixedAssets)).Add(dec(last.Receivables)).Equal(dec(last.Assets)), "assets: %s", last.Assets)
	assert.Equal(t, last.Investments, h.Series.Investments.Last)
	assert.Equal(t, last.NetWorth, h.Series.NetWorth.Last)
	assert.Equal(t, last.Assets, h.Series.Assets.Last)

	_, err := s.Networth.SnapshotAll(ctx, zerologNop())
	require.NoError(t, err)
	var snapInv, snapNW string
	require.NoError(t, s.Tx.QueryRow(ctx, `SELECT investments::text, net_worth::text FROM net_worth_snapshots WHERE space_id = $1`, e.sid).Scan(&snapInv, &snapNW))
	assert.True(t, dec(nw.Investments.Value).Equal(dec(snapInv).Round(2)), "the snapshot uses the same valuation: %s vs %s", snapInv, nw.Investments.Value)
	assert.True(t, dec(nw.NetWorth).Equal(dec(snapNW).Round(2)))
}
