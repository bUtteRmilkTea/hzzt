//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type valuationRowBody struct {
	ID        string `json:"id"`
	AsOf      string `json:"asOf"`
	NAV       string `json:"nav"`
	Quantity  string `json:"quantity"`
	CostBasis string `json:"costBasis"`
	Value     string `json:"value"`
	Note      *string
	PnL       string  `json:"pnl"`
	PnLPct    *string `json:"pnlPct"`
	Change    *struct {
		Value string  `json:"value"`
		Pct   *string `json:"pct"`
	} `json:"changeFromPrevious"`
	IsLatest bool `json:"isLatest"`
	Base     *struct {
		Currency string `json:"currency"`
		Value    string `json:"value"`
		PnL      string `json:"pnl"`
	} `json:"base"`
}

type valuationListBody struct {
	Currency        string             `json:"currency"`
	Items           []valuationRowBody `json:"items"`
	LastValuationAt *string            `json:"lastValuationAt"`
}

func valURL(sid, inv string, rest ...string) string {
	return invURL(sid, "/", inv, "/valuations") + strings.Join(rest, "")
}

type valEnv struct {
	*tradeEnv
	fund investmentBody
}

func newValEnv(t *testing.T, s *TestServer) *valEnv {
	t.Helper()
	e := newTradeEnv(t, s)
	e.inv.setSpace(map[string]any{"baseCurrency": "USD"})
	fund := e.inv.must(map[string]any{"assetType": "MUTUAL_FUND", "name": "Growth fund", "quantity": "10", "avgBuyPrice": "100", "manualPrice": "100",
		"quoteCurrency": "USD", "details": map[string]any{"fundType": "EQUITY"}})
	return &valEnv{tradeEnv: e, fund: fund}
}

func (e *valEnv) post(asOf, nav string, extra ...any) Response {
	body := clone(map[string]any{"asOf": asOf, "nav": nav}, extra...)
	return e.s.Do(http.MethodPost, valURL(e.sid, e.fund.ID), body, bearer(e.u.AccessToken))
}

func (e *valEnv) mustPost(asOf, nav string, status int) valuationRowBody {
	e.s.t.Helper()
	res := e.post(asOf, nav)
	require.Equal(e.s.t, status, res.Status, string(res.Body))
	var v valuationRowBody
	res.JSON(e.s.t, &v)
	return v
}

func (e *valEnv) list() valuationListBody {
	e.s.t.Helper()
	res := e.s.Get(valURL(e.sid, e.fund.ID), bearer(e.u.AccessToken))
	require.Equal(e.s.t, http.StatusOK, res.Status, string(res.Body))
	var l valuationListBody
	res.JSON(e.s.t, &l)
	return l
}

func (e *valEnv) del(id string) Response {
	return e.s.Do(http.MethodDelete, valURL(e.sid, e.fund.ID, "/", id), nil, bearer(e.u.AccessToken))
}

// every total that includes the fund must show quantity x nav
func (e *valEnv) assertTotals(qty, nav string) {
	e.s.t.Helper()
	want := dec(qty).Mul(dec(nav))
	got := e.inv.get(e.fund.ID)
	assert.True(e.s.t, dec(nav).Equal(dec(got.ManualPrice)), "manualPrice %s", got.ManualPrice)
	assert.True(e.s.t, want.Equal(dec(got.Valuation.MarketValue)), "fund value %s want %s", got.Valuation.MarketValue, want)
	// the space holds only this fund as an investment
	l := e.inv.list("")
	assert.True(e.s.t, want.Equal(dec(l.Summary.TotalValue)), "list summary %s", l.Summary.TotalValue)
	g := e.s.grouped(e.u, e.sid, "")
	assert.True(e.s.t, want.Equal(dec(g.Total.Value)), "grouped %s", g.Total.Value)
	a := e.s.assetSummary(e.u, e.sid)
	require.NotNil(e.s.t, a.Investments)
	assert.True(e.s.t, want.Equal(dec(a.Investments.Total)), "assets summary %s", a.Investments.Total)
	nw := e.s.networth(e.u, e.sid)
	require.NotNil(e.s.t, nw.Investments)
	assert.True(e.s.t, want.Equal(dec(nw.Investments.Value)), "net worth %s", nw.Investments.Value)
}

