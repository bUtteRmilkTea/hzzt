// Package wallets implements wallets with card styling and cached balances.
// Balance changes always go through package ledger.
package wallets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/JonathanRainee/hzzt/internal/currency"
	"github.com/JonathanRainee/hzzt/internal/db"
	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
	"github.com/JonathanRainee/hzzt/internal/ledger"
	"github.com/JonathanRainee/hzzt/internal/platform/apperror"
)

// Wallet types.
const (
	TypeBank    = "BANK"
	TypeEWallet = "E_WALLET"
	TypeCash    = "CASH"
	TypeOther   = "OTHER"
)

// Error codes owned by this package (see plan Appendix B).
const (
	CodeWalletNotFound        = "WALLET_NOT_FOUND"
	CodeWalletCurrencyLocked  = "WALLET_CURRENCY_LOCKED"
	CodeWalletHasBalance      = "WALLET_HAS_BALANCE"
	CodeWalletHasTransactions = "WALLET_HAS_TRANSACTIONS"
)

var (
	errWalletNF       = apperror.New(404, CodeWalletNotFound, "Wallet not found")
	errCurrencyLocked = apperror.Unprocessable(CodeWalletCurrencyLocked, "The currency cannot change once the wallet has transactions")
	errHasBalance     = apperror.Unprocessable(CodeWalletHasBalance, "The wallet still has a balance; pass force=true to delete it anyway")
)

// DB is what the service needs; *pgxpool.Pool and pgx.Tx both satisfy it.
type DB interface {
	sqlc.DBTX
	db.TxStarter
}

// Service holds wallet logic.
type Service struct {
	db       DB
	q        *sqlc.Queries
	currency *currency.Service
	now      func() time.Time
}

// NewService builds the service; now may be nil.
func NewService(database DB, cur *currency.Service, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{db: database, q: sqlc.New(database), currency: cur, now: now}
}

// Wallet pairs a row with its decoded card style and the amount reserved for goals.
type Wallet struct {
	sqlc.Wallet
	Style CardStyle
	// Reserved is the sum of the wallet's goal allocations (in the wallet's currency).
	Reserved decimal.Decimal
}

// Available is the balance that is not reserved for a goal; it may be negative
// when expenses pushed the wallet below its reserves.
func (w Wallet) Available() decimal.Decimal { return w.Balance.Sub(w.Reserved) }

// OverReserved reports whether the reserves exceed the balance.
func (w Wallet) OverReserved() bool { return w.Reserved.GreaterThan(w.Balance) }

func toWallet(w sqlc.Wallet) (Wallet, error) {
	var cs CardStyle
	if err := json.Unmarshal(w.CardStyle, &cs); err != nil {
		return Wallet{}, fmt.Errorf("decode card style of wallet %s: %w", w.ID, err)
	}
	return Wallet{Wallet: w, Style: cs}, nil
}

