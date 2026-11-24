package transactions

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/JonathanRainee/hzzt/internal/auth"
	"github.com/JonathanRainee/hzzt/internal/currency"
	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
	"github.com/JonathanRainee/hzzt/internal/ledger"
	"github.com/JonathanRainee/hzzt/internal/platform/apperror"
	"github.com/JonathanRainee/hzzt/internal/platform/httpx"
	"github.com/JonathanRainee/hzzt/internal/platform/period"
	"github.com/JonathanRainee/hzzt/internal/spaces"
)

// Handler exposes the transaction endpoints.
type Handler struct {
	svc    *Service
	auth   *auth.Service
	spaces *spaces.Service
}

// NewHandler creates the HTTP layer.
func NewHandler(svc *Service, authSvc *auth.Service, spacesSvc *spaces.Service) *Handler {
	return &Handler{svc: svc, auth: authSvc, spaces: spacesSvc}
}

// Mount registers the routes on r (the /api/v1 router). Any space member may
// record transactions. Space routes carry the full path because the spaces
// handler owns the /spaces/{spaceId} mount point.
func (h *Handler) Mount(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(h.auth.Middleware, h.spaces.Middleware)
		const p = "/spaces/{spaceId}/transactions"
		r.Get(p, h.list)
		r.Post(p, h.create)
		r.Get(p+"/daily-summary", h.dailySummary) // static segment wins over {id}
		r.Get(p+"/{id}", h.get)
		r.Patch(p+"/{id}", h.update)
		r.Delete(p+"/{id}", h.delete)
		r.Post("/spaces/{spaceId}/wallets/{id}/top-up", h.topUp)
	})
}

// --- DTOs ------------------------------------------------------------------

// Ref is a wallet or category reference with display info.
type Ref struct {
	ID    uuid.UUID `json:"id"`
	Name  string    `json:"name"`
	Icon  string    `json:"icon"`
	Color *string   `json:"color,omitempty"`
}

// InvestmentRef is the investment a BUY/SELL belongs to.
type InvestmentRef struct {
	ID     uuid.UUID `json:"id"`
	Name   string    `json:"name"`
	Symbol *string   `json:"symbol"`
	Icon   string    `json:"icon"`
}

// ManualAssetRef is the asset a settlement belongs to.
type ManualAssetRef struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	Icon string    `json:"icon"`
}

// TransactionDTO is the public representation of a transaction.
type TransactionDTO struct {
	ID        uuid.UUID `json:"id"`
	Type      string    `json:"type" enums:"INCOME,EXPENSE,TRANSFER,ADJUSTMENT,OPENING_BALANCE"`
	Direction *string   `json:"direction,omitempty" enums:"IN,OUT"`
	// SystemKey is set on transactions the backend wrote itself, so the client can localize the
	// title: OPENING_BALANCE, BALANCE_TOP_UP, BALANCE_ADJUSTMENT. Null for user transactions.
	SystemKey *string         `json:"systemKey" enums:"OPENING_BALANCE,BALANCE_TOP_UP,BALANCE_ADJUSTMENT"`
	Title     *string         `json:"title"`
	Note      *string         `json:"note"`
	Amount    decimal.Decimal `json:"amount" swaggertype:"string" example:"50000"`
	Currency  string          `json:"currency" example:"IDR"`
	Wallet    *Ref            `json:"wallet"`
	Category  *Ref            `json:"category"`
	ToWallet  *Ref            `json:"toWallet,omitempty"`
	// Investment trades: a BUY (wallet → investment) or SELL (investment → wallet).
	Investment       *InvestmentRef   `json:"investment,omitempty"`
	InvestmentAction *string          `json:"investmentAction,omitempty" enums:"BUY,SELL"`
	Quantity         *decimal.Decimal `json:"quantity,omitempty" swaggertype:"string"`
	UnitPrice        *decimal.Decimal `json:"unitPrice,omitempty" swaggertype:"string"`
	Fee              *decimal.Decimal `json:"fee,omitempty" swaggertype:"string"`
	RealizedPL       *decimal.Decimal `json:"realizedPl,omitempty" swaggertype:"string"`
	// Settlements of money owed / debt: the manual asset they belong to.
	ManualAsset *ManualAssetRef  `json:"manualAsset,omitempty"`
	ToAmount    *decimal.Decimal `json:"toAmount,omitempty" swaggertype:"string"`
	ToCurrency  *string          `json:"toCurrency,omitempty"`
	FxRate      *decimal.Decimal `json:"fxRate,omitempty" swaggertype:"string"`
	OccurredAt  time.Time        `json:"occurredAt"`
	CreatedByID uuid.UUID        `json:"createdById"`
	CreatedAt   time.Time        `json:"createdAt"`
	UpdatedAt   time.Time        `json:"updatedAt"`
}