func TestValuationsLivePriceAndHistory(t *testing.T) {
	s := NewTestServer(t)
	e := newValEnv(t, s)

	t.Run("a valuation becomes the live price and NAV date, and totals follow", func(t *testing.T) {
		v := e.mustPost("2026-09-30", "110", http.StatusCreated)
		assert.Equal(t, "2026-09-30", v.AsOf)
		assert.True(t, v.IsLatest)
		assert.True(t, dec("1100").Equal(dec(v.Value)))
		assert.True(t, dec("1000").Equal(dec(v.CostBasis)))
		assert.True(t, dec("100").Equal(dec(v.PnL)))
		require.NotNil(t, v.PnLPct)
		assert.Equal(t, "10.00", *v.PnLPct)
		assert.Nil(t, v.Change, "nothing before it")
		got := e.inv.get(e.fund.ID)
		assert.Equal(t, "2026-09-30", got.Details["navDate"])
		require.NotNil(t, got.Valuation.NavAgeDays)
		assert.Equal(t, "MANUAL", got.Valuation.PriceSource)
		e.assertTotals("10", "110")
	})

	t.Run("an older date only adds history", func(t *testing.T) {
		e.mustPost("2026-08-31", "105", http.StatusCreated)
		e.assertTotals("10", "110")
		got := e.inv.get(e.fund.ID)
		assert.Equal(t, "2026-09-30", got.Details["navDate"])
		l := e.list()
		require.Len(t, l.Items, 2)
		assert.Equal(t, "2026-09-30", l.Items[0].AsOf, "newest first")
		assert.True(t, l.Items[0].IsLatest)
		assert.False(t, l.Items[1].IsLatest)
		require.NotNil(t, l.LastValuationAt)
		assert.Equal(t, "2026-09-30", *l.LastValuationAt)
	})

	t.Run("the same date replaces it", func(t *testing.T) {
		v := e.mustPost("2026-09-30", "1,120.50", http.StatusOK)
		assert.True(t, dec("1120.5").Equal(dec(v.NAV)), "grouping commas are accepted")
		assert.Len(t, e.list().Items, 2)
		e.assertTotals("10", "1120.5")
		e.mustPost("2026-09-30", "110", http.StatusOK)
		e.assertTotals("10", "110")
	})

	t.Run("a BUY between two months is not counted as gain", func(t *testing.T) {
		require.Equal(t, http.StatusCreated, e.buy(e.a, e.fund.ID, "10", "120", "").Status)
		e.inv.must(map[string]any{"assetType": "CRYPTO", "name": "x", "symbol": "XXX", "quantity": "0", "manualPrice": "1", "priceMode": "MANUAL"}) // another holding is irrelevant to the fund list
		v := e.mustPost("2026-10-31", "130", http.StatusCreated)
		assert.True(t, dec("20").Equal(dec(v.Quantity)))
		assert.True(t, dec("2200").Equal(dec(v.CostBasis)), "20 x 110 average")
		assert.True(t, dec("2600").Equal(dec(v.Value)))
		assert.True(t, dec("400").Equal(dec(v.PnL)))
		require.NotNil(t, v.Change)
		assert.True(t, dec("400").Equal(dec(v.Change.Value)), "(130-110) x 20, not the 1,200 that was bought: %s", v.Change.Value)
		require.NotNil(t, v.Change.Pct)
		assert.Equal(t, "18.18", *v.Change.Pct)

		l := e.list()
		require.Len(t, l.Items, 3)
		assert.True(t, dec("10").Equal(dec(l.Items[1].Quantity)), "last month keeps its own position")
		assert.True(t, dec("1000").Equal(dec(l.Items[1].CostBasis)))
		assert.True(t, dec("100").Equal(dec(l.Items[1].PnL)), "last month's PnL is not rewritten")
		require.NotNil(t, l.Items[1].Change)
		assert.True(t, dec("50").Equal(dec(l.Items[1].Change.Value)), "(110-105) x 10")
		assert.Nil(t, l.Items[2].Change, "the oldest has none")
		require.NotNil(t, l.Items[0].Base)
		assert.Equal(t, "USD", l.Items[0].Base.Currency)
		assert.True(t, dec("2600").Equal(dec(l.Items[0].Base.Value)))
	})
}

func TestValuationsDelete(t *testing.T) {
	s := NewTestServer(t)
	e := newValEnv(t, s)
	first := e.mustPost("2026-08-31", "105", http.StatusCreated)
	second := e.mustPost("2026-09-30", "110", http.StatusCreated)
	e.assertTotals("10", "110")

	t.Run("deleting an older one only removes history", func(t *testing.T) {
		third := e.mustPost("2026-07-31", "100", http.StatusCreated)
		res := e.del(third.ID)
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		assert.Len(t, e.list().Items, 2)
		e.assertTotals("10", "110")
	})

	t.Run("deleting the newest restores the previous NAV and date", func(t *testing.T) {
		res := e.del(second.ID)
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		assert.NotContains(t, string(res.Body), "NAV_NOT_REVERTED")
		e.assertTotals("10", "105")
		assert.Equal(t, "2026-08-31", e.inv.get(e.fund.ID).Details["navDate"])
		l := e.list()
		require.Len(t, l.Items, 1)
		assert.True(t, l.Items[0].IsLatest)
	})

	t.Run("deleting the only one keeps the NAV and says so", func(t *testing.T) {
		res := e.del(first.ID)
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		var out struct {
			Warnings []struct{ Code string } `json:"warnings"`
		}
		res.JSON(t, &out)
		require.Len(t, out.Warnings, 1)
		assert.Equal(t, "NAV_NOT_REVERTED", out.Warnings[0].Code)
		e.assertTotals("10", "105")
		assert.Empty(t, e.list().Items)
		assert.Nil(t, e.list().LastValuationAt)
	})

	t.Run("not found", func(t *testing.T) {
		assert.Equal(t, http.StatusNotFound, e.del(first.ID).Status, "already gone")
		assert.Equal(t, http.StatusNotFound, e.del("nope").Status)
	})
}

