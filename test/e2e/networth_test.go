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

type networthBody struct {
	Currency     string `json:"currency"`
	TrackingMode string `json:"trackingMode"`
	AsOf         string `json:"asOf"`
	Cash         string `json:"cash"`
	Investments  *struct {
		Value           string  `json:"value"`
		Cost            string  `json:"cost"`
		UnrealizedPL    string  `json:"unrealizedPl"`
		UnrealizedPLPct *string `json:"unrealizedPlPct"`
	} `json:"investments"`
	FixedAssets *string                                 `json:"fixedAssets"`
	Receivables *string                                 `json:"receivables"`
	Liabilities *string                                 `json:"liabilities"`
	NetWorth    string                                  `json:"netWorth"`
	Warnings    []struct{ Code, Message, Label string } `json:"warnings"`
}

func nwURL(sid string) string { return "/api/v1/spaces/" + sid + "/net-worth/summary" }

func (s *TestServer) networth(u session, sid string) networthBody {
	s.t.Helper()
	res := s.Get(nwURL(sid), bearer(u.AccessToken))
	require.Equal(s.t, http.StatusOK, res.Status, string(res.Body))
	var b networthBody
	res.JSON(s.t, &b)
	return b
}

// buildFixture creates a USD space (1 USD = 16000 IDR = 0.8 EUR = 1/0.75 GBP) holding, in USD:
// cash 2000 (1000 USD + 16,000,000 IDR), investments worth 1150 at a cost of 1043.75,
// a car of 10,000, 125 owed to me and 500 of debt.
func buildFixture(t *testing.T, s *TestServer) (*assetEnv, *invEnv) {
	t.Helper()
	e := newAssetEnv(t, s)
	// Wallet A = 1000 USD, Wallet B = 500 USD, IDR wallet = 10,000,000 IDR: reshape to 1000 + 0 + 16,000,000
	require.Equal(t, http.StatusOK, s.Do(http.MethodPut, walURL(e.sid, "/", e.a.ID, "/balance"), map[string]any{"newBalance": "1000"}, bearer(e.u.AccessToken)).Status)
	require.Equal(t, http.StatusOK, s.Do(http.MethodPut, walURL(e.sid, "/", e.b.ID, "/balance"), map[string]any{"newBalance": "0"}, bearer(e.u.AccessToken)).Status)
	require.Equal(t, http.StatusOK, s.Do(http.MethodPut, walURL(e.sid, "/", e.idr.ID, "/balance"), map[string]any{"newBalance": "16000000"}, bearer(e.u.AccessToken)).Status)

	ie := &invEnv{s: s, u: e.u, sid: e.sid}
	ie.must(map[string]any{"assetType": "US_STOCK", "name": "Apple", "symbol": "AAPL", "quantity": "10", "avgBuyPrice": "100", "manualPrice": "110", "details": map[string]any{"exchange": "NASDAQ"}})
	ie.must(map[string]any{"assetType": "ID_STOCK", "name": "BBCA", "symbol": "BBCA", "avgBuyPrice": "7000", "manualPrice": "8000", "details": map[string]any{"lots": 1}})

	e.must(map[string]any{"kind": "FIXED_ASSET", "name": "Car", "currentValue": "160000000", "currency": "IDR"})
	e.must(map[string]any{"kind": "RECEIVABLE", "name": "Loan to Sam", "currentValue": "100", "currency": "EUR"})
	e.must(debt("Credit card", "500"))
	return e, ie
}

