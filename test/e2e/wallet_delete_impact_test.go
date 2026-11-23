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

type deleteImpactBody struct {
	Transactions int    `json:"transactions"`
	Transfers    int    `json:"transfers"`
	GoalReserves int    `json:"goalReserves"`
	Balance      string `json:"balance"`
	Currency     string `json:"currency"`
}

// I-T1/I-T2 (T7): the client can ask what a wallet delete takes with it, and a delete of a wallet with
// history needs force=true (folded into the existing force rule, plan correction C4).
func TestWalletDeleteImpactAndGuard(t *testing.T) {
	s := NewTestServer(t)
	w := newWorld(t, s) // wallets a (1000) and b (500), one opening balance each
	impactURL := func(id string) string { return walURL(w.sid, "/", id, "/delete-impact") }
	impact := func(id string) deleteImpactBody {
		res := s.Get(impactURL(id), bearer(w.u.AccessToken))
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		var b deleteImpactBody
		res.JSON(t, &b)
		return b
	}
	del := func(id, q string) Response {
		return s.Do(http.MethodDelete, walURL(w.sid, "/", id)+q, nil, bearer(w.u.AccessToken))
	}

	t.Run("counts the wallet's transactions and transfers (both directions)", func(t *testing.T) {
		w.must(map[string]any{"type": "EXPENSE", "walletId": w.a.ID, "amount": "10"})
		w.must(map[string]any{"type": "INCOME", "walletId": w.a.ID, "amount": "5"})
		w.transfer(w.a, w.b, "20", "20", time.Now())
		w.transfer(w.b, w.a, "7", "7", time.Now())
		got := impact(w.a.ID)
		assert.Equal(t, 5, got.Transactions, "opening balance, expense, income and two transfers")
		assert.Equal(t, 2, got.Transfers)
		assert.Equal(t, 0, got.GoalReserves)
		assert.Equal(t, "USD", got.Currency)
		assert.True(t, dec("1000").Add(dec("-10")).Add(dec("5")).Add(dec("-20")).Add(dec("7")).Equal(dec(got.Balance)), got.Balance)
		assert.Equal(t, 3, impact(w.b.ID).Transactions, "opening balance and the two transfers")
	})

	t.Run("goal reserves are counted", func(t *testing.T) {
		before := impact(w.b.ID).GoalReserves
		_, err := s.Tx.Exec(context.Background(), `
			INSERT INTO goals (space_id, name, icon, color, target_amount, currency, created_by_id) VALUES ($1, 'Trip', 'x', '#112233', 1000, 'USD', $2)`, w.sid, w.u.User.ID)
		require.NoError(t, err)
		_, err = s.Tx.Exec(context.Background(), `
			INSERT INTO goal_allocations (goal_id, wallet_id, amount) SELECT id, $2, 50 FROM goals WHERE space_id = $1 AND name = 'Trip'`, w.sid, w.b.ID)
		require.NoError(t, err)
		assert.Equal(t, before+1, impact(w.b.ID).GoalReserves)
	})

	t.Run("impact of a wallet with no history is all zero", func(t *testing.T) {
		e := s.newWallet(w.u, w.sid, wal("Empty"))
		got := impact(e.ID)
		assert.Equal(t, 0, got.Transactions)
		assert.Equal(t, 0, got.Transfers)
		assert.True(t, dec("0").Equal(dec(got.Balance)))
	})

	t.Run("unknown, malformed and other-space wallets are 404", func(t *testing.T) {
		assert.Equal(t, http.StatusNotFound, s.Get(impactURL("nope"), bearer(w.u.AccessToken)).Status)
		assert.Equal(t, http.StatusNotFound, s.Get(impactURL("00000000-0000-0000-0000-000000000000"), bearer(w.u.AccessToken)).Status)
		outsider := s.register("outsider@example.com")
		expectCrossSpace404(t, s, outsider, http.MethodGet, impactURL(w.a.ID), nil)
		assert.Equal(t, http.StatusUnauthorized, s.Get(impactURL(w.a.ID)).Status)
	})

	t.Run("a wallet with history and a zero balance needs force, with the counts in the error", func(t *testing.T) {
		z := s.newWallet(w.u, w.sid, wal("Zero"))
		require.Equal(t, http.StatusCreated, s.Do(http.MethodPost, walURL(w.sid, "/", z.ID, "/top-up"), map[string]any{"amount": "30"}, bearer(w.u.AccessToken)).Status)
		w.must(map[string]any{"type": "EXPENSE", "walletId": z.ID, "amount": "30"})
		require.True(t, dec("0").Equal(w.balance(z)))

		res := del(z.ID, "")
		require.Equal(t, http.StatusConflict, res.Status, string(res.Body))
		assert.Equal(t, "WALLET_HAS_TRANSACTIONS", errCode(t, res))
		var env struct {
			Error struct {
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		res.JSON(t, &env)
		assert.EqualValues(t, 2, env.Error.Details["transactions"])
		assert.EqualValues(t, 0, env.Error.Details["transfers"])
		assert.Equal(t, http.StatusConflict, del(z.ID, "?force=false").Status)

		require.Equal(t, http.StatusNoContent, del(z.ID, "?force=true").Status)
		assert.Equal(t, http.StatusNotFound, del(z.ID, "?force=true").Status)
	})

	t.Run("a wallet with a balance still answers WALLET_HAS_BALANCE", func(t *testing.T) {
		res := del(w.a.ID, "")
		require.Equal(t, http.StatusUnprocessableEntity, res.Status)
		assert.Equal(t, "WALLET_HAS_BALANCE", errCode(t, res))
	})

	t.Run("a wallet with no history deletes without force", func(t *testing.T) {
		e := s.newWallet(w.u, w.sid, wal("Never used"))
		assert.Equal(t, http.StatusNoContent, del(e.ID, "").Status)
	})
}
