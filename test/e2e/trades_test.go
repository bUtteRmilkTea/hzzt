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

type tradeBody struct {
	ID               string  `json:"id"`
	Type             string  `json:"type"`
	Title            *string `json:"title"`
	Amount           string  `json:"amount"`
	Currency         string  `json:"currency"`
	Wallet           *ref    `json:"wallet"`
	ToAmount         *string `json:"toAmount"`
	ToCurrency       *string `json:"toCurrency"`
	FxRate           *string `json:"fxRate"`
	InvestmentAction *string `json:"investmentAction"`
	Investment       *struct {
		ID     string  `json:"id"`
		Name   string  `json:"name"`
		Symbol *string `json:"symbol"`
		Icon   string  `json:"icon"`
	} `json:"investment"`
	Quantity   *string `json:"quantity"`
	UnitPrice  *string `json:"unitPrice"`
	Fee        *string `json:"fee"`
	RealizedPL *string `json:"realizedPl"`
	OccurredAt string  `json:"occurredAt"`
	Note       *string `json:"note"`
}

type tradeEnv struct {
	*world
	idr walletBody
	btc investmentBody // a manual BTC position, quantity 0 to start with
	inv *invEnv
}

func newTradeEnv(t *testing.T, s *TestServer) *tradeEnv {
	t.Helper()
	w := newWorld(t, s) // Wallet A = 1000 USD, Wallet B = 500 USD
	// plenty of cash for the trades
	require.Equal(t, http.StatusOK, s.Do(http.MethodPut, walURL(w.sid, "/", w.a.ID, "/balance"), map[string]any{"newBalance": "100000"}, bearer(w.u.AccessToken)).Status)
	b := wal("Rupiah")
	b["currency"], b["initialBalance"] = "IDR", "2000000000"
	idr := s.newWallet(w.u, w.sid, b)
	require.NoError(t, s.Currency.Refresh(context.Background())) // 1 USD = 16000 IDR
	e := &invEnv{s: s, u: w.u, sid: w.sid}
	btc := e.must(map[string]any{"assetType": "CRYPTO", "name": "Bitcoin", "symbol": "BTC", "quantity": "0",
		"avgBuyPrice": "0", "manualPrice": "70000", "priceMode": "MANUAL"})
	return &tradeEnv{world: w, idr: idr, btc: btc, inv: e}
}

func leg(inv string, qty, price, fee string) map[string]any {
	m := map[string]any{"type": "INVESTMENT", "investmentId": inv, "quantity": qty, "unitPrice": price}
	if fee != "" {
		m["fee"] = fee
	}
	return m
}

func (e *tradeEnv) buy(wallet walletBody, inv string, qty, price, fee string, extra ...any) Response {
	body := clone(map[string]any{"from": walletEnd(wallet), "to": leg(inv, qty, price, fee)}, extra...)
	return e.s.Do(http.MethodPost, trURL(e.sid), body, bearer(e.u.AccessToken))
}

func (e *tradeEnv) sell(wallet walletBody, inv string, qty, price, fee string, extra ...any) Response {
	body := clone(map[string]any{"from": leg(inv, qty, price, fee), "to": walletEnd(wallet)}, extra...)
	return e.s.Do(http.MethodPost, trURL(e.sid), body, bearer(e.u.AccessToken))
}

func (e *tradeEnv) mustTrade(res Response) tradeBody {
	e.s.t.Helper()
	require.Equal(e.s.t, http.StatusCreated, res.Status, string(res.Body))
	var b tradeBody
	res.JSON(e.s.t, &b)
	return b
}

func (e *tradeEnv) patchTrade(id string, body map[string]any) Response {
	return e.s.Do(http.MethodPatch, trURL(e.sid, "/", id), body, bearer(e.u.AccessToken))
}

func (e *tradeEnv) deleteTrade(id string) Response {
	return e.s.Do(http.MethodDelete, trURL(e.sid, "/", id), nil, bearer(e.u.AccessToken))
}

func (e *tradeEnv) position(inv string) (qty, avg, fees string) {
	e.s.t.Helper()
	got := e.inv.get(inv)
	return got.Quantity, got.AvgBuyPrice, got.Fees
}

func (e *tradeEnv) assertPosition(inv, qty, avg, fees string) {
	e.s.t.Helper()
	q, a, f := e.position(inv)
	assert.True(e.s.t, dec(qty).Equal(dec(q)), "quantity: want %s got %s", qty, q)
	assert.True(e.s.t, dec(avg).Equal(dec(a)), "avg: want %s got %s", avg, a)
	assert.True(e.s.t, dec(fees).Equal(dec(f)), "fees: want %s got %s", fees, f)
}

