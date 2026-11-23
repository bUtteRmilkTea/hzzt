package wallets

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/JonathanRainee/hzzt/internal/auth"
	"github.com/JonathanRainee/hzzt/internal/currency"
	"github.com/JonathanRainee/hzzt/internal/platform/allocation"
	"github.com/JonathanRainee/hzzt/internal/platform/apperror"
	"github.com/JonathanRainee/hzzt/internal/platform/httpx"
	"github.com/JonathanRainee/hzzt/internal/platform/money"
	"github.com/JonathanRainee/hzzt/internal/spaces"
)

// Handler exposes the wallet endpoints.
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
// manage wallets. Space routes carry the full path because the spaces handler
// owns the /spaces/{spaceId} mount point.
func (h *Handler) Mount(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(h.auth.Middleware)
		r.Get("/wallet-card-presets", h.presets)
		r.Get("/wallet-card-templates", h.templates)
	})

	r.Group(func(r chi.Router) {
		r.Use(h.auth.Middleware, h.spaces.Middleware)
		const p = "/spaces/{spaceId}/wallets"
		r.Get(p, h.list)
		r.Post(p, h.create)
		r.Put(p+"/order", h.reorder) // static segment wins over {id}
		r.Get(p+"/{id}", h.get)
		r.Patch(p+"/{id}", h.update)
		r.Put(p+"/{id}/balance", h.setBalance)
		r.Patch(p+"/{id}/display-prefs", h.updateDisplayPrefs)
		r.Delete(p+"/{id}", h.delete)
	})
}

// --- DTOs ------------------------------------------------------------------

// WalletDTO is the public representation of a wallet.
type WalletDTO struct {
	ID       uuid.UUID `json:"id"`
	Type     string    `json:"type" enums:"BANK,E_WALLET,CASH,OTHER"`
	Name     string    `json:"name"`
	Icon     string    `json:"icon"`
	Currency string    `json:"currency" example:"IDR"`
	// Balance, Reserved and Available are in the wallet's currency: IDR and JPY show no decimals, others 2.
	Balance money.Amount `json:"balance" swaggertype:"string" example:"134.26"`
	// Reserved is the part of the balance earmarked for goals; Available = balance − reserved
	// and may be negative when expenses pushed the wallet below its reserves (OverReserved).
	Reserved     money.Amount `json:"reserved" swaggertype:"string" example:"100.00"`
	Available    money.Amount `json:"available" swaggertype:"string" example:"34.26"`
	OverReserved bool         `json:"overReserved"`
	IsFavorite   bool         `json:"isFavorite"`
	SortOrder    int          `json:"sortOrder"`
	CardStyle    CardStyle    `json:"cardStyle"`
	// DisplayPrefs are the calling user's own display choices for this wallet (all false by default);
	// other members never see them.
	DisplayPrefs DisplayPrefs `json:"displayPrefs"`
	// BaseBalance and PctOfTotal are set in the list response only: the balance in the space's
	// base currency, and its share of the total (percentages add up to exactly 100.00). Null
	// without an FX rate, or when the total is 0.
	BaseBalance *money.Amount `json:"baseBalance" swaggertype:"string" example:"2150000"`
	PctOfTotal  *money.Pct    `json:"pctOfTotal" swaggertype:"string" example:"33.33"`
	CreatedAt   time.Time     `json:"createdAt"`
}

// ToDTO converts a wallet.
func ToDTO(w Wallet) WalletDTO {
	return WalletDTO{
		ID: w.ID, Type: w.Type, Name: w.Name, Icon: w.Icon, Currency: strings.TrimSpace(w.Currency),
		Balance: money.NewAmount(w.Balance, w.Currency), Reserved: money.NewAmount(w.Reserved, w.Currency),
		Available: money.NewAmount(w.Available(), w.Currency), OverReserved: w.OverReserved(),
		IsFavorite: w.IsFavorite, SortOrder: int(w.SortOrder),
		CardStyle: w.Style, CreatedAt: w.CreatedAt.UTC(),
	}
}

// ListResponse is the wallet list plus the converted total.
type ListResponse struct {
	Items []WalletDTO              `json:"items"`
	Total currency.AggregateResult `json:"total"`
}