func ref(id uuid.NullUUID, name, icon, color *string) *Ref {
	if !id.Valid || name == nil {
		return nil
	}
	r := &Ref{ID: id.UUID, Name: *name, Color: color}
	if icon != nil {
		r.Icon = *icon
	}
	return r
}

// ToDTO converts a transaction with its references.
func ToDTO(d Detail) TransactionDTO {
	t := TransactionDTO{
		ID: d.ID, Type: d.Type, Direction: d.Direction, SystemKey: d.SystemKey, Title: d.Title, Note: d.Note,
		Amount: d.Amount, Currency: strings.TrimSpace(d.Currency),
		Wallet:     ref(d.WalletID, d.WalletName, d.WalletIcon, nil),
		Category:   ref(d.CategoryID, d.CategoryName, d.CategoryIcon, d.CategoryColor),
		ToWallet:   ref(d.ToWalletID, d.ToWalletName, d.ToWalletIcon, nil),
		OccurredAt: d.OccurredAt.UTC(), CreatedByID: d.CreatedByID,
		CreatedAt: d.CreatedAt.UTC(), UpdatedAt: d.UpdatedAt.UTC(),
	}
	if d.ToAmount.Valid {
		v := d.ToAmount.Decimal
		t.ToAmount = &v
	}
	if d.ToCurrency != nil {
		c := strings.TrimSpace(*d.ToCurrency)
		t.ToCurrency = &c
	}
	if d.FxRate.Valid {
		v := d.FxRate.Decimal
		t.FxRate = &v
	}
	if d.ManualAssetID.Valid && d.ManualAssetName != nil {
		ic := ""
		if d.ManualAssetIcon != nil {
			ic = *d.ManualAssetIcon
		}
		t.ManualAsset = &ManualAssetRef{ID: d.ManualAssetID.UUID, Name: *d.ManualAssetName, Icon: ic}
	}
	if d.InvestmentID.Valid && d.InvestmentName != nil {
		ic := ""
		if d.InvestmentIcon != nil {
			ic = *d.InvestmentIcon
		}
		t.Investment = &InvestmentRef{ID: d.InvestmentID.UUID, Name: *d.InvestmentName, Symbol: d.InvestmentSymbol, Icon: ic}
		t.InvestmentAction = d.InvestmentAction
		opt := func(n decimal.NullDecimal) *decimal.Decimal {
			if !n.Valid {
				return nil
			}
			v := n.Decimal
			return &v
		}
		t.Quantity, t.UnitPrice, t.Fee, t.RealizedPL = opt(d.Quantity), opt(d.UnitPrice), opt(d.Fee), opt(d.RealizedPl)
	}
	return t
}

// ListResponse is one page of transactions.
type ListResponse struct {
	Items      []TransactionDTO `json:"items"`
	NextCursor *string          `json:"nextCursor"`
	// Total is how many transactions match the filters in all pages (not just this one).
	Total int `json:"total" example:"42"`
}

// DaySummaryDTO is one local date's totals in the space base currency.
type DaySummaryDTO struct {
	Date     string          `json:"date" example:"2026-11-18"`
	Income   decimal.Decimal `json:"income" swaggertype:"string"`
	Expense  decimal.Decimal `json:"expense" swaggertype:"string"`
	Transfer decimal.Decimal `json:"transfer" swaggertype:"string"`
	Count    int             `json:"count"`
}

