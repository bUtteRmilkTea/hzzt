package investments

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"github.com/JonathanRainee/hzzt/internal/auth"
	"github.com/JonathanRainee/hzzt/internal/currency"
	"github.com/JonathanRainee/hzzt/internal/db"
	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
	"github.com/JonathanRainee/hzzt/internal/platform/apperror"
	"github.com/JonathanRainee/hzzt/internal/platform/httpx"
	"github.com/JonathanRainee/hzzt/internal/platform/money"
	"github.com/JonathanRainee/hzzt/internal/spaces"
)

// Codes of the dated valuations (see plan Appendix B).
const (
	CodeValuationsNotSupported = "VALUATIONS_NOT_SUPPORTED"
	CodeValuationNotFound      = "VALUATION_NOT_FOUND"
	// CodeNavNotReverted is the warning of a delete that left the investment's NAV as it was (no earlier valuation).
	CodeNavNotReverted = "NAV_NOT_REVERTED"
)

var errValuationNF = apperror.New(404, CodeValuationNotFound, "Valuation not found")

const dateLayout = "2006-01-02"

// thousands is a NAV typed with grouping commas such as 1,534.20.
var thousands = regexp.MustCompile(`^\d{1,3}(,\d{3})+(\.\d+)?$`)

// parseNAV reads a NAV as the API and the forms write it: 1534.20 or 1,534.20.
func parseNAV(s string) (decimal.Decimal, error) {
	s = strings.TrimSpace(s)
	if thousands.MatchString(s) {
		s = strings.ReplaceAll(s, ",", "")
	}
	return money.Parse(s)
}

// ValuationInput is a new or replaced valuation.
type ValuationInput struct {
	AsOf time.Time
	NAV  decimal.Decimal
	Note *string
}

// ValuationRow is a stored valuation with its server computed figures (in the investment's quote currency).
type ValuationRow struct {
	sqlc.InvestmentValuation
	PnL       decimal.Decimal
	PnLPct    *decimal.Decimal
	HasChange bool
	// ChangeValue and ChangePct are the market move since the previous valuation, the position change excluded:
	// (nav − previous nav) × quantity, and (nav − previous nav) / previous nav × 100 (nil when the previous NAV is 0).
	ChangeValue decimal.Decimal
	ChangePct   *decimal.Decimal
	IsLatest    bool
}

// ValuationList is the valuations of one investment, newest first.
type ValuationList struct {
	Currency string
	Items    []ValuationRow
	Rate     *decimal.Decimal // quote → base at the current rate; nil without one
	Base     string
}

func buildRows(list []sqlc.InvestmentValuation) []ValuationRow {
	out := make([]ValuationRow, len(list))
	for i, v := range list {
		r := ValuationRow{InvestmentValuation: v, PnL: v.Value.Sub(v.CostBasis), IsLatest: i == 0}
		if !v.CostBasis.IsZero() {
			p := r.PnL.Div(v.CostBasis).Mul(hundred)
			r.PnLPct = &p
		}
		if i+1 < len(list) {
			prev := list[i+1]
			r.HasChange = true
			delta := v.Nav.Sub(prev.Nav)
			r.ChangeValue = delta.Mul(v.Quantity)
			if !prev.Nav.IsZero() {
				p := delta.Div(prev.Nav).Mul(hundred)
				r.ChangePct = &p
			}
		}
		out[i] = r
	}
	return out
}

// ownedMutualFund loads an investment the viewer may read and change valuations of: only the owner (valuations are the
// owner's records and are not scaled to a share), and only a MUTUAL_FUND. A shared-in viewer or another participant gets 403 OWNER_ONLY.
func (s *Service) ownedMutualFund(ctx context.Context, q *sqlc.Queries, spaceID, viewer, id uuid.UUID, lock bool) (sqlc.Investment, error) {
	var inv sqlc.Investment
	var err error
	if lock {
		inv, err = lockInvestment(ctx, q, spaceID, viewer, id)
	} else {
		inv, err = q.GetInvestmentForViewer(ctx, sqlc.GetInvestmentForViewerParams{ID: id, SpaceID: spaceID, UserID: uuid.NullUUID{UUID: viewer, Valid: true}})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return inv, errInvestmentNF
		case err == nil && inv.SpaceID != spaceID:
			return inv, ownerOnlyError()
		}
	}
	if err != nil {
		return inv, err
	}
	if err := requireOwner(inv, viewer); err != nil {
		return inv, err
	}
	if inv.AssetType != TypeMutualFund {
		return inv, apperror.Unprocessable(CodeValuationsNotSupported, "Dated valuations are only kept for mutual funds")
	}
	return inv, nil
}

