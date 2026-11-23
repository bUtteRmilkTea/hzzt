// Package transactions implements income/expense transactions, wallet top-ups,
// filtered history, daily summaries and category usage. Balance changes always
// go through package ledger inside one database transaction.
package transactions

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
	"github.com/JonathanRainee/hzzt/internal/platform/httpx"
	"github.com/JonathanRainee/hzzt/internal/platform/period"
)

// Error codes owned by this package (see plan Appendix B).
const (
	CodeTransactionNotFound  = "TRANSACTION_NOT_FOUND"
	CodeCategoryTypeMismatch = "CATEGORY_TYPE_MISMATCH"
	CodeUseSpecificEndpoint  = "USE_SPECIFIC_ENDPOINT"
	CodeTypeChangeNotAllowed = "TYPE_CHANGE_NOT_ALLOWED"
	codeWalletNotFound       = "WALLET_NOT_FOUND"
	codeCategoryNotFound     = "CATEGORY_NOT_FOUND"
)

var (
	errTxnNF        = apperror.New(404, CodeTransactionNotFound, "Transaction not found")
	errWalletNF     = apperror.New(404, codeWalletNotFound, "Wallet not found")
	errCategoryNF   = apperror.New(404, codeCategoryNotFound, "Category not found")
	errTypeMismatch = apperror.Unprocessable(CodeCategoryTypeMismatch, "The category type does not match the transaction type")
)

func typeChangeNotAllowed(msg string) error {
	return apperror.Unprocessable(CodeTypeChangeNotAllowed, msg)
}

func useSpecific(what string) error {
	return apperror.Unprocessable(CodeUseSpecificEndpoint, what+" transactions are managed through their own endpoint")
}

// DB is what the service needs; *pgxpool.Pool and pgx.Tx both satisfy it.
type DB interface {
	sqlc.DBTX
	db.TxStarter
}

// Service holds transaction logic.
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

// Detail is a transaction with the names/icons of what it points at.
type Detail = sqlc.GetTransactionDetailRow

// CategoryInUse reports whether any transaction uses the category; it is the
// categories.UsageChecker that locks a category's type (CATEGORY_TYPE_LOCKED).
func (s *Service) CategoryInUse(ctx context.Context, spaceID, categoryID uuid.UUID) (bool, error) {
	return s.q.CategoryInUse(ctx, sqlc.CategoryInUseParams{SpaceID: spaceID, CategoryID: uuid.NullUUID{UUID: categoryID, Valid: true}})
}

func nu(id uuid.UUID) uuid.NullUUID { return uuid.NullUUID{UUID: id, Valid: true} }

func cleanText(s *string) *string {
	if s == nil {
		return nil
	}
	t := strings.TrimSpace(*s)
	if t == "" {
		return nil
	}
	return &t
}