// DailySummaryResponse lists per-day totals converted to the base currency.
type DailySummaryResponse struct {
	Currency string             `json:"currency" example:"IDR"`
	Items    []DaySummaryDTO    `json:"items"`
	Warnings []currency.Warning `json:"warnings"`
}

type createRequest struct {
	Type       string           `json:"type" validate:"required,oneof=INCOME EXPENSE"`
	WalletID   uuid.UUID        `json:"walletId" validate:"required"`
	Amount     *decimal.Decimal `json:"amount" validate:"required" swaggertype:"string" example:"50000"`
	CategoryID *uuid.UUID       `json:"categoryId"`
	Title      *string          `json:"title" validate:"omitempty,max=200"`
	Note       *string          `json:"note" validate:"omitempty,max=1000"`
	OccurredAt *time.Time       `json:"occurredAt"`
}

type topUpRequest struct {
	Amount *decimal.Decimal `json:"amount" validate:"required" swaggertype:"string" example:"100000"`
	Note   *string          `json:"note" validate:"omitempty,max=1000"`
}

// optional decodes "absent" vs "null" vs a value.
type optional[T any] struct {
	Set   bool
	Value *T
}

func (o *optional[T]) UnmarshalJSON(b []byte) error {
	o.Set = true
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return nil
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	o.Value = &v
	return nil
}

type updateRequest struct {
	Type       *string             `json:"type" validate:"omitempty,oneof=INCOME EXPENSE ADJUSTMENT TRANSFER OPENING_BALANCE"`
	WalletID   *uuid.UUID          `json:"walletId"`
	Amount     *decimal.Decimal    `json:"amount" swaggertype:"string"`
	CategoryID optional[uuid.UUID] `json:"categoryId" swaggertype:"string"`
	Title      optional[string]    `json:"title" swaggertype:"string"`
	Note       optional[string]    `json:"note" swaggertype:"string"`
	OccurredAt *time.Time          `json:"occurredAt"`
}

var maxMoney = decimal.New(1, 20)

func checkAmount(f map[string]any, field string, d *decimal.Decimal) {
	switch {
	case d == nil:
	case !d.IsPositive():
		f[field] = "must be greater than 0"
	case !d.Round(8).Equal(*d):
		f[field] = "must have at most 8 decimal places"
	case d.GreaterThanOrEqual(maxMoney):
		f[field] = "is too large"
	}
}

func fieldsErr(f map[string]any) error {
	if len(f) == 0 {
		return nil
	}
	return apperror.Validation("Validation failed", f)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func txnID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, errTxnNF
	}
	return id, nil
}

func locationOf(tz string) *time.Location {
	if l, err := time.LoadLocation(tz); err == nil {
		return l
	}
	return time.UTC
}

// multi returns every value of a repeated, "[]"-suffixed or comma-separated query parameter.
func multi(v url.Values, name string) []string {
	var out []string
	for _, key := range []string{name, name + "[]"} {
		for _, raw := range v[key] {
			for _, part := range strings.Split(raw, ",") {
				if p := strings.TrimSpace(part); p != "" {
					out = append(out, p)
				}
			}
		}
	}
	return out
}

var validTypes = map[string]bool{
	ledger.TypeIncome: true, ledger.TypeExpense: true, ledger.TypeTransfer: true,
	ledger.TypeAdjustment: true, ledger.TypeOpeningBalance: true,
}

