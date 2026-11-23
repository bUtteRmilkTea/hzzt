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

type walletBody struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Name       string `json:"name"`
	Icon       string `json:"icon"`
	Currency   string `json:"currency"`
	Balance    string `json:"balance"`
	IsFavorite bool   `json:"isFavorite"`
	SortOrder  int    `json:"sortOrder"`
	CardStyle  struct {
		Mode       string   `json:"mode"`
		Color      string   `json:"color"`
		Colors     []string `json:"colors"`
		Direction  string   `json:"direction"`
		PresetID   *string  `json:"presetId"`
		TemplateID string   `json:"templateId"`
	} `json:"cardStyle"`
}

type walletListBody struct {
	Items []walletBody `json:"items"`
	Total struct {
		Total     string `json:"total"`
		Currency  string `json:"currency"`
		Breakdown []struct {
			Currency  string `json:"currency"`
			Original  string `json:"original"`
			Converted string `json:"converted"`
		} `json:"breakdown"`
		Warnings []struct {
			Code, Message, Label string
		} `json:"warnings"`
	} `json:"total"`
}

func walURL(sid string, rest ...string) string {
	return "/api/v1/spaces/" + sid + "/wallets" + strings.Join(rest, "")
}

func wal(name string) map[string]any {
	return map[string]any{"type": "BANK", "name": name, "icon": "🏦"}
}

func (s *TestServer) newWallet(u session, sid string, body map[string]any) walletBody {
	s.t.Helper()
	res := s.Do(http.MethodPost, walURL(sid), body, bearer(u.AccessToken))
	require.Equal(s.t, http.StatusCreated, res.Status, string(res.Body))
	var w walletBody
	res.JSON(s.t, &w)
	return w
}

func (s *TestServer) walletList(u session, sid string) walletListBody {
	s.t.Helper()
	res := s.Get(walURL(sid), bearer(u.AccessToken))
	require.Equal(s.t, http.StatusOK, res.Status, string(res.Body))
	var l walletListBody
	res.JSON(s.t, &l)
	return l
}

func (s *TestServer) walletTxns(walletID string) []struct{ Type, Direction, Title, Amount string } {
	s.t.Helper()
	rows, err := s.Tx.Query(context.Background(),
		`SELECT type, COALESCE(direction, ''), COALESCE(title, ''), amount::text FROM transactions WHERE wallet_id = $1 ORDER BY occurred_at, id`, walletID)
	require.NoError(s.t, err)
	defer rows.Close()
	var out []struct{ Type, Direction, Title, Amount string }
	for rows.Next() {
		var x struct{ Type, Direction, Title, Amount string }
		require.NoError(s.t, rows.Scan(&x.Type, &x.Direction, &x.Title, &x.Amount))
		out = append(out, x)
	}
	return out
}