func TestValuationsRulesAndValidation(t *testing.T) {
	s := NewTestServer(t)
	e := newValEnv(t, s)

	t.Run("only mutual funds", func(t *testing.T) {
		res := s.Do(http.MethodPost, valURL(e.sid, e.btc.ID), map[string]any{"asOf": "2026-09-30", "nav": "1"}, bearer(e.u.AccessToken))
		require.Equal(t, http.StatusUnprocessableEntity, res.Status, string(res.Body))
		assert.Equal(t, "VALUATIONS_NOT_SUPPORTED", errCode(t, res))
		assert.Equal(t, http.StatusUnprocessableEntity, s.Get(valURL(e.sid, e.btc.ID), bearer(e.u.AccessToken)).Status)
	})

	t.Run("validation", func(t *testing.T) {
		for name, body := range map[string]map[string]any{
			"no date":       {"nav": "1"},
			"bad date":      {"asOf": "31/09/2026", "nav": "1"},
			"future date":   {"asOf": "2099-01-01", "nav": "1"},
			"no nav":        {"asOf": "2026-09-30"},
			"negative nav":  {"asOf": "2026-09-30", "nav": "-1"},
			"text nav":      {"asOf": "2026-09-30", "nav": "abc"},
			"9 decimals":    {"asOf": "2026-09-30", "nav": "1.123456789"},
			"bad grouping":  {"asOf": "2026-09-30", "nav": "1,53"},
			"huge nav":      {"asOf": "2026-09-30", "nav": "100000000000000000000"},
			"long note":     {"asOf": "2026-09-30", "nav": "1", "note": strings.Repeat("n", 501)},
			"unknown field": {"asOf": "2026-09-30", "nav": "1", "value": "5"},
		} {
			res := s.Do(http.MethodPost, valURL(e.sid, e.fund.ID), body, bearer(e.u.AccessToken))
			assert.Equal(t, http.StatusBadRequest, res.Status, "%s: %s", name, res.Body)
		}
		assert.Empty(t, e.list().Items, "nothing was stored")
		e.assertTotals("10", "100")
		assert.True(t, dec("0").Equal(dec(e.mustPost("2026-09-30", "0", http.StatusCreated).NAV)), "a zero NAV is allowed")
	})

	t.Run("a manual current value is never touched", func(t *testing.T) {
		require.Equal(t, http.StatusOK, e.inv.patch(e.fund.ID, map[string]any{"manualCurrentValue": "5000"}).Status)
		e.mustPost("2026-10-31", "120", http.StatusCreated)
		got := e.inv.get(e.fund.ID)
		require.NotNil(t, got.ManualCurrentValue)
		assert.True(t, dec("5000").Equal(dec(*got.ManualCurrentValue)))
		assert.True(t, dec("120").Equal(dec(got.ManualPrice)))
		assert.Equal(t, "MANUAL_VALUE", got.Valuation.PriceSource, "the manual value still wins")
	})

	t.Run("auth and spaces", func(t *testing.T) {
		assert.Equal(t, http.StatusUnauthorized, s.Do(http.MethodPost, valURL(e.sid, e.fund.ID), map[string]any{"asOf": "2026-09-30", "nav": "1"}).Status)
		outsider := s.register("mallory@example.com")
		expectCrossSpace404(t, s, outsider, http.MethodGet, valURL(e.sid, e.fund.ID), nil)
		expectCrossSpace404(t, s, outsider, http.MethodPost, valURL(e.sid, e.fund.ID), map[string]any{"asOf": "2026-09-30", "nav": "1"})
	})
}