func TestNetWorthSummaryAcrossCurrencies(t *testing.T) {
	s := NewTestServer(t)
	e, _ := buildFixture(t, s)

	nw := s.networth(e.u, e.sid)
	assert.Equal(t, "USD", nw.Currency)
	assert.Equal(t, "FULL_NET_WORTH", nw.TrackingMode)
	assert.NotEmpty(t, nw.AsOf)

	assert.True(t, dec("2000").Equal(dec(nw.Cash)), "1000 USD + 16,000,000 IDR (= 1000 USD): %s", nw.Cash)
	require.NotNil(t, nw.Investments)
	assert.True(t, dec("1150").Equal(dec(nw.Investments.Value)), "AAPL 1100 + BBCA 100 × 8000 IDR (= 50): %s", nw.Investments.Value)
	assert.True(t, dec("1043.75").Equal(dec(nw.Investments.Cost)), "1000 + 700,000 IDR (= 43.75)")
	assert.True(t, dec("106.25").Equal(dec(nw.Investments.UnrealizedPL)))
	require.NotNil(t, nw.Investments.UnrealizedPLPct)
	assert.Equal(t, "10.18", *nw.Investments.UnrealizedPLPct)
	require.NotNil(t, nw.FixedAssets)
	assert.True(t, dec("10000").Equal(dec(*nw.FixedAssets)), "160,000,000 IDR")
	assert.True(t, dec("125").Equal(dec(*nw.Receivables)), "100 EUR at 0.8 per USD")
	assert.True(t, dec("500").Equal(dec(*nw.Liabilities)))
	// 2000 + 1150 + 10000 + 125 − 500
	assert.True(t, dec("12775").Equal(dec(nw.NetWorth)), "netWorth = %s", nw.NetWorth)
	assert.Empty(t, nw.Warnings)

	t.Run("a deleted wallet, investment or asset leaves the totals", func(t *testing.T) {
		require.Equal(t, http.StatusNoContent, s.Do(http.MethodDelete, walURL(e.sid, "/", e.idr.ID)+"?force=true", nil, bearer(e.u.AccessToken)).Status)
		assert.True(t, dec("1000").Equal(dec(s.networth(e.u, e.sid).Cash)))
	})

	t.Run("a currency without a rate is excluded with a warning, never an error", func(t *testing.T) {
		e.must(map[string]any{"kind": "FIXED_ASSET", "name": "Singapore flat", "currentValue": "999999", "currency": "SGD"})
		nw := s.networth(e.u, e.sid)
		assert.True(t, dec("10000").Equal(dec(*nw.FixedAssets)), "the SGD asset is left out")
		require.Len(t, nw.Warnings, 1)
		assert.Equal(t, "FX_RATE_UNAVAILABLE", nw.Warnings[0].Code)
		assert.Contains(t, nw.Warnings[0].Message, "SGD")
	})
}

func TestNetWorthFollowsWalletsAndSettlements(t *testing.T) {
	s := NewTestServer(t)
	e, _ := buildFixture(t, s)
	before := dec(s.networth(e.u, e.sid).NetWorth)

	// paying 200 of the 500 debt from Wallet A: cash −200, liabilities −200 → net worth unchanged
	var card assetBody
	var list assetList
	s.Get(assetURL(e.sid)+"?kind=LIABILITY", bearer(e.u.AccessToken)).JSON(t, &list)
	card = list.Items[0]
	require.Equal(t, http.StatusCreated, e.settle(card.ID, map[string]any{"walletId": e.a.ID, "amount": "200"}).Status)
	nw := s.networth(e.u, e.sid)
	assert.True(t, dec("1800").Equal(dec(nw.Cash)), "cash %s", nw.Cash)
	assert.True(t, dec("300").Equal(dec(*nw.Liabilities)))
	assert.True(t, before.Equal(dec(nw.NetWorth)), "repaying a debt does not change net worth: %s vs %s", before, nw.NetWorth)

	// an expense lowers net worth
	e.world.must(exp(e.a, "100", ""))
	assert.True(t, before.Sub(dec("100")).Equal(dec(s.networth(e.u, e.sid).NetWorth)))
}

func TestNetWorthManualFXMode(t *testing.T) {
	s := NewTestServer(t)
	e, _ := buildFixture(t, s)
	require.Equal(t, http.StatusOK, s.Do(http.MethodPatch, "/api/v1/spaces/"+e.sid, map[string]any{"autoFxConversion": false}, bearer(e.u.AccessToken)).Status)
	require.Equal(t, http.StatusOK, s.Do(http.MethodPut, "/api/v1/spaces/"+e.sid+"/fx/manual-rates/USD/IDR", map[string]any{"rate": "20000"}, bearer(e.u.AccessToken)).Status)

	nw := s.networth(e.u, e.sid)
	assert.True(t, dec("1800").Equal(dec(nw.Cash)), "1000 + 16,000,000 / 20,000: %s", nw.Cash)
	assert.True(t, dec("8000").Equal(dec(*nw.FixedAssets)), "160,000,000 / 20,000")
	require.Len(t, nw.Warnings, 1, "the EUR receivable has no manual rate")
	assert.True(t, dec("0").Equal(dec(*nw.Receivables)))
}