func TestBuyAndSellAcceptance(t *testing.T) {
	s := NewTestServer(t)
	e := newTradeEnv(t, s)
	require.True(t, dec("100000").Equal(e.balance(e.a)))

	// buying 0.5 BTC at 60,000 and then 0.5 at 70,000 gives avg 65,000 and quantity 1
	first := e.mustTrade(e.buy(e.a, e.btc.ID, "0.5", "60000", ""))
	assert.Equal(t, "TRANSFER", first.Type, "a trade is a transfer: never income or expense")
	require.NotNil(t, first.InvestmentAction)
	assert.Equal(t, "BUY", *first.InvestmentAction)
	assert.True(t, dec("30000").Equal(dec(first.Amount)), "wallet side")
	assert.Equal(t, "USD", first.Currency)
	assert.True(t, dec("30000").Equal(dec(*first.ToAmount)), "investment side, in BTC's quote currency")
	assert.Equal(t, "USD", *first.ToCurrency)
	assert.True(t, dec("1").Equal(dec(*first.FxRate)))
	require.NotNil(t, first.Investment)
	assert.Equal(t, "Bitcoin", first.Investment.Name)
	assert.Equal(t, "BTC", *first.Investment.Symbol)
	assert.True(t, dec("0.5").Equal(dec(*first.Quantity)))
	assert.True(t, dec("60000").Equal(dec(*first.UnitPrice)))
	assert.Nil(t, first.RealizedPL)
	assert.True(t, dec("70000").Equal(e.balance(e.a)))
	e.assertPosition(e.btc.ID, "0.5", "60000", "0")

	second := e.mustTrade(e.buy(e.a, e.btc.ID, "0.5", "70000", ""))
	e.assertPosition(e.btc.ID, "1", "65000", "0")
	assert.True(t, dec("35000").Equal(dec(second.Amount)))
	assert.True(t, dec("35000").Equal(e.balance(e.a)))
	expectLedgerConsistent(t, s, e.a.ID)

	// the stored snapshots chain: the second BUY started where the first one ended
	var before, after string
	require.NoError(t, s.Tx.QueryRow(context.Background(),
		`SELECT position_before::text, position_after::text FROM transactions WHERE id = $1`, second.ID).Scan(&before, &after))
	assert.JSONEq(t, `{"quantity":"0.5","avgBuyPrice":"60000","fees":"0"}`, before)
	assert.JSONEq(t, `{"quantity":"1","avgBuyPrice":"65000","fees":"0"}`, after)

	// selling 0.4 at 80,000 with fee 10 records realizedPl 5,990
	sale := e.mustTrade(e.sell(e.a, e.btc.ID, "0.4", "80000", "10"))
	require.NotNil(t, sale.InvestmentAction)
	assert.Equal(t, "SELL", *sale.InvestmentAction)
	require.NotNil(t, sale.RealizedPL)
	assert.True(t, dec("5990").Equal(dec(*sale.RealizedPL)), *sale.RealizedPL)
	assert.True(t, dec("31990").Equal(dec(sale.Amount)), "0.4 × 80000 − 10 credited to the wallet")
	assert.True(t, dec("66990").Equal(e.balance(e.a)))
	e.assertPosition(e.btc.ID, "0.6", "65000", "0")
	assert.Equal(t, "Wallet A", sale.Wallet.Name)
	expectLedgerConsistent(t, s, e.a.ID)

	t.Run("realized P/L shows on the investment", func(t *testing.T) {
		got := e.inv.get(e.btc.ID)
		assert.True(t, dec("5990").Equal(dec(got.RealizedPL.Amount)))
		assert.Equal(t, "USD", got.RealizedPL.Currency)
		require.NotNil(t, got.RealizedPL.Base)
		assert.True(t, dec("5990").Equal(dec(*got.RealizedPL.Base)), "converted to the USD base")
		assert.Equal(t, 1, got.RealizedPL.Sells)
		l := e.inv.list("")
		for _, it := range l.Items {
			if it.ID == e.btc.ID {
				assert.True(t, dec("5990").Equal(dec(it.RealizedPL.Amount)), "also in the list")
			}
		}
	})

	t.Run("deleting that SELL restores quantity 1", func(t *testing.T) {
		require.Equal(t, http.StatusNoContent, e.deleteTrade(sale.ID).Status)
		e.assertPosition(e.btc.ID, "1", "65000", "0")
		assert.True(t, dec("35000").Equal(e.balance(e.a)), "the proceeds are taken back")
		assert.Zero(t, e.inv.get(e.btc.ID).RealizedPL.Sells)
		assert.Equal(t, http.StatusNotFound, s.Get(txURL(e.sid, "/", sale.ID), bearer(e.u.AccessToken)).Status)
		expectLedgerConsistent(t, s, e.a.ID)
	})

	t.Run("editing an older BUY after a later SELL is refused with 409", func(t *testing.T) {
		later := e.mustTrade(e.sell(e.a, e.btc.ID, "0.1", "90000", ""))
		for name, res := range map[string]Response{
			"edit older BUY":   e.patchTrade(first.ID, map[string]any{"to": map[string]any{"type": "INVESTMENT", "quantity": "0.6"}}),
			"delete older BUY": e.deleteTrade(first.ID),
			"edit the middle":  e.patchTrade(second.ID, map[string]any{"note": "x"}),
		} {
			require.Equal(t, http.StatusConflict, res.Status, "%s: %s", name, res.Body)
			assert.Equal(t, "POSITION_CHANGED_SINCE", errCode(t, res), name)
		}
		e.assertPosition(e.btc.ID, "0.9", "65000", "0")
		assert.True(t, dec("35000").Add(dec("9000")).Equal(e.balance(e.a)), "a refused edit changes nothing")

		// the latest operation is still editable, and once it is undone the older ones are again
		require.Equal(t, http.StatusNoContent, e.deleteTrade(later.ID).Status)
		res := e.patchTrade(second.ID, map[string]any{"note": "now allowed"})
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
	})
}

