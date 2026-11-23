//go:build e2e

package e2e

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A top up must not count as income anywhere: not in the overview, the
// calendar, the analytics buckets or the "vs previous period" change.
func TestTopUpIsNotIncome(t *testing.T) {
	s := NewTestServer(t)
	e := newDashEnv(t, s)
	e.tx("INCOME", e.a, "100", e.cat["Salary"], "2026-11-02T09:00:00Z")
	res := s.Do(http.MethodPost, walURL(e.sid, "/", e.a.ID, "/top-up"), map[string]any{"amount": "500"}, bearer(e.u.AccessToken))
	require.Equal(t, http.StatusCreated, res.Status, string(res.Body))

	o := e.overview("?period=THIS_PERIOD")
	assert.True(t, dec("100").Equal(dec(o.Income)), "only the real income: %s", o.Income)

	var c calendarBody
	e.get("calendar", "?month=2026-11", &c)
	total := dec("0")
	for _, d := range c.Days {
		total = total.Add(dec(d.Income))
	}
	assert.True(t, dec("100").Equal(total), "calendar income %s", total)

	var a analyticsBody
	e.get("analytics", "?period=THIS_PERIOD", &a)
	total = dec("0")
	for _, b := range a.Buckets {
		total = total.Add(dec(b.Income))
	}
	assert.True(t, dec("100").Equal(total), "analytics income %s", total)
	assert.True(t, dec("1600").Equal(e.balance(e.a)), "the balance still rises: %s", e.balance(e.a))
}

// Only INCOME and EXPENSE enter the calendar and the analytics buckets: transfers,
// adjustments and opening balances never do, even on the same day.
func TestCalendarAndAnalyticsIgnoreOtherTypes(t *testing.T) {
	s := NewTestServer(t)
	e := newDashEnv(t, s)
	e.tx("INCOME", e.a, "100", "", "2026-11-05T09:00:00Z")
	e.tx("EXPENSE", e.a, "30", "", "2026-11-05T10:00:00Z")
	e.transfer(e.a, e.b, "70", "70", time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC))
	require.Equal(t, http.StatusOK, s.Do(http.MethodPut, walURL(e.sid, "/", e.b.ID, "/balance"), map[string]any{"newBalance": "9999"}, bearer(e.u.AccessToken)).Status)
	require.Equal(t, http.StatusCreated, s.Do(http.MethodPost, walURL(e.sid, "/", e.a.ID, "/top-up"), map[string]any{"amount": "500"}, bearer(e.u.AccessToken)).Status)

	var c calendarBody
	e.get("calendar", "?month=2026-11", &c)
	d5 := c.Days[4]
	assert.True(t, dec("100").Equal(dec(d5.Income)))
	assert.True(t, dec("30").Equal(dec(d5.Expense)))
	assert.True(t, dec("70").Equal(dec(d5.Net)))
	assert.Equal(t, 2, d5.Count)
	for i, d := range c.Days {
		if i == 4 {
			continue
		}
		assert.True(t, dec("0").Equal(dec(d.Income)) && dec("0").Equal(dec(d.Expense)) && d.Count == 0, "day %s is empty", d.Date)
	}

	var a analyticsBody
	e.get("analytics", "?period=THIS_PERIOD", &a)
	inc, exp := dec("0"), dec("0")
	for _, b := range a.Buckets {
		inc, exp = inc.Add(dec(b.Income)), exp.Add(dec(b.Expense))
	}
	assert.True(t, dec("100").Equal(inc), "income %s", inc)
	assert.True(t, dec("30").Equal(exp), "expense %s", exp)
}

func (w *world) patch(id string, body map[string]any) Response {
	return w.s.Do(http.MethodPatch, txURL(w.sid, "/", id), body, bearer(w.u.AccessToken))
}

