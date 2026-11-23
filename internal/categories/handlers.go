package categories

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/JonathanRainee/hzzt/internal/auth"
	"github.com/JonathanRainee/hzzt/internal/currency"
	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
	"github.com/JonathanRainee/hzzt/internal/platform/apperror"
	"github.com/JonathanRainee/hzzt/internal/platform/httpx"
	"github.com/JonathanRainee/hzzt/internal/platform/period"
	"github.com/JonathanRainee/hzzt/internal/spaces"
)

// Handler exposes the category endpoints.
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
// manage categories. Space routes carry the full path because the spaces
// handler owns the /spaces/{spaceId} mount point.
func (h *Handler) Mount(r chi.Router) {
	r.With(h.auth.Middleware).Get("/categories/suggestions", h.suggestions)

	r.Group(func(r chi.Router) {
		r.Use(h.auth.Middleware, h.spaces.Middleware)
		const p = "/spaces/{spaceId}/categories"
		r.Get(p, h.list)
		r.Post(p, h.create)
		r.Put(p+"/order", h.reorder) // static segment wins over {id}
		r.Patch(p+"/{id}", h.update)
		r.Delete(p+"/{id}", h.delete)
	})
}

// --- DTOs ------------------------------------------------------------------

// CategoryDTO is the public representation of a category.
type CategoryDTO struct {
	ID    uuid.UUID `json:"id"`
	Type  string    `json:"type" enums:"EXPENSE,INCOME"`
	Name  string    `json:"name"`
	Icon  string    `json:"icon"`
	Color string    `json:"color"`
	// SystemKey is the stable key of a default category (SALARY, SUBSCRIPTIONS, ...) so the client can
	// localize its name; null for categories the user created. A rename never changes it.
	SystemKey      *string          `json:"systemKey"`
	BudgetAmount   *decimal.Decimal `json:"budgetAmount" swaggertype:"string" example:"500000"`
	BudgetCurrency *string          `json:"budgetCurrency" example:"IDR"` // the space's base currency when a budget is set
	SortOrder      int              `json:"sortOrder"`
	CreatedAt      time.Time        `json:"createdAt"`
	Stats          *StatsDTO        `json:"stats,omitempty"` // only with includeStats=true
}

// StatsDTO is a category's usage in the requested period, in the space base currency.
// Expense categories fill spent (and budgetRemaining / budgetUsedPct when a budget is set);
// income categories fill received.
type StatsDTO struct {
	Currency        string             `json:"currency" example:"IDR"`
	Spent           *decimal.Decimal   `json:"spent,omitempty" swaggertype:"string"`
	BudgetRemaining *decimal.Decimal   `json:"budgetRemaining,omitempty" swaggertype:"string"`
	BudgetUsedPct   *decimal.Decimal   `json:"budgetUsedPct,omitempty" swaggertype:"string"`
	Received        *decimal.Decimal   `json:"received,omitempty" swaggertype:"string"`
	Warnings        []currency.Warning `json:"warnings,omitempty"`
}

func toDTO(c sqlc.Category, baseCurrency string) CategoryDTO {
	d := CategoryDTO{
		ID: c.ID, Type: c.Type, Name: c.Name, Icon: c.Icon, Color: c.Color, SystemKey: c.SystemKey,
		SortOrder: int(c.SortOrder), CreatedAt: c.CreatedAt.UTC(),
	}
	if c.BudgetAmount.Valid {
		b := c.BudgetAmount.Decimal
		cur := strings.TrimSpace(baseCurrency)
		d.BudgetAmount, d.BudgetCurrency = &b, &cur
	}
	return d
}

func toDTOs(cs []sqlc.Category, baseCurrency string) []CategoryDTO {
	out := make([]CategoryDTO, 0, len(cs))
	for _, c := range cs {
		out = append(out, toDTO(c, baseCurrency))
	}
	return out
}

