package investments

import (
	"net/http"
	"strings"

	"github.com/JonathanRainee/hzzt/internal/auth"
	"github.com/JonathanRainee/hzzt/internal/currency"
	"github.com/JonathanRainee/hzzt/internal/platform/apperror"
	"github.com/JonathanRainee/hzzt/internal/platform/httpx"
	"github.com/JonathanRainee/hzzt/internal/platform/money"
	"github.com/JonathanRainee/hzzt/internal/spaces"
)

// TotalsDTO are base-currency sums.
type TotalsDTO struct {
	Value           money.Amount `json:"value" swaggertype:"string" example:"900000"`
	Cost            money.Amount `json:"cost" swaggertype:"string" example:"700000"`
	UnrealizedPL    money.Amount `json:"unrealizedPl" swaggertype:"string" example:"200000"`
	UnrealizedPLPct *money.Pct   `json:"unrealizedPlPct" swaggertype:"string" example:"28.57"`
}

func toTotalsDTO(t Totals, cur string) TotalsDTO {
	return TotalsDTO{Value: money.NewAmount(t.Value, cur), Cost: money.NewAmount(t.Cost, cur), UnrealizedPL: money.NewAmount(t.UnrealizedPL, cur),
		UnrealizedPLPct: money.PctPtr(t.UnrealizedPLPct)}
}

// WrapperGroupDTO is one exchange, broker or platform within an asset type.
type WrapperGroupDTO struct {
	// Wrapper is the display name; null for investments without a wrapper.
	Wrapper *string `json:"wrapper" example:"Binance"`
	// Label is the wrapper, or "Unassigned".
	Label string `json:"label" example:"Binance"`
	// Key is the lower-cased, whitespace-normalised wrapper ("_unassigned" for none).
	Key string `json:"key" example:"binance"`
	TotalsDTO
	PctOfCategory *money.Pct `json:"pctOfCategory" swaggertype:"string" example:"66.67"`
	PctOfTotal    *money.Pct `json:"pctOfTotal" swaggertype:"string" example:"40.00"`
	// TargetPct is the percentage set for this wrapper (null when none); DriftPct = pctOfCategory - targetPct.
	TargetPct *money.Pct      `json:"targetPct" swaggertype:"string" example:"60.00"`
	DriftPct  *money.Pct      `json:"driftPct" swaggertype:"string" example:"6.67"`
	Count     int             `json:"count"`
	Items     []InvestmentDTO `json:"items,omitempty"`
}

// CategoryGroupDTO is one asset type.
type CategoryGroupDTO struct {
	AssetType string `json:"assetType" enums:"CRYPTO,ID_STOCK,US_STOCK,MUTUAL_FUND,GLOBAL_ETF"`
	Label     string `json:"label" example:"Crypto"`
	TotalsDTO
	PctOfTotal *money.Pct `json:"pctOfTotal" swaggertype:"string" example:"60.00"`
	// AssignedPct is the sum of the wrapper percentages set in this asset type; UnallocatedPct = 100 - assigned.
	AssignedPct    money.Pct         `json:"assignedPct" swaggertype:"string" example:"70.00"`
	UnallocatedPct money.Pct         `json:"unallocatedPct" swaggertype:"string" example:"30.00"`
	Wrappers       []WrapperGroupDTO `json:"wrappers"`
}

// GroupedResponse is the portfolio grouped by asset type, then wrapper.
type GroupedResponse struct {
	Currency   string             `json:"currency" example:"IDR"`
	Total      TotalsDTO          `json:"total"`
	Categories []CategoryGroupDTO `json:"categories"`
	Warnings   []currency.Warning `json:"warnings"`
}

// grouped godoc
//
//	@Summary		Investments grouped by asset type and wrapper
//	@Description	Groups the same valued list as GET /investments by asset type, then by wrapper (exchange, broker or platform; compared case- and whitespace-insensitively; none is `wrapper: null`, key `_unassigned`). Sums use the viewer's base-currency values, so they equal `summary` of the flat list; items without a usable FX rate are listed but left out of every sum and reported in `warnings`. `pctOfCategory` and `pctOfTotal` are percentages with 2 decimals whose slices add up to exactly 100.00 (null when the denominator is 0), so pie data is `categories[].pctOfTotal` and `wrappers[].pctOfCategory`. `items=false` leaves the items out.
//	@Tags			investments
//	@Security		BearerAuth
//	@Produce		json
//	@Param			spaceId	path		string	true	"Space ID"
//	@Param			include	query		string	false	"shared: also group investments other people shared into this space (valued for my share)"
//	@Param			items	query		bool	false	"false: omit the items of each wrapper (default true)"
//	@Success		200		{object}	GroupedResponse
//	@Failure		400		{object}	httpx.ErrorEnvelope
//	@Failure		404		{object}	httpx.ErrorEnvelope
//	@Router			/spaces/{spaceId}/investments/grouped [get]
func (h *Handler) grouped(w http.ResponseWriter, r *http.Request) {
	includeShared, err := parseInclude(r)
	withItems := true
	if err == nil {
		switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("items"))) {
		case "", "true":
		case "false":
			withItems = false
		default:
			err = apperror.Validation("Validation failed", map[string]any{"items": "must be true or false"})
		}
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	user, _ := auth.UserFromContext(r.Context())
	res, err := h.svc.List(r.Context(), spaces.SpaceIDFromContext(r.Context()), "", includeShared, user.ID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	g := Group(res.Summary.Currency, res.Items)
	rows, err := h.svc.q.ListWrapperAllocations(r.Context(), spaces.SpaceIDFromContext(r.Context()))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toGroupedResponse(g, BuildAllocations(g, rows), withItems))
}

func toGroupedResponse(g Grouped, al Allocations, withItems bool) GroupedResponse {
	out := GroupedResponse{Currency: g.Currency, Total: toTotalsDTO(g.Total, g.Currency), Categories: make([]CategoryGroupDTO, 0, len(g.Categories)), Warnings: g.Warnings}
	for _, c := range g.Categories {
		cd := CategoryGroupDTO{AssetType: c.AssetType, Label: c.Label, TotalsDTO: toTotalsDTO(c.Totals, g.Currency),
			PctOfTotal: money.PctPtr(c.PctOfTotal), Wrappers: make([]WrapperGroupDTO, 0, len(c.Wrappers))}
		cat := al.category(c.AssetType)
		cd.AssignedPct, cd.UnallocatedPct = money.NewPct(cat.AssignedPct), money.NewPct(cat.UnallocatedPct)
		for _, wg := range c.Wrappers {
			wd := WrapperGroupDTO{Wrapper: wg.Wrapper, Label: groupLabel(wg), Key: wg.Key, TotalsDTO: toTotalsDTO(wg.Totals, g.Currency),
				PctOfCategory: money.PctPtr(wg.PctOfCategory), PctOfTotal: money.PctPtr(wg.PctOfTotal), Count: len(wg.Items)}
			if _, a, ok := al.find(c.AssetType, wg.Key); ok {
				wd.TargetPct, wd.DriftPct = money.PctPtr(a.TargetPct), money.PctPtr(a.DriftPct)
			}
			if withItems {
				wd.Items = make([]InvestmentDTO, 0, len(wg.Items))
				for _, it := range wg.Items {
					wd.Items = append(wd.Items, toDTO(it))
				}
			}
			cd.Wrappers = append(cd.Wrappers, wd)
		}
		out.Categories = append(out.Categories, cd)
	}
	return out
}
