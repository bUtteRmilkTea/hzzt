package investments

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/JonathanRainee/hzzt/internal/auth"
	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
	"github.com/JonathanRainee/hzzt/internal/platform/apperror"
	"github.com/JonathanRainee/hzzt/internal/platform/httpx"
	"github.com/JonathanRainee/hzzt/internal/platform/money"
	"github.com/JonathanRainee/hzzt/internal/spaces"
)

// Limits are the per-endpoint rate limiters.
type Limits struct {
	RefreshPrice *httpx.RateLimiter // per investment
}

// DefaultLimits: on-demand price refresh 1/min per investment.
func DefaultLimits() Limits {
	return Limits{RefreshPrice: httpx.NewRateLimiter(1, time.Minute)}
}

// Handler exposes the investment endpoints.
type Handler struct {
	svc    *Service
	auth   *auth.Service
	spaces *spaces.Service
	limits Limits
}

// NewHandler creates the HTTP layer.
func NewHandler(svc *Service, authSvc *auth.Service, spacesSvc *spaces.Service, limits Limits) *Handler {
	return &Handler{svc: svc, auth: authSvc, spaces: spacesSvc, limits: limits}
}

// Mount registers the routes on r (the /api/v1 router). Any space member may
// manage investments. Space routes carry the full path because the spaces
// handler owns the /spaces/{spaceId} mount point.
func (h *Handler) Mount(r chi.Router) {
	r.With(h.auth.Middleware).Get("/investment-types", h.types)

	r.Group(func(r chi.Router) {
		r.Use(h.auth.Middleware, h.spaces.Middleware)
		const p = "/spaces/{spaceId}/investments"
		r.Get(p, h.list)
		r.Get(p+"/grouped", h.grouped)
		r.Get(p+"/wrappers", h.wrappers)
		r.Patch(p+"/wrappers/{assetType}/{wrapperKey}", h.setWrapperTarget)
		r.Post(p, h.create)
		r.Get(p+"/{id}", h.get)
		r.Patch(p+"/{id}", h.update)
		r.Delete(p+"/{id}", h.delete)
		r.Post(p+"/{id}/refresh-price", h.refreshPrice)
		r.Get(p+"/{id}/participants", h.participants)
		r.Put(p+"/{id}/participants", h.setParticipants)
		r.Delete(p+"/{id}/participants", h.stopSharing)
		r.Get(p+"/{id}/share-changes", h.shareChanges)
	})

	// Full paths, not r.Route("/me"): the users handler already owns that mount point.
	r.Group(func(r chi.Router) {
		r.Use(h.auth.Middleware)
		r.Get("/me/shared-investment-invitations", h.invitations)
		r.Post("/me/shared-investment-invitations/{participantId}/accept", h.acceptInvitation)
		r.Post("/me/shared-investment-invitations/{participantId}/decline", h.declineInvitation)
		r.Post("/me/shared-investments/{participantId}/leave", h.leave)
	})
}