// loadWallet returns a live wallet of the space or WALLET_NOT_FOUND.
func loadWallet(ctx context.Context, q *sqlc.Queries, spaceID, id uuid.UUID) (sqlc.Wallet, error) {
	w, err := q.GetWallet(ctx, sqlc.GetWalletParams{ID: id, SpaceID: spaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return w, errWalletNF
	}
	return w, err
}

// checkCategory validates that the category is a live one of the space whose type matches typ.
func checkCategory(ctx context.Context, q *sqlc.Queries, spaceID, id uuid.UUID, typ string) error {
	c, err := q.GetCategory(ctx, sqlc.GetCategoryParams{ID: id, SpaceID: spaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return errCategoryNF
	}
	if err != nil {
		return err
	}
	if c.Type != typ { // category types are the strings INCOME / EXPENSE, same as transaction types
		return errTypeMismatch
	}
	return nil
}

// CreateInput describes a new INCOME or EXPENSE transaction.
type CreateInput struct {
	Type       string
	WalletID   uuid.UUID
	Amount     decimal.Decimal
	CategoryID *uuid.UUID
	Title      *string
	Note       *string
	OccurredAt *time.Time
}

// Create records an income or expense and applies it to the wallet.
func (s *Service) Create(ctx context.Context, spaceID, userID uuid.UUID, in CreateInput) (Detail, error) {
	var id uuid.UUID
	err := db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		w, err := loadWallet(ctx, q, spaceID, in.WalletID)
		if err != nil {
			return err
		}
		p := sqlc.CreateTransactionParams{
			SpaceID: spaceID, Type: in.Type, Title: cleanText(in.Title), Note: cleanText(in.Note),
			Amount: in.Amount, Currency: w.Currency, WalletID: nu(w.ID),
			OccurredAt: s.now().UTC(), CreatedByID: userID,
		}
		if in.OccurredAt != nil {
			p.OccurredAt = in.OccurredAt.UTC()
		}
		if in.CategoryID != nil {
			if err := checkCategory(ctx, q, spaceID, *in.CategoryID, in.Type); err != nil {
				return err
			}
			p.CategoryID = nu(*in.CategoryID)
		}
		t, err := q.CreateTransaction(ctx, p)
		if err != nil {
			return fmt.Errorf("create transaction: %w", err)
		}
		id = t.ID
		return ledger.Apply(ctx, tx, t)
	})
	if err != nil {
		return Detail{}, err
	}
	return s.Get(ctx, spaceID, id)
}

// System keys of the transactions the backend writes itself (see sqlc transactions.system_key).
const (
	SystemKeyOpeningBalance    = "OPENING_BALANCE"
	SystemKeyBalanceTopUp      = "BALANCE_TOP_UP"
	SystemKeyBalanceAdjustment = "BALANCE_ADJUSTMENT"
)

// TopUp adds money to a wallet. It is an ADJUSTMENT IN without a category, not an
// INCOME: money put into a wallet is not earnings, so it never reaches the income totals.
func (s *Service) TopUp(ctx context.Context, spaceID, userID, walletID uuid.UUID, amount decimal.Decimal, note *string) (Detail, error) {
	var id uuid.UUID
	err := db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		w, err := loadWallet(ctx, q, spaceID, walletID)
		if err != nil {
			return err
		}
		title, dir, key := "Balance top up", ledger.DirectionIn, SystemKeyBalanceTopUp
		t, err := q.CreateTransaction(ctx, sqlc.CreateTransactionParams{
			SpaceID: spaceID, Type: ledger.TypeAdjustment, Direction: &dir, Title: &title, Note: cleanText(note),
			Amount: amount, Currency: w.Currency, WalletID: nu(w.ID),
			OccurredAt: s.now().UTC(), CreatedByID: userID, SystemKey: &key,
		})
		if err != nil {
			return fmt.Errorf("create top up: %w", err)
		}
		id = t.ID
		return ledger.Apply(ctx, tx, t)
	})
	if err != nil {
		return Detail{}, err
	}
	return s.Get(ctx, spaceID, id)
}