type createRequest struct {
	Type           string           `json:"type" validate:"required,oneof=BANK E_WALLET CASH OTHER"`
	Name           string           `json:"name" validate:"required,min=1,max=120"`
	Icon           string           `json:"icon" validate:"required,max=64"`
	Currency       *string          `json:"currency" validate:"omitempty,max=8"`
	InitialBalance *decimal.Decimal `json:"initialBalance" swaggertype:"string" example:"134.26"`
	CardStyle      json.RawMessage  `json:"cardStyle" swaggertype:"object"`
	IsFavorite     *bool            `json:"isFavorite"`
}

type updateRequest struct {
	Type       *string         `json:"type" validate:"omitempty,oneof=BANK E_WALLET CASH OTHER"`
	Name       *string         `json:"name" validate:"omitempty,min=1,max=120"`
	Icon       *string         `json:"icon" validate:"omitempty,max=64"`
	Currency   *string         `json:"currency" validate:"omitempty,max=8"`
	CardStyle  json.RawMessage `json:"cardStyle" swaggertype:"object"`
	IsFavorite *bool           `json:"isFavorite"`
}

type setBalanceRequest struct {
	NewBalance *decimal.Decimal `json:"newBalance" validate:"required" swaggertype:"string" example:"100"`
	Note       *string          `json:"note" validate:"omitempty,max=500"`
}

type reorderRequest struct {
	IDs []uuid.UUID `json:"ids" validate:"required,max=500" swaggertype:"array,string"`
}

var maxMoney = decimal.New(1, 20)

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

func checkCurrency(f map[string]any, c *string) *string {
	if c == nil {
		return nil
	}
	up := strings.ToUpper(strings.TrimSpace(*c))
	if !money.IsSupportedCurrency(up) {
		f["currency"] = "must be a supported ISO 4217 currency code"
	}
	return &up
}

// checkMoney accepts at most 8 decimal places and a magnitude below 10^20.
func checkMoney(f map[string]any, field string, d decimal.Decimal) {
	if !d.Round(8).Equal(d) {
		f[field] = "must have at most 8 decimal places"
	} else if d.Abs().GreaterThanOrEqual(maxMoney) {
		f[field] = "is too large"
	}
}

func fieldsErr(f map[string]any) error {
	if len(f) == 0 {
		return nil
	}
	return apperror.Validation("Validation failed", f)
}

func isAbsent(raw json.RawMessage) bool {
	t := strings.TrimSpace(string(raw))
	return t == "" || t == "null"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func walletID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, errWalletNF
	}
	return id, nil
}

// --- handlers --------------------------------------------------------------

// list godoc
//
//	@Summary		List wallets with the converted total
//	@Description	Favorites first, then sortOrder. `total` is converted to the space base currency; wallets without a usable FX rate are left out and reported in `total.warnings`. Each item also carries `baseBalance` and `pctOfTotal` (percent of the total, 2 decimals, adding up to exactly 100.00; null without an FX rate) for pie charts.
//	@Tags			wallets
//	@Security		BearerAuth
//	@Produce		json
//	@Param			spaceId	path		string	true	"Space ID"
//	@Success		200		{object}	ListResponse
//	@Failure		404		{object}	httpx.ErrorEnvelope
//	@Router			/spaces/{spaceId}/wallets [get]
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	res, err := h.svc.List(r.Context(), spaces.SpaceIDFromContext(r.Context()))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	user, _ := auth.UserFromContext(r.Context())
	prefs, err := h.svc.Prefs(r.Context(), user.ID, spaces.SpaceIDFromContext(r.Context()))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	items := make([]WalletDTO, 0, len(res.Wallets))
	var idx []int
	var vals []decimal.Decimal
	for i, x := range res.Wallets {
		d := ToDTO(x)
		d.DisplayPrefs = prefs[x.ID]
		if v, ok := res.BaseBalance[x.ID]; ok {
			ba := money.NewAmount(v, res.Total.Currency)
			d.BaseBalance = &ba
			idx, vals = append(idx, i), append(vals, v)
		}
		items = append(items, d)
	}
	for k, p := range allocation.SplitPct(vals) {
		items[idx[k]].PctOfTotal = money.PctPtr(p)
	}
	writeJSON(w, http.StatusOK, ListResponse{Items: items, Total: res.Total})
}

