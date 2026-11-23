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

type categoryBody struct {
	ID             string  `json:"id"`
	Type           string  `json:"type"`
	Name           string  `json:"name"`
	Icon           string  `json:"icon"`
	Color          string  `json:"color"`
	BudgetAmount   *string `json:"budgetAmount"`
	BudgetCurrency *string `json:"budgetCurrency"`
	SortOrder      int     `json:"sortOrder"`
	SystemKey      *string `json:"systemKey"`
}

func catURL(sid string, rest ...string) string {
	return "/api/v1/spaces/" + sid + "/categories" + strings.Join(rest, "")
}

func (s *TestServer) categories(u session, sid, query string) []categoryBody {
	s.t.Helper()
	res := s.Get(catURL(sid)+query, bearer(u.AccessToken))
	require.Equal(s.t, http.StatusOK, res.Status, string(res.Body))
	var out []categoryBody
	res.JSON(s.t, &out)
	return out
}

func names(cs []categoryBody) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}
	return out
}

func (s *TestServer) newCategory(u session, sid string, body map[string]any) Response {
	return s.Do(http.MethodPost, catURL(sid), body, bearer(u.AccessToken))
}

func cat(typ, name string) map[string]any {
	return map[string]any{"type": typ, "name": name, "icon": "🧪", "color": "#112233"}
}

func TestDefaultCategories(t *testing.T) {
	s := NewTestServer(t)
	u := s.register("jane@example.com")
	sid := spaceIDOf(u)

	all := s.categories(u, sid, "")
	assert.Len(t, all, 11)

	exp := s.categories(u, sid, "?type=EXPENSE")
	assert.Equal(t, []string{"Transport", "Shopping", "Subscriptions", "Health", "Gifts", "Snacks", "Family"}, names(exp))
	assert.Equal(t, "🚗", exp[0].Icon)
	assert.Equal(t, "👨‍👩‍👧", exp[6].Icon)
	inc := s.categories(u, sid, "?type=INCOME")
	assert.Equal(t, []string{"Salary", "Freelance", "Investment", "Dividend"}, names(inc))
	for i, c := range exp {
		assert.Equal(t, i, c.SortOrder)
		assert.Nil(t, c.BudgetAmount)
		assert.Regexp(t, `^#[0-9A-F]{6}$`, c.Color)
	}

	t.Run("a space created later is seeded too", func(t *testing.T) {
		res := s.Do(http.MethodPost, "/api/v1/spaces", map[string]any{"name": "Second"}, bearer(u.AccessToken))
		require.Equal(t, http.StatusCreated, res.Status)
		var sp spaceBody
		res.JSON(t, &sp)
		assert.Len(t, s.categories(u, sp.ID, ""), 11)
		assert.Len(t, s.categories(u, sid, ""), 11, "the first space is untouched")
	})

	t.Run("bad type filter", func(t *testing.T) {
		assert.Equal(t, http.StatusBadRequest, s.Get(catURL(sid)+"?type=NOPE", bearer(u.AccessToken)).Status)
	})
}

