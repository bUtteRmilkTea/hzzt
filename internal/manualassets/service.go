// Package manualassets implements fixed assets, money owed (receivables) and
// debt (liabilities) that are tracked by hand, with settlement payments.
package manualassets

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/JonathanRainee/hzzt/internal/transactions"
)

// Asset kinds.
const (
	KindFixedAsset = "FIXED_ASSET"
	KindReceivable = "RECEIVABLE"
	KindLiability  = "LIABILITY"
)

// Value change reasons.
const (
	ReasonManual     = "MANUAL"
	ReasonSettlement = "SETTLEMENT"
)

// Error codes owned by this package (see plan Appendix B).
const (
	CodeManualAssetNotFound  = "MANUAL_ASSET_NOT_FOUND"
	CodeSettlementExceeds    = "SETTLEMENT_EXCEEDS_BALANCE"
	CodeSettlementNotSupport = "SETTLEMENT_NOT_SUPPORTED"
	CodeSettlementCurrency   = "SETTLEMENT_CURRENCY_MISMATCH"
	CodeAssetCurrencyLocked  = "MANUAL_ASSET_CURRENCY_LOCKED"
	CodeValueChangeNotFound  = "VALUE_CHANGE_NOT_FOUND"
	CodeValueChangedSince    = "VALUE_CHANGED_SINCE"
	CodeUseSettlementDelete  = "USE_SETTLEMENT_DELETE"
	CodeCannotRevertInitial  = "CANNOT_REVERT_INITIAL_VALUE"
	codeWalletNotFound       = "WALLET_NOT_FOUND"
	maxValue                 = 20 // digits before the decimal point (numeric(28,8))
)

var (
	errAssetNF   = apperror.New(404, CodeManualAssetNotFound, "Asset not found")
	errWalletNF  = apperror.New(404, codeWalletNotFound, "Wallet not found")
	errNotSettle = errors.New("not a settlement")
	// ErrNotASettlement means the transaction exists but is not an asset settlement.
	ErrNotASettlement = errNotSettle
)

// DB is what the service needs; *pgxpool.Pool and pgx.Tx both satisfy it.
type DB interface {
	sqlc.DBTX
	db.TxStarter
}

// Service holds manual asset logic.
type Service struct {
	db       DB
	q        *sqlc.Queries
	currency *currency.Service
	txn      *transactions.Service
	now      func() time.Time
}

// NewService builds the service; now may be nil.
func NewService(database DB, cur *currency.Service, txn *transactions.Service, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{db: database, q: sqlc.New(database), currency: cur, txn: txn, now: now}
}

// Asset is a manual asset with its recent value history.
type Asset struct {
	sqlc.ManualAsset
	History []sqlc.ManualAssetValueChange // newest first; only filled by Get
}

// CreateInput describes a new asset.
type CreateInput struct {
	Kind, Name, Icon string
	Currency         string // "" → the space's base currency
	CurrentValue     decimal.Decimal
	Counterparty     *string
	DueDate          *time.Time
	Note             *string
}

// Create stores an asset and writes its first value-history row.
func (s *Service) Create(ctx context.Context, space sqlc.Space, userID uuid.UUID, in CreateInput) (Asset, error) {
	cur := strings.ToUpper(strings.TrimSpace(in.Currency))
	if cur == "" {
		cur = strings.TrimSpace(space.BaseCurrency)
	}
	var a sqlc.ManualAsset
	err := db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		var err error
		a, err = q.CreateManualAsset(ctx, sqlc.CreateManualAssetParams{
			SpaceID: space.ID, Kind: in.Kind, Name: in.Name, Icon: in.Icon, Currency: cur, CurrentValue: in.CurrentValue,
			CounterpartyName: in.Counterparty, DueDate: in.DueDate,
			Note: in.Note, CreatedByID: userID,
		})
		if err != nil {
			return err
		}
		return q.InsertValueChange(ctx, sqlc.InsertValueChangeParams{
			AssetID: a.ID, PreviousValue: decimal.Zero, NewValue: a.CurrentValue, Reason: ReasonManual,
		})
	})
	if err != nil {
		return Asset{}, err
	}
	return s.Get(ctx, space.ID, a.ID)
}

// ListResult is a list of assets with their converted totals per kind.
type ListResult struct {
	Items  []Asset
	Totals map[string]currency.AggregateResult
}