func toWallets(ws []sqlc.Wallet) ([]Wallet, error) {
	out := make([]Wallet, 0, len(ws))
	for _, w := range ws {
		x, err := toWallet(w)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, nil
}

// ListResult is the wallet list plus the converted total.
type ListResult struct {
	Wallets []Wallet
	Total   currency.AggregateResult
	// BaseBalance is each wallet's balance in the base currency; a wallet without a usable FX rate has no entry.
	BaseBalance map[uuid.UUID]decimal.Decimal
}

// List returns live wallets (favorites first, then sort_order) and their total
// converted to the space's base currency.
func (s *Service) List(ctx context.Context, spaceID uuid.UUID) (ListResult, error) {
	rows, err := s.q.ListWallets(ctx, spaceID)
	if err != nil {
		return ListResult{}, err
	}
	ws, err := toWallets(rows)
	if err != nil {
		return ListResult{}, err
	}
	reserved, err := s.q.ReservedByWallet(ctx, spaceID)
	if err != nil {
		return ListResult{}, err
	}
	byWallet := make(map[uuid.UUID]decimal.Decimal, len(reserved))
	for _, r := range reserved {
		byWallet[r.WalletID] = r.Reserved
	}
	for i := range ws {
		ws[i].Reserved = byWallet[ws[i].ID]
	}
	cc, err := s.currency.BuildContext(ctx, spaceID)
	if err != nil {
		return ListResult{}, err
	}
	items := make([]currency.MoneyItem, 0, len(ws))
	base := make(map[uuid.UUID]decimal.Decimal, len(ws))
	for _, w := range ws {
		items = append(items, currency.MoneyItem{Label: w.Name, Amount: w.Balance, Currency: w.Currency})
		if v, err := currency.Convert(w.Balance, strings.TrimSpace(w.Currency), cc.BaseCurrency, cc); err == nil {
			base[w.ID] = v
		}
	}
	return ListResult{Wallets: ws, Total: currency.Aggregate(items, cc), BaseBalance: base}, nil
}

// Get returns one live wallet of the space.
func (s *Service) Get(ctx context.Context, spaceID, id uuid.UUID) (Wallet, error) {
	w, err := s.q.GetWallet(ctx, sqlc.GetWalletParams{ID: id, SpaceID: spaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Wallet{}, errWalletNF
	}
	if err != nil {
		return Wallet{}, err
	}
	return s.finish(ctx, w)
}

// finish decodes a wallet row and attaches its reserved amount.
func (s *Service) finish(ctx context.Context, w sqlc.Wallet) (Wallet, error) {
	x, err := toWallet(w)
	if err != nil {
		return Wallet{}, err
	}
	if x.Reserved, err = s.q.ReservedForWallet(ctx, w.ID); err != nil {
		return Wallet{}, err
	}
	return x, nil
}

// CreateInput describes a new wallet.
type CreateInput struct {
	Type, Name, Icon string
	Currency         string // "" → the space's base currency
	InitialBalance   decimal.Decimal
	Style            *CardStyle
	Favorite         bool
}

// Create inserts a wallet; a non-zero initial balance becomes an OPENING_BALANCE
// transaction written in the same database transaction.
func (s *Service) Create(ctx context.Context, space sqlc.Space, userID uuid.UUID, in CreateInput) (Wallet, error) {
	cur := strings.ToUpper(strings.TrimSpace(in.Currency))
	if cur == "" {
		cur = strings.TrimSpace(space.BaseCurrency)
	}
	style := DefaultCardStyle()
	if in.Style != nil {
		style = *in.Style
	}
	styleJSON, err := json.Marshal(style)
	if err != nil {
		return Wallet{}, err
	}

	var created sqlc.Wallet
	err = db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		order, err := q.NextWalletSortOrder(ctx, space.ID)
		if err != nil {
			return err
		}
		w, err := q.CreateWallet(ctx, sqlc.CreateWalletParams{
			SpaceID: space.ID, Type: in.Type, Name: in.Name, Icon: in.Icon, Currency: cur,
			IsFavorite: in.Favorite, SortOrder: order, CardStyle: styleJSON, CreatedByID: userID,
		})
		if err != nil {
			return fmt.Errorf("create wallet: %w", err)
		}
		if !in.InitialBalance.IsZero() {
			if err := s.writeBalanceTx(ctx, tx, w, userID, ledger.TypeOpeningBalance, "Opening balance", "OPENING_BALANCE", nil, in.InitialBalance); err != nil {
				return err
			}
		}
		created, err = q.GetWallet(ctx, sqlc.GetWalletParams{ID: w.ID, SpaceID: space.ID})
		return err
	})
	if err != nil {
		return Wallet{}, err
	}
	return s.finish(ctx, created)
}

// writeBalanceTx records a signed balance change as an OPENING_BALANCE or ADJUSTMENT
// transaction and applies it through the ledger, inside the caller's tx.
func (s *Service) writeBalanceTx(ctx context.Context, tx pgx.Tx, w sqlc.Wallet, userID uuid.UUID, typ, title, systemKey string, note *string, signed decimal.Decimal) error {
	dir := ledger.DirectionIn
	if signed.IsNegative() {
		dir = ledger.DirectionOut
	}
	t, err := sqlc.New(tx).CreateTransaction(ctx, sqlc.CreateTransactionParams{
		SpaceID: w.SpaceID, Type: typ, Direction: &dir, Title: &title, Note: note,
		Amount: signed.Abs(), Currency: w.Currency, WalletID: uuid.NullUUID{UUID: w.ID, Valid: true},
		OccurredAt: s.now().UTC(), CreatedByID: userID, SystemKey: &systemKey,
	})
	if err != nil {
		return fmt.Errorf("create %s transaction: %w", typ, err)
	}
	return ledger.Apply(ctx, tx, t)
}

// UpdateInput is a partial update; nil means "not sent".
type UpdateInput struct {
	Type, Name, Icon, Currency *string
	Style                      *CardStyle
	Favorite                   *bool
}

// Update applies a partial update.
func (s *Service) Update(ctx context.Context, spaceID, id uuid.UUID, in UpdateInput) (Wallet, error) {
	var out sqlc.Wallet
	err := db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		cur, err := q.GetWalletForUpdate(ctx, sqlc.GetWalletForUpdateParams{ID: id, SpaceID: spaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return errWalletNF
		}
		if err != nil {
			return err
		}
		p := sqlc.UpdateWalletParams{ID: id, SpaceID: spaceID, Type: in.Type, Name: in.Name, Icon: in.Icon, IsFavorite: in.Favorite}
		if in.Currency != nil && !strings.EqualFold(*in.Currency, cur.Currency) {
			used, err := q.WalletHasTransactions(ctx, uuid.NullUUID{UUID: id, Valid: true})
			if err != nil {
				return err
			}
			if used {
				return errCurrencyLocked
			}
			p.Currency = in.Currency
		}
		if in.Style != nil {
			b, err := json.Marshal(in.Style)
			if err != nil {
				return err
			}
			p.CardStyle = b
		}
		out, err = q.UpdateWallet(ctx, p)
		return err
	})
	if err != nil {
		return Wallet{}, err
	}
	return s.finish(ctx, out)
}

