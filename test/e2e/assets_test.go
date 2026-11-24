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

type assetBody struct {
	ID               string  `json:"id"`
	Kind             string  `json:"kind"`
	Name             string  `json:"name"`
	Icon             string  `json:"icon"`
	Currency         string  `json:"currency"`
	CurrentValue     string  `json:"currentValue"`
	CounterpartyName *string `json:"counterpartyName"`
	DueDate          *string `json:"dueDate"`
	Note             *string `json:"note"`
	History          []struct {
		ID            string  `json:"id"`
		CanRevert     bool    `json:"canRevert"`
		PreviousValue string  `json:"previousValue"`
		NewValue      string  `json:"newValue"`
		Reason        string  `json:"reason"`
		TransactionID *string `json:"transactionId"`
	} `json:"history"`
}

type assetList struct {
	Items  []assetBody `json:"items"`
	Totals map[string]struct {
		Total    string                                  `json:"total"`
		Currency string                                  `json:"currency"`
		Warnings []struct{ Code, Message, Label string } `json:"warnings"`
	} `json:"totals"`
}

func assetURL(sid string, rest ...string) string {
	return "/api/v1/spaces/" + sid + "/manual-assets" + strings.Join(rest, "")
}

type assetEnv struct {
	*world
	idr walletBody
}

func newAssetEnv(t *testing.T, s *TestServer) *assetEnv {
	t.Helper()
	w := newWorld(t, s) // Wallet A = 1000 USD, Wallet B = 500 USD
	b := wal("Rupiah")
	b["currency"], b["initialBalance"] = "IDR", "10000000"
	require.NoError(t, s.Currency.Refresh(context.Background())) // 1 USD = 16000 IDR = 0.8 EUR
	return &assetEnv{world: w, idr: s.newWallet(w.u, w.sid, b)}
}

func (e *assetEnv) post(body map[string]any) Response {
	return e.s.Do(http.MethodPost, assetURL(e.sid), body, bearer(e.u.AccessToken))
}

func (e *assetEnv) must(body map[string]any) assetBody {
	e.s.t.Helper()
	res := e.post(body)
	require.Equal(e.s.t, http.StatusCreated, res.Status, string(res.Body))
	var a assetBody
	res.JSON(e.s.t, &a)
	return a
}

func (e *assetEnv) get(id string) assetBody {
	e.s.t.Helper()
	res := e.s.Get(assetURL(e.sid, "/", id), bearer(e.u.AccessToken))
	require.Equal(e.s.t, http.StatusOK, res.Status, string(res.Body))
	var a assetBody
	res.JSON(e.s.t, &a)
	return a
}

func (e *assetEnv) patch(id string, body map[string]any) Response {
	return e.s.Do(http.MethodPatch, assetURL(e.sid, "/", id), body, bearer(e.u.AccessToken))
}

func (e *assetEnv) settle(id string, body map[string]any) Response {
	return e.s.Do(http.MethodPost, assetURL(e.sid, "/", id, "/settlements"), body, bearer(e.u.AccessToken))
}

type settleBody struct {
	Transaction struct {
		ID          string  `json:"id"`
		Type        string  `json:"type"`
		Title       *string `json:"title"`
		Amount      string  `json:"amount"`
		Currency    string  `json:"currency"`
		Wallet      *ref    `json:"wallet"`
		ManualAsset *struct {
			ID, Name, Icon string
		} `json:"manualAsset"`
		OccurredAt string `json:"occurredAt"`
	} `json:"transaction"`
	Asset assetBody `json:"asset"`
}

func debt(name, value string) map[string]any {
	return map[string]any{"kind": "LIABILITY", "name": name, "currentValue": value}
}