func TestWalletCreateAndOpeningBalance(t *testing.T) {
	s := NewTestServer(t)
	u := s.register("jane@example.com")
	sid := spaceIDOf(u)

	t.Run("unauthenticated", func(t *testing.T) {
		assert.Equal(t, http.StatusUnauthorized, s.Get(walURL(sid)).Status)
		assert.Equal(t, http.StatusUnauthorized, s.Do(http.MethodPost, walURL(sid), wal("x")).Status)
	})

	t.Run("defaults: base currency, zero balance, solid style, no transaction", func(t *testing.T) {
		w := s.newWallet(u, sid, wal("  Main  "))
		assert.Equal(t, "Main", w.Name)
		assert.Equal(t, "USD", w.Currency, "defaults to the space base currency")
		assert.True(t, dec("0").Equal(dec(w.Balance)))
		assert.Equal(t, "SOLID", w.CardStyle.Mode)
		assert.Equal(t, "#4480EE", w.CardStyle.Color)
		assert.False(t, w.IsFavorite)
		assert.Empty(t, s.walletTxns(w.ID), "no opening transaction for a zero balance")
	})

	t.Run("acceptance: initialBalance 134.26 → one OPENING_BALANCE", func(t *testing.T) {
		body := wal("Savings")
		body["initialBalance"] = "134.26"
		w := s.newWallet(u, sid, body)
		assert.True(t, dec("134.26").Equal(dec(w.Balance)), w.Balance)
		txns := s.walletTxns(w.ID)
		require.Len(t, txns, 1)
		assert.Equal(t, "OPENING_BALANCE", txns[0].Type)
		assert.Equal(t, "IN", txns[0].Direction)
		assert.Equal(t, "Opening balance", txns[0].Title)
		assert.True(t, dec("134.26").Equal(dec(txns[0].Amount)))
		expectLedgerConsistent(t, s, w.ID)
	})

	t.Run("negative initial balance is an OUT opening balance", func(t *testing.T) {
		body := wal("Credit card")
		body["initialBalance"] = "-250"
		w := s.newWallet(u, sid, body)
		assert.True(t, dec("-250").Equal(dec(w.Balance)))
		txns := s.walletTxns(w.ID)
		require.Len(t, txns, 1)
		assert.Equal(t, "OUT", txns[0].Direction)
		assert.True(t, dec("250").Equal(dec(txns[0].Amount)), "amounts are always positive")
	})

	t.Run("own currency and numeric JSON balance", func(t *testing.T) {
		body := wal("Rupiah")
		body["currency"] = "idr"
		body["initialBalance"] = 1500000
		body["isFavorite"] = true
		w := s.newWallet(u, sid, body)
		assert.Equal(t, "IDR", w.Currency)
		assert.True(t, w.IsFavorite)
		assert.True(t, dec("1500000").Equal(dec(w.Balance)))
	})

	t.Run("card styles", func(t *testing.T) {
		for name, style := range map[string]map[string]any{
			"solid":    {"mode": "SOLID", "color": "#112233"},
			"gradient": {"mode": "GRADIENT", "colors": []string{"#4480EE", "#0E76B7"}, "direction": "NE", "presetId": "ocean"},
			"template": {"mode": "TEMPLATE", "templateId": "marble"},
		} {
			body := wal("Style " + name)
			body["cardStyle"] = style
			w := s.newWallet(u, sid, body)
			assert.Equal(t, strings.ToUpper(name), w.CardStyle.Mode)
		}
	})

	t.Run("validation", func(t *testing.T) {
		mk := func(mod func(m map[string]any)) map[string]any { m := wal("Valid"); mod(m); return m }
		for name, body := range map[string]map[string]any{
			"missing name":         mk(func(m map[string]any) { delete(m, "name") }),
			"blank name":           mk(func(m map[string]any) { m["name"] = "  " }),
			"long name":            mk(func(m map[string]any) { m["name"] = strings.Repeat("x", 31) }),
			"bad type":             mk(func(m map[string]any) { m["type"] = "CRYPTO" }),
			"missing icon":         mk(func(m map[string]any) { delete(m, "icon") }),
			"unsupported currency": mk(func(m map[string]any) { m["currency"] = "XXX" }),
			"balance not a number": mk(func(m map[string]any) { m["initialBalance"] = "lots" }),
			"too many decimals":    mk(func(m map[string]any) { m["initialBalance"] = "1.123456789" }),
			"balance too large":    mk(func(m map[string]any) { m["initialBalance"] = "100000000000000000000" }),
			"bad card style":       mk(func(m map[string]any) { m["cardStyle"] = map[string]any{"mode": "SOLID", "color": "red"} }),
			"unknown preset": mk(func(m map[string]any) {
				m["cardStyle"] = map[string]any{"mode": "GRADIENT", "colors": []string{"#4480EE", "#0E76B7"}, "direction": "N", "presetId": "nope"}
			}),
			"unknown template": mk(func(m map[string]any) { m["cardStyle"] = map[string]any{"mode": "TEMPLATE", "templateId": "nope"} }),
			"unknown field":    mk(func(m map[string]any) { m["balance"] = "5" }),
		} {
			res := s.Do(http.MethodPost, walURL(sid), body, bearer(u.AccessToken))
			assert.Equal(t, http.StatusBadRequest, res.Status, "%s: %s", name, res.Body)
		}
		// 8 decimals is the limit
		body := wal("Precise")
		body["initialBalance"] = "0.12345678"
		assert.Equal(t, http.StatusCreated, s.Do(http.MethodPost, walURL(sid), body, bearer(u.AccessToken)).Status)
	})
}

