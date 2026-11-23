//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type assetSummaryBody struct {
	Currency     string `json:"currency"`
	TrackingMode string `json:"trackingMode"`
	NetWorth     string `json:"netWorth"`
	Cash         struct {
		Total         string  `json:"total"`
		PctOfNetWorth *string `json:"pctOfNetWorth"`
		Wallets       []struct {
			ID          string  `json:"id"`
			Name        string  `json:"name"`
			Currency    string  `json:"currency"`
			Balance     string  `json:"balance"`
			BaseBalance *string `json:"baseBalance"`
			PctOfTotal  *string `json:"pctOfTotal"`
		} `json:"wallets"`
	} `json:"cash"`
	Investments *struct {
		Total           string  `json:"total"`
		Cost            string  `json:"cost"`
		UnrealizedPL    string  `json:"unrealizedPl"`
		UnrealizedPLPct *string `json:"unrealizedPlPct"`
		Categories      []struct {
			AssetType       string  `json:"assetType"`
			Value           string  `json:"value"`
			UnrealizedPL    string  `json:"unrealizedPl"`
			UnrealizedPLPct *string `json:"unrealizedPlPct"`
			PctOfTotal      *string `json:"pctOfTotal"`
		} `json:"categories"`
	} `json:"investments"`
	FixedAssets *assetSectionBody                       `json:"fixedAssets"`
	Receivables *assetSectionBody                       `json:"receivables"`
	Liabilities *assetSectionBody                       `json:"liabilities"`
	Warnings    []struct{ Code, Message, Label string } `json:"warnings"`
}

type assetSectionBody struct {
	Total string `json:"total"`
	Items []struct {
		Name       string  `json:"name"`
		Currency   string  `json:"currency"`
		Value      string  `json:"value"`
		BaseValue  *string `json:"baseValue"`
		PctOfTotal *string `json:"pctOfTotal"`
	} `json:"items"`
}

func (s *TestServer) assetSummary(u session, sid string) assetSummaryBody {
	s.t.Helper()
	res := s.Get("/api/v1/spaces/"+sid+"/assets/summary", bearer(u.AccessToken))
	require.Equal(s.t, http.StatusOK, res.Status, string(res.Body))
	var b assetSummaryBody
	res.JSON(s.t, &b)
	return b
}

