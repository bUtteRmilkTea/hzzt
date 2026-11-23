//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
	"github.com/JonathanRainee/hzzt/internal/ledger"
)

type ref struct {
	ID, Name, Icon string
	Color          *string
}

type txnBody struct {
	ID         string  `json:"id"`
	Type       string  `json:"type"`
	Direction  *string `json:"direction"`
	SystemKey  *string `json:"systemKey"`
	Title      *string `json:"title"`
	Note       *string `json:"note"`
	Amount     string  `json:"amount"`
	Currency   string  `json:"currency"`
	Wallet     *ref    `json:"wallet"`
	Category   *ref    `json:"category"`
	ToWallet   *ref    `json:"toWallet"`
	ToAmount   *string `json:"toAmount"`
	OccurredAt string  `json:"occurredAt"`
}

type txnList struct {
	Items      []txnBody `json:"items"`
	NextCursor *string   `json:"nextCursor"`
}

type summaryBody struct {
	Currency string `json:"currency"`
	Items    []struct {
		Date     string `json:"date"`
		Income   string `json:"income"`
		Expense  string `json:"expense"`
		Transfer string `json:"transfer"`
		Count    int    `json:"count"`
	} `json:"items"`
	Warnings []struct{ Code, Message string } `json:"warnings"`
}

func txURL(sid string, rest ...string) string {
	return "/api/v1/spaces/" + sid + "/transactions" + strings.Join(rest, "")
}

// world is a space with two USD wallets and the seeded categories.
type world struct {
	s    *TestServer
	u    session
	sid  string
	a, b walletBody
	cat  map[string]string // category name → id
}

func newWorld(t *testing.T, s *TestServer) *world {
	t.Helper()
	u := s.register("jane@example.com")
	sid := spaceIDOf(u)
	w := &world{s: s, u: u, sid: sid, cat: map[string]string{}}
	mk := func(name, bal string) walletBody {
		b := wal(name)
		b["initialBalance"] = bal
		return s.newWallet(u, sid, b)
	}
	w.a, w.b = mk("Wallet A", "1000"), mk("Wallet B", "500")
	for _, c := range s.categories(u, sid, "") {
		w.cat[c.Name] = c.ID
	}
	return w
}

func (w *world) post(body map[string]any) Response {
	return w.s.Do(http.MethodPost, txURL(w.sid), body, bearer(w.u.AccessToken))
}

func (w *world) must(body map[string]any) txnBody {
	w.s.t.Helper()
	res := w.post(body)
	require.Equal(w.s.t, http.StatusCreated, res.Status, string(res.Body))
	var t txnBody
	res.JSON(w.s.t, &t)
	return t
}

func exp(wallet walletBody, amount, cat string) map[string]any {
	m := map[string]any{"type": "EXPENSE", "walletId": wallet.ID, "amount": amount}
	if cat != "" {
		m["categoryId"] = cat
	}
	return m
}

func (w *world) balance(wallet walletBody) decimal.Decimal {
	w.s.t.Helper()
	res := w.s.Get(walURL(w.sid, "/", wallet.ID), bearer(w.u.AccessToken))
	require.Equal(w.s.t, http.StatusOK, res.Status)
	var b walletBody
	res.JSON(w.s.t, &b)
	return dec(b.Balance)
}

func (w *world) list(query string) txnList {
	w.s.t.Helper()
	res := w.s.Get(txURL(w.sid)+query, bearer(w.u.AccessToken))
	require.Equal(w.s.t, http.StatusOK, res.Status, "%s: %s", query, res.Body)
	var l txnList
	res.JSON(w.s.t, &l)
	return l
}

func (w *world) summary(query string) summaryBody {
	w.s.t.Helper()
	res := w.s.Get(txURL(w.sid, "/daily-summary")+query, bearer(w.u.AccessToken))
	require.Equal(w.s.t, http.StatusOK, res.Status, "%s: %s", query, res.Body)
	var sm summaryBody
	res.JSON(w.s.t, &sm)
	return sm
}

func ids(l txnList) []string {
	out := make([]string, len(l.Items))
	for i, t := range l.Items {
		out[i] = t.ID
	}
	return out
}

// transfer records a transfer directly through the ledger (the endpoint arrives in P9).
func (w *world) transfer(from, to walletBody, amount, toAmount string, at time.Time) string {
	w.s.t.Helper()
	ctx := context.Background()
	q := sqlc.New(w.s.Tx)
	toCur := to.Currency
	t, err := q.CreateTransaction(ctx, sqlc.CreateTransactionParams{
		SpaceID: uuid.MustParse(w.sid), Type: "TRANSFER", Amount: dec(amount), Currency: from.Currency,
		WalletID: uuid.NullUUID{UUID: uuid.MustParse(from.ID), Valid: true}, OccurredAt: at,
		CreatedByID: uuid.MustParse(w.u.User.ID),
		ToWalletID:  uuid.NullUUID{UUID: uuid.MustParse(to.ID), Valid: true},
		ToAmount:    decimal.NullDecimal{Decimal: dec(toAmount), Valid: true}, ToCurrency: &toCur,
	})
	require.NoError(w.s.t, err)
	require.NoError(w.s.t, ledger.Apply(ctx, w.s.Tx, t))
	return t.ID.String()
}