func TestTradesStayOutOfIncomeAndExpense(t *testing.T) {
	s := NewTestServer(t)
	e := newTradeEnv(t, s)
	at := "2026-11-10T10:00:00Z"
	e.mustTrade(e.buy(e.a, e.btc.ID, "1", "60000", "", "occurredAt", at))
	e.mustTrade(e.sell(e.a, e.btc.ID, "0.5", "70000", "", "occurredAt", at))
	e.must(map[string]any{"type": "EXPENSE", "walletId": e.a.ID, "amount": "100", "occurredAt": at})

	sm := e.summary("?from=2026-11-10&to=2026-11-10&types=INCOME,EXPENSE,TRANSFER")
	require.Len(t, sm.Items, 1)
	d := sm.Items[0]
	assert.True(t, dec("100").Equal(dec(d.Expense)), "only the real expense: %s", d.Expense)
	assert.True(t, dec("0").Equal(dec(d.Income)), "a SELL is not income: %s", d.Income)
	assert.True(t, dec("95000").Equal(dec(d.Transfer)), "BUY 60000 + SELL 35000 in the transfer column: %s", d.Transfer)

	l := e.list("?types=TRANSFER&walletIds=" + e.a.ID)
	require.Len(t, l.Items, 2, "trades appear in the wallet's history")

	t.Run("search finds trades by investment name or symbol", func(t *testing.T) {
		assert.Len(t, e.list("?q=bitcoin&types=TRANSFER").Items, 2)
		assert.Len(t, e.list("?q=btc&types=TRANSFER").Items, 2)
	})

	t.Run("the generic endpoints point to the transfer endpoints", func(t *testing.T) {
		id := l.Items[0].ID
		res := s.Do(http.MethodPatch, txURL(e.sid, "/", id), map[string]any{"note": "x"}, bearer(e.u.AccessToken))
		assert.Equal(t, "USE_SPECIFIC_ENDPOINT", errCode(t, res))
		res = s.Do(http.MethodDelete, txURL(e.sid, "/", id), nil, bearer(e.u.AccessToken))
		assert.Equal(t, "USE_SPECIFIC_ENDPOINT", errCode(t, res))
	})
}