func (s *Service) applyNAV(ctx context.Context, q *sqlc.Queries, inv sqlc.Investment, nav decimal.Decimal, asOf time.Time) error {
	d, err := parseStoredDetails(inv.AssetType, inv.Details)
	if err != nil {
		return err
	}
	det := d.(mutualFundDetails)
	date := asOf.Format(dateLayout)
	det.NavDate = &date
	raw, err := DetailsJSON(det)
	if err != nil {
		return err
	}
	return q.UpdateInvestmentNav(ctx, sqlc.UpdateInvestmentNavParams{ID: inv.ID, SpaceID: inv.SpaceID, ManualPrice: nav, Details: raw})
}

// AddValuation stores the NAV of a date (a second one for the same date replaces it). The newest valuation is copied into
// the investment (manual price and NAV date) so every total keeps using one path; an older date only adds history. The
// position snapshot (quantity and cost basis) is taken now, from the investment, and the manual current value is never touched.
// The second result is true when a new row was inserted (false: replaced).
func (s *Service) AddValuation(ctx context.Context, spaceID, userID, id uuid.UUID, in ValuationInput) (ValuationRow, bool, error) {
	var created bool
	var saved uuid.UUID
	err := db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		inv, err := s.ownedMutualFund(ctx, q, spaceID, userID, id, true)
		if err != nil {
			return err
		}
		old, oerr := q.GetValuationByDate(ctx, sqlc.GetValuationByDateParams{InvestmentID: inv.ID, AsOf: in.AsOf})
		if oerr != nil && !errors.Is(oerr, pgx.ErrNoRows) {
			return oerr
		}
		cost := inv.Quantity.Mul(inv.AvgBuyPrice).Add(inv.Fees).Round(priceScale)
		row, err := q.UpsertValuation(ctx, sqlc.UpsertValuationParams{
			InvestmentID: inv.ID, AsOf: in.AsOf, Nav: in.NAV, Quantity: inv.Quantity, CostBasis: cost,
			Value: inv.Quantity.Mul(in.NAV).Round(priceScale), Note: in.Note, CreatedByID: userID,
		})
		if err != nil {
			return err
		}
		created, saved = row.Inserted, row.ID
		if oerr == nil {
			log.Info().Str("spaceId", spaceID.String()).Str("investmentId", inv.ID.String()).Str("asOf", in.AsOf.Format(dateLayout)).
				Str("oldNav", old.Nav.String()).Str("newNav", in.NAV.String()).Msg("valuation replaced")
		}
		list, err := q.ListValuations(ctx, inv.ID)
		if err != nil {
			return err
		}
		if len(list) > 0 && list[0].ID == row.ID {
			return s.applyNAV(ctx, q, inv, in.NAV, in.AsOf)
		}
		return nil
	})
	if err != nil {
		return ValuationRow{}, false, err
	}
	list, err := s.q.ListValuations(ctx, id)
	if err != nil {
		return ValuationRow{}, false, err
	}
	for _, r := range buildRows(list) {
		if r.ID == saved {
			return r, created, nil
		}
	}
	return ValuationRow{}, false, errValuationNF
}

// Valuations lists the valuations of a mutual fund, newest first, with PnL since cost, the move since the previous one
// and a base currency copy at the current rate.
func (s *Service) Valuations(ctx context.Context, spaceID, userID, id uuid.UUID) (ValuationList, error) {
	inv, err := s.ownedMutualFund(ctx, s.q, spaceID, userID, id, false)
	if err != nil {
		return ValuationList{}, err
	}
	list, err := s.q.ListValuations(ctx, inv.ID)
	if err != nil {
		return ValuationList{}, err
	}
	cc, err := s.currency.BuildContext(ctx, spaceID)
	if err != nil {
		return ValuationList{}, err
	}
	out := ValuationList{Currency: strings.TrimSpace(inv.QuoteCurrency), Items: buildRows(list), Base: strings.TrimSpace(cc.BaseCurrency)}
	if rate, _, err := conversionRate(inv, out.Currency, cc); err == nil {
		out.Rate = &rate
	}
	return out, nil
}