func TestWalletListOrderAndTotal(t *testing.T) {
	s := NewTestServer(t)
	require.NoError(t, s.Currency.Refresh(context.Background())) // 1 USD = 16000 IDR = 0.8 EUR
	u := s.register("jane@example.com")
	sid := spaceIDOf(u)

	mk := func(name, cur, bal string, fav bool) walletBody {
		b := wal(name)
		b["currency"], b["initialBalance"], b["isFavorite"] = cur, bal, fav
		return s.newWallet(u, sid, b)
	}
	a := mk("A", "USD", "100", false)
	b := mk("B", "IDR", "160000", false)
	c := mk("C", "USD", "10", true)

	l := s.walletList(u, sid)
	require.Len(t, l.Items, 3)
	assert.Equal(t, []string{"C", "A", "B"}, []string{l.Items[0].Name, l.Items[1].Name, l.Items[2].Name}, "favorites first, then creation order")

	assert.Equal(t, "USD", l.Total.Currency)
	assert.True(t, dec("120").Equal(dec(l.Total.Total)), l.Total.Total) // 100 + 10 + 160000/16000
	require.Len(t, l.Total.Breakdown, 2)
	assert.Empty(t, l.Total.Warnings)

	t.Run("a wallet in a currency without a rate is excluded with a warning", func(t *testing.T) {
		mk("Pounds", "GBP", "50", false) // the fake provider has a GBP rate but no SGD rate
		e := mk("Dollars SG", "SGD", "70", false)
		l := s.walletList(u, sid)
		assert.Equal(t, "186.67", l.Total.Total, "120 + 50 / 0.75, shown with 2 decimals")
		require.Len(t, l.Total.Warnings, 1)
		assert.Equal(t, "FX_RATE_UNAVAILABLE", l.Total.Warnings[0].Code)
		assert.Equal(t, "Dollars SG", l.Total.Warnings[0].Label)
		assert.Len(t, l.Items, 5, "the wallet itself is still listed")
		require.Equal(t, http.StatusNoContent, s.Do(http.MethodDelete, walURL(sid, "/", e.ID)+"?force=true", nil, bearer(u.AccessToken)).Status)
	})

	t.Run("deleted wallets are excluded from totals", func(t *testing.T) {
		before := dec(s.walletList(u, sid).Total.Total)
		require.Equal(t, http.StatusNoContent, s.Do(http.MethodDelete, walURL(sid, "/", b.ID)+"?force=true", nil, bearer(u.AccessToken)).Status)
		after := s.walletList(u, sid)
		assert.True(t, before.Sub(dec("10")).Equal(dec(after.Total.Total)), "IDR wallet was worth 10 USD")
		for _, it := range after.Items {
			assert.NotEqual(t, b.ID, it.ID)
		}
	})

	t.Run("manual FX mode changes the total", func(t *testing.T) {
		require.Equal(t, http.StatusOK, s.Do(http.MethodPatch, "/api/v1/spaces/"+sid, map[string]any{"autoFxConversion": false, "baseCurrency": "EUR"}, bearer(u.AccessToken)).Status)
		l := s.walletList(u, sid)
		assert.Equal(t, "EUR", l.Total.Currency)
		assert.True(t, dec("0").Equal(dec(l.Total.Total)), "no manual rates yet → everything excluded")
		assert.NotEmpty(t, l.Total.Warnings)
		require.Equal(t, http.StatusOK, s.Do(http.MethodPut, "/api/v1/spaces/"+sid+"/fx/manual-rates/USD/EUR", map[string]any{"rate": "0.5"}, bearer(u.AccessToken)).Status)
		l = s.walletList(u, sid)
		assert.True(t, dec("55").Equal(dec(l.Total.Total)) || dec("55").Sub(dec(l.Total.Total)).Abs().LessThan(dec("5")), l.Total.Total)
	})

	t.Run("reorder", func(t *testing.T) {
		cur := s.walletList(u, sid)
		var ids []string
		for i := len(cur.Items) - 1; i >= 0; i-- {
			ids = append(ids, cur.Items[i].ID)
		}
		res := s.Do(http.MethodPut, walURL(sid, "/order"), map[string]any{"ids": ids}, bearer(u.AccessToken))
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		after := s.walletList(u, sid)
		// the favorite stays first; the rest follow the new order
		assert.Equal(t, c.ID, after.Items[0].ID)
		assert.Equal(t, a.ID, after.Items[len(after.Items)-1].ID)

		assert.Equal(t, http.StatusBadRequest, s.Do(http.MethodPut, walURL(sid, "/order"), map[string]any{"ids": ids[1:]}, bearer(u.AccessToken)).Status)
		assert.Equal(t, http.StatusBadRequest, s.Do(http.MethodPut, walURL(sid, "/order"), map[string]any{"ids": append(append([]string{}, ids...), uuid.NewString())}, bearer(u.AccessToken)).Status)
	})
}