// Get returns one transaction of the space.
func (s *Service) Get(ctx context.Context, spaceID, id uuid.UUID) (Detail, error) {
	d, err := s.q.GetTransactionDetail(ctx, sqlc.GetTransactionDetailParams{ID: id, SpaceID: spaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return d, errTxnNF
	}
	return d, err
}

// Optional is a PATCH field that distinguishes "not sent" from an explicit null.
type Optional[T any] struct {
	Set   bool
	Value *T
}

// UpdateInput is a partial update; nil / unset fields stay as they are.
type UpdateInput struct {
	Type       *string
	WalletID   *uuid.UUID
	Amount     *decimal.Decimal
	CategoryID Optional[uuid.UUID]
	Title      Optional[string]
	Note       Optional[string]
	OccurredAt *time.Time
}

func (u UpdateInput) onlyNote() bool {
	return u.Type == nil && u.WalletID == nil && u.Amount == nil && !u.CategoryID.Set && !u.Title.Set && u.OccurredAt == nil
}

// Update edits a transaction. For INCOME/EXPENSE the old effect is reversed
// and the new one applied in the same database transaction.
func (s *Service) Update(ctx context.Context, spaceID, id uuid.UUID, in UpdateInput) (Detail, error) {
	err := db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		old, err := q.GetTransactionForUpdate(ctx, sqlc.GetTransactionForUpdateParams{ID: id, SpaceID: spaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return errTxnNF
		}
		if err != nil {
			return err
		}
		if what := specialKind(old); what != "" {
			return useSpecific(what)
		}
		if in.Type != nil && (*in.Type != old.Type || old.Type == ledger.TypeAdjustment || old.Type == ledger.TypeOpeningBalance) {
			return s.changeType(ctx, tx, q, old, in)
		}
		if old.Type == ledger.TypeAdjustment || old.Type == ledger.TypeOpeningBalance {
			if !in.onlyNote() {
				return apperror.Validation("Validation failed", map[string]any{
					"body": "only the note can be edited on " + old.Type + " transactions",
				})
			}
			_, err := q.UpdateTransactionNote(ctx, sqlc.UpdateTransactionNoteParams{ID: id, SpaceID: spaceID, Note: finalText(in.Note, old.Note)})
			return err
		}

		return s.updatePlain(ctx, tx, q, old, in)
	})
	if err != nil {
		return Detail{}, err
	}
	return s.Get(ctx, spaceID, id)
}

// updatePlain edits an INCOME / EXPENSE: the old effect is reversed and the new one applied.
func (s *Service) updatePlain(ctx context.Context, tx pgx.Tx, q *sqlc.Queries, old sqlc.Transaction, in UpdateInput) error {
	typ := old.Type
	if in.Type != nil {
		typ = *in.Type
	}
	walletID := old.WalletID.UUID
	if in.WalletID != nil {
		walletID = *in.WalletID
	}
	w, err := loadWallet(ctx, q, old.SpaceID, walletID)
	if err != nil {
		return err
	}
	amount := old.Amount
	if in.Amount != nil {
		amount = *in.Amount
	}
	occurred := old.OccurredAt
	if in.OccurredAt != nil {
		occurred = in.OccurredAt.UTC()
	}
	category := old.CategoryID
	if in.CategoryID.Set {
		category = uuid.NullUUID{}
		if in.CategoryID.Value != nil {
			category = nu(*in.CategoryID.Value)
		}
	}
	if category.Valid {
		if err := checkCategory(ctx, q, old.SpaceID, category.UUID, typ); err != nil {
			return err
		}
	}

	if err := ledger.Reverse(ctx, tx, old); err != nil {
		return err
	}
	updated, err := q.UpdateIncomeExpense(ctx, sqlc.UpdateIncomeExpenseParams{
		ID: old.ID, SpaceID: old.SpaceID, Type: typ, WalletID: nu(walletID), Amount: amount, Currency: w.Currency,
		CategoryID: category, Title: finalText(in.Title, old.Title), Note: finalText(in.Note, old.Note), OccurredAt: occurred,
	})
	if err != nil {
		return err
	}
	return ledger.Apply(ctx, tx, updated)
}

