//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type posBody struct {
	Quantity    string `json:"quantity"`
	AvgBuyPrice string `json:"avgBuyPrice"`
	Fees        string `json:"fees"`
}

type tradeRowBody struct {
	ID             string  `json:"id"`
	Action         string  `json:"action"`
	OccurredAt     string  `json:"occurredAt"`
	Quantity       string  `json:"quantity"`
	UnitPrice      string  `json:"unitPrice"`
	Fee            string  `json:"fee"`
	Amount         string  `json:"amount"`
	Currency       string  `json:"currency"`
	WalletID       *string `json:"walletId"`
	WalletName     *string `json:"walletName"`
	RealizedPL     *string `json:"realizedPl"`
	PositionBefore posBody `json:"positionBefore"`
	PositionAfter  posBody `json:"positionAfter"`
	CanDelete      bool    `json:"canDelete"`
	BlockedReason  *string `json:"blockedReason"`
	BlockedBy      *string `json:"blockedByTradeId"`
}

type tradeListBody struct {
	Items           []tradeRowBody `json:"items"`
	Next            *string        `json:"next"`
	OpeningPosition struct {
		Quantity    string `json:"quantity"`
		AvgBuyPrice string `json:"avgBuyPrice"`
	} `json:"openingPosition"`
}

func (e *tradeEnv) trades(u session, inv, query string) (Response, tradeListBody) {
	e.s.t.Helper()
	res := e.s.Get(invURL(e.sid, "/", inv, "/trades")+query, bearer(u.AccessToken))
	var out tradeListBody
	if res.Status == http.StatusOK {
		res.JSON(e.s.t, &out)
	}
	return res, out
}

func (e *tradeEnv) mustTrades(inv, query string) tradeListBody {
	e.s.t.Helper()
	res, out := e.trades(e.u, inv, query)
	require.Equal(e.s.t, http.StatusOK, res.Status, string(res.Body))
	return out
}

func TestInvestmentTradeList(t *testing.T) {
	s := NewTestServer(t)
	e := newTradeEnv(t, s)

	t.Run("an investment without trades shows its whole position as the opening one", func(t *testing.T) {
		held := e.inv.must(map[string]any{"assetType": "CRYPTO", "name": "Held", "symbol": "HLD", "quantity": "2", "avgBuyPrice": "100", "manualPrice": "100", "priceMode": "MANUAL"})
		l := e.mustTrades(held.ID, "")
		assert.Empty(t, l.Items)
		assert.Nil(t, l.Next)
		assert.True(t, dec("2").Equal(dec(l.OpeningPosition.Quantity)))
		assert.True(t, dec("100").Equal(dec(l.OpeningPosition.AvgBuyPrice)))
	})

	t.Run("a funded create starts at zero, trades list newest first and page by cursor", func(t *testing.T) {
		fresh := e.inv.must(map[string]any{"assetType": "CRYPTO", "name": "Funded", "symbol": "FND", "quantity": "1", "avgBuyPrice": "100", "manualPrice": "100",
			"priceMode": "MANUAL", "funding": map[string]any{"walletId": e.a.ID}})
		b := e.mustTrade(e.buy(e.a, fresh.ID, "1", "120", "1", "occurredAt", "2030-01-01T10:00:00Z"))
		sl := e.mustTrade(e.sell(e.a, fresh.ID, "0.5", "150", "1", "occurredAt", "2030-01-02T10:00:00Z"))
		l := e.mustTrades(fresh.ID, "")
		require.Len(t, l.Items, 3)
		assert.Equal(t, sl.ID, l.Items[0].ID, "newest first by occurredAt")
		assert.Equal(t, b.ID, l.Items[1].ID)
		assert.Equal(t, "BUY", l.Items[1].Action)
		assert.Equal(t, "SELL", l.Items[0].Action)
		assert.True(t, dec("0").Equal(dec(l.OpeningPosition.Quantity)), "the funding BUY is the first trade")
		require.NotNil(t, l.Items[0].RealizedPL)
		assert.Equal(t, "Wallet A", *l.Items[0].WalletName)
		assert.Equal(t, "USD", l.Items[0].Currency)
		assert.True(t, dec("2").Equal(dec(l.Items[0].PositionBefore.Quantity)))
		assert.True(t, dec("1.5").Equal(dec(l.Items[0].PositionAfter.Quantity)))
		assert.True(t, l.Items[0].CanDelete)
		assert.Nil(t, l.Items[0].BlockedReason)

		p1 := e.mustTrades(fresh.ID, "?limit=2")
		require.Len(t, p1.Items, 2)
		require.NotNil(t, p1.Next)
		p2 := e.mustTrades(fresh.ID, "?limit=2&cursor="+url.QueryEscape(*p1.Next))
		require.Len(t, p2.Items, 1)
		assert.Nil(t, p2.Next)
		assert.NotEqual(t, p1.Items[1].ID, p2.Items[0].ID)
	})

	t.Run("validation and not found", func(t *testing.T) {
		for _, q := range []string{"?limit=0", "?limit=101", "?limit=x", "?cursor=nope"} {
			res, _ := e.trades(e.u, e.btc.ID, q)
			assert.Equal(t, http.StatusBadRequest, res.Status, q)
		}
		res, _ := e.trades(e.u, "00000000-0000-0000-0000-000000000001", "")
		assert.Equal(t, http.StatusNotFound, res.Status)
		assert.Equal(t, http.StatusUnauthorized, s.Get(invURL(e.sid, "/", e.btc.ID, "/trades")).Status)
	})

	t.Run("another user's space cannot read it", func(t *testing.T) {
		outsider := s.register("mallory@example.com")
		expectCrossSpace404(t, s, outsider, http.MethodGet, invURL(e.sid, "/", e.btc.ID, "/trades"), nil)
	})
}

