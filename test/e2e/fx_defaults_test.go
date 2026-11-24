//go:build e2e

package e2e

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// H-T1/H-T2 (T4): the FX rate is automatic by default. A USD stock in an IDR space with no manualFxRate is valued
// with the stored provider rate and no warning, and the DTO says which rate was used.
func TestInvestmentFXIsAutomaticByDefault(t *testing.T) {
	ctx := context.Background()
	s := NewTestServer(t)
	require.NoError(t, s.Currency.Refresh(ctx)) // 1 USD = 16000 IDR
	e := newInvEnv(s)
	e.setSpace(map[string]any{"baseCurrency": "IDR"})

	t.Run("no manualFxRate: the stored provider rate, no warning", func(t *testing.T) {
		got := e.must(clone(aapl))
		assert.Nil(t, got.ManualFxRate)
		assert.Empty(t, got.Valuation.Warnings)
		require.NotNil(t, got.Valuation.Base)
		assert.True(t, dec("17600000").Equal(dec(got.Valuation.Base.MarketValue)), "10 x 110 USD x 16000")
		assert.Equal(t, "USD", got.QuoteCurrency)
		require.NotNil(t, got.FxRate)
		assert.True(t, dec("16000").Equal(dec(*got.FxRate)))
		require.NotNil(t, got.FxSource)
		assert.Equal(t, "PROVIDER", *got.FxSource)
		require.NotNil(t, got.FxAsOf, "the date of the stored rate")
	})

	t.Run("the list carries the same fields", func(t *testing.T) {
		l := e.list("")
		require.NotEmpty(t, l.Items)
		assert.Equal(t, "PROVIDER", *l.Items[0].FxSource)
	})

	t.Run("an IDR-only stock exposes quoteCurrency IDR and no rate in an IDR space", func(t *testing.T) {
		got := e.must(clone(bbca))
		assert.Equal(t, "IDR", got.QuoteCurrency)
		assert.Nil(t, got.FxRate, "no conversion needed")
		assert.Nil(t, got.FxSource)
		assert.Nil(t, got.FxAsOf)
	})

	t.Run("a different base currency changes the rate used", func(t *testing.T) {
		e.setSpace(map[string]any{"baseCurrency": "EUR"})
		l := e.list("?assetType=US_STOCK")
		require.NotEmpty(t, l.Items)
		assert.True(t, dec("0.8").Equal(dec(*l.Items[0].FxRate)))
	})

	t.Run("manual FX mode: the typed rate wins and is labelled MANUAL", func(t *testing.T) {
		e.setSpace(map[string]any{"baseCurrency": "IDR", "autoFxConversion": false})
		got := e.must(clone(aapl, "name", "Apple manual", "manualFxRate", "15000"))
		require.NotNil(t, got.FxRate)
		assert.True(t, dec("15000").Equal(dec(*got.FxRate)))
		assert.Equal(t, "MANUAL", *got.FxSource)
		assert.Nil(t, got.FxAsOf)
	})

	t.Run("a currency with no rate leaves the base out with FX_RATE_UNAVAILABLE and null fx fields", func(t *testing.T) {
		e.setSpace(map[string]any{"baseCurrency": "IDR", "autoFxConversion": true})
		delete(s.FX.Set.Rates, "GBP")
		_, err := s.Tx.Exec(ctx, `DELETE FROM fx_rates WHERE quote = 'GBP'`)
		require.NoError(t, err)
		got := e.must(map[string]any{"assetType": "MUTUAL_FUND", "name": "UK fund", "quoteCurrency": "GBP", "quantity": "10",
			"avgBuyPrice": "5", "manualPrice": "6", "details": map[string]any{"fundType": "EQUITY"}})
		assert.Nil(t, got.Valuation.Base)
		assert.Nil(t, got.FxRate)
		require.Len(t, got.Valuation.Warnings, 1)
		assert.Equal(t, "FX_RATE_UNAVAILABLE", got.Valuation.Warnings[0].Code)
		assert.Contains(t, warnCodes(e.list("").Summary.Warnings), "FX_RATE_UNAVAILABLE")
	})
}
