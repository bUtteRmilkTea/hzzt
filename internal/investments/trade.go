package investments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"github.com/JonathanRainee/hzzt/internal/currency"
	"github.com/JonathanRainee/hzzt/internal/db"
	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
	"github.com/JonathanRainee/hzzt/internal/ledger"
	"github.com/JonathanRainee/hzzt/internal/platform/apperror"
)

// ErrNotATrade means the transaction exists but is not an investment BUY/SELL.
var ErrNotATrade = errors.New("not an investment trade")

var (
	errWalletNF        = apperror.New(404, "WALLET_NOT_FOUND", "Wallet not found")
	errPositionChanged = apperror.Conflict(CodePositionChangedSince,
		"The investment's position changed after this transaction; undo the later ones first")
)

// TradeInput describes a BUY (wallet → investment) or SELL (investment → wallet).
type TradeInput struct {
	Action       string // ledger.ActionBuy or ledger.ActionSell
	WalletID     uuid.UUID
	InvestmentID uuid.UUID
	Leg          Leg
	// Amount is the wallet side in the wallet's currency; optional when the wallet
	// uses the investment's currency or an FX rate is available.
	Amount     *decimal.Decimal
	OccurredAt *time.Time
	Note       *string
	Title      *string
}

// TradePatch is a partial edit of a BUY/SELL.
type TradePatch struct {
	WalletID   *uuid.UUID
	Quantity   *decimal.Decimal
	UnitPrice  *decimal.Decimal
	Fee        *decimal.Decimal
	Amount     *decimal.Decimal
	OccurredAt *time.Time
	NoteSet    bool
	Note       *string
}

// tradeResult is everything a BUY/SELL writes.
type tradeResult struct {
	amount   decimal.Decimal // wallet side
	total    decimal.Decimal // investment side: gross + fee (BUY) or gross − fee (SELL), quote currency
	fxRate   decimal.Decimal // wallet currency per quote currency unit
	before   Position
	after    Position
	realized *decimal.Decimal
	details  []byte // investment details after the trade (ID_STOCK lots follow the quantity)
}

func validationErr(field, msg string) error {
	return apperror.Validation("Validation failed", map[string]any{field: msg})
}

// walletSide works out how much the wallet is debited/credited and the rate used.
// override wins; the same currency needs no rate; fallback (the rate of the trade
// being edited) keeps an edited cross-currency trade on its agreed rate; otherwise
// the space's ConversionContext converts.
func walletSide(w sqlc.Wallet, quote string, total decimal.Decimal, override, fallback *decimal.Decimal, cc *currency.ConversionContext) (amount, rate decimal.Decimal, err error) {
	wc := strings.TrimSpace(w.Currency)
	switch {
	case override != nil:
		amount = *override
	case strings.EqualFold(wc, quote):
		amount = total
	case fallback != nil:
		amount = total.Mul(*fallback)
	default:
		amount, err = currency.Convert(total, quote, wc, cc)
		if err != nil {
			return decimal.Zero, decimal.Zero, apperror.Unprocessable("FX_RATE_UNAVAILABLE",
				"No exchange rate between "+quote+" and "+wc+"; enter the wallet amount yourself").WithCause(err)
		}
	}
	amount = amount.Round(8)
	if !amount.IsPositive() {
		return decimal.Zero, decimal.Zero, validationErr("amount", "must be greater than 0")
	}
	if !total.IsPositive() {
		return decimal.Zero, decimal.Zero, validationErr("fee", "the fee must be smaller than the proceeds")
	}
	rate = amount.DivRound(total, 12)
	if rate.GreaterThanOrEqual(decimal.New(1, 16)) {
		return decimal.Zero, decimal.Zero, validationErr("amount", "implies an exchange rate that is out of range")
	}
	return amount, rate, nil
}

// lotsDetails returns the investment's details with lots following the quantity (ID_STOCK).
func lotsDetails(inv sqlc.Investment, qty decimal.Decimal) ([]byte, error) {
	if inv.AssetType != TypeIDStock {
		return inv.Details, nil
	}
	d, err := parseStoredDetails(inv.AssetType, inv.Details)
	if err != nil {
		return nil, err
	}
	det := d.(idStockDetails)
	lots := json.Number(qty.Div(decimal.NewFromInt(LotSize)).String())
	det.Lots = &lots
	return json.Marshal(det)
}