// SetBalance moves the wallet to newBalance by recording an ADJUSTMENT for the
// difference. A zero difference records nothing.
func (s *Service) SetBalance(ctx context.Context, spaceID, id, userID uuid.UUID, newBalance decimal.Decimal, note *string) (Wallet, error) {
	var out sqlc.Wallet
	err := db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		w, err := q.GetWalletForUpdate(ctx, sqlc.GetWalletForUpdateParams{ID: id, SpaceID: spaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return errWalletNF
		}
		if err != nil {
			return err
		}
		out = w
		delta := newBalance.Sub(w.Balance)
		if delta.IsZero() {
			return nil
		}
		if err := s.writeBalanceTx(ctx, tx, w, userID, ledger.TypeAdjustment, "Balance adjustment", "BALANCE_ADJUSTMENT", note, delta); err != nil {
			return err
		}
		out, err = q.GetWallet(ctx, sqlc.GetWalletParams{ID: id, SpaceID: spaceID})
		return err
	})
	if err != nil {
		return Wallet{}, err
	}
	return s.finish(ctx, out)
}

// Delete soft-deletes a wallet, releasing its goal reservations. A non-zero balance needs force.
func (s *Service) Delete(ctx context.Context, spaceID, id uuid.UUID, force bool) error {
	return db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		w, err := q.GetWalletForUpdate(ctx, sqlc.GetWalletForUpdateParams{ID: id, SpaceID: spaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return errWalletNF
		}
		if err != nil {
			return err
		}
		if !w.Balance.IsZero() && !force {
			return errHasBalance
		}
		if !force {
			imp, err := s.deleteImpact(ctx, q, w)
			if err != nil {
				return err
			}
			if imp.Transactions > 0 {
				return apperror.Conflict(CodeWalletHasTransactions, "The wallet has transactions that stay in the history; pass force=true to delete it anyway").
					WithDetails(map[string]any{"transactions": imp.Transactions, "transfers": imp.Transfers, "goalReserves": imp.GoalReserves})
			}
		}
		// the wallet's goal reservations go with it
		if err := q.DeleteAllocationsForWallet(ctx, id); err != nil {
			return err
		}
		_, err = q.SoftDeleteWallet(ctx, sqlc.SoftDeleteWalletParams{ID: id, SpaceID: spaceID})
		return err
	})
}

// DeleteImpact is what deleting a wallet affects.
type DeleteImpact struct {
	// Transactions counts every transaction row that points at the wallet; Transfers is how many of those are
	// transfers to or from another own wallet. GoalReserves counts the goal reservations that go with the wallet.
	Transactions, Transfers, GoalReserves int
	Balance                               decimal.Decimal
	Currency                              string
}

func (s *Service) deleteImpact(ctx context.Context, q *sqlc.Queries, w sqlc.Wallet) (DeleteImpact, error) {
	row, err := q.WalletDeleteImpact(ctx, uuid.NullUUID{UUID: w.ID, Valid: true})
	if err != nil {
		return DeleteImpact{}, err
	}
	n, err := q.CountAllocationsForWallet(ctx, w.ID)
	if err != nil {
		return DeleteImpact{}, err
	}
	return DeleteImpact{Transactions: int(row.Transactions), Transfers: int(row.Transfers), GoalReserves: int(n), Balance: w.Balance, Currency: strings.TrimSpace(w.Currency)}, nil
}

// DeleteImpact tells what deleting the wallet would affect, so the client can warn before it asks.
func (s *Service) DeleteImpact(ctx context.Context, spaceID, id uuid.UUID) (DeleteImpact, error) {
	w, err := s.q.GetWallet(ctx, sqlc.GetWalletParams{ID: id, SpaceID: spaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return DeleteImpact{}, errWalletNF
	}
	if err != nil {
		return DeleteImpact{}, err
	}
	return s.deleteImpact(ctx, s.q, w)
}

// Reorder sets the display order; ids must list every live wallet exactly once.
func (s *Service) Reorder(ctx context.Context, spaceID uuid.UUID, ids []uuid.UUID) error {
	return db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		have, err := q.ListWalletIDsForUpdate(ctx, spaceID)
		if err != nil {
			return err
		}
		want := slices.Clone(ids)
		cmp := func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) }
		slices.SortFunc(want, cmp)
		slices.SortFunc(have, cmp)
		if !slices.Equal(want, have) {
			return apperror.Validation("Validation failed", map[string]any{"ids": "must list every wallet of this space exactly once"})
		}
		for i, id := range ids {
			if err := q.SetWalletSortOrder(ctx, sqlc.SetWalletSortOrderParams{ID: id, SpaceID: spaceID, SortOrder: int32(i)}); err != nil {
				return err
			}
		}
		return nil
	})
}
