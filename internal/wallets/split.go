package wallets

import (
	"context"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
	"github.com/JonathanRainee/hzzt/internal/ledger"
	"github.com/JonathanRainee/hzzt/internal/platform/allocation"
	"github.com/JonathanRainee/hzzt/internal/platform/money"
)

// TransactionSplit is one participant's part of a transaction on a shared wallet.
type TransactionSplit struct {
	ParticipantID uuid.UUID
	DisplayName   string
	ShareBps      int
	// Amount is signed exactly like ledger.Delta: positive adds to the wallet,
	// negative takes from it. The parts always sum to the wallet's own delta.
	Amount decimal.Decimal
}

// Split attributes a transaction's effect on walletID between the wallet's live
// participants, by their current share_bps. It reuses ledger.Deltas so the sign
// of every transaction type (income, expense, a transfer's two sides, a
// settlement, an investment BUY/SELL) matches the ledger exactly — this is a
// read-side attribution layer, never a second source of truth for the balance.
//
// Returns nil when the transaction has no effect on walletID (not one of its
// wallets), or when walletID is not shared (participants has fewer than 2 rows).
func Split(t sqlc.Transaction, walletID uuid.UUID, participants []sqlc.WalletParticipant) ([]TransactionSplit, error) {
	if len(participants) < 2 {
		return nil, nil
	}
	deltas, err := ledger.Deltas(t)
	if err != nil {
		return nil, err
	}
	var amount decimal.Decimal
	found := false
	for _, d := range deltas {
		if d.WalletID != walletID {
			continue
		}
		if found {
			// Both sides of the transaction land on the same wallet (a degenerate
			// self-transfer); there is no single delta to attribute. Skip.
			return nil, nil
		}
		amount, found = d.Amount, true
	}
	if !found {
		return nil, nil
	}

	cur := t.Currency
	if t.ToWalletID.Valid && t.ToWalletID.UUID == walletID && t.ToCurrency != nil {
		cur = *t.ToCurrency
	}
	shares := make([]int, len(participants))
	for i, p := range participants {
		shares[i] = int(p.ShareBps)
	}
	parts := allocation.Split(amount, shares, money.MinorUnits(cur))

	out := make([]TransactionSplit, len(participants))
	for i, p := range participants {
		out[i] = TransactionSplit{ParticipantID: p.ID, DisplayName: p.DisplayName, ShareBps: int(p.ShareBps), Amount: parts[i]}
	}
	return out, nil
}

// LiveParticipantsByWallet batches ListLiveWalletParticipants over several wallets at
// once (e.g. every wallet referenced by a page of transactions), so callers that need
// to attach a Split per transaction don't issue one query per row. A wallet with fewer
// than 2 live participants (not shared) is simply absent from the result.
func (s *Service) LiveParticipantsByWallet(ctx context.Context, walletIDs []uuid.UUID) (map[uuid.UUID][]sqlc.WalletParticipant, error) {
	if len(walletIDs) == 0 {
		return nil, nil
	}
	rows, err := s.q.ListLiveWalletParticipantsForWallets(ctx, walletIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID][]sqlc.WalletParticipant, len(walletIDs))
	for _, r := range rows {
		out[r.WalletID] = append(out[r.WalletID], r)
	}
	for id, rows := range out {
		if len(rows) < 2 {
			delete(out, id)
		}
	}
	return out, nil
}
