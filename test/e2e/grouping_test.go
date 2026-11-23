//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type groupedBody struct {
	Currency string `json:"currency"`
	Total    struct {
		Value, Cost, UnrealizedPL string
		UnrealizedPLPct           *string `json:"unrealizedPlPct"`
	} `json:"total"`
	Categories []struct {
		AssetType      string  `json:"assetType"`
		Label          string  `json:"label"`
		Value          string  `json:"value"`
		PctOfTotal     *string `json:"pctOfTotal"`
		AssignedPct    string  `json:"assignedPct"`
		UnallocatedPct string  `json:"unallocatedPct"`
		Wrappers       []struct {
			Wrapper       *string          `json:"wrapper"`
			Label         string           `json:"label"`
			Key           string           `json:"key"`
			Value         string           `json:"value"`
			PctOfCategory *string          `json:"pctOfCategory"`
			PctOfTotal    *string          `json:"pctOfTotal"`
			TargetPct     *string          `json:"targetPct"`
			DriftPct      *string          `json:"driftPct"`
			Count         int              `json:"count"`
			Items         []investmentBody `json:"items"`
		} `json:"wrappers"`
	} `json:"categories"`
	Warnings []struct{ Code, Message, Label string } `json:"warnings"`
}

func (s *TestServer) grouped(u session, sid, query string) groupedBody {
	s.t.Helper()
	res := s.Get(invURL(sid, "/grouped")+query, bearer(u.AccessToken))
	require.Equal(s.t, http.StatusOK, res.Status, string(res.Body))
	var g groupedBody
	res.JSON(s.t, &g)
	return g
}

func crypto(name, exchange, qty, price string) map[string]any {
	return map[string]any{"assetType": "CRYPTO", "name": name, "symbol": strings.ToUpper(name[:3]), "quantity": qty,
		"avgBuyPrice": price, "manualPrice": price, "priceMode": "MANUAL", "details": map[string]any{"exchangeOrWallet": exchange}}
}

func TestInvestmentWrapperColumn(t *testing.T) {
	s := NewTestServer(t)
	e := newInvEnv(s)

	t.Run("derived from details when not sent", func(t *testing.T) {
		assert.Equal(t, "Binance", *e.must(crypto("Bitcoin", "  Binance ", "1", "100")).Wrapper)
		idx := e.must(map[string]any{"assetType": "ID_STOCK", "name": "BCA", "symbol": "BBCA", "avgBuyPrice": "9000", "manualPrice": "9500",
			"details": map[string]any{"broker": "Stockbit", "lots": 1}})
		assert.Equal(t, "Stockbit", *idx.Wrapper)
		us := e.must(map[string]any{"assetType": "US_STOCK", "name": "Apple", "symbol": "AAPL", "quantity": "1", "manualPrice": "10",
			"details": map[string]any{"exchange": "NASDAQ", "platform": "Robinhood"}})
		assert.Equal(t, "Robinhood", *us.Wrapper, "the platform, not the listing venue")
		bare := e.must(map[string]any{"assetType": "US_STOCK", "name": "Tesla", "symbol": "TSLA", "quantity": "1", "manualPrice": "10",
			"details": map[string]any{"exchange": "NASDAQ"}})
		assert.Nil(t, bare.Wrapper)
	})

	t.Run("an explicit wrapper wins, is normalised and validated", func(t *testing.T) {
		body := crypto("Ether", "Binance", "1", "10")
		body["wrapper"] = "  Ledger   Nano "
		assert.Equal(t, "Ledger Nano", *e.must(body).Wrapper)
		body["wrapper"] = strings.Repeat("x", 61)
		res := e.post(body)
		require.Equal(t, http.StatusBadRequest, res.Status, string(res.Body))
		assert.Equal(t, "VALIDATION_FAILED", errCode(t, res))
	})

	t.Run("patch: set, follow details, null re-derives, trades keep it", func(t *testing.T) {
		c := e.must(crypto("Solana", "Kraken", "2", "50"))
		require.Equal(t, "Kraken", *c.Wrapper)
		res := e.patch(c.ID, map[string]any{"wrapper": "Cold storage"})
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		assert.Equal(t, "Cold storage", *e.get(c.ID).Wrapper)
		e.patch(c.ID, map[string]any{"name": "Solana 2"})
		assert.Equal(t, "Cold storage", *e.get(c.ID).Wrapper, "an unrelated edit keeps it")
		e.patch(c.ID, map[string]any{"details": map[string]any{"exchangeOrWallet": "Coinbase"}})
		assert.Equal(t, "Coinbase", *e.get(c.ID).Wrapper, "a changed exchange in details moves the wrapper")
		e.patch(c.ID, map[string]any{"wrapper": "Cold storage"})
		e.patch(c.ID, map[string]any{"wrapper": nil})
		assert.Equal(t, "Coinbase", *e.get(c.ID).Wrapper, "null re-derives from details")

		w := s.newWallet(e.u, e.sid, map[string]any{"type": "BANK", "name": "Cash", "icon": "x", "currency": "USD", "initialBalance": "1000"})
		res = s.Do(http.MethodPost, trURL(e.sid), map[string]any{"from": walletEnd(w), "to": leg(c.ID, "1", "10", "")}, bearer(e.u.AccessToken))
		require.Equal(t, http.StatusCreated, res.Status, string(res.Body))
		assert.Equal(t, "Coinbase", *e.get(c.ID).Wrapper, "a trade does not clear it")
	})
}