// ParseFilter builds a Filter from the query string: `period` (a preset) or
// `from`/`to` (YYYY-MM-DD, inclusive), `types`, `categoryIds`, `walletIds` and
// `q`. Periods resolve in the user's timezone and the space's budget start day.
// With no period or dates the range is unbounded (ALL_TIME).
func ParseFilter(r *http.Request, user sqlc.User, space sqlc.Space, now time.Time) (Filter, error) {
	v := r.URL.Query()
	f := Filter{}
	bad := map[string]any{}
	loc := locationOf(user.Timezone)

	from, to := strings.TrimSpace(v.Get("from")), strings.TrimSpace(v.Get("to"))
	switch p := strings.TrimSpace(v.Get("period")); {
	case from != "" || to != "":
		rng, err := period.Custom(from, to, loc)
		if err != nil {
			bad["from"] = err.Error()
		}
		f.Range = rng
	case p != "":
		rng, err := period.Resolve(p, period.Opts{Location: loc, BudgetStartDay: int(space.BudgetPeriodStartDay), Now: now})
		if err != nil {
			bad["period"] = "must be one of: " + strings.Join(period.Presets, " ")
		}
		f.Range = rng
	}

	for _, t := range multi(v, "types") {
		t = strings.ToUpper(t)
		if !validTypes[t] {
			bad["types"] = "must be any of: INCOME EXPENSE TRANSFER ADJUSTMENT OPENING_BALANCE"
			break
		}
		f.Types = append(f.Types, t)
	}
	parseIDs := func(name string) []uuid.UUID {
		var out []uuid.UUID
		for _, raw := range multi(v, name) {
			id, err := uuid.Parse(raw)
			if err != nil {
				bad[name] = "must be a list of UUIDs"
				return nil
			}
			out = append(out, id)
		}
		return out
	}
	f.CategoryIDs = parseIDs("categoryIds")
	f.WalletIDs = parseIDs("walletIds")
	f.InvestmentIDs = parseIDs("investmentIds")

	f.Q = strings.TrimSpace(v.Get("q"))
	if len([]rune(f.Q)) > 100 {
		bad["q"] = "must be at most 100 characters"
	}
	return f, fieldsErr(bad)
}

// --- handlers --------------------------------------------------------------

// create godoc
//
//	@Summary		Record an income or expense
//	@Description	The currency is the wallet's currency; the category type must match the transaction type.
//	@Tags			transactions
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			spaceId	path		string			true	"Space ID"
//	@Param			body	body		createRequest	true	"Transaction"
//	@Success		201		{object}	TransactionDTO
//	@Failure		400		{object}	httpx.ErrorEnvelope
//	@Failure		404		{object}	httpx.ErrorEnvelope	"space, wallet or category not found"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"CATEGORY_TYPE_MISMATCH"
//	@Router			/spaces/{spaceId}/transactions [post]
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	req, err := httpx.DecodeAndValidate[createRequest](r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	f := map[string]any{}
	checkAmount(f, "amount", req.Amount)
	if err := fieldsErr(f); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	d, err := h.svc.Create(r.Context(), spaces.SpaceIDFromContext(r.Context()), user.ID, CreateInput{
		Type: req.Type, WalletID: req.WalletID, Amount: *req.Amount, CategoryID: req.CategoryID,
		Title: req.Title, Note: req.Note, OccurredAt: req.OccurredAt,
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, ToDTO(d))
}

// topUp godoc
//
//	@Summary	Top up a wallet (an ADJUSTMENT IN titled "Balance top up", no category, never income)
//	@Tags		transactions
//	@Security	BearerAuth
//	@Accept		json
//	@Produce	json
//	@Param		spaceId	path		string			true	"Space ID"
//	@Param		id		path		string			true	"Wallet ID"
//	@Param		body	body		topUpRequest	true	"Amount"
//	@Success	201		{object}	TransactionDTO
//	@Failure	400		{object}	httpx.ErrorEnvelope
//	@Failure	404		{object}	httpx.ErrorEnvelope
//	@Router		/spaces/{spaceId}/wallets/{id}/top-up [post]
func (h *Handler) topUp(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	walletID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, errWalletNF)
		return
	}
	req, err := httpx.DecodeAndValidate[topUpRequest](r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	f := map[string]any{}
	checkAmount(f, "amount", req.Amount)
	if err := fieldsErr(f); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	d, err := h.svc.TopUp(r.Context(), spaces.SpaceIDFromContext(r.Context()), user.ID, walletID, *req.Amount, req.Note)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, ToDTO(d))
}