// An uncategorised income or expense can be turned into an adjustment explicitly,
// and nothing else about the type may be changed this way.
func TestReclassifyToAdjustment(t *testing.T) {
	s := NewTestServer(t)
	w := newWorld(t, s)

	t.Run("an uncategorised income becomes ADJUSTMENT IN and the balance is unchanged", func(t *testing.T) {
		tx := w.must(map[string]any{"type": "INCOME", "walletId": w.a.ID, "amount": "15.67", "title": "Saldo awal"})
		before := w.balance(w.a)
		res := w.patch(tx.ID, map[string]any{"type": "ADJUSTMENT"})
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		var got txnBody
		res.JSON(t, &got)
		assert.Equal(t, "ADJUSTMENT", got.Type)
		require.NotNil(t, got.Direction)
		assert.Equal(t, "IN", *got.Direction)
		assert.Nil(t, got.SystemKey, "a user transaction keeps no system key")
		assert.True(t, before.Equal(w.balance(w.a)))
	})

	t.Run("an uncategorised expense becomes ADJUSTMENT OUT", func(t *testing.T) {
		tx := w.must(map[string]any{"type": "EXPENSE", "walletId": w.a.ID, "amount": "4"})
		before := w.balance(w.a)
		res := w.patch(tx.ID, map[string]any{"type": "ADJUSTMENT"})
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		var got txnBody
		res.JSON(t, &got)
		require.NotNil(t, got.Direction)
		assert.Equal(t, "OUT", *got.Direction)
		assert.True(t, before.Equal(w.balance(w.a)))
	})

	t.Run("a categorised transaction is refused", func(t *testing.T) {
		tx := w.must(map[string]any{"type": "INCOME", "walletId": w.a.ID, "amount": "5", "categoryId": w.cat["Salary"]})
		res := w.patch(tx.ID, map[string]any{"type": "ADJUSTMENT"})
		require.Equal(t, http.StatusUnprocessableEntity, res.Status)
		assert.Equal(t, "TYPE_CHANGE_NOT_ALLOWED", errCode(t, res))
	})

	t.Run("an adjustment cannot be changed again", func(t *testing.T) {
		tx := w.must(map[string]any{"type": "INCOME", "walletId": w.a.ID, "amount": "5"})
		require.Equal(t, http.StatusOK, w.patch(tx.ID, map[string]any{"type": "ADJUSTMENT"}).Status)
		for _, typ := range []string{"ADJUSTMENT", "INCOME"} {
			res := w.patch(tx.ID, map[string]any{"type": typ})
			require.Equal(t, http.StatusUnprocessableEntity, res.Status, typ)
			assert.Equal(t, "TYPE_CHANGE_NOT_ALLOWED", errCode(t, res))
		}
	})

	t.Run("other targets are refused", func(t *testing.T) {
		tx := w.must(map[string]any{"type": "INCOME", "walletId": w.a.ID, "amount": "5"})
		for _, typ := range []string{"TRANSFER", "OPENING_BALANCE"} {
			res := w.patch(tx.ID, map[string]any{"type": typ})
			require.Equal(t, http.StatusUnprocessableEntity, res.Status, typ)
			assert.Equal(t, "TYPE_CHANGE_NOT_ALLOWED", errCode(t, res))
		}
		assert.Equal(t, http.StatusBadRequest, w.patch(tx.ID, map[string]any{"type": "NOPE"}).Status)
	})

	t.Run("a transfer keeps using its own endpoint", func(t *testing.T) {
		id := w.transfer(w.a, w.b, "3", "3", time.Now())
		res := w.patch(id, map[string]any{"type": "ADJUSTMENT"})
		require.Equal(t, http.StatusUnprocessableEntity, res.Status)
		assert.Equal(t, "USE_SPECIFIC_ENDPOINT", errCode(t, res))
	})

	t.Run("it cannot be mixed with other edits", func(t *testing.T) {
		tx := w.must(map[string]any{"type": "INCOME", "walletId": w.a.ID, "amount": "5"})
		assert.Equal(t, http.StatusBadRequest, w.patch(tx.ID, map[string]any{"type": "ADJUSTMENT", "amount": "6"}).Status)
	})
}