func TestCategoryCRUD(t *testing.T) {
	s := NewTestServer(t)
	u := s.register("jane@example.com")
	sid := spaceIDOf(u)

	t.Run("unauthenticated", func(t *testing.T) {
		assert.Equal(t, http.StatusUnauthorized, s.Get(catURL(sid)).Status)
		assert.Equal(t, http.StatusUnauthorized, s.Do(http.MethodPost, catURL(sid), cat("EXPENSE", "x")).Status)
		assert.Equal(t, http.StatusUnauthorized, s.Get("/api/v1/categories/suggestions?type=INCOME").Status)
	})

	var coffee categoryBody
	t.Run("create appends and normalizes", func(t *testing.T) {
		body := cat("EXPENSE", "  Coffee ")
		body["color"] = "#aa00bb"
		body["budgetAmount"] = "250000.50"
		res := s.newCategory(u, sid, body)
		require.Equal(t, http.StatusCreated, res.Status, string(res.Body))
		res.JSON(t, &coffee)
		assert.Equal(t, "Coffee", coffee.Name)
		assert.Equal(t, "#AA00BB", coffee.Color)
		assert.Equal(t, 7, coffee.SortOrder, "after the 7 seeded expense categories")
		require.NotNil(t, coffee.BudgetAmount)
		assert.Equal(t, "250000.5", *coffee.BudgetAmount)
		require.NotNil(t, coffee.BudgetCurrency)
		assert.Equal(t, "USD", *coffee.BudgetCurrency, "budgets are in the space base currency")
	})

	t.Run("create validation", func(t *testing.T) {
		mk := func(mod func(m map[string]any)) map[string]any { m := cat("EXPENSE", "Valid"); mod(m); return m }
		for name, body := range map[string]map[string]any{
			"missing name":  mk(func(m map[string]any) { delete(m, "name") }),
			"blank name":    mk(func(m map[string]any) { m["name"] = "   " }),
			"long name":     mk(func(m map[string]any) { m["name"] = strings.Repeat("x", 31) }),
			"bad type":      mk(func(m map[string]any) { m["type"] = "TRANSFER" }),
			"missing icon":  mk(func(m map[string]any) { delete(m, "icon") }),
			"bad color":     mk(func(m map[string]any) { m["color"] = "blue" }),
			"zero budget":   mk(func(m map[string]any) { m["budgetAmount"] = "0" }),
			"negative":      mk(func(m map[string]any) { m["budgetAmount"] = "-5" }),
			"non-numeric":   mk(func(m map[string]any) { m["budgetAmount"] = "lots" }),
			"unknown field": mk(func(m map[string]any) { m["sortOrder"] = 3 }),
		} {
			res := s.newCategory(u, sid, body)
			assert.Equal(t, http.StatusBadRequest, res.Status, "%s: %s", name, res.Body)
		}
		assert.Equal(t, 30, len([]rune(strings.Repeat("x", 30))))
		assert.Equal(t, http.StatusCreated, s.newCategory(u, sid, cat("EXPENSE", strings.Repeat("y", 30))).Status, "30 chars is allowed")
	})

	t.Run("names are unique per type, case-insensitively", func(t *testing.T) {
		res := s.newCategory(u, sid, cat("EXPENSE", "COFFEE"))
		require.Equal(t, http.StatusConflict, res.Status)
		assert.Equal(t, "CATEGORY_NAME_TAKEN", errCode(t, res))
		res = s.newCategory(u, sid, cat("EXPENSE", "transport"))
		assert.Equal(t, "CATEGORY_NAME_TAKEN", errCode(t, res), "also against seeded names")
		// the same name is fine under the other type
		assert.Equal(t, http.StatusCreated, s.newCategory(u, sid, cat("INCOME", "Coffee")).Status)
		// and in another space
		other := s.Do(http.MethodPost, "/api/v1/spaces", map[string]any{"name": "Other"}, bearer(u.AccessToken))
		var sp spaceBody
		other.JSON(t, &sp)
		assert.Equal(t, http.StatusCreated, s.newCategory(u, sp.ID, cat("EXPENSE", "Coffee")).Status)
		// the failed insert must not poison the connection
		assert.Len(t, s.categories(u, sid, "?type=INCOME"), 5)
	})

	t.Run("income categories cannot have a budget", func(t *testing.T) {
		body := cat("INCOME", "Side gig")
		body["budgetAmount"] = "100"
		res := s.newCategory(u, sid, body)
		require.Equal(t, http.StatusUnprocessableEntity, res.Status)
		assert.Equal(t, "BUDGET_NOT_ALLOWED", errCode(t, res))
	})

	t.Run("patch is partial; null clears the budget", func(t *testing.T) {
		res := s.Do(http.MethodPatch, catURL(sid, "/", coffee.ID), map[string]any{"name": "Espresso", "budgetAmount": "300"}, bearer(u.AccessToken))
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		var c categoryBody
		res.JSON(t, &c)
		assert.Equal(t, "Espresso", c.Name)
		assert.Equal(t, "#AA00BB", c.Color, "untouched")
		assert.Equal(t, "300", *c.BudgetAmount)

		res = s.Do(http.MethodPatch, catURL(sid, "/", coffee.ID), map[string]any{"icon": "☕"}, bearer(u.AccessToken))
		res.JSON(t, &c)
		assert.Equal(t, "☕", c.Icon)
		require.NotNil(t, c.BudgetAmount, "budget kept when not sent")

		res = s.Do(http.MethodPatch, catURL(sid, "/", coffee.ID), map[string]any{"budgetAmount": nil}, bearer(u.AccessToken))
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		res.JSON(t, &c)
		assert.Nil(t, c.BudgetAmount)
		assert.Nil(t, c.BudgetCurrency)
	})

	t.Run("patch rules", func(t *testing.T) {
		p := func(id string, body map[string]any) Response {
			return s.Do(http.MethodPatch, catURL(sid, "/", id), body, bearer(u.AccessToken))
		}
		assert.Equal(t, http.StatusBadRequest, p(coffee.ID, map[string]any{"name": " "}).Status)
		assert.Equal(t, http.StatusBadRequest, p(coffee.ID, map[string]any{"color": "red"}).Status)
		assert.Equal(t, http.StatusBadRequest, p(coffee.ID, map[string]any{"budgetAmount": "-1"}).Status)
		assert.Equal(t, http.StatusBadRequest, p(coffee.ID, map[string]any{"type": "NOPE"}).Status)
		res := p(coffee.ID, map[string]any{"name": "transport"})
		assert.Equal(t, "CATEGORY_NAME_TAKEN", errCode(t, res))
		assert.Equal(t, http.StatusNotFound, p(uuid.NewString(), map[string]any{"name": "x"}).Status)
		assert.Equal(t, http.StatusNotFound, p("not-a-uuid", map[string]any{"name": "x"}).Status)

		// renaming to its own name (different case) is allowed
		assert.Equal(t, http.StatusOK, p(coffee.ID, map[string]any{"name": "ESPRESSO"}).Status)
	})

	t.Run("type change moves a category and drops its budget", func(t *testing.T) {
		res := s.newCategory(u, sid, map[string]any{"type": "EXPENSE", "name": "Odd jobs", "icon": "🧰", "color": "#010203", "budgetAmount": "50"})
		var c categoryBody
		res.JSON(t, &c)
		res = s.Do(http.MethodPatch, catURL(sid, "/", c.ID), map[string]any{"type": "INCOME"}, bearer(u.AccessToken))
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		res.JSON(t, &c)
		assert.Equal(t, "INCOME", c.Type)
		assert.Nil(t, c.BudgetAmount)

		res = s.Do(http.MethodPatch, catURL(sid, "/", c.ID), map[string]any{"budgetAmount": "5"}, bearer(u.AccessToken))
		require.Equal(t, http.StatusUnprocessableEntity, res.Status)
		assert.Equal(t, "BUDGET_NOT_ALLOWED", errCode(t, res))
		res = s.Do(http.MethodPatch, catURL(sid, "/", coffee.ID), map[string]any{"type": "INCOME", "budgetAmount": "5"}, bearer(u.AccessToken))
		assert.Equal(t, "BUDGET_NOT_ALLOWED", errCode(t, res))
	})

	t.Run("type is locked once transactions use the category", func(t *testing.T) {
		s.Categories.SetUsageChecker(func(_ context.Context, _, id uuid.UUID) (bool, error) { return id.String() == coffee.ID, nil })
		defer s.Categories.SetUsageChecker(func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return false, nil })

		res := s.Do(http.MethodPatch, catURL(sid, "/", coffee.ID), map[string]any{"type": "INCOME"}, bearer(u.AccessToken))
		require.Equal(t, http.StatusUnprocessableEntity, res.Status)
		assert.Equal(t, "CATEGORY_TYPE_LOCKED", errCode(t, res))
		// other fields stay editable, and sending the unchanged type is fine
		assert.Equal(t, http.StatusOK, s.Do(http.MethodPatch, catURL(sid, "/", coffee.ID), map[string]any{"type": "EXPENSE", "name": "Flat white"}, bearer(u.AccessToken)).Status)
	})

	t.Run("delete is soft and frees the name", func(t *testing.T) {
		res := s.Do(http.MethodDelete, catURL(sid, "/", coffee.ID), nil, bearer(u.AccessToken))
		require.Equal(t, http.StatusNoContent, res.Status)
		assert.NotContains(t, names(s.categories(u, sid, "")), "Flat white")

		var deleted *string
		require.NoError(t, s.Tx.QueryRow(context.Background(),
			`SELECT deleted_at::text FROM categories WHERE id = $1`, coffee.ID).Scan(&deleted), "row is kept for history")
		assert.NotNil(t, deleted)

		assert.Equal(t, http.StatusNotFound, s.Do(http.MethodDelete, catURL(sid, "/", coffee.ID), nil, bearer(u.AccessToken)).Status)
		assert.Equal(t, http.StatusNotFound, s.Do(http.MethodPatch, catURL(sid, "/", coffee.ID), map[string]any{"name": "x"}, bearer(u.AccessToken)).Status)
		// a deleted name can be reused
		assert.Equal(t, http.StatusCreated, s.newCategory(u, sid, cat("EXPENSE", "Flat white")).Status)
	})
}