func TestAssetSummaryEqualsNetWorth(t *testing.T) {
	s := NewTestServer(t)
	e, _ := buildFixture(t, s)
	nw := s.networth(e.u, e.sid)
	a := s.assetSummary(e.u, e.sid)

	assert.Equal(t, "USD", a.Currency)
	assert.Equal(t, "FULL_NET_WORTH", a.TrackingMode)
	assert.Equal(t, nw.NetWorth, a.NetWorth)
	assert.Equal(t, nw.Cash, a.Cash.Total)
	require.NotNil(t, a.Investments)
	assert.Equal(t, nw.Investments.Value, a.Investments.Total)
	assert.Equal(t, nw.Investments.Cost, a.Investments.Cost)
	assert.Equal(t, nw.Investments.UnrealizedPL, a.Investments.UnrealizedPL)
	assert.Equal(t, *nw.Investments.UnrealizedPLPct, *a.Investments.UnrealizedPLPct)
	assert.Equal(t, *nw.FixedAssets, a.FixedAssets.Total)
	assert.Equal(t, *nw.Receivables, a.Receivables.Total)
	assert.Equal(t, *nw.Liabilities, a.Liabilities.Total)
	assert.Empty(t, a.Warnings)
	assert.Equal(t, len(nw.Warnings), len(a.Warnings))

	t.Run("cash per wallet", func(t *testing.T) {
		require.Len(t, a.Cash.Wallets, 3)
		sum, pct := decimal.Zero, decimal.Zero
		for _, w := range a.Cash.Wallets {
			require.NotNil(t, w.BaseBalance, w.Name)
			require.NotNil(t, w.PctOfTotal, w.Name)
			sum = sum.Add(dec(*w.BaseBalance))
			pct = pct.Add(dec(*w.PctOfTotal))
		}
		assert.True(t, sum.Equal(dec(a.Cash.Total)), "%s vs %s", sum, a.Cash.Total)
		assert.True(t, pct.Equal(dec("100")), pct.String())
		assert.Equal(t, "2000.00", a.Cash.Total)
		assert.Equal(t, "50.00", *a.Cash.Wallets[0].PctOfTotal, "1000 of 2000")
		require.NotNil(t, a.Cash.PctOfNetWorth)
		assert.Equal(t, "2000.00", a.Cash.Total)
		// net worth = 2000 + 1150 + 10000 + 125 - 500 = 12775
		assert.Equal(t, "12775.00", a.NetWorth)
		assert.Equal(t, "15.66", *a.Cash.PctOfNetWorth, "2000 of 12775")
	})

	t.Run("investments per asset type", func(t *testing.T) {
		require.Len(t, a.Investments.Categories, 2)
		pct := decimal.Zero
		val := decimal.Zero
		for _, c := range a.Investments.Categories {
			pct = pct.Add(dec(*c.PctOfTotal))
			val = val.Add(dec(c.Value))
		}
		assert.True(t, pct.Equal(dec("100")))
		assert.True(t, val.Equal(dec(a.Investments.Total)))
	})

	t.Run("manual asset sections with items", func(t *testing.T) {
		require.Len(t, a.FixedAssets.Items, 1)
		car := a.FixedAssets.Items[0]
		assert.Equal(t, "Car", car.Name)
		assert.Equal(t, "IDR", car.Currency)
		assert.Equal(t, "160000000", car.Value, "IDR shows no decimals")
		assert.Equal(t, "10000.00", *car.BaseValue)
		assert.Equal(t, "100.00", *car.PctOfTotal)
		assert.Equal(t, "EUR", a.Receivables.Items[0].Currency)
		assert.Equal(t, "100.00", a.Receivables.Items[0].Value)
		assert.Equal(t, "125.00", *a.Receivables.Items[0].BaseValue, "100 EUR at 0.8")
		assert.Equal(t, "500.00", a.Liabilities.Total)
	})
}

func TestAssetSummaryCashOnlyAndMissingRates(t *testing.T) {
	s := NewTestServer(t)
	e, _ := buildFixture(t, s)
	s.newWallet(e.u, e.sid, map[string]any{"type": "BANK", "name": "Singapore", "icon": "x", "currency": "SGD", "initialBalance": "70"})
	e.must(map[string]any{"kind": "FIXED_ASSET", "name": "Flat in SG", "currentValue": "1000", "currency": "SGD"})

	a := s.assetSummary(e.u, e.sid)
	nw := s.networth(e.u, e.sid)
	require.Len(t, a.Warnings, 2, "the SGD wallet and the SGD flat")
	assert.Equal(t, "FX_RATE_UNAVAILABLE", a.Warnings[0].Code)
	assert.Equal(t, len(nw.Warnings), len(a.Warnings))
	assert.Equal(t, nw.NetWorth, a.NetWorth)
	for _, w := range a.Cash.Wallets {
		if w.Name == "Singapore" {
			assert.Equal(t, "70.00", w.Balance)
			assert.Nil(t, w.BaseBalance)
			assert.Nil(t, w.PctOfTotal)
		}
	}
	var flat *string
	for _, it := range a.FixedAssets.Items {
		if it.Name == "Flat in SG" {
			flat = it.BaseValue
		}
	}
	assert.Nil(t, flat)
	assert.Equal(t, "10000.00", a.FixedAssets.Total, "left out of the total")

	t.Run("CASH_ONLY returns only cash", func(t *testing.T) {
		require.Equal(t, http.StatusOK, s.Do(http.MethodPatch, "/api/v1/spaces/"+e.sid, map[string]any{"trackingMode": "CASH_ONLY"}, bearer(e.u.AccessToken)).Status)
		res := s.Get("/api/v1/spaces/"+e.sid+"/assets/summary", bearer(e.u.AccessToken))
		require.Equal(t, http.StatusOK, res.Status)
		var raw map[string]json.RawMessage
		res.JSON(t, &raw)
		for _, k := range []string{"investments", "fixedAssets", "receivables", "liabilities"} {
			assert.NotContains(t, raw, k)
		}
		assert.Contains(t, raw, "cash")
		b := s.assetSummary(e.u, e.sid)
		assert.Equal(t, "CASH_ONLY", b.TrackingMode)
		assert.Equal(t, b.Cash.Total, b.NetWorth)
		assert.Equal(t, s.networth(e.u, e.sid).NetWorth, b.NetWorth)
	})

	t.Run("space members only", func(t *testing.T) {
		outsider := s.register("out@example.com")
		expectCrossSpace404(t, s, outsider, http.MethodGet, "/api/v1/spaces/"+e.sid+"/assets/summary", nil)
		assert.Equal(t, http.StatusUnauthorized, s.Get("/api/v1/spaces/"+e.sid+"/assets/summary").Status)
	})
}

