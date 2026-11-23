//go:build e2e

package e2e

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// I-T4: the list reports how many rows match the filters in all, whatever the page size or cursor,
// so the filter badge needs no second request.
func TestTransactionListTotal(t *testing.T) {
	s := NewTestServer(t)
	w := newWorld(t, s)
	for i := 0; i < 5; i++ {
		w.must(map[string]any{"type": "EXPENSE", "walletId": w.a.ID, "amount": "1", "categoryId": w.cat["Transport"]})
	}
	w.must(map[string]any{"type": "INCOME", "walletId": w.a.ID, "amount": "9", "categoryId": w.cat["Salary"]})

	total := func(query string) (int, txnList) {
		res := s.Get(txURL(w.sid)+query, bearer(w.u.AccessToken))
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		var body struct {
			Total int `json:"total"`
		}
		res.JSON(t, &body)
		var l txnList
		res.JSON(t, &l)
		return body.Total, l
	}

	n, l := total("")
	assert.Equal(t, 8, n, "2 opening balances + 5 expenses + 1 income")
	assert.Len(t, l.Items, 8)

	n, l = total("?types=EXPENSE&limit=2")
	assert.Equal(t, 5, n, "the total ignores the page size")
	assert.Len(t, l.Items, 2)
	require.NotNil(t, l.NextCursor)

	n, l = total("?types=EXPENSE&limit=2&cursor=" + *l.NextCursor)
	assert.Equal(t, 5, n, "and the cursor")
	assert.Len(t, l.Items, 2)

	n, _ = total("?categoryIds=" + w.cat["Salary"])
	assert.Equal(t, 1, n)
	n, _ = total("?q=nothing-matches-this")
	assert.Equal(t, 0, n)
}