func TestGroupedInvestments(t *testing.T) {
	ctx := context.Background()
	s := NewTestServer(t)
	require.NoError(t, s.Currency.Refresh(ctx))
	e := newInvEnv(s)
	e.setSpace(map[string]any{"baseCurrency": "USD"})
	e.must(crypto("Bitcoin", "Binance", "1", "300"))
	e.must(crypto("Ether", "binance ", "1", "100")) // same group, other casing
	e.must(crypto("Solana", "Ledger", "1", "100"))
	e.must(map[string]any{"assetType": "CRYPTO", "name": "Loose", "symbol": "LOO", "quantity": "1", "manualPrice": "50", "avgBuyPrice": "50"})
	e.must(map[string]any{"assetType": "US_STOCK", "name": "Apple", "symbol": "AAPL", "quantity": "1", "manualPrice": "450", "avgBuyPrice": "400",
		"details": map[string]any{"exchange": "NASDAQ", "platform": "Robinhood"}})
	// a currency without a rate: listed, left out of the sums, reported
	e.must(map[string]any{"assetType": "MUTUAL_FUND", "name": "Odd fund", "quantity": "1", "manualPrice": "70", "avgBuyPrice": "70",
		"quoteCurrency": "CHF", "wrapper": "Bibit", "details": map[string]any{"fundType": "EQUITY"}})

	flat := e.list("")
	g := s.grouped(e.u, e.sid, "")

	t.Run("sums equal the flat summary", func(t *testing.T) {
		assert.Equal(t, "USD", g.Currency)
		assert.True(t, dec(flat.Summary.TotalValue).Equal(dec(g.Total.Value)), "%s vs %s", flat.Summary.TotalValue, g.Total.Value)
		assert.True(t, dec(flat.Summary.TotalCost).Equal(dec(g.Total.Cost)))
		assert.True(t, dec(flat.Summary.UnrealizedPL).Equal(dec(g.Total.UnrealizedPL)))
		sum, pct := decimal.Zero, decimal.Zero
		for _, c := range g.Categories {
			sum = sum.Add(dec(c.Value))
			pct = pct.Add(dec(*c.PctOfTotal))
		}
		assert.True(t, sum.Equal(dec(g.Total.Value)))
		assert.True(t, pct.Equal(dec("100")), pct.String())
	})

	t.Run("categories, merged wrappers, unassigned", func(t *testing.T) {
		require.Len(t, g.Categories, 3, "crypto, US stock, mutual fund; empty types are left out")
		c := g.Categories[0]
		assert.Equal(t, "CRYPTO", c.AssetType)
		assert.Equal(t, "Crypto", c.Label)
		require.Len(t, c.Wrappers, 3)
		b := c.Wrappers[0]
		assert.Equal(t, "binance", b.Key)
		assert.Equal(t, 2, b.Count)
		assert.True(t, dec("400").Equal(dec(b.Value)))
		assert.Equal(t, "72.73", *b.PctOfCategory, "400 of 550")
		un := c.Wrappers[2]
		assert.Equal(t, "_unassigned", un.Key)
		assert.Nil(t, un.Wrapper)
		assert.Equal(t, "Unassigned", un.Label)
		inCat := decimal.Zero
		for _, w := range c.Wrappers {
			inCat = inCat.Add(dec(*w.PctOfCategory))
			assert.Regexp(t, regexp.MustCompile(`^\d+\.\d{2}$`), *w.PctOfTotal)
		}
		assert.True(t, inCat.Equal(dec("100")))
		assert.Len(t, b.Items, 2)
	})

	t.Run("an item without an FX rate is excluded and warned about", func(t *testing.T) {
		require.Len(t, g.Warnings, 1)
		assert.Equal(t, "FX_RATE_UNAVAILABLE", g.Warnings[0].Code)
		mf := g.Categories[2]
		assert.Equal(t, "MUTUAL_FUND", mf.AssetType)
		assert.Equal(t, 1, mf.Wrappers[0].Count)
		assert.True(t, dec("0").Equal(dec(mf.Wrappers[0].Value)))
		assert.Equal(t, "0.00", *mf.PctOfTotal)
		assert.Nil(t, mf.Wrappers[0].PctOfCategory, "null when the category is worth 0")
	})

	t.Run("items=false drops the items; bad parameters are 400", func(t *testing.T) {
		light := s.grouped(e.u, e.sid, "?items=false")
		assert.Empty(t, light.Categories[0].Wrappers[0].Items)
		assert.Equal(t, 2, light.Categories[0].Wrappers[0].Count)
		for _, q := range []string{"?items=maybe", "?include=other"} {
			res := s.Get(invURL(e.sid, "/grouped")+q, bearer(e.u.AccessToken))
			assert.Equal(t, http.StatusBadRequest, res.Status, q)
		}
		assert.Equal(t, http.StatusUnauthorized, s.Get(invURL(e.sid, "/grouped")).Status)
	})

	t.Run("the flat list carries pctOfTotal per investment, adding up to 100", func(t *testing.T) {
		res := s.Get(invURL(e.sid), bearer(e.u.AccessToken))
		var l struct {
			Items []struct {
				Name       string  `json:"name"`
				PctOfTotal *string `json:"pctOfTotal"`
			} `json:"items"`
		}
		res.JSON(t, &l)
		sum := decimal.Zero
		for _, it := range l.Items {
			if it.Name == "Odd fund" {
				assert.Nil(t, it.PctOfTotal)
				continue
			}
			require.NotNil(t, it.PctOfTotal, it.Name)
			sum = sum.Add(dec(*it.PctOfTotal))
		}
		assert.True(t, sum.Equal(dec("100")), sum.String())
	})
}