// checkLeg validates the numbers of a trade against the investment's type.
func checkLeg(inv sqlc.Investment, l Leg) error {
	f := map[string]any{}
	if !l.Quantity.IsPositive() {
		f["quantity"] = "must be greater than 0"
	} else if scale := QuantityScaleFor(inv.AssetType); !l.Quantity.Round(scale).Equal(l.Quantity) {
		f["quantity"] = fmt.Sprintf("must have at most %d decimal places", scale)
	} else if inv.AssetType == TypeIDStock && !l.Quantity.Div(decimal.NewFromInt(LotSize)).Round(lotsScale).Equal(l.Quantity.Div(decimal.NewFromInt(LotSize))) {
		f["quantity"] = "must be lots with at most 1 decimal place (a multiple of 10 shares)"
	}
	if !l.UnitPrice.IsPositive() {
		f["unitPrice"] = "must be greater than 0"
	} else if !l.UnitPrice.Round(priceScale).Equal(l.UnitPrice) {
		f["unitPrice"] = "must have at most 8 decimal places"
	}
	if l.Fee.IsNegative() {
		f["fee"] = "must not be negative"
	} else if !l.Fee.Round(priceScale).Equal(l.Fee) {
		f["fee"] = "must have at most 8 decimal places"
	}
	for k, v := range map[string]decimal.Decimal{"quantity": l.Quantity, "unitPrice": l.UnitPrice, "fee": l.Fee} {
		if v.GreaterThanOrEqual(maxValue) {
			f[k] = "is too large"
		}
	}
	return fieldsErr(f)
}

// computeTrade applies a BUY/SELL to `from` (the position before the trade).
func computeTrade(inv sqlc.Investment, from Position, w sqlc.Wallet, action string, l Leg, override, fallback *decimal.Decimal, cc *currency.ConversionContext) (tradeResult, error) {
	if err := checkLeg(inv, l); err != nil {
		return tradeResult{}, err
	}
	r := tradeResult{before: from}
	quote := strings.TrimSpace(inv.QuoteCurrency)
	switch action {
	case ledger.ActionBuy:
		r.after = from.Buy(l)
		r.total = l.Gross().Add(l.Fee)
	case ledger.ActionSell:
		after, realized, err := from.Sell(l)
		if err != nil {
			return tradeResult{}, err
		}
		r.after, r.realized = after, &realized
		r.total = l.Gross().Sub(l.Fee)
	default:
		return tradeResult{}, validationErr("action", "must be BUY or SELL")
	}
	var err error
	if r.amount, r.fxRate, err = walletSide(w, quote, r.total, override, fallback, cc); err != nil {
		return tradeResult{}, err
	}
	if r.details, err = lotsDetails(inv, r.after.Quantity); err != nil {
		return tradeResult{}, err
	}
	return r, nil
}

func nullD(d decimal.Decimal) decimal.NullDecimal {
	return decimal.NullDecimal{Decimal: d, Valid: true}
}

