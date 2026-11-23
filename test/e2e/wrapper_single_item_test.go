//go:build e2e

package e2e

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// J-T1: the wrappers[] list of the grouped view and the categories of the assets summary hold an entry for
// every wrapper and category, including one that has a single holding, and for "Unassigned" with one item.
// The frontend renders a row for each of them (one holding is not collapsed away).
func TestSingleHoldingGroups(t *testing.T) {
	ctx := context.Background()
	s := NewTestServer(t)
	require.NoError(t, s.Currency.Refresh(ctx))
	e := newInvEnv(s)
	e.setSpace(map[string]any{"baseCurrency": "USD"})

	// one holding per category, each on its own wrapper, plus a single unassigned crypto
	e.must(crypto("Bitcoin", "Binance", "1", "300"))
	e.must(map[string]any{"assetType": "CRYPTO", "name": "Loose", "symbol": "LOO", "quantity": "1", "manualPrice": "50", "avgBuyPrice": "50", "priceMode": "MANUAL"})
	e.must(map[string]any{"assetType": "US_STOCK", "name": "Apple", "symbol": "AAPL", "quantity": "1", "manualPrice": "450", "avgBuyPrice": "400",
		"details": map[string]any{"exchange": "NASDAQ", "platform": "Robinhood"}})
	e.must(map[string]any{"assetType": "MUTUAL_FUND", "name": "Fund", "quantity": "1", "manualPrice": "70", "avgBuyPrice": "70",
		"wrapper": "Bibit", "details": map[string]any{"fundType": "EQUITY"}})

	t.Run("grouped: every wrapper is listed, with count 1 and its item", func(t *testing.T) {
		g := s.grouped(e.u, e.sid, "")
		require.Len(t, g.Categories, 3)
		byType := map[string]int{}
		for _, c := range g.Categories {
			byType[c.AssetType] = len(c.Wrappers)
			for _, w := range c.Wrappers {
				assert.GreaterOrEqual(t, w.Count, 1, c.AssetType+"/"+w.Label)
				assert.Len(t, w.Items, w.Count)
			}
		}
		assert.Equal(t, 2, byType["CRYPTO"], "Binance and Unassigned")
		assert.Equal(t, 1, byType["US_STOCK"])
		assert.Equal(t, 1, byType["MUTUAL_FUND"])

		for _, c := range g.Categories {
			switch c.AssetType {
			case "US_STOCK":
				require.Len(t, c.Wrappers, 1)
				w := c.Wrappers[0]
				assert.Equal(t, "Robinhood", w.Label)
				assert.Equal(t, 1, w.Count)
				require.NotNil(t, w.PctOfCategory)
				assert.Equal(t, "100.00", *w.PctOfCategory, "the only wrapper is the whole category")
			case "CRYPTO":
				var un = c.Wrappers[len(c.Wrappers)-1]
				assert.Equal(t, "_unassigned", un.Key, "Unassigned is last")
				assert.Equal(t, "Unassigned", un.Label)
				assert.Nil(t, un.Wrapper)
				assert.Equal(t, 1, un.Count)
				require.Len(t, un.Items, 1)
				assert.Equal(t, "Loose", un.Items[0].Name)
			}
		}
	})

	t.Run("grouped without items still counts the single holding", func(t *testing.T) {
		g := s.grouped(e.u, e.sid, "?items=false")
		for _, c := range g.Categories {
			for _, w := range c.Wrappers {
				assert.Equal(t, 1, w.Count, c.AssetType+"/"+w.Label)
				assert.Empty(t, w.Items)
			}
		}
	})

	t.Run("assets summary: every category with a single holding is listed", func(t *testing.T) {
		a := s.assetSummary(e.u, e.sid)
		require.NotNil(t, a.Investments)
		types := map[string]bool{}
		for _, c := range a.Investments.Categories {
			types[c.AssetType] = true
			assert.True(t, dec(c.Value).IsPositive(), c.AssetType)
			require.NotNil(t, c.PctOfTotal, c.AssetType)
		}
		assert.Equal(t, map[string]bool{"CRYPTO": true, "US_STOCK": true, "MUTUAL_FUND": true}, types)
	})
}
