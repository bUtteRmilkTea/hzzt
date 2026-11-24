package investments

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/JonathanRainee/hzzt/internal/auth"
	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
	"github.com/JonathanRainee/hzzt/internal/platform/apperror"
	"github.com/JonathanRainee/hzzt/internal/platform/httpx"
	"github.com/JonathanRainee/hzzt/internal/platform/money"
	"github.com/JonathanRainee/hzzt/internal/spaces"
)

// TradeRow is one BUY or SELL of an investment as the history shows it.
type TradeRow struct {
	Trade      sqlc.ListTradesByInvestmentRow
	Before     Position
	After      Position
	CanDelete  bool
	BlockedBy  string     // why it cannot be deleted: OWNER_ONLY, POSITION_CHANGED_SINCE or TRADE_DELETE_BREAKS_LATER_SELL
	BlockingID *uuid.UUID // the later SELL that would no longer fit
	ShowWallet bool       // false for a viewer of a shared investment (the wallet belongs to the owner)
}

// TradeHistory is a page of an investment's trades, newest first.
type TradeHistory struct {
	Items      []TradeRow
	NextCursor string
	Opening    Position
}

// Trades lists the BUY and SELL rows of one investment, newest first (occurred_at desc, id desc), cursor paged. Whoever may
// read the investment may read its history (the owner's space, or a participant of a shared one); only the owner can
// delete, which each row says in canDelete / blockedReason.
func (s *Service) Trades(ctx context.Context, spaceID, viewer, id uuid.UUID, cursor string, limit int) (TradeHistory, error) {
	inv, err := s.q.GetInvestmentForViewer(ctx, sqlc.GetInvestmentForViewerParams{ID: id, SpaceID: spaceID, UserID: uuid.NullUUID{UUID: viewer, Valid: true}})
	if errors.Is(err, pgx.ErrNoRows) {
		return TradeHistory{}, errInvestmentNF
	}
	if err != nil {
		return TradeHistory{}, err
	}
	if inv.SpaceID != spaceID { // shared in: the viewer must be a participant attached to this space
		if _, err := s.q.GetViewerParticipation(ctx, sqlc.GetViewerParticipationParams{
			InvestmentID: inv.ID, UserID: uuid.NullUUID{UUID: viewer, Valid: true}, AttachedSpaceID: uuid.NullUUID{UUID: spaceID, Valid: true},
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return TradeHistory{}, errInvestmentNF
			}
			return TradeHistory{}, err
		}
	}
	isOwner := inv.SpaceID == spaceID && requireOwner(inv, viewer) == nil

	p := sqlc.ListTradesByInvestmentParams{InvestmentID: uuid.NullUUID{UUID: inv.ID, Valid: true}, SpaceID: inv.SpaceID, RowLimit: int32(limit + 1)}
	if cursor != "" {
		c, err := httpx.DecodeCursor(cursor)
		if err != nil {
			return TradeHistory{}, apperror.Validation("Validation failed", map[string]any{"cursor": "is invalid"})
		}
		at := c.CreatedAt
		p.CursorAt, p.CursorID = &at, uuid.NullUUID{UUID: c.ID, Valid: true}
	}
	rows, err := s.q.ListTradesByInvestment(ctx, p)
	if err != nil {
		return TradeHistory{}, err
	}
	chain, err := s.q.ListTradeChain(ctx, sqlc.ListTradeChainParams{InvestmentID: p.InvestmentID, SpaceID: inv.SpaceID})
	if err != nil {
		return TradeHistory{}, err
	}
	parsed, err := parseChain(chain)
	if err != nil {
		return TradeHistory{}, err
	}
	pos := map[uuid.UUID]int{}
	for i, t := range chain {
		pos[t.ID] = i
	}

	out := TradeHistory{Opening: PositionOf(inv)}
	if len(chain) > 0 {
		if out.Opening, _, err = snapshots(chain[0]); err != nil {
			return TradeHistory{}, err
		}
	}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[limit-1]
		out.NextCursor = httpx.EncodeCursor(httpx.Cursor{CreatedAt: last.OccurredAt, ID: last.ID})
	}
	for _, r := range rows {
		row := TradeRow{Trade: r, ShowWallet: isOwner}
		if row.Before, row.After, err = snapshots(sqlc.Transaction{PositionBefore: r.PositionBefore, PositionAfter: r.PositionAfter}); err != nil {
			return TradeHistory{}, err
		}
		switch {
		case !isOwner:
			row.BlockedBy = CodeOwnerOnly
		default:
			row.BlockedBy, row.BlockingID = blockedReason(parsed, pos[r.ID], PositionOf(inv))
			row.CanDelete = row.BlockedBy == ""
		}
		out.Items = append(out.Items, row)
	}
	return out, nil
}