func TestWalletUpdate(t *testing.T) {
	s := NewTestServer(t)
	u := s.register("jane@example.com")
	sid := spaceIDOf(u)
	empty := s.newWallet(u, sid, wal("Empty"))
	body := wal("Funded")
	body["initialBalance"] = "10"
	funded := s.newWallet(u, sid, body)

	patch := func(id string, b map[string]any) Response {
		return s.Do(http.MethodPatch, walURL(sid, "/", id), b, bearer(u.AccessToken))
	}

	t.Run("partial update", func(t *testing.T) {
		res := patch(funded.ID, map[string]any{"name": "Renamed", "isFavorite": true, "type": "CASH", "icon": "💵",
			"cardStyle": map[string]any{"mode": "TEMPLATE", "templateId": "grid"}})
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		var w walletBody
		res.JSON(t, &w)
		assert.Equal(t, "Renamed", w.Name)
		assert.Equal(t, "CASH", w.Type)
		assert.True(t, w.IsFavorite)
		assert.Equal(t, "TEMPLATE", w.CardStyle.Mode)
		assert.True(t, dec("10").Equal(dec(w.Balance)), "balance untouched")

		res = patch(funded.ID, map[string]any{"isFavorite": false})
		res.JSON(t, &w)
		assert.False(t, w.IsFavorite)
		assert.Equal(t, "Renamed", w.Name)
		assert.Equal(t, "TEMPLATE", w.CardStyle.Mode, "style kept when not sent")
	})

	t.Run("balance cannot be patched", func(t *testing.T) {
		assert.Equal(t, http.StatusBadRequest, patch(funded.ID, map[string]any{"balance": "999"}).Status)
	})

	t.Run("validation", func(t *testing.T) {
		for name, b := range map[string]map[string]any{
			"blank name": {"name": " "}, "bad type": {"type": "X"}, "bad currency": {"currency": "XXX"},
			"bad style": {"cardStyle": map[string]any{"mode": "SOLID", "color": "zzz"}},
		} {
			assert.Equal(t, http.StatusBadRequest, patch(funded.ID, b).Status, name)
		}
		assert.Equal(t, http.StatusNotFound, patch(uuid.NewString(), map[string]any{"name": "x"}).Status)
		assert.Equal(t, http.StatusNotFound, patch("nope", map[string]any{"name": "x"}).Status)
	})

	t.Run("currency is locked once transactions exist", func(t *testing.T) {
		res := patch(funded.ID, map[string]any{"currency": "EUR"})
		require.Equal(t, http.StatusUnprocessableEntity, res.Status, string(res.Body))
		assert.Equal(t, "WALLET_CURRENCY_LOCKED", errCode(t, res))
		// re-sending the same currency is fine
		assert.Equal(t, http.StatusOK, patch(funded.ID, map[string]any{"currency": "usd"}).Status)
		// a wallet with no transactions can still change currency
		res = patch(empty.ID, map[string]any{"currency": "EUR"})
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		var w walletBody
		res.JSON(t, &w)
		assert.Equal(t, "EUR", w.Currency)
	})
}

