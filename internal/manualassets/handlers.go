package manualassets

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/JonathanRainee/hzzt/internal/auth"
	"github.com/JonathanRainee/hzzt/internal/currency"
	"github.com/JonathanRainee/hzzt/internal/platform/apperror"
	"github.com/JonathanRainee/hzzt/internal/platform/httpx"
	"github.com/JonathanRainee/hzzt/internal/platform/money"
	"github.com/JonathanRainee/hzzt/internal/spaces"
	"github.com/JonathanRainee/hzzt/internal/transactions"
)

// Handler exposes the manual asset endpoints.
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
// manage assets. Space routes carry the full path because the spaces handler
// owns the /spaces/{spaceId} mount point.
func (h *Handler) Mount(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(h.auth.Middleware, h.spaces.Middleware)
		const p = "/spaces/{spaceId}/manual-assets"
		r.Get(p, h.list)
		r.Post(p, h.create)
		r.Get(p+"/{id}", h.get)
		r.Patch(p+"/{id}", h.update)
		r.Delete(p+"/{id}", h.delete)
		r.Post(p+"/{id}/settlements", h.settle)
		r.Delete(p+"/{id}/settlements/{txnId}", h.deleteSettlement)
	})
}

// --- DTOs ------------------------------------------------------------------

// ValueChangeDTO is one entry of an asset's value history.
type ValueChangeDTO struct {
	PreviousValue money.Amount `json:"previousValue" swaggertype:"string"`
	NewValue      money.Amount `json:"newValue" swaggertype:"string"`
	Reason        string       `json:"reason" enums:"MANUAL,SETTLEMENT"`
	TransactionID *uuid.UUID   `json:"transactionId"`
	ChangedAt     time.Time    `json:"changedAt"`
}

// AssetDTO is the public representation of a manual asset.
type AssetDTO struct {
	ID                       uuid.UUID        `json:"id"`
	Kind                     string           `json:"kind" enums:"FIXED_ASSET,RECEIVABLE,LIABILITY"`
	Name                     string           `json:"name"`
	Icon                     string           `json:"icon"`
	Currency                 string           `json:"currency" example:"IDR"`
	CurrentValue             money.Amount     `json:"currentValue" swaggertype:"string" example:"500.00"` // in the asset's currency (IDR and JPY: no decimals, others 2); a liability's outstanding amount
	CounterpartyName         *string          `json:"counterpartyName"`
	DueDate                  *string          `json:"dueDate" example:"2026-12-31"`
	EstimatedAnnualChangePct *decimal.Decimal `json:"estimatedAnnualChangePct" swaggertype:"string"`
	Note                     *string          `json:"note"`
	History                  []ValueChangeDTO `json:"history,omitempty"` // detail only, newest first
	CreatedAt                time.Time        `json:"createdAt"`
	UpdatedAt                time.Time        `json:"updatedAt"`
}

func toDTO(a Asset) AssetDTO {
	d := AssetDTO{
		ID: a.ID, Kind: a.Kind, Name: a.Name, Icon: a.Icon, Currency: strings.TrimSpace(a.Currency), CurrentValue: money.NewAmount(a.CurrentValue, a.Currency),
		CounterpartyName: a.CounterpartyName, Note: a.Note, CreatedAt: a.CreatedAt.UTC(), UpdatedAt: a.UpdatedAt.UTC(),
	}
	if a.DueDate != nil {
		s := a.DueDate.Format("2006-01-02")
		d.DueDate = &s
	}
	if a.EstimatedAnnualChangePct.Valid {
		v := a.EstimatedAnnualChangePct.Decimal
		d.EstimatedAnnualChangePct = &v
	}
	for _, h := range a.History {
		c := ValueChangeDTO{PreviousValue: money.NewAmount(h.PreviousValue, a.Currency), NewValue: money.NewAmount(h.NewValue, a.Currency), Reason: h.Reason, ChangedAt: h.ChangedAt.UTC()}
		if h.TransactionID.Valid {
			id := h.TransactionID.UUID
			c.TransactionID = &id
		}
		d.History = append(d.History, c)
	}
	return d
}

