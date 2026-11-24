//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The form asks for the exchange or broker once: `wrapper`. The three details keys are gone from the metadata,
// are still accepted from an old client (mapped into the wrapper), and are never stored or returned.
func TestSingleWrapperField(t *testing.T) {
	s := NewTestServer(t)
	e := newInvEnv(s)

	t.Run("metadata no longer lists the three fields and keeps the listing exchange", func(t *testing.T) {
		res := s.Get("/api/v1/investment-types", bearer(e.u.AccessToken))
		require.Equal(t, http.StatusOK, res.Status)
		var metas []struct {
			AssetType    string `json:"assetType"`
			DetailFields []struct {
				Name     string `json:"name"`
				Label    string `json:"label"`
				Required bool   `json:"required"`
			} `json:"detailFields"`
		}
		res.JSON(t, &metas)
		require.Len(t, metas, 5)
		for _, m := range metas {
			for _, f := range m.DetailFields {
				assert.NotContains(t, []string{"exchangeOrWallet", "broker", "platform"}, f.Name, m.AssetType)
				if f.Name == "exchange" {
					assert.Equal(t, "Listing exchange", f.Label)
					assert.True(t, f.Required, "exchange stays required for "+m.AssetType)
				}
			}
		}
	})

	t.Run("old details key lands in the wrapper and is not stored or returned", func(t *testing.T) {
		got := e.must(map[string]any{"assetType": "ID_STOCK", "name": "BCA", "symbol": "BBCA", "avgBuyPrice": "9000", "manualPrice": "9500",
			"details": map[string]any{"broker": "Stockbit", "lots": 1}})
		assert.Equal(t, "Stockbit", *got.Wrapper)
		assert.NotContains(t, got.Details, "broker")
		var n int
		require.NoError(t, s.Tx.QueryRow(context.Background(), `SELECT count(*) FROM investments WHERE details ?| array['exchangeOrWallet','platform','broker']`).Scan(&n))
		assert.Zero(t, n)
	})

	t.Run("exchange stays required for a US stock", func(t *testing.T) {
		res := e.post(map[string]any{"assetType": "US_STOCK", "name": "Apple", "symbol": "AAPL", "quantity": "1", "wrapper": "IBKR", "details": map[string]any{}})
		assert.Equal(t, http.StatusBadRequest, res.Status)
	})
}

// A row migrated from details.broker and a new row with wrapper = "stockbit " land in one group and one target.
func TestFoldMigrationKeepsTheGroup(t *testing.T) {
	ctx := context.Background()
	s := NewTestServer(t)
	require.NoError(t, s.Currency.Refresh(ctx))
	e := newInvEnv(s)
	e.setSpace(map[string]any{"baseCurrency": "USD"})

	old := e.must(map[string]any{"assetType": "US_STOCK", "name": "Old", "symbol": "OLD", "quantity": "1", "avgBuyPrice": "10", "manualPrice": "10", "wrapper": "tmp",
		"details": map[string]any{"exchange": "NASDAQ"}})
	two := e.must(map[string]any{"assetType": "US_STOCK", "name": "Two", "symbol": "TWO", "quantity": "1", "avgBuyPrice": "10", "manualPrice": "10", "wrapper": "Keep",
		"details": map[string]any{"exchange": "NASDAQ"}})
	crypto := e.must(map[string]any{"assetType": "CRYPTO", "name": "Coin", "symbol": "COI", "quantity": "1", "avgBuyPrice": "10", "manualPrice": "10", "priceMode": "MANUAL"})
	// the old shape: wrapper empty, the broker only in details (two different values on the third row)
	_, err := s.Tx.Exec(ctx, `UPDATE investments SET wrapper = NULL, details = details || '{"platform":"  Stockbit  "}'::jsonb WHERE id = $1`, old.ID)
	require.NoError(t, err)
	_, err = s.Tx.Exec(ctx, `UPDATE investments SET details = details || '{"platform":"Other"}'::jsonb WHERE id = $1`, two.ID)
	require.NoError(t, err)
	_, err = s.Tx.Exec(ctx, `UPDATE investments SET wrapper = NULL, details = details || '{"exchangeOrWallet":"Ledger","network":"X"}'::jsonb WHERE id = $1`, crypto.ID)
	require.NoError(t, err)

	s.runMigrationUp("00023_details_wrapper_fold.sql")

	assert.Equal(t, "Stockbit", *e.get(old.ID).Wrapper)
	assert.Equal(t, "Keep", *e.get(two.ID).Wrapper, "an existing wrapper is never overwritten")
	assert.Equal(t, "Ledger", *e.get(crypto.ID).Wrapper)
	assert.Equal(t, "X", e.get(crypto.ID).Details["network"], "other keys stay")
	var n int
	require.NoError(t, s.Tx.QueryRow(ctx, `SELECT count(*) FROM investments WHERE details ?| array['exchangeOrWallet','platform','broker']`).Scan(&n))
	assert.Zero(t, n, "the three keys are gone")

	// a new row with the same broker in another casing joins the same group and the same target
	e.must(map[string]any{"assetType": "US_STOCK", "name": "New", "symbol": "NEW", "quantity": "1", "avgBuyPrice": "10", "manualPrice": "10", "wrapper": "stockbit ",
		"details": map[string]any{"exchange": "NYSE"}})
	g := s.grouped(e.u, e.sid, "")
	for _, c := range g.Categories {
		if c.AssetType != "US_STOCK" {
			continue
		}
		var count int
		for _, w := range c.Wrappers {
			if w.Key == "stockbit" {
				count = w.Count
			}
		}
		assert.Equal(t, 2, count, "migrated and new row share one group")
	}
	res := s.Do(http.MethodPatch, wrapURL(e.sid, "/US_STOCK/stockbit"), map[string]any{"targetPct": "60"}, bearer(e.u.AccessToken))
	require.Equal(t, http.StatusOK, res.Status, string(res.Body))
	res = s.Get(wrapURL(e.sid), bearer(e.u.AccessToken))
	require.Equal(t, http.StatusOK, res.Status)
	var al struct {
		Categories []struct {
			AssetType string `json:"assetType"`
			Wrappers  []struct {
				Key       string  `json:"key"`
				TargetPct *string `json:"targetPct"`
			} `json:"wrappers"`
		} `json:"categories"`
	}
	res.JSON(t, &al)
	keys := 0
	for _, c := range al.Categories {
		for _, w := range c.Wrappers {
			if c.AssetType == "US_STOCK" && w.Key == "stockbit" {
				keys++
				require.NotNil(t, w.TargetPct)
			}
		}
	}
	assert.Equal(t, 1, keys, "one group and one target")
}