type createRequest struct {
	Type         string           `json:"type" validate:"required,oneof=EXPENSE INCOME"`
	Name         string           `json:"name" validate:"required,min=1,max=120"`
	Icon         string           `json:"icon" validate:"required,max=64"`
	Color        string           `json:"color" validate:"required,max=16"`
	BudgetAmount *decimal.Decimal `json:"budgetAmount" swaggertype:"string"`
}

// nullableDecimal distinguishes "absent" from an explicit null (clear the budget).
type nullableDecimal struct {
	Set   bool
	Value *decimal.Decimal
}

func (n *nullableDecimal) UnmarshalJSON(b []byte) error {
	n.Set = true
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return nil
	}
	var d decimal.Decimal
	if err := json.Unmarshal(b, &d); err != nil {
		return err
	}
	n.Value = &d
	return nil
}

type updateRequest struct {
	Type         *string         `json:"type" validate:"omitempty,oneof=EXPENSE INCOME"`
	Name         *string         `json:"name" validate:"omitempty,min=1,max=120"`
	Icon         *string         `json:"icon" validate:"omitempty,max=64"`
	Color        *string         `json:"color" validate:"omitempty,max=16"`
	BudgetAmount nullableDecimal `json:"budgetAmount" swaggertype:"string"`
}

type reorderRequest struct {
	Type string      `json:"type" validate:"required,oneof=EXPENSE INCOME"`
	IDs  []uuid.UUID `json:"ids" validate:"required,max=500" swaggertype:"array,string"`
}

var hexColor = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

var maxBudget = decimal.New(1, 20)

func checkName(f map[string]any, name *string) *string {
	if name == nil {
		return nil
	}
	n := strings.TrimSpace(*name)
	if n == "" || utf8.RuneCountInString(n) > 30 {
		f["name"] = "must be 1 to 30 characters"
	}
	return &n
}

func checkIcon(f map[string]any, icon *string) *string {
	if icon == nil {
		return nil
	}
	i := strings.TrimSpace(*icon)
	if i == "" || utf8.RuneCountInString(i) > 16 {
		f["icon"] = "must be an emoji (1 to 16 characters)"
	}
	return &i
}

func checkColor(f map[string]any, color *string) *string {
	if color == nil {
		return nil
	}
	if !hexColor.MatchString(*color) {
		f["color"] = "must be a hex color like #3B82F6"
	}
	up := strings.ToUpper(*color)
	return &up
}

func checkBudget(f map[string]any, b *decimal.Decimal) *decimal.Decimal {
	if b == nil {
		return nil
	}
	r := b.Round(8)
	if !r.IsPositive() || r.GreaterThanOrEqual(maxBudget) {
		f["budgetAmount"] = "must be greater than 0"
	}
	return &r
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

func categoryID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, errCategoryNF
	}
	return id, nil
}

func listType(r *http.Request) (string, error) {
	t := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("type")))
	if t != "" && t != TypeExpense && t != TypeIncome {
		return "", apperror.Validation("Validation failed", map[string]any{"type": "must be one of: EXPENSE INCOME"})
	}
	return t, nil
}

// --- handlers --------------------------------------------------------------

