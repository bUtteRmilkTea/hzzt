//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func rawBody(t *testing.T, res Response) map[string]any {
	t.Helper()
	var m map[string]any
	res.JSON(t, &m)
	return m
}

// The expected yearly change and the staked quantity are gone from every response. Old clients may still send
// them for one release: the request is accepted and the values are ignored.
func TestRemovedFieldsAreIgnoredOnWrite(t *testing.T) {
	s := NewTestServer(t)
	e := newInvEnv(s)

	t.Run("investment without the fields", func(t *testing.T) {
		res := e.post(map[string]any{"assetType": "CRYPTO", "name": "Ethereum", "symbol": "ETH", "quantity": "2", "avgBuyPrice": "3000", "manualPrice": "3500", "priceMode": "MANUAL"})
		require.Equal(t, http.StatusCreated, res.Status, string(res.Body))
		m := rawBody(t, res)
		assert.NotContains(t, m, "estimatedAnnualChangePct")
		assert.NotContains(t, m["details"], "stakedQuantity")
	})

	t.Run("investment with the old fields", func(t *testing.T) {
		res := e.post(map[string]any{"assetType": "CRYPTO", "name": "Ether", "symbol": "ETH", "quantity": "2", "avgBuyPrice": "3000", "manualPrice": "3500",
			"priceMode": "MANUAL", "estimatedAnnualChangePct": "5.5", "details": map[string]any{"stakedQuantity": "1.5", "network": "Ethereum"}})
		require.Equal(t, http.StatusCreated, res.Status, string(res.Body))
		m := rawBody(t, res)
		assert.NotContains(t, m, "estimatedAnnualChangePct")
		assert.NotContains(t, m["details"], "stakedQuantity")
		assert.Equal(t, "Ethereum", m["details"].(map[string]any)["network"])
		id := m["id"].(string)

		res = e.patch(id, map[string]any{"estimatedAnnualChangePct": nil, "details": map[string]any{"stakedQuantity": "1", "network": "Ethereum"}})
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		m = rawBody(t, res)
		assert.NotContains(t, m, "estimatedAnnualChangePct")
		assert.NotContains(t, m["details"], "stakedQuantity")

		var stored int
		require.NoError(t, s.Tx.QueryRow(context.Background(), `SELECT count(*) FROM investments WHERE details ? 'stakedQuantity'`).Scan(&stored))
		assert.Zero(t, stored, "nothing is stored")
	})

	t.Run("investment types metadata", func(t *testing.T) {
		res := s.Get("/api/v1/investment-types", bearer(e.u.AccessToken))
		require.Equal(t, http.StatusOK, res.Status)
		assert.NotContains(t, string(res.Body), "stakedQuantity")
		assert.NotContains(t, string(res.Body), "estimatedAnnualChangePct")
	})
}

func TestRemovedFieldsAreIgnoredOnManualAssets(t *testing.T) {
	s := NewTestServer(t)
	ae := newAssetEnv(t, s)
	res := ae.post(map[string]any{"kind": "FIXED_ASSET", "name": "House", "currentValue": "1000", "estimatedAnnualChangePct": "4"})
	require.Equal(t, http.StatusCreated, res.Status, string(res.Body))
	m := rawBody(t, res)
	assert.NotContains(t, m, "estimatedAnnualChangePct")
	res = ae.patch(m["id"].(string), map[string]any{"estimatedAnnualChangePct": "6"})
	require.Equal(t, http.StatusOK, res.Status, string(res.Body))
	assert.NotContains(t, rawBody(t, res), "estimatedAnnualChangePct")
	res = s.Get(assetURL(ae.sid), bearer(ae.u.AccessToken))
	require.Equal(t, http.StatusOK, res.Status)
	assert.NotContains(t, string(res.Body), "estimatedAnnualChangePct")
	ids := ae.must(debt("Card", "10"))
	assert.Equal(t, http.StatusOK, ae.patch(ids.ID, map[string]any{"estimatedAnnualChangePct": "5"}).Status, "ignored, even on a debt")
}

func TestFormerlyStakedCryptoIsFullySellable(t *testing.T) {
	s := NewTestServer(t)
	e := newTradeEnv(t, s)
	eth := e.inv.must(map[string]any{"assetType": "CRYPTO", "name": "Ethereum", "symbol": "ETH", "quantity": "10",
		"avgBuyPrice": "2000", "manualPrice": "2500", "priceMode": "MANUAL", "details": map[string]any{"stakedQuantity": "4"}})
	got := e.mustTrade(e.sell(e.a, eth.ID, "10", "2500", ""))
	assert.True(t, dec("5000").Equal(dec(*got.RealizedPL)), *got.RealizedPL)
	e.assertPosition(eth.ID, "0", "2000", "0")
}

// The migration removes the stored key (details are read leniently, so this is tidy-up).
func TestStripStakedQuantityMigration(t *testing.T) {
	s := NewTestServer(t)
	e := newInvEnv(s)
	inv := e.must(map[string]any{"assetType": "CRYPTO", "name": "Ethereum", "symbol": "ETH", "quantity": "2", "priceMode": "MANUAL"})
	ctx := context.Background()
	_, err := s.Tx.Exec(ctx, `UPDATE investments SET details = details || '{"stakedQuantity":"1","network":"X"}'::jsonb WHERE id = $1`, inv.ID)
	require.NoError(t, err)
	s.runMigrationUp("00022_strip_staked_quantity.sql")
	var has bool
	var network string
	require.NoError(t, s.Tx.QueryRow(ctx, `SELECT details ? 'stakedQuantity', details->>'network' FROM investments WHERE id = $1`, inv.ID).Scan(&has, &network))
	assert.False(t, has)
	assert.Equal(t, "X", network)
	assert.Equal(t, "2", e.get(inv.ID).Quantity)
}