// changeType handles a PATCH whose type differs from the stored one. The only change of kind
// that is not INCOME <-> EXPENSE is an explicit "this is not income/expense" action: an
// uncategorised INCOME or EXPENSE becomes an ADJUSTMENT (IN or OUT, same sign and amount, so the
// wallet balance does not move and no ledger reversal is needed). It is never guessed from the title.
func (s *Service) changeType(ctx context.Context, tx pgx.Tx, q *sqlc.Queries, old sqlc.Transaction, in UpdateInput) error {
	target := *in.Type
	isPlain := old.Type == ledger.TypeIncome || old.Type == ledger.TypeExpense
	switch {
	case target == ledger.TypeAdjustment && isPlain:
		if in.WalletID != nil || in.Amount != nil || in.CategoryID.Set || in.Title.Set || in.OccurredAt != nil {
			return apperror.Validation("Validation failed", map[string]any{
				"body": "only the note can be sent together with type ADJUSTMENT",
			})
		}
		if old.CategoryID.Valid {
			return typeChangeNotAllowed("A categorised transaction cannot become an adjustment: remove the category first")
		}
		dir := ledger.DirectionIn
		if old.Type == ledger.TypeExpense {
			dir = ledger.DirectionOut
		}
		if _, err := q.ReclassifyToAdjustment(ctx, sqlc.ReclassifyToAdjustmentParams{ID: old.ID, SpaceID: old.SpaceID, Direction: &dir}); err != nil {
			return err
		}
		if in.Note.Set {
			_, err := q.UpdateTransactionNote(ctx, sqlc.UpdateTransactionNoteParams{ID: old.ID, SpaceID: old.SpaceID, Note: cleanText(in.Note.Value)})
			return err
		}
		return nil
	case isPlain && (target == ledger.TypeIncome || target == ledger.TypeExpense):
		return s.updatePlain(ctx, tx, q, old, in)
	default:
		return typeChangeNotAllowed("A " + old.Type + " transaction cannot be changed to " + target)
	}
}

func finalText(o Optional[string], old *string) *string {
	if !o.Set {
		return old
	}
	return cleanText(o.Value)
}

// specialKind names transactions that are not plain income/expense and have their own endpoints.
func specialKind(t sqlc.Transaction) string {
	switch {
	case t.Type == ledger.TypeTransfer:
		return "Transfer"
	case t.InvestmentID.Valid:
		return "Investment"
	case t.ManualAssetID.Valid:
		return "Manual asset"
	}
	return ""
}

// Delete reverses the transaction's effect and removes it.
func (s *Service) Delete(ctx context.Context, spaceID, id uuid.UUID) error {
	return db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		old, err := q.GetTransactionForUpdate(ctx, sqlc.GetTransactionForUpdateParams{ID: id, SpaceID: spaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return errTxnNF
		}
		if err != nil {
			return err
		}
		if what := specialKind(old); what != "" {
			return useSpecific(what)
		}
		if err := ledger.Reverse(ctx, tx, old); err != nil {
			return err
		}
		_, err = q.DeleteTransaction(ctx, sqlc.DeleteTransactionParams{ID: id, SpaceID: spaceID})
		return err
	})
}

// Filter selects transactions; every set field narrows the result (AND across
// fields, OR within a list).
type Filter struct {
	Range       period.Range
	Types       []string
	CategoryIDs []uuid.UUID
	WalletIDs   []uuid.UUID
	Q           string
}

// likePattern turns user text into an ILIKE pattern, escaping wildcards.
func likePattern(q string) *string {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil
	}
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	p := "%" + r.Replace(q) + "%"
	return &p
}

func nilIfEmpty[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	return s
}

// ListResult is one page of the history.
type ListResult struct {
	Items      []Detail
	NextCursor string
	// Total is how many transactions match the filters in all, ignoring the cursor and the page size.
	Total int
}

