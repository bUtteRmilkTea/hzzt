package investments

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
)

func TestPctToBps(t *testing.T) {
	for in, want := range map[string]int{"0": 0, "0.01": 1, "40": 4000, "33.33": 3333, "100": 10000, "100.00": 10000} {
		got, ok := pctToBps(d(in))
		assert.True(t, ok, in)
		assert.Equal(t, want, got, in)
	}
	for _, in := range []string{"-0.01", "100.01", "12.345", "1000"} {
		_, ok := pctToBps(d(in))
		assert.False(t, ok, in)
	}
	assert.Equal(t, "70.00", bpsToPct(7000).StringFixed(2))
	assert.Equal(t, "0.05", bpsToPct(5).StringFixed(2))
}

func alloc(asset, key, label string, bps int32) sqlc.InvestmentWrapperAllocation {
	return sqlc.InvestmentWrapperAllocation{ID: uuid.New(), AssetType: asset, WrapperKey: key, Label: label, PctBps: bps}
}

func TestBuildAllocations(t *testing.T) {
	items := []Item{
		wrapped(TypeCrypto, "USD", "1", "300", strp("Binance"), now),
		wrapped(TypeCrypto, "USD", "1", "100", strp("Ledger"), now),
		wrapped(TypeCrypto, "USD", "1", "100", nil, now),
		wrapped(TypeUSStock, "USD", "1", "0", strp("Robinhood"), now),
	}
	rows := []sqlc.InvestmentWrapperAllocation{
		alloc(TypeCrypto, "binance", "Binance", 6000),
		alloc(TypeCrypto, "old broker", "Old Broker", 1000), // orphan: nothing is held there any more
		alloc(TypeMutualFund, "bibit", "Bibit", 2500),       // orphan in an otherwise empty type
	}
	al := BuildAllocations(Group("USD", items), rows)
	require.Len(t, al.Categories, 3)

	c := al.Categories[0]
	assert.Equal(t, TypeCrypto, c.AssetType)
	assert.Equal(t, "70.00", c.AssignedPct.StringFixed(2))
	assert.Equal(t, "30.00", c.UnallocatedPct.StringFixed(2))
	require.Len(t, c.Wrappers, 4)
	b := c.Wrappers[0]
	assert.Equal(t, "binance", b.Key)
	assert.Equal(t, "60.00", b.TargetPct.StringFixed(2))
	assert.Equal(t, "60.00", b.ActualPct.StringFixed(2))
	assert.Equal(t, "0.00", b.DriftPct.StringFixed(2))
	ledger := c.Wrappers[1]
	assert.Nil(t, ledger.TargetPct, "unset, not 0")
	assert.Nil(t, ledger.DriftPct)
	assert.Equal(t, UnassignedKey, c.Wrappers[2].Key)
	orphan := c.Wrappers[3]
	assert.Equal(t, "old broker", orphan.Key)
	assert.Equal(t, "Old Broker", *orphan.Wrapper)
	assert.Equal(t, "0.00", orphan.ActualPct.StringFixed(2))
	assert.Equal(t, "-10.00", orphan.DriftPct.StringFixed(2))

	us := al.Categories[1]
	assert.Equal(t, TypeUSStock, us.AssetType)
	assert.Nil(t, us.Wrappers[0].ActualPct, "the type is worth 0")
	assert.Equal(t, "0.00", us.AssignedPct.StringFixed(2))

	mf := al.Categories[2]
	assert.Equal(t, TypeMutualFund, mf.AssetType)
	assert.Nil(t, mf.Wrappers[0].ActualPct)
	assert.Equal(t, "75.00", mf.UnallocatedPct.StringFixed(2))

	cat, w, ok := al.find(TypeCrypto, "ledger")
	assert.True(t, ok)
	assert.Equal(t, "ledger", w.Key)
	assert.Equal(t, TypeCrypto, cat.AssetType)
	assert.Equal(t, "100.00", al.category(TypeGlobalETF).UnallocatedPct.StringFixed(2))
}