// ListResponse lists assets with their converted totals per kind.
type ListResponse struct {
	Items  []AssetDTO                          `json:"items"`
	Totals map[string]currency.AggregateResult `json:"totals"`
}

type createRequest struct {
	Kind                     string           `json:"kind" validate:"required,oneof=FIXED_ASSET RECEIVABLE LIABILITY"`
	Name                     string           `json:"name" validate:"required,min=1,max=120"`
	Icon                     *string          `json:"icon" validate:"omitempty,max=64"`
	Currency                 *string          `json:"currency" validate:"omitempty,max=8"`
	CurrentValue             *decimal.Decimal `json:"currentValue" validate:"required" swaggertype:"string" example:"500"`
	CounterpartyName         *string          `json:"counterpartyName" validate:"omitempty,max=120"`
	DueDate                  *string          `json:"dueDate" validate:"omitempty,max=10" example:"2026-12-31"`
	EstimatedAnnualChangePct *decimal.Decimal `json:"estimatedAnnualChangePct" swaggertype:"string"`
	Note                     *string          `json:"note" validate:"omitempty,max=2000"`
}

type nullable[T any] struct {
	Set   bool
	Value *T
}

func (n *nullable[T]) UnmarshalJSON(b []byte) error {
	n.Set = true
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return nil
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	n.Value = &v
	return nil
}

type updateRequest struct {
	Name                     *string                   `json:"name" validate:"omitempty,min=1,max=120"`
	Icon                     *string                   `json:"icon" validate:"omitempty,max=64"`
	Currency                 *string                   `json:"currency" validate:"omitempty,max=8"`
	CurrentValue             *decimal.Decimal          `json:"currentValue" swaggertype:"string"`
	CounterpartyName         nullable[string]          `json:"counterpartyName" swaggertype:"string"`
	DueDate                  nullable[string]          `json:"dueDate" swaggertype:"string"`
	EstimatedAnnualChangePct nullable[decimal.Decimal] `json:"estimatedAnnualChangePct" swaggertype:"string"`
	Note                     nullable[string]          `json:"note" swaggertype:"string"`
}

type settleRequest struct {
	WalletID   uuid.UUID        `json:"walletId" validate:"required"`
	Amount     *decimal.Decimal `json:"amount" validate:"required" swaggertype:"string" example:"200"`
	OccurredAt *time.Time       `json:"occurredAt"`
	Note       *string          `json:"note" validate:"omitempty,max=1000"`
}

// SettleResponse is the settlement transaction and the asset after it.
type SettleResponse struct {
	Transaction transactions.TransactionDTO `json:"transaction"`
	Asset       AssetDTO                    `json:"asset"`
}

var defaultIcons = map[string]string{KindFixedAsset: "🏠", KindReceivable: "🤝", KindLiability: "💳"}

var maxAmount = decimal.New(1, 20)

func fieldsErr(f map[string]any) error {
	if len(f) == 0 {
		return nil
	}
	return apperror.Validation("Validation failed", f)
}

func checkValue(f map[string]any, field string, d *decimal.Decimal, positive bool) {
	switch {
	case d == nil:
	case d.IsNegative():
		f[field] = "must not be negative"
	case positive && !d.IsPositive():
		f[field] = "must be greater than 0"
	case !d.Round(8).Equal(*d):
		f[field] = "must have at most 8 decimal places"
	case d.GreaterThanOrEqual(maxAmount):
		f[field] = "is too large"
	}
}

func cleanName(f map[string]any, n *string) *string {
	if n == nil {
		return nil
	}
	v := strings.TrimSpace(*n)
	if v == "" || utf8.RuneCountInString(v) > 60 {
		f["name"] = "must be 1 to 60 characters"
	}
	return &v
}

