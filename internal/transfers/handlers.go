package transfers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/JonathanRainee/hzzt/internal/auth"
	"github.com/JonathanRainee/hzzt/internal/investments"
	"github.com/JonathanRainee/hzzt/internal/ledger"
	"github.com/JonathanRainee/hzzt/internal/platform/apperror"
	"github.com/JonathanRainee/hzzt/internal/platform/httpx"
	"github.com/JonathanRainee/hzzt/internal/spaces"
	"github.com/JonathanRainee/hzzt/internal/transactions"
)

// Handler exposes the transfer endpoints.
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
// transfer between wallets. Space routes carry the full path because the
// spaces handler owns the /spaces/{spaceId} mount point.
func (h *Handler) Mount(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(h.auth.Middleware, h.spaces.Middleware)
		const p = "/spaces/{spaceId}/transfers"
		r.Post(p, h.create)
		r.Patch(p+"/{id}", h.update)
		r.Delete(p+"/{id}", h.delete)
	})
}

// --- request contract --------------------------------------------------------

// Endpoint types of a transfer side.
const (
	endpointWallet     = "WALLET"
	endpointInvestment = "INVESTMENT"
)

// EndpointDoc documents one side of a transfer: `{type:"WALLET", walletId}` or,
// for a BUY or SELL, `{type:"INVESTMENT", investmentId, quantity, unitPrice, fee?}`.
type EndpointDoc struct {
	Type         string     `json:"type" enums:"WALLET,INVESTMENT"`
	WalletID     *uuid.UUID `json:"walletId,omitempty"`
	InvestmentID *uuid.UUID `json:"investmentId,omitempty"`
	Quantity     string     `json:"quantity,omitempty" example:"0.5"`
	UnitPrice    string     `json:"unitPrice,omitempty" example:"60000"`
	Fee          string     `json:"fee,omitempty" example:"10"`
}

type walletEndpoint struct {
	Type     string    `json:"type"`
	WalletID uuid.UUID `json:"walletId" validate:"required"`
}

// investmentEndpoint is the investment side of a BUY or SELL (all of it required on create).
type investmentEndpoint struct {
	Type         string           `json:"type"`
	InvestmentID uuid.UUID        `json:"investmentId" validate:"required"`
	Quantity     *decimal.Decimal `json:"quantity" validate:"required"`
	UnitPrice    *decimal.Decimal `json:"unitPrice" validate:"required"`
	Fee          *decimal.Decimal `json:"fee"`
}

// investmentPatchEndpoint is the same side in an edit: everything but the type is optional.
type investmentPatchEndpoint struct {
	Type         string           `json:"type"`
	InvestmentID *uuid.UUID       `json:"investmentId"`
	Quantity     *decimal.Decimal `json:"quantity"`
	UnitPrice    *decimal.Decimal `json:"unitPrice"`
	Fee          *decimal.Decimal `json:"fee"`
}

// endpoint is a decoded side of a transfer.
type endpoint interface{ side() side }

// side is one decoded end of a transfer: a wallet or an investment leg.
type side struct {
	// investment is true for an INVESTMENT side even if it carries no fields (an edit may send just the type).
	investment   bool
	wallet       uuid.UUID
	investmentID *uuid.UUID
	quantity     *decimal.Decimal
	unitPrice    *decimal.Decimal
	fee          *decimal.Decimal
}

func (e *walletEndpoint) side() side { return side{wallet: e.WalletID} }
func (e *investmentEndpoint) side() side {
	id := e.InvestmentID
	return side{investment: true, investmentID: &id, quantity: e.Quantity, unitPrice: e.UnitPrice, fee: e.Fee}
}
func (e *investmentPatchEndpoint) side() side {
	return side{investment: true, investmentID: e.InvestmentID, quantity: e.Quantity, unitPrice: e.UnitPrice, fee: e.Fee}
}

func (s side) isWallet() bool { return !s.investment }