// List returns transactions newest first (occurred_at desc, id desc) with keyset pagination.
func (s *Service) List(ctx context.Context, spaceID uuid.UUID, f Filter, cursor string, limit int) (ListResult, error) {
	p := sqlc.ListTransactionsParams{
		SpaceID: spaceID, FromTs: f.Range.From, ToTs: f.Range.To, Types: nilIfEmpty(f.Types),
		CategoryIds: nilIfEmpty(f.CategoryIDs), WalletIds: nilIfEmpty(f.WalletIDs), Q: likePattern(f.Q),
		RowLimit: int32(limit + 1),
	}
	if cursor != "" {
		c, err := httpx.DecodeCursor(cursor)
		if err != nil {
			return ListResult{}, apperror.Validation("Validation failed", map[string]any{"cursor": "is invalid"})
		}
		at := c.CreatedAt
		p.CursorAt, p.CursorID = &at, nu(c.ID)
	}
	rows, err := s.q.ListTransactions(ctx, p)
	if err != nil {
		return ListResult{}, err
	}
	total, err := s.q.CountTransactions(ctx, sqlc.CountTransactionsParams{
		SpaceID: spaceID, FromTs: f.Range.From, ToTs: f.Range.To, Types: nilIfEmpty(f.Types),
		CategoryIds: nilIfEmpty(f.CategoryIDs), WalletIds: nilIfEmpty(f.WalletIDs), Q: likePattern(f.Q),
	})
	if err != nil {
		return ListResult{}, err
	}
	res := ListResult{Items: make([]Detail, 0, len(rows)), Total: int(total)}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[limit-1]
		res.NextCursor = httpx.EncodeCursor(httpx.Cursor{CreatedAt: last.OccurredAt, ID: last.ID})
	}
	for _, r := range rows {
		res.Items = append(res.Items, Detail(r))
	}
	return res, nil
}

// DayTotals is one local date's totals in the base currency.
type DayTotals struct {
	Date     string
	Income   decimal.Decimal
	Expense  decimal.Decimal
	Transfer decimal.Decimal
	Count    int
}

// SummaryResult is the daily summary for a filter.
type SummaryResult struct {
	Currency string
	Days     []DayTotals
	Warnings []currency.Warning
}

// DailySummary groups the filtered transactions by the user's local date and
// converts each day's income, expense and transfer totals to the base currency.
func (s *Service) DailySummary(ctx context.Context, spaceID uuid.UUID, f Filter, tz *time.Location) (SummaryResult, error) {
	cc, err := s.currency.BuildContext(ctx, spaceID)
	if err != nil {
		return SummaryResult{}, err
	}
	rows, err := s.q.DailySummaryRows(ctx, sqlc.DailySummaryRowsParams{
		SpaceID: spaceID, Tz: tz.String(), FromTs: f.Range.From, ToTs: f.Range.To, Types: nilIfEmpty(f.Types),
		CategoryIds: nilIfEmpty(f.CategoryIDs), WalletIds: nilIfEmpty(f.WalletIDs), Q: likePattern(f.Q),
	})
	if err != nil {
		return SummaryResult{}, err
	}

	type bucket struct{ income, expense, transfer []currency.MoneyItem }
	byDay := map[string]*bucket{}
	counts := map[string]int{}
	var order []string
	for _, r := range rows {
		day := r.Day.Format("2006-01-02")
		b := byDay[day]
		if b == nil {
			b = &bucket{}
			byDay[day] = b
			order = append(order, day) // rows arrive newest day first
		}
		counts[day] += int(r.Cnt)
		item := currency.MoneyItem{Amount: r.Total, Currency: r.Currency} // no label: identical warnings collapse
		switch r.Type {
		case ledger.TypeIncome:
			b.income = append(b.income, item)
		case ledger.TypeExpense:
			b.expense = append(b.expense, item)
		case ledger.TypeTransfer:
			b.transfer = append(b.transfer, item)
		}
	}

	res := SummaryResult{Currency: strings.TrimSpace(cc.BaseCurrency), Days: make([]DayTotals, 0, len(order)), Warnings: []currency.Warning{}}
	seen := map[string]bool{}
	sum := func(items []currency.MoneyItem) decimal.Decimal {
		agg := currency.Aggregate(items, cc)
		for _, w := range agg.Warnings {
			if !seen[w.Message] {
				seen[w.Message] = true
				res.Warnings = append(res.Warnings, w)
			}
		}
		return agg.Total
	}
	for _, day := range order {
		b := byDay[day]
		res.Days = append(res.Days, DayTotals{
			Date: day, Income: sum(b.income), Expense: sum(b.expense), Transfer: sum(b.transfer), Count: counts[day],
		})
	}
	return res, nil
}