func cleanIcon(f map[string]any, i *string) *string {
	if i == nil {
		return nil
	}
	v := strings.TrimSpace(*i)
	if v == "" || utf8.RuneCountInString(v) > 16 {
		f["icon"] = "must be an emoji (1 to 16 characters)"
	}
	return &v
}

func cleanCurrency(f map[string]any, c *string) *string {
	if c == nil {
		return nil
	}
	v := strings.ToUpper(strings.TrimSpace(*c))
	if !money.IsSupportedCurrency(v) {
		f["currency"] = "must be a supported ISO 4217 currency code"
	}
	return &v
}

func cleanText(f map[string]any, field string, s *string, max int) *string {
	if s == nil {
		return nil
	}
	v := strings.TrimSpace(*s)
	if utf8.RuneCountInString(v) > max {
		f[field] = "is too long"
	}
	if v == "" {
		return nil
	}
	return &v
}

func parseDate(f map[string]any, field string, s *string) *time.Time {
	if s == nil {
		return nil
	}
	t, err := time.Parse("2006-01-02", strings.TrimSpace(*s))
	if err != nil {
		f[field] = "must be a date like 2026-12-31"
		return nil
	}
	return &t
}

func checkChange(f map[string]any, d *decimal.Decimal) {
	if d == nil {
		return
	}
	if d.LessThan(decimal.NewFromInt(-100)) || d.GreaterThan(decimal.NewFromInt(1000)) {
		f["estimatedAnnualChangePct"] = "must be between -100 and 1000"
	} else if !d.Round(4).Equal(*d) {
		f["estimatedAnnualChangePct"] = "must have at most 4 decimal places"
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func assetID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, errAssetNF
	}
	return id, nil
}

// --- handlers --------------------------------------------------------------

// list godoc
//
//	@Summary		List fixed assets, money owed and debt
//	@Description	`totals` converts each kind to the space base currency; items without a usable FX rate are left out and reported in that kind's `warnings`.
//	@Tags			manual-assets
//	@Security		BearerAuth
//	@Produce		json
//	@Param			spaceId	path		string	true	"Space ID"
//	@Param			kind	query		string	false	"FIXED_ASSET, RECEIVABLE or LIABILITY"
//	@Success		200		{object}	ListResponse
//	@Failure		400		{object}	httpx.ErrorEnvelope
//	@Failure		404		{object}	httpx.ErrorEnvelope
//	@Router			/spaces/{spaceId}/manual-assets [get]
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	kind := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("kind")))
	if kind != "" && kind != KindFixedAsset && kind != KindReceivable && kind != KindLiability {
		httpx.WriteError(w, r, apperror.Validation("Validation failed", map[string]any{"kind": "must be one of: FIXED_ASSET RECEIVABLE LIABILITY"}))
		return
	}
	res, err := h.svc.List(r.Context(), spaces.SpaceIDFromContext(r.Context()), kind)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := ListResponse{Items: make([]AssetDTO, 0, len(res.Items)), Totals: res.Totals}
	for _, a := range res.Items {
		out.Items = append(out.Items, toDTO(a))
	}
	writeJSON(w, http.StatusOK, out)
}