// DeleteValuation removes one valuation. When it was the newest, the investment's NAV and NAV date go back to the previous
// valuation; with none left the investment keeps its NAV and the warning NAV_NOT_REVERTED says so.
func (s *Service) DeleteValuation(ctx context.Context, spaceID, userID, id, valuationID uuid.UUID) ([]currency.Warning, error) {
	warnings := []currency.Warning{}
	err := db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		inv, err := s.ownedMutualFund(ctx, q, spaceID, userID, id, true)
		if err != nil {
			return err
		}
		v, err := q.GetValuation(ctx, sqlc.GetValuationParams{ID: valuationID, InvestmentID: inv.ID})
		if errors.Is(err, pgx.ErrNoRows) {
			return errValuationNF
		}
		if err != nil {
			return err
		}
		list, err := q.ListValuations(ctx, inv.ID)
		if err != nil {
			return err
		}
		if err := q.DeleteValuation(ctx, sqlc.DeleteValuationParams{ID: v.ID, InvestmentID: inv.ID}); err != nil {
			return err
		}
		if list[0].ID != v.ID {
			return nil // history only
		}
		if len(list) == 1 {
			warnings = append(warnings, currency.Warning{Code: CodeNavNotReverted, Label: inv.Name,
				Message: "This was the only valuation, so the NAV of " + inv.Name + " stays as it is"})
			return nil
		}
		prev := list[1]
		return s.applyNAV(ctx, q, inv, prev.Nav, prev.AsOf)
	})
	return warnings, err
}

// --- HTTP ---------------------------------------------------------------------------

// ValuationRequest is a NAV for a date.
type ValuationRequest struct {
	// AsOf is the date of the NAV, not the day it is typed (YYYY-MM-DD, not in the future).
	AsOf string `json:"asOf" validate:"required" example:"2026-10-31"`
	// NAV is the price per unit, as 1534.20 or 1,534.20.
	NAV  string  `json:"nav" validate:"required" example:"1534.20"`
	Note *string `json:"note" validate:"omitempty"`
}

// ValuationBaseDTO is a valuation converted to the space base currency at the current rate.
type ValuationBaseDTO struct {
	Currency   string          `json:"currency" example:"IDR"`
	Value      money.Amount    `json:"value" swaggertype:"string"`
	CostBasis  money.Amount    `json:"costBasis" swaggertype:"string"`
	PnL        money.Amount    `json:"pnl" swaggertype:"string"`
	ChangeFrom *money.Amount   `json:"changeFromPrevious" swaggertype:"string"`
	FxRate     decimal.Decimal `json:"fxRate" swaggertype:"string"`
}

// ValuationChangeDTO is the market move since the previous valuation (the position change excluded).
type ValuationChangeDTO struct {
	Value money.Amount `json:"value" swaggertype:"string"`
	Pct   *money.Pct   `json:"pct" swaggertype:"string"`
}

// ValuationDTO is one dated NAV of a mutual fund.
type ValuationDTO struct {
	ID   uuid.UUID       `json:"id"`
	AsOf string          `json:"asOf" example:"2026-10-31"`
	NAV  decimal.Decimal `json:"nav" swaggertype:"string" example:"1534.2"`
	// Quantity and CostBasis are the position when the valuation was typed.
	Quantity  decimal.Decimal `json:"quantity" swaggertype:"string"`
	CostBasis money.Amount    `json:"costBasis" swaggertype:"string"`
	Value     money.Amount    `json:"value" swaggertype:"string"`
	Note      *string         `json:"note"`
	CreatedAt time.Time       `json:"createdAt"`
	// PnL is value − costBasis and PnLPct is PnL / costBasis × 100 (null when the cost is 0): profit since cost, not a yearly rate.
	PnL    money.Amount `json:"pnl" swaggertype:"string"`
	PnLPct *money.Pct   `json:"pnlPct" swaggertype:"string"`
	// ChangeFromPrevious is null for the oldest valuation.
	ChangeFromPrevious *ValuationChangeDTO `json:"changeFromPrevious"`
	// IsLatest is true for the newest valuation, the one the investment uses as its current NAV.
	IsLatest bool              `json:"isLatest"`
	Base     *ValuationBaseDTO `json:"base"`
}

// ValuationListResponse is the valuations of a mutual fund, newest first.
type ValuationListResponse struct {
	Currency string         `json:"currency" example:"IDR"`
	Items    []ValuationDTO `json:"items"`
	// LastValuationAt is the date of the newest valuation (null when there is none).
	LastValuationAt *string `json:"lastValuationAt" example:"2026-10-31"`
}