func TestCreateIncomeExpense(t *testing.T) {
	s := NewTestServer(t)
	w := newWorld(t, s)

	t.Run("unauthenticated", func(t *testing.T) {
		assert.Equal(t, http.StatusUnauthorized, s.Get(txURL(w.sid)).Status)
		assert.Equal(t, http.StatusUnauthorized, s.Do(http.MethodPost, txURL(w.sid), exp(w.a, "1", "")).Status)
	})

	t.Run("expense lowers the balance and carries references", func(t *testing.T) {
		body := exp(w.a, "50.25", w.cat["Transport"])
		body["title"], body["note"] = "  Taxi  ", "airport"
		got := w.must(body)
		assert.Equal(t, "EXPENSE", got.Type)
		assert.Equal(t, "USD", got.Currency)
		assert.True(t, dec("50.25").Equal(dec(got.Amount)))
		require.NotNil(t, got.Title)
		assert.Equal(t, "Taxi", *got.Title)
		require.NotNil(t, got.Wallet)
		assert.Equal(t, "Wallet A", got.Wallet.Name)
		require.NotNil(t, got.Category)
		assert.Equal(t, "Transport", got.Category.Name)
		assert.Equal(t, "🚗", got.Category.Icon)
		require.NotNil(t, got.Category.Color)
		assert.True(t, dec("949.75").Equal(w.balance(w.a)))
		expectLedgerConsistent(t, s, w.a.ID)
	})

	t.Run("income raises the balance; category optional", func(t *testing.T) {
		got := w.must(map[string]any{"type": "INCOME", "walletId": w.b.ID, "amount": 100, "categoryId": w.cat["Salary"]})
		assert.Nil(t, got.Direction)
		assert.True(t, dec("600").Equal(w.balance(w.b)))
		got = w.must(map[string]any{"type": "INCOME", "walletId": w.b.ID, "amount": "1"})
		assert.Nil(t, got.Category)
		assert.Nil(t, got.Title)
		assert.True(t, dec("601").Equal(w.balance(w.b)))
	})

	t.Run("currency is the wallet's currency", func(t *testing.T) {
		b := wal("Rupiah")
		b["currency"] = "IDR"
		idr := s.newWallet(w.u, w.sid, b)
		got := w.must(exp(idr, "25000", ""))
		assert.Equal(t, "IDR", got.Currency)
		assert.True(t, dec("-25000").Equal(w.balance(idr)), "balances may go negative")
	})

	t.Run("occurredAt is honoured", func(t *testing.T) {
		body := exp(w.a, "1", "")
		body["occurredAt"] = "2026-01-02T03:04:05Z"
		got := w.must(body)
		assert.Equal(t, "2026-01-02T03:04:05Z", got.OccurredAt)
	})

	t.Run("validation", func(t *testing.T) {
		mk := func(mod func(m map[string]any)) map[string]any { m := exp(w.a, "5", ""); mod(m); return m }
		for name, body := range map[string]map[string]any{
			"zero":              mk(func(m map[string]any) { m["amount"] = "0" }),
			"negative":          mk(func(m map[string]any) { m["amount"] = "-5" }),
			"not a number":      mk(func(m map[string]any) { m["amount"] = "five" }),
			"too many decimals": mk(func(m map[string]any) { m["amount"] = "1.123456789" }),
			"too large":         mk(func(m map[string]any) { m["amount"] = "100000000000000000000" }),
			"missing amount":    mk(func(m map[string]any) { delete(m, "amount") }),
			"missing wallet":    mk(func(m map[string]any) { delete(m, "walletId") }),
			"bad wallet id":     mk(func(m map[string]any) { m["walletId"] = "nope" }),
			"bad type":          mk(func(m map[string]any) { m["type"] = "TRANSFER" }),
			"adjustment type":   mk(func(m map[string]any) { m["type"] = "ADJUSTMENT" }),
			"long title":        mk(func(m map[string]any) { m["title"] = strings.Repeat("t", 201) }),
			"bad date":          mk(func(m map[string]any) { m["occurredAt"] = "yesterday" }),
			"unknown field":     mk(func(m map[string]any) { m["currency"] = "EUR" }),
		} {
			res := w.post(body)
			assert.Equal(t, http.StatusBadRequest, res.Status, "%s: %s", name, res.Body)
		}
		assert.True(t, dec("949.75").Sub(w.balance(w.a)).Abs().LessThanOrEqual(dec("1")), "rejected requests change nothing")
	})

	t.Run("references must exist in this space", func(t *testing.T) {
		res := w.post(exp(walletBody{ID: uuid.NewString()}, "5", ""))
		require.Equal(t, http.StatusNotFound, res.Status)
		assert.Equal(t, "WALLET_NOT_FOUND", errCode(t, res))

		res = w.post(exp(w.a, "5", uuid.NewString()))
		require.Equal(t, http.StatusNotFound, res.Status)
		assert.Equal(t, "CATEGORY_NOT_FOUND", errCode(t, res))

		// a deleted wallet is gone
		gone := s.newWallet(w.u, w.sid, wal("Gone"))
		require.Equal(t, http.StatusNoContent, s.Do(http.MethodDelete, walURL(w.sid, "/", gone.ID), nil, bearer(w.u.AccessToken)).Status)
		assert.Equal(t, "WALLET_NOT_FOUND", errCode(t, w.post(exp(gone, "5", ""))))

		// a deleted category is gone
		del := s.newCategory(w.u, w.sid, cat("EXPENSE", "Soon deleted"))
		var dc categoryBody
		del.JSON(t, &dc)
		require.Equal(t, http.StatusNoContent, s.Do(http.MethodDelete, catURL(w.sid, "/", dc.ID), nil, bearer(w.u.AccessToken)).Status)
		assert.Equal(t, "CATEGORY_NOT_FOUND", errCode(t, w.post(exp(w.a, "5", dc.ID))))

		// another space's wallet and category are invisible
		other := s.register("other@example.com")
		osid := spaceIDOf(other)
		ow := s.newWallet(other, osid, wal("Theirs"))
		assert.Equal(t, "WALLET_NOT_FOUND", errCode(t, w.post(exp(ow, "5", ""))))
		oc := s.categories(other, osid, "")[0].ID
		assert.Equal(t, "CATEGORY_NOT_FOUND", errCode(t, w.post(exp(w.a, "5", oc))))
		var n int
		require.NoError(t, s.Tx.QueryRow(context.Background(), `SELECT count(*) FROM transactions WHERE wallet_id = $1`, ow.ID).Scan(&n))
		assert.Zero(t, n, "nothing was written to the other space's wallet")
	})

	t.Run("category type must match", func(t *testing.T) {
		res := w.post(exp(w.a, "5", w.cat["Salary"]))
		require.Equal(t, http.StatusUnprocessableEntity, res.Status)
		assert.Equal(t, "CATEGORY_TYPE_MISMATCH", errCode(t, res))
		res = w.post(map[string]any{"type": "INCOME", "walletId": w.a.ID, "amount": "5", "categoryId": w.cat["Transport"]})
		assert.Equal(t, "CATEGORY_TYPE_MISMATCH", errCode(t, res))
	})
}