func TestValuationsNeedTheOwner(t *testing.T) {
	s := NewTestServer(t)
	e := newShareEnv(t, s)
	ie := e.invEnv()
	fund := ie.must(map[string]any{"assetType": "MUTUAL_FUND", "name": "Shared fund", "quantity": "10", "avgBuyPrice": "100", "manualPrice": "100",
		"quoteCurrency": "USD", "details": map[string]any{"fundType": "EQUITY"}})
	bob := s.register("bob@example.com")
	res := s.Do(http.MethodPut, invURL(e.sid, "/", fund.ID, "/participants"), map[string]any{"participants": []map[string]any{owner("50"), emailP("bob@example.com", "Bob", "50")}}, bearer(e.owner.AccessToken))
	require.Equal(t, http.StatusOK, res.Status, string(res.Body))
	var ps participantsResp
	res.JSON(t, &ps)
	require.Equal(t, http.StatusOK, s.acceptShare(bob, byName(ps.Items, "Bob").ID, spaceIDOf(bob)).Status)

	post := s.Do(http.MethodPost, valURL(spaceIDOf(bob), fund.ID), map[string]any{"asOf": "2026-09-30", "nav": "1"}, bearer(bob.AccessToken))
	require.Equal(t, http.StatusForbidden, post.Status, string(post.Body))
	assert.Equal(t, "OWNER_ONLY", errCode(t, post))
	get := s.Get(valURL(spaceIDOf(bob), fund.ID), bearer(bob.AccessToken))
	require.Equal(t, http.StatusForbidden, get.Status, string(get.Body))
	assert.Equal(t, "OWNER_ONLY", errCode(t, get))
	assert.Equal(t, http.StatusForbidden, s.Do(http.MethodDelete, valURL(spaceIDOf(bob), fund.ID, "/", "00000000-0000-0000-0000-000000000001"), nil, bearer(bob.AccessToken)).Status)

	// the owner can
	ok := s.Do(http.MethodPost, valURL(e.sid, fund.ID), map[string]any{"asOf": "2026-09-30", "nav": "110"}, bearer(e.owner.AccessToken))
	require.Equal(t, http.StatusCreated, ok.Status, string(ok.Body))
	_ = url.QueryEscape
}

func TestValuationBackfillMigration(t *testing.T) {
	ctx := context.Background()
	s := NewTestServer(t)
	e := newValEnv(t, s)
	dated := e.fund
	require.Equal(t, http.StatusOK, e.inv.patch(dated.ID, map[string]any{"manualPrice": "123.45", "details": map[string]any{"fundType": "EQUITY", "navDate": "2026-10-15"}}).Status)
	undated := e.inv.must(map[string]any{"assetType": "MUTUAL_FUND", "name": "No date", "quantity": "4", "avgBuyPrice": "10", "manualPrice": "12", "quoteCurrency": "USD", "details": map[string]any{"fundType": "BALANCED"}})
	notAFund := e.inv.must(map[string]any{"assetType": "CRYPTO", "name": "Coin", "symbol": "COI", "quantity": "1", "manualPrice": "1", "priceMode": "MANUAL"})
	_ = notAFund

	var before int
	require.NoError(t, s.Tx.QueryRow(ctx, `SELECT count(*) FROM investment_valuations WHERE investment_id IN ($1, $2)`, dated.ID, undated.ID).Scan(&before))
	require.Zero(t, before)
	s.runMigrationUp("00026_valuations_backfill.sql")

	rows, err := s.Tx.Query(ctx, `SELECT investment_id::text, as_of::text, nav::text, quantity::text, value::text FROM investment_valuations WHERE investment_id IN ($1, $2)`, dated.ID, undated.ID)
	require.NoError(t, err)
	defer rows.Close()
	got := map[string][4]string{}
	for rows.Next() {
		var id string
		var r [4]string
		require.NoError(t, rows.Scan(&id, &r[0], &r[1], &r[2], &r[3]))
		got[id] = r
	}
	require.Len(t, got, 2, "one row per fund")
	assert.Equal(t, "2026-10-15", got[dated.ID][0], "dated by navDate")
	assert.True(t, dec("123.45").Equal(dec(got[dated.ID][1])))
	assert.True(t, dec("1234.5").Equal(dec(got[dated.ID][3])), "10 x 123.45")
	assert.True(t, dec("48").Equal(dec(got[undated.ID][3])), "4 x 12")
	assert.NotEmpty(t, got[undated.ID][0], "dated by the last update when there is no NAV date")
	var n int
	require.NoError(t, s.Tx.QueryRow(ctx, `SELECT count(*) FROM investment_valuations WHERE investment_id = $1`, notAFund.ID).Scan(&n))
	assert.Zero(t, n, "only mutual funds")
	s.runMigrationUp("00026_valuations_backfill.sql") // a second run adds nothing
	require.NoError(t, s.Tx.QueryRow(ctx, `SELECT count(*) FROM investment_valuations WHERE investment_id IN ($1, $2)`, dated.ID, undated.ID).Scan(&n))
	assert.Equal(t, 2, n)
}