func TestManualAssetCRUD(t *testing.T) {
	s := NewTestServer(t)
	e := newAssetEnv(t, s)

	t.Run("unauthenticated", func(t *testing.T) {
		assert.Equal(t, http.StatusUnauthorized, s.Get(assetURL(e.sid)).Status)
		assert.Equal(t, http.StatusUnauthorized, s.Do(http.MethodPost, assetURL(e.sid), debt("x", "1")).Status)
	})

	t.Run("create each kind with defaults", func(t *testing.T) {
		car := e.must(map[string]any{"kind": "FIXED_ASSET", "name": "  Car  ", "currentValue": "160000000", "currency": "idr",
			"note": "Toyota", "dueDate": "2030-01-31"})
		assert.Equal(t, "Car", car.Name)
		assert.Equal(t, "IDR", car.Currency)
		assert.Equal(t, "🏠", car.Icon, "a default icon per kind")
		assert.Equal(t, "2030-01-31", *car.DueDate)
		require.Len(t, car.History, 1, "the first value is part of the history")
		assert.Equal(t, "0", car.History[0].PreviousValue)

		owed := e.must(map[string]any{"kind": "RECEIVABLE", "name": "Loan to Sam", "currentValue": "100", "counterpartyName": " Sam ", "icon": "💸"})
		assert.Equal(t, "USD", owed.Currency, "defaults to the space base currency")
		assert.Equal(t, "Sam", *owed.CounterpartyName)
		assert.Equal(t, "💸", owed.Icon)
		d := e.must(debt("Credit card", "500"))
		assert.Equal(t, "💳", d.Icon)
	})

	t.Run("validation", func(t *testing.T) {
		mk := func(mod func(m map[string]any)) map[string]any { m := debt("Valid", "10"); mod(m); return m }
		for name, body := range map[string]map[string]any{
			"bad kind":          mk(func(m map[string]any) { m["kind"] = "GIFT" }),
			"missing name":      mk(func(m map[string]any) { delete(m, "name") }),
			"blank name":        mk(func(m map[string]any) { m["name"] = " " }),
			"long name":         mk(func(m map[string]any) { m["name"] = strings.Repeat("x", 61) }),
			"missing value":     mk(func(m map[string]any) { delete(m, "currentValue") }),
			"negative value":    mk(func(m map[string]any) { m["currentValue"] = "-1" }),
			"9 decimals":        mk(func(m map[string]any) { m["currentValue"] = "1.123456789" }),
			"huge":              mk(func(m map[string]any) { m["currentValue"] = "100000000000000000000" }),
			"bad currency":      mk(func(m map[string]any) { m["currency"] = "XXX" }),
			"bad date":          mk(func(m map[string]any) { m["dueDate"] = "soon" }),
			"long note":         mk(func(m map[string]any) { m["note"] = strings.Repeat("n", 1001) }),
			"long counterparty": mk(func(m map[string]any) { m["counterpartyName"] = strings.Repeat("c", 61) }),
			"unknown field":     mk(func(m map[string]any) { m["settled"] = true }),
		} {
			res := e.post(body)
			assert.Equal(t, http.StatusBadRequest, res.Status, "%s: %s", name, res.Body)
		}
		// a zero value is allowed (a fully paid debt)
		assert.Equal(t, http.StatusCreated, e.post(debt("Paid off", "0")).Status)
	})

	t.Run("list filters by kind and totals each kind in the base currency", func(t *testing.T) {
		res := s.Get(assetURL(e.sid), bearer(e.u.AccessToken))
		var all assetList
		res.JSON(t, &all)
		assert.Len(t, all.Items, 4)
		assert.True(t, dec("10000").Equal(dec(all.Totals["FIXED_ASSET"].Total)), "160,000,000 IDR = 10,000 USD: %s", all.Totals["FIXED_ASSET"].Total)
		assert.True(t, dec("100").Equal(dec(all.Totals["RECEIVABLE"].Total)))
		assert.True(t, dec("500").Equal(dec(all.Totals["LIABILITY"].Total)))
		assert.Equal(t, "USD", all.Totals["LIABILITY"].Currency)

		res = s.Get(assetURL(e.sid)+"?kind=liability", bearer(e.u.AccessToken))
		var only assetList
		res.JSON(t, &only)
		require.Len(t, only.Items, 2)
		assert.Len(t, only.Totals, 1)
		assert.Equal(t, http.StatusBadRequest, s.Get(assetURL(e.sid)+"?kind=gift", bearer(e.u.AccessToken)).Status)
	})

	t.Run("a currency without a rate is left out of its total with a warning", func(t *testing.T) {
		e.must(map[string]any{"kind": "RECEIVABLE", "name": "Singapore loan", "currentValue": "50", "currency": "SGD"})
		var l assetList
		s.Get(assetURL(e.sid)+"?kind=RECEIVABLE", bearer(e.u.AccessToken)).JSON(t, &l)
		assert.True(t, dec("100").Equal(dec(l.Totals["RECEIVABLE"].Total)), "the SGD 50 is excluded")
		require.Len(t, l.Totals["RECEIVABLE"].Warnings, 1)
		assert.Equal(t, "FX_RATE_UNAVAILABLE", l.Totals["RECEIVABLE"].Warnings[0].Code)
	})
}