func TestTopUp(t *testing.T) {
	s := NewTestServer(t)
	w := newWorld(t, s)
	top := func(wallet string, body map[string]any) Response {
		return s.Do(http.MethodPost, walURL(w.sid, "/", wallet, "/top-up"), body, bearer(w.u.AccessToken))
	}

	res := top(w.a.ID, map[string]any{"amount": "250.5", "note": "salary advance"})
	require.Equal(t, http.StatusCreated, res.Status, string(res.Body))
	var got txnBody
	res.JSON(t, &got)
	assert.Equal(t, "ADJUSTMENT", got.Type, "a top up is not income")
	require.NotNil(t, got.Direction)
	assert.Equal(t, "IN", *got.Direction)
	require.NotNil(t, got.SystemKey)
	assert.Equal(t, "BALANCE_TOP_UP", *got.SystemKey)
	require.NotNil(t, got.Title)
	assert.Equal(t, "Balance top up", *got.Title)
	assert.Nil(t, got.Category)
	require.NotNil(t, got.Note)
	assert.Equal(t, "salary advance", *got.Note)
	assert.True(t, dec("1250.5").Equal(w.balance(w.a)))

	assert.Equal(t, http.StatusBadRequest, top(w.a.ID, map[string]any{"amount": "0"}).Status)
	assert.Equal(t, http.StatusBadRequest, top(w.a.ID, map[string]any{}).Status)
	assert.Equal(t, http.StatusNotFound, top(uuid.NewString(), map[string]any{"amount": "1"}).Status)
	assert.Equal(t, http.StatusNotFound, top("nope", map[string]any{"amount": "1"}).Status)
	assert.Equal(t, http.StatusUnauthorized, s.Do(http.MethodPost, walURL(w.sid, "/", w.a.ID, "/top-up"), map[string]any{"amount": "1"}).Status)
}

func TestEditAndDelete(t *testing.T) {
	s := NewTestServer(t)
	w := newWorld(t, s)
	patch := func(id string, body map[string]any) Response {
		return s.Do(http.MethodPatch, txURL(w.sid, "/", id), body, bearer(w.u.AccessToken))
	}

	t.Run("acceptance: moving an expense from wallet A (50) to wallet B (70)", func(t *testing.T) {
		e := w.must(exp(w.a, "50", w.cat["Shopping"]))
		require.True(t, dec("950").Equal(w.balance(w.a)))
		res := patch(e.ID, map[string]any{"walletId": w.b.ID, "amount": "70"})
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		var got txnBody
		res.JSON(t, &got)
		assert.Equal(t, "Wallet B", got.Wallet.Name)
		assert.True(t, dec("70").Equal(dec(got.Amount)))
		assert.True(t, dec("1000").Equal(w.balance(w.a)), "A is restored by 50")
		assert.True(t, dec("430").Equal(w.balance(w.b)), "B is reduced by 70")
		assert.Equal(t, "Shopping", got.Category.Name, "untouched fields stay")
		expectLedgerConsistent(t, s, w.a.ID)
		expectLedgerConsistent(t, s, w.b.ID)
		require.Equal(t, http.StatusNoContent, s.Do(http.MethodDelete, txURL(w.sid, "/", e.ID), nil, bearer(w.u.AccessToken)).Status)
		assert.True(t, dec("500").Equal(w.balance(w.b)))
	})

	t.Run("changing type, amount and clearing fields", func(t *testing.T) {
		e := w.must(map[string]any{"type": "EXPENSE", "walletId": w.a.ID, "amount": "10", "categoryId": w.cat["Gifts"], "title": "t", "note": "n"})
		// switching to INCOME with an expense category is a mismatch
		res := patch(e.ID, map[string]any{"type": "INCOME"})
		require.Equal(t, http.StatusUnprocessableEntity, res.Status)
		assert.Equal(t, "CATEGORY_TYPE_MISMATCH", errCode(t, res))
		assert.True(t, dec("990").Equal(w.balance(w.a)), "a rejected edit changes nothing")

		res = patch(e.ID, map[string]any{"type": "INCOME", "categoryId": w.cat["Salary"], "amount": "30", "title": nil, "note": nil})
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		var got txnBody
		res.JSON(t, &got)
		assert.Equal(t, "INCOME", got.Type)
		assert.Equal(t, "Salary", got.Category.Name)
		assert.Nil(t, got.Title)
		assert.Nil(t, got.Note)
		assert.True(t, dec("1030").Equal(w.balance(w.a)))

		res = patch(e.ID, map[string]any{"categoryId": nil, "occurredAt": "2026-03-04T05:06:07Z"})
		require.Equal(t, http.StatusOK, res.Status)
		res.JSON(t, &got)
		assert.Nil(t, got.Category)
		assert.Equal(t, "2026-03-04T05:06:07Z", got.OccurredAt)
		assert.True(t, dec("1030").Equal(w.balance(w.a)))
		expectLedgerConsistent(t, s, w.a.ID)
	})

	t.Run("moving to a wallet in another currency adopts that currency", func(t *testing.T) {
		b := wal("Rupiah")
		b["currency"] = "IDR"
		idr := s.newWallet(w.u, w.sid, b)
		e := w.must(exp(w.a, "5", ""))
		res := patch(e.ID, map[string]any{"walletId": idr.ID, "amount": "80000"})
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		var got txnBody
		res.JSON(t, &got)
		assert.Equal(t, "IDR", got.Currency)
		expectLedgerConsistent(t, s, idr.ID)
		expectLedgerConsistent(t, s, w.a.ID)
	})

	t.Run("validation and not-found", func(t *testing.T) {
		e := w.must(exp(w.a, "5", ""))
		for name, body := range map[string]map[string]any{
			"zero amount": {"amount": "0"}, "bad type": {"type": "NOPE"}, "bad wallet": {"walletId": "x"},
			"long note": {"note": strings.Repeat("n", 1001)}, "unknown": {"direction": "IN"},
		} {
			assert.Equal(t, http.StatusBadRequest, patch(e.ID, body).Status, name)
		}
		assert.Equal(t, "WALLET_NOT_FOUND", errCode(t, patch(e.ID, map[string]any{"walletId": uuid.NewString()})))
		assert.Equal(t, "CATEGORY_NOT_FOUND", errCode(t, patch(e.ID, map[string]any{"categoryId": uuid.NewString()})))
		res := patch(uuid.NewString(), map[string]any{"note": "x"})
		require.Equal(t, http.StatusNotFound, res.Status)
		assert.Equal(t, "TRANSACTION_NOT_FOUND", errCode(t, res))
		assert.Equal(t, http.StatusNotFound, patch("nope", map[string]any{"note": "x"}).Status)
		assert.Equal(t, http.StatusNotFound, s.Do(http.MethodDelete, txURL(w.sid, "/", uuid.NewString()), nil, bearer(w.u.AccessToken)).Status)
	})

	t.Run("adjustments and opening balances: only the note", func(t *testing.T) {
		var openingID string
		require.NoError(t, s.Tx.QueryRow(context.Background(),
			`SELECT id::text FROM transactions WHERE wallet_id = $1 AND type = 'OPENING_BALANCE'`, w.b.ID).Scan(&openingID))
		res := patch(openingID, map[string]any{"note": "from the old app"})
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		var got txnBody
		res.JSON(t, &got)
		require.NotNil(t, got.Note)
		assert.Equal(t, "from the old app", *got.Note)
		require.NotNil(t, got.Direction)
		assert.Equal(t, "IN", *got.Direction)

		balance := w.balance(w.b)
		for _, body := range []map[string]any{{"amount": "1"}, {"walletId": w.a.ID}, {"note": "x", "title": "y"}} {
			assert.Equal(t, http.StatusBadRequest, patch(openingID, body).Status, "%v", body)
		}
		// a change of kind is TYPE_CHANGE_NOT_ALLOWED, not a validation error (Phase F)
		assert.Equal(t, http.StatusUnprocessableEntity, patch(openingID, map[string]any{"type": "EXPENSE"}).Status)
		assert.True(t, balance.Equal(w.balance(w.b)))
	})

	t.Run("transfers and investments use their own endpoints", func(t *testing.T) {
		tr := w.transfer(w.a, w.b, "10", "10", time.Now())
		for _, c := range []struct {
			method string
			body   any
		}{{http.MethodPatch, map[string]any{"note": "x"}}, {http.MethodDelete, nil}} {
			res := s.Do(c.method, txURL(w.sid, "/", tr), c.body, bearer(w.u.AccessToken))
			require.Equal(t, http.StatusUnprocessableEntity, res.Status, string(res.Body))
			assert.Equal(t, "USE_SPECIFIC_ENDPOINT", errCode(t, res))
		}
		// an expense that belongs to an investment, simulated with the column P13 will set
		e := w.must(exp(w.a, "5", ""))
		_, err := s.Tx.Exec(context.Background(), `
			WITH inv AS (
				INSERT INTO investments (space_id, asset_type, name, symbol, icon, quote_currency, created_by_id)
				VALUES ($1, 'CRYPTO', 'Bitcoin', 'BTC', '🪙', 'USD', $2) RETURNING id)
			UPDATE transactions SET investment_id = (SELECT id FROM inv) WHERE id = $3`, w.sid, w.u.User.ID, e.ID)
		require.NoError(t, err)
		res := patch(e.ID, map[string]any{"note": "x"})
		require.Equal(t, http.StatusUnprocessableEntity, res.Status)
		assert.Equal(t, "USE_SPECIFIC_ENDPOINT", errCode(t, res))
		assert.Equal(t, http.StatusUnprocessableEntity, s.Do(http.MethodDelete, txURL(w.sid, "/", e.ID), nil, bearer(w.u.AccessToken)).Status)
	})

	t.Run("delete reverses and removes", func(t *testing.T) {
		before := w.balance(w.b)
		e := w.must(exp(w.b, "123.45", ""))
		require.True(t, before.Sub(dec("123.45")).Equal(w.balance(w.b)))
		require.Equal(t, http.StatusNoContent, s.Do(http.MethodDelete, txURL(w.sid, "/", e.ID), nil, bearer(w.u.AccessToken)).Status)
		assert.True(t, before.Equal(w.balance(w.b)))
		assert.Equal(t, http.StatusNotFound, s.Get(txURL(w.sid, "/", e.ID), bearer(w.u.AccessToken)).Status)
		assert.Equal(t, http.StatusNotFound, s.Do(http.MethodDelete, txURL(w.sid, "/", e.ID), nil, bearer(w.u.AccessToken)).Status)
	})

	t.Run("deleting an adjustment reverses it", func(t *testing.T) {
		require.Equal(t, http.StatusOK, s.Do(http.MethodPut, walURL(w.sid, "/", w.b.ID, "/balance"), map[string]any{"newBalance": "1"}, bearer(w.u.AccessToken)).Status)
		var adj string
		require.NoError(t, s.Tx.QueryRow(context.Background(), `SELECT id::text FROM transactions WHERE wallet_id = $1 AND type = 'ADJUSTMENT'`, w.b.ID).Scan(&adj))
		require.Equal(t, http.StatusNoContent, s.Do(http.MethodDelete, txURL(w.sid, "/", adj), nil, bearer(w.u.AccessToken)).Status)
		expectLedgerConsistent(t, s, w.b.ID)
	})
}

