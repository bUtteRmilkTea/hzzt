package investments

import (
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/JonathanRainee/hzzt/internal/auth"
	"github.com/JonathanRainee/hzzt/internal/currency"
	"github.com/JonathanRainee/hzzt/internal/platform/apperror"
	"github.com/JonathanRainee/hzzt/internal/platform/httpx"
	"github.com/JonathanRainee/hzzt/internal/platform/money"
	"github.com/JonathanRainee/hzzt/internal/spaces"
)

// WrapperAllocationDTO is one wrapper's planned and actual share of its asset type.
type WrapperAllocationDTO struct {
	AssetType string `json:"assetType" enums:"CRYPTO,ID_STOCK,US_STOCK,MUTUAL_FUND,GLOBAL_ETF"`
	// Wrapper is the display name; null for the unassigned group.
	Wrapper *string `json:"wrapper" example:"Binance"`
	Label   string  `json:"label" example:"Binance"`
	Key     string  `json:"key" example:"binance"`
	// TargetPct is the percentage set for this wrapper; null when none is set (not 0).
	TargetPct *money.Pct `json:"targetPct" swaggertype:"string" example:"40.00"`
	// ActualPct is the wrapper's share of its asset type's value; null when the asset type is worth 0.
	ActualPct *money.Pct `json:"actualPct" swaggertype:"string" example:"33.33"`
	// DriftPct is actualPct - targetPct; null unless both are known.
	DriftPct *money.Pct `json:"driftPct" swaggertype:"string" example:"-6.67"`
}

// CategoryTotalsDTO is how much of an asset type's 100% has been assigned.
type CategoryTotalsDTO struct {
	AssignedPct    money.Pct `json:"assignedPct" swaggertype:"string" example:"70.00"`
	UnallocatedPct money.Pct `json:"unallocatedPct" swaggertype:"string" example:"30.00"`
}

// CategoryAllocationDTO is the planned split of one asset type.
type CategoryAllocationDTO struct {
	AssetType string `json:"assetType" enums:"CRYPTO,ID_STOCK,US_STOCK,MUTUAL_FUND,GLOBAL_ETF"`
	Label     string `json:"label" example:"Crypto"`
	CategoryTotalsDTO
	Wrappers []WrapperAllocationDTO `json:"wrappers"`
}

// AllocationsResponse lists every wrapper's target and actual share.
type AllocationsResponse struct {
	Currency   string                  `json:"currency" example:"IDR"`
	Categories []CategoryAllocationDTO `json:"categories"`
	Warnings   []currency.Warning      `json:"warnings"`
}

// SetTargetResponse is the result of editing one wrapper's percentage.
type SetTargetResponse struct {
	WrapperAllocationDTO
	Category CategoryTotalsDTO `json:"category"`
}

// SetTargetRequest sets or clears a wrapper's percentage.
type SetTargetRequest struct {
	// TargetPct is a percentage from 0 to 100 with at most 2 decimals ("40.00"); null clears it.
	TargetPct nullStr `json:"targetPct" swaggertype:"string" example:"40.00"`
}

func toWrapperAllocationDTO(w WrapperAlloc) WrapperAllocationDTO {
	return WrapperAllocationDTO{AssetType: w.AssetType, Wrapper: w.Wrapper, Label: w.Label, Key: w.Key,
		TargetPct: money.PctPtr(w.TargetPct), ActualPct: money.PctPtr(w.ActualPct), DriftPct: money.PctPtr(w.DriftPct)}
}

func toCategoryTotals(c CategoryAlloc) CategoryTotalsDTO {
	return CategoryTotalsDTO{AssignedPct: money.NewPct(c.AssignedPct), UnallocatedPct: money.NewPct(c.UnallocatedPct)}
}