// List returns the space's assets (kind "" means all) and their totals in the base currency.
func (s *Service) List(ctx context.Context, spaceID uuid.UUID, kind string) (ListResult, error) {
	var k *string
	if kind != "" {
		k = &kind
	}
	rows, err := s.q.ListManualAssets(ctx, sqlc.ListManualAssetsParams{SpaceID: spaceID, Kind: k})
	if err != nil {
		return ListResult{}, err
	}
	cc, err := s.currency.BuildContext(ctx, spaceID)
	if err != nil {
		return ListResult{}, err
	}
	res := ListResult{Items: make([]Asset, 0, len(rows)), Totals: map[string]currency.AggregateResult{}}
	byKind := map[string][]currency.MoneyItem{}
	for _, r := range rows {
		res.Items = append(res.Items, Asset{ManualAsset: r})
		byKind[r.Kind] = append(byKind[r.Kind], currency.MoneyItem{Label: r.Name, Amount: r.CurrentValue, Currency: r.Currency})
	}
	for kind, items := range byKind {
		res.Totals[kind] = currency.Aggregate(items, cc)
	}
	return res, nil
}

// Totals sums the assets of each kind in the base currency (used by the net-worth summary).
func (s *Service) Totals(ctx context.Context, spaceID uuid.UUID, cc *currency.ConversionContext) (map[string]currency.AggregateResult, error) {
	rows, err := s.q.ListManualAssets(ctx, sqlc.ListManualAssetsParams{SpaceID: spaceID})
	if err != nil {
		return nil, err
	}
	byKind := map[string][]currency.MoneyItem{KindFixedAsset: nil, KindReceivable: nil, KindLiability: nil}
	for _, r := range rows {
		byKind[r.Kind] = append(byKind[r.Kind], currency.MoneyItem{Label: r.Name, Amount: r.CurrentValue, Currency: r.Currency})
	}
	out := map[string]currency.AggregateResult{}
	for kind, items := range byKind {
		out[kind] = currency.Aggregate(items, cc)
	}
	return out, nil
}

// Get returns one asset with its value history.
func (s *Service) Get(ctx context.Context, spaceID, id uuid.UUID) (Asset, error) {
	a, err := s.q.GetManualAsset(ctx, sqlc.GetManualAssetParams{ID: id, SpaceID: spaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Asset{}, errAssetNF
	}
	if err != nil {
		return Asset{}, err
	}
	h, err := s.q.ListValueChanges(ctx, id)
	return Asset{ManualAsset: a, History: h}, err
}

// Optional is a PATCH field that distinguishes "not sent" from an explicit null.
type Optional[T any] struct {
	Set   bool
	Value *T
}

// UpdateInput is a partial update.
type UpdateInput struct {
	Name, Icon, Currency *string
	CurrentValue         *decimal.Decimal
	Counterparty         Optional[string]
	DueDate              Optional[time.Time]
	Note                 Optional[string]
}

// Update applies a partial update; a changed value is recorded in the history.
func (s *Service) Update(ctx context.Context, spaceID, id uuid.UUID, in UpdateInput) (Asset, error) {
	var out sqlc.ManualAsset
	err := db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		cur, err := q.GetManualAssetForUpdate(ctx, sqlc.GetManualAssetForUpdateParams{ID: id, SpaceID: spaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return errAssetNF
		}
		if err != nil {
			return err
		}
		p := sqlc.UpdateManualAssetParams{
			ID: id, SpaceID: spaceID, Name: cur.Name, Icon: cur.Icon, Currency: cur.Currency, CurrentValue: cur.CurrentValue,
			CounterpartyName: cur.CounterpartyName, DueDate: cur.DueDate, Note: cur.Note,
		}
		if in.Name != nil {
			p.Name = *in.Name
		}
		if in.Icon != nil {
			p.Icon = *in.Icon
		}
		if in.Currency != nil && !strings.EqualFold(*in.Currency, strings.TrimSpace(cur.Currency)) {
			used, err := q.ManualAssetHasSettlements(ctx, uuid.NullUUID{UUID: id, Valid: true})
			if err != nil {
				return err
			}
			if used {
				return apperror.Unprocessable(CodeAssetCurrencyLocked, "The currency cannot change once the asset has settlements")
			}
			p.Currency = *in.Currency
		}
		if in.CurrentValue != nil {
			p.CurrentValue = *in.CurrentValue
		}
		if in.Counterparty.Set {
			p.CounterpartyName = in.Counterparty.Value
		}
		if in.DueDate.Set {
			p.DueDate = in.DueDate.Value
		}
		if in.Note.Set {
			p.Note = in.Note.Value
		}
		if out, err = q.UpdateManualAsset(ctx, p); err != nil {
			return err
		}
		if !p.CurrentValue.Equal(cur.CurrentValue) {
			return q.InsertValueChange(ctx, sqlc.InsertValueChangeParams{
				AssetID: id, PreviousValue: cur.CurrentValue, NewValue: p.CurrentValue, Reason: ReasonManual,
			})
		}
		return nil
	})
	if err != nil {
		return Asset{}, err
	}
	return s.Get(ctx, spaceID, out.ID)
}