func TestTransactionsAreSpaceScoped(t *testing.T) {
	s := NewTestServer(t)
	w := newWorld(t, s)
	outsider := s.register("outsider@example.com")
	member := s.register("member@example.com")
	s.join(w.u, w.sid, member)
	e := w.must(exp(w.a, "5", ""))

	t.Run("outsider gets 404 on every route", func(t *testing.T) {
		expectCrossSpace404(t, s, outsider, http.MethodGet, txURL(w.sid), nil)
		expectCrossSpace404(t, s, outsider, http.MethodPost, txURL(w.sid), exp(w.a, "1", ""))
		expectCrossSpace404(t, s, outsider, http.MethodGet, txURL(w.sid, "/daily-summary"), nil)
		expectCrossSpace404(t, s, outsider, http.MethodGet, txURL(w.sid, "/", e.ID), nil)
		expectCrossSpace404(t, s, outsider, http.MethodPatch, txURL(w.sid, "/", e.ID), map[string]any{"note": "x"})
		expectCrossSpace404(t, s, outsider, http.MethodDelete, txURL(w.sid, "/", e.ID), nil)
		expectCrossSpace404(t, s, outsider, http.MethodPost, walURL(w.sid, "/", w.a.ID, "/top-up"), map[string]any{"amount": "1"})
	})

	t.Run("a transaction id from another space is 404 in your own space", func(t *testing.T) {
		mine := spaceIDOf(outsider)
		for _, c := range []struct {
			method string
			body   any
		}{
			{http.MethodGet, nil}, {http.MethodPatch, map[string]any{"note": "x"}}, {http.MethodDelete, nil},
		} {
			res := s.Do(c.method, txURL(mine, "/", e.ID), c.body, bearer(outsider.AccessToken))
			require.Equal(t, http.StatusNotFound, res.Status, string(res.Body))
			assert.Equal(t, "TRANSACTION_NOT_FOUND", errCode(t, res))
		}
		res := s.Get(txURL(mine), bearer(outsider.AccessToken))
		var l txnList
		res.JSON(t, &l)
		assert.Empty(t, l.Items, "the list never leaks other spaces")
		assert.True(t, dec("995").Equal(w.balance(w.a)), "owner data untouched")
	})

	t.Run("members can record transactions", func(t *testing.T) {
		res := s.Do(http.MethodPost, txURL(w.sid), exp(w.a, "1", ""), bearer(member.AccessToken))
		require.Equal(t, http.StatusCreated, res.Status, string(res.Body))
		var got struct {
			CreatedByID string `json:"createdById"`
		}
		res.JSON(t, &got)
		assert.Equal(t, member.User.ID, got.CreatedByID)
	})
}