var (
	reWhole = regexp.MustCompile(`^-?\d+$`)
	reTwo   = regexp.MustCompile(`^-?\d+\.\d{2}$`)
)

// Money in IDR and JPY has no decimals, in any other currency exactly 2; percentages always 2.
func TestMoneyAndPercentageFormatsAcrossEndpoints(t *testing.T) {
	ctx := context.Background()
	s := NewTestServer(t)
	require.NoError(t, s.Currency.Refresh(ctx)) // 1 USD = 16000 IDR = 150 JPY = 0.8 EUR
	e := newInvEnv(s)
	e.setSpace(map[string]any{"baseCurrency": "IDR"})
	mkw := func(name, cur, bal string) {
		s.newWallet(e.u, e.sid, map[string]any{"type": "BANK", "name": name, "icon": "x", "currency": cur, "initialBalance": bal})
	}
	mkw("Rupiah", "IDR", "1234567.89")
	mkw("Yen", "JPY", "1000.5")
	mkw("Dollars", "USD", "10.005")
	mkw("Euros", "EUR", "33.3333")
	e.must(map[string]any{"assetType": "US_STOCK", "name": "Apple", "symbol": "AAPL", "quantity": "3", "avgBuyPrice": "10.333", "manualPrice": "12.3456",
		"wrapper": "Robinhood", "details": map[string]any{"exchange": "NASDAQ"}})
	e.must(map[string]any{"assetType": "ID_STOCK", "name": "BBCA", "symbol": "BBCA", "avgBuyPrice": "9200.5", "manualPrice": "9500.25", "details": map[string]any{"lots": 3}})
	e.must(map[string]any{"assetType": "MUTUAL_FUND", "name": "Yen fund", "quantity": "7.5", "avgBuyPrice": "100.125", "manualPrice": "111.0625", "quoteCurrency": "JPY",
		"details": map[string]any{"fundType": "EQUITY"}})

	get := func(path string) any {
		res := s.Get(path, bearer(e.u.AccessToken))
		require.Equal(t, http.StatusOK, res.Status, path+": "+string(res.Body))
		var v any
		res.JSON(t, &v)
		return v
	}
	whole := func(path string, v any, ctx string) {
		str, ok := v.(string)
		require.True(t, ok, "%s %s: %v", path, ctx, v)
		assert.Regexp(t, reWhole, str, "%s %s", path, ctx)
	}
	two := func(path string, v any, ctx string) {
		str, ok := v.(string)
		require.True(t, ok, "%s %s: %v", path, ctx, v)
		assert.Regexp(t, reTwo, str, "%s %s", path, ctx)
	}
	obj := func(v any) map[string]any { return v.(map[string]any) }
	list := func(v any) []any { return v.([]any) }

	t.Run("wallets", func(t *testing.T) {
		p := "/api/v1/spaces/" + e.sid + "/wallets"
		l := obj(get(p))
		for _, it := range list(l["items"]) {
			w := obj(it)
			f := two
			if w["currency"] == "IDR" || w["currency"] == "JPY" {
				f = whole
			}
			for _, k := range []string{"balance", "reserved", "available"} {
				f(p, w[k], w["currency"].(string)+" "+k)
			}
			whole(p, w["baseBalance"], "baseBalance in IDR")
			two(p, w["pctOfTotal"], "pctOfTotal")
		}
		total := obj(l["total"])
		whole(p, total["total"], "total in IDR")
		for _, b := range list(total["breakdown"]) {
			bd := obj(b)
			whole(p, bd["converted"], "converted in IDR")
			if bd["currency"] == "IDR" || bd["currency"] == "JPY" {
				whole(p, bd["original"], "original")
			} else {
				two(p, bd["original"], "original")
			}
		}
	})

	t.Run("investments", func(t *testing.T) {
		p := "/api/v1/spaces/" + e.sid + "/investments"
		l := obj(get(p))
		sum := obj(l["summary"])
		for _, k := range []string{"totalValue", "totalCost", "unrealizedPl"} {
			whole(p, sum[k], "summary "+k)
		}
		two(p, sum["unrealizedPlPct"], "summary pct")
		for _, it := range list(l["items"]) {
			inv := obj(it)
			f := two
			if inv["quoteCurrency"] == "IDR" || inv["quoteCurrency"] == "JPY" {
				f = whole
			}
			v := obj(inv["valuation"])
			for _, k := range []string{"marketValue", "costBasis", "unrealizedPl"} {
				f(p, v[k], inv["name"].(string)+" "+k)
			}
			two(p, v["unrealizedPlPct"], "pct")
			two(p, inv["pctOfTotal"], "pctOfTotal")
			for _, k := range []string{"marketValue", "costBasis", "unrealizedPl"} {
				whole(p, obj(v["base"])[k], inv["name"].(string)+" base "+k)
			}
			f(p, obj(inv["realizedPl"])["amount"], "realized")
		}
		// prices and quantities are never rounded
		for _, it := range list(l["items"]) {
			inv := obj(it)
			if inv["name"] == "Apple" {
				assert.Equal(t, "12.3456", obj(inv["valuation"])["price"])
				assert.Equal(t, "10.333", inv["avgBuyPrice"])
			}
			if inv["name"] == "Yen fund" {
				assert.Equal(t, "7.5", inv["quantity"])
			}
		}
	})

	t.Run("grouped and net worth", func(t *testing.T) {
		p := "/api/v1/spaces/" + e.sid + "/investments/grouped"
		g := obj(get(p))
		whole(p, obj(g["total"])["value"], "total")
		for _, c := range list(g["categories"]) {
			cat := obj(c)
			whole(p, cat["value"], "category value")
			two(p, cat["assignedPct"], "assigned")
			for _, w := range list(cat["wrappers"]) {
				whole(p, obj(w)["value"], "wrapper value")
			}
		}
		np := "/api/v1/spaces/" + e.sid + "/net-worth/summary"
		n := obj(get(np))
		whole(np, n["cash"], "cash")
		whole(np, n["netWorth"], "netWorth")
		two(np, obj(n["investments"])["unrealizedPlPct"], "pct")
		a := obj(get("/api/v1/spaces/" + e.sid + "/assets/summary"))
		whole("assets", a["netWorth"], "netWorth")
		for _, w := range list(obj(a["cash"])["wallets"]) {
			wl := obj(w)
			if wl["currency"] == "USD" || wl["currency"] == "EUR" {
				two("assets", wl["balance"], "balance")
			} else {
				whole("assets", wl["balance"], "balance")
			}
			whole("assets", wl["baseBalance"], "baseBalance")
		}
	})

	t.Run("dashboard", func(t *testing.T) {
		p := "/api/v1/spaces/" + e.sid + "/dashboard/overview"
		o := obj(get(p))
		whole(p, o["income"], "income")
		whole(p, o["expense"], "expense")
		whole(p, o["net"], "net")
	})
}