func TestCategoryReorder(t *testing.T) {
	s := NewTestServer(t)
	u := s.register("jane@example.com")
	sid := spaceIDOf(u)
	exp := s.categories(u, sid, "?type=EXPENSE")
	inc := s.categories(u, sid, "?type=INCOME")

	ids := func(cs []categoryBody) []string {
		out := make([]string, len(cs))
		for i, c := range cs {
			out[i] = c.ID
		}
		return out
	}
	reorder := func(typ string, order []string) Response {
		return s.Do(http.MethodPut, catURL(sid, "/order"), map[string]any{"type": typ, "ids": order}, bearer(u.AccessToken))
	}

	rev := ids(exp)
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	res := reorder("EXPENSE", rev)
	require.Equal(t, http.StatusOK, res.Status, string(res.Body))
	var got []categoryBody
	res.JSON(t, &got)
	assert.Equal(t, rev, ids(got))
	assert.Equal(t, rev, ids(s.categories(u, sid, "?type=EXPENSE")), "order persists")
	assert.Equal(t, ids(inc), ids(s.categories(u, sid, "?type=INCOME")), "other type untouched")

	t.Run("must list every category once", func(t *testing.T) {
		assert.Equal(t, http.StatusBadRequest, reorder("EXPENSE", rev[1:]).Status, "missing one")
		assert.Equal(t, http.StatusBadRequest, reorder("EXPENSE", append(append([]string{}, rev...), rev[0])).Status, "duplicate")
		assert.Equal(t, http.StatusBadRequest, reorder("EXPENSE", ids(inc)).Status, "wrong type's ids")
		assert.Equal(t, http.StatusBadRequest, reorder("EXPENSE", append(append([]string{}, rev[1:]...), uuid.NewString())).Status, "unknown id")
		assert.Equal(t, http.StatusBadRequest, reorder("BAD", rev).Status)
		assert.Equal(t, rev, ids(s.categories(u, sid, "?type=EXPENSE")), "failed reorder changes nothing")
	})

	t.Run("new categories go last; deleted ones are not required", func(t *testing.T) {
		require.Equal(t, http.StatusNoContent, s.Do(http.MethodDelete, catURL(sid, "/", rev[0]), nil, bearer(u.AccessToken)).Status)
		res := s.newCategory(u, sid, cat("EXPENSE", "Newest"))
		var c categoryBody
		res.JSON(t, &c)
		after := s.categories(u, sid, "?type=EXPENSE")
		assert.Equal(t, c.ID, after[len(after)-1].ID)
		assert.Equal(t, http.StatusOK, reorder("EXPENSE", ids(after)).Status)
	})
}