func TestWalletSetBalance(t *testing.T) {
	s := NewTestServer(t)
	u := s.register("jane@example.com")
	sid := spaceIDOf(u)
	body := wal("Main")
	body["initialBalance"] = "134.26"
	w := s.newWallet(u, sid, body)
	put := func(b map[string]any) Response {
		return s.Do(http.MethodPut, walURL(sid, "/", w.ID, "/balance"), b, bearer(u.AccessToken))
	}

	t.Run("acceptance: setting 100 creates an ADJUSTMENT OUT of 34.26", func(t *testing.T) {
		res := put(map[string]any{"newBalance": "100", "note": "counted the cash"})
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		var got walletBody
		res.JSON(t, &got)
		assert.True(t, dec("100").Equal(dec(got.Balance)))

		txns := s.walletTxns(w.ID)
		require.Len(t, txns, 2)
		assert.Equal(t, "ADJUSTMENT", txns[1].Type)
		assert.Equal(t, "OUT", txns[1].Direction)
		assert.Equal(t, "Balance adjustment", txns[1].Title)
		assert.True(t, dec("34.26").Equal(dec(txns[1].Amount)), txns[1].Amount)
		var note string
		require.NoError(t, s.Tx.QueryRow(context.Background(), `SELECT note FROM transactions WHERE wallet_id = $1 AND type = 'ADJUSTMENT'`, w.ID).Scan(&note))
		assert.Equal(t, "counted the cash", note)
		expectLedgerConsistent(t, s, w.ID)
	})

	t.Run("raising it creates an ADJUSTMENT IN", func(t *testing.T) {
		require.Equal(t, http.StatusOK, put(map[string]any{"newBalance": "150.5"}).Status)
		txns := s.walletTxns(w.ID)
		require.Len(t, txns, 3)
		assert.Equal(t, "IN", txns[2].Direction)
		assert.True(t, dec("50.5").Equal(dec(txns[2].Amount)))
		expectLedgerConsistent(t, s, w.ID)
	})

	t.Run("an unchanged balance is a no-op", func(t *testing.T) {
		require.Equal(t, http.StatusOK, put(map[string]any{"newBalance": "150.50"}).Status)
		assert.Len(t, s.walletTxns(w.ID), 3)
	})

	t.Run("can go negative", func(t *testing.T) {
		require.Equal(t, http.StatusOK, put(map[string]any{"newBalance": "-20"}).Status)
		expectLedgerConsistent(t, s, w.ID)
		assert.True(t, dec("-20").Equal(dec(s.walletList(u, sid).Items[0].Balance)))
	})

	t.Run("validation", func(t *testing.T) {
		assert.Equal(t, http.StatusBadRequest, put(map[string]any{}).Status)
		assert.Equal(t, http.StatusBadRequest, put(map[string]any{"newBalance": "abc"}).Status)
		assert.Equal(t, http.StatusBadRequest, put(map[string]any{"newBalance": "1.123456789"}).Status)
		assert.Equal(t, http.StatusBadRequest, put(map[string]any{"newBalance": "1", "note": strings.Repeat("n", 501)}).Status)
		res := s.Do(http.MethodPut, walURL(sid, "/", uuid.NewString(), "/balance"), map[string]any{"newBalance": "1"}, bearer(u.AccessToken))
		assert.Equal(t, http.StatusNotFound, res.Status)
	})
}

func TestWalletDelete(t *testing.T) {
	s := NewTestServer(t)
	u := s.register("jane@example.com")
	sid := spaceIDOf(u)
	body := wal("Funded")
	body["initialBalance"] = "5"
	funded := s.newWallet(u, sid, body)
	empty := s.newWallet(u, sid, wal("Empty"))
	del := func(id, q string) Response {
		return s.Do(http.MethodDelete, walURL(sid, "/", id)+q, nil, bearer(u.AccessToken))
	}

	res := del(funded.ID, "")
	require.Equal(t, http.StatusUnprocessableEntity, res.Status)
	assert.Equal(t, "WALLET_HAS_BALANCE", errCode(t, res))
	assert.Equal(t, http.StatusUnprocessableEntity, del(funded.ID, "?force=false").Status)
	assert.Equal(t, http.StatusBadRequest, del(funded.ID, "?force=maybe").Status)

	require.Equal(t, http.StatusNoContent, del(empty.ID, "").Status, "zero balance deletes without force")
	require.Equal(t, http.StatusNoContent, del(funded.ID, "?force=true").Status)
	assert.Equal(t, http.StatusNotFound, del(funded.ID, "?force=true").Status)
	assert.Equal(t, http.StatusNotFound, s.Get(walURL(sid, "/", funded.ID), bearer(u.AccessToken)).Status)
	assert.Empty(t, s.walletList(u, sid).Items)

	var n int
	require.NoError(t, s.Tx.QueryRow(context.Background(), `SELECT count(*) FROM transactions WHERE wallet_id = $1`, funded.ID).Scan(&n))
	assert.Equal(t, 1, n, "history is kept")
}