// blockedReason says why chain[at] cannot be deleted ("" when it can), using the same planDelete the delete runs. For a
// sale that would no longer fit it also returns that sale's id.
func blockedReason(chain []chainTrade, at int, current Position) (string, *uuid.UUID) {
	_, err := planDelete(chain, at, current)
	switch {
	case err == nil:
		return "", nil
	case errors.Is(err, errPositionChanged):
		return CodePositionChangedSince, nil
	}
	var blocking *uuid.UUID
	if ae, ok := apperror.As(err); ok {
		if raw, ok := ae.Details["tradeId"].(string); ok {
			if id, perr := uuid.Parse(raw); perr == nil {
				blocking = &id
			}
		}
	}
	return CodeTradeDeleteBreaksLaterSell, blocking
}

// --- HTTP ---------------------------------------------------------------------------

// PositionDTO is a position snapshot.
type PositionDTO struct {
	Quantity    decimal.Decimal `json:"quantity" swaggertype:"string" example:"1.5"`
	AvgBuyPrice decimal.Decimal `json:"avgBuyPrice" swaggertype:"string" example:"65000"`
	Fees        decimal.Decimal `json:"fees" swaggertype:"string" example:"10"`
}

func toPositionDTO(p Position) PositionDTO {
	return PositionDTO(p)
}

// TradeDTO is one BUY or SELL in the history of an investment.
type TradeDTO struct {
	ID         uuid.UUID       `json:"id"`
	Action     string          `json:"action" enums:"BUY,SELL"`
	OccurredAt time.Time       `json:"occurredAt"`
	Quantity   decimal.Decimal `json:"quantity" swaggertype:"string" example:"0.5"`
	UnitPrice  decimal.Decimal `json:"unitPrice" swaggertype:"string" example:"60000"`
	Fee        decimal.Decimal `json:"fee" swaggertype:"string" example:"0"`
	// Amount is the wallet side (what left or entered the wallet), in the wallet's currency.
	Amount   money.Amount `json:"amount" swaggertype:"string" example:"30000"`
	Currency string       `json:"currency" example:"USD"`
	// WalletID and WalletName are null for a viewer of a shared investment (the wallet is the owner's).
	WalletID   *uuid.UUID       `json:"walletId"`
	WalletName *string          `json:"walletName"`
	RealizedPL *decimal.Decimal `json:"realizedPl" swaggertype:"string"`
	// PositionBefore and PositionAfter are the position of the investment around this trade.
	PositionBefore PositionDTO `json:"positionBefore"`
	PositionAfter  PositionDTO `json:"positionAfter"`
	// CanDelete says whether DELETE /transfers/{id} would work now. When false, BlockedReason is OWNER_ONLY,
	// POSITION_CHANGED_SINCE (the position was edited by hand after the trade) or TRADE_DELETE_BREAKS_LATER_SELL.
	CanDelete     bool       `json:"canDelete"`
	BlockedReason *string    `json:"blockedReason" enums:"OWNER_ONLY,POSITION_CHANGED_SINCE,TRADE_DELETE_BREAKS_LATER_SELL"`
	BlockedBy     *uuid.UUID `json:"blockedByTradeId"`
}

// OpeningPositionDTO is the position the history starts from.
type OpeningPositionDTO struct {
	Quantity    decimal.Decimal `json:"quantity" swaggertype:"string" example:"0"`
	AvgBuyPrice decimal.Decimal `json:"avgBuyPrice" swaggertype:"string" example:"0"`
}