func TestListFiltersAndSearch(t *testing.T) {
	s := NewTestServer(t)
	w := newWorld(t, s)
	at := func(d string) string { return d + "T12:00:00Z" }
	mk := func(typ, wallet, cat, amount, date, title, note string) txnBody {
		b := map[string]any{"type": typ, "walletId": wallet, "amount": amount, "occurredAt": at(date)}
		if cat != "" {
			b["categoryId"] = cat
		}
		if title != "" {
			b["title"] = title
		}
		if note != "" {
			b["note"] = note
		}
		return w.must(b)
	}
	coffee := mk("EXPENSE", w.a.ID, w.cat["Snacks"], "5", "2026-11-01", "Morning coffee", "")
	taxi := mk("EXPENSE", w.a.ID, w.cat["Transport"], "20", "2026-11-05", "Taxi home", "late night")
	pay := mk("INCOME", w.b.ID, w.cat["Salary"], "3000", "2026-11-10", "November pay", "")
	lunch := mk("EXPENSE", w.b.ID, w.cat["Snacks"], "12", "2026-11-12", "Lunch", "with team")
	gift := mk("EXPENSE", w.b.ID, w.cat["Gifts"], "40", "2026-11-15", "100% birthday_gift", "")
	tr := w.transfer(w.a, w.b, "100", "100", time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC))

	t.Run("newest first", func(t *testing.T) {
		l := w.list("?types=INCOME,EXPENSE,TRANSFER")
		got := ids(l)
		assert.Equal(t, []string{tr, gift.ID, lunch.ID, pay.ID, taxi.ID, coffee.ID}, got[:6])
		assert.Nil(t, l.NextCursor)
		for _, it := range l.Items {
			if it.ID == tr {
				require.NotNil(t, it.ToWallet)
				assert.Equal(t, "Wallet B", it.ToWallet.Name)
				assert.Equal(t, "Wallet A", it.Wallet.Name)
			}
		}
	})

	t.Run("types", func(t *testing.T) {
		assert.ElementsMatch(t, []string{pay.ID}, ids(w.list("?types=INCOME&from=2026-11-01")))
		for _, q := range []string{"?types=TRANSFER", "?types[]=TRANSFER", "?types=transfer"} {
			assert.Equal(t, []string{tr}, ids(w.list(q)), q)
		}
		assert.Len(t, w.list("?types=INCOME&types=TRANSFER").Items, 2+0, "wallet opening balances are not INCOME")
	})

	t.Run("categories", func(t *testing.T) {
		assert.ElementsMatch(t, []string{coffee.ID, lunch.ID}, ids(w.list("?categoryIds="+w.cat["Snacks"])))
		assert.ElementsMatch(t, []string{coffee.ID, lunch.ID, taxi.ID},
			ids(w.list("?categoryIds="+w.cat["Snacks"]+","+w.cat["Transport"])), "OR within a list")
		assert.Empty(t, ids(w.list("?categoryIds="+uuid.NewString())))
	})

	t.Run("wallets match source or destination", func(t *testing.T) {
		a := ids(w.list("?walletIds=" + w.a.ID + "&types=EXPENSE,TRANSFER"))
		assert.ElementsMatch(t, []string{coffee.ID, taxi.ID, tr}, a)
		b := ids(w.list("?walletIds=" + w.b.ID + "&types=EXPENSE,TRANSFER,INCOME"))
		assert.ElementsMatch(t, []string{pay.ID, lunch.ID, gift.ID, tr}, b, "the transfer shows in the destination wallet's history too")
	})

	t.Run("AND across filters", func(t *testing.T) {
		got := ids(w.list("?types=EXPENSE&walletIds=" + w.b.ID + "&categoryIds=" + w.cat["Snacks"]))
		assert.Equal(t, []string{lunch.ID}, got)
		got = ids(w.list("?types=INCOME&categoryIds=" + w.cat["Snacks"]))
		assert.Empty(t, got)
	})

	t.Run("period and custom dates", func(t *testing.T) {
		got := ids(w.list("?from=2026-11-05&to=2026-11-12&types=INCOME,EXPENSE"))
		assert.ElementsMatch(t, []string{taxi.ID, pay.ID, lunch.ID}, got, "both ends inclusive")
		got = ids(w.list("?from=2026-11-15&types=INCOME,EXPENSE"))
		assert.ElementsMatch(t, []string{gift.ID}, got[:1])
		got = ids(w.list("?to=2026-11-01&types=INCOME,EXPENSE"))
		assert.Contains(t, got, coffee.ID)
		assert.NotContains(t, got, taxi.ID)
		s.Clock.Set(time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC))
		assert.ElementsMatch(t, []string{lunch.ID, gift.ID, tr},
			ids(w.list("?period=LAST_7_DAYS&types=INCOME,EXPENSE,TRANSFER")), "11–17 Nov")
		assert.ElementsMatch(t, []string{tr}, ids(w.list("?period=THIS_WEEK&types=INCOME,EXPENSE,TRANSFER")), "week of Mon 16 Nov: Sunday the 15th is last week")
		assert.Empty(t, ids(w.list("?period=TODAY&types=INCOME,EXPENSE,TRANSFER")))
		assert.NotEmpty(t, ids(w.list("?period=ALL_TIME&types=EXPENSE")))
	})

	t.Run("search", func(t *testing.T) {
		assert.Equal(t, []string{coffee.ID}, ids(w.list("?q=coffee")))
		assert.Equal(t, []string{coffee.ID}, ids(w.list("?q=MORNING")), "case-insensitive")
		assert.Equal(t, []string{taxi.ID}, ids(w.list("?q=late+night")), "notes")
		assert.ElementsMatch(t, []string{coffee.ID, lunch.ID}, ids(w.list("?q=snacks")), "category names")
		assert.Contains(t, ids(w.list("?q=wallet+b")), pay.ID, "wallet names")
		assert.Contains(t, ids(w.list("?q=wallet+b")), tr, "destination wallet names")
		assert.Equal(t, []string{gift.ID}, ids(w.list("?q="+url.QueryEscape("100%"))), "% is a literal")
		assert.Equal(t, []string{gift.ID}, ids(w.list("?q="+url.QueryEscape("day_gift"))), "_ is a literal")
		assert.Empty(t, ids(w.list("?q="+url.QueryEscape("%%%"))))
		assert.Empty(t, ids(w.list("?q=nothing-matches-this")))
		assert.Equal(t, []string{coffee.ID}, ids(w.list("?q=coffee&types=EXPENSE&categoryIds="+w.cat["Snacks"])), "search combines with filters")
	})

	t.Run("invalid parameters", func(t *testing.T) {
		for _, q := range []string{
			"?period=YESTERDAY", "?from=11/01/2026", "?to=soon", "?from=2026-12-01&to=2026-11-01", "?types=SPENDING",
			"?categoryIds=abc", "?walletIds=1,2", "?limit=0", "?limit=101", "?limit=x", "?cursor=garbage",
			"?q=" + strings.Repeat("x", 101),
		} {
			res := s.Get(txURL(w.sid)+q, bearer(w.u.AccessToken))
			assert.Equal(t, http.StatusBadRequest, res.Status, "%s: %s", q, res.Body)
		}
	})
}