func TestTransactionsFilterByInvestment(t *testing.T) {
	s := NewTestServer(t)
	e := newTradeEnv(t, s)
	other := e.inv.must(map[string]any{"assetType": "CRYPTO", "name": "Other", "symbol": "OTH", "quantity": "0", "manualPrice": "1", "priceMode": "MANUAL"})
	e.mustTrade(e.buy(e.a, e.btc.ID, "1", "100", ""))
	e.mustTrade(e.buy(e.a, e.btc.ID, "1", "100", ""))
	e.mustTrade(e.buy(e.a, other.ID, "1", "1", ""))
	assert.Len(t, e.list("?investmentIds="+e.btc.ID).Items, 2)
	assert.Len(t, e.list("?investmentIds="+other.ID).Items, 1)
	assert.Len(t, e.list("?investmentIds="+e.btc.ID+","+other.ID).Items, 3)
	assert.Equal(t, http.StatusBadRequest, s.Get(txURL(e.sid)+"?investmentIds=nope", bearer(e.u.AccessToken)).Status)
}

// A fresh investment built from the remaining trades must equal the one rebuilt by the delete: quantity, average, fees,
// every later SELL's realised result, the wallet balance and the realised total.
func TestDeleteAnOlderTradeReplaysTheLaterOnes(t *testing.T) {
	s := NewTestServer(t)
	e := newTradeEnv(t, s)
	// investment A: b1, b2, s1, b3; we delete b2. investment B is built from b1, s1, b3 in a second wallet.
	a := e.btc
	b := e.inv.must(map[string]any{"assetType": "CRYPTO", "name": "Twin", "symbol": "TWN", "quantity": "0", "avgBuyPrice": "0", "manualPrice": "70000", "priceMode": "MANUAL"})
	wb := e.b
	startA, startB := e.balance(e.a), e.balance(wb)

	e.mustTrade(e.buy(e.a, a.ID, "1", "100", "2"))
	b2 := e.mustTrade(e.buy(e.a, a.ID, "1", "200", "2"))
	e.mustTrade(e.sell(e.a, a.ID, "1", "300", "1"))
	e.mustTrade(e.buy(e.a, a.ID, "2", "250", "0"))
	before := e.mustTrades(a.ID, "")
	require.Len(t, before.Items, 4)
	for _, it := range before.Items {
		assert.True(t, it.CanDelete, "every trade of an intact history can be deleted: %v %v", it.BlockedReason, it.PositionBefore)
	}

	e.mustTrade(e.buy(wb, b.ID, "1", "100", "2"))
	e.mustTrade(e.sell(wb, b.ID, "1", "300", "1"))
	e.mustTrade(e.buy(wb, b.ID, "2", "250", "0"))

	require.Equal(t, http.StatusNoContent, e.deleteTrade(b2.ID).Status)

	ga, gb := e.inv.get(a.ID), e.inv.get(b.ID)
	assert.True(t, dec(gb.Quantity).Equal(dec(ga.Quantity)), "quantity %s vs %s", ga.Quantity, gb.Quantity)
	assert.True(t, dec(gb.AvgBuyPrice).Equal(dec(ga.AvgBuyPrice)), "average")
	assert.True(t, dec(gb.Fees).Equal(dec(ga.Fees)), "fees")
	assert.True(t, dec(gb.RealizedPL.Amount).Equal(dec(ga.RealizedPL.Amount)), "realised total: %s vs %s", ga.RealizedPL.Amount, gb.RealizedPL.Amount)
	assert.True(t, dec(gb.Valuation.MarketValue).Equal(dec(ga.Valuation.MarketValue)), "valuation")
	assert.True(t, startA.Sub(e.balance(e.a)).Equal(startB.Sub(e.balance(wb))), "the wallet only keeps the remaining trades")

	la, lb := e.mustTrades(a.ID, ""), e.mustTrades(b.ID, "")
	require.Len(t, la.Items, 3)
	require.Len(t, lb.Items, 3)
	for i := range la.Items {
		assert.True(t, dec(lb.Items[i].PositionAfter.Quantity).Equal(dec(la.Items[i].PositionAfter.Quantity)), "row %d after quantity", i)
		assert.True(t, dec(lb.Items[i].PositionAfter.AvgBuyPrice).Equal(dec(la.Items[i].PositionAfter.AvgBuyPrice)), "row %d after avg", i)
		assert.True(t, dec(lb.Items[i].PositionBefore.Fees).Equal(dec(la.Items[i].PositionBefore.Fees)), "row %d before fees", i)
		if lb.Items[i].RealizedPL != nil {
			require.NotNil(t, la.Items[i].RealizedPL)
			assert.True(t, dec(*lb.Items[i].RealizedPL).Equal(dec(*la.Items[i].RealizedPL)), "row %d realised", i)
		}
	}
	for _, id := range []string{e.a.ID, wb.ID} {
		expectLedgerConsistent(t, s, id)
	}

	t.Run("deleting it again is 404 and reverses nothing twice", func(t *testing.T) {
		bal := e.balance(e.a)
		assert.Equal(t, http.StatusNotFound, e.deleteTrade(b2.ID).Status)
		assert.True(t, bal.Equal(e.balance(e.a)))
	})
}