// InvestmentDTO is the public representation of an investment with its valuation.
type InvestmentDTO struct {
	ID                       uuid.UUID        `json:"id"`
	AssetType                string           `json:"assetType" enums:"CRYPTO,ID_STOCK,US_STOCK,MUTUAL_FUND,GLOBAL_ETF"`
	PriceMode                string           `json:"priceMode" enums:"AUTO,MANUAL"`
	Name                     string           `json:"name"`
	Symbol                   *string          `json:"symbol"`
	Icon                     string           `json:"icon"`
	QuoteCurrency            string           `json:"quoteCurrency" example:"IDR"`
	Quantity                 decimal.Decimal  `json:"quantity" swaggertype:"string" example:"200"`
	AvgBuyPrice              decimal.Decimal  `json:"avgBuyPrice" swaggertype:"string"`
	ManualPrice              decimal.Decimal  `json:"manualPrice" swaggertype:"string"`
	Fees                     decimal.Decimal  `json:"fees" swaggertype:"string"`
	ManualCurrentValue       *decimal.Decimal `json:"manualCurrentValue" swaggertype:"string"`
	PurchaseDate             *string          `json:"purchaseDate" example:"2026-01-31"`
	EstimatedAnnualChangePct decimal.Decimal  `json:"estimatedAnnualChangePct" swaggertype:"string"`
	ManualFxRate             *decimal.Decimal `json:"manualFxRate" swaggertype:"string"`
	Details                  any              `json:"details" swaggertype:"object"`
	// Wrapper is the exchange, broker or platform the position is held on; null when unknown.
	Wrapper      *string    `json:"wrapper" example:"Binance"`
	InstrumentID *uuid.UUID `json:"instrumentId"`
	IsShared     bool       `json:"isShared"`
	// PctOfTotal is this investment's share of the portfolio's base-currency value (only in the list response; null without a base value).
	PctOfTotal *money.Pct `json:"pctOfTotal"`
	// Valuation is the viewer's part: the whole investment unless it is shared.
	Valuation Valuation `json:"valuation"`
	// TotalValuation is the whole investment's valuation; present only for shared investments.
	TotalValuation *Valuation `json:"totalValuation"`
	// Ownership is OWNER for this space's own investments and SHARED_IN for ones
	// others shared into it (read-only).
	Ownership    string           `json:"ownership" enums:"OWNER,SHARED_IN"`
	MyShareBps   int              `json:"myShareBps" example:"3000"`
	Participants []ParticipantDTO `json:"participants"`
	RealizedPL   RealizedPLDTO    `json:"realizedPl"`
	CreatedAt    time.Time        `json:"createdAt"`
	UpdatedAt    time.Time        `json:"updatedAt"`
}

// ParticipantDTO is a person a shared investment is shared with. Other people's
// email addresses are masked unless the viewer is the owner (GET .../participants).
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

// RealizedPLDTO is the total realized profit/loss of an investment's SELL transactions.
type RealizedPLDTO struct {
	// Amount is in the investment's quote currency, Base in the space's base currency (null without an FX rate).
	Amount   money.Amount  `json:"amount" swaggertype:"string" example:"5990.00"`
	Currency string        `json:"currency" example:"USD"`
	Base     *money.Amount `json:"base" swaggertype:"string"`
	Sells    int           `json:"sells"`
}

func toDTO(it Item) InvestmentDTO {
	i := it.Investment
	d := InvestmentDTO{
		ID: i.ID, AssetType: i.AssetType, PriceMode: i.PriceMode, Name: i.Name, Symbol: i.Symbol, Icon: i.Icon,
		QuoteCurrency: strings.TrimSpace(i.QuoteCurrency), Quantity: i.Quantity, AvgBuyPrice: i.AvgBuyPrice,
		ManualPrice: i.ManualPrice, Fees: i.Fees, EstimatedAnnualChangePct: i.EstimatedAnnualChangePct,
		Details: it.Details, Wrapper: i.Wrapper, IsShared: i.IsShared, Valuation: it.Valuation, TotalValuation: it.TotalValuation,
		Ownership: it.Ownership, MyShareBps: it.MyShareBps, Participants: toParticipantDTOs(it.Participants),
		RealizedPL: RealizedPLDTO{
			Amount: money.NewAmount(it.Realized.Amount, i.QuoteCurrency), Currency: strings.TrimSpace(i.QuoteCurrency),
			Base: money.AmountPtr(it.Realized.Base, it.Realized.BaseCurrency), Sells: it.Realized.Sells,
		},
		CreatedAt: i.CreatedAt.UTC(), UpdatedAt: i.UpdatedAt.UTC(),
	}
	if i.ManualCurrentValue.Valid {
		v := i.ManualCurrentValue.Decimal
		d.ManualCurrentValue = &v
	}
	if i.ManualFxRate.Valid {
		v := i.ManualFxRate.Decimal
		d.ManualFxRate = &v
	}
	if i.PurchaseDate != nil {
		s := i.PurchaseDate.Format("2006-01-02")
		d.PurchaseDate = &s
	}
	if i.InstrumentID.Valid {
		id := i.InstrumentID.UUID
		d.InstrumentID = &id
	}
	return d
}