func loadWallet(ctx context.Context, q *sqlc.Queries, spaceID, id uuid.UUID) (sqlc.Wallet, error) {
	w, err := q.GetWallet(ctx, sqlc.GetWalletParams{ID: id, SpaceID: spaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return w, errWalletNF
	}
	return w, err
}

// lockInvestment locks an investment of the space for writing. A miss is 403
// OWNER_ONLY when the investment is one shared into the space (visible to the
// user but not theirs to change), otherwise 404.
func lockInvestment(ctx context.Context, q *sqlc.Queries, spaceID, userID, id uuid.UUID) (sqlc.Investment, error) {
	inv, err := q.GetInvestmentForUpdate(ctx, sqlc.GetInvestmentForUpdateParams{ID: id, SpaceID: spaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return inv, missOrOwnerOnly(ctx, q, spaceID, userID, id)
	}
	return inv, err
}

// missOrOwnerOnly explains why an owned lookup missed: OWNER_ONLY for a
// shared-in investment the user can see, 404 for everything else (no existence leak).
func missOrOwnerOnly(ctx context.Context, q *sqlc.Queries, spaceID, userID, id uuid.UUID) error {
	_, err := q.GetInvestmentForViewer(ctx, sqlc.GetInvestmentForViewerParams{ID: id, SpaceID: spaceID, UserID: uuid.NullUUID{UUID: userID, Valid: true}})
	switch {
	case err == nil:
		return ownerOnlyError()
	case errors.Is(err, pgx.ErrNoRows):
		return errInvestmentNF
	default:
		return err
	}
}

func (s *Service) writePosition(ctx context.Context, q *sqlc.Queries, inv sqlc.Investment, pos Position, details []byte) error {
	_, err := q.UpdateInvestmentPosition(ctx, sqlc.UpdateInvestmentPositionParams{
		ID: inv.ID, SpaceID: inv.SpaceID, Quantity: pos.Quantity, AvgBuyPrice: pos.AvgBuyPrice, Fees: pos.Fees, Details: details,
	})
	return err
}

func tradeParams(spaceID, userID uuid.UUID, inv sqlc.Investment, w sqlc.Wallet, action string, l Leg, r tradeResult, occurred time.Time, note, title *string) (sqlc.CreateTransactionParams, error) {
	before, err := json.Marshal(r.before)
	if err != nil {
		return sqlc.CreateTransactionParams{}, err
	}
	after, err := json.Marshal(r.after)
	if err != nil {
		return sqlc.CreateTransactionParams{}, err
	}
	quote := strings.TrimSpace(inv.QuoteCurrency)
	p := sqlc.CreateTransactionParams{
		SpaceID: spaceID, Type: ledger.TypeTransfer, Title: title, Note: note,
		Amount: r.amount, Currency: w.Currency, WalletID: uuid.NullUUID{UUID: w.ID, Valid: true},
		OccurredAt: occurred.UTC(), CreatedByID: userID,
		ToAmount: nullD(r.total), ToCurrency: &quote, FxRate: nullD(r.fxRate),
		InvestmentID: uuid.NullUUID{UUID: inv.ID, Valid: true}, InvestmentAction: &action,
		Quantity: nullD(l.Quantity), UnitPrice: nullD(l.UnitPrice), Fee: nullD(l.Fee),
		PositionBefore: before, PositionAfter: after,
	}
	if r.realized != nil {
		p.RealizedPl = nullD(*r.realized)
	}
	return p, nil
}

// Trade records a BUY or SELL: the wallet is debited/credited through the
// ledger, the position is updated and the transaction row is written, all in
// one database transaction.
func (s *Service) Trade(ctx context.Context, spaceID, userID uuid.UUID, in TradeInput) (sqlc.Transaction, error) {
	cc, err := s.currency.BuildContext(ctx, spaceID)
	if err != nil {
		return sqlc.Transaction{}, err
	}
	var out sqlc.Transaction
	err = db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		w, err := loadWallet(ctx, q, spaceID, in.WalletID)
		if err != nil {
			return err
		}
		inv, err := lockInvestment(ctx, q, spaceID, userID, in.InvestmentID)
		if err != nil {
			return err
		}
		if err := requireOwner(inv, userID); err != nil {
			return err
		}
		r, err := computeTrade(inv, PositionOf(inv), w, in.Action, in.Leg, in.Amount, nil, cc)
		if err != nil {
			return err
		}
		occurred := s.now()
		if in.OccurredAt != nil {
			occurred = *in.OccurredAt
		}
		p, err := tradeParams(spaceID, userID, inv, w, in.Action, in.Leg, r, occurred, in.Note, in.Title)
		if err != nil {
			return err
		}
		if out, err = q.CreateTransaction(ctx, p); err != nil {
			return err
		}
		if err := ledger.Apply(ctx, tx, out); err != nil {
			return err
		}
		return s.writePosition(ctx, q, inv, r.after, r.details)
	})
	return out, err
}

func tradeOf(old sqlc.Transaction) (action string, ok bool) {
	if !old.InvestmentID.Valid || old.InvestmentAction == nil {
		return "", false
	}
	return *old.InvestmentAction, true
}

// legOf reads the investment side of a stored trade.
func legOf(t sqlc.Transaction) Leg {
	return Leg{Quantity: t.Quantity.Decimal, UnitPrice: t.UnitPrice.Decimal, Fee: t.Fee.Decimal}
}