func TestCategoriesAreSpaceScoped(t *testing.T) {
	s := NewTestServer(t)
	owner := s.register("owner@example.com")
	member := s.register("member@example.com")
	outsider := s.register("outsider@example.com")
	sid := spaceIDOf(owner)
	s.join(owner, sid, member)
	someCat := s.categories(owner, sid, "")[0].ID

	t.Run("outsider gets 404 on every route", func(t *testing.T) {
		expectCrossSpace404(t, s, outsider, http.MethodGet, catURL(sid), nil)
		expectCrossSpace404(t, s, outsider, http.MethodPost, catURL(sid), cat("EXPENSE", "Hack"))
		expectCrossSpace404(t, s, outsider, http.MethodPatch, catURL(sid, "/", someCat), map[string]any{"name": "Hack"})
		expectCrossSpace404(t, s, outsider, http.MethodDelete, catURL(sid, "/", someCat), nil)
		expectCrossSpace404(t, s, outsider, http.MethodPut, catURL(sid, "/order"), map[string]any{"type": "EXPENSE", "ids": []string{someCat}})
	})

	t.Run("a category id from another space is 404 even in your own space", func(t *testing.T) {
		mine := spaceIDOf(outsider)
		res := s.Do(http.MethodPatch, catURL(mine, "/", someCat), map[string]any{"name": "Stolen"}, bearer(outsider.AccessToken))
		require.Equal(t, http.StatusNotFound, res.Status)
		assert.Equal(t, "CATEGORY_NOT_FOUND", errCode(t, res))
		assert.Equal(t, http.StatusNotFound, s.Do(http.MethodDelete, catURL(mine, "/", someCat), nil, bearer(outsider.AccessToken)).Status)
		assert.Equal(t, "Transport", s.categories(owner, sid, "?type=EXPENSE")[0].Name, "owner's category unchanged")
	})

	t.Run("members can manage categories", func(t *testing.T) {
		assert.Equal(t, http.StatusCreated, s.newCategory(member, sid, cat("EXPENSE", "Member made")).Status)
		assert.Contains(t, names(s.categories(owner, sid, "")), "Member made")
	})
}

func TestCategorySuggestions(t *testing.T) {
	s := NewTestServer(t)
	u := s.register("jane@example.com")
	get := func(q string) Response { return s.Get("/api/v1/categories/suggestions"+q, bearer(u.AccessToken)) }

	var out []string
	res := get("?type=INCOME")
	require.Equal(t, http.StatusOK, res.Status)
	res.JSON(t, &out)
	assert.Equal(t, []string{"Salary", "Bonus", "Freelance", "Investment", "Gift", "Refund"}, out)

	res = get("?type=expense")
	require.Equal(t, http.StatusOK, res.Status)
	res.JSON(t, &out)
	assert.Equal(t, []string{"Food", "Coffee", "Transport", "Groceries", "Bills", "Entertainment"}, out)

	assert.Equal(t, http.StatusBadRequest, get("").Status)
	assert.Equal(t, http.StatusBadRequest, get("?type=X").Status)
}