func TestListPagination(t *testing.T) {
	s := NewTestServer(t)
	w := newWorld(t, s)
	const same = "2026-11-10T08:00:00Z"
	var want []string
	// 7 transactions share one timestamp (ties must paginate by id), 3 more are older
	for i := 0; i < 7; i++ {
		want = append(want, w.must(map[string]any{"type": "EXPENSE", "walletId": w.a.ID, "amount": "1", "occurredAt": same, "title": fmt.Sprint("tie ", i)}).ID)
	}
	for i := 0; i < 3; i++ {
		want = append(want, w.must(map[string]any{"type": "EXPENSE", "walletId": w.a.ID, "amount": "1", "occurredAt": fmt.Sprintf("2026-11-0%dT08:00:00Z", i+1)}).ID)
	}
	full := ids(w.list("?types=EXPENSE&limit=100"))
	require.Len(t, full, 10)
	assert.ElementsMatch(t, want, full)

	var paged []string
	cursor := ""
	pages := 0
	for {
		q := "?types=EXPENSE&limit=3"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		l := w.list(q)
		pages++
		paged = append(paged, ids(l)...)
		if l.NextCursor == nil {
			assert.LessOrEqual(t, len(l.Items), 3)
			break
		}
		assert.Len(t, l.Items, 3)
		cursor = *l.NextCursor
		require.Less(t, pages, 10, "pagination must terminate")
	}
	assert.Equal(t, 4, pages)
	assert.Equal(t, full, paged, "pages concatenate to exactly the unpaginated order: no gaps, no repeats")

	t.Run("exact multiple of the page size ends without an empty cursor", func(t *testing.T) {
		l := w.list("?types=EXPENSE&limit=10")
		assert.Len(t, l.Items, 10)
		assert.Nil(t, l.NextCursor)
	})

	t.Run("filters apply on later pages", func(t *testing.T) {
		l := w.list("?types=EXPENSE&q=tie&limit=4")
		require.NotNil(t, l.NextCursor)
		l2 := w.list("?types=EXPENSE&q=tie&limit=4&cursor=" + *l.NextCursor)
		assert.Len(t, l2.Items, 3)
		assert.Nil(t, l2.NextCursor)
	})
}

func TestPeriodsUseTheUsersTimezone(t *testing.T) {
	s := NewTestServer(t)
	u := s.register("jakarta@example.com")
	require.Equal(t, http.StatusOK, s.Do(http.MethodPatch, "/api/v1/me", map[string]any{"timezone": "Asia/Jakarta"}, bearer(u.AccessToken)).Status)
	sid := spaceIDOf(u)
	wal1 := s.newWallet(u, sid, wal("Main"))
	w := &world{s: s, u: u, sid: sid, a: wal1, cat: map[string]string{}}

	jkt, _ := time.LoadLocation("Asia/Jakarta")
	// "now" is 23:30 local on 17 Nov in Jakarta
	s.Clock.Set(time.Date(2026, 11, 17, 23, 30, 0, 0, jkt))

	mk := func(local time.Time) string {
		return w.must(map[string]any{"type": "EXPENSE", "walletId": wal1.ID, "amount": "1", "occurredAt": local.UTC().Format(time.RFC3339)}).ID
	}
	dayBefore := mk(time.Date(2026, 11, 16, 23, 59, 0, 0, jkt))
	startOfDay := mk(time.Date(2026, 11, 17, 0, 0, 0, 0, jkt))
	evening := mk(time.Date(2026, 11, 17, 23, 0, 0, 0, jkt))
	nextDay := mk(time.Date(2026, 11, 18, 0, 0, 0, 0, jkt))

	got := ids(w.list("?period=TODAY&types=EXPENSE"))
	assert.ElementsMatch(t, []string{startOfDay, evening}, got, "TODAY at 23:30 Jakarta is exactly the local 17th")
	assert.NotContains(t, got, dayBefore)
	assert.NotContains(t, got, nextDay)

	// the same instants grouped by local date in the summary
	sm := w.summary("?types=EXPENSE")
	require.Len(t, sm.Items, 3)
	assert.Equal(t, "2026-11-18", sm.Items[0].Date)
	assert.Equal(t, 2, sm.Items[1].Count)
	assert.Equal(t, "2026-11-17", sm.Items[1].Date)
	assert.Equal(t, "2026-11-16", sm.Items[2].Date)
}