// wrappers godoc
//
//	@Summary		Planned and actual share of every wrapper
//	@Description	Per asset type: each wrapper (exchange, broker or platform) with its `targetPct` (null when none is set), its `actualPct` share of the asset type's base-currency value and `driftPct` (actual - target), plus the type's `assignedPct` and `unallocatedPct` (100 - assigned). A wrapper whose percentage is set but that no longer holds any investment is still listed (actualPct 0.00). The same fields are embedded in GET /investments/grouped.
//	@Tags			investments
//	@Security		BearerAuth
//	@Produce		json
//	@Param			spaceId	path		string	true	"Space ID"
//	@Param			include	query		string	false	"shared: also count investments other people shared into this space for the actual share"
//	@Success		200		{object}	AllocationsResponse
//	@Failure		400		{object}	httpx.ErrorEnvelope
//	@Failure		404		{object}	httpx.ErrorEnvelope
//	@Router			/spaces/{spaceId}/investments/wrappers [get]
func (h *Handler) wrappers(w http.ResponseWriter, r *http.Request) {
	includeShared, err := parseInclude(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	user, _ := auth.UserFromContext(r.Context())
	al, err := h.svc.Allocations(r.Context(), spaces.SpaceIDFromContext(r.Context()), includeShared, user.ID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := AllocationsResponse{Currency: al.Currency, Warnings: al.Warnings, Categories: make([]CategoryAllocationDTO, 0, len(al.Categories))}
	for _, c := range al.Categories {
		cd := CategoryAllocationDTO{AssetType: c.AssetType, Label: c.Label, CategoryTotalsDTO: toCategoryTotals(c), Wrappers: make([]WrapperAllocationDTO, 0, len(c.Wrappers))}
		for _, wr := range c.Wrappers {
			cd.Wrappers = append(cd.Wrappers, toWrapperAllocationDTO(wr))
		}
		out.Categories = append(out.Categories, cd)
	}
	writeJSON(w, http.StatusOK, out)
}

// setWrapperTarget godoc
//
//	@Summary		Set or clear the percentage of one wrapper
//	@Description	Edits one wrapper at a time. `wrapperKey` is the wrapper lower-cased and URL-encoded (any casing and extra whitespace is accepted; `_unassigned` is rejected). The percentages set in an asset type may add up to at most 100.00; the rest stays unallocated (422 ALLOCATION_EXCEEDS_100, with `details.assigned` by the other wrappers and `details.available`). `targetPct: null` clears the percentage. The wrapper must hold a live investment or already have a percentage (404 WRAPPER_NOT_FOUND). Any member of the space may edit; every write records who did it. The response carries the asset type's new assigned and unallocated percentage.
//	@Tags			investments
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			spaceId		path		string				true	"Space ID"
//	@Param			assetType	path		string				true	"CRYPTO, ID_STOCK, US_STOCK, MUTUAL_FUND or GLOBAL_ETF"
//	@Param			wrapperKey	path		string				true	"Wrapper, lower-cased and URL-encoded"
//	@Param			body		body		SetTargetRequest	true	"Percentage"
//	@Success		200			{object}	SetTargetResponse
//	@Failure		400			{object}	httpx.ErrorEnvelope
//	@Failure		404			{object}	httpx.ErrorEnvelope	"space or WRAPPER_NOT_FOUND"
//	@Failure		422			{object}	httpx.ErrorEnvelope	"ALLOCATION_EXCEEDS_100"
//	@Router			/spaces/{spaceId}/investments/wrappers/{assetType}/{wrapperKey} [patch]
func (h *Handler) setWrapperTarget(w http.ResponseWriter, r *http.Request) {
	assetType, key, err := wrapperParams(r)
	var target *decimal.Decimal
	if err == nil {
		var in SetTargetRequest
		if in, err = httpx.DecodeAndValidate[SetTargetRequest](r); err == nil {
			target, err = parseTarget(in.TargetPct)
		}
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	user, _ := auth.UserFromContext(r.Context())
	cat, wr, err := h.svc.SetWrapperTarget(r.Context(), spaces.SpaceIDFromContext(r.Context()), user.ID, assetType, key, target)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, SetTargetResponse{WrapperAllocationDTO: toWrapperAllocationDTO(wr), Category: toCategoryTotals(cat)})
}

func wrapperParams(r *http.Request) (assetType, key string, err error) {
	assetType = strings.ToUpper(strings.TrimSpace(chi.URLParam(r, "assetType")))
	if _, ok := MetaFor(assetType); !ok {
		return "", "", apperror.Validation("Validation failed", map[string]any{"assetType": "must be one of: CRYPTO ID_STOCK US_STOCK MUTUAL_FUND GLOBAL_ETF"})
	}
	raw := chi.URLParam(r, "wrapperKey")
	if dec, uerr := url.PathUnescape(raw); uerr == nil {
		raw = dec
	}
	key = WrapperKey(raw)
	switch {
	case key == UnassignedKey:
		return "", "", apperror.Validation("Validation failed", map[string]any{"wrapperKey": "the unassigned group cannot have a percentage"})
	case key == "" || utf8.RuneCountInString(key) > 60:
		return "", "", apperror.Validation("Validation failed", map[string]any{"wrapperKey": "must be 1 to 60 characters"})
	}
	return assetType, key, nil
}

// parseTarget reads targetPct: required, a decimal string or null.
func parseTarget(n nullStr) (*decimal.Decimal, error) {
	if !n.Set {
		return nil, apperror.Validation("Validation failed", map[string]any{"targetPct": "is required (a percentage, or null to clear it)"})
	}
	if n.Value == nil {
		return nil, nil
	}
	d, err := money.Parse(strings.TrimSpace(*n.Value))
	if err != nil {
		return nil, apperror.Validation("Validation failed", map[string]any{"targetPct": "must be a decimal like 40.00"})
	}
	if _, ok := pctToBps(d); !ok {
		return nil, apperror.Validation("Validation failed", map[string]any{"targetPct": "must be from 0 to 100 with at most 2 decimal places"})
	}
	return &d, nil
}