// ListResponse is the portfolio with a base-currency summary.
type ListResponse struct {
	Items   []InvestmentDTO `json:"items"`
	Summary Summary         `json:"summary"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func investmentID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, errInvestmentNF
	}
	return id, nil
}

// types godoc
//
//	@Summary		Investment type metadata for dynamic forms
//	@Description	Per asset type: the fixed or free quote currency, whether automatic pricing is supported, how quantity is expressed (ID_STOCK uses lots; quantity = lots × 100) and the fields of `details`.
//	@Tags			investments
//	@Security		BearerAuth
//	@Produce		json
//	@Success		200	{array}	TypeMeta
//	@Router			/investment-types [get]
func (h *Handler) types(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, Metas)
}

// list godoc
//
//	@Summary		List investments with valuations and a portfolio summary
//	@Description	Values are the viewer's: for a shared investment `valuation` is my share (base-currency parts add up exactly across all participants) and `totalValuation` the whole. `summary` totals the base-currency values. Investments without a usable FX rate are left out of the totals and reported in `summary.warnings`. `unrealizedPlPct` is a percentage (12.5 = +12.5%).
//	@Tags			investments
//	@Security		BearerAuth
//	@Produce		json
//	@Param			spaceId		path		string	true	"Space ID"
//	@Param			assetType	query		string	false	"CRYPTO, ID_STOCK, US_STOCK, MUTUAL_FUND or GLOBAL_ETF"
//	@Param			include		query		string	false	"shared: also list investments other people shared into this space (read-only, ownership SHARED_IN, valued for my share)"
//	@Success		200			{object}	ListResponse
//	@Failure		400			{object}	httpx.ErrorEnvelope
//	@Failure		404			{object}	httpx.ErrorEnvelope
//	@Router			/spaces/{spaceId}/investments [get]
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	at := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("assetType")))
	if at != "" {
		if _, ok := MetaFor(at); !ok {
			httpx.WriteError(w, r, apperror.Validation("Validation failed", map[string]any{
				"assetType": "must be one of: CRYPTO ID_STOCK US_STOCK MUTUAL_FUND GLOBAL_ETF"}))
			return
		}
	}
	includeShared, err := parseInclude(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	user, _ := auth.UserFromContext(r.Context())
	res, err := h.svc.List(r.Context(), spaces.SpaceIDFromContext(r.Context()), at, includeShared, user.ID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := ListResponse{Items: make([]InvestmentDTO, 0, len(res.Items)), Summary: res.Summary}
	pcts := ItemPcts(res.Items)
	for i, it := range res.Items {
		d := toDTO(it)
		d.PctOfTotal = money.PctPtr(pcts[i])
		out.Items = append(out.Items, d)
	}
	writeJSON(w, http.StatusOK, out)
}

// parseInclude reads ?include=shared.
func parseInclude(r *http.Request) (bool, error) {
	includeShared := false
	for _, inc := range strings.Split(r.URL.Query().Get("include"), ",") {
		switch strings.ToLower(strings.TrimSpace(inc)) {
		case "":
		case "shared":
			includeShared = true
		default:
			return false, apperror.Validation("Validation failed", map[string]any{"include": "must be: shared"})
		}
	}
	return includeShared, nil
}

// CreateRequestDoc documents the create body; the real decoding is keyed on assetType.
type CreateRequestDoc struct {
	AssetType string `json:"assetType" enums:"CRYPTO,ID_STOCK,US_STOCK,MUTUAL_FUND,GLOBAL_ETF"`
	Name      string `json:"name" example:"Bank Central Asia"`
	base
	Quantity *decimal.Decimal `json:"quantity,omitempty" swaggertype:"string" example:"0.5"`
	Details  map[string]any   `json:"details" swaggertype:"object"`
}

// create godoc
//
//	@Summary		Create an investment
//	@Description	The body is validated by `assetType` (see GET /investment-types). ID_STOCK takes `details.lots` and derives quantity = lots × 100. CRYPTO checks `details.stakedQuantity` <= quantity. `priceMode: AUTO` is accepted for CRYPTO, US_STOCK and GLOBAL_ETF and answers 422 AUTO_PRICE_NOT_SUPPORTED otherwise. A CRYPTO created without `priceMode` is AUTO by default and linked to its CoinMarketCap coin by symbol; if the symbol is unknown or ambiguous the holding is created as MANUAL and `valuation.warnings` carries `PRICE_NOT_LINKED` (an explicit `priceMode` never falls back). Valuation warnings also include `PRICE_STALE` (a stale market price still counts and is flagged) and `MANUAL_VALUE_OVERRIDES_MARKET` (an AUTO holding with a manual current value, which wins). An AUTO investment is linked to a market instrument: send the `instrumentId` picked from GET /market/crypto/search (CRYPTO) or GET /market/stocks/search (US_STOCK, GLOBAL_ETF), or the symbol is resolved to the highest-ranked coin / the exact ticker (422 INSTRUMENT_NOT_FOUND if none matches). Without a market price the manual price is used. Optional `funding: {walletId, amount?}` pays for the initial position from a wallet: it creates a BUY titled "Initial position {symbol}" (wallet debited quantity × avgBuyPrice + fees, converted to the wallet's currency unless `amount` is given) in the same database transaction.
//	@Tags			investments
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			spaceId	path		string				true	"Space ID"
//	@Param			body	body		CreateRequestDoc	true	"Investment"
//	@Success		201		{object}	InvestmentDTO
//	@Failure		400		{object}	httpx.ErrorEnvelope
//	@Failure		404		{object}	httpx.ErrorEnvelope
//	@Failure		422		{object}	httpx.ErrorEnvelope	"AUTO_PRICE_NOT_SUPPORTED"
//	@Router			/spaces/{spaceId}/investments [post]
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	sp, _ := spaces.SpaceFromContext(r.Context())
	in, err := httpx.DecodeDiscriminated(r, "assetType", Variants())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	spec, err := in.spec(strings.TrimSpace(sp.BaseCurrency))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	it, err := h.svc.Create(r.Context(), sp.ID, user.ID, spec)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toDTO(it))
}

// get godoc
//
//	@Summary		Get an investment with its valuation
//	@Description	Also opens investments shared into this space by someone else: the response is then `ownership: SHARED_IN`, valued with the caller's share and the space's base currency, with other participants' emails masked.
//	@Tags			investments
//	@Security	BearerAuth
//	@Produce	json
//	@Param		spaceId	path		string	true	"Space ID"
//	@Param		id		path		string	true	"Investment ID"
//	@Success	200		{object}	InvestmentDTO
//	@Failure	404		{object}	httpx.ErrorEnvelope
//	@Router		/spaces/{spaceId}/investments/{id} [get]
func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := investmentID(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	user, _ := auth.UserFromContext(r.Context())
	it, err := h.svc.Get(r.Context(), spaces.SpaceIDFromContext(r.Context()), user.ID, id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toDTO(it))
}

// update godoc
//
//	@Summary		Update an investment
//	@Description	`assetType` is immutable (it may be sent only if unchanged). `details`, when sent, replaces the whole details object. `symbol`, `manualCurrentValue`, `purchaseDate` and `manualFxRate` accept null to clear.
//	@Tags			investments
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			spaceId	path		string		true	"Space ID"
//	@Param			id		path		string		true	"Investment ID"
//	@Param			body	body		PatchInput	true	"Fields to change"
//	@Success		200		{object}	InvestmentDTO
//	@Failure		400		{object}	httpx.ErrorEnvelope
//	@Failure		404		{object}	httpx.ErrorEnvelope
//	@Failure		422		{object}	httpx.ErrorEnvelope	"AUTO_PRICE_NOT_SUPPORTED"
//	@Failure	403	{object}	httpx.ErrorEnvelope	"OWNER_ONLY (investment shared into this space)"
//	@Router			/spaces/{spaceId}/investments/{id} [patch]
func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	sp, _ := spaces.SpaceFromContext(r.Context())
	id, err := investmentID(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	p, err := httpx.DecodeAndValidate[PatchInput](r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	user, _ := auth.UserFromContext(r.Context())
	it, err := h.svc.Update(r.Context(), sp, user.ID, id, p)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toDTO(it))
}

// delete godoc
//
//	@Summary	Delete an investment (soft delete)
//	@Tags		investments
//	@Security	BearerAuth
//	@Param		spaceId	path	string	true	"Space ID"
//	@Param		id		path	string	true	"Investment ID"
//	@Success	204
//	@Failure	404	{object}	httpx.ErrorEnvelope
//	@Failure	403	{object}	httpx.ErrorEnvelope	"OWNER_ONLY (investment shared into this space)"
//	@Router		/spaces/{spaceId}/investments/{id} [delete]
func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	id, err := investmentID(r)
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

// refreshPrice godoc
//
//	@Summary		Refresh the market price of an AUTO investment now
//	@Description	Rate limited to 1 per minute per investment. On a provider failure the last known price is kept (it turns stale).
//	@Tags			investments
//	@Security		BearerAuth
//	@Produce		json
//	@Param			spaceId	path		string	true	"Space ID"
//	@Param			id		path		string	true	"Investment ID"
//	@Success		200		{object}	InvestmentDTO
//	@Failure		404		{object}	httpx.ErrorEnvelope
//	@Failure		422		{object}	httpx.ErrorEnvelope	"AUTO_PRICE_NOT_SUPPORTED / PRICE_MODE_NOT_AUTO / INSTRUMENT_NOT_LINKED"
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		502		{object}	httpx.ErrorEnvelope	"UPSTREAM_PROVIDER_ERROR"
//	@Failure	403	{object}	httpx.ErrorEnvelope	"OWNER_ONLY (investment shared into this space)"
//	@Router			/spaces/{spaceId}/investments/{id}/refresh-price [post]
func (h *Handler) refreshPrice(w http.ResponseWriter, r *http.Request) {
	id, err := investmentID(r)
	if err == nil {
		// keyed by space too, so a foreign id in your own space cannot use up someone else's budget
		err = h.limits.RefreshPrice.Check(w, spaces.SpaceIDFromContext(r.Context()).String()+"/"+id.String())
	}
	var it Item
	if err == nil {
		user, _ := auth.UserFromContext(r.Context())
		it, err = h.svc.RefreshPrice(r.Context(), spaces.SpaceIDFromContext(r.Context()), user.ID, id)
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toDTO(it))
}

// --- sharing ---------------------------------------------------------------------------

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

// ParticipantsResponse lists the people an investment is shared with.
type ParticipantsResponse struct {
	Items []ParticipantDTO `json:"items"`
}

// SetParticipantsResponse is the new participant list plus the investment as it is now, so the
// client can update in place (isShared, participants, shared valuation) without refetching.
type SetParticipantsResponse struct {
	Items      []ParticipantDTO `json:"items"`
	Investment InvestmentDTO    `json:"investment"`
}

// participants godoc
//
//	@Summary	List who an investment is shared with (owner only)
//	@Tags		sharing
//	@Security	BearerAuth
//	@Produce	json
//	@Param		spaceId	path		string	true	"Space ID"
//	@Param		id		path		string	true	"Investment ID"
//	@Success	200		{object}	ParticipantsResponse
//	@Failure	403		{object}	httpx.ErrorEnvelope	"OWNER_ONLY"
//	@Failure	404		{object}	httpx.ErrorEnvelope
//	@Router		/spaces/{spaceId}/investments/{id}/participants [get]
func (h *Handler) participants(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	id, err := investmentID(r)
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

// setParticipants godoc
//
//	@Summary		Share an investment: replace the participant list (owner only)
//	@Description	Full replacement. Include yourself (`isOwner: true`, or your own email) with a share above 0. Shares are percentages with up to 2 decimals and the shares of all participants must add up to exactly 100.00 (422 SHARE_TOTAL_INVALID); at most 10 people (422 PARTICIPANT_LIMIT). A participant with an `email` becomes INVITED, one with only a `displayName` is ACTIVE right away (someone without an account). The response is identical whether or not an email belongs to an account. Entries left out of the list are removed. Shares count whatever the invitation status. Only the owner may edit a shared investment, its prices, trades and the participant list (403 OWNER_ONLY for other members of the space).
//	@Tags			sharing
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			spaceId	path		string					true	"Space ID"
//	@Param			id		path		string					true	"Investment ID"
//	@Param			body	body		SetParticipantsRequest	true	"Participants"
//	@Success		200		{object}	SetParticipantsResponse
//	@Failure		400		{object}	httpx.ErrorEnvelope
//	@Failure		403		{object}	httpx.ErrorEnvelope	"OWNER_ONLY"
//	@Failure		404		{object}	httpx.ErrorEnvelope
//	@Failure		422		{object}	httpx.ErrorEnvelope	"SHARE_TOTAL_INVALID / PARTICIPANT_LIMIT"
//	@Router			/spaces/{spaceId}/investments/{id}/participants [put]
func (h *Handler) setParticipants(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	id, err := investmentID(r)
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
	var it Item
	if err == nil {
		it, err = h.svc.Get(r.Context(), spaceID, user.ID, id)
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, SetParticipantsResponse{Items: toParticipantDTOs(ps), Investment: toDTO(it)})
}

// stopSharing godoc
//
//	@Summary		Stop sharing: 100% returns to the owner and everyone else is removed (owner only)
//	@Description	Answers 204. With `?return=investment` it answers 200 with the investment as it is now, so the client can update in place.
//	@Tags			sharing
//	@Security		BearerAuth
//	@Produce		json
//	@Param			spaceId	path	string	true	"Space ID"
//	@Param			id		path	string	true	"Investment ID"
//	@Param			return	query	string	false	"investment: answer 200 with the investment instead of 204"
//	@Success		204
//	@Success		200	{object}	InvestmentDTO
//	@Failure	403	{object}	httpx.ErrorEnvelope	"OWNER_ONLY"
//	@Failure	404	{object}	httpx.ErrorEnvelope
//	@Router		/spaces/{spaceId}/investments/{id}/participants [delete]
func (h *Handler) stopSharing(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	spaceID := spaces.SpaceIDFromContext(r.Context())
	id, err := investmentID(r)
	ret := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("return")))
	if err == nil && ret != "" && ret != "investment" {
		err = apperror.Validation("Validation failed", map[string]any{"return": "must be: investment"})
	}
	if err == nil {
		err = h.svc.StopSharing(r.Context(), spaceID, id, user)
	}
	var it Item
	if err == nil && ret == "investment" {
		it, err = h.svc.Get(r.Context(), spaceID, user.ID, id)
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if ret == "investment" {
		writeJSON(w, http.StatusOK, toDTO(it))
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
//	@Summary	Audit trail of an investment's sharing arrangement, newest first (owner only)
//	@Tags		sharing
//	@Security	BearerAuth
//	@Produce	json
//	@Param		spaceId	path		string	true	"Space ID"
//	@Param		id		path		string	true	"Investment ID"
//	@Success	200		{array}		ShareChangeDTO
//	@Failure	403		{object}	httpx.ErrorEnvelope	"OWNER_ONLY"
//	@Failure	404		{object}	httpx.ErrorEnvelope
//	@Router		/spaces/{spaceId}/investments/{id}/share-changes [get]
func (h *Handler) shareChanges(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	id, err := investmentID(r)
	var rows []sqlc.InvestmentShareChange
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

// InvitationDTO is a pending invitation to co-own an investment.
type InvitationDTO struct {
	ParticipantID uuid.UUID `json:"participantId"`
	OwnerName     string    `json:"ownerName"`
	Investment    struct {
		Name      string  `json:"name"`
		Symbol    *string `json:"symbol"`
		Icon      string  `json:"icon"`
		AssetType string  `json:"assetType"`
	} `json:"investment"`
	SharePct  string    `json:"sharePct" example:"30.00"`
	ShareBps  int       `json:"shareBps"`
	InvitedAt time.Time `json:"invitedAt"`
}

// invitations godoc
//
//	@Summary		My pending shared-investment invitations
//	@Description	Invitations addressed to my account or to my account's email (case-insensitive), including ones sent before I registered. Shows the investment and my share, never the owner's space.
//	@Tags			sharing
//	@Security		BearerAuth
//	@Produce		json
//	@Success		200	{array}	InvitationDTO
//	@Router			/me/shared-investment-invitations [get]
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
		d.Investment.Name, d.Investment.Symbol, d.Investment.Icon, d.Investment.AssetType = i.Name, i.Symbol, i.Icon, i.AssetType
		out = append(out, d)
	}
	writeJSON(w, http.StatusOK, out)
}

type acceptRequest struct {
	SpaceID uuid.UUID `json:"spaceId" validate:"required" swaggertype:"string"`
}

// AcceptResponse says where the accepted investment can be opened: GET /spaces/{spaceId}/investments/{investmentId}.
type AcceptResponse struct {
	SpaceID      uuid.UUID `json:"spaceId"`
	InvestmentID uuid.UUID `json:"investmentId"`
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
//	@Summary		Accept a shared-investment invitation
//	@Description	The investment is attached to the given space (you must be a member of it, and it must not be the owner's space); that space then counts my share and lists it with `include=shared`.
//	@Tags			sharing
//	@Security		BearerAuth
//	@Accept			json
//	@Param			participantId	path		string			true	"Invitation (participant) ID"
//	@Produce		json
//	@Param			body			body		acceptRequest	true	"Space to attach it to"
//	@Success		200				{object}	AcceptResponse
//	@Failure		400	{object}	httpx.ErrorEnvelope
//	@Failure		404	{object}	httpx.ErrorEnvelope
//	@Failure		409	{object}	httpx.ErrorEnvelope	"SHARE_INVITATION_ANSWERED"
//	@Router			/me/shared-investment-invitations/{participantId}/accept [post]
func (h *Handler) acceptInvitation(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	id, err := participantID(r)
	var req acceptRequest
	if err == nil {
		req, err = httpx.DecodeAndValidate[acceptRequest](r)
	}
	var invID uuid.UUID
	if err == nil {
		invID, err = h.svc.AcceptInvitation(r.Context(), user, id, req.SpaceID)
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, AcceptResponse{SpaceID: req.SpaceID, InvestmentID: invID})
}

// declineInvitation godoc
//
//	@Summary	Decline a shared-investment invitation (the share stays allocated until the owner changes it)
//	@Tags		sharing
//	@Security	BearerAuth
//	@Param		participantId	path	string	true	"Invitation (participant) ID"
//	@Success	204
//	@Failure	404	{object}	httpx.ErrorEnvelope
//	@Failure	409	{object}	httpx.ErrorEnvelope	"SHARE_INVITATION_ANSWERED"
//	@Router		/me/shared-investment-invitations/{participantId}/decline [post]
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
//	@Summary		Leave a shared investment
//	@Description	My share stays allocated but becomes a name-only participant; the owner finds the change in the audit trail.
//	@Tags			sharing
//	@Security		BearerAuth
//	@Param			participantId	path	string	true	"My participant ID"
//	@Success		204
//	@Failure		404	{object}	httpx.ErrorEnvelope
//	@Failure		422	{object}	httpx.ErrorEnvelope	"SHARE_OWNER_CANNOT_LEAVE"
//	@Router			/me/shared-investments/{participantId}/leave [post]
func (h *Handler) leave(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	id, err := participantID(r)
	if err == nil {
		err = h.svc.LeaveSharedInvestment(r.Context(), user, id)
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