// get godoc
//
//	@Summary	Get a transaction
//	@Tags		transactions
//	@Security	BearerAuth
//	@Produce	json
//	@Param		spaceId	path		string	true	"Space ID"
//	@Param		id		path		string	true	"Transaction ID"
//	@Success	200		{object}	TransactionDTO
//	@Failure	404		{object}	httpx.ErrorEnvelope
//	@Router		/spaces/{spaceId}/transactions/{id} [get]
func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := txnID(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	d, err := h.svc.Get(r.Context(), spaces.SpaceIDFromContext(r.Context()), id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ToDTO(d))
}

// update godoc
//
//	@Summary		Edit a transaction
//	@Description	INCOME/EXPENSE: any field (the old balance effect is reversed and the new one applied atomically). ADJUSTMENT/OPENING_BALANCE: only `note`. `type: ADJUSTMENT` on an uncategorised INCOME/EXPENSE reclassifies it explicitly as a balance adjustment (IN for income, OUT for expense; the wallet balance does not change); a categorised one, or any other type change, returns TYPE_CHANGE_NOT_ALLOWED. Transfers and investment/asset transactions use their own endpoints (USE_SPECIFIC_ENDPOINT). `categoryId`, `title` and `note` accept null to clear.
//	@Tags			transactions
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			spaceId	path		string			true	"Space ID"
//	@Param			id		path		string			true	"Transaction ID"
//	@Param			body	body		updateRequest	true	"Fields to change"
//	@Success		200		{object}	TransactionDTO
//	@Failure		400		{object}	httpx.ErrorEnvelope
//	@Failure		404		{object}	httpx.ErrorEnvelope
//	@Failure		422		{object}	httpx.ErrorEnvelope	"CATEGORY_TYPE_MISMATCH / USE_SPECIFIC_ENDPOINT / TYPE_CHANGE_NOT_ALLOWED"
//	@Router			/spaces/{spaceId}/transactions/{id} [patch]
func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	id, err := txnID(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	req, err := httpx.DecodeAndValidate[updateRequest](r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	f := map[string]any{}
	checkAmount(f, "amount", req.Amount)
	if req.Title.Value != nil && len(*req.Title.Value) > 200 {
		f["title"] = "must be at most 200 characters"
	}
	if req.Note.Value != nil && len(*req.Note.Value) > 1000 {
		f["note"] = "must be at most 1000 characters"
	}
	if err := fieldsErr(f); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	d, err := h.svc.Update(r.Context(), spaces.SpaceIDFromContext(r.Context()), id, UpdateInput{
		Type: req.Type, WalletID: req.WalletID, Amount: req.Amount,
		CategoryID: Optional[uuid.UUID](req.CategoryID), Title: Optional[string](req.Title), Note: Optional[string](req.Note),
		OccurredAt: req.OccurredAt,
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ToDTO(d))
}

// delete godoc
//
//	@Summary	Delete a transaction (its balance effect is reversed)
//	@Tags		transactions
//	@Security	BearerAuth
//	@Param		spaceId	path	string	true	"Space ID"
//	@Param		id		path	string	true	"Transaction ID"
//	@Success	204
//	@Failure	404	{object}	httpx.ErrorEnvelope
//	@Failure	422	{object}	httpx.ErrorEnvelope	"USE_SPECIFIC_ENDPOINT"
//	@Router		/spaces/{spaceId}/transactions/{id} [delete]
func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	id, err := txnID(r)
	if err == nil {
		err = h.svc.Delete(r.Context(), spaces.SpaceIDFromContext(r.Context()), id)
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// list godoc
//
//	@Summary		Search and filter the transaction history
//	@Description	Newest first (occurredAt desc, id desc) with cursor pagination. `period` is TODAY, THIS_WEEK, THIS_PERIOD, LAST_7_DAYS, LAST_30_DAYS, LAST_60_DAYS, LAST_90_DAYS, THIS_YEAR or ALL_TIME; or use `from`/`to` (YYYY-MM-DD, inclusive). `types`, `categoryIds`, `walletIds` and `investmentIds` accept repeated, `[]`-suffixed or comma-separated values; walletIds matches the source or destination wallet. `q` searches title, note, category and wallet names. Filters combine with AND across fields and OR within a list.
//	@Tags			transactions
//	@Security		BearerAuth
//	@Produce		json
//	@Param			spaceId		path		string	true	"Space ID"
//	@Param			period		query		string	false	"Period preset"
//	@Param			from		query		string	false	"Start date (YYYY-MM-DD)"
//	@Param			to			query		string	false	"End date, inclusive (YYYY-MM-DD)"
//	@Param			types		query		[]string	false	"INCOME, EXPENSE, TRANSFER, ADJUSTMENT, OPENING_BALANCE"
//	@Param			categoryIds	query		[]string	false	"Category IDs"
//	@Param			walletIds	query		[]string	false	"Wallet IDs"
//	@Param			investmentIds	query		[]string	false	"Investment IDs (the trades of those investments)"
//	@Param			q			query		string	false	"Search text"
//	@Param			limit		query		int		false	"Page size (default 30, max 100)"
//	@Param			cursor		query		string	false	"nextCursor of the previous page"
//	@Success		200			{object}	ListResponse
//	@Failure		400			{object}	httpx.ErrorEnvelope
//	@Router			/spaces/{spaceId}/transactions [get]
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	sp, _ := spaces.SpaceFromContext(r.Context())
	f, err := ParseFilter(r, user, sp, h.svc.now())
	limit := 30
	if err == nil {
		if v := r.URL.Query().Get("limit"); v != "" {
			n, perr := strconv.Atoi(v)
			if perr != nil || n < 1 || n > 100 {
				err = apperror.Validation("Validation failed", map[string]any{"limit": "must be between 1 and 100"})
			}
			limit = n
		}
	}
	var res ListResult
	if err == nil {
		res, err = h.svc.List(r.Context(), sp.ID, f, r.URL.Query().Get("cursor"), limit)
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := ListResponse{Items: make([]TransactionDTO, 0, len(res.Items)), Total: res.Total}
	for _, d := range res.Items {
		out.Items = append(out.Items, ToDTO(d))
	}
	if res.NextCursor != "" {
		out.NextCursor = &res.NextCursor
	}
	writeJSON(w, http.StatusOK, out)
}

// dailySummary godoc
//
//	@Summary		Daily totals for the filtered transactions
//	@Description	Same filters as the list. Dates are the user's local dates; income, expense and transfer are converted to the space base currency. Amounts without a usable FX rate are left out and reported in `warnings`. Transfers appear only in `transfer`, never in income or expense.
//	@Tags			transactions
//	@Security		BearerAuth
//	@Produce		json
//	@Param			spaceId		path		string		true	"Space ID"
//	@Param			period		query		string		false	"Period preset"
//	@Param			from		query		string		false	"Start date (YYYY-MM-DD)"
//	@Param			to			query		string		false	"End date, inclusive (YYYY-MM-DD)"
//	@Param			types		query		[]string	false	"Transaction types"
//	@Param			categoryIds	query		[]string	false	"Category IDs"
//	@Param			walletIds	query		[]string	false	"Wallet IDs"
//	@Param			investmentIds	query		[]string	false	"Investment IDs (the trades of those investments)"
//	@Param			q			query		string		false	"Search text"
//	@Success		200			{object}	DailySummaryResponse
//	@Failure		400			{object}	httpx.ErrorEnvelope
//	@Router			/spaces/{spaceId}/transactions/daily-summary [get]
func (h *Handler) dailySummary(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	sp, _ := spaces.SpaceFromContext(r.Context())
	f, err := ParseFilter(r, user, sp, h.svc.now())
	var res SummaryResult
	if err == nil {
		res, err = h.svc.DailySummary(r.Context(), sp.ID, f, locationOf(user.Timezone))
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := DailySummaryResponse{Currency: res.Currency, Items: make([]DaySummaryDTO, 0, len(res.Days)), Warnings: res.Warnings}
	for _, d := range res.Days {
		out.Items = append(out.Items, DaySummaryDTO(d))
	}
	writeJSON(w, http.StatusOK, out)
}