// create godoc
//
//	@Summary		Create a fixed asset, a receivable (money owed to me) or a liability (debt)
//	@Description	For a liability `currentValue` is the outstanding amount. `estimatedAnnualChangePct` applies to fixed assets only.
//	@Tags			manual-assets
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			spaceId	path		string			true	"Space ID"
//	@Param			body	body		createRequest	true	"Asset"
//	@Success		201		{object}	AssetDTO
//	@Failure		400		{object}	httpx.ErrorEnvelope
//	@Failure		404		{object}	httpx.ErrorEnvelope
//	@Router			/spaces/{spaceId}/manual-assets [post]
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	sp, _ := spaces.SpaceFromContext(r.Context())
	req, err := httpx.DecodeAndValidate[createRequest](r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	f := map[string]any{}
	in := CreateInput{Kind: req.Kind, Name: *cleanName(f, &req.Name), CurrentValue: *req.CurrentValue}
	if i := cleanIcon(f, req.Icon); i != nil {
		in.Icon = *i
	} else {
		in.Icon = defaultIcons[req.Kind]
	}
	if c := cleanCurrency(f, req.Currency); c != nil {
		in.Currency = *c
	}
	checkValue(f, "currentValue", req.CurrentValue, false)
	in.Counterparty = cleanText(f, "counterpartyName", req.CounterpartyName, 60)
	in.Note = cleanText(f, "note", req.Note, 1000)
	in.DueDate = parseDate(f, "dueDate", req.DueDate)
	checkChange(f, req.EstimatedAnnualChangePct)
	if req.EstimatedAnnualChangePct != nil {
		if req.Kind != KindFixedAsset {
			f["estimatedAnnualChangePct"] = "only applies to fixed assets"
		}
		in.EstimatedChange = req.EstimatedAnnualChangePct
	}
	if err := fieldsErr(f); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	a, err := h.svc.Create(r.Context(), sp, user.ID, in)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toDTO(a))
}

// get godoc
//
//	@Summary	Get an asset with its value history
//	@Tags		manual-assets
//	@Security	BearerAuth
//	@Produce	json
//	@Param		spaceId	path		string	true	"Space ID"
//	@Param		id		path		string	true	"Asset ID"
//	@Success	200		{object}	AssetDTO
//	@Failure	404		{object}	httpx.ErrorEnvelope
//	@Router		/spaces/{spaceId}/manual-assets/{id} [get]
func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := assetID(r)
	var a Asset
	if err == nil {
		a, err = h.svc.Get(r.Context(), spaces.SpaceIDFromContext(r.Context()), id)
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toDTO(a))
}

// update godoc
//
//	@Summary		Update an asset
//	@Description	Changing `currentValue` is recorded in the value history. `counterpartyName`, `dueDate`, `estimatedAnnualChangePct` and `note` accept null to clear. The currency can only change while the asset has no settlements (MANUAL_ASSET_CURRENCY_LOCKED).
//	@Tags			manual-assets
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			spaceId	path		string			true	"Space ID"
//	@Param			id		path		string			true	"Asset ID"
//	@Param			body	body		updateRequest	true	"Fields to change"
//	@Success		200		{object}	AssetDTO
//	@Failure		400		{object}	httpx.ErrorEnvelope
//	@Failure		404		{object}	httpx.ErrorEnvelope
//	@Failure		422		{object}	httpx.ErrorEnvelope	"MANUAL_ASSET_CURRENCY_LOCKED"
//	@Router			/spaces/{spaceId}/manual-assets/{id} [patch]
func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	id, err := assetID(r)
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
	in := UpdateInput{Name: cleanName(f, req.Name), Icon: cleanIcon(f, req.Icon), Currency: cleanCurrency(f, req.Currency), CurrentValue: req.CurrentValue}
	checkValue(f, "currentValue", req.CurrentValue, false)
	if req.CounterpartyName.Set {
		in.Counterparty = Optional[string]{Set: true, Value: cleanText(f, "counterpartyName", req.CounterpartyName.Value, 60)}
	}
	if req.Note.Set {
		in.Note = Optional[string]{Set: true, Value: cleanText(f, "note", req.Note.Value, 1000)}
	}
	if req.DueDate.Set {
		in.DueDate = Optional[time.Time]{Set: true, Value: parseDate(f, "dueDate", req.DueDate.Value)}
	}
	if req.EstimatedAnnualChangePct.Set {
		checkChange(f, req.EstimatedAnnualChangePct.Value)
		in.EstimatedChange = Optional[decimal.Decimal]{Set: true, Value: req.EstimatedAnnualChangePct.Value}
	}
	if err := fieldsErr(f); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	a, err := h.svc.Update(r.Context(), spaces.SpaceIDFromContext(r.Context()), id, in)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toDTO(a))
}