// UpdateTrade edits a BUY/SELL. It is allowed only while the investment's
// position still equals the position this trade left behind (otherwise 409
// POSITION_CHANGED_SINCE); the old effect is reversed and the new one applied
// on the position as it was before the trade.
func (s *Service) UpdateTrade(ctx context.Context, spaceID, userID, id uuid.UUID, p TradePatch) (sqlc.Transaction, error) {
	cc, err := s.currency.BuildContext(ctx, spaceID)
	if err != nil {
		return sqlc.Transaction{}, err
	}
	var out sqlc.Transaction
	err = db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		old, err := q.GetTransactionForUpdate(ctx, sqlc.GetTransactionForUpdateParams{ID: id, SpaceID: spaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotATrade
		}
		if err != nil {
			return err
		}
		action, ok := tradeOf(old)
		if !ok {
			return ErrNotATrade
		}
		inv, err := lockInvestment(ctx, q, spaceID, userID, old.InvestmentID.UUID)
		if err != nil {
			return err
		}
		if err := requireOwner(inv, userID); err != nil {
			return err
		}
		before, after, err := snapshots(old)
		if err != nil {
			return err
		}
		if !PositionOf(inv).Equal(after) {
			return errPositionChanged
		}

		walletID := old.WalletID.UUID
		if p.WalletID != nil {
			walletID = *p.WalletID
		}
		w, err := loadWallet(ctx, q, spaceID, walletID)
		if err != nil {
			return err
		}
		l := legOf(old)
		if p.Quantity != nil {
			l.Quantity = *p.Quantity
		}
		if p.UnitPrice != nil {
			l.UnitPrice = *p.UnitPrice
		}
		if p.Fee != nil {
			l.Fee = *p.Fee
		}
		var fallback *decimal.Decimal
		if old.FxRate.Valid && strings.EqualFold(strings.TrimSpace(w.Currency), strings.TrimSpace(old.Currency)) {
			fallback = &old.FxRate.Decimal // keep the rate agreed when the trade was made
		}
		r, err := computeTrade(inv, before, w, action, l, p.Amount, fallback, cc)
		if err != nil {
			return err
		}

		if err := ledger.Reverse(ctx, tx, old); err != nil {
			return err
		}
		occurred := old.OccurredAt
		if p.OccurredAt != nil {
			occurred = p.OccurredAt.UTC()
		}
		note := old.Note
		if p.NoteSet {
			note = cleanNote(p.Note)
		}
		beforeJSON, _ := json.Marshal(r.before)
		afterJSON, _ := json.Marshal(r.after)
		quote := strings.TrimSpace(inv.QuoteCurrency)
		upd := sqlc.UpdateInvestmentTradeParams{
			ID: id, SpaceID: spaceID, WalletID: uuid.NullUUID{UUID: w.ID, Valid: true}, Amount: r.amount, Currency: w.Currency,
			FxRate: nullD(r.fxRate), ToAmount: nullD(r.total), ToCurrency: &quote,
			Quantity: nullD(l.Quantity), UnitPrice: nullD(l.UnitPrice), Fee: nullD(l.Fee),
			PositionBefore: beforeJSON, PositionAfter: afterJSON, OccurredAt: occurred, Note: note,
		}
		if r.realized != nil {
			upd.RealizedPl = nullD(*r.realized)
		}
		if out, err = q.UpdateInvestmentTrade(ctx, upd); err != nil {
			return err
		}
		if err := ledger.Apply(ctx, tx, out); err != nil {
			return err
		}
		return s.writePosition(ctx, q, inv, r.after, r.details)
	})
	return out, err
}