// ValuationDeleteResponse answers a delete.
type ValuationDeleteResponse struct {
	Warnings []currency.Warning `json:"warnings"`
}

func toValuationDTO(r ValuationRow, cur string, rate *decimal.Decimal, base string) ValuationDTO {
	d := ValuationDTO{
		ID: r.ID, AsOf: r.AsOf.Format(dateLayout), NAV: r.Nav, Quantity: r.Quantity,
		CostBasis: money.NewAmount(r.CostBasis, cur), Value: money.NewAmount(r.Value, cur), Note: r.Note, CreatedAt: r.CreatedAt.UTC(),
		PnL: money.NewAmount(r.PnL, cur), PnLPct: money.PctPtr(r.PnLPct), IsLatest: r.IsLatest,
	}
	if r.HasChange {
		d.ChangeFromPrevious = &ValuationChangeDTO{Value: money.NewAmount(r.ChangeValue, cur), Pct: money.PctPtr(r.ChangePct)}
	}
	if rate != nil {
		b := &ValuationBaseDTO{Currency: base, Value: money.NewAmount(r.Value.Mul(*rate), base), CostBasis: money.NewAmount(r.CostBasis.Mul(*rate), base),
			PnL: money.NewAmount(r.PnL.Mul(*rate), base), FxRate: *rate}
		if r.HasChange {
			c := money.NewAmount(r.ChangeValue.Mul(*rate), base)
			b.ChangeFrom = &c
		}
		d.Base = b
	}
	return d
}

func (h *Handler) valuationFields(req ValuationRequest) (ValuationInput, error) {
	f := map[string]any{}
	var in ValuationInput
	asOf, err := time.Parse(dateLayout, strings.TrimSpace(req.AsOf))
	switch {
	case err != nil:
		f["asOf"] = "must be a date like 2026-10-31"
	case asOf.After(h.svc.now().UTC().Add(24 * time.Hour)):
		f["asOf"] = "must not be in the future"
	default:
		in.AsOf = asOf
	}
	nav, err := parseNAV(req.NAV)
	switch {
	case err != nil:
		f["nav"] = "must be a number like 1534.20 or 1,534.20"
	case nav.IsNegative():
		f["nav"] = "must not be negative"
	case !nav.Round(priceScale).Equal(nav):
		f["nav"] = "must have at most 8 decimal places"
	case nav.GreaterThanOrEqual(maxValue):
		f["nav"] = "is too large"
	default:
		in.NAV = nav
	}
	if req.Note != nil {
		n := strings.TrimSpace(*req.Note)
		if utf8.RuneCountInString(n) > 500 {
			f["note"] = "must be at most 500 characters"
		}
		if n != "" {
			in.Note = &n
		}
	}
	return in, fieldsErr(f)
}

