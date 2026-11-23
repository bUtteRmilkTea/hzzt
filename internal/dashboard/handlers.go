package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/JonathanRainee/hzzt/internal/auth"
	"github.com/JonathanRainee/hzzt/internal/currency"
	"github.com/JonathanRainee/hzzt/internal/platform/apperror"
	"github.com/JonathanRainee/hzzt/internal/platform/httpx"
	"github.com/JonathanRainee/hzzt/internal/platform/money"
	"github.com/JonathanRainee/hzzt/internal/platform/period"
	"github.com/JonathanRainee/hzzt/internal/spaces"
)

// Handler exposes the dashboard endpoints.
type Handler struct {
	svc    *Service
	auth   *auth.Service
	spaces *spaces.Service
}

// NewHandler creates the HTTP layer.
func NewHandler(svc *Service, authSvc *auth.Service, spacesSvc *spaces.Service) *Handler {
	return &Handler{svc: svc, auth: authSvc, spaces: spacesSvc}
}

// Mount registers the routes on r (the /api/v1 router). Space routes carry the
// full path because the spaces handler owns the /spaces/{spaceId} mount point.
func (h *Handler) Mount(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(h.auth.Middleware, h.spaces.Middleware)
		const p = "/spaces/{spaceId}/dashboard"
		r.Get(p+"/overview", h.overview)
		r.Get(p+"/category-breakdown", h.breakdown)
		r.Get(p+"/analytics", h.analytics)
		r.Get(p+"/calendar", h.calendar)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func locationOf(tz string) *time.Location {
	if l, err := time.LoadLocation(tz); err == nil {
		return l
	}
	return time.UTC
}

// selection resolves `period` (default THIS_PERIOD) or `from`/`to` in the user's
// timezone and the space's budget period start day, and the previous period.
func (h *Handler) selection(r *http.Request, defaultPreset string) (Selection, error) {
	user, _ := auth.UserFromContext(r.Context())
	sp, _ := spaces.SpaceFromContext(r.Context())
	loc := locationOf(user.Timezone)
	v := r.URL.Query()
	opts := period.Opts{Location: loc, BudgetStartDay: int(sp.BudgetPeriodStartDay), Now: h.svc.now()}

	if from, to := strings.TrimSpace(v.Get("from")), strings.TrimSpace(v.Get("to")); from != "" || to != "" {
		rng, err := period.Custom(from, to, loc)
		if err != nil {
			return Selection{}, apperror.Validation("Validation failed", map[string]any{"from": err.Error()})
		}
		prev, ok := period.PreviousOfRange(rng)
		return Selection{Range: rng, Previous: prev, HasPrevious: ok}, nil
	}
	preset := strings.ToUpper(strings.TrimSpace(v.Get("period")))
	if preset == "" {
		preset = defaultPreset
	}
	rng, err := period.Resolve(preset, opts)
	if err != nil {
		return Selection{}, apperror.Validation("Validation failed", map[string]any{"period": "must be one of: " + strings.Join(period.Presets, " ")})
	}
	prev, ok, err := period.Previous(preset, opts)
	if err != nil {
		return Selection{}, err
	}
	return Selection{Range: rng, Preset: preset, Previous: prev, HasPrevious: ok}, nil
}

// PeriodDTO is the resolved time range of a response (UTC instants; `to` is exclusive).
type PeriodDTO struct {
	From *time.Time `json:"from"`
	To   *time.Time `json:"to"`
}

func periodDTO(r period.Range) PeriodDTO { return PeriodDTO{From: r.From, To: r.To} }

// TotalsDTO is income, expense and the transaction count of one period.
type TotalsDTO struct {
	Income           money.Amount `json:"income" swaggertype:"string"`
	Expense          money.Amount `json:"expense" swaggertype:"string"`
	Net              money.Amount `json:"net" swaggertype:"string"`
	TransactionCount int          `json:"transactionCount"`
}

func totalsDTO(t Totals, cur string) TotalsDTO {
	return TotalsDTO{Income: money.NewAmount(t.Income, cur), Expense: money.NewAmount(t.Expense, cur), Net: money.NewAmount(t.Net(), cur), TransactionCount: t.TransactionCount}
}

// ChangeDTO holds percentage changes against the previous period (null when the previous value is 0).
type ChangeDTO struct {
	IncomePct  *money.Pct `json:"incomePct" swaggertype:"string"`
	ExpensePct *money.Pct `json:"expensePct" swaggertype:"string"`
	NetPct     *money.Pct `json:"netPct" swaggertype:"string"`
}

// OverviewDTO is the home-screen overview.
type OverviewDTO struct {
	Currency string    `json:"currency" example:"USD"`
	Period   PeriodDTO `json:"period"`
	TotalsDTO
	Previous *TotalsDTO         `json:"previous"` // null for ALL_TIME
	Change   ChangeDTO          `json:"change"`
	Warnings []currency.Warning `json:"warnings"`
}

// overview godoc
//
//	@Summary		Income and expense of a period compared with the previous one
//	@Description	Totals are converted to the space base currency and cover INCOME and EXPENSE transactions only (transfers, trades, settlements, adjustments and opening balances are excluded). `period` is a preset (TODAY, THIS_WEEK, THIS_PERIOD (default), LAST_7_DAYS ... THIS_YEAR, ALL_TIME) or use `from`/`to` (YYYY-MM-DD, inclusive). The previous period is the one right before (yesterday, last week, the previous budget period, the N days before, last year; for a custom range the same length before it). Percentage changes are null when the previous value is 0.
//	@Tags			dashboard
//	@Security		BearerAuth
//	@Produce		json
//	@Param			spaceId	path		string	true	"Space ID"
//	@Param			period	query		string	false	"Period preset"
//	@Param			from	query		string	false	"Start date (YYYY-MM-DD)"
//	@Param			to		query		string	false	"End date, inclusive (YYYY-MM-DD)"
//	@Success		200		{object}	OverviewDTO
//	@Failure		400		{object}	httpx.ErrorEnvelope
//	@Failure		404		{object}	httpx.ErrorEnvelope
//	@Router			/spaces/{spaceId}/dashboard/overview [get]
func (h *Handler) overview(w http.ResponseWriter, r *http.Request) {
	sp, _ := spaces.SpaceFromContext(r.Context())
	sel, err := h.selection(r, period.ThisPeriod)
	var o Overview
	if err == nil {
		o, err = h.svc.Overview(r.Context(), sp, sel)
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := OverviewDTO{
		Currency: o.Currency, Period: periodDTO(o.Range), TotalsDTO: totalsDTO(o.Current, o.Currency), Warnings: o.Warnings,
		Change: ChangeDTO{IncomePct: money.PctPtr(o.IncomePct), ExpensePct: money.PctPtr(o.ExpensePct), NetPct: money.PctPtr(o.NetPct)},
	}
	if o.Previous != nil {
		p := totalsDTO(*o.Previous, o.Currency)
		out.Previous = &p
	}
	writeJSON(w, http.StatusOK, out)
}

// CategoryShareDTO is one slice of the breakdown.
type CategoryShareDTO struct {
	CategoryID *uuid.UUID   `json:"categoryId"`
	Name       string       `json:"name"`
	Icon       string       `json:"icon"`
	Color      string       `json:"color"`
	Amount     money.Amount `json:"amount" swaggertype:"string"`
	Pct        money.Pct    `json:"pct" swaggertype:"string" example:"42.50"` // a percentage of the total
	Count      int          `json:"count"`
}

func shareDTO(c CategoryShare, cur string) CategoryShareDTO {
	return CategoryShareDTO{CategoryID: c.CategoryID, Name: c.Name, Icon: c.Icon, Color: c.Color, Amount: money.NewAmount(c.Amount, cur), Pct: money.NewPct(c.Pct), Count: c.Count}
}

// BreakdownDTO splits a period's income or expense by category.
type BreakdownDTO struct {
	Currency      string             `json:"currency" example:"USD"`
	Type          string             `json:"type" enums:"EXPENSE,INCOME"`
	Period        PeriodDTO          `json:"period"`
	Total         money.Amount       `json:"total" swaggertype:"string"`
	Items         []CategoryShareDTO `json:"items"`
	Uncategorized *CategoryShareDTO  `json:"uncategorized"`
	Warnings      []currency.Warning `json:"warnings"`
}

// breakdown godoc
//
//	@Summary		Income or expense of a period by category
//	@Description	Per category: amount (space base currency), pct of the total and transaction count, largest first; transactions without a category are reported as `uncategorized`. Deleted categories still appear for their historical transactions.
//	@Tags			dashboard
//	@Security		BearerAuth
//	@Produce		json
//	@Param			spaceId	path		string	true	"Space ID"
//	@Param			period	query		string	false	"Period preset (default THIS_PERIOD)"
//	@Param			from	query		string	false	"Start date (YYYY-MM-DD)"
//	@Param			to		query		string	false	"End date, inclusive (YYYY-MM-DD)"
//	@Param			type	query		string	false	"EXPENSE (default) or INCOME"
//	@Success		200		{object}	BreakdownDTO
//	@Failure		400		{object}	httpx.ErrorEnvelope
//	@Failure		404		{object}	httpx.ErrorEnvelope
//	@Router			/spaces/{spaceId}/dashboard/category-breakdown [get]
func (h *Handler) breakdown(w http.ResponseWriter, r *http.Request) {
	sp, _ := spaces.SpaceFromContext(r.Context())
	typ := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("type")))
	if typ == "" {
		typ = typeExpense
	}
	var b Breakdown
	sel, err := h.selection(r, period.ThisPeriod)
	if err == nil && typ != typeExpense && typ != typeIncome {
		err = apperror.Validation("Validation failed", map[string]any{"type": "must be EXPENSE or INCOME"})
	}
	if err == nil {
		b, err = h.svc.CategoryBreakdown(r.Context(), sp, typ, sel.Range)
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := BreakdownDTO{Currency: b.Currency, Type: b.Type, Period: periodDTO(b.Range), Total: money.NewAmount(b.Total, b.Currency), Items: make([]CategoryShareDTO, 0, len(b.Items)), Warnings: b.Warnings}
	for _, it := range b.Items {
		out.Items = append(out.Items, shareDTO(it, b.Currency))
	}
	if b.Uncategorized != nil {
		u := shareDTO(*b.Uncategorized, b.Currency)
		out.Uncategorized = &u
	}
	writeJSON(w, http.StatusOK, out)
}

// BucketDTO is one bar of the analytics chart.
type BucketDTO struct {
	Label    string       `json:"label" example:"29–30 Nov"`
	FromDate string       `json:"fromDate" example:"2026-11-29"`
	ToDate   string       `json:"toDate" example:"2026-11-30"`
	Income   money.Amount `json:"income" swaggertype:"string"`
	Expense  money.Amount `json:"expense" swaggertype:"string"`
}

// AnalyticsDTO holds the bars of a period.
type AnalyticsDTO struct {
	Currency string             `json:"currency" example:"USD"`
	Period   PeriodDTO          `json:"period"`
	Buckets  []BucketDTO        `json:"buckets"`
	Warnings []currency.Warning `json:"warnings"`
}

// analytics godoc
//
//	@Summary		Income and expense bars for a period
//	@Description	For a month-length period the bars are days 1–7, 8–14, 15–21, 22–28 and 29–end of the period (the last one is labelled e.g. "29–30 Nov"); THIS_YEAR and other long ranges give one bar per calendar month; up to 14 days give one bar per day. Days are the user's local days; amounts are in the space base currency.
//	@Tags			dashboard
//	@Security		BearerAuth
//	@Produce		json
//	@Param			spaceId	path		string	true	"Space ID"
//	@Param			period	query		string	false	"Period preset (default THIS_PERIOD)"
//	@Param			from	query		string	false	"Start date (YYYY-MM-DD)"
//	@Param			to		query		string	false	"End date, inclusive (YYYY-MM-DD)"
//	@Success		200		{object}	AnalyticsDTO
//	@Failure		400		{object}	httpx.ErrorEnvelope
//	@Failure		404		{object}	httpx.ErrorEnvelope
//	@Router			/spaces/{spaceId}/dashboard/analytics [get]
func (h *Handler) analytics(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	sp, _ := spaces.SpaceFromContext(r.Context())
	var a Analytics
	sel, err := h.selection(r, period.ThisPeriod)
	if err == nil {
		a, err = h.svc.Analytics(r.Context(), sp, sel.Range, locationOf(user.Timezone))
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := AnalyticsDTO{Currency: a.Currency, Period: periodDTO(a.Range), Buckets: make([]BucketDTO, 0, len(a.Buckets)), Warnings: a.Warnings}
	for _, b := range a.Buckets {
		out.Buckets = append(out.Buckets, BucketDTO{Label: b.Label, FromDate: b.FromDate, ToDate: b.ToDate, Income: money.NewAmount(b.Income, a.Currency), Expense: money.NewAmount(b.Expense, a.Currency)})
	}
	writeJSON(w, http.StatusOK, out)
}

// DayDTO is one local day of the calendar.
type DayDTO struct {
	Date    string       `json:"date" example:"2026-11-18"`
	Income  money.Amount `json:"income" swaggertype:"string"`
	Expense money.Amount `json:"expense" swaggertype:"string"`
	Net     money.Amount `json:"net" swaggertype:"string"`
	Count   int          `json:"count"`
}

// CalendarDTO lists every day of a month.
type CalendarDTO struct {
	Currency string             `json:"currency" example:"USD"`
	Month    string             `json:"month" example:"2026-11"`
	Days     []DayDTO           `json:"days"`
	Warnings []currency.Warning `json:"warnings"`
}

// calendar godoc
//
//	@Summary		Income, expense and transaction count per local day of a month
//	@Description	Every day of the month is listed (days without transactions are zero), grouped by the user's local date and converted to the space base currency.
//	@Tags			dashboard
//	@Security		BearerAuth
//	@Produce		json
//	@Param			spaceId	path		string	true	"Space ID"
//	@Param			month	query		string	true	"Month (YYYY-MM)"
//	@Success		200		{object}	CalendarDTO
//	@Failure		400		{object}	httpx.ErrorEnvelope
//	@Failure		404		{object}	httpx.ErrorEnvelope
//	@Router			/spaces/{spaceId}/dashboard/calendar [get]
func (h *Handler) calendar(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	sp, _ := spaces.SpaceFromContext(r.Context())
	m, err := time.Parse("2006-01", strings.TrimSpace(r.URL.Query().Get("month")))
	if err != nil {
		httpx.WriteError(w, r, apperror.Validation("Validation failed", map[string]any{"month": "is required and must look like 2026-11"}))
		return
	}
	c, err := h.svc.Calendar(r.Context(), sp, m.Year(), m.Month(), locationOf(user.Timezone))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := CalendarDTO{Currency: c.Currency, Month: c.Month, Days: make([]DayDTO, 0, len(c.Days)), Warnings: c.Warnings}
	for _, d := range c.Days {
		out.Days = append(out.Days, DayDTO{Date: d.Date, Income: money.NewAmount(d.Income, c.Currency), Expense: money.NewAmount(d.Expense, c.Currency), Net: money.NewAmount(d.Net(), c.Currency), Count: d.Count})
	}
	writeJSON(w, http.StatusOK, out)
}