func TestNetWorthCashOnlyHidesTheRest(t *testing.T) {
	s := NewTestServer(t)
	e, _ := buildFixture(t, s)
	require.Equal(t, http.StatusOK, s.Do(http.MethodPatch, "/api/v1/spaces/"+e.sid, map[string]any{"trackingMode": "CASH_ONLY"}, bearer(e.u.AccessToken)).Status)

	res := s.Get(nwURL(e.sid), bearer(e.u.AccessToken))
	require.Equal(t, http.StatusOK, res.Status)
	var raw map[string]any
	res.JSON(t, &raw)
	assert.Equal(t, "CASH_ONLY", raw["trackingMode"])
	assert.Contains(t, raw, "cash")
	assert.Contains(t, raw, "netWorth")
	for _, hidden := range []string{"investments", "fixedAssets", "receivables", "liabilities"} {
		assert.NotContains(t, raw, hidden, "CASH_ONLY returns only cash and netWorth")
	}
	var nw networthBody
	res.JSON(t, &nw)
	assert.True(t, dec("2000").Equal(dec(nw.Cash)))
	assert.True(t, dec(nw.Cash).Equal(dec(nw.NetWorth)), "net worth equals cash")
}

func TestNetWorthCountsSharedInvestmentsByShare(t *testing.T) {
	s := NewTestServer(t)
	owner := s.register("jane@example.com")
	sid := spaceIDOf(owner)
	ie := &invEnv{s: s, u: owner, sid: sid}
	inv := ie.must(map[string]any{"assetType": "US_STOCK", "name": "Apple", "symbol": "AAPL", "quantity": "1", "avgBuyPrice": "500",
		"manualPrice": "1000.01", "details": map[string]any{"exchange": "NASDAQ"}})
	bob := s.register("bob@example.com")
	env := &shareEnv{s: s, owner: owner, inv: inv, sid: sid}
	out := env.mustPut(owner2("50"), emailP("bob@example.com", "Bob", "30"), nameP("Mum", "20"))

	// invited but not accepted: Bob's space counts nothing, the owner counts 50%
	assert.True(t, dec("500.01").Equal(dec(s.networth(owner, sid).Investments.Value)), "owner space: 50%")
	assert.True(t, dec("0").Equal(dec(s.networth(bob, spaceIDOf(bob)).Investments.Value)))

	require.Equal(t, http.StatusOK, s.acceptShare(bob, byName(out.Items, "Bob").ID, spaceIDOf(bob)).Status)
	nw := s.networth(bob, spaceIDOf(bob))
	assert.True(t, dec("300").Equal(dec(nw.Investments.Value)), "the attached space counts 30% after acceptance")
	assert.True(t, dec("300").Equal(dec(nw.NetWorth)))
	assert.True(t, dec("500.01").Equal(dec(s.networth(owner, sid).Investments.Value)))
}

func owner2(pct string) map[string]any {
	return map[string]any{"isOwner": true, "displayName": "Jane", "sharePct": pct}
}

func TestNetWorthComputeAtADate(t *testing.T) {
	ctx := context.Background()
	s := NewTestServer(t)
	e, _ := buildFixture(t, s)
	spaceID := mustUUID(t, e.sid)

	now, err := s.Networth.Compute(ctx, spaceID, nil)
	require.NoError(t, err)
	assert.True(t, dec("12775").Equal(now.NetWorth))
	assert.NotNil(t, now.Investments)

	// the same wallets and assets valued with the rates that were in force on an earlier day
	s.Clock.Advance(24 * time.Hour)
	s.FX.Set = defaultRates(s.Clock)
	s.FX.Set.Rates["IDR"] = dec("32000") // the rupiah halved today
	require.NoError(t, s.Currency.Refresh(ctx))
	today, err := s.Networth.Compute(ctx, spaceID, nil)
	require.NoError(t, err)
	assert.True(t, today.NetWorth.LessThan(now.NetWorth), "the rupiah holdings are worth less today")

	yesterday := s.Clock.Now().Add(-24 * time.Hour)
	back, err := s.Networth.Compute(ctx, spaceID, &yesterday)
	require.NoError(t, err)
	assert.True(t, dec("12775").Equal(back.NetWorth), "with `at`, yesterday's rate is used: %s", back.NetWorth)
	assert.True(t, back.AsOf.Equal(yesterday.UTC()))
}
