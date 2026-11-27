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
	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
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
		r.Get(p+"/{id}/delete-impact", h.deleteImpact)
		r.Get(p+"/{id}/participants", h.participants)
		r.Put(p+"/{id}/participants", h.setParticipants)
		r.Delete(p+"/{id}/participants", h.stopSharing)
		r.Get(p+"/{id}/share-changes", h.shareChanges)
	})

	// Full paths, not r.Route("/me"): the users handler already owns that mount point.
	r.Group(func(r chi.Router) {
		r.Use(h.auth.Middleware)
		r.Get("/me/shared-wallet-invitations", h.invitations)
		r.Post("/me/shared-wallet-invitations/{participantId}/accept", h.acceptInvitation)
		r.Post("/me/shared-wallet-invitations/{participantId}/decline", h.declineInvitation)
		r.Post("/me/shared-wallets/{participantId}/leave", h.leave)
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
	IsShared    bool          `json:"isShared"`
	// Ownership is OWNER for this space's own wallets and SHARED_IN for ones shared
	// into it by someone else (read-only; present only with ?include=shared).
	Ownership string `json:"ownership" enums:"OWNER,SHARED_IN"`
	// MyShareBps and MyBalance are set only for a SHARED_IN wallet: the viewer's
	// percentage of it and their share of Balance, in the wallet's own currency.
	MyShareBps *int          `json:"myShareBps,omitempty" example:"3000"`
	MyBalance  *money.Amount `json:"myBalance,omitempty" swaggertype:"string"`
	CreatedAt  time.Time     `json:"createdAt"`
}

// ToDTO converts a wallet owned by the caller's own space.
func ToDTO(w Wallet) WalletDTO {
	return WalletDTO{
		ID: w.ID, Type: w.Type, Name: w.Name, Icon: w.Icon, Currency: strings.TrimSpace(w.Currency),
		Balance: money.NewAmount(w.Balance, w.Currency), Reserved: money.NewAmount(w.Reserved, w.Currency),
		Available: money.NewAmount(w.Available(), w.Currency), OverReserved: w.OverReserved(),
		IsFavorite: w.IsFavorite, SortOrder: int(w.SortOrder),
		CardStyle: w.Style, IsShared: w.IsShared, Ownership: OwnershipOwner, CreatedAt: w.CreatedAt.UTC(),
	}
}

// sharedInToDTO converts a wallet shared into the caller's space by someone else: read-only,
// with the viewer's share and share of the balance instead of the full one.
func sharedInToDTO(w SharedIn) WalletDTO {
	d := ToDTO(w.Wallet)
	d.Ownership = OwnershipSharedIn
	bps := w.MyShareBps
	d.MyShareBps = &bps
	mb := money.NewAmount(w.MyBalance, w.Currency)
	d.MyBalance = &mb
	return d
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
//	@Description	Favorites first, then sortOrder. `total` is converted to the space base currency; wallets without a usable FX rate are left out and reported in `total.warnings`. Each item also carries `baseBalance` and `pctOfTotal` (percent of the total, 2 decimals, adding up to exactly 100.00; null without an FX rate) for pie charts. With `?include=shared`, also lists wallets shared into this space by another space's owner: these come back `ownership: SHARED_IN`, read-only, with `myShareBps` and `myBalance` (my share of the wallet's balance); they are not part of `total`.
//	@Tags			wallets
//	@Security		BearerAuth
//	@Produce		json
//	@Param			spaceId	path		string	true	"Space ID"
//	@Param			include	query		string	false	"shared: also include wallets shared into this space"
//	@Success		200		{object}	ListResponse
//	@Failure		404		{object}	httpx.ErrorEnvelope
//	@Router			/spaces/{spaceId}/wallets [get]
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	spaceID := spaces.SpaceIDFromContext(r.Context())
	res, err := h.svc.List(r.Context(), spaceID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	user, _ := auth.UserFromContext(r.Context())
	prefs, err := h.svc.Prefs(r.Context(), user.ID, spaceID)
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
	if strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include")), "shared") {
		sharedIn, err := h.svc.ListSharedIn(r.Context(), spaceID)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		for _, x := range sharedIn {
			items = append(items, sharedInToDTO(x))
		}
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
//	@Param		force	query	bool	false	"Required when the balance is not zero or the wallet has transactions"
//	@Success	204
//	@Failure	404	{object}	httpx.ErrorEnvelope
//	@Failure	409	{object}	httpx.ErrorEnvelope	"WALLET_HAS_TRANSACTIONS (details: transactions, transfers, goalReserves)"
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

// DeleteImpactDTO is what deleting a wallet affects.
type DeleteImpactDTO struct {
	// Transactions counts every transaction that points at the wallet (the history that stays behind); Transfers is how many
	// of them are transfers to or from another of the space's wallets.
	Transactions int `json:"transactions" example:"12"`
	Transfers    int `json:"transfers" example:"3"`
	// GoalReserves is the number of goal reservations that are removed with the wallet.
	GoalReserves int          `json:"goalReserves" example:"1"`
	Balance      money.Amount `json:"balance" swaggertype:"string" example:"134.26"`
	Currency     string       `json:"currency" example:"USD"`
}

// deleteImpact godoc
//
//	@Summary		What deleting a wallet affects
//	@Description	Counts the wallet's transactions (as source or destination), the transfers among them and the goal reservations that go with it, plus its balance. DELETE needs force=true when the balance is not zero or the wallet has transactions.
//	@Tags			wallets
//	@Security		BearerAuth
//	@Produce		json
//	@Param			spaceId	path		string	true	"Space ID"
//	@Param			id		path		string	true	"Wallet ID"
//	@Success		200		{object}	DeleteImpactDTO
//	@Failure		404		{object}	httpx.ErrorEnvelope
//	@Router			/spaces/{spaceId}/wallets/{id}/delete-impact [get]
func (h *Handler) deleteImpact(w http.ResponseWriter, r *http.Request) {
	id, err := walletID(r)
	var imp DeleteImpact
	if err == nil {
		imp, err = h.svc.DeleteImpact(r.Context(), spaces.SpaceIDFromContext(r.Context()), id)
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, DeleteImpactDTO{
		Transactions: imp.Transactions, Transfers: imp.Transfers, GoalReserves: imp.GoalReserves,
		Balance: money.NewAmount(imp.Balance, imp.Currency), Currency: imp.Currency,
	})
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

// --- sharing -----------------------------------------------------------------------
//
// Sharing a wallet changes only how it is counted (each transaction attributed by
// percentage, and net worth split between the owner's and participants' spaces), never
// who may spend from it: creating, editing or deleting a transaction on the wallet, and
// editing the wallet itself, stay gated by Space membership exactly as before.

// ParticipantDTO is a person a shared wallet is shared with. Other people's email
// addresses are masked unless the viewer is the owner (GET .../participants).
type ParticipantDTO struct {
	ID          uuid.UUID `json:"id"`
	Role        string    `json:"role" enums:"OWNER,PARTICIPANT"`
	DisplayName string    `json:"displayName"`
	Email       *string   `json:"email"`
	SharePct    string    `json:"sharePct" example:"30.00"`
	ShareBps    int       `json:"shareBps" example:"3000"`
	Status      string    `json:"status" enums:"ACTIVE,INVITED,DECLINED,REMOVED"`
	IsMe        bool      `json:"isMe"`
	InvitedAt   time.Time `json:"invitedAt"`
}

func toParticipantDTOs(ps []ParticipantView) []ParticipantDTO {
	out := make([]ParticipantDTO, 0, len(ps))
	for _, p := range ps {
		out = append(out, ParticipantDTO{
			ID: p.ID, Role: p.Role, DisplayName: p.DisplayName, Email: p.Email, SharePct: bpsPct(p.ShareBps),
			ShareBps: p.ShareBps, Status: p.Status, IsMe: p.IsMe, InvitedAt: p.InvitedAt,
		})
	}
	return out
}

// ParticipantsResponse lists the people a wallet is shared with.
type ParticipantsResponse struct {
	Items []ParticipantDTO `json:"items"`
}

// participants godoc
//
//	@Summary	List who a wallet is shared with (owner only)
//	@Tags		wallet-sharing
//	@Security	BearerAuth
//	@Produce	json
//	@Param		spaceId	path		string	true	"Space ID"
//	@Param		id		path		string	true	"Wallet ID"
//	@Success	200		{object}	ParticipantsResponse
//	@Failure	403		{object}	httpx.ErrorEnvelope	"OWNER_ONLY"
//	@Failure	404		{object}	httpx.ErrorEnvelope
//	@Router		/spaces/{spaceId}/wallets/{id}/participants [get]
func (h *Handler) participants(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	id, err := walletID(r)
	var ps []ParticipantView
	if err == nil {
		ps, err = h.svc.Participants(r.Context(), spaces.SpaceIDFromContext(r.Context()), id, user)
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ParticipantsResponse{Items: toParticipantDTOs(ps)})
}

type participantRequest struct {
	ParticipantID *uuid.UUID `json:"participantId"`
	Email         *string    `json:"email" validate:"omitempty,max=254"`
	DisplayName   string     `json:"displayName" validate:"required,min=1,max=120"`
	SharePct      string     `json:"sharePct" validate:"required,max=16" example:"30.00"`
	IsOwner       bool       `json:"isOwner"`
}

// SetParticipantsRequest is the full replacement list of participants.
type SetParticipantsRequest struct {
	Participants []participantRequest `json:"participants" validate:"required,min=1,max=50,dive"`
}

// SetParticipantsResponse is the new participant list plus the wallet as it is now, so the
// client can update in place (isShared, participants) without refetching.
type SetParticipantsResponse struct {
	Items  []ParticipantDTO `json:"items"`
	Wallet WalletDTO        `json:"wallet"`
}

// setParticipants godoc
//
//	@Summary		Share a wallet: replace the participant list (owner only)
//	@Description	Full replacement. Include yourself (`isOwner: true`, or your own email) with a share above 0. Shares are percentages with up to 2 decimals and the shares of all participants must add up to exactly 100.00 (422 SHARE_TOTAL_INVALID); at most 10 people (422 PARTICIPANT_LIMIT). A participant with an `email` becomes INVITED, one with only a `displayName` is ACTIVE right away (someone without an account). The response is identical whether or not an email belongs to an account. Entries left out of the list are removed. This only changes how the wallet is counted; who may spend from it is unaffected. Only the owner may edit the participant list (403 OWNER_ONLY for other members of the space).
//	@Tags			wallet-sharing
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			spaceId	path		string					true	"Space ID"
//	@Param			id		path		string					true	"Wallet ID"
//	@Param			body	body		SetParticipantsRequest	true	"Participants"
//	@Success		200		{object}	SetParticipantsResponse
//	@Failure		400		{object}	httpx.ErrorEnvelope
//	@Failure		403		{object}	httpx.ErrorEnvelope	"OWNER_ONLY"
//	@Failure		404		{object}	httpx.ErrorEnvelope
//	@Failure		422		{object}	httpx.ErrorEnvelope	"SHARE_TOTAL_INVALID / PARTICIPANT_LIMIT"
//	@Router			/spaces/{spaceId}/wallets/{id}/participants [put]
func (h *Handler) setParticipants(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	id, err := walletID(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	req, err := httpx.DecodeAndValidate[SetParticipantsRequest](r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	in := make([]ParticipantInput, 0, len(req.Participants))
	for i, p := range req.Participants {
		pct, perr := decimal.NewFromString(strings.TrimSpace(p.SharePct))
		if perr != nil {
			httpx.WriteError(w, r, apperror.Validation("Validation failed", map[string]any{
				"participants[" + strconv.Itoa(i) + "].sharePct": "must be a number like 30.00"}))
			return
		}
		pi := ParticipantInput{ParticipantID: p.ParticipantID, DisplayName: p.DisplayName, SharePct: pct, IsOwner: p.IsOwner}
		if p.Email != nil {
			pi.Email = strings.ToLower(strings.TrimSpace(*p.Email))
		}
		in = append(in, pi)
	}
	spaceID := spaces.SpaceIDFromContext(r.Context())
	ps, err := h.svc.SetParticipants(r.Context(), spaceID, id, user, in)
	var wl Wallet
	if err == nil {
		wl, err = h.svc.Get(r.Context(), spaceID, id)
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, SetParticipantsResponse{Items: toParticipantDTOs(ps), Wallet: ToDTO(wl)})
}

// stopSharing godoc
//
//	@Summary		Stop sharing: 100% returns to the owner and everyone else is removed (owner only)
//	@Description	Answers 204. With `?return=wallet` it answers 200 with the wallet as it is now, so the client can update in place.
//	@Tags			wallet-sharing
//	@Security		BearerAuth
//	@Produce		json
//	@Param			spaceId	path	string	true	"Space ID"
//	@Param			id		path	string	true	"Wallet ID"
//	@Param			return	query	string	false	"wallet: answer 200 with the wallet instead of 204"
//	@Success		204
//	@Success		200	{object}	WalletDTO
//	@Failure	403	{object}	httpx.ErrorEnvelope	"OWNER_ONLY"
//	@Failure	404	{object}	httpx.ErrorEnvelope
//	@Router		/spaces/{spaceId}/wallets/{id}/participants [delete]
func (h *Handler) stopSharing(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	spaceID := spaces.SpaceIDFromContext(r.Context())
	id, err := walletID(r)
	ret := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("return")))
	if err == nil && ret != "" && ret != "wallet" {
		err = apperror.Validation("Validation failed", map[string]any{"return": "must be: wallet"})
	}
	if err == nil {
		err = h.svc.StopSharing(r.Context(), spaceID, id, user)
	}
	var wl Wallet
	if err == nil && ret == "wallet" {
		wl, err = h.svc.Get(r.Context(), spaceID, id)
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if ret == "wallet" {
		writeJSON(w, http.StatusOK, ToDTO(wl))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ShareChangeDTO is one entry of the audit trail.
type ShareChangeDTO struct {
	ID        uuid.UUID       `json:"id"`
	ChangedBy uuid.UUID       `json:"changedById"`
	Before    json.RawMessage `json:"before" swaggertype:"object"`
	After     json.RawMessage `json:"after" swaggertype:"object"`
	CreatedAt time.Time       `json:"createdAt"`
}

// shareChanges godoc
//
//	@Summary	Audit trail of a wallet's sharing arrangement, newest first (owner only)
//	@Tags		wallet-sharing
//	@Security	BearerAuth
//	@Produce	json
//	@Param		spaceId	path		string	true	"Space ID"
//	@Param		id		path		string	true	"Wallet ID"
//	@Success	200		{array}		ShareChangeDTO
//	@Failure	403		{object}	httpx.ErrorEnvelope	"OWNER_ONLY"
//	@Failure	404		{object}	httpx.ErrorEnvelope
//	@Router		/spaces/{spaceId}/wallets/{id}/share-changes [get]
func (h *Handler) shareChanges(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	id, err := walletID(r)
	var rows []sqlc.WalletShareChange
	if err == nil {
		rows, err = h.svc.ShareChanges(r.Context(), spaces.SpaceIDFromContext(r.Context()), id, user.ID)
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := make([]ShareChangeDTO, 0, len(rows))
	for _, c := range rows {
		out = append(out, ShareChangeDTO{ID: c.ID, ChangedBy: c.ChangedByID, Before: c.Before, After: c.After, CreatedAt: c.CreatedAt.UTC()})
	}
	writeJSON(w, http.StatusOK, out)
}

// InvitationDTO is a pending invitation to co-own a wallet.
type InvitationDTO struct {
	ParticipantID uuid.UUID `json:"participantId"`
	OwnerName     string    `json:"ownerName"`
	Wallet        struct {
		Name     string `json:"name"`
		Icon     string `json:"icon"`
		Currency string `json:"currency"`
	} `json:"wallet"`
	SharePct  string    `json:"sharePct" example:"30.00"`
	ShareBps  int       `json:"shareBps"`
	InvitedAt time.Time `json:"invitedAt"`
}

// invitations godoc
//
//	@Summary		My pending shared-wallet invitations
//	@Description	Invitations addressed to my account or to my account's email (case-insensitive), including ones sent before I registered. Shows the wallet and my share, never the owner's space.
//	@Tags			wallet-sharing
//	@Security		BearerAuth
//	@Produce		json
//	@Success		200	{array}	InvitationDTO
//	@Router			/me/shared-wallet-invitations [get]
func (h *Handler) invitations(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	inv, err := h.svc.Invitations(r.Context(), user)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := make([]InvitationDTO, 0, len(inv))
	for _, i := range inv {
		d := InvitationDTO{ParticipantID: i.ParticipantID, OwnerName: i.OwnerName, SharePct: bpsPct(i.ShareBps), ShareBps: i.ShareBps, InvitedAt: i.InvitedAt}
		d.Wallet.Name, d.Wallet.Icon, d.Wallet.Currency = i.Name, i.Icon, i.Currency
		out = append(out, d)
	}
	writeJSON(w, http.StatusOK, out)
}

type acceptRequest struct {
	SpaceID uuid.UUID `json:"spaceId" validate:"required" swaggertype:"string"`
}

// AcceptResponse says where the accepted wallet can be opened: GET /spaces/{spaceId}/wallets?include=shared.
type AcceptResponse struct {
	SpaceID  uuid.UUID `json:"spaceId"`
	WalletID uuid.UUID `json:"walletId"`
}

func participantID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "participantId"))
	if err != nil {
		return uuid.Nil, errInvitationNF
	}
	return id, nil
}

// acceptInvitation godoc
//
//	@Summary		Accept a shared-wallet invitation
//	@Description	The wallet is attached to the given space (you must be a member of it, and it must not be the owner's space); that space then counts my share and lists it with `include=shared`.
//	@Tags			wallet-sharing
//	@Security		BearerAuth
//	@Accept			json
//	@Param			participantId	path		string			true	"Invitation (participant) ID"
//	@Produce		json
//	@Param			body			body		acceptRequest	true	"Space to attach it to"
//	@Success		200				{object}	AcceptResponse
//	@Failure		400	{object}	httpx.ErrorEnvelope
//	@Failure		404	{object}	httpx.ErrorEnvelope
//	@Failure		409	{object}	httpx.ErrorEnvelope	"SHARE_INVITATION_ANSWERED"
//	@Router			/me/shared-wallet-invitations/{participantId}/accept [post]
func (h *Handler) acceptInvitation(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	id, err := participantID(r)
	var req acceptRequest
	if err == nil {
		req, err = httpx.DecodeAndValidate[acceptRequest](r)
	}
	var walletID uuid.UUID
	if err == nil {
		walletID, err = h.svc.AcceptInvitation(r.Context(), user, id, req.SpaceID)
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, AcceptResponse{SpaceID: req.SpaceID, WalletID: walletID})
}

// declineInvitation godoc
//
//	@Summary	Decline a shared-wallet invitation (the share stays allocated until the owner changes it)
//	@Tags		wallet-sharing
//	@Security	BearerAuth
//	@Param		participantId	path	string	true	"Invitation (participant) ID"
//	@Success	204
//	@Failure	404	{object}	httpx.ErrorEnvelope
//	@Failure	409	{object}	httpx.ErrorEnvelope	"SHARE_INVITATION_ANSWERED"
//	@Router		/me/shared-wallet-invitations/{participantId}/decline [post]
func (h *Handler) declineInvitation(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	id, err := participantID(r)
	if err == nil {
		err = h.svc.DeclineInvitation(r.Context(), user, id)
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// leave godoc
//
//	@Summary		Leave a shared wallet
//	@Description	My share stays allocated but becomes a name-only participant; the owner finds the change in the audit trail.
//	@Tags			wallet-sharing
//	@Security		BearerAuth
//	@Param			participantId	path	string	true	"My participant ID"
//	@Success		204
//	@Failure		404	{object}	httpx.ErrorEnvelope
//	@Failure		422	{object}	httpx.ErrorEnvelope	"SHARE_OWNER_CANNOT_LEAVE"
//	@Router			/me/shared-wallets/{participantId}/leave [post]
func (h *Handler) leave(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	id, err := participantID(r)
	if err == nil {
		err = h.svc.LeaveSharedWallet(r.Context(), user, id)
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
