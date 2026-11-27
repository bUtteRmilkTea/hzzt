package wallets

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/JonathanRainee/hzzt/internal/currency"
	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
	"github.com/JonathanRainee/hzzt/internal/platform/money"
)

// SharedIn is a wallet owned by another space, shared into spaceID at a
// percentage. It is always read-only from spaceID's side.
type SharedIn struct {
	Wallet
	MyShareBps      int
	MyParticipantID uuid.UUID
	// MyBalance is Wallet.Balance scaled to the viewer's share, rounded to the
	// wallet currency's minor units (the same rounding allocation.Split would give
	// this one share, since a lone share of a two-or-more-way split is still exact
	// to the currency's minor unit under the largest-remainder method).
	MyBalance decimal.Decimal
}

// ListSharedIn returns the wallets of other spaces that have been shared into spaceID,
// with the viewer's own share of each wallet's balance.
func (s *Service) ListSharedIn(ctx context.Context, spaceID uuid.UUID) ([]SharedIn, error) {
	rows, err := s.q.ListSharedInWallets(ctx, uuid.NullUUID{UUID: spaceID, Valid: true})
	if err != nil {
		return nil, err
	}
	out := make([]SharedIn, 0, len(rows))
	for _, r := range rows {
		w := sqlc.Wallet{
			ID: r.ID, SpaceID: r.SpaceID, Type: r.Type, Name: r.Name, Icon: r.Icon, Currency: r.Currency,
			Balance: r.Balance, IsFavorite: r.IsFavorite, SortOrder: r.SortOrder, CardStyle: r.CardStyle,
			CreatedByID: r.CreatedByID, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, DeletedAt: r.DeletedAt, IsShared: r.IsShared,
		}
		x, err := toWallet(w)
		if err != nil {
			return nil, err
		}
		factor := decimal.NewFromInt(int64(r.MyShareBps)).Div(decimal.NewFromInt(TotalBps))
		out = append(out, SharedIn{
			Wallet: x, MyShareBps: int(r.MyShareBps), MyParticipantID: r.MyParticipantID,
			MyBalance: money.RoundToMinor(r.Balance.Mul(factor), strings.TrimSpace(r.Currency)),
		})
	}
	return out, nil
}

// ownerShareBps is the owner's own bps from a wallet's live participants; TotalBps
// (fully owned) when the wallet isn't actually shared or has no recorded owner row.
func ownerShareBps(participants []sqlc.WalletParticipant) int {
	for _, p := range participants {
		if p.Role == RoleOwner {
			return int(p.ShareBps)
		}
	}
	return TotalBps
}

// NetWorthItems is every wallet balance that counts toward spaceID's net worth, each
// already scaled to the viewing space's share: a shared wallet this space owns counts
// only the owner's bps (never the whole balance twice, once here and once in the
// participant's space), and a wallet shared INTO this space by another space's owner
// counts the participant's bps. Replaces a plain ListWallets wherever a wallet balance
// feeds a total (net worth, dashboard), per Phase P20.
func (s *Service) NetWorthItems(ctx context.Context, spaceID uuid.UUID) ([]currency.MoneyItem, error) {
	owned, err := s.q.ListWallets(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	sharedIDs := make([]uuid.UUID, 0, len(owned))
	for _, w := range owned {
		if w.IsShared {
			sharedIDs = append(sharedIDs, w.ID)
		}
	}
	byWallet, err := s.LiveParticipantsByWallet(ctx, sharedIDs)
	if err != nil {
		return nil, err
	}

	items := make([]currency.MoneyItem, 0, len(owned))
	for _, w := range owned {
		amt := w.Balance
		if ps, ok := byWallet[w.ID]; ok {
			factor := decimal.NewFromInt(int64(ownerShareBps(ps))).Div(decimal.NewFromInt(TotalBps))
			amt = money.RoundToMinor(amt.Mul(factor), strings.TrimSpace(w.Currency))
		}
		items = append(items, currency.MoneyItem{Label: w.Name, Amount: amt, Currency: w.Currency})
	}

	sharedIn, err := s.ListSharedIn(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	for _, si := range sharedIn {
		items = append(items, currency.MoneyItem{Label: si.Name, Amount: si.MyBalance, Currency: si.Currency})
	}
	return items, nil
}