// Delete soft-deletes an asset; its settlement transactions are kept for history.
func (s *Service) Delete(ctx context.Context, spaceID, id uuid.UUID) error {
	n, err := s.q.SoftDeleteManualAsset(ctx, sqlc.SoftDeleteManualAssetParams{ID: id, SpaceID: spaceID})
	if err != nil {
		return err
	}
	if n == 0 {
		return errAssetNF
	}
	return nil
}

// SettleInput describes a settlement payment.
type SettleInput struct {
	WalletID   uuid.UUID
	Amount     decimal.Decimal
	OccurredAt *time.Time
	Note       *string
}

// SettleResult is the settlement transaction and the asset after it.
type SettleResult struct {
	Transaction transactions.Detail
	Asset       Asset
}

// Settle records a payment against a receivable (money received back: wallet +amount,
// value −amount) or a liability (a debt payment: wallet −amount, outstanding −amount).
// It is a TRANSFER transaction tied to the asset; the ledger, the asset value, its
// history and the transaction row change together in one database transaction.
func (s *Service) Settle(ctx context.Context, spaceID, userID, id uuid.UUID, in SettleInput) (SettleResult, error) {
	var txnID uuid.UUID
	err := db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		a, err := q.GetManualAssetForUpdate(ctx, sqlc.GetManualAssetForUpdateParams{ID: id, SpaceID: spaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return errAssetNF
		}
		if err != nil {
			return err
		}
		if a.Kind != KindReceivable && a.Kind != KindLiability {
			return apperror.Unprocessable(CodeSettlementNotSupport, "Only money owed and debt can be settled")
		}
		w, err := q.GetWallet(ctx, sqlc.GetWalletParams{ID: in.WalletID, SpaceID: spaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return errWalletNF
		}
		if err != nil {
			return err
		}
		if !strings.EqualFold(strings.TrimSpace(w.Currency), strings.TrimSpace(a.Currency)) {
			return apperror.Unprocessable(CodeSettlementCurrency, "The wallet and the asset must use the same currency").
				WithDetails(map[string]any{"walletCurrency": strings.TrimSpace(w.Currency), "assetCurrency": strings.TrimSpace(a.Currency)})
		}
		if in.Amount.GreaterThan(a.CurrentValue) {
			return apperror.Unprocessable(CodeSettlementExceeds, fmt.Sprintf("The amount exceeds the outstanding balance of %s", a.CurrentValue.String())).
				WithDetails(map[string]any{"outstanding": a.CurrentValue.String()})
		}

		dir, title := ledger.SettlementOut, "Payment of "+a.Name
		if a.Kind == KindReceivable {
			dir = ledger.SettlementIn
			who := a.Name
			if a.CounterpartyName != nil {
				who = *a.CounterpartyName
			}
			title = "Repayment from " + who
		}
		occurred := s.now().UTC()
		if in.OccurredAt != nil {
			occurred = in.OccurredAt.UTC()
		}
		t, err := q.CreateTransaction(ctx, sqlc.CreateTransactionParams{
			SpaceID: spaceID, Type: ledger.TypeTransfer, Title: &title, Note: in.Note,
			Amount: in.Amount, Currency: w.Currency, WalletID: uuid.NullUUID{UUID: w.ID, Valid: true},
			OccurredAt: occurred, CreatedByID: userID,
			ManualAssetID: uuid.NullUUID{UUID: a.ID, Valid: true}, SettlementDirection: &dir,
		})
		if err != nil {
			return err
		}
		txnID = t.ID
		if err := ledger.Apply(ctx, tx, t); err != nil {
			return err
		}
		next := a.CurrentValue.Sub(in.Amount)
		if err := q.SetManualAssetValue(ctx, sqlc.SetManualAssetValueParams{ID: a.ID, CurrentValue: next}); err != nil {
			return err
		}
		return q.InsertValueChange(ctx, sqlc.InsertValueChangeParams{
			AssetID: a.ID, PreviousValue: a.CurrentValue, NewValue: next, Reason: ReasonSettlement,
			TransactionID: uuid.NullUUID{UUID: t.ID, Valid: true},
		})
	})
	if err != nil {
		return SettleResult{}, err
	}
	d, err := s.txn.Get(ctx, spaceID, txnID)
	if err != nil {
		return SettleResult{}, err
	}
	asset, err := s.Get(ctx, spaceID, id)
	return SettleResult{Transaction: d, Asset: asset}, err
}

