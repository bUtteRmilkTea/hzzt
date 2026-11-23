// Package dashboard feeds the home screen: the period overview, the category
// breakdown, the analytics bars and the calendar.
package dashboard

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/JonathanRainee/hzzt/internal/currency"
	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
	"github.com/JonathanRainee/hzzt/internal/platform/period"
)

// Transaction types the dashboard sums; transfers, adjustments and opening
// balances are neither income nor expense.
const (
	typeIncome  = "INCOME"
	typeExpense = "EXPENSE"
)

// Service computes the dashboard figures.
type Service struct {
	q        *sqlc.Queries
	currency *currency.Service
	now      func() time.Time
}

// NewService builds the service; now may be nil.
func NewService(database sqlc.DBTX, cur *currency.Service, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{q: sqlc.New(database), currency: cur, now: now}
}

// Selection is the period a request asks for: a preset or a custom date range.
type Selection struct {
	Range period.Range
	// Preset is the resolved preset ("" for a custom range).
	Preset string
	// Previous is the period right before, when there is one.
	Previous    period.Range
	HasPrevious bool
}

// Totals are income and expense in the base currency for one period.
type Totals struct {
	Income           decimal.Decimal
	Expense          decimal.Decimal
	TransactionCount int
}

// Net is income − expense.
func (t Totals) Net() decimal.Decimal { return t.Income.Sub(t.Expense) }

// Overview compares a period with the one before it.
type Overview struct {
	Currency string
	Range    period.Range
	Current  Totals
	Previous *Totals
	// IncomePct and ExpensePct are percentage changes against the previous period;
	// nil when there is no previous period or its value is 0.
	IncomePct  *decimal.Decimal
	ExpensePct *decimal.Decimal
	NetPct     *decimal.Decimal
	Warnings   []currency.Warning
}

func pctChange(cur, prev decimal.Decimal) *decimal.Decimal {
	if prev.IsZero() {
		return nil
	}
	v := cur.Sub(prev).Div(prev.Abs()).Mul(decimal.NewFromInt(100)).Round(2)
	return &v
}

type warnSet struct {
	seen map[string]bool
	out  []currency.Warning
}

func newWarnSet() *warnSet { return &warnSet{seen: map[string]bool{}, out: []currency.Warning{}} }

func (w *warnSet) add(ws []currency.Warning) {
	for _, x := range ws {
		if !w.seen[x.Message] {
			w.seen[x.Message] = true
			w.out = append(w.out, x)
		}
	}
}

func (s *Service) totals(ctx context.Context, spaceID uuid.UUID, r period.Range, cc *currency.ConversionContext, w *warnSet) (Totals, error) {
	rows, err := s.q.OverviewRows(ctx, sqlc.OverviewRowsParams{SpaceID: spaceID, FromTs: r.From, ToTs: r.To})
	if err != nil {
		return Totals{}, err
	}
	var inc, exp []currency.MoneyItem
	var t Totals
	for _, row := range rows {
		it := currency.MoneyItem{Amount: row.Total, Currency: row.Currency}
		if row.Type == typeIncome {
			inc = append(inc, it)
		} else {
			exp = append(exp, it)
		}
		t.TransactionCount += int(row.Cnt)
	}
	ai, ae := currency.Aggregate(inc, cc), currency.Aggregate(exp, cc)
	w.add(ai.Warnings)
	w.add(ae.Warnings)
	t.Income, t.Expense = ai.Total, ae.Total
	return t, nil
}

// Overview returns the period's income, expense and transaction count, and the
// same for the previous period with percentage changes.
func (s *Service) Overview(ctx context.Context, space sqlc.Space, sel Selection) (Overview, error) {
	cc, err := s.currency.BuildContext(ctx, space.ID)
	if err != nil {
		return Overview{}, err
	}
	w := newWarnSet()
	cur, err := s.totals(ctx, space.ID, sel.Range, cc, w)
	if err != nil {
		return Overview{}, err
	}
	out := Overview{Currency: strings.TrimSpace(space.BaseCurrency), Range: sel.Range, Current: cur}
	if sel.HasPrevious {
		prev, err := s.totals(ctx, space.ID, sel.Previous, cc, w)
		if err != nil {
			return Overview{}, err
		}
		out.Previous = &prev
		out.IncomePct = pctChange(cur.Income, prev.Income)
		out.ExpensePct = pctChange(cur.Expense, prev.Expense)
		out.NetPct = pctChange(cur.Net(), prev.Net())
	}
	out.Warnings = w.out
	return out, nil
}