// The three kinds of transaction the backend writes carry a system key.
func TestSystemKeys(t *testing.T) {
	s := NewTestServer(t)
	w := newWorld(t, s)
	top := s.Do(http.MethodPost, walURL(w.sid, "/", w.a.ID, "/top-up"), map[string]any{"amount": "1"}, bearer(w.u.AccessToken))
	require.Equal(t, http.StatusCreated, top.Status)
	require.Equal(t, http.StatusOK, s.Do(http.MethodPut, walURL(w.sid, "/", w.a.ID, "/balance"), map[string]any{"newBalance": "5000"}, bearer(w.u.AccessToken)).Status)
	user := w.must(map[string]any{"type": "EXPENSE", "walletId": w.a.ID, "amount": "2"})
	require.Nil(t, user.SystemKey)

	keys := map[string]int{}
	for _, it := range w.list("").Items {
		if it.SystemKey != nil {
			keys[*it.SystemKey]++
		}
	}
	assert.Equal(t, 1, keys["BALANCE_TOP_UP"])
	assert.Equal(t, 1, keys["BALANCE_ADJUSTMENT"])
	assert.Equal(t, 2, keys["OPENING_BALANCE"], "one per wallet of the world")
}

// The default categories carry a stable key, and a name that means the same as a category the
// space already has is refused unless the client forces it.
func TestCategorySystemKeysAndSimilarNames(t *testing.T) {
	s := NewTestServer(t)
	u := s.register("jane@example.com")
	sid := spaceIDOf(u)

	byName := map[string]categoryBody{}
	keys := map[string]bool{}
	for _, c := range s.categories(u, sid, "") {
		byName[c.Name] = c
		require.NotNil(t, c.SystemKey, c.Name)
		keys[*c.SystemKey] = true
	}
	assert.Len(t, keys, 11, "one distinct key per default category")
	assert.Equal(t, "SALARY", *byName["Salary"].SystemKey)

	t.Run("an alias of an existing category is refused with its id", func(t *testing.T) {
		res := s.newCategory(u, sid, cat("INCOME", "Gaji"))
		require.Equal(t, http.StatusConflict, res.Status, string(res.Body))
		assert.Equal(t, "CATEGORY_EXISTS_SIMILAR", errCode(t, res))
		var env struct {
			Error struct {
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		res.JSON(t, &env)
		assert.Equal(t, byName["Salary"].ID, env.Error.Details["existingId"])
	})

	t.Run("force overrides it and the new one has no system key", func(t *testing.T) {
		res := s.Do(http.MethodPost, catURL(sid)+"?force=true", cat("INCOME", "Gaji"), bearer(u.AccessToken))
		require.Equal(t, http.StatusCreated, res.Status, string(res.Body))
		var c categoryBody
		res.JSON(t, &c)
		assert.Nil(t, c.SystemKey)
	})

	t.Run("the same alias on the other type is fine", func(t *testing.T) {
		assert.Equal(t, http.StatusCreated, s.newCategory(u, sid, cat("EXPENSE", "Gaji")).Status)
	})

	t.Run("an exact duplicate is still CATEGORY_NAME_TAKEN, force or not", func(t *testing.T) {
		res := s.Do(http.MethodPost, catURL(sid)+"?force=true", cat("INCOME", "salary"), bearer(u.AccessToken))
		assert.Equal(t, "CATEGORY_NAME_TAKEN", errCode(t, res))
	})

	t.Run("a rename to an alias is checked too, and a rename keeps the key", func(t *testing.T) {
		pay := s.newCategory(u, sid, cat("INCOME", "Bonus"))
		require.Equal(t, http.StatusCreated, pay.Status)
		var b categoryBody
		pay.JSON(t, &b)
		res := s.Do(http.MethodPatch, catURL(sid, "/", b.ID), map[string]any{"name": "Pendapatan"}, bearer(u.AccessToken))
		require.Equal(t, http.StatusConflict, res.Status, string(res.Body))
		assert.Equal(t, "CATEGORY_EXISTS_SIMILAR", errCode(t, res))

		res = s.Do(http.MethodPatch, catURL(sid, "/", byName["Salary"].ID), map[string]any{"name": "Paycheck"}, bearer(u.AccessToken))
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		var renamed categoryBody
		res.JSON(t, &renamed)
		assert.Equal(t, "SALARY", *renamed.SystemKey, "a user rename never changes the key")
	})
}