// TradeListResponse is a page of trades, newest first.
type TradeListResponse struct {
	Items []TradeDTO `json:"items"`
	// Next is the cursor of the next page; null on the last page.
	Next *string `json:"next"`
	// OpeningPosition is the position before the oldest trade (the whole current position when there are no trades),
	// so the screen can show where the history starts. An investment created without funding starts above zero.
	OpeningPosition OpeningPositionDTO `json:"openingPosition"`
}

func toTradeDTO(r TradeRow) TradeDTO {
	t := r.Trade
	d := TradeDTO{
		ID: t.ID, OccurredAt: t.OccurredAt.UTC(), Quantity: t.Quantity.Decimal, UnitPrice: t.UnitPrice.Decimal, Fee: t.Fee.Decimal,
		Amount: money.NewAmount(t.Amount, t.Currency), Currency: strings.TrimSpace(t.Currency),
		PositionBefore: toPositionDTO(r.Before), PositionAfter: toPositionDTO(r.After), CanDelete: r.CanDelete, BlockedBy: r.BlockingID,
	}
	if t.InvestmentAction != nil {
		d.Action = *t.InvestmentAction
	}
	if t.RealizedPl.Valid {
		v := t.RealizedPl.Decimal
		d.RealizedPL = &v
	}
	if r.ShowWallet && t.WalletID.Valid {
		id := t.WalletID.UUID
		d.WalletID, d.WalletName = &id, t.WalletName
	}
	if r.BlockedBy != "" {
		b := r.BlockedBy
		d.BlockedReason = &b
	}
	return d
}

// trades godoc
//
//	@Summary		History of the BUY and SELL trades of one investment
//	@Description	Newest first (occurredAt desc, id desc) with cursor paging (`next`). Whoever can read the investment can read its history (a viewer of a shared investment too; the wallet is then hidden). `openingPosition` is the position before the oldest trade, or the whole current position when there are none; an investment created without funding starts above zero, and the history can never go below that. `canDelete` and `blockedReason` say whether `DELETE /spaces/{spaceId}/transfers/{id}` works for each row: any trade can be deleted, not only the newest; the trades after it are replayed on top of the position it started from and their `positionBefore`, `positionAfter` and `realizedPl` are rewritten (each later trade keeps its own wallet amounts). It is refused with 422 TRADE_DELETE_BREAKS_LATER_SELL (naming the first sale that would no longer fit, nothing changes) or 409 POSITION_CHANGED_SINCE (the position was edited by hand after the trade). Net worth snapshots of past days are not rewritten; only today's figures move. Only the owner can delete (403 OWNER_ONLY).
//	@Tags			investments
//	@Security		BearerAuth
//	@Produce		json
//	@Param			spaceId	path		string	true	"Space ID"
//	@Param			id		path		string	true	"Investment ID"
//	@Param			cursor	query		string	false	"next of the previous page"
//	@Param			limit	query		int		false	"Page size, 1 to 100 (default 30)"
//	@Success		200		{object}	TradeListResponse
//	@Failure		400		{object}	httpx.ErrorEnvelope
//	@Failure		404		{object}	httpx.ErrorEnvelope
//	@Router			/spaces/{spaceId}/investments/{id}/trades [get]
func (h *Handler) trades(w http.ResponseWriter, r *http.Request) {
	id, err := investmentID(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	limit := 30
	if v := r.URL.Query().Get("limit"); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 1 || n > 100 {
			httpx.WriteError(w, r, apperror.Validation("Validation failed", map[string]any{"limit": "must be between 1 and 100"}))
			return
		}
		limit = n
	}
	user, _ := auth.UserFromContext(r.Context())
	res, err := h.svc.Trades(r.Context(), spaces.SpaceIDFromContext(r.Context()), user.ID, id, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := TradeListResponse{Items: make([]TradeDTO, 0, len(res.Items)),
		OpeningPosition: OpeningPositionDTO{Quantity: res.Opening.Quantity, AvgBuyPrice: res.Opening.AvgBuyPrice}}
	for _, it := range res.Items {
		out.Items = append(out.Items, toTradeDTO(it))
	}
	if res.NextCursor != "" {
		out.Next = &res.NextCursor
	}
	writeJSON(w, http.StatusOK, out)
}