func TestDeleteThatBreaksALaterSellIsRefused(t *testing.T) {
	s := NewTestServer(t)
	e := newTradeEnv(t, s)
	buy := e.mustTrade(e.buy(e.a, e.btc.ID, "2", "100", ""))
	sell := e.mustTrade(e.sell(e.a, e.btc.ID, "2", "150", ""))
	bal := e.balance(e.a)

	l := e.mustTrades(e.btc.ID, "")
	var row tradeRowBody
	for _, it := range l.Items {
		if it.ID == buy.ID {
			row = it
		}
	}
	assert.False(t, row.CanDelete)
	require.NotNil(t, row.BlockedReason)
	assert.Equal(t, "TRADE_DELETE_BREAKS_LATER_SELL", *row.BlockedReason)
	require.NotNil(t, row.BlockedBy)
	assert.Equal(t, sell.ID, *row.BlockedBy)

	res := e.deleteTrade(buy.ID)
	require.Equal(t, http.StatusUnprocessableEntity, res.Status, string(res.Body))
	assert.Equal(t, "TRADE_DELETE_BREAKS_LATER_SELL", errCode(t, res))
	var er struct {
		Error struct {
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	res.JSON(t, &er)
	assert.Equal(t, sell.ID, er.Error.Details["tradeId"], "names the sale that would go negative")
	e.assertPosition(e.btc.ID, "0", "100", "0")
	assert.True(t, bal.Equal(e.balance(e.a)), "nothing changed")
	assert.Len(t, e.mustTrades(e.btc.ID, "").Items, 2)

	// deleting the sale first works, then the buy
	require.Equal(t, http.StatusNoContent, e.deleteTrade(sell.ID).Status)
	require.Equal(t, http.StatusNoContent, e.deleteTrade(buy.ID).Status)
	e.assertPosition(e.btc.ID, "0", "0", "0")
}

func TestDeleteTradeEdgeCases(t *testing.T) {
	s := NewTestServer(t)
	e := newTradeEnv(t, s)

	t.Run("the funding BUY can be deleted: the wallet is refunded and the position drops to its opening state", func(t *testing.T) {
		start := e.balance(e.a)
		inv := e.inv.must(map[string]any{"assetType": "CRYPTO", "name": "Funded", "symbol": "FUN", "quantity": "2", "avgBuyPrice": "10", "manualPrice": "10",
			"priceMode": "MANUAL", "funding": map[string]any{"walletId": e.a.ID}})
		assert.True(t, start.Sub(dec("20")).Equal(e.balance(e.a)))
		l := e.mustTrades(inv.ID, "")
		require.Len(t, l.Items, 1)
		assert.True(t, dec("0").Equal(dec(l.Items[0].PositionBefore.Quantity)), "position_before of the funding BUY is zero")
		require.Equal(t, http.StatusNoContent, e.deleteTrade(l.Items[0].ID).Status)
		e.assertPosition(inv.ID, "0", "0", "0")
		assert.True(t, start.Equal(e.balance(e.a)))
		assert.Empty(t, e.mustTrades(inv.ID, "").Items)
		expectLedgerConsistent(t, s, e.a.ID)
	})

	t.Run("a position edited by hand blocks the delete with 409 and canDelete says so", func(t *testing.T) {
		inv := e.inv.must(map[string]any{"assetType": "CRYPTO", "name": "Edited", "symbol": "EDT", "quantity": "0", "manualPrice": "1", "priceMode": "MANUAL"})
		b := e.mustTrade(e.buy(e.a, inv.ID, "1", "100", ""))
		e.mustTrade(e.buy(e.a, inv.ID, "1", "100", ""))
		require.Equal(t, http.StatusOK, e.inv.patch(inv.ID, map[string]any{"quantity": "5"}).Status)
		res := e.deleteTrade(b.ID)
		require.Equal(t, http.StatusConflict, res.Status)
		assert.Equal(t, "POSITION_CHANGED_SINCE", errCode(t, res))
		for _, it := range e.mustTrades(inv.ID, "").Items {
			assert.False(t, it.CanDelete)
			require.NotNil(t, it.BlockedReason)
			assert.Equal(t, "POSITION_CHANGED_SINCE", *it.BlockedReason)
		}
	})

	t.Run("a trade whose wallet was deleted can still be deleted", func(t *testing.T) {
		w := s.newWallet(e.u, e.sid, map[string]any{"type": "BANK", "name": "Gone", "icon": "x", "currency": "USD", "initialBalance": "1000"})
		inv := e.inv.must(map[string]any{"assetType": "CRYPTO", "name": "Orphan", "symbol": "ORP", "quantity": "0", "manualPrice": "1", "priceMode": "MANUAL"})
		b := e.mustTrade(e.buy(w, inv.ID, "1", "100", ""))
		require.Equal(t, http.StatusNoContent, s.Do(http.MethodDelete, walURL(e.sid, "/", w.ID)+"?force=true", nil, bearer(e.u.AccessToken)).Status)
		res := e.deleteTrade(b.ID)
		require.Equal(t, http.StatusNoContent, res.Status, string(res.Body))
		e.assertPosition(inv.ID, "0", "0", "0")
	})

	t.Run("an ID stock keeps its lots in step with the replayed quantity", func(t *testing.T) {
		inv := e.inv.must(map[string]any{"assetType": "ID_STOCK", "name": "BCA", "symbol": "BBCA", "avgBuyPrice": "9000", "manualPrice": "9500", "details": map[string]any{"lots": 1}})
		idr := e.idr
		b := e.mustTrade(e.buy(idr, inv.ID, "100", "9000", ""))
		e.mustTrade(e.buy(idr, inv.ID, "100", "9500", ""))
		require.Equal(t, http.StatusNoContent, e.deleteTrade(b.ID).Status)
		got := e.inv.get(inv.ID)
		assert.True(t, dec("200").Equal(dec(got.Quantity)), got.Quantity)
		assert.EqualValues(t, 2, got.Details["lots"])
	})

	t.Run("the realised total stays equal to the history", func(t *testing.T) {
		inv := e.inv.must(map[string]any{"assetType": "CRYPTO", "name": "Real", "symbol": "RLZ", "quantity": "0", "manualPrice": "1", "priceMode": "MANUAL"})
		b1 := e.mustTrade(e.buy(e.a, inv.ID, "2", "100", ""))
		e.mustTrade(e.buy(e.a, inv.ID, "2", "200", ""))
		e.mustTrade(e.sell(e.a, inv.ID, "1", "300", "")) // avg 150: realised 150
		require.Equal(t, http.StatusNoContent, e.deleteTrade(b1.ID).Status)
		// avg is now 200: the sale realised (300-200)*1
		l := e.mustTrades(inv.ID, "")
		require.NotNil(t, l.Items[0].RealizedPL)
		assert.True(t, dec("100").Equal(dec(*l.Items[0].RealizedPL)), *l.Items[0].RealizedPL)
		assert.True(t, dec("100").Equal(dec(e.inv.get(inv.ID).RealizedPL.Amount)))
	})
}

func TestTradeHistoryForASharedInViewer(t *testing.T) {
	s := NewTestServer(t)
	e := newShareEnv(t, s)
	bob := s.register("bob@example.com")
	out := e.mustPut(owner("50"), emailP("bob@example.com", "Bob", "50"))
	require.Equal(t, http.StatusOK, s.acceptShare(bob, byName(out.Items, "Bob").ID, spaceIDOf(bob)).Status)

	w := s.newWallet(e.owner, e.sid, wal("Cash"))
	res := s.Do(http.MethodPost, trURL(e.sid), map[string]any{"from": walletEnd(w), "to": leg(e.inv.ID, "1", "600", "")}, bearer(e.owner.AccessToken))
	require.Equal(t, http.StatusCreated, res.Status, string(res.Body))
	var buy tradeBody
	res.JSON(t, &buy)

	t.Run("the viewer reads the history but cannot delete, and the wallet is hidden", func(t *testing.T) {
		res := s.Get(invURL(spaceIDOf(bob), "/", e.inv.ID, "/trades"), bearer(bob.AccessToken))
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		var l tradeListBody
		res.JSON(t, &l)
		require.Len(t, l.Items, 1)
		assert.False(t, l.Items[0].CanDelete)
		require.NotNil(t, l.Items[0].BlockedReason)
		assert.Equal(t, "OWNER_ONLY", *l.Items[0].BlockedReason)
		assert.Nil(t, l.Items[0].WalletID)
		assert.Nil(t, l.Items[0].WalletName)

		// Bob is not a member of the owner's space: the delete does not even reach the trade
		del := s.Do(http.MethodDelete, trURL(e.sid, "/", buy.ID), nil, bearer(bob.AccessToken))
		assert.Equal(t, http.StatusNotFound, del.Status, string(del.Body))
	})

	t.Run("the owner deletes it and the shared position follows", func(t *testing.T) {
		res := s.Get(invURL(e.sid, "/", e.inv.ID, "/trades"), bearer(e.owner.AccessToken))
		require.Equal(t, http.StatusOK, res.Status)
		var l tradeListBody
		res.JSON(t, &l)
		assert.True(t, l.Items[0].CanDelete)
		require.NotNil(t, l.Items[0].WalletName)
		require.Equal(t, http.StatusNoContent, s.Do(http.MethodDelete, trURL(e.sid, "/", buy.ID), nil, bearer(e.owner.AccessToken)).Status)
		assert.Equal(t, "1", e.invEnv().get(e.inv.ID).Quantity)
	})
	_ = context.Background
}