// create godoc
//
//	@Summary		Create a wallet
//	@Description	A non-zero initialBalance creates an OPENING_BALANCE transaction. cardStyle is `{mode:"SOLID",color}`, `{mode:"GRADIENT",colors:[a,b],direction,presetId?}` or `{mode:"TEMPLATE",templateId}`.
//	@Tags			wallets
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			spaceId	path		string			true	"Space ID"
//	@Param			body	body		createRequest	true	"Wallet"
//	@Success		201		{object}	WalletDTO
//	@Failure		400		{object}	httpx.ErrorEnvelope
//	@Failure		404		{object}	httpx.ErrorEnvelope
//	@Router			/spaces/{spaceId}/wallets [post]
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	sp, _ := spaces.SpaceFromContext(r.Context())
	req, err := httpx.DecodeAndValidate[createRequest](r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	f := map[string]any{}
	in := CreateInput{Type: req.Type, Name: *checkName(f, &req.Name), Icon: *checkIcon(f, &req.Icon)}
	if c := checkCurrency(f, req.Currency); c != nil {
		in.Currency = *c
	}
	if req.InitialBalance != nil {
		checkMoney(f, "initialBalance", *req.InitialBalance)
		in.InitialBalance = *req.InitialBalance
	}
	if req.IsFavorite != nil {
		in.Favorite = *req.IsFavorite
	}
	if err := fieldsErr(f); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if !isAbsent(req.CardStyle) {
		cs, err := ParseCardStyle(req.CardStyle)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		in.Style = &cs
	}
	wl, err := h.svc.Create(r.Context(), sp, user.ID, in)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, ToDTO(wl))
}