func TestSellRules(t *testing.T) {
	s := NewTestServer(t)
	e := newTradeEnv(t, s)
	e.mustTrade(e.buy(e.a, e.btc.ID, "2", "50000", "20"))
	e.assertPosition(e.btc.ID, "2", "50000", "20")

	t.Run("cannot sell more than the quantity", func(t *testing.T) {
		res := e.sell(e.a, e.btc.ID, "2.00000001", "1", "")
		require.Equal(t, http.StatusUnprocessableEntity, res.Status, string(res.Body))
		assert.Equal(t, "INSUFFICIENT_QUANTITY", errCode(t, res))
		var er struct {
			Error struct {
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		res.JSON(t, &er)
		assert.Equal(t, "2", er.Error.Details["available"])
		e.assertPosition(e.btc.ID, "2", "50000", "20")
	})

	t.Run("staked crypto cannot be sold", func(t *testing.T) {
		eth := e.inv.must(map[string]any{"assetType": "CRYPTO", "name": "Ethereum", "symbol": "ETH", "quantity": "10",
			"avgBuyPrice": "2000", "manualPrice": "2500", "details": map[string]any{"stakedQuantity": "4"}})
		res := e.sell(e.a, eth.ID, "6.5", "2500", "")
		require.Equal(t, http.StatusUnprocessableEntity, res.Status)
		assert.Equal(t, "INSUFFICIENT_QUANTITY", errCode(t, res))
		got := e.mustTrade(e.sell(e.a, eth.ID, "6", "2500", ""))
		assert.True(t, dec("3000").Equal(dec(*got.RealizedPL)), "(2500−2000)×6: %s", *got.RealizedPL)
	})

	t.Run("selling everything is allowed and keeps the average and fees", func(t *testing.T) {
		e.mustTrade(e.sell(e.a, e.btc.ID, "2", "55000", "5"))
		e.assertPosition(e.btc.ID, "0", "50000", "20")
		res := e.sell(e.a, e.btc.ID, "0.1", "1", "")
		require.Equal(t, http.StatusUnprocessableEntity, res.Status)
		assert.Equal(t, "INSUFFICIENT_QUANTITY", errCode(t, res))
	})

	t.Run("the fee cannot swallow the proceeds", func(t *testing.T) {
		e.mustTrade(e.buy(e.a, e.btc.ID, "1", "100", ""))
		res := e.sell(e.a, e.btc.ID, "1", "100", "100")
		assert.Equal(t, http.StatusBadRequest, res.Status, string(res.Body))
		res = e.sell(e.a, e.btc.ID, "1", "100", "150")
		assert.Equal(t, http.StatusBadRequest, res.Status)
	})
}

func TestTradeValidationAndReferences(t *testing.T) {
	s := NewTestServer(t)
	e := newTradeEnv(t, s)

	t.Run("unauthenticated", func(t *testing.T) {
		assert.Equal(t, http.StatusUnauthorized, s.Do(http.MethodPost, trURL(e.sid), map[string]any{
			"from": walletEnd(e.a), "to": leg(e.btc.ID, "1", "1", "")}).Status)
	})

	t.Run("numbers", func(t *testing.T) {
		for name, res := range map[string]Response{
			"zero quantity":       e.buy(e.a, e.btc.ID, "0", "100", ""),
			"negative quantity":   e.buy(e.a, e.btc.ID, "-1", "100", ""),
			"zero price":          e.buy(e.a, e.btc.ID, "1", "0", ""),
			"negative fee":        e.buy(e.a, e.btc.ID, "1", "100", "-1"),
			"19 decimal quantity": e.buy(e.a, e.btc.ID, "0.1234567890123456789", "100", ""),
			"9 decimal price":     e.buy(e.a, e.btc.ID, "1", "100.123456789", ""),
			"huge":                e.buy(e.a, e.btc.ID, "100000000000000000000", "1", ""),
			"zero amount":         e.buy(e.a, e.btc.ID, "1", "100", "", "amount", "0"),
			"toAmount is unused":  e.buy(e.a, e.btc.ID, "1", "100", "", "toAmount", "5"),
			"bad date":            e.buy(e.a, e.btc.ID, "1", "100", "", "occurredAt", "soon"),
			"long note":           e.buy(e.a, e.btc.ID, "1", "100", "", "note", strings.Repeat("n", 1001)),
		} {
			assert.Equal(t, http.StatusBadRequest, res.Status, "%s: %s", name, res.Body)
		}
		assert.True(t, dec("100000").Equal(e.balance(e.a)))
		e.assertPosition(e.btc.ID, "0", "0", "0")
	})

	t.Run("wallet and investment must exist in this space", func(t *testing.T) {
		res := e.buy(e.a, uuid.NewString(), "1", "1", "")
		require.Equal(t, http.StatusNotFound, res.Status)
		assert.Equal(t, "INVESTMENT_NOT_FOUND", errCode(t, res))
		res = e.sell(e.a, uuid.NewString(), "1", "1", "")
		assert.Equal(t, "INVESTMENT_NOT_FOUND", errCode(t, res))
		res = e.buy(walletBody{ID: uuid.NewString()}, e.btc.ID, "1", "1", "")
		assert.Equal(t, "WALLET_NOT_FOUND", errCode(t, res))

		other := s.register("other@example.com")
		ow := s.newWallet(other, spaceIDOf(other), wal("Theirs"))
		assert.Equal(t, "WALLET_NOT_FOUND", errCode(t, e.buy(ow, e.btc.ID, "1", "1", "")))
		oi := (&invEnv{s: s, u: other, sid: spaceIDOf(other)}).must(clone(aapl, "name", "Theirs"))
		assert.Equal(t, "INVESTMENT_NOT_FOUND", errCode(t, e.buy(e.a, oi.ID, "1", "1", "")))

		gone := e.inv.must(clone(aapl, "name", "Soon gone"))
		require.Equal(t, http.StatusNoContent, s.Do(http.MethodDelete, invURL(e.sid, "/", gone.ID), nil, bearer(e.u.AccessToken)).Status)
		assert.Equal(t, "INVESTMENT_NOT_FOUND", errCode(t, e.buy(e.a, gone.ID, "1", "1", "")))
		del := s.newWallet(e.u, e.sid, wal("Deleted"))
		require.Equal(t, http.StatusNoContent, s.Do(http.MethodDelete, walURL(e.sid, "/", del.ID), nil, bearer(e.u.AccessToken)).Status)
		assert.Equal(t, "WALLET_NOT_FOUND", errCode(t, e.buy(del, e.btc.ID, "1", "1", "")))
		e.assertPosition(e.btc.ID, "0", "0", "0")
	})

	t.Run("investment to investment is not supported", func(t *testing.T) {
		res := s.Do(http.MethodPost, trURL(e.sid), map[string]any{
			"from": leg(e.btc.ID, "1", "1", ""), "to": leg(e.btc.ID, "1", "1", ""), "amount": "1"}, bearer(e.u.AccessToken))
		require.Equal(t, http.StatusUnprocessableEntity, res.Status)
		assert.Equal(t, "NOT_SUPPORTED_YET", errCode(t, res))
	})

	t.Run("transfer endpoints refuse ids that are not transfers or trades", func(t *testing.T) {
		exp := e.must(exp(e.a, "5", ""))
		assert.Equal(t, "TRANSFER_NOT_FOUND", errCode(t, e.patchTrade(exp.ID, map[string]any{"note": "x"})))
		assert.Equal(t, "TRANSFER_NOT_FOUND", errCode(t, e.deleteTrade(exp.ID)))
	})
}

func TestTradesAreSpaceScoped(t *testing.T) {
	s := NewTestServer(t)
	e := newTradeEnv(t, s)
	outsider := s.register("outsider@example.com")
	tr := e.mustTrade(e.buy(e.a, e.btc.ID, "1", "100", ""))

	expectCrossSpace404(t, s, outsider, http.MethodPost, trURL(e.sid), map[string]any{"from": walletEnd(e.a), "to": leg(e.btc.ID, "1", "1", "")})
	expectCrossSpace404(t, s, outsider, http.MethodPatch, trURL(e.sid, "/", tr.ID), map[string]any{"note": "x"})
	expectCrossSpace404(t, s, outsider, http.MethodDelete, trURL(e.sid, "/", tr.ID), nil)

	mine := spaceIDOf(outsider)
	for _, c := range []struct {
		method string
		body   any
	}{{http.MethodPatch, map[string]any{"note": "x"}}, {http.MethodDelete, nil}} {
		res := s.Do(c.method, trURL(mine, "/", tr.ID), c.body, bearer(outsider.AccessToken))
		require.Equal(t, http.StatusNotFound, res.Status)
		assert.Equal(t, "TRANSFER_NOT_FOUND", errCode(t, res))
	}
	e.assertPosition(e.btc.ID, "1", "100", "0")
}

func TestEditTrade(t *testing.T) {
	s := NewTestServer(t)
	e := newTradeEnv(t, s)

	t.Run("edit a BUY: quantity, price, fee and date; the position is recomputed from where it started", func(t *testing.T) {
		e.inv.patch(e.btc.ID, map[string]any{"quantity": "1", "avgBuyPrice": "50000"}) // an existing position
		buy := e.mustTrade(e.buy(e.a, e.btc.ID, "1", "70000", "10"))                   // → qty 2, avg 60000, fees 10
		e.assertPosition(e.btc.ID, "2", "60000", "10")
		assert.True(t, dec("29990").Equal(e.balance(e.a)), "100000 − 70010")

		res := e.patchTrade(buy.ID, map[string]any{
			"to":         map[string]any{"type": "INVESTMENT", "quantity": "2", "unitPrice": "65000", "fee": "20"},
			"occurredAt": "2026-03-04T05:06:07Z", "note": "bought more",
		})
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		var got tradeBody
		res.JSON(t, &got)
		assert.True(t, dec("130020").Equal(dec(got.Amount)), "2 × 65000 + 20")
		assert.Equal(t, "2026-03-04T05:06:07Z", got.OccurredAt)
		require.NotNil(t, got.Note)
		assert.Equal(t, "bought more", *got.Note)
		// (1×50000 + 2×65000)/3 = 60000; fees 20
		e.assertPosition(e.btc.ID, "3", "60000", "20")
		assert.True(t, dec("100000").Sub(dec("130020")).Equal(e.balance(e.a)))
		expectLedgerConsistent(t, s, e.a.ID)

		// the date moved but the position chain still matches, so it stays editable
		require.Equal(t, http.StatusOK, e.patchTrade(buy.ID, map[string]any{"note": nil}).Status)
		require.Equal(t, http.StatusNoContent, e.deleteTrade(buy.ID).Status)
		e.assertPosition(e.btc.ID, "1", "50000", "0")
		assert.True(t, dec("100000").Equal(e.balance(e.a)))
	})

	t.Run("move a BUY to another wallet", func(t *testing.T) {
		buy := e.mustTrade(e.buy(e.a, e.btc.ID, "1", "1000", ""))
		before := e.balance(e.a)
		res := e.patchTrade(buy.ID, map[string]any{"from": walletEnd(e.b)})
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		assert.True(t, before.Add(dec("1000")).Equal(e.balance(e.a)), "wallet A gets its money back")
		assert.True(t, dec("500").Sub(dec("1000")).Equal(e.balance(e.b)), "wallet B pays")
		expectLedgerConsistent(t, s, e.a.ID)
		expectLedgerConsistent(t, s, e.b.ID)
		require.Equal(t, http.StatusNoContent, e.deleteTrade(buy.ID).Status)
	})

	t.Run("edit a SELL: quantity re-checked, realized P/L recomputed", func(t *testing.T) {
		sale := e.mustTrade(e.sell(e.a, e.btc.ID, "0.5", "60000", "")) // avg 50000 → 5000
		assert.True(t, dec("5000").Equal(dec(*sale.RealizedPL)))
		res := e.patchTrade(sale.ID, map[string]any{"from": map[string]any{"type": "INVESTMENT", "quantity": "0.25", "unitPrice": "70000", "fee": "5"}})
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		var got tradeBody
		res.JSON(t, &got)
		assert.True(t, dec("4995").Equal(dec(*got.RealizedPL)), "(70000−50000)×0.25 − 5: %s", *got.RealizedPL)
		e.assertPosition(e.btc.ID, "0.75", "50000", "0")

		// selling more than what is there at that point is refused and nothing changes
		before := e.balance(e.a)
		res = e.patchTrade(sale.ID, map[string]any{"from": map[string]any{"type": "INVESTMENT", "quantity": "1.5"}})
		require.Equal(t, http.StatusUnprocessableEntity, res.Status, string(res.Body))
		assert.Equal(t, "INSUFFICIENT_QUANTITY", errCode(t, res))
		assert.True(t, before.Equal(e.balance(e.a)))
		e.assertPosition(e.btc.ID, "0.75", "50000", "0")
		require.Equal(t, http.StatusNoContent, e.deleteTrade(sale.ID).Status)
		e.assertPosition(e.btc.ID, "1", "50000", "0")
	})

	t.Run("edit validation", func(t *testing.T) {
		buy := e.mustTrade(e.buy(e.a, e.btc.ID, "1", "100", ""))
		for name, body := range map[string]map[string]any{
			"zero quantity":    {"to": map[string]any{"type": "INVESTMENT", "quantity": "0"}},
			"negative fee":     {"to": map[string]any{"type": "INVESTMENT", "fee": "-1"}},
			"zero price":       {"to": map[string]any{"type": "INVESTMENT", "unitPrice": "0"}},
			"zero amount":      {"amount": "0"},
			"toAmount":         {"toAmount": "5"},
			"other investment": {"to": map[string]any{"type": "INVESTMENT", "investmentId": uuid.NewString()}},
			"bad leg":          {"to": map[string]any{"type": "BANK"}},
			"unknown field":    {"direction": "IN"},
		} {
			res := e.patchTrade(buy.ID, body)
			assert.Equal(t, http.StatusBadRequest, res.Status, "%s: %s", name, res.Body)
		}
		assert.Equal(t, "WALLET_NOT_FOUND", errCode(t, e.patchTrade(buy.ID, map[string]any{"from": walletEnd(walletBody{ID: uuid.NewString()})})))
		e.assertPosition(e.btc.ID, "2", "25050", "0")
	})
}

func TestManualPositionEditBlocksTradeEdits(t *testing.T) {
	s := NewTestServer(t)
	e := newTradeEnv(t, s)
	buy := e.mustTrade(e.buy(e.a, e.btc.ID, "1", "100", ""))
	// the user corrects the quantity by hand: the trade's snapshot no longer matches
	require.Equal(t, http.StatusOK, e.inv.patch(e.btc.ID, map[string]any{"quantity": "5"}).Status)
	for _, res := range []Response{e.patchTrade(buy.ID, map[string]any{"note": "x"}), e.deleteTrade(buy.ID)} {
		require.Equal(t, http.StatusConflict, res.Status)
		assert.Equal(t, "POSITION_CHANGED_SINCE", errCode(t, res))
	}
}

func TestPriceChangesDoNotBlockTradeEdits(t *testing.T) {
	s := NewTestServer(t)
	e := newTradeEnv(t, s)
	buy := e.mustTrade(e.buy(e.a, e.btc.ID, "1", "100", ""))
	require.Equal(t, http.StatusOK, e.inv.patch(e.btc.ID, map[string]any{"manualPrice": "99999", "name": "Renamed"}).Status)
	require.Equal(t, http.StatusOK, e.patchTrade(buy.ID, map[string]any{"note": "still fine"}).Status)
	require.Equal(t, http.StatusNoContent, e.deleteTrade(buy.ID).Status)
	e.assertPosition(e.btc.ID, "0", "0", "0")
}

func TestDeletingAnInvestmentKeepsItsTradesDeletable(t *testing.T) {
	s := NewTestServer(t)
	e := newTradeEnv(t, s)
	buy := e.mustTrade(e.buy(e.a, e.btc.ID, "1", "1000", ""))
	require.True(t, dec("99000").Equal(e.balance(e.a)))
	require.Equal(t, http.StatusNoContent, s.Do(http.MethodDelete, invURL(e.sid, "/", e.btc.ID), nil, bearer(e.u.AccessToken)).Status)

	assert.Equal(t, http.StatusNotFound, e.patchTrade(buy.ID, map[string]any{"note": "x"}).Status, "editing needs the investment")
	require.Equal(t, http.StatusNoContent, e.deleteTrade(buy.ID).Status, "deleting only gives the money back")
	assert.True(t, dec("100000").Equal(e.balance(e.a)))
}

func TestCrossCurrencyTrades(t *testing.T) {
	s := NewTestServer(t)
	e := newTradeEnv(t, s) // IDR wallet funds a USD investment; 1 USD = 16000 IDR

	t.Run("the wallet is converted with the space's FX context and the rate is stored", func(t *testing.T) {
		got := e.mustTrade(e.buy(e.idr, e.btc.ID, "0.5", "60000", "10"))
		assert.True(t, dec("480160000").Equal(dec(got.Amount)), "30010 USD × 16000 = %s", got.Amount)
		assert.Equal(t, "IDR", got.Currency)
		assert.True(t, dec("30010").Equal(dec(*got.ToAmount)))
		assert.Equal(t, "USD", *got.ToCurrency)
		assert.True(t, dec("16000").Equal(dec(*got.FxRate)))
		assert.True(t, dec("2000000000").Sub(dec("480160000")).Equal(e.balance(e.idr)))
		expectLedgerConsistent(t, s, e.idr.ID)
	})

	t.Run("an explicit wallet amount wins and defines the rate", func(t *testing.T) {
		got := e.mustTrade(e.buy(e.idr, e.btc.ID, "0.1", "60000", "", "amount", "96600000"))
		assert.True(t, dec("96600000").Equal(dec(got.Amount)))
		assert.True(t, dec("16100").Equal(dec(*got.FxRate)), "96,600,000 / 6000")
	})

	t.Run("a SELL credits the wallet in its own currency", func(t *testing.T) {
		got := e.mustTrade(e.sell(e.idr, e.btc.ID, "0.2", "70000", ""))
		assert.True(t, dec("224000000").Equal(dec(got.Amount)), "14000 USD × 16000")
		assert.True(t, dec("14000").Equal(dec(*got.ToAmount)))
		require.NotNil(t, got.RealizedPL)
		assert.True(t, dec("2000").Equal(dec(*got.RealizedPL)), "(70000−60000)×0.2, in USD not IDR: %s", *got.RealizedPL)
		expectLedgerConsistent(t, s, e.idr.ID)
	})

	t.Run("editing keeps the agreed rate", func(t *testing.T) {
		got := e.mustTrade(e.buy(e.idr, e.btc.ID, "1", "1000", "", "amount", "17000000")) // rate 17000
		res := e.patchTrade(got.ID, map[string]any{"to": map[string]any{"type": "INVESTMENT", "quantity": "2"}})
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		var upd tradeBody
		res.JSON(t, &upd)
		assert.True(t, dec("34000000").Equal(dec(upd.Amount)), "2 × 1000 USD × the agreed 17000: %s", upd.Amount)
		assert.True(t, dec("17000").Equal(dec(*upd.FxRate)))
	})

	t.Run("no rate and no amount: 422", func(t *testing.T) {
		sg := wal("Singapore")
		sg["currency"] = "SGD"
		sgd := s.newWallet(e.u, e.sid, sg)
		res := e.buy(sgd, e.btc.ID, "1", "100", "")
		require.Equal(t, http.StatusUnprocessableEntity, res.Status, string(res.Body))
		assert.Equal(t, "FX_RATE_UNAVAILABLE", errCode(t, res))
		// the wallet amount can always be given by hand
		assert.Equal(t, http.StatusCreated, e.buy(sgd, e.btc.ID, "1", "100", "", "amount", "135").Status)
	})
}

func TestIDStockTradesMoveWholeLots(t *testing.T) {
	s := NewTestServer(t)
	e := newTradeEnv(t, s)
	stock := e.inv.must(bbca) // 2 lots = 200 shares, avg 9200, price 9500, fees 5000
	assert.True(t, dec("200").Equal(dec(stock.Quantity)))

	t.Run("a BUY of whole lots updates quantity and lots together", func(t *testing.T) {
		got := e.mustTrade(e.buy(e.idr, stock.ID, "300", "9800", "1000"))
		assert.True(t, dec("2940000").Add(dec("1000")).Equal(dec(got.Amount)), "IDR wallet, same currency: %s", got.Amount)
		now := e.inv.get(stock.ID)
		assert.True(t, dec("500").Equal(dec(now.Quantity)))
		assert.EqualValues(t, 5, now.Details["lots"], "lots follow the quantity")
		// (200×9200 + 300×9800)/500 = 9560
		assert.True(t, dec("9560").Equal(dec(now.AvgBuyPrice)), now.AvgBuyPrice)
		assert.True(t, dec("6000").Equal(dec(now.Fees)))
	})

	t.Run("a SELL of whole lots", func(t *testing.T) {
		got := e.mustTrade(e.sell(e.idr, stock.ID, "200", "10000", ""))
		assert.True(t, dec("2000000").Equal(dec(got.Amount)))
		now := e.inv.get(stock.ID)
		assert.True(t, dec("300").Equal(dec(now.Quantity)))
		assert.EqualValues(t, 3, now.Details["lots"])
		assert.True(t, dec("88000").Equal(dec(*got.RealizedPL)), "(10000−9560)×200")
		require.Equal(t, http.StatusNoContent, e.deleteTrade(got.ID).Status)
		assert.EqualValues(t, 5, e.inv.get(stock.ID).Details["lots"], "deleting restores the lots too")
	})

	t.Run("a tenth of a lot (10 shares) is allowed, a hundredth is not", func(t *testing.T) {
		before := dec(e.inv.get(stock.ID).Quantity)
		got := e.mustTrade(e.buy(e.idr, stock.ID, "50", "9000", ""))
		now := e.inv.get(stock.ID)
		assert.True(t, before.Add(dec("50")).Equal(dec(now.Quantity)), "0.5 lot = 50 shares")
		assert.EqualValues(t, 5.5, now.Details["lots"])
		require.Equal(t, http.StatusNoContent, e.deleteTrade(got.ID).Status)
		assert.EqualValues(t, 5, e.inv.get(stock.ID).Details["lots"])
	})

	t.Run("fractions of a lot beyond one decimal are refused", func(t *testing.T) {
		for _, qty := range []string{"55", "105", "100.5"} {
			res := e.buy(e.idr, stock.ID, qty, "9000", "")
			require.Equal(t, http.StatusBadRequest, res.Status, qty)
		}
		res := e.sell(e.idr, stock.ID, "55", "9000", "")
		assert.Equal(t, http.StatusBadRequest, res.Status)
	})
}

func TestFundingTheInitialPosition(t *testing.T) {
	s := NewTestServer(t)
	e := newTradeEnv(t, s)
	create := func(body map[string]any) Response { return e.inv.post(body) }
	pos := func(extra ...any) map[string]any {
		return clone(map[string]any{"assetType": "CRYPTO", "name": "Bitcoin", "symbol": "BTC", "quantity": "0.5",
			"avgBuyPrice": "60000", "fees": "10", "manualPrice": "65000"}, extra...)
	}

	t.Run("creates a BUY titled 'Initial position BTC' in the same transaction", func(t *testing.T) {
		res := create(pos("funding", map[string]any{"walletId": e.a.ID}))
		require.Equal(t, http.StatusCreated, res.Status, string(res.Body))
		var inv investmentBody
		res.JSON(t, &inv)
		assert.True(t, dec("0.5").Equal(dec(inv.Quantity)))
		assert.True(t, dec("100000").Sub(dec("30010")).Equal(e.balance(e.a)), "0.5 × 60000 + 10 debited")

		l := e.list("?types=TRANSFER&walletIds=" + e.a.ID)
		require.Len(t, l.Items, 1)
		var tr tradeBody
		// fetch it as a trade to read the investment fields
		res = s.Get(txURL(e.sid, "/", l.Items[0].ID), bearer(e.u.AccessToken))
		res.JSON(t, &tr)
		require.NotNil(t, tr.Title)
		assert.Equal(t, "Initial position BTC", *tr.Title)
		assert.Equal(t, "BUY", *tr.InvestmentAction)
		assert.Equal(t, inv.ID, tr.Investment.ID)
		assert.True(t, dec("0.5").Equal(dec(*tr.Quantity)))
		assert.True(t, dec("60000").Equal(dec(*tr.UnitPrice)))
		assert.True(t, dec("10").Equal(dec(*tr.Fee)))

		var before, after string
		require.NoError(t, s.Tx.QueryRow(context.Background(),
			`SELECT position_before::text, position_after::text FROM transactions WHERE id = $1`, tr.ID).Scan(&before, &after))
		assert.JSONEq(t, `{"quantity":"0","avgBuyPrice":"0","fees":"0"}`, before, "the position started from nothing")
		assert.JSONEq(t, `{"quantity":"0.5","avgBuyPrice":"60000","fees":"10"}`, after)
		expectLedgerConsistent(t, s, e.a.ID)

		// it behaves like any BUY afterwards: later trades guard it, and undoing it empties the position
		sale := e.mustTrade(e.sell(e.a, inv.ID, "0.1", "70000", ""))
		assert.Equal(t, "POSITION_CHANGED_SINCE", errCode(t, e.deleteTrade(tr.ID)))
		require.Equal(t, http.StatusNoContent, e.deleteTrade(sale.ID).Status)
		require.Equal(t, http.StatusNoContent, e.deleteTrade(tr.ID).Status)
		e.assertPosition(inv.ID, "0", "0", "0")
		assert.True(t, dec("100000").Equal(e.balance(e.a)))
	})

	t.Run("symbol-less investments use their name", func(t *testing.T) {
		res := create(map[string]any{"assetType": "MUTUAL_FUND", "name": "Pasar Uang", "quantity": "1000", "quoteCurrency": "USD",
			"avgBuyPrice": "1", "manualPrice": "1.1", "details": map[string]any{"fundType": "MONEY_MARKET"},
			"funding": map[string]any{"walletId": e.a.ID}})
		require.Equal(t, http.StatusCreated, res.Status, string(res.Body))
		var title string
		require.NoError(t, s.Tx.QueryRow(context.Background(),
			`SELECT title FROM transactions WHERE title LIKE 'Initial position Pasar%'`).Scan(&title))
		assert.Equal(t, "Initial position Pasar Uang", title)
	})

	t.Run("another currency: converted, or an explicit amount", func(t *testing.T) {
		before := e.balance(e.idr)
		require.Equal(t, http.StatusCreated, create(pos("name", "ETH", "symbol", "ETH", "funding", map[string]any{"walletId": e.idr.ID})).Status)
		assert.True(t, before.Sub(dec("30010").Mul(dec("16000"))).Equal(e.balance(e.idr)))
		before = e.balance(e.idr)
		require.Equal(t, http.StatusCreated, create(pos("name", "SOL", "symbol", "SOL", "funding", map[string]any{"walletId": e.idr.ID, "amount": "500000000"})).Status)
		assert.True(t, before.Sub(dec("500000000")).Equal(e.balance(e.idr)))
		expectLedgerConsistent(t, s, e.idr.ID)
	})

	t.Run("a failed funding leaves no investment behind", func(t *testing.T) {
		count := func() int {
			var n int
			require.NoError(t, s.Tx.QueryRow(context.Background(), `SELECT count(*) FROM investments WHERE symbol = 'DOGE'`).Scan(&n))
			return n
		}
		res := create(pos("name", "DOGE", "symbol", "DOGE", "funding", map[string]any{"walletId": uuid.NewString()}))
		require.Equal(t, http.StatusNotFound, res.Status, string(res.Body))
		assert.Equal(t, "WALLET_NOT_FOUND", errCode(t, res))
		assert.Zero(t, count())

		sg := wal("Singapore")
		sg["currency"] = "SGD"
		sgd := s.newWallet(e.u, e.sid, sg)
		res = create(pos("name", "DOGE", "symbol", "DOGE", "funding", map[string]any{"walletId": sgd.ID}))
		require.Equal(t, http.StatusUnprocessableEntity, res.Status)
		assert.Equal(t, "FX_RATE_UNAVAILABLE", errCode(t, res))
		assert.Zero(t, count())

		res = create(pos("name", "DOGE", "symbol", "DOGE", "avgBuyPrice", "0", "funding", map[string]any{"walletId": e.a.ID}))
		assert.Equal(t, http.StatusBadRequest, res.Status, "nothing to pay for")
		assert.Zero(t, count())
		for _, f := range []map[string]any{{}, {"walletId": "x"}, {"walletId": e.a.ID, "amount": "0"}, {"walletId": e.a.ID, "amount": "-5"}, {"walletId": e.a.ID, "x": 1}} {
			assert.Equal(t, http.StatusBadRequest, create(pos("name", "DOGE", "symbol", "DOGE", "funding", f)).Status, "%v", f)
		}
		assert.Zero(t, count())
	})

	t.Run("without funding nothing is debited", func(t *testing.T) {
		before := e.balance(e.a)
		require.Equal(t, http.StatusCreated, create(pos("name", "ADA", "symbol", "ADA")).Status)
		assert.True(t, before.Equal(e.balance(e.a)))
	})
}
