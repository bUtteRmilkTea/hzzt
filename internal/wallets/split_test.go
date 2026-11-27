package wallets

import (
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
	"github.com/JonathanRainee/hzzt/internal/platform/allocation"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func participants(bps ...int32) []sqlc.WalletParticipant {
	out := make([]sqlc.WalletParticipant, len(bps))
	for i, b := range bps {
		out[i] = sqlc.WalletParticipant{ID: uuid.New(), DisplayName: "p", ShareBps: b}
	}
	return out
}

func sumSplit(parts []TransactionSplit) decimal.Decimal {
	out := decimal.Zero
	for _, p := range parts {
		out = out.Add(p.Amount)
	}
	return out
}

func TestSplit_Unshared(t *testing.T) {
	walletID := uuid.New()
	tr := sqlc.Transaction{Type: "EXPENSE", Amount: d("100"), Currency: "USD", WalletID: uuid.NullUUID{UUID: walletID, Valid: true}}
	parts, err := Split(tr, walletID, participants(10000))
	require.NoError(t, err)
	assert.Nil(t, parts, "a wallet with only one live participant is not shared")
}

func TestSplit_NotThisWallet(t *testing.T) {
	walletID, other := uuid.New(), uuid.New()
	tr := sqlc.Transaction{Type: "EXPENSE", Amount: d("100"), Currency: "USD", WalletID: uuid.NullUUID{UUID: other, Valid: true}}
	parts, err := Split(tr, walletID, participants(7000, 3000))
	require.NoError(t, err)
	assert.Nil(t, parts)
}

func TestSplit_Expense70_30(t *testing.T) {
	walletID := uuid.New()
	tr := sqlc.Transaction{Type: "EXPENSE", Amount: d("1000"), Currency: "USD", WalletID: uuid.NullUUID{UUID: walletID, Valid: true}}
	parts, err := Split(tr, walletID, participants(7000, 3000))
	require.NoError(t, err)
	require.Len(t, parts, 2)
	// EXPENSE deltas are negative (ledger.Deltas negates); 70/30 of -1000.
	assert.True(t, d("-700").Equal(parts[0].Amount))
	assert.True(t, d("-300").Equal(parts[1].Amount))
	assert.True(t, d("-1000").Equal(sumSplit(parts)))
}

func TestSplit_Income(t *testing.T) {
	walletID := uuid.New()
	tr := sqlc.Transaction{Type: "INCOME", Amount: d("1000"), Currency: "USD", WalletID: uuid.NullUUID{UUID: walletID, Valid: true}}
	parts, err := Split(tr, walletID, participants(5000, 5000))
	require.NoError(t, err)
	assert.True(t, d("500").Equal(parts[0].Amount))
	assert.True(t, d("500").Equal(parts[1].Amount))
}

func TestSplit_ThreeWay_AlwaysSumsExactly(t *testing.T) {
	walletID := uuid.New()
	tr := sqlc.Transaction{Type: "EXPENSE", Amount: d("10.01"), Currency: "USD", WalletID: uuid.NullUUID{UUID: walletID, Valid: true}}
	parts, err := Split(tr, walletID, participants(3334, 3333, 3333))
	require.NoError(t, err)
	require.Len(t, parts, 3)
	assert.True(t, d("-10.01").Equal(sumSplit(parts)), "largest-remainder split must sum back exactly: got %s", sumSplit(parts))
}

func TestSplit_InvestmentBuy_FundingWalletSide(t *testing.T) {
	walletID := uuid.New()
	action := "BUY"
	tr := sqlc.Transaction{
		Type: "TRANSFER", Amount: d("500"), Currency: "USD",
		WalletID: uuid.NullUUID{UUID: walletID, Valid: true}, InvestmentAction: &action,
	}
	parts, err := Split(tr, walletID, participants(6000, 4000))
	require.NoError(t, err)
	require.Len(t, parts, 2)
	// A BUY debits the funding wallet (ledger.Deltas negates it).
	assert.True(t, d("-300").Equal(parts[0].Amount))
	assert.True(t, d("-200").Equal(parts[1].Amount))
}

func TestSplit_InvestmentSell_CreditsWallet(t *testing.T) {
	walletID := uuid.New()
	action := "SELL"
	tr := sqlc.Transaction{
		Type: "TRANSFER", Amount: d("500"), Currency: "USD",
		WalletID: uuid.NullUUID{UUID: walletID, Valid: true}, InvestmentAction: &action,
	}
	parts, err := Split(tr, walletID, participants(5000, 5000))
	require.NoError(t, err)
	assert.True(t, d("250").Equal(parts[0].Amount))
	assert.True(t, d("250").Equal(parts[1].Amount))
}

func TestSplit_TransferToWallet_SplitsDestinationSide(t *testing.T) {
	src, dst := uuid.New(), uuid.New()
	toCur := "USD"
	tr := sqlc.Transaction{
		Type: "TRANSFER", Amount: d("400"), Currency: "USD",
		WalletID: uuid.NullUUID{UUID: src, Valid: true},
		ToWalletID: uuid.NullUUID{UUID: dst, Valid: true}, ToAmount: decimal.NullDecimal{Decimal: d("400"), Valid: true}, ToCurrency: &toCur,
	}
	parts, err := Split(tr, dst, participants(2000, 8000))
	require.NoError(t, err)
	assert.True(t, d("80").Equal(parts[0].Amount))
	assert.True(t, d("320").Equal(parts[1].Amount))
}

// TestAllocationSplit_PropertyLike exercises allocation.Split directly across many
// share combinations to document the guarantee Split relies on: parts always sum
// back to the input exactly, for both gains and losses.
func TestAllocationSplit_PropertyLike(t *testing.T) {
	cases := [][]int{{1}, {5000, 5000}, {3333, 3333, 3334}, {1, 9999}, {10000}}
	amounts := []string{"0.01", "1", "99.99", "-1000.37", "1234567.89"}
	for _, shares := range cases {
		for _, a := range amounts {
			amt := d(a)
			parts := allocation.Split(amt, shares, 2)
			assert.True(t, allocation.Sum(parts).Round(2).Equal(amt.Round(2)), "shares=%v amount=%s got sum=%s", shares, a, allocation.Sum(parts))
		}
	}
}