// list godoc
//
//	@Summary		List categories ordered by sortOrder
//	@Description	With includeStats=true each category carries `stats` for the period (default THIS_PERIOD): expense → spent, budgetRemaining, budgetUsedPct; income → received, all in the space base currency. `period` is a preset (TODAY, THIS_WEEK, THIS_PERIOD, LAST_7_DAYS, ..., ALL_TIME) or use `from`/`to` (YYYY-MM-DD, inclusive).
//	@Tags			categories
//	@Security		BearerAuth
//	@Produce		json
//	@Param			spaceId			path		string	true	"Space ID"
//	@Param			type			query		string	false	"EXPENSE or INCOME"
//	@Param			includeStats	query		bool	false	"Include usage stats"
//	@Param			period			query		string	false	"Period preset for stats"
//	@Param			from			query		string	false	"Start date for stats (YYYY-MM-DD)"
//	@Param			to				query		string	false	"End date for stats, inclusive (YYYY-MM-DD)"
//	@Success		200				{array}		CategoryDTO
//	@Failure		400				{object}	httpx.ErrorEnvelope
//	@Failure		404				{object}	httpx.ErrorEnvelope
//	@Router			/spaces/{spaceId}/categories [get]
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	typ, err := listType(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	sp, _ := spaces.SpaceFromContext(r.Context())

	withStats := false
	if v := r.URL.Query().Get("includeStats"); v != "" {
		b, perr := strconv.ParseBool(v)
		if perr != nil {
			httpx.WriteError(w, r, apperror.Validation("Validation failed", map[string]any{"includeStats": "must be true or false"}))
			return
		}
		withStats = b
	}
	if !withStats {
		cs, err := h.svc.List(r.Context(), sp.ID, typ)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, toDTOs(cs, sp.BaseCurrency))
		return
	}

	user, _ := auth.UserFromContext(r.Context())
	rng, err := statsRange(r, user, sp, h.svc.now())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	rows, err := h.svc.ListWithStats(r.Context(), sp.ID, typ, rng)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := make([]CategoryDTO, 0, len(rows))
	for _, row := range rows {
		d := toDTO(row.Category, sp.BaseCurrency)
		d.Stats = &StatsDTO{
			Currency: BaseCurrencyOf(sp), Spent: row.Stats.Spent, BudgetRemaining: row.Stats.BudgetRemaining,
			BudgetUsedPct: row.Stats.BudgetUsedPct, Received: row.Stats.Received, Warnings: row.Stats.Warnings,
		}
		out = append(out, d)
	}
	writeJSON(w, http.StatusOK, out)
}

// statsRange resolves period (default THIS_PERIOD) or from/to in the user's timezone.
func statsRange(r *http.Request, user sqlc.User, sp sqlc.Space, now time.Time) (period.Range, error) {
	loc, err := time.LoadLocation(user.Timezone)
	if err != nil {
		loc = time.UTC
	}
	v := r.URL.Query()
	if from, to := v.Get("from"), v.Get("to"); from != "" || to != "" {
		rng, err := period.Custom(from, to, loc)
		if err != nil {
			return rng, apperror.Validation("Validation failed", map[string]any{"from": err.Error()})
		}
		return rng, nil
	}
	preset := v.Get("period")
	if preset == "" {
		preset = period.ThisPeriod
	}
	rng, err := period.Resolve(preset, period.Opts{Location: loc, BudgetStartDay: int(sp.BudgetPeriodStartDay), Now: now})
	if err != nil {
		return rng, apperror.Validation("Validation failed", map[string]any{"period": "must be one of: " + strings.Join(period.Presets, " ")})
	}
	return rng, nil
}

// forceParam reads ?force=true, which skips the CATEGORY_EXISTS_SIMILAR check.
func forceParam(r *http.Request) bool { return r.URL.Query().Get("force") == "true" }

// create godoc
//
//	@Summary		Create a category
//	@Description	Names are unique per type (case-insensitive). budgetAmount is a monthly budget in the space's base currency, expense categories only.
//	@Tags			categories
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			spaceId	path		string			true	"Space ID"
//	@Param			force	query		bool			false	"true skips the CATEGORY_EXISTS_SIMILAR check"
//	@Param			body	body		createRequest	true	"Category"
//	@Success		201		{object}	CategoryDTO
//	@Failure		400		{object}	httpx.ErrorEnvelope
//	@Failure		404		{object}	httpx.ErrorEnvelope
//	@Failure		409		{object}	httpx.ErrorEnvelope	"CATEGORY_NAME_TAKEN"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"BUDGET_NOT_ALLOWED"
//	@Router			/spaces/{spaceId}/categories [post]
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	sp, _ := spaces.SpaceFromContext(r.Context())
	req, err := httpx.DecodeAndValidate[createRequest](r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	f := map[string]any{}
	in := CreateInput{Type: req.Type, Force: forceParam(r)}
	in.Name = *checkName(f, &req.Name)
	in.Icon = *checkIcon(f, &req.Icon)
	in.Color = *checkColor(f, &req.Color)
	in.Budget = checkBudget(f, req.BudgetAmount)
	if err := fieldsErr(f); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	c, err := h.svc.Create(r.Context(), sp.ID, user.ID, in)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toDTO(c, sp.BaseCurrency))
}

