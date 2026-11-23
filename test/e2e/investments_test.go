//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type valuationBody struct {
	Price           string  `json:"price"`
	PriceSource     string  `json:"priceSource"`
	PriceAsOf       *string `json:"priceAsOf"`
	IsStale         bool    `json:"isStale"`
	MarketValue     string  `json:"marketValue"`
	CostBasis       string  `json:"costBasis"`
	UnrealizedPL    string  `json:"unrealizedPl"`
	UnrealizedPLPct *string `json:"unrealizedPlPct"`
	Base            *struct {
		Currency     string `json:"currency"`
		MarketValue  string `json:"marketValue"`
		CostBasis    string `json:"costBasis"`
		UnrealizedPL string `json:"unrealizedPl"`
		FxRate       string `json:"fxRate"`
	} `json:"base"`
	Warnings []struct{ Code, Message, Label string } `json:"warnings"`
}

type investmentBody struct {
	ID                       string         `json:"id"`
	AssetType                string         `json:"assetType"`
	PriceMode                string         `json:"priceMode"`
	Name                     string         `json:"name"`
	Symbol                   *string        `json:"symbol"`
	Icon                     string         `json:"icon"`
	QuoteCurrency            string         `json:"quoteCurrency"`
	Quantity                 string         `json:"quantity"`
	AvgBuyPrice              string         `json:"avgBuyPrice"`
	ManualPrice              string         `json:"manualPrice"`
	Fees                     string         `json:"fees"`
	ManualCurrentValue       *string        `json:"manualCurrentValue"`
	PurchaseDate             *string        `json:"purchaseDate"`
	EstimatedAnnualChangePct *string        `json:"estimatedAnnualChangePct"`
	ManualFxRate             *string        `json:"manualFxRate"`
	FxRate                   *string        `json:"fxRate"`
	FxSource                 *string        `json:"fxSource"`
	FxAsOf                   *string        `json:"fxAsOf"`
	Details                  map[string]any `json:"details"`
	RealizedPL               struct {
		Amount   string  `json:"amount"`
		Currency string  `json:"currency"`
		Base     *string `json:"base"`
		Sells    int     `json:"sells"`
	} `json:"realizedPl"`
	Wrapper      *string       `json:"wrapper"`
	InstrumentID *string       `json:"instrumentId"`
	IsShared     bool          `json:"isShared"`
	Valuation    valuationBody `json:"valuation"`
}

type investmentList struct {
	Items   []investmentBody `json:"items"`
	Summary struct {
		Currency        string                                  `json:"currency"`
		TotalValue      string                                  `json:"totalValue"`
		TotalCost       string                                  `json:"totalCost"`
		UnrealizedPL    string                                  `json:"unrealizedPl"`
		UnrealizedPLPct *string                                 `json:"unrealizedPlPct"`
		Warnings        []struct{ Code, Message, Label string } `json:"warnings"`
	} `json:"summary"`
}

func invURL(sid string, rest ...string) string {
	return "/api/v1/spaces/" + sid + "/investments" + strings.Join(rest, "")
}

type invEnv struct {
	s   *TestServer
	u   session
	sid string
}

func newInvEnv(s *TestServer) *invEnv {
	u := s.register("jane@example.com")
	return &invEnv{s: s, u: u, sid: spaceIDOf(u)}
}

func (e *invEnv) post(body map[string]any) Response {
	return e.s.Do(http.MethodPost, invURL(e.sid), body, bearer(e.u.AccessToken))
}

func (e *invEnv) must(body map[string]any) investmentBody {
	e.s.t.Helper()
	res := e.post(body)
	require.Equal(e.s.t, http.StatusCreated, res.Status, string(res.Body))
	var b investmentBody
	res.JSON(e.s.t, &b)
	return b
}

func (e *invEnv) patch(id string, body map[string]any) Response {
	return e.s.Do(http.MethodPatch, invURL(e.sid, "/", id), body, bearer(e.u.AccessToken))
}

func (e *invEnv) get(id string) investmentBody {
	e.s.t.Helper()
	res := e.s.Get(invURL(e.sid, "/", id), bearer(e.u.AccessToken))
	require.Equal(e.s.t, http.StatusOK, res.Status, string(res.Body))
	var b investmentBody
	res.JSON(e.s.t, &b)
	return b
}