// DeleteTrade removes a BUY/SELL and reverses its wallet effect. Any trade can go, not only the newest: the trades
// recorded after it are replayed on top of the position it started from, and their position snapshots and realised
// profit are rewritten in the same database transaction (the investment row is locked first). Each later trade keeps its
// own wallet amounts. Refused with 422 TRADE_DELETE_BREAKS_LATER_SELL when a later SELL would no longer have enough
// quantity (nothing changes), and with 409 POSITION_CHANGED_SINCE when the position was edited by hand after the trade
// (the snapshots no longer form an unbroken chain up to today's position). If the investment itself was deleted only the
// wallet effect is reversed.
func (s *Service) DeleteTrade(ctx context.Context, spaceID, userID, id uuid.UUID) error {
	return db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		old, err := q.GetTransactionForUpdate(ctx, sqlc.GetTransactionForUpdateParams{ID: id, SpaceID: spaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotATrade
		}
		if err != nil {
			return err
		}
		if _, ok := tradeOf(old); !ok {
			return ErrNotATrade
		}
		inv, err := q.GetInvestmentForUpdate(ctx, sqlc.GetInvestmentForUpdateParams{ID: old.InvestmentID.UUID, SpaceID: spaceID})
		gone := errors.Is(err, pgx.ErrNoRows)
		if err != nil && !gone {
			return err
		}
		var (
			final   Position
			later   []sqlc.Transaction
			replay  []ReplayedTrade
			rewrite bool
		)
		if !gone {
			if err := requireOwner(inv, userID); err != nil {
				return err
			}
			chain, err := q.ListTradeChain(ctx, sqlc.ListTradeChainParams{InvestmentID: old.InvestmentID, SpaceID: spaceID})
			if err != nil {
				return err
			}
			at := slices.IndexFunc(chain, func(t sqlc.Transaction) bool { return t.ID == id })
			if at < 0 {
				return fmt.Errorf("trade %s is missing from the chain of investment %s", id, inv.ID)
			}
			parsed, err := parseChain(chain)
			if err != nil {
				return err
			}
			plan, err := planDelete(parsed, at, PositionOf(inv))
			if err != nil {
				return err
			}
			final, later, replay, rewrite = plan.final, chain[at+1:], plan.replay, true
		}
		if err := ledger.Reverse(ctx, tx, old); err != nil {
			return err
		}
		if _, err := q.DeleteTransaction(ctx, sqlc.DeleteTransactionParams{ID: id, SpaceID: spaceID}); err != nil {
			return err
		}
		if !rewrite {
			return nil
		}
		for i, r := range replay {
			beforeJSON, _ := json.Marshal(r.Before)
			afterJSON, _ := json.Marshal(r.After)
			upd := sqlc.UpdateTradePositionsParams{ID: later[i].ID, SpaceID: spaceID, PositionBefore: beforeJSON, PositionAfter: afterJSON}
			if r.Realized != nil {
				upd.RealizedPl = nullD(*r.Realized)
			}
			if err := q.UpdateTradePositions(ctx, upd); err != nil {
				return err
			}
		}
		details, err := lotsDetails(inv, final.Quantity)
		if err != nil {
			return err
		}
		if err := s.writePosition(ctx, q, inv, final, details); err != nil {
			return err
		}
		log.Info().Str("spaceId", spaceID.String()).Str("userId", userID.String()).Str("investmentId", inv.ID.String()).
			Str("tradeId", id.String()).Int("laterTradesRewritten", len(replay)).
			Str("positionBefore", positionLog(PositionOf(inv))).Str("positionAfter", positionLog(final)).
			Msg("investment trade deleted")
		return nil
	})
}

// deletePlan is the outcome of removing one trade from a chain.
type deletePlan struct {
	replay []ReplayedTrade // the later trades, recomputed
	final  Position        // the position of the investment afterwards
}

// chainTrade is a stored trade with its snapshots parsed once.
type chainTrade struct {
	step          TradeStep
	before, after Position
}

// parseChain reads the snapshots of a chain of trades.
func parseChain(chain []sqlc.Transaction) ([]chainTrade, error) {
	out := make([]chainTrade, len(chain))
	for i, t := range chain {
		b, a, err := snapshots(t)
		if err != nil {
			return nil, err
		}
		action, _ := tradeOf(t)
		out[i] = chainTrade{step: TradeStep{ID: t.ID, Action: action, Leg: legOf(t)}, before: b, after: a}
	}
	return out, nil
}

// planDelete works out what removing chain[at] does. The chain is the investment's trades in the order they were
// recorded; every trade stores the position before and after it. The trades after `at` must continue the position the
// deleted trade left (an unbroken run up to the investment's current position), otherwise the position was edited by
// hand and the answer is errPositionChanged.
func planDelete(chain []chainTrade, at int, current Position) (deletePlan, error) {
	expect := chain[at].after
	steps := make([]TradeStep, 0, len(chain)-at-1)
	for _, t := range chain[at+1:] {
		if !t.before.Equal(expect) {
			return deletePlan{}, errPositionChanged
		}
		expect = t.after
		steps = append(steps, t.step)
	}
	if !current.Equal(expect) {
		return deletePlan{}, errPositionChanged
	}
	replay, err := ReplayPositions(chain[at].before, steps)
	if err != nil {
		return deletePlan{}, err
	}
	plan := deletePlan{replay: replay, final: chain[at].before}
	if len(replay) > 0 {
		plan.final = replay[len(replay)-1].After
	}
	return plan, nil
}

func positionLog(p Position) string {
	return fmt.Sprintf("qty=%s avg=%s fees=%s", p.Quantity, p.AvgBuyPrice, p.Fees)
}

func snapshots(t sqlc.Transaction) (before, after Position, err error) {
	if before, err = ParsePosition(t.PositionBefore); err != nil {
		return
	}
	after, err = ParsePosition(t.PositionAfter)
	return
}

func cleanNote(n *string) *string {
	if n == nil {
		return nil
	}
	t := strings.TrimSpace(*n)
	if t == "" {
		return nil
	}
	return &t
}