// decodeSide decodes `from`/`to` with the discriminated-decode helper. patch
// selects the lenient investment shape used by edits.
func decodeSide(field string, raw json.RawMessage, patch bool) (side, error) {
	if isAbsent(raw) {
		return side{}, apperror.Validation("Validation failed", map[string]any{field: "is required"})
	}
	invVariant := func() endpoint { return &investmentEndpoint{} }
	if patch {
		invVariant = func() endpoint { return &investmentPatchEndpoint{} }
	}
	e, err := httpx.DecodeDiscriminatedBytes(raw, "type", map[string]func() endpoint{
		endpointWallet:     func() endpoint { return &walletEndpoint{} },
		endpointInvestment: invVariant,
	})
	if err != nil {
		if ae, ok := apperror.As(err); ok && ae.Code == apperror.CodeValidationFailed {
			f := map[string]any{}
			if len(ae.Details) == 0 {
				f[field] = ae.Message
			}
			for k, v := range ae.Details {
				f[field+"."+k] = v
			}
			return side{}, apperror.Validation("Validation failed", f)
		}
		return side{}, err
	}
	return e.side(), nil
}

type createRequest struct {
	From json.RawMessage `json:"from" swaggertype:"object"`
	To   json.RawMessage `json:"to" swaggertype:"object"`
	// Amount is required between two wallets. For a BUY or SELL it is the wallet side
	// (debited / credited) and optional: it defaults to the trade value, converted
	// to the wallet's currency when needed.
	Amount     *decimal.Decimal `json:"amount" swaggertype:"string" example:"100.00"`
	ToAmount   *decimal.Decimal `json:"toAmount" swaggertype:"string" example:"1600000"`
	OccurredAt *time.Time       `json:"occurredAt"`
	Note       *string          `json:"note" validate:"omitempty,max=1000"`
}

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
	From       json.RawMessage  `json:"from" swaggertype:"object"`
	To         json.RawMessage  `json:"to" swaggertype:"object"`
	Amount     *decimal.Decimal `json:"amount" swaggertype:"string"`
	ToAmount   *decimal.Decimal `json:"toAmount" swaggertype:"string"`
	OccurredAt *time.Time       `json:"occurredAt"`
	Note       optional[string] `json:"note" swaggertype:"string"`
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

func isAbsent(raw json.RawMessage) bool {
	t := strings.TrimSpace(string(raw))
	return t == "" || t == "null"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func transferID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, errTransferNF
	}
	return id, nil
}

// --- handlers ----------------------------------------------------------------