// CategoryShare is one slice of the breakdown.
type CategoryShare struct {
	CategoryID *uuid.UUID
	Name       string
	Icon       string
	Color      string
	Amount     decimal.Decimal
	Pct        decimal.Decimal
	Count      int
}

// Breakdown splits a period's income or expense by category.
type Breakdown struct {
	Currency string
	Type     string
	Range    period.Range
	Total    decimal.Decimal
	Items    []CategoryShare
	// Uncategorized holds transactions without a category (nil when there are none).
	Uncategorized *CategoryShare
	Warnings      []currency.Warning
}

// CategoryBreakdown totals a transaction type per category (typ is INCOME or
// EXPENSE), largest first, with each category's share of the total.
func (s *Service) CategoryBreakdown(ctx context.Context, space sqlc.Space, typ string, r period.Range) (Breakdown, error) {
	cc, err := s.currency.BuildContext(ctx, space.ID)
	if err != nil {
		return Breakdown{}, err
	}
	rows, err := s.q.BreakdownRows(ctx, sqlc.BreakdownRowsParams{SpaceID: space.ID, Type: typ, FromTs: r.From, ToTs: r.To})
	if err != nil {
		return Breakdown{}, err
	}
	type acc struct {
		share CategoryShare
		items []currency.MoneyItem
	}
	by := map[uuid.NullUUID]*acc{}
	var order []uuid.NullUUID
	for _, row := range rows {
		a := by[row.CategoryID]
		if a == nil {
			a = &acc{}
			if row.CategoryID.Valid {
				id := row.CategoryID.UUID
				a.share.CategoryID = &id
				if row.CategoryName != nil {
					a.share.Name = *row.CategoryName
				}
				if row.CategoryIcon != nil {
					a.share.Icon = *row.CategoryIcon
				}
				if row.CategoryColor != nil {
					a.share.Color = *row.CategoryColor
				}
			}
			by[row.CategoryID] = a
			order = append(order, row.CategoryID)
		}
		a.items = append(a.items, currency.MoneyItem{Amount: row.Total, Currency: row.Currency})
		a.share.Count += int(row.Cnt)
	}

	w := newWarnSet()
	out := Breakdown{Currency: strings.TrimSpace(space.BaseCurrency), Type: typ, Range: r, Items: []CategoryShare{}}
	var shares []CategoryShare
	for _, k := range order {
		a := by[k]
		agg := currency.Aggregate(a.items, cc)
		w.add(agg.Warnings)
		a.share.Amount = agg.Total
		out.Total = out.Total.Add(agg.Total)
		shares = append(shares, a.share)
	}
	hundred := decimal.NewFromInt(100)
	for i := range shares {
		if out.Total.IsPositive() {
			shares[i].Pct = shares[i].Amount.Div(out.Total).Mul(hundred).Round(2)
		}
	}
	sort.SliceStable(shares, func(i, j int) bool {
		if !shares[i].Amount.Equal(shares[j].Amount) {
			return shares[i].Amount.GreaterThan(shares[j].Amount)
		}
		return shares[i].Name < shares[j].Name
	})
	for i := range shares {
		if shares[i].CategoryID == nil {
			u := shares[i]
			u.Name = "Uncategorized"
			out.Uncategorized = &u
			continue
		}
		out.Items = append(out.Items, shares[i])
	}
	out.Warnings = w.out
	return out, nil
}

// BucketTotals is one bar of the analytics chart.
type BucketTotals struct {
	Bucket
	Income  decimal.Decimal
	Expense decimal.Decimal
}

// Analytics holds the bars of a period.
type Analytics struct {
	Currency string
	Range    period.Range
	Buckets  []BucketTotals
	Warnings []currency.Warning
}

func (s *Service) dayRows(ctx context.Context, spaceID uuid.UUID, r period.Range, tz *time.Location) ([]sqlc.DailySummaryRowsRow, error) {
	return s.q.DailySummaryRows(ctx, sqlc.DailySummaryRowsParams{
		SpaceID: spaceID, Tz: tz.String(), FromTs: r.From, ToTs: r.To, Types: []string{typeIncome, typeExpense},
	})
}