func TestManualAssetUpdateAndHistory(t *testing.T) {
	s := NewTestServer(t)
	e := newAssetEnv(t, s)
	house := e.must(map[string]any{"kind": "FIXED_ASSET", "name": "House", "currentValue": "300000", "counterpartyName": "Bank", "dueDate": "2040-01-01", "note": "mortgaged"})

	t.Run("every value change is recorded", func(t *testing.T) {
		res := e.patch(house.ID, map[string]any{"currentValue": "310000.5", "name": "Family house"})
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		var got assetBody
		res.JSON(t, &got)
		assert.Equal(t, "Family house", got.Name)
		assert.True(t, dec("310000.5").Equal(dec(got.CurrentValue)))
		require.Len(t, got.History, 2)
		assert.True(t, dec("300000").Equal(dec(got.History[0].PreviousValue)), "newest first")
		assert.True(t, dec("310000.5").Equal(dec(got.History[0].NewValue)))
		assert.Equal(t, "MANUAL", got.History[0].Reason)

		// a patch that leaves the value alone adds no history
		require.Equal(t, http.StatusOK, e.patch(house.ID, map[string]any{"name": "Home", "currentValue": "310000.50"}).Status)
		assert.Len(t, e.get(house.ID).History, 2)
	})

	t.Run("null clears the optional fields", func(t *testing.T) {
		res := e.patch(house.ID, map[string]any{"counterpartyName": nil, "dueDate": nil, "note": nil})
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		var got assetBody
		res.JSON(t, &got)
		assert.Nil(t, got.CounterpartyName)
		assert.Nil(t, got.DueDate)
		assert.Nil(t, got.Note)
		assert.Equal(t, "Home", got.Name, "everything else stays")
	})

	t.Run("validation and not found", func(t *testing.T) {
		for name, body := range map[string]map[string]any{
			"blank name": {"name": ""}, "negative": {"currentValue": "-5"}, "bad currency": {"currency": "ZZZ"},
			"bad date": {"dueDate": "x"}, "kind is fixed": {"kind": "LIABILITY"}, "long note": {"note": strings.Repeat("n", 1001)},
		} {
			assert.Equal(t, http.StatusBadRequest, e.patch(house.ID, body).Status, name)
		}
		for _, id := range []string{uuid.NewString(), "nope"} {
			res := e.patch(id, map[string]any{"name": "x"})
			require.Equal(t, http.StatusNotFound, res.Status)
			assert.Equal(t, "MANUAL_ASSET_NOT_FOUND", errCode(t, res))
			assert.Equal(t, http.StatusNotFound, s.Get(assetURL(e.sid, "/", id), bearer(e.u.AccessToken)).Status)
			assert.Equal(t, http.StatusNotFound, s.Do(http.MethodDelete, assetURL(e.sid, "/", id), nil, bearer(e.u.AccessToken)).Status)
		}
	})

	t.Run("currency changes until the first settlement", func(t *testing.T) {
		owed := e.must(map[string]any{"kind": "RECEIVABLE", "name": "Loan", "currentValue": "100"})
		require.Equal(t, http.StatusOK, e.patch(owed.ID, map[string]any{"currency": "EUR"}).Status)
		require.Equal(t, http.StatusOK, e.patch(owed.ID, map[string]any{"currency": "usd"}).Status)
		require.Equal(t, http.StatusCreated, e.settle(owed.ID, map[string]any{"walletId": e.a.ID, "amount": "10"}).Status)
		res := e.patch(owed.ID, map[string]any{"currency": "EUR"})
		require.Equal(t, http.StatusUnprocessableEntity, res.Status)
		assert.Equal(t, "MANUAL_ASSET_CURRENCY_LOCKED", errCode(t, res))
		assert.Equal(t, http.StatusOK, e.patch(owed.ID, map[string]any{"currency": "usd", "name": "Loan 2"}).Status, "the same currency is fine")
	})

	t.Run("delete is soft", func(t *testing.T) {
		require.Equal(t, http.StatusNoContent, s.Do(http.MethodDelete, assetURL(e.sid, "/", house.ID), nil, bearer(e.u.AccessToken)).Status)
		assert.Equal(t, http.StatusNotFound, s.Get(assetURL(e.sid, "/", house.ID), bearer(e.u.AccessToken)).Status)
		assert.Equal(t, http.StatusNotFound, s.Do(http.MethodDelete, assetURL(e.sid, "/", house.ID), nil, bearer(e.u.AccessToken)).Status)
		var deleted *string
		require.NoError(t, s.Tx.QueryRow(context.Background(), `SELECT deleted_at::text FROM manual_assets WHERE id = $1`, house.ID).Scan(&deleted))
		assert.NotNil(t, deleted)
	})
}