func TestThisPeriodUsesBudgetStartDay(t *testing.T) {
	s := NewTestServer(t)
	w := newWorld(t, s)
	// UTC user; start day 15; "now" is 10 Nov 2026 → the period is 15 Oct .. 15 Nov
	_, err := s.Tx.Exec(context.Background(), `UPDATE spaces SET budget_period_start_day = 15 WHERE id = $1`, w.sid)
	require.NoError(t, err)
	s.Clock.Set(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	mk := func(date string) string {
		return w.must(map[string]any{"type": "EXPENSE", "walletId": w.a.ID, "amount": "1", "occurredAt": date + "T12:00:00Z"}).ID
	}
	before, first, last, after := mk("2026-10-14"), mk("2026-10-15"), mk("2026-11-14"), mk("2026-11-15")
	got := ids(w.list("?period=THIS_PERIOD&types=EXPENSE"))
	assert.ElementsMatch(t, []string{first, last}, got)
	assert.NotContains(t, got, before)
	assert.NotContains(t, got, after)
}

func TestDailySummary(t *testing.T) {
	s := NewTestServer(t)
	require.NoError(t, s.Currency.Refresh(context.Background())) // 1 USD = 16000 IDR
	w := newWorld(t, s)
	b := wal("Rupiah")
	b["currency"] = "IDR"
	idr := s.newWallet(w.u, w.sid, b)
	sg := wal("Singapore")
	sg["currency"] = "SGD" // no SGD rate in the fake provider
	sgd := s.newWallet(w.u, w.sid, sg)

	day := func(d string) string { return d + "T10:00:00Z" }
	w.must(map[string]any{"type": "INCOME", "walletId": w.a.ID, "amount": "100", "occurredAt": day("2026-11-10")})
	w.must(map[string]any{"type": "EXPENSE", "walletId": w.a.ID, "amount": "30", "occurredAt": day("2026-11-10")})
	w.must(map[string]any{"type": "EXPENSE", "walletId": idr.ID, "amount": "160000", "occurredAt": day("2026-11-10")}) // 10 USD
	w.must(map[string]any{"type": "EXPENSE", "walletId": w.a.ID, "amount": "7.5", "occurredAt": day("2026-11-11")})
	w.must(map[string]any{"type": "EXPENSE", "walletId": sgd.ID, "amount": "99", "occurredAt": day("2026-11-11")}) // excluded
	w.transfer(w.a, idr, "25", "400000", time.Date(2026, 11, 11, 11, 0, 0, 0, time.UTC))

	sm := w.summary("?types=INCOME,EXPENSE,TRANSFER")
	assert.Equal(t, "USD", sm.Currency)
	require.Len(t, sm.Items, 2)

	d11, d10 := sm.Items[0], sm.Items[1]
	assert.Equal(t, "2026-11-11", d11.Date, "newest day first")
	assert.True(t, dec("0").Equal(dec(d11.Income)))
	assert.True(t, dec("7.5").Equal(dec(d11.Expense)), "the SGD expense has no rate and is left out: %s", d11.Expense)
	assert.True(t, dec("25").Equal(dec(d11.Transfer)), "transfers only in the transfer column")
	assert.Equal(t, 3, d11.Count, "count includes every matching transaction")

	assert.Equal(t, "2026-11-10", d10.Date)
	assert.True(t, dec("100").Equal(dec(d10.Income)))
	assert.True(t, dec("40").Equal(dec(d10.Expense)), "30 USD + 160000 IDR (=10 USD): %s", d10.Expense)
	assert.True(t, dec("0").Equal(dec(d10.Transfer)))
	assert.Equal(t, 3, d10.Count)

	require.Len(t, sm.Warnings, 1)
	assert.Equal(t, "FX_RATE_UNAVAILABLE", sm.Warnings[0].Code)
	assert.Contains(t, sm.Warnings[0].Message, "SGD")

	t.Run("filters apply", func(t *testing.T) {
		sm := w.summary("?types=EXPENSE&walletIds=" + w.a.ID)
		require.Len(t, sm.Items, 2)
		assert.True(t, dec("30").Equal(dec(sm.Items[1].Expense)))
		assert.Empty(t, sm.Warnings)
		sm = w.summary("?from=2026-11-11&to=2026-11-11&types=INCOME")
		assert.Empty(t, sm.Items)
		assert.NotNil(t, sm.Warnings)
	})

	t.Run("a different base currency", func(t *testing.T) {
		require.Equal(t, http.StatusOK, s.Do(http.MethodPatch, "/api/v1/spaces/"+w.sid, map[string]any{"baseCurrency": "IDR"}, bearer(w.u.AccessToken)).Status)
		sm := w.summary("?types=INCOME&from=2026-11-10&to=2026-11-10")
		assert.Equal(t, "IDR", sm.Currency)
		require.Len(t, sm.Items, 1)
		assert.True(t, dec("1600000").Equal(dec(sm.Items[0].Income)), sm.Items[0].Income)
	})

	t.Run("validation", func(t *testing.T) {
		assert.Equal(t, http.StatusBadRequest, s.Get(txURL(w.sid, "/daily-summary")+"?period=NOPE", bearer(w.u.AccessToken)).Status)
	})
}

func TestDailySummaryMatchesTheList(t *testing.T) {
	s := NewTestServer(t)
	w := newWorld(t, s)
	for i, a := range []string{"10.10", "20.20", "30.30", "0.01", "99.99"} {
		typ := "EXPENSE"
		if i%2 == 1 {
			typ = "INCOME"
		}
		w.must(map[string]any{"type": typ, "walletId": w.a.ID, "amount": a, "occurredAt": fmt.Sprintf("2026-11-%02dT09:00:00Z", 10+i%2)})
	}
	list := w.list("?types=INCOME,EXPENSE&limit=100")
	income, expense := dec("0"), dec("0")
	for _, it := range list.Items {
		if it.Type == "INCOME" {
			income = income.Add(dec(it.Amount))
		} else {
			expense = expense.Add(dec(it.Amount))
		}
	}
	si, se, count := dec("0"), dec("0"), 0
	sm := w.summary("?types=INCOME,EXPENSE")
	for _, d := range sm.Items {
		si, se, count = si.Add(dec(d.Income)), se.Add(dec(d.Expense)), count+d.Count
	}
	assert.True(t, income.Equal(si), "income %s vs %s", income, si)
	assert.True(t, expense.Equal(se), "expense %s vs %s", expense, se)
	assert.Equal(t, len(list.Items), count)
}

func TestCategoryStats(t *testing.T) {
	s := NewTestServer(t)
	require.NoError(t, s.Currency.Refresh(context.Background()))
	w := newWorld(t, s)
	s.Clock.Set(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC))
	stats := func(q string) []categoryStatsBody {
		res := s.Get(catURL(w.sid)+"?includeStats=true"+q, bearer(w.u.AccessToken))
		require.Equal(t, http.StatusOK, res.Status, "%s: %s", q, res.Body)
		var out []categoryStatsBody
		res.JSON(t, &out)
		return out
	}
	byName := func(cs []categoryStatsBody, name string) categoryStatsBody {
		for _, c := range cs {
			if c.Name == name {
				return c
			}
		}
		t.Fatalf("category %q not found", name)
		return categoryStatsBody{}
	}

	// a 500 USD monthly budget on Shopping
	require.Equal(t, http.StatusOK, s.Do(http.MethodPatch, catURL(w.sid, "/", w.cat["Shopping"]), map[string]any{"budgetAmount": "500"}, bearer(w.u.AccessToken)).Status)
	b := wal("Rupiah")
	b["currency"] = "IDR"
	idr := s.newWallet(w.u, w.sid, b)

	d := func(day int) string { return fmt.Sprintf("2026-11-%02dT10:00:00Z", day) }
	w.must(map[string]any{"type": "EXPENSE", "walletId": w.a.ID, "amount": "100", "categoryId": w.cat["Shopping"], "occurredAt": d(3)})
	w.must(map[string]any{"type": "EXPENSE", "walletId": idr.ID, "amount": "800000", "categoryId": w.cat["Shopping"], "occurredAt": d(4)}) // 50 USD
	w.must(map[string]any{"type": "EXPENSE", "walletId": w.a.ID, "amount": "999", "categoryId": w.cat["Shopping"], "occurredAt": "2026-10-20T10:00:00Z"})
	w.must(map[string]any{"type": "EXPENSE", "walletId": w.a.ID, "amount": "40", "categoryId": w.cat["Transport"], "occurredAt": d(5)})
	w.must(map[string]any{"type": "INCOME", "walletId": w.a.ID, "amount": "3000", "categoryId": w.cat["Salary"], "occurredAt": d(1)})
	w.must(map[string]any{"type": "INCOME", "walletId": w.a.ID, "amount": "10", "occurredAt": d(2)}) // uncategorized

	cs := stats("")
	require.Len(t, cs, 11)
	shop := byName(cs, "Shopping")
	require.NotNil(t, shop.Stats)
	assert.Equal(t, "USD", shop.Stats.Currency)
	assert.True(t, dec("150").Equal(dec(*shop.Stats.Spent)), "this period only, converted: %s", *shop.Stats.Spent)
	assert.True(t, dec("350").Equal(dec(*shop.Stats.BudgetRemaining)))
	assert.True(t, dec("30").Equal(dec(*shop.Stats.BudgetUsedPct)), *shop.Stats.BudgetUsedPct)
	assert.Nil(t, shop.Stats.Received)

	tr := byName(cs, "Transport")
	assert.True(t, dec("40").Equal(dec(*tr.Stats.Spent)))
	assert.Nil(t, tr.Stats.BudgetRemaining, "no budget, no remaining")
	assert.Nil(t, tr.Stats.BudgetUsedPct)
	assert.True(t, dec("0").Equal(dec(*byName(cs, "Health").Stats.Spent)))

	sal := byName(cs, "Salary")
	assert.True(t, dec("3000").Equal(dec(*sal.Stats.Received)))
	assert.Nil(t, sal.Stats.Spent)

	t.Run("period and type filters", func(t *testing.T) {
		all := byName(stats("&period=ALL_TIME"), "Shopping")
		assert.True(t, dec("1149").Equal(dec(*all.Stats.Spent)))
		assert.True(t, dec("-649").Equal(dec(*all.Stats.BudgetRemaining)), "an overspent budget goes negative")
		assert.True(t, dec("229.8").Equal(dec(*all.Stats.BudgetUsedPct)))

		onlyExpense := stats("&type=EXPENSE")
		assert.Len(t, onlyExpense, 7)
		week := byName(stats("&period=THIS_WEEK"), "Shopping") // 16–22 Nov: nothing
		assert.True(t, dec("0").Equal(dec(*week.Stats.Spent)))
		custom := byName(stats("&from=2026-11-04&to=2026-11-04"), "Shopping")
		assert.True(t, dec("50").Equal(dec(*custom.Stats.Spent)))

		for _, q := range []string{"&period=NOPE", "&from=x", "&type=OTHER"} {
			assert.Equal(t, http.StatusBadRequest, s.Get(catURL(w.sid)+"?includeStats=true"+q, bearer(w.u.AccessToken)).Status, q)
		}
		assert.Equal(t, http.StatusBadRequest, s.Get(catURL(w.sid)+"?includeStats=maybe", bearer(w.u.AccessToken)).Status)
	})

	t.Run("plain list has no stats", func(t *testing.T) {
		for _, c := range s.categories(w.u, w.sid, "") {
			_ = c
		}
		res := s.Get(catURL(w.sid), bearer(w.u.AccessToken))
		assert.NotContains(t, string(res.Body), `"stats"`)
	})

	t.Run("categories used by transactions lock their type", func(t *testing.T) {
		res := s.Do(http.MethodPatch, catURL(w.sid, "/", w.cat["Transport"]), map[string]any{"type": "INCOME"}, bearer(w.u.AccessToken))
		require.Equal(t, http.StatusUnprocessableEntity, res.Status, string(res.Body))
		assert.Equal(t, "CATEGORY_TYPE_LOCKED", errCode(t, res))
		// an unused category can still change type
		res = s.Do(http.MethodPatch, catURL(w.sid, "/", w.cat["Family"]), map[string]any{"type": "INCOME"}, bearer(w.u.AccessToken))
		assert.Equal(t, http.StatusOK, res.Status, string(res.Body))
	})

	t.Run("a missing FX rate is a warning on that category", func(t *testing.T) {
		sg := wal("Singapore")
		sg["currency"] = "SGD"
		sgd := s.newWallet(w.u, w.sid, sg)
		w.must(map[string]any{"type": "EXPENSE", "walletId": sgd.ID, "amount": "10", "categoryId": w.cat["Gifts"], "occurredAt": d(6)})
		gifts := byName(stats(""), "Gifts")
		assert.True(t, dec("0").Equal(dec(*gifts.Stats.Spent)))
		require.NotEmpty(t, gifts.Stats.Warnings)
		assert.Equal(t, "FX_RATE_UNAVAILABLE", gifts.Stats.Warnings[0].Code)
	})
}

type categoryStatsBody struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Type  string `json:"type"`
	Stats *struct {
		Currency        string                           `json:"currency"`
		Spent           *string                          `json:"spent"`
		BudgetRemaining *string                          `json:"budgetRemaining"`
		BudgetUsedPct   *string                          `json:"budgetUsedPct"`
		Received        *string                          `json:"received"`
		Warnings        []struct{ Code, Message string } `json:"warnings"`
	} `json:"stats"`
}