func TestWalletsAreSpaceScoped(t *testing.T) {
	s := NewTestServer(t)
	owner := s.register("owner@example.com")
	member := s.register("member@example.com")
	outsider := s.register("outsider@example.com")
	sid := spaceIDOf(owner)
	s.join(owner, sid, member)
	w := s.newWallet(owner, sid, wal("Shared"))

	t.Run("outsider gets 404 on every route", func(t *testing.T) {
		expectCrossSpace404(t, s, outsider, http.MethodGet, walURL(sid), nil)
		expectCrossSpace404(t, s, outsider, http.MethodPost, walURL(sid), wal("Hack"))
		expectCrossSpace404(t, s, outsider, http.MethodGet, walURL(sid, "/", w.ID), nil)
		expectCrossSpace404(t, s, outsider, http.MethodPatch, walURL(sid, "/", w.ID), map[string]any{"name": "Hack"})
		expectCrossSpace404(t, s, outsider, http.MethodPut, walURL(sid, "/", w.ID, "/balance"), map[string]any{"newBalance": "1"})
		expectCrossSpace404(t, s, outsider, http.MethodDelete, walURL(sid, "/", w.ID), nil)
		expectCrossSpace404(t, s, outsider, http.MethodPut, walURL(sid, "/order"), map[string]any{"ids": []string{w.ID}})
	})

	t.Run("a wallet id from another space is 404 in your own space", func(t *testing.T) {
		mine := spaceIDOf(outsider)
		for _, r := range []struct {
			method, path string
			body         any
		}{
			{http.MethodGet, walURL(mine, "/", w.ID), nil},
			{http.MethodPatch, walURL(mine, "/", w.ID), map[string]any{"name": "Stolen"}},
			{http.MethodPut, walURL(mine, "/", w.ID, "/balance"), map[string]any{"newBalance": "1"}},
			{http.MethodDelete, walURL(mine, "/", w.ID) + "?force=true", nil},
		} {
			res := s.Do(r.method, r.path, r.body, bearer(outsider.AccessToken))
			require.Equal(t, http.StatusNotFound, res.Status, "%s %s: %s", r.method, r.path, res.Body)
			assert.Equal(t, "WALLET_NOT_FOUND", errCode(t, res))
		}
		assert.Equal(t, "Shared", s.walletList(owner, sid).Items[0].Name)
	})

	t.Run("members can manage wallets", func(t *testing.T) {
		m := s.newWallet(member, sid, wal("Member wallet"))
		assert.Len(t, s.walletList(owner, sid).Items, 2)
		assert.Equal(t, http.StatusOK, s.Get(walURL(sid, "/", m.ID), bearer(owner.AccessToken)).Status)
	})
}

func TestWalletCardCatalogs(t *testing.T) {
	s := NewTestServer(t)
	u := s.register("jane@example.com")

	assert.Equal(t, http.StatusUnauthorized, s.Get("/api/v1/wallet-card-presets").Status)
	assert.Equal(t, http.StatusUnauthorized, s.Get("/api/v1/wallet-card-templates").Status)

	res := s.Get("/api/v1/wallet-card-presets", bearer(u.AccessToken))
	require.Equal(t, http.StatusOK, res.Status)
	var presets []struct {
		ID, Name, Direction string
		Colors              []string
	}
	res.JSON(t, &presets)
	require.NotEmpty(t, presets)
	assert.Equal(t, "Wallet color", presets[0].Name)
	names := map[string]bool{}
	for _, p := range presets {
		names[p.Name] = true
		assert.Len(t, p.Colors, 2)
		assert.NotEmpty(t, p.Direction)
	}
	for _, n := range []string{"Midnight", "Ocean", "Aurora", "Sunset"} {
		assert.True(t, names[n], n)
	}

	res = s.Get("/api/v1/wallet-card-templates", bearer(u.AccessToken))
	require.Equal(t, http.StatusOK, res.Status)
	var templates []struct{ ID, Name string }
	res.JSON(t, &templates)
	assert.NotEmpty(t, templates)
}