// addValuation godoc
//
//	@Summary		Record the NAV of a mutual fund on a date
//	@Description	Mutual funds only (422 VALUATIONS_NOT_SUPPORTED otherwise) and the owner only (403 OWNER_ONLY). One valuation per `asOf` date: sending the same date again replaces it (200, the old value is logged), so a typo is fixed by sending it again. The newest valuation is copied into the investment (`manualPrice` = nav, `details.navDate` = asOf) so every total keeps working; an older date only adds history. `manualCurrentValue` is never touched (if one is set it still wins). The quantity and cost basis of the position are snapshotted when it is typed, so a later BUY or SELL does not rewrite an earlier month. There is no reminder or job: the age of the NAV is `valuation.navAgeDays` on the investment.
//	@Tags			investments
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			spaceId	path		string				true	"Space ID"
//	@Param			id		path		string				true	"Investment ID"
//	@Param			body	body		ValuationRequest	true	"The NAV and its date"
//	@Success		200		{object}	ValuationDTO		"Replaced the valuation of that date"
//	@Success		201		{object}	ValuationDTO
//	@Failure		400		{object}	httpx.ErrorEnvelope
//	@Failure		403		{object}	httpx.ErrorEnvelope	"OWNER_ONLY"
//	@Failure		404		{object}	httpx.ErrorEnvelope
//	@Failure		422		{object}	httpx.ErrorEnvelope	"VALUATIONS_NOT_SUPPORTED"
//	@Router			/spaces/{spaceId}/investments/{id}/valuations [post]
func (h *Handler) addValuation(w http.ResponseWriter, r *http.Request) {
	id, err := investmentID(r)
	var in ValuationInput
	if err == nil {
		var req ValuationRequest
		if req, err = httpx.DecodeAndValidate[ValuationRequest](r); err == nil {
			in, err = h.valuationFields(req)
		}
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	spaceID := spaces.SpaceIDFromContext(r.Context())
	user, _ := auth.UserFromContext(r.Context())
	row, created, err := h.svc.AddValuation(r.Context(), spaceID, user.ID, id, in)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	inv, ierr := h.svc.q.GetInvestmentForViewer(r.Context(), sqlc.GetInvestmentForViewerParams{ID: id, SpaceID: spaceID, UserID: uuid.NullUUID{UUID: user.ID, Valid: true}})
	cur := "USD"
	if ierr == nil {
		cur = strings.TrimSpace(inv.QuoteCurrency)
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, toValuationDTO(row, cur, nil, ""))
}

// valuations godoc
//
//	@Summary		Dated valuations of a mutual fund, with the PnL of each
//	@Description	Newest first. `pnl` = value − costBasis and `pnlPct` = pnl / costBasis × 100 (null when the cost is 0). `changeFromPrevious` is the market move since the previous valuation with the position change excluded: (nav − previous nav) × quantity, and (nav − previous nav) / previous nav × 100. Neither is an annual rate. `base` is a copy in the space base currency at the current rate (null without a rate). The list never invents missing months; `lastValuationAt` is the date of the newest one. Owner only (403 OWNER_ONLY); mutual funds only (422).
//	@Tags			investments
//	@Security		BearerAuth
//	@Produce		json
//	@Param			spaceId	path		string	true	"Space ID"
//	@Param			id		path		string	true	"Investment ID"
//	@Success		200		{object}	ValuationListResponse
//	@Failure		403		{object}	httpx.ErrorEnvelope	"OWNER_ONLY"
//	@Failure		404		{object}	httpx.ErrorEnvelope
//	@Failure		422		{object}	httpx.ErrorEnvelope	"VALUATIONS_NOT_SUPPORTED"
//	@Router			/spaces/{spaceId}/investments/{id}/valuations [get]
func (h *Handler) valuations(w http.ResponseWriter, r *http.Request) {
	id, err := investmentID(r)
	var res ValuationList
	if err == nil {
		user, _ := auth.UserFromContext(r.Context())
		res, err = h.svc.Valuations(r.Context(), spaces.SpaceIDFromContext(r.Context()), user.ID, id)
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := ValuationListResponse{Currency: res.Currency, Items: make([]ValuationDTO, 0, len(res.Items))}
	for _, it := range res.Items {
		out.Items = append(out.Items, toValuationDTO(it, res.Currency, res.Rate, res.Base))
	}
	if len(res.Items) > 0 {
		d := res.Items[0].AsOf.Format(dateLayout)
		out.LastValuationAt = &d
	}
	writeJSON(w, http.StatusOK, out)
}

// deleteValuation godoc
//
//	@Summary		Delete one valuation of a mutual fund
//	@Description	Removes the valuation. When it was the newest, the investment's `manualPrice` and `details.navDate` go back to the previous valuation. When it was the only one the investment keeps its NAV and `warnings` carries NAV_NOT_REVERTED. Deleting an older valuation only removes history. Owner only.
//	@Tags			investments
//	@Security		BearerAuth
//	@Produce		json
//	@Param			spaceId			path		string	true	"Space ID"
//	@Param			id				path		string	true	"Investment ID"
//	@Param			valuationId		path		string	true	"Valuation ID"
//	@Success		200				{object}	ValuationDeleteResponse
//	@Failure		403				{object}	httpx.ErrorEnvelope	"OWNER_ONLY"
//	@Failure		404				{object}	httpx.ErrorEnvelope	"VALUATION_NOT_FOUND, INVESTMENT_NOT_FOUND"
//	@Router			/spaces/{spaceId}/investments/{id}/valuations/{valuationId} [delete]
func (h *Handler) deleteValuation(w http.ResponseWriter, r *http.Request) {
	id, err := investmentID(r)
	var vid uuid.UUID
	if err == nil {
		if vid, err = uuid.Parse(chi.URLParam(r, "valuationId")); err != nil {
			err = errValuationNF
		}
	}
	var warnings []currency.Warning
	if err == nil {
		user, _ := auth.UserFromContext(r.Context())
		warnings, err = h.svc.DeleteValuation(r.Context(), spaces.SpaceIDFromContext(r.Context()), user.ID, id, vid)
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ValuationDeleteResponse{Warnings: warnings})
}
