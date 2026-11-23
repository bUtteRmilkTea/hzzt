package networth

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/JonathanRainee/hzzt/internal/auth"
	"github.com/JonathanRainee/hzzt/internal/currency"
	"github.com/JonathanRainee/hzzt/internal/platform/httpx"
	"github.com/JonathanRainee/hzzt/internal/platform/money"
	"github.com/JonathanRainee/hzzt/internal/spaces"
)

// Handler exposes the net-worth endpoints.
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
		r.Get("/spaces/{spaceId}/net-worth/summary", h.summary)
		r.Get("/spaces/{spaceId}/net-worth/history", h.history)
		r.Get("/spaces/{spaceId}/assets/summary", h.assetSummary)
	})
}

// InvestmentsDTO is the investments section of the summary.
type InvestmentsDTO struct {
	Value           money.Amount `json:"value" swaggertype:"string"`
	Cost            money.Amount `json:"cost" swaggertype:"string"`
	UnrealizedPL    money.Amount `json:"unrealizedPl" swaggertype:"string"`
	UnrealizedPLPct *money.Pct   `json:"unrealizedPlPct" swaggertype:"string"` // a percentage; null when the cost is 0
}

// SummaryDTO is the net-worth summary in the space's base currency.
type SummaryDTO struct {
	Currency     string             `json:"currency" example:"USD"`
	TrackingMode string             `json:"trackingMode" enums:"FULL_NET_WORTH,CASH_ONLY"`
	AsOf         time.Time          `json:"asOf"`
	Cash         money.Amount       `json:"cash" swaggertype:"string"`
	Investments  *InvestmentsDTO    `json:"investments,omitempty"`
	FixedAssets  *money.Amount      `json:"fixedAssets,omitempty" swaggertype:"string"`
	Receivables  *money.Amount      `json:"receivables,omitempty" swaggertype:"string"`
	Liabilities  *money.Amount      `json:"liabilities,omitempty" swaggertype:"string"`
	NetWorth     money.Amount       `json:"netWorth" swaggertype:"string"`
	Warnings     []currency.Warning `json:"warnings"`
}

// ToDTO converts a summary.
func ToDTO(s Summary) SummaryDTO {
	d := SummaryDTO{
		Currency: s.Currency, TrackingMode: s.TrackingMode, AsOf: s.AsOf, Cash: money.NewAmount(s.Cash, s.Currency), NetWorth: money.NewAmount(s.NetWorth, s.Currency),
		FixedAssets: money.AmountPtr(s.FixedAssets, s.Currency), Receivables: money.AmountPtr(s.Receivables, s.Currency),
		Liabilities: money.AmountPtr(s.Liabilities, s.Currency), Warnings: s.Warnings,
	}
	if s.Investments != nil {
		d.Investments = &InvestmentsDTO{
			Value: money.NewAmount(s.Investments.Value, s.Currency), Cost: money.NewAmount(s.Investments.Cost, s.Currency),
			UnrealizedPL: money.NewAmount(s.Investments.UnrealizedPL, s.Currency), UnrealizedPLPct: money.PctPtr(s.Investments.UnrealizedPLPct),
		}
	}
	return d
}

// summary godoc
//
//	@Summary		Net-worth summary
//	@Description	netWorth = cash + investments (my share, including investments shared into this space) + fixedAssets + receivables − liabilities, all converted to the space base currency. Anything without a usable FX rate is left out and reported in `warnings`. In CASH_ONLY tracking mode only `cash` and `netWorth` (= cash) are returned.
//	@Tags			net-worth
//	@Security		BearerAuth
//	@Produce		json
//	@Param			spaceId	path		string	true	"Space ID"
//	@Success		200		{object}	SummaryDTO
//	@Failure		404		{object}	httpx.ErrorEnvelope
//	@Router			/spaces/{spaceId}/net-worth/summary [get]
func (h *Handler) summary(w http.ResponseWriter, r *http.Request) {
	s, err := h.svc.Compute(r.Context(), spaces.SpaceIDFromContext(r.Context()), nil)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(ToDTO(s))
}

// PointDTO is one day of the net-worth history.
type PointDTO struct {
	Date     string       `json:"date" example:"2026-11-18"`
	NetWorth money.Amount `json:"netWorth" swaggertype:"string"`
	// Investments is the portfolio value of the day (the investments line of the net worth).
	Investments money.Amount `json:"investments" swaggertype:"string"`
	Live        bool         `json:"live,omitempty"` // the point computed now for today
}

// HistoryDTO is the net-worth graph of a space.
type HistoryDTO struct {
	Currency  string     `json:"currency" example:"USD"`
	Range     string     `json:"range" enums:"1M,3M,6M,1Y,ALL"`
	Points    []PointDTO `json:"points"`
	ChangePct *money.Pct `json:"changePct" swaggertype:"string"` // first → last point, a percentage; null when it cannot be computed
	// InvestmentsChangePct is the same change for the portfolio value.
	InvestmentsChangePct *money.Pct `json:"investmentsChangePct" swaggertype:"string"`
}

// history godoc
//
//	@Summary		Net-worth history for a range
//	@Description	Daily snapshots (stored at 00:15 UTC, one per UTC date) in the space's current base currency, plus a live point for today, at most about 120 points (evenly downsampled, first and last kept). A space created today returns just the live point. `changePct` is the change from the first to the last point.
//	@Tags			net-worth
//	@Security		BearerAuth
//	@Produce		json
//	@Param			spaceId	path		string	true	"Space ID"
//	@Param			range	query		string	false	"1M, 3M, 6M, 1Y or ALL (default 1M)"
//	@Success		200		{object}	HistoryDTO
//	@Failure		400		{object}	httpx.ErrorEnvelope
//	@Failure		404		{object}	httpx.ErrorEnvelope
//	@Router			/spaces/{spaceId}/net-worth/history [get]
func (h *Handler) history(w http.ResponseWriter, r *http.Request) {
	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = "1M"
	}
	res, err := h.svc.History(r.Context(), spaces.SpaceIDFromContext(r.Context()), rng)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := HistoryDTO{Currency: res.Currency, Range: res.Range, ChangePct: money.PctPtr(res.ChangePct), InvestmentsChangePct: money.PctPtr(res.InvestmentsChangePct), Points: make([]PointDTO, 0, len(res.Points))}
	for _, p := range res.Points {
		out.Points = append(out.Points, PointDTO{Date: p.Date, NetWorth: money.NewAmount(p.NetWorth, res.Currency), Investments: money.NewAmount(p.Investments, res.Currency), Live: p.Live})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(out)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