// update godoc
//
//	@Summary		Update a category
//	@Description	The type cannot change once transactions use the category (CATEGORY_TYPE_LOCKED). `budgetAmount: null` clears the budget.
//	@Tags			categories
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			spaceId	path		string			true	"Space ID"
//	@Param			id		path		string			true	"Category ID"
//	@Param			force	query		bool			false	"true skips the CATEGORY_EXISTS_SIMILAR check"
//	@Param			body	body		updateRequest	true	"Fields to change"
//	@Success		200		{object}	CategoryDTO
//	@Failure		400		{object}	httpx.ErrorEnvelope
//	@Failure		404		{object}	httpx.ErrorEnvelope
//	@Failure		409		{object}	httpx.ErrorEnvelope	"CATEGORY_NAME_TAKEN"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"BUDGET_NOT_ALLOWED / CATEGORY_TYPE_LOCKED"
//	@Router			/spaces/{spaceId}/categories/{id} [patch]
func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	sp, _ := spaces.SpaceFromContext(r.Context())
	id, err := categoryID(r)
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
	in := UpdateInput{
		Type: req.Type, Name: checkName(f, req.Name), Icon: checkIcon(f, req.Icon), Color: checkColor(f, req.Color),
		BudgetSet: req.BudgetAmount.Set, Budget: checkBudget(f, req.BudgetAmount.Value), Force: forceParam(r),
	}
	if err := fieldsErr(f); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	c, err := h.svc.Update(r.Context(), sp.ID, id, in)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toDTO(c, sp.BaseCurrency))
}

// delete godoc
//
//	@Summary	Delete a category (soft delete; transactions keep the link)
//	@Tags		categories
//	@Security	BearerAuth
//	@Param		spaceId	path	string	true	"Space ID"
//	@Param		id		path	string	true	"Category ID"
//	@Success	204
//	@Failure	404	{object}	httpx.ErrorEnvelope
//	@Router		/spaces/{spaceId}/categories/{id} [delete]
func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	id, err := categoryID(r)
	if err == nil {
		err = h.svc.Delete(r.Context(), spaces.SpaceIDFromContext(r.Context()), id)
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// reorder godoc
//
//	@Summary	Set the display order of one category type
//	@Tags		categories
//	@Security	BearerAuth
//	@Accept		json
//	@Produce	json
//	@Param		spaceId	path		string			true	"Space ID"
//	@Param		body	body		reorderRequest	true	"Type and every category id of that type in the new order"
//	@Success	200		{array}		CategoryDTO
//	@Failure	400		{object}	httpx.ErrorEnvelope
//	@Failure	404		{object}	httpx.ErrorEnvelope
//	@Router		/spaces/{spaceId}/categories/order [put]
func (h *Handler) reorder(w http.ResponseWriter, r *http.Request) {
	sp, _ := spaces.SpaceFromContext(r.Context())
	req, err := httpx.DecodeAndValidate[reorderRequest](r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	cs, err := h.svc.Reorder(r.Context(), sp.ID, req.Type, req.IDs)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toDTOs(cs, sp.BaseCurrency))
}

// suggestions godoc
//
//	@Summary	Suggested category names
//	@Tags		categories
//	@Security	BearerAuth
//	@Produce	json
//	@Param		type	query		string	true	"EXPENSE or INCOME"
//	@Success	200		{array}		string
//	@Failure	400		{object}	httpx.ErrorEnvelope
//	@Router		/categories/suggestions [get]
func (h *Handler) suggestions(w http.ResponseWriter, r *http.Request) {
	typ, err := listType(r)
	if err == nil && typ == "" {
		err = apperror.Validation("Validation failed", map[string]any{"type": "is required"})
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, Suggestions[typ])
}