// delete godoc
//
//	@Summary	Delete an asset (soft delete; its settlements are kept for history)
//	@Tags		manual-assets
//	@Security	BearerAuth
//	@Param		spaceId	path	string	true	"Space ID"
//	@Param		id		path	string	true	"Asset ID"
//	@Success	204
//	@Failure	404	{object}	httpx.ErrorEnvelope
//	@Router		/spaces/{spaceId}/manual-assets/{id} [delete]
func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	id, err := assetID(r)
	if err == nil {
		err = h.svc.Delete(r.Context(), spaces.SpaceIDFromContext(r.Context()), id)
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// settle godoc
//
//	@Summary		Settle money owed or pay down a debt through a wallet
//	@Description	RECEIVABLE: the wallet is credited and the amount owed goes down (money received back). LIABILITY: the wallet is debited and the outstanding amount goes down (a debt payment). Recorded as a TRANSFER transaction tied to the asset. The wallet must use the asset's currency (422 SETTLEMENT_CURRENCY_MISMATCH) and the amount must not exceed the current value (422 SETTLEMENT_EXCEEDS_BALANCE). Fixed assets cannot be settled.
//	@Tags			manual-assets
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			spaceId	path		string			true	"Space ID"
//	@Param			id		path		string			true	"Asset ID"
//	@Param			body	body		settleRequest	true	"Settlement"
//	@Success		201		{object}	SettleResponse
//	@Failure		400		{object}	httpx.ErrorEnvelope
//	@Failure		404		{object}	httpx.ErrorEnvelope
//	@Failure		422		{object}	httpx.ErrorEnvelope	"SETTLEMENT_EXCEEDS_BALANCE / SETTLEMENT_CURRENCY_MISMATCH / SETTLEMENT_NOT_SUPPORTED"
//	@Router			/spaces/{spaceId}/manual-assets/{id}/settlements [post]
func (h *Handler) settle(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	id, err := assetID(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	req, err := httpx.DecodeAndValidate[settleRequest](r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	f := map[string]any{}
	checkValue(f, "amount", req.Amount, true)
	if err := fieldsErr(f); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	res, err := h.svc.Settle(r.Context(), spaces.SpaceIDFromContext(r.Context()), user.ID, id, SettleInput{
		WalletID: req.WalletID, Amount: *req.Amount, OccurredAt: req.OccurredAt, Note: req.Note,
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, SettleResponse{Transaction: transactions.ToDTO(res.Transaction), Asset: toDTO(res.Asset)})
}

// deleteSettlement godoc
//
//	@Summary	Reverse a settlement: the wallet effect is undone and the asset's value goes back up
//	@Tags		manual-assets
//	@Security	BearerAuth
//	@Param		spaceId	path	string	true	"Space ID"
//	@Param		id		path	string	true	"Asset ID"
//	@Param		txnId	path	string	true	"Settlement transaction ID"
//	@Success	204
//	@Failure	404	{object}	httpx.ErrorEnvelope
//	@Router		/spaces/{spaceId}/manual-assets/{id}/settlements/{txnId} [delete]
func (h *Handler) deleteSettlement(w http.ResponseWriter, r *http.Request) {
	spaceID := spaces.SpaceIDFromContext(r.Context())
	aid, err := assetID(r)
	var tid uuid.UUID
	if err == nil {
		tid, err = uuid.Parse(chi.URLParam(r, "txnId"))
		if err != nil {
			err = apperror.New(http.StatusNotFound, transactions.CodeTransactionNotFound, "Transaction not found")
		}
	}
	if err == nil {
		err = h.svc.DeleteSettlement(r.Context(), spaceID, aid, tid)
		if errors.Is(err, ErrNotASettlement) {
			err = apperror.New(http.StatusNotFound, transactions.CodeTransactionNotFound, "Settlement not found")
		}
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