// DeleteSettlement reverses a settlement: the wallet effect is undone, the
// asset's value goes back up by the settled amount and the transaction is
// removed. It works on settlements of a deleted asset too (only the wallet is restored).
// assetID, when not uuid.Nil, must be the asset the settlement belongs to.
func (s *Service) DeleteSettlement(ctx context.Context, spaceID, assetID, txnID uuid.UUID) error {
	return db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		old, err := q.GetTransactionForUpdate(ctx, sqlc.GetTransactionForUpdateParams{ID: txnID, SpaceID: spaceID})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (!old.ManualAssetID.Valid || (assetID != uuid.Nil && old.ManualAssetID.UUID != assetID))) {
			return ErrNotASettlement
		}
		if err != nil {
			return err
		}
		a, err := q.GetManualAssetAnyForUpdate(ctx, sqlc.GetManualAssetAnyForUpdateParams{ID: old.ManualAssetID.UUID, SpaceID: spaceID})
		if err != nil {
			return err
		}
		if err := ledger.Reverse(ctx, tx, old); err != nil {
			return err
		}
		if _, err := q.DeleteTransaction(ctx, sqlc.DeleteTransactionParams{ID: txnID, SpaceID: spaceID}); err != nil {
			return err
		}
		if a.DeletedAt != nil {
			return nil
		}
		next := a.CurrentValue.Add(old.Amount)
		if err := q.SetManualAssetValue(ctx, sqlc.SetManualAssetValueParams{ID: a.ID, CurrentValue: next}); err != nil {
			return err
		}
		return q.InsertValueChange(ctx, sqlc.InsertValueChangeParams{
			AssetID: a.ID, PreviousValue: a.CurrentValue, NewValue: next, Reason: ReasonSettlement,
		})
	})
}

// RevertValueChange undoes the newest value change of an asset: the value goes back to the change's previous value and the
// entry is removed, in one database transaction with the asset locked. Only the newest can go (a value change is a plain
// overwrite and a later one stores its own previous value), otherwise 409 VALUE_CHANGED_SINCE. A settlement entry answers
// 409 USE_SETTLEMENT_DELETE carrying the settlement's transaction id (details.transactionId) for the settlement delete, and
// the entry that created the asset answers 422 CANNOT_REVERT_INITIAL_VALUE.
func (s *Service) RevertValueChange(ctx context.Context, spaceID, assetID, changeID uuid.UUID) (Asset, error) {
	err := db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		a, err := q.GetManualAssetForUpdate(ctx, sqlc.GetManualAssetForUpdateParams{ID: assetID, SpaceID: spaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return errAssetNF
		}
		if err != nil {
			return err
		}
		c, err := q.GetValueChange(ctx, sqlc.GetValueChangeParams{ID: changeID, AssetID: assetID})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperror.New(404, CodeValueChangeNotFound, "Value change not found")
		}
		if err != nil {
			return err
		}
		if c.Reason == ReasonSettlement {
			e := apperror.Conflict(CodeUseSettlementDelete, "This entry belongs to a settlement: delete the settlement instead")
			if c.TransactionID.Valid {
				return e.WithDetails(map[string]any{"transactionId": c.TransactionID.UUID.String()})
			}
			return e.WithDetails(map[string]any{"transactionId": nil})
		}
		newest, err := q.NewestValueChange(ctx, assetID)
		if err != nil {
			return err
		}
		if newest.ID != c.ID {
			return apperror.Conflict(CodeValueChangedSince, "The value changed again after this entry; revert the newer ones first")
		}
		history, err := q.ListValueChanges(ctx, assetID)
		if err != nil {
			return err
		}
		if len(history) == 1 {
			return apperror.Unprocessable(CodeCannotRevertInitial, "The first value of an asset cannot be reverted; delete the asset instead")
		}
		if err := q.SetManualAssetValue(ctx, sqlc.SetManualAssetValueParams{ID: a.ID, CurrentValue: c.PreviousValue}); err != nil {
			return err
		}
		return q.DeleteValueChange(ctx, sqlc.DeleteValueChangeParams{ID: c.ID, AssetID: assetID})
	})
	if err != nil {
		return Asset{}, err
	}
	return s.Get(ctx, spaceID, assetID)
}