func (e *invEnv) list(query string) investmentList {
	e.s.t.Helper()
	res := e.s.Get(invURL(e.sid)+query, bearer(e.u.AccessToken))
	require.Equal(e.s.t, http.StatusOK, res.Status, string(res.Body))
	var l investmentList
	res.JSON(e.s.t, &l)
	return l
}

func (e *invEnv) setSpace(body map[string]any) {
	e.s.t.Helper()
	require.Equal(e.s.t, http.StatusOK, e.s.Do(http.MethodPatch, "/api/v1/spaces/"+e.sid, body, bearer(e.u.AccessToken)).Status)
}

var (
	bbca = map[string]any{"assetType": "ID_STOCK", "name": "Bank Central Asia", "symbol": "bbca",
		"avgBuyPrice": "9200", "manualPrice": "9500", "fees": "5000", "details": map[string]any{"broker": "Stockbit", "lots": 2}}
	aapl = map[string]any{"assetType": "US_STOCK", "name": "Apple", "symbol": "AAPL", "quantity": "10",
		"avgBuyPrice": "100", "manualPrice": "110", "details": map[string]any{"exchange": "NASDAQ", "platform": "IBKR"}}
)

func clone(m map[string]any, kv ...any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	for i := 0; i < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

func TestInvestmentTypesEndpoint(t *testing.T) {
	s := NewTestServer(t)
	assert.Equal(t, http.StatusUnauthorized, s.Get("/api/v1/investment-types").Status)
	u := s.register("jane@example.com")

	res := s.Get("/api/v1/investment-types", bearer(u.AccessToken))
	require.Equal(t, http.StatusOK, res.Status)
	var metas []struct {
		AssetType             string  `json:"assetType"`
		QuoteCurrency         *string `json:"quoteCurrency"`
		QuoteCurrencyEditable bool    `json:"quoteCurrencyEditable"`
		SupportsAutoPrice     bool    `json:"supportsAutoPrice"`
		QuantityMode          string  `json:"quantityMode"`
		LotSize               *int    `json:"lotSize"`
		SymbolRequired        bool    `json:"symbolRequired"`
		Defaults              struct {
			PriceMode                string
			EstimatedAnnualChangePct *string
		} `json:"defaults"`
		DetailFields []struct {
			Name     string   `json:"name"`
			Required bool     `json:"required"`
			Options  []string `json:"options"`
		} `json:"detailFields"`
	}
	res.JSON(t, &metas)
	by := map[string]int{}
	for i, m := range metas {
		by[m.AssetType] = i
	}
	require.Len(t, metas, 5)

	crypto := metas[by["CRYPTO"]]
	assert.Equal(t, "USD", *crypto.QuoteCurrency)
	assert.True(t, crypto.SupportsAutoPrice)
	idStock := metas[by["ID_STOCK"]]
	assert.Equal(t, "IDR", *idStock.QuoteCurrency)
	assert.False(t, idStock.SupportsAutoPrice, "D-04: no automatic prices for Indonesian stocks")
	assert.Equal(t, "LOTS", idStock.QuantityMode)
	assert.Equal(t, 100, *idStock.LotSize)
	assert.True(t, metas[by["US_STOCK"]].SupportsAutoPrice)
	assert.True(t, metas[by["GLOBAL_ETF"]].SupportsAutoPrice)
	mf := metas[by["MUTUAL_FUND"]]
	assert.Nil(t, mf.QuoteCurrency)
	assert.True(t, mf.QuoteCurrencyEditable)
	assert.False(t, mf.SupportsAutoPrice)
	assert.False(t, mf.SymbolRequired)
	assert.Equal(t, "MANUAL", mf.Defaults.PriceMode)
	assert.Nil(t, mf.Defaults.EstimatedAnnualChangePct, "no assumed return")
	for _, f := range mf.DetailFields {
		if f.Name == "fundType" {
			assert.True(t, f.Required)
			assert.Contains(t, f.Options, "MONEY_MARKET")
		}
	}
}

func TestCreateEachInvestmentType(t *testing.T) {
	s := NewTestServer(t)
	e := newInvEnv(s)
	e.setSpace(map[string]any{"baseCurrency": "IDR"})

	t.Run("acceptance: BBCA with 2 lots has quantity 200", func(t *testing.T) {
		got := e.must(bbca)
		assert.Equal(t, "ID_STOCK", got.AssetType)
		assert.True(t, dec("200").Equal(dec(got.Quantity)))
		assert.Equal(t, "IDR", got.QuoteCurrency)
		assert.Equal(t, "BBCA", *got.Symbol)
		assert.Equal(t, "MANUAL", got.PriceMode)
		assert.Nil(t, got.EstimatedAnnualChangePct, "not assumed")
		assert.EqualValues(t, 2, got.Details["lots"])
		assert.Equal(t, "Stockbit", got.Details["broker"])
		assert.False(t, got.IsShared)
		assert.Nil(t, got.InstrumentID)
		// valuation: 200 × 9500 = 1,900,000; cost 200 × 9200 + 5000 = 1,845,000
		v := got.Valuation
		assert.True(t, dec("1900000").Equal(dec(v.MarketValue)), v.MarketValue)
		assert.True(t, dec("1845000").Equal(dec(v.CostBasis)))
		assert.True(t, dec("55000").Equal(dec(v.UnrealizedPL)))
		require.NotNil(t, v.UnrealizedPLPct)
		assert.Equal(t, "2.98", *v.UnrealizedPLPct, "55000 / 1845000, a percentage with 2 decimals")
		assert.Equal(t, "MANUAL", v.PriceSource)
		require.NotNil(t, v.Base)
		assert.Equal(t, "IDR", v.Base.Currency)
		assert.True(t, dec("1").Equal(dec(v.Base.FxRate)))
	})

	t.Run("crypto", func(t *testing.T) {
		got := e.must(map[string]any{"assetType": "CRYPTO", "name": "Ethereum", "symbol": "eth", "quantity": "2",
			"avgBuyPrice": "3000", "manualPrice": "3500", "details": map[string]any{"network": "Ethereum", "stakedQuantity": "1.5"}})
		assert.Equal(t, "USD", got.QuoteCurrency)
		assert.Equal(t, "ETH", *got.Symbol)
		assert.Equal(t, "1.5", got.Details["stakedQuantity"])
	})

	t.Run("us stock, mutual fund and etf", func(t *testing.T) {
		got := e.must(clone(aapl, "purchaseDate", "2026-01-31", "manualFxRate", "16000"))
		assert.Equal(t, "2026-01-31", *got.PurchaseDate)
		assert.Equal(t, "16000", *got.ManualFxRate)

		fund := e.must(map[string]any{"assetType": "MUTUAL_FUND", "name": "Pasar Uang", "quantity": "1000.5",
			"avgBuyPrice": "1500", "manualPrice": "1600", "details": map[string]any{"fundManager": "Bareksa", "fundType": "MONEY_MARKET", "navDate": "2026-11-17"}})
		assert.Equal(t, "IDR", fund.QuoteCurrency, "defaults to the space base currency")
		assert.Nil(t, fund.Symbol)
		usdFund := e.must(map[string]any{"assetType": "MUTUAL_FUND", "name": "USD fund", "quantity": "10", "quoteCurrency": "usd", "details": map[string]any{"fundType": "EQUITY"}})
		assert.Equal(t, "USD", usdFund.QuoteCurrency)

		etf := e.must(map[string]any{"assetType": "GLOBAL_ETF", "name": "VOO", "symbol": "voo", "quantity": "3",
			"details": map[string]any{"exchange": "NYSE", "expenseRatioPct": "0.03", "assetClass": "Equity"}})
		assert.Equal(t, "USD", etf.QuoteCurrency)
		assert.Equal(t, "0.03", etf.Details["expenseRatioPct"])
	})

	t.Run("validation (400) and unauthenticated", func(t *testing.T) {
		assert.Equal(t, http.StatusUnauthorized, s.Do(http.MethodPost, invURL(e.sid), bbca).Status)
		for name, body := range map[string]map[string]any{
			"unknown type":      {"assetType": "BOND", "name": "x"},
			"no type":           {"name": "x"},
			"no lots":           clone(bbca, "details", map[string]any{}),
			"staked > qty":      {"assetType": "CRYPTO", "name": "B", "symbol": "BTC", "quantity": "1", "details": map[string]any{"stakedQuantity": "2"}},
			"negative price":    clone(aapl, "manualPrice", "-1"),
			"wrong quote":       clone(aapl, "quoteCurrency", "IDR"),
			"no exchange":       clone(aapl, "details", map[string]any{}),
			"fractional lots":   clone(bbca, "details", map[string]any{"lots": 1.5}),
			"unknown field":     clone(aapl, "ownershipPct", 50),
			"bad fund type":     {"assetType": "MUTUAL_FUND", "name": "F", "quantity": "1", "details": map[string]any{"fundType": "BOND"}},
			"too many decimals": clone(aapl, "avgBuyPrice", "1.123456789"),
		} {
			res := e.post(body)
			assert.Equal(t, http.StatusBadRequest, res.Status, "%s: %s", name, res.Body)
		}
	})

	t.Run("AUTO pricing: accepted where supported, 422 elsewhere", func(t *testing.T) {
		got := e.must(map[string]any{"assetType": "CRYPTO", "name": "Bitcoin", "symbol": "BTC", "quantity": "0.5",
			"avgBuyPrice": "60000", "manualPrice": "65000", "priceMode": "AUTO"})
		assert.Equal(t, "AUTO", got.PriceMode)
		assert.Equal(t, "MARKET", got.Valuation.PriceSource, "linked to the coin and priced right away (P11)")
		assert.True(t, dec("63000").Equal(dec(got.Valuation.Price)), "the market price, not the 65000 manual one")
		assert.False(t, got.Valuation.IsStale)
		assert.NotNil(t, got.Valuation.PriceAsOf)
		assert.NotNil(t, got.InstrumentID)

		for _, body := range []map[string]any{clone(bbca, "priceMode", "AUTO"),
			{"assetType": "MUTUAL_FUND", "name": "F", "quantity": "1", "priceMode": "AUTO", "details": map[string]any{"fundType": "EQUITY"}}} {
			res := e.post(body)
			require.Equal(t, http.StatusUnprocessableEntity, res.Status, string(res.Body))
			assert.Equal(t, "AUTO_PRICE_NOT_SUPPORTED", errCode(t, res))
		}
		assert.Equal(t, http.StatusCreated, e.post(clone(aapl, "priceMode", "AUTO")).Status)
		assert.Equal(t, http.StatusCreated, e.post(map[string]any{"assetType": "GLOBAL_ETF", "name": "VOO", "symbol": "VOO", "quantity": "1", "priceMode": "AUTO", "details": map[string]any{"exchange": "NYSE"}}).Status)
	})
}

func TestValuationAndConversion(t *testing.T) {
	ctx := context.Background()
	s := NewTestServer(t)
	require.NoError(t, s.Currency.Refresh(ctx)) // 1 USD = 16000 IDR = 0.8 EUR
	e := newInvEnv(s)
	e.setSpace(map[string]any{"baseCurrency": "IDR"})
	apple := e.must(aapl) // 10 shares, cost 1000 USD, value 1100 USD

	t.Run("auto FX converts a USD investment into the IDR base", func(t *testing.T) {
		v := apple.Valuation
		assert.True(t, dec("1100").Equal(dec(v.MarketValue)), "native")
		require.NotNil(t, v.Base)
		assert.Equal(t, "IDR", v.Base.Currency)
		assert.True(t, dec("17600000").Equal(dec(v.Base.MarketValue)), v.Base.MarketValue)
		assert.True(t, dec("16000000").Equal(dec(v.Base.CostBasis)))
		assert.True(t, dec("1600000").Equal(dec(v.Base.UnrealizedPL)))
		assert.True(t, dec("16000").Equal(dec(v.Base.FxRate)))
	})

	t.Run("acceptance: auto FX off + manualFxRate 16,000 converts with 16,000", func(t *testing.T) {
		e.setSpace(map[string]any{"autoFxConversion": false})
		require.Equal(t, http.StatusOK, s.Do(http.MethodPut, "/api/v1/spaces/"+e.sid+"/fx/manual-rates/USD/IDR", map[string]any{"rate": "15000"}, bearer(e.u.AccessToken)).Status)

		// without a per-investment rate the space's manual rate (15000) is used
		got := e.get(apple.ID)
		assert.True(t, dec("15000").Equal(dec(got.Valuation.Base.FxRate)), got.Valuation.Base.FxRate)

		res := e.patch(apple.ID, map[string]any{"manualFxRate": "16000"})
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		res.JSON(t, &got)
		assert.True(t, dec("16000").Equal(dec(got.Valuation.Base.FxRate)), "the per-investment rate overrides the space rate")
		assert.True(t, dec("17600000").Equal(dec(got.Valuation.Base.MarketValue)))

		// with auto FX back on, the per-investment rate is ignored
		e.setSpace(map[string]any{"autoFxConversion": true})
		got = e.get(apple.ID)
		assert.True(t, dec("16000").Equal(dec(got.Valuation.Base.FxRate)))
		s.FX.Set.Rates["IDR"] = dec("17000")
		require.NoError(t, s.Currency.Refresh(ctx))
		s.Clock.Advance(24 * 3600 * 1e9)
		s.FX.Set = defaultRates(s.Clock)
		s.FX.Set.Rates["IDR"] = dec("17000")
		require.NoError(t, s.Currency.Refresh(ctx))
		got = e.get(apple.ID)
		assert.True(t, dec("17000").Equal(dec(got.Valuation.Base.FxRate)), "live rate, not the 16000 override: %s", got.Valuation.Base.FxRate)
	})

	t.Run("manual current value overrides quantity × price", func(t *testing.T) {
		got := e.must(clone(bbca, "manualCurrentValue", "2000000"))
		assert.Equal(t, "MANUAL_VALUE", got.Valuation.PriceSource)
		assert.True(t, dec("2000000").Equal(dec(got.Valuation.MarketValue)), "not 200 × 9500 = 1,900,000")
		assert.True(t, dec("155000").Equal(dec(got.Valuation.UnrealizedPL)))
		// clearing it restores quantity × price
		res := e.patch(got.ID, map[string]any{"manualCurrentValue": nil})
		require.Equal(t, http.StatusOK, res.Status)
		res.JSON(t, &got)
		assert.Equal(t, "MANUAL", got.Valuation.PriceSource)
		assert.True(t, dec("1900000").Equal(dec(got.Valuation.MarketValue)))
	})

	t.Run("a currency without a rate leaves base null with a warning", func(t *testing.T) {
		got := e.must(map[string]any{"assetType": "MUTUAL_FUND", "name": "Singapore fund", "quantity": "10", "quoteCurrency": "SGD",
			"avgBuyPrice": "1", "manualPrice": "2", "details": map[string]any{"fundType": "EQUITY"}})
		assert.Nil(t, got.Valuation.Base)
		require.Len(t, got.Valuation.Warnings, 1)
		assert.Equal(t, "FX_RATE_UNAVAILABLE", got.Valuation.Warnings[0].Code)
		assert.True(t, dec("20").Equal(dec(got.Valuation.MarketValue)), "native values are still shown")
	})
}

func TestInvestmentListAndSummary(t *testing.T) {
	ctx := context.Background()
	s := NewTestServer(t)
	require.NoError(t, s.Currency.Refresh(ctx))
	e := newInvEnv(s)
	e.setSpace(map[string]any{"baseCurrency": "USD"})

	assert.Equal(t, http.StatusUnauthorized, s.Get(invURL(e.sid)).Status)
	empty := e.list("")
	assert.Empty(t, empty.Items)
	assert.True(t, dec("0").Equal(dec(empty.Summary.TotalValue)))
	assert.Nil(t, empty.Summary.UnrealizedPLPct)
	assert.NotNil(t, empty.Summary.Warnings)

	us := e.must(aapl) // value 1100, cost 1000
	id := e.must(clone(bbca, "avgBuyPrice", "8000", "manualPrice", "8000", "fees", "0",
		"details", map[string]any{"lots": 10})) // 1000 shares × 8000 IDR = 8,000,000 IDR = 500 USD, cost the same
	e.must(map[string]any{"assetType": "MUTUAL_FUND", "name": "Singapore fund", "quantity": "10", "quoteCurrency": "SGD",
		"avgBuyPrice": "1", "manualPrice": "2", "details": map[string]any{"fundType": "EQUITY"}})

	l := e.list("")
	require.Len(t, l.Items, 3)
	var got []string
	for _, it := range l.Items {
		got = append(got, it.ID)
	}
	assert.Contains(t, got, us.ID)
	assert.Equal(t, "USD", l.Summary.Currency)
	assert.True(t, dec("1600").Equal(dec(l.Summary.TotalValue)), "1100 + 500: %s", l.Summary.TotalValue)
	assert.True(t, dec("1500").Equal(dec(l.Summary.TotalCost)))
	assert.True(t, dec("100").Equal(dec(l.Summary.UnrealizedPL)))
	require.NotNil(t, l.Summary.UnrealizedPLPct)
	assert.Equal(t, "6.67", *l.Summary.UnrealizedPLPct)
	require.Len(t, l.Summary.Warnings, 1, "the SGD fund is left out and reported")
	assert.Equal(t, "Singapore fund", l.Summary.Warnings[0].Label)

	t.Run("filter by asset type", func(t *testing.T) {
		f := e.list("?assetType=ID_STOCK")
		require.Len(t, f.Items, 1)
		assert.Equal(t, id.ID, f.Items[0].ID)
		assert.True(t, dec("500").Equal(dec(f.Summary.TotalValue)))
		assert.Len(t, e.list("?assetType=crypto").Items, 0)
		assert.Equal(t, http.StatusBadRequest, s.Get(invURL(e.sid)+"?assetType=BOND", bearer(e.u.AccessToken)).Status)
	})

	t.Run("deleted investments leave the list and the summary", func(t *testing.T) {
		require.Equal(t, http.StatusNoContent, s.Do(http.MethodDelete, invURL(e.sid, "/", id.ID), nil, bearer(e.u.AccessToken)).Status)
		l := e.list("")
		assert.Len(t, l.Items, 2)
		assert.True(t, dec("1100").Equal(dec(l.Summary.TotalValue)))
	})
}

func TestInvestmentUpdateAndDelete(t *testing.T) {
	s := NewTestServer(t)
	e := newInvEnv(s)
	e.setSpace(map[string]any{"baseCurrency": "IDR"})
	stock := e.must(bbca)

	t.Run("partial update keeps the rest", func(t *testing.T) {
		res := e.patch(stock.ID, map[string]any{"manualPrice": "9800", "name": "  BCA  ", "estimatedAnnualChangePct": "8.25"})
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		var got investmentBody
		res.JSON(t, &got)
		assert.Equal(t, "BCA", got.Name)
		assert.True(t, dec("9800").Equal(dec(got.ManualPrice)))
		require.NotNil(t, got.EstimatedAnnualChangePct)
		assert.True(t, dec("8.25").Equal(dec(*got.EstimatedAnnualChangePct)))
		assert.True(t, dec("200").Equal(dec(got.Quantity)))
		assert.Equal(t, "BBCA", *got.Symbol)
		assert.True(t, dec("200").Mul(dec("9800")).Equal(dec(got.Valuation.MarketValue)))
	})

	t.Run("changing lots recomputes the quantity", func(t *testing.T) {
		res := e.patch(stock.ID, map[string]any{"details": map[string]any{"lots": 5, "broker": "IPOT"}})
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		var got investmentBody
		res.JSON(t, &got)
		assert.True(t, dec("500").Equal(dec(got.Quantity)))
		assert.Equal(t, "IPOT", got.Details["broker"])
		// quantity itself cannot be set for lot-based types
		assert.Equal(t, http.StatusBadRequest, e.patch(stock.ID, map[string]any{"quantity": "100"}).Status)
	})

	t.Run("asset type is immutable", func(t *testing.T) {
		assert.Equal(t, http.StatusBadRequest, e.patch(stock.ID, map[string]any{"assetType": "US_STOCK"}).Status)
		assert.Equal(t, http.StatusOK, e.patch(stock.ID, map[string]any{"assetType": "ID_STOCK"}).Status)
		assert.Equal(t, "ID_STOCK", e.get(stock.ID).AssetType)
	})

	t.Run("type rules still apply on update", func(t *testing.T) {
		res := e.patch(stock.ID, map[string]any{"priceMode": "AUTO"})
		require.Equal(t, http.StatusUnprocessableEntity, res.Status)
		assert.Equal(t, "AUTO_PRICE_NOT_SUPPORTED", errCode(t, res))
		eth := e.must(map[string]any{"assetType": "CRYPTO", "name": "ETH", "symbol": "ETH", "quantity": "2", "details": map[string]any{"stakedQuantity": "1"}})
		assert.Equal(t, http.StatusBadRequest, e.patch(eth.ID, map[string]any{"quantity": "0.5"}).Status, "stake would exceed the quantity")
		assert.Equal(t, http.StatusOK, e.patch(eth.ID, map[string]any{"priceMode": "AUTO"}).Status)
		for _, body := range []map[string]any{{"manualPrice": "-1"}, {"manualFxRate": "0"}, {"details": map[string]any{"stakedQuantity": "9"}}, {"name": " "}, {"ownership": 1}} {
			assert.Equal(t, http.StatusBadRequest, e.patch(eth.ID, body).Status, "%v", body)
		}
	})

	t.Run("not found", func(t *testing.T) {
		for _, id := range []string{uuid.NewString(), "nope"} {
			res := e.patch(id, map[string]any{"name": "x"})
			require.Equal(t, http.StatusNotFound, res.Status)
			assert.Equal(t, "INVESTMENT_NOT_FOUND", errCode(t, res))
			assert.Equal(t, http.StatusNotFound, s.Get(invURL(e.sid, "/", id), bearer(e.u.AccessToken)).Status)
			assert.Equal(t, http.StatusNotFound, s.Do(http.MethodDelete, invURL(e.sid, "/", id), nil, bearer(e.u.AccessToken)).Status)
		}
	})

	t.Run("delete is soft", func(t *testing.T) {
		require.Equal(t, http.StatusNoContent, s.Do(http.MethodDelete, invURL(e.sid, "/", stock.ID), nil, bearer(e.u.AccessToken)).Status)
		assert.Equal(t, http.StatusNotFound, s.Get(invURL(e.sid, "/", stock.ID), bearer(e.u.AccessToken)).Status)
		assert.Equal(t, http.StatusNotFound, e.patch(stock.ID, map[string]any{"name": "x"}).Status)
		assert.Equal(t, http.StatusNotFound, s.Do(http.MethodDelete, invURL(e.sid, "/", stock.ID), nil, bearer(e.u.AccessToken)).Status)
		var deleted *string
		require.NoError(t, s.Tx.QueryRow(context.Background(), `SELECT deleted_at::text FROM investments WHERE id = $1`, stock.ID).Scan(&deleted))
		assert.NotNil(t, deleted, "the row is kept")
	})
}

func TestInvestmentsAreSpaceScoped(t *testing.T) {
	s := NewTestServer(t)
	e := newInvEnv(s)
	outsider := s.register("outsider@example.com")
	member := s.register("member@example.com")
	s.join(e.u, e.sid, member)
	inv := e.must(aapl)

	t.Run("outsider gets 404 on every route", func(t *testing.T) {
		expectCrossSpace404(t, s, outsider, http.MethodGet, invURL(e.sid), nil)
		expectCrossSpace404(t, s, outsider, http.MethodPost, invURL(e.sid), aapl)
		expectCrossSpace404(t, s, outsider, http.MethodGet, invURL(e.sid, "/", inv.ID), nil)
		expectCrossSpace404(t, s, outsider, http.MethodPatch, invURL(e.sid, "/", inv.ID), map[string]any{"name": "x"})
		expectCrossSpace404(t, s, outsider, http.MethodDelete, invURL(e.sid, "/", inv.ID), nil)
	})

	t.Run("an investment id from another space is 404 in your own space", func(t *testing.T) {
		mine := spaceIDOf(outsider)
		for _, c := range []struct {
			method string
			body   any
		}{{http.MethodGet, nil}, {http.MethodPatch, map[string]any{"name": "x"}}, {http.MethodDelete, nil}} {
			res := s.Do(c.method, invURL(mine, "/", inv.ID), c.body, bearer(outsider.AccessToken))
			require.Equal(t, http.StatusNotFound, res.Status)
			assert.Equal(t, "INVESTMENT_NOT_FOUND", errCode(t, res))
		}
		var l investmentList
		s.Get(invURL(mine), bearer(outsider.AccessToken)).JSON(t, &l)
		assert.Empty(t, l.Items)
		assert.Equal(t, "Apple", e.get(inv.ID).Name)
	})

	t.Run("members can manage investments", func(t *testing.T) {
		res := s.Do(http.MethodPost, invURL(e.sid), clone(aapl, "name", "Member's Apple"), bearer(member.AccessToken))
		require.Equal(t, http.StatusCreated, res.Status, string(res.Body))
		assert.Len(t, e.list("").Items, 2)
	})
}