// Analytics sums income and expense per bucket (see BuildBuckets), in the base currency.
func (s *Service) Analytics(ctx context.Context, space sqlc.Space, r period.Range, tz *time.Location) (Analytics, error) {
	cc, err := s.currency.BuildContext(ctx, space.ID)
	if err != nil {
		return Analytics{}, err
	}
	var first *time.Time
	if r.From == nil {
		t, err := s.q.FirstTransactionAt(ctx, space.ID)
		if err != nil {
			return Analytics{}, err
		}
		if ts, ok := t.(time.Time); ok {
			first = &ts
		}
	}
	buckets := BuildBuckets(r, tz, s.now(), first)
	rows, err := s.dayRows(ctx, space.ID, r, tz)
	if err != nil {
		return Analytics{}, err
	}
	inc := make([][]currency.MoneyItem, len(buckets))
	exp := make([][]currency.MoneyItem, len(buckets))
	for _, row := range rows {
		i := indexOf(buckets, row.Day)
		if i < 0 {
			continue
		}
		it := currency.MoneyItem{Amount: row.Total, Currency: row.Currency}
		switch row.Type {
		case typeIncome:
			inc[i] = append(inc[i], it)
		case typeExpense:
			exp[i] = append(exp[i], it)
		default: // transfers, trades, adjustments and opening balances never enter a bucket
			continue
		}
	}
	w := newWarnSet()
	out := Analytics{Currency: strings.TrimSpace(space.BaseCurrency), Range: r, Buckets: make([]BucketTotals, 0, len(buckets))}
	for i, b := range buckets {
		ai, ae := currency.Aggregate(inc[i], cc), currency.Aggregate(exp[i], cc)
		w.add(ai.Warnings)
		w.add(ae.Warnings)
		out.Buckets = append(out.Buckets, BucketTotals{Bucket: b, Income: ai.Total, Expense: ae.Total})
	}
	out.Warnings = w.out
	return out, nil
}

// Day is one local day of the calendar.
type Day struct {
	Date    string
	Income  decimal.Decimal
	Expense decimal.Decimal
	Count   int
}

// Net is income − expense.
func (d Day) Net() decimal.Decimal { return d.Income.Sub(d.Expense) }

// CalendarMonth lists every local day of a month with its income, expense and transaction count.
type CalendarMonth struct {
	Currency string
	Month    string
	Days     []Day
	Warnings []currency.Warning
}

// Calendar groups a month's transactions by the user's local date.
func (s *Service) Calendar(ctx context.Context, space sqlc.Space, year int, month time.Month, tz *time.Location) (CalendarMonth, error) {
	cc, err := s.currency.BuildContext(ctx, space.ID)
	if err != nil {
		return CalendarMonth{}, err
	}
	from := time.Date(year, month, 1, 0, 0, 0, 0, tz).UTC()
	to := time.Date(year, month+1, 1, 0, 0, 0, 0, tz).UTC()
	rows, err := s.dayRows(ctx, space.ID, period.Range{From: &from, To: &to}, tz)
	if err != nil {
		return CalendarMonth{}, err
	}
	n := time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
	inc := make([][]currency.MoneyItem, n+1)
	exp := make([][]currency.MoneyItem, n+1)
	counts := make([]int, n+1)
	for _, row := range rows {
		d := row.Day.Day()
		if row.Day.Month() != month || d < 1 || d > n {
			continue
		}
		it := currency.MoneyItem{Amount: row.Total, Currency: row.Currency}
		switch row.Type {
		case typeIncome:
			inc[d] = append(inc[d], it)
		case typeExpense:
			exp[d] = append(exp[d], it)
		default: // only income and expense count on the calendar
			continue
		}
		counts[d] += int(row.Cnt)
	}
	w := newWarnSet()
	out := CalendarMonth{Currency: strings.TrimSpace(space.BaseCurrency), Month: time.Date(year, month, 1, 0, 0, 0, 0, time.UTC).Format("2006-01"), Days: make([]Day, 0, n)}
	for d := 1; d <= n; d++ {
		ai, ae := currency.Aggregate(inc[d], cc), currency.Aggregate(exp[d], cc)
		w.add(ai.Warnings)
		w.add(ae.Warnings)
		out.Days = append(out.Days, Day{Date: time.Date(year, month, d, 0, 0, 0, 0, time.UTC).Format("2006-01-02"), Income: ai.Total, Expense: ae.Total, Count: counts[d]})
	}
	out.Warnings = w.out
	return out, nil
}
