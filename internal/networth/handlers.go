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
	// The other components of the net worth of the day (from the snapshot; computed now for the live point).
	Cash        money.Amount `json:"cash" swaggertype:"string"`
	FixedAssets money.Amount `json:"fixedAssets" swaggertype:"string"`
	Receivables money.Amount `json:"receivables" swaggertype:"string"`
	Liabilities money.Amount `json:"liabilities" swaggertype:"string"`
	// Assets is everything owned: cash + investments + fixedAssets + receivables (debt not subtracted).
	Assets money.Amount `json:"assets" swaggertype:"string"`
	Live   bool         `json:"live,omitempty"` // the point computed now for today
}

// SeriesDTO is the verdict of one line of the graph over the requested range.
type SeriesDTO struct {
	First money.Amount `json:"first" swaggertype:"string"`
	Last  money.Amount `json:"last" swaggertype:"string"`
	// Change is last - first, in money.
	Change money.Amount `json:"change" swaggertype:"string"`
	// ChangePct is first to last of the value, a percentage; null when the first value is 0 or there is one point only.
	ChangePct *money.Pct `json:"changePct" swaggertype:"string"`
	// NetContributions is the money put in (or taken out) from outside in the range; MarketChange = change - netContributions.
	NetContributions money.Amount `json:"netContributions" swaggertype:"string"`
	MarketChange     money.Amount `json:"marketChange" swaggertype:"string"`
	// PerformancePct is the return excluding money put in: marketChange / (first + netContributions); null when that base is not positive.
	PerformancePct *money.Pct `json:"performancePct" swaggertype:"string"`
	// Direction is decided by the server from change: FLAT when the absolute changePct is below 0.05.
	Direction string `json:"direction" enums:"RISING,FALLING,FLAT"`
}

// SeriesSetDTO holds the four lines the client draws.
type SeriesSetDTO struct {
	NetWorth    SeriesDTO `json:"netWorth"`
	Investments SeriesDTO `json:"investments"`
	FixedAssets SeriesDTO `json:"fixedAssets"`
	Assets      SeriesDTO `json:"assets"`
}

// CoverageDTO tells how much of the requested range the stored history covers.
type CoverageDTO struct {
	// FirstPointDate is the date of the first point returned.
	FirstPointDate string `json:"firstPointDate" example:"2026-11-02"`
	// RequestedFrom is the first date of the range asked for; null for ALL.
	RequestedFrom *string `json:"requestedFrom" example:"2026-08-18"`
	// Short is true when the history starts later than the requested range (it only exists from the first daily snapshot).
	Short bool `json:"short"`
}

func toSeriesDTO(s SeriesStat, cur string) SeriesDTO {
	return SeriesDTO{
		First: money.NewAmount(s.First, cur), Last: money.NewAmount(s.Last, cur), Change: money.NewAmount(s.Change, cur),
		ChangePct: money.PctPtr(s.ChangePct), NetContributions: money.NewAmount(s.NetContributions, cur),
		MarketChange: money.NewAmount(s.MarketChange, cur), PerformancePct: money.PctPtr(s.PerformancePct), Direction: s.Direction,
	}
}