// create godoc
//
//	@Summary		Transfer between wallets, or buy/sell an investment through a wallet
//	@Description	`from` and `to` are `{type:"WALLET", walletId}` or `{type:"INVESTMENT", investmentId, quantity, unitPrice, fee?}`.\n\n**Wallet → wallet**: `toAmount` is required when the wallets use different currencies (the stored rate is toAmount / amount); for the same currency it must equal `amount` or be omitted. Negative balances are allowed.\n\n**BUY** (wallet → investment): the wallet is debited quantity × unitPrice + fee, converted to the wallet's currency unless `amount` is given; the position becomes new_qty = q + Δq and new_avg = (q × avg + Δq × unitPrice) / new_qty, fees += fee.\n\n**SELL** (investment → wallet): the wallet is credited quantity × unitPrice − fee; the average stays; `realizedPl` = (unitPrice − avg) × Δq − fee. Selling more than the quantity answers 422 INSUFFICIENT_QUANTITY.\n\nInvestment → investment is not supported. Transfers and trades never count as income or expense; they show in the `transfer` column of the daily summary and in the history of the wallet.
//	@Tags			transfers
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			spaceId	path		string			true	"Space ID"
//	@Param			body	body		createRequest	true	"Transfer, BUY or SELL"
//	@Success		201		{object}	transactions.TransactionDTO
//	@Failure		400		{object}	httpx.ErrorEnvelope
//	@Failure		404		{object}	httpx.ErrorEnvelope	"space, wallet or investment not found"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"TRANSFER_SAME_WALLET / TRANSFER_AMOUNT_REQUIRED / NOT_SUPPORTED_YET / INSUFFICIENT_QUANTITY / FX_RATE_UNAVAILABLE"
//	@Failure	403	{object}	httpx.ErrorEnvelope	"OWNER_ONLY (BUY/SELL on an investment shared into this space)"
//	@Router			/spaces/{spaceId}/transfers [post]
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	spaceID := spaces.SpaceIDFromContext(r.Context())
	req, err := httpx.DecodeAndValidate[createRequest](r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	f := map[string]any{}
	checkAmount(f, "amount", req.Amount)
	checkAmount(f, "toAmount", req.ToAmount)
	sides := map[string]side{}
	for field, raw := range map[string]json.RawMessage{"from": req.From, "to": req.To} {
		sd, err := decodeSide(field, raw, false)
		if err != nil {
			ae, _ := apperror.As(err)
			for k, v := range ae.Details {
				f[k] = v
			}
			continue
		}
		sides[field] = sd
	}
	if len(f) > 0 {
		httpx.WriteError(w, r, apperror.Validation("Validation failed", f))
		return
	}
	from, to := sides["from"], sides["to"]

	var d transactions.Detail
	switch {
	case from.investment && to.investment:
		err = apperror.Unprocessable(CodeNotSupportedYet, "Moving between two investments is not supported")
	case from.investment || to.investment:
		d, err = h.trade(r, user.ID, spaceID, req, from, to)
	default:
		if req.Amount == nil {
			err = apperror.Validation("Validation failed", map[string]any{"amount": "is required"})
			break
		}
		d, err = h.svc.Create(r.Context(), spaceID, user.ID, Input{
			FromWalletID: from.wallet, ToWalletID: to.wallet, Amount: *req.Amount,
			ToAmount: req.ToAmount, OccurredAt: req.OccurredAt, Note: req.Note,
		})
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, transactions.ToDTO(d))
}

// trade handles a wallet ↔ investment create: a BUY (wallet → investment) or SELL (investment → wallet).
func (h *Handler) trade(r *http.Request, userID, spaceID uuid.UUID, req createRequest, from, to side) (transactions.Detail, error) {
	if req.ToAmount != nil {
		return transactions.Detail{}, apperror.Validation("Validation failed", map[string]any{"toAmount": "is not used for investment trades; use amount for the wallet side"})
	}
	in := investments.TradeInput{Amount: req.Amount, OccurredAt: req.OccurredAt, Note: req.Note}
	var leg side
	if !from.investment { // wallet → investment
		in.Action, in.WalletID, leg = ledger.ActionBuy, from.wallet, to
	} else { // investment → wallet
		in.Action, in.WalletID, leg = ledger.ActionSell, to.wallet, from
	}
	in.InvestmentID = *leg.investmentID
	in.Leg = investments.Leg{Quantity: *leg.quantity, UnitPrice: *leg.unitPrice}
	if leg.fee != nil {
		in.Leg.Fee = *leg.fee
	}
	return h.svc.Trade(r.Context(), spaceID, userID, in)
}