// writeWallet answers with one wallet including the calling user's display preferences.
func (h *Handler) writeWallet(w http.ResponseWriter, r *http.Request, status int, wl Wallet) {
	user, _ := auth.UserFromContext(r.Context())
	prefs, err := h.svc.PrefsOf(r.Context(), user.ID, wl.ID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	d := ToDTO(wl)
	d.DisplayPrefs = prefs
	writeJSON(w, status, d)
}

// PrefsRequest changes some of the calling user's display preferences of a wallet.
type PrefsRequest struct {
	HideLogo *bool `json:"hideLogo"`
	HideName *bool `json:"hideName"`
	HideType *bool `json:"hideType"`
}

// updateDisplayPrefs godoc
//
//	@Summary		Set my display preferences of a wallet (hide logo, name, type)
//	@Description	Per user and synced across devices; other members of the space never see them. Fields that are not sent keep their value; a result with nothing hidden is the default and is not stored. Also returned as `displayPrefs` by GET /wallets and GET /wallets/{id}.
//	@Tags			wallets
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			spaceId	path		string			true	"Space ID"
//	@Param			id		path		string			true	"Wallet ID"
//	@Param			body	body		PrefsRequest	true	"Preferences to change"
//	@Success		200		{object}	DisplayPrefs
//	@Failure		400		{object}	httpx.ErrorEnvelope
//	@Failure		404		{object}	httpx.ErrorEnvelope	"space or WALLET_NOT_FOUND"
//	@Router			/spaces/{spaceId}/wallets/{id}/display-prefs [patch]
func (h *Handler) updateDisplayPrefs(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	id, err := walletID(r)
	var req PrefsRequest
	if err == nil {
		req, err = httpx.DecodeAndValidate[PrefsRequest](r)
	}
	var out DisplayPrefs
	if err == nil {
		out, err = h.svc.UpdatePrefs(r.Context(), spaces.SpaceIDFromContext(r.Context()), user.ID, id, PrefsPatch(req))
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// get godoc
//
//	@Summary	Get a wallet
//	@Tags		wallets
//	@Security	BearerAuth
//	@Produce	json
//	@Param		spaceId	path		string	true	"Space ID"
//	@Param		id		path		string	true	"Wallet ID"
//	@Success	200		{object}	WalletDTO
//	@Failure	404		{object}	httpx.ErrorEnvelope
//	@Router		/spaces/{spaceId}/wallets/{id} [get]
func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := walletID(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	wl, err := h.svc.Get(r.Context(), spaces.SpaceIDFromContext(r.Context()), id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	h.writeWallet(w, r, http.StatusOK, wl)
}

// update godoc
//
//	@Summary		Update a wallet
//	@Description	The currency can only change while the wallet has no transactions (WALLET_CURRENCY_LOCKED). Use PUT .../balance to change the balance.
//	@Tags			wallets
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			spaceId	path		string			true	"Space ID"
//	@Param			id		path		string			true	"Wallet ID"
//	@Param			body	body		updateRequest	true	"Fields to change"
//	@Success		200		{object}	WalletDTO
//	@Failure		400		{object}	httpx.ErrorEnvelope
//	@Failure		404		{object}	httpx.ErrorEnvelope
//	@Failure		422		{object}	httpx.ErrorEnvelope	"WALLET_CURRENCY_LOCKED"
//	@Router			/spaces/{spaceId}/wallets/{id} [patch]
func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	id, err := walletID(r)
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
		Type: req.Type, Name: checkName(f, req.Name), Icon: checkIcon(f, req.Icon),
		Currency: checkCurrency(f, req.Currency), Favorite: req.IsFavorite,
	}
	if err := fieldsErr(f); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if !isAbsent(req.CardStyle) {
		cs, err := ParseCardStyle(req.CardStyle)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		in.Style = &cs
	}
	wl, err := h.svc.Update(r.Context(), spaces.SpaceIDFromContext(r.Context()), id, in)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	h.writeWallet(w, r, http.StatusOK, wl)
}

// setBalance godoc
//
//	@Summary		Set the wallet balance
//	@Description	Records an ADJUSTMENT (direction IN/OUT) for the difference; an unchanged balance records nothing.
//	@Tags			wallets
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			spaceId	path		string				true	"Space ID"
//	@Param			id		path		string				true	"Wallet ID"
//	@Param			body	body		setBalanceRequest	true	"New balance"
//	@Success		200		{object}	WalletDTO
//	@Failure		400		{object}	httpx.ErrorEnvelope
//	@Failure		404		{object}	httpx.ErrorEnvelope
//	@Router			/spaces/{spaceId}/wallets/{id}/balance [put]
func (h *Handler) setBalance(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	id, err := walletID(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	req, err := httpx.DecodeAndValidate[setBalanceRequest](r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	f := map[string]any{}
	checkMoney(f, "newBalance", *req.NewBalance)
	if err := fieldsErr(f); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	wl, err := h.svc.SetBalance(r.Context(), spaces.SpaceIDFromContext(r.Context()), id, user.ID, *req.NewBalance, req.Note)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	h.writeWallet(w, r, http.StatusOK, wl)
}

// delete godoc
//
//	@Summary	Delete a wallet (soft delete)
//	@Tags		wallets
//	@Security	BearerAuth
//	@Param		spaceId	path	string	true	"Space ID"
//	@Param		id		path	string	true	"Wallet ID"
//	@Param		force	query	bool	false	"Required when the balance is not zero"
//	@Success	204
//	@Failure	404	{object}	httpx.ErrorEnvelope
//	@Failure	422	{object}	httpx.ErrorEnvelope	"WALLET_HAS_BALANCE"
//	@Router		/spaces/{spaceId}/wallets/{id} [delete]
func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	id, err := walletID(r)
	force := false
	if err == nil {
		if v := r.URL.Query().Get("force"); v != "" {
			b, perr := strconv.ParseBool(v)
			if perr != nil {
				err = apperror.Validation("Validation failed", map[string]any{"force": "must be true or false"})
			}
			force = b
		}
	}
	if err == nil {
		err = h.svc.Delete(r.Context(), spaces.SpaceIDFromContext(r.Context()), id, force)
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// reorder godoc
//
//	@Summary	Set the wallet display order
//	@Tags		wallets
//	@Security	BearerAuth
//	@Accept		json
//	@Produce	json
//	@Param		spaceId	path		string			true	"Space ID"
//	@Param		body	body		reorderRequest	true	"Every wallet id in the new order"
//	@Success	200		{object}	ListResponse
//	@Failure	400		{object}	httpx.ErrorEnvelope
//	@Failure	404		{object}	httpx.ErrorEnvelope
//	@Router		/spaces/{spaceId}/wallets/order [put]
func (h *Handler) reorder(w http.ResponseWriter, r *http.Request) {
	req, err := httpx.DecodeAndValidate[reorderRequest](r)
	if err == nil {
		err = h.svc.Reorder(r.Context(), spaces.SpaceIDFromContext(r.Context()), req.IDs)
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	h.list(w, r)
}

// presets godoc
//
//	@Summary	Gradient presets for wallet cards
//	@Tags		wallets
//	@Security	BearerAuth
//	@Produce	json
//	@Success	200	{array}	Preset
//	@Router		/wallet-card-presets [get]
func (h *Handler) presets(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, Presets)
}

// templates godoc
//
//	@Summary	Template ids for wallet cards (images are frontend assets)
//	@Tags		wallets
//	@Security	BearerAuth
//	@Produce	json
//	@Success	200	{array}	Template
//	@Router		/wallet-card-templates [get]
func (h *Handler) templates(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, Templates)
}