// HistoryDTO is the net-worth graph of a space.
type HistoryDTO struct {
	Currency  string     `json:"currency" example:"USD"`
	Range     string     `json:"range" enums:"1W,1M,3M,6M,1Y,YTD,ALL"`
	Points    []PointDTO `json:"points"`
	ChangePct *money.Pct `json:"changePct" swaggertype:"string"` // first → last point, a percentage; null when it cannot be computed
	// InvestmentsChangePct is the same change for the portfolio value.
	InvestmentsChangePct *money.Pct `json:"investmentsChangePct" swaggertype:"string"`
	// NetContributions is the money put in or taken out from outside (top ups, adjustments, opening
	// balances) between the first and the last point, in the base currency, converted with each
	// day's rate. A deposit is not growth: use marketChange / performancePct for the real return.
	NetContributions money.Amount `json:"netContributions" swaggertype:"string"`
	// MarketChange is (last - first) - netContributions.
	MarketChange money.Amount `json:"marketChange" swaggertype:"string"`
	// PerformancePct is marketChange / (first + netContributions), a percentage; null when that base is not positive.
	PerformancePct *money.Pct `json:"performancePct" swaggertype:"string"`
	// Series gives the verdict of each line over the range: first, last, change, changePct, the return excluding deposits
	// and the direction. Known limit: history only exists from the day the daily snapshot first ran for the space (00:15 UTC,
	// one row per day) because no historical market prices are stored; earlier days are never invented. See coverage.
	Series   SeriesSetDTO       `json:"series"`
	Coverage CoverageDTO        `json:"coverage"`
	Warnings []currency.Warning `json:"warnings"`
}

// history godoc
//
//	@Summary		Net-worth history for a range
//	@Description	Daily snapshots (stored at 00:15 UTC, one per UTC date) in the space's current base currency, plus a live point for today, at most about 120 points (evenly downsampled, first and last kept). A space created today returns just the live point. `changePct` is the change from the first to the last point. Each point carries every component (`cash`, `investments`, `fixedAssets`, `receivables`, `liabilities`) and `assets` (everything owned, debt not subtracted). `series` gives, for `netWorth`, `investments`, `fixedAssets` and `assets`, the first and last value, the change in money and in percent, the money put in from outside (`netContributions`: wallet top ups, adjustments and opening balances; BUY minus SELL with fees as a cost; fixed assets bought minus sold; holdings added with no funding wallet), the `marketChange` and `performancePct` (the return excluding what was put in) and a `direction` (RISING, FALLING or FLAT when the absolute changePct is below 0.05) so the client never decides it. Contributions are converted with the FX rate of their own date; one without a rate is left out and reported in `warnings` (FX_RATE_UNAVAILABLE). `coverage` says when the history really starts: it only exists from the first daily snapshot, no past prices are rebuilt or invented.
//	@Tags			net-worth
//	@Security		BearerAuth
//	@Produce		json
//	@Param			spaceId	path		string	true	"Space ID"
//	@Param			range	query		string	false	"1W, 1M, 3M, 6M, 1Y, YTD or ALL (default 1M)"
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
	out := HistoryDTO{Currency: res.Currency, Range: res.Range, ChangePct: money.PctPtr(res.ChangePct), InvestmentsChangePct: money.PctPtr(res.InvestmentsChangePct), NetContributions: money.NewAmount(res.NetContributions, res.Currency), MarketChange: money.NewAmount(res.MarketChange, res.Currency), PerformancePct: money.PctPtr(res.PerformancePct), Warnings: res.Warnings,
		Series:   SeriesSetDTO{NetWorth: toSeriesDTO(res.Series.NetWorth, res.Currency), Investments: toSeriesDTO(res.Series.Investments, res.Currency), FixedAssets: toSeriesDTO(res.Series.FixedAssets, res.Currency), Assets: toSeriesDTO(res.Series.Assets, res.Currency)},
		Coverage: CoverageDTO{FirstPointDate: res.Coverage.FirstPointDate, RequestedFrom: res.Coverage.RequestedFrom, Short: res.Coverage.Short},
		Points:   make([]PointDTO, 0, len(res.Points))}
	for _, p := range res.Points {
		out.Points = append(out.Points, PointDTO{
			Date: p.Date, NetWorth: money.NewAmount(p.NetWorth, res.Currency), Investments: money.NewAmount(p.Investments, res.Currency),
			Cash: money.NewAmount(p.Cash, res.Currency), FixedAssets: money.NewAmount(p.FixedAssets, res.Currency),
			Receivables: money.NewAmount(p.Receivables, res.Currency), Liabilities: money.NewAmount(p.Liabilities, res.Currency),
			Assets: money.NewAmount(p.Assets, res.Currency), Live: p.Live,
		})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(out)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