func TestSettlements(t *testing.T) {
	s := NewTestServer(t)
	e := newAssetEnv(t, s)

	t.Run("acceptance: a debt of 500 with a 200 payment → outstanding 300 and wallet −200", func(t *testing.T) {
		card := e.must(debt("Credit card", "500"))
		before := e.balance(e.a)
		res := e.settle(card.ID, map[string]any{"walletId": e.a.ID, "amount": "200", "occurredAt": "2026-11-10T08:00:00Z", "note": "november"})
		require.Equal(t, http.StatusCreated, res.Status, string(res.Body))
		var got settleBody
		res.JSON(t, &got)

		assert.True(t, dec("300").Equal(dec(got.Asset.CurrentValue)), "outstanding")
		assert.True(t, before.Sub(dec("200")).Equal(e.balance(e.a)), "wallet −200")
		assert.Equal(t, "TRANSFER", got.Transaction.Type, "a settlement is a transfer, not an expense")
		assert.True(t, dec("200").Equal(dec(got.Transaction.Amount)))
		assert.Equal(t, "USD", got.Transaction.Currency)
		require.NotNil(t, got.Transaction.Title)
		assert.Equal(t, "Payment of Credit card", *got.Transaction.Title)
		require.NotNil(t, got.Transaction.ManualAsset)
		assert.Equal(t, card.ID, got.Transaction.ManualAsset.ID)
		assert.Equal(t, "Wallet A", got.Transaction.Wallet.Name)
		assert.Equal(t, "2026-11-10T08:00:00Z", got.Transaction.OccurredAt)

		require.Len(t, got.Asset.History, 2)
		assert.Equal(t, "SETTLEMENT", got.Asset.History[0].Reason)
		assert.True(t, dec("500").Equal(dec(got.Asset.History[0].PreviousValue)))
		assert.True(t, dec("300").Equal(dec(got.Asset.History[0].NewValue)))
		require.NotNil(t, got.Asset.History[0].TransactionID)
		assert.Equal(t, got.Transaction.ID, *got.Asset.History[0].TransactionID)
		expectLedgerConsistent(t, s, e.a.ID)
	})

	t.Run("a receivable: the wallet is credited and the amount owed goes down", func(t *testing.T) {
		loan := e.must(map[string]any{"kind": "RECEIVABLE", "name": "Loan to Sam", "currentValue": "100", "counterpartyName": "Sam"})
		before := e.balance(e.b)
		res := e.settle(loan.ID, map[string]any{"walletId": e.b.ID, "amount": "40"})
		require.Equal(t, http.StatusCreated, res.Status, string(res.Body))
		var got settleBody
		res.JSON(t, &got)
		assert.True(t, dec("60").Equal(dec(got.Asset.CurrentValue)))
		assert.True(t, before.Add(dec("40")).Equal(e.balance(e.b)))
		assert.Equal(t, "Repayment from Sam", *got.Transaction.Title)
		expectLedgerConsistent(t, s, e.b.ID)
	})

	t.Run("settling the whole balance leaves zero", func(t *testing.T) {
		d := e.must(debt("Small debt", "75.25"))
		res := e.settle(d.ID, map[string]any{"walletId": e.a.ID, "amount": "75.25"})
		require.Equal(t, http.StatusCreated, res.Status, string(res.Body))
		assert.True(t, dec("0").Equal(dec(e.get(d.ID).CurrentValue)))
		res = e.settle(d.ID, map[string]any{"walletId": e.a.ID, "amount": "0.01"})
		require.Equal(t, http.StatusUnprocessableEntity, res.Status, "nothing left to settle")
		assert.Equal(t, "SETTLEMENT_EXCEEDS_BALANCE", errCode(t, res))
	})

	t.Run("the amount cannot exceed the outstanding balance", func(t *testing.T) {
		d := e.must(debt("Limited", "100"))
		before := e.balance(e.a)
		res := e.settle(d.ID, map[string]any{"walletId": e.a.ID, "amount": "100.01"})
		require.Equal(t, http.StatusUnprocessableEntity, res.Status, string(res.Body))
		assert.Equal(t, "SETTLEMENT_EXCEEDS_BALANCE", errCode(t, res))
		var er struct {
			Error struct {
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		res.JSON(t, &er)
		assert.Equal(t, "100", er.Error.Details["outstanding"])
		assert.True(t, before.Equal(e.balance(e.a)), "nothing changed")
		assert.True(t, dec("100").Equal(dec(e.get(d.ID).CurrentValue)))
	})

	t.Run("same currency only", func(t *testing.T) {
		d := e.must(debt("Dollar debt", "100"))
		res := e.settle(d.ID, map[string]any{"walletId": e.idr.ID, "amount": "10"})
		require.Equal(t, http.StatusUnprocessableEntity, res.Status, string(res.Body))
		assert.Equal(t, "SETTLEMENT_CURRENCY_MISMATCH", errCode(t, res))
		rp := e.must(map[string]any{"kind": "LIABILITY", "name": "Rupiah debt", "currentValue": "1000000", "currency": "IDR"})
		assert.Equal(t, http.StatusCreated, e.settle(rp.ID, map[string]any{"walletId": e.idr.ID, "amount": "250000"}).Status)
		assert.True(t, dec("9750000").Equal(e.balance(e.idr)))
	})

	t.Run("fixed assets cannot be settled", func(t *testing.T) {
		car := e.must(map[string]any{"kind": "FIXED_ASSET", "name": "Bike", "currentValue": "100"})
		res := e.settle(car.ID, map[string]any{"walletId": e.a.ID, "amount": "10"})
		require.Equal(t, http.StatusUnprocessableEntity, res.Status)
		assert.Equal(t, "SETTLEMENT_NOT_SUPPORTED", errCode(t, res))
	})

	t.Run("validation and references", func(t *testing.T) {
		d := e.must(debt("Checks", "100"))
		for name, body := range map[string]map[string]any{
			"zero": {"walletId": e.a.ID, "amount": "0"}, "negative": {"walletId": e.a.ID, "amount": "-5"},
			"decimals": {"walletId": e.a.ID, "amount": "1.123456789"}, "missing amount": {"walletId": e.a.ID},
			"missing wallet": {"amount": "5"}, "bad wallet": {"walletId": "x", "amount": "5"}, "bad date": {"walletId": e.a.ID, "amount": "5", "occurredAt": "soon"},
			"unknown field": {"walletId": e.a.ID, "amount": "5", "direction": "IN"},
		} {
			assert.Equal(t, http.StatusBadRequest, e.settle(d.ID, body).Status, name)
		}
		res := e.settle(d.ID, map[string]any{"walletId": uuid.NewString(), "amount": "5"})
		require.Equal(t, http.StatusNotFound, res.Status)
		assert.Equal(t, "WALLET_NOT_FOUND", errCode(t, res))
		other := s.register("other@example.com")
		ow := s.newWallet(other, spaceIDOf(other), wal("Theirs"))
		assert.Equal(t, "WALLET_NOT_FOUND", errCode(t, e.settle(d.ID, map[string]any{"walletId": ow.ID, "amount": "5"})))
		assert.Equal(t, "MANUAL_ASSET_NOT_FOUND", errCode(t, e.settle(uuid.NewString(), map[string]any{"walletId": e.a.ID, "amount": "5"})))
		assert.True(t, dec("100").Equal(dec(e.get(d.ID).CurrentValue)))
	})

	t.Run("settlements show up as transfers in the history and search", func(t *testing.T) {
		l := e.list("?types=TRANSFER&q=credit")
		require.Len(t, l.Items, 1)
		assert.Equal(t, "Payment of Credit card", *l.Items[0].Title)
		assert.Len(t, e.list("?q=sam&types=TRANSFER").Items, 1, "by counterparty name")
		sm := e.summary("?types=EXPENSE,TRANSFER")
		var transfer, expense = dec("0"), dec("0")
		for _, d := range sm.Items {
			transfer, expense = transfer.Add(dec(d.Transfer)), expense.Add(dec(d.Expense))
		}
		assert.True(t, expense.IsZero(), "a debt payment is not an expense")
		assert.True(t, transfer.GreaterThan(dec("0")))
	})
}

func TestDeletingASettlementReversesBoth(t *testing.T) {
	s := NewTestServer(t)
	e := newAssetEnv(t, s)
	card := e.must(debt("Card", "500"))
	loan := e.must(map[string]any{"kind": "RECEIVABLE", "name": "Loan", "currentValue": "100"})
	pay := func(id string, wallet walletBody, amount string) settleBody {
		res := e.settle(id, map[string]any{"walletId": wallet.ID, "amount": amount})
		require.Equal(t, http.StatusCreated, res.Status, string(res.Body))
		var b settleBody
		res.JSON(t, &b)
		return b
	}

	t.Run("through the asset's own endpoint", func(t *testing.T) {
		before := e.balance(e.a)
		p := pay(card.ID, e.a, "200")
		res := s.Do(http.MethodDelete, assetURL(e.sid, "/", card.ID, "/settlements/", p.Transaction.ID), nil, bearer(e.u.AccessToken))
		require.Equal(t, http.StatusNoContent, res.Status, string(res.Body))
		assert.True(t, dec("500").Equal(dec(e.get(card.ID).CurrentValue)), "the outstanding amount is back")
		assert.True(t, before.Equal(e.balance(e.a)))
		assert.Equal(t, http.StatusNotFound, s.Get(txURL(e.sid, "/", p.Transaction.ID), bearer(e.u.AccessToken)).Status)
		h := e.get(card.ID).History
		require.GreaterOrEqual(t, len(h), 3)
		assert.Equal(t, "SETTLEMENT", h[0].Reason, "the reversal is recorded")
		assert.True(t, dec("500").Equal(dec(h[0].NewValue)))
		expectLedgerConsistent(t, s, e.a.ID)
		assert.Equal(t, http.StatusNotFound, s.Do(http.MethodDelete, assetURL(e.sid, "/", card.ID, "/settlements/", p.Transaction.ID), nil, bearer(e.u.AccessToken)).Status, "a second time")
	})

	t.Run("through the transfer endpoint", func(t *testing.T) {
		before := e.balance(e.b)
		p := pay(loan.ID, e.b, "30")
		require.True(t, before.Add(dec("30")).Equal(e.balance(e.b)))
		require.Equal(t, http.StatusNoContent, s.Do(http.MethodDelete, trURL(e.sid, "/", p.Transaction.ID), nil, bearer(e.u.AccessToken)).Status)
		assert.True(t, dec("100").Equal(dec(e.get(loan.ID).CurrentValue)))
		assert.True(t, before.Equal(e.balance(e.b)))
	})

	t.Run("the generic and transfer-edit endpoints refuse", func(t *testing.T) {
		p := pay(card.ID, e.a, "10")
		res := s.Do(http.MethodDelete, txURL(e.sid, "/", p.Transaction.ID), nil, bearer(e.u.AccessToken))
		require.Equal(t, http.StatusUnprocessableEntity, res.Status)
		assert.Equal(t, "USE_SPECIFIC_ENDPOINT", errCode(t, res))
		res = s.Do(http.MethodPatch, txURL(e.sid, "/", p.Transaction.ID), map[string]any{"note": "x"}, bearer(e.u.AccessToken))
		assert.Equal(t, "USE_SPECIFIC_ENDPOINT", errCode(t, res))
		res = s.Do(http.MethodPatch, trURL(e.sid, "/", p.Transaction.ID), map[string]any{"note": "x"}, bearer(e.u.AccessToken))
		require.Equal(t, http.StatusUnprocessableEntity, res.Status, string(res.Body))
		assert.Equal(t, "NOT_SUPPORTED_YET", errCode(t, res), "a settlement is deleted and recorded again, not edited")
		assert.True(t, dec("490").Equal(dec(e.get(card.ID).CurrentValue)))
	})

	t.Run("the path must name the settlement's own asset", func(t *testing.T) {
		p := pay(card.ID, e.a, "5")
		res := s.Do(http.MethodDelete, assetURL(e.sid, "/", loan.ID, "/settlements/", p.Transaction.ID), nil, bearer(e.u.AccessToken))
		assert.Equal(t, http.StatusNotFound, res.Status)
		assert.Equal(t, http.StatusNotFound, s.Do(http.MethodDelete, assetURL(e.sid, "/", card.ID, "/settlements/", uuid.NewString()), nil, bearer(e.u.AccessToken)).Status)
		assert.Equal(t, http.StatusNotFound, s.Do(http.MethodDelete, assetURL(e.sid, "/", card.ID, "/settlements/nope"), nil, bearer(e.u.AccessToken)).Status)
		exp := e.world.must(exp(e.a, "1", ""))
		assert.Equal(t, http.StatusNotFound, s.Do(http.MethodDelete, assetURL(e.sid, "/", card.ID, "/settlements/", exp.ID), nil, bearer(e.u.AccessToken)).Status, "an ordinary expense is not a settlement")
	})

	t.Run("a deleted asset's settlement can still be removed (the wallet is restored)", func(t *testing.T) {
		d := e.must(debt("Gone soon", "100"))
		before := e.balance(e.a)
		p := pay(d.ID, e.a, "40")
		require.Equal(t, http.StatusNoContent, s.Do(http.MethodDelete, assetURL(e.sid, "/", d.ID), nil, bearer(e.u.AccessToken)).Status)
		require.Equal(t, http.StatusNoContent, s.Do(http.MethodDelete, trURL(e.sid, "/", p.Transaction.ID), nil, bearer(e.u.AccessToken)).Status)
		assert.True(t, before.Equal(e.balance(e.a)))
		var v string
		require.NoError(t, s.Tx.QueryRow(context.Background(), `SELECT current_value::text FROM manual_assets WHERE id = $1`, d.ID).Scan(&v))
		assert.True(t, dec("60").Equal(dec(v)), "the deleted asset itself is left as it was")
	})

	t.Run("reversal after a manual correction simply adds the amount back", func(t *testing.T) {
		d := e.must(debt("Corrected", "100"))
		p := pay(d.ID, e.a, "30") // 70 left
		require.Equal(t, http.StatusOK, e.patch(d.ID, map[string]any{"currentValue": "50"}).Status)
		require.Equal(t, http.StatusNoContent, s.Do(http.MethodDelete, assetURL(e.sid, "/", d.ID, "/settlements/", p.Transaction.ID), nil, bearer(e.u.AccessToken)).Status)
		assert.True(t, dec("80").Equal(dec(e.get(d.ID).CurrentValue)))
	})
}

func TestManualAssetsAreSpaceScoped(t *testing.T) {
	s := NewTestServer(t)
	e := newAssetEnv(t, s)
	outsider := s.register("outsider@example.com")
	member := s.register("member@example.com")
	s.join(e.u, e.sid, member)
	a := e.must(debt("Card", "100"))
	p := e.settle(a.ID, map[string]any{"walletId": e.a.ID, "amount": "10"})
	var sb settleBody
	p.JSON(t, &sb)

	t.Run("outsider gets 404 on every route", func(t *testing.T) {
		expectCrossSpace404(t, s, outsider, http.MethodGet, assetURL(e.sid), nil)
		expectCrossSpace404(t, s, outsider, http.MethodPost, assetURL(e.sid), debt("x", "1"))
		expectCrossSpace404(t, s, outsider, http.MethodGet, assetURL(e.sid, "/", a.ID), nil)
		expectCrossSpace404(t, s, outsider, http.MethodPatch, assetURL(e.sid, "/", a.ID), map[string]any{"name": "x"})
		expectCrossSpace404(t, s, outsider, http.MethodDelete, assetURL(e.sid, "/", a.ID), nil)
		expectCrossSpace404(t, s, outsider, http.MethodPost, assetURL(e.sid, "/", a.ID, "/settlements"), map[string]any{"walletId": e.a.ID, "amount": "1"})
		expectCrossSpace404(t, s, outsider, http.MethodDelete, assetURL(e.sid, "/", a.ID, "/settlements/", sb.Transaction.ID), nil)
		expectCrossSpace404(t, s, outsider, http.MethodGet, "/api/v1/spaces/"+e.sid+"/net-worth/summary", nil)
	})

	t.Run("an asset id from another space is 404 in your own space", func(t *testing.T) {
		mine := spaceIDOf(outsider)
		for _, c := range []struct {
			method, path string
			body         any
		}{
			{http.MethodGet, assetURL(mine, "/", a.ID), nil},
			{http.MethodPatch, assetURL(mine, "/", a.ID), map[string]any{"name": "x"}},
			{http.MethodDelete, assetURL(mine, "/", a.ID), nil},
			{http.MethodDelete, assetURL(mine, "/", a.ID, "/settlements/", sb.Transaction.ID), nil},
		} {
			res := s.Do(c.method, c.path, c.body, bearer(outsider.AccessToken))
			require.Equal(t, http.StatusNotFound, res.Status, "%s %s", c.method, c.path)
		}
		assert.True(t, dec("90").Equal(dec(e.get(a.ID).CurrentValue)))
	})

	t.Run("members can manage assets", func(t *testing.T) {
		res := s.Do(http.MethodPost, assetURL(e.sid), debt("Member's", "5"), bearer(member.AccessToken))
		require.Equal(t, http.StatusCreated, res.Status, string(res.Body))
	})
}