func TestGroupedIncludesSharedInAndIsMembersOnly(t *testing.T) {
	s := NewTestServer(t)
	e := newShareEnv(t, s) // Jane's AAPL, no platform: unassigned
	bob := s.register("bob@example.com")
	own := (&invEnv{s: s, u: bob, sid: spaceIDOf(bob)}).must(crypto("Bitcoin", "Binance", "1", "100"))
	require.NotEmpty(t, own.ID)
	out := e.mustPut(owner("50"), emailP("bob@example.com", "Bob", "50"))
	require.Equal(t, http.StatusOK, s.acceptShare(bob, byName(out.Items, "Bob").ID, spaceIDOf(bob)).Status)

	plain := s.grouped(bob, spaceIDOf(bob), "")
	require.Len(t, plain.Categories, 1, "shared-in items only with include=shared")
	with := s.grouped(bob, spaceIDOf(bob), "?include=shared")
	require.Len(t, with.Categories, 2)
	us := with.Categories[1]
	assert.Equal(t, "US_STOCK", us.AssetType)
	assert.Equal(t, "_unassigned", us.Wrappers[0].Key)
	assert.True(t, dec("500").Equal(dec(us.Value)), "Bob's half of 1,000.01: %s", us.Value)
	assert.Equal(t, 1, us.Wrappers[0].Count)
	expectCrossSpace404(t, s, bob, http.MethodGet, invURL(e.sid, "/grouped"), nil)
}

func TestWalletListHasBaseBalanceAndPctOfTotal(t *testing.T) {
	s := NewTestServer(t)
	require.NoError(t, s.Currency.Refresh(context.Background())) // 1 USD = 0.8 EUR
	u := s.register("jane@example.com")
	sid := spaceIDOf(u)
	mk := func(name, cur, bal string) {
		s.newWallet(u, sid, map[string]any{"type": "BANK", "name": name, "icon": "x", "currency": cur, "initialBalance": bal})
	}
	mk("A", "USD", "100")
	mk("B", "EUR", "80") // 100 USD
	mk("C", "USD", "100")
	mk("D", "CHF", "5") // no rate
	res := s.Get(walURL(sid), bearer(u.AccessToken))
	require.Equal(t, http.StatusOK, res.Status, string(res.Body))
	var l struct {
		Items []struct {
			Name        string  `json:"name"`
			BaseBalance *string `json:"baseBalance"`
			PctOfTotal  *string `json:"pctOfTotal"`
		} `json:"items"`
	}
	res.JSON(t, &l)
	require.Len(t, l.Items, 4)
	sum := decimal.Zero
	for _, w := range l.Items {
		if w.Name == "D" {
			assert.Nil(t, w.BaseBalance)
			assert.Nil(t, w.PctOfTotal)
			continue
		}
		require.NotNil(t, w.PctOfTotal, w.Name)
		assert.True(t, dec("100").Equal(dec(*w.BaseBalance)), w.Name)
		sum = sum.Add(dec(*w.PctOfTotal))
	}
	assert.True(t, sum.Equal(dec("100")), sum.String())
	// the single-wallet response has no percentage
	var one struct {
		ID         string  `json:"id"`
		PctOfTotal *string `json:"pctOfTotal"`
	}
	list := s.walletList(u, sid)
	s.Get(walURL(sid, "/", list.Items[0].ID), bearer(u.AccessToken)).JSON(t, &one)
	assert.Nil(t, one.PctOfTotal)
}