// update godoc
//
//	@Summary		Edit a transfer, BUY or SELL
//	@Description	Any field may be sent; the old effect is reversed and the new one applied atomically. For wallet transfers, changing the amount or either wallet of a cross-currency transfer requires `toAmount`. For a BUY/SELL, send the investment side as `{type:"INVESTMENT", quantity?, unitPrice?, fee?}` and the wallet side as `{type:"WALLET", walletId}`; the investment itself cannot change. A BUY/SELL can only be edited while the investment's position is still the one it left behind, otherwise 409 POSITION_CHANGED_SINCE (deleting is more permissive, see DELETE).
//	@Tags			transfers
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			spaceId	path		string			true	"Space ID"
//	@Param			id		path		string			true	"Transfer ID"
//	@Param			body	body		updateRequest	true	"Fields to change"
//	@Success		200		{object}	transactions.TransactionDTO
//	@Failure		400		{object}	httpx.ErrorEnvelope
//	@Failure		404		{object}	httpx.ErrorEnvelope
//	@Failure		409		{object}	httpx.ErrorEnvelope	"POSITION_CHANGED_SINCE"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"TRANSFER_SAME_WALLET / TRANSFER_AMOUNT_REQUIRED / NOT_SUPPORTED_YET / INSUFFICIENT_QUANTITY"
//	@Failure	403	{object}	httpx.ErrorEnvelope	"OWNER_ONLY (BUY/SELL on an investment shared into this space)"
//	@Router			/spaces/{spaceId}/transfers/{id} [patch]
func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	id, err := transferID(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	req, err := httpx.DecodeAndValidate[updateRequest](r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	p := Patch{Amount: req.Amount, ToAmount: req.ToAmount, OccurredAt: req.OccurredAt, Note: Optional[string](req.Note)}
	f := map[string]any{}
	checkAmount(f, "amount", req.Amount)
	checkAmount(f, "toAmount", req.ToAmount)
	if req.Note.Value != nil && len(*req.Note.Value) > 1000 {
		f["note"] = "must be at most 1000 characters"
	}
	for field, raw := range map[string]json.RawMessage{"from": req.From, "to": req.To} {
		if isAbsent(raw) {
			continue
		}
		sd, err := decodeSide(field, raw, true)
		if err != nil {
			ae, _ := apperror.As(err)
			for k, v := range ae.Details {
				f[k] = v
			}
			continue
		}
		if sd.investment {
			p.Investment = &InvestmentPatch{InvestmentID: sd.investmentID, Quantity: sd.quantity, UnitPrice: sd.unitPrice, Fee: sd.fee}
			checkAmount(f, field+".quantity", sd.quantity)
			checkAmount(f, field+".unitPrice", sd.unitPrice)
			if sd.fee != nil && sd.fee.IsNegative() {
				f[field+".fee"] = "must not be negative"
			}
			continue
		}
		wid := sd.wallet
		if field == "from" {
			p.FromWalletID = &wid
		} else {
			p.ToWalletID = &wid
		}
	}
	if len(f) > 0 {
		httpx.WriteError(w, r, apperror.Validation("Validation failed", f))
		return
	}
	user, _ := auth.UserFromContext(r.Context())
	d, err := h.svc.Update(r.Context(), spaces.SpaceIDFromContext(r.Context()), user.ID, id, p)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, transactions.ToDTO(d))
}

// delete godoc
//
//	@Summary		Delete a transfer, BUY or SELL (balances and position are restored)
//	@Description	Any BUY/SELL can be deleted, not only the newest: the trades recorded after it are replayed on top of the position it started from, and their position snapshots and realised profit are rewritten in the same database transaction (each later trade keeps its own wallet amounts, the deleted trade's wallet effect is reversed). Refused with 422 TRADE_DELETE_BREAKS_LATER_SELL (details.tradeId names the first later sale that would no longer have enough quantity; nothing changes) and with 409 POSITION_CHANGED_SINCE when the position was edited by hand after the trade. A second delete of the same id is 404. Net worth snapshots of past days are not rewritten. See GET /spaces/{spaceId}/investments/{id}/trades (canDelete, blockedReason).
//	@Tags		transfers
//	@Security	BearerAuth
//	@Param		spaceId	path	string	true	"Space ID"
//	@Param		id		path	string	true	"Transfer ID"
//	@Success	204
//	@Failure	404	{object}	httpx.ErrorEnvelope
//	@Failure	409	{object}	httpx.ErrorEnvelope	"POSITION_CHANGED_SINCE"
//	@Failure	422	{object}	httpx.ErrorEnvelope	"TRADE_DELETE_BREAKS_LATER_SELL"
//	@Failure	403	{object}	httpx.ErrorEnvelope	"OWNER_ONLY (BUY/SELL on an investment shared into this space)"
//	@Router		/spaces/{spaceId}/transfers/{id} [delete]
func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	id, err := transferID(r)
	if err == nil {
		user, _ := auth.UserFromContext(r.Context())
		err = h.svc.Delete(r.Context(), spaces.SpaceIDFromContext(r.Context()), user.ID, id)
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
