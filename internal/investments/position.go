package investments

import (
	"encoding/json"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
	"github.com/JonathanRainee/hzzt/internal/platform/apperror"
)

// Additional error codes (see plan Appendix B).
const (
	CodeInsufficientQuantity = "INSUFFICIENT_QUANTITY"
	CodePositionChangedSince = "POSITION_CHANGED_SINCE"
)

// Stored precision: the investments table keeps quantities at 18 and prices and
// fees at 8 decimal places, so every computed position is rounded the same way
// before it is compared with what the database holds.
const (
	quantityScale = 18
	priceScale    = 8
)

// Position is the part of an investment that BUY and SELL change.
type Position struct {
	Quantity    decimal.Decimal
	AvgBuyPrice decimal.Decimal
	Fees        decimal.Decimal
}

// Leg is the investment side of a BUY or SELL.
type Leg struct {
	Quantity  decimal.Decimal
	UnitPrice decimal.Decimal
	Fee       decimal.Decimal
}

// Gross is quantity × unit price, in the investment's quote currency.
func (l Leg) Gross() decimal.Decimal { return l.Quantity.Mul(l.UnitPrice) }

// PositionOf reads the position of a stored investment.
func PositionOf(inv sqlc.Investment) Position {
	return Position{Quantity: inv.Quantity, AvgBuyPrice: inv.AvgBuyPrice, Fees: inv.Fees}
}

// Equal reports whether two positions hold the same numbers.
func (p Position) Equal(o Position) bool {
	return p.Quantity.Equal(o.Quantity) && p.AvgBuyPrice.Equal(o.AvgBuyPrice) && p.Fees.Equal(o.Fees)
}

// Buy adds a purchase to the position:
//
//	new_qty = q + Δq
//	new_avg = (q × avg + Δq × unit_price) / new_qty
//	fees   += fee
func (p Position) Buy(l Leg) Position {
	newQty := p.Quantity.Add(l.Quantity)
	avg := p.AvgBuyPrice
	if newQty.IsPositive() {
		avg = p.Quantity.Mul(p.AvgBuyPrice).Add(l.Gross()).DivRound(newQty, 24).Round(priceScale)
	}
	return Position{
		Quantity:    newQty.Round(quantityScale),
		AvgBuyPrice: avg,
		Fees:        p.Fees.Add(l.Fee).Round(priceScale),
	}
}

// Sell removes a sale from the position and reports its realized profit or loss:
//
//	realized_pl = (unit_price − avg) × Δq − fee
//
// The average and the fees already paid stay as they are. Selling more than the
// quantity fails with INSUFFICIENT_QUANTITY.
func (p Position) Sell(l Leg) (Position, decimal.Decimal, error) {
	available := p.Quantity
	if l.Quantity.GreaterThan(available) {
		return Position{}, decimal.Zero, apperror.Unprocessable(CodeInsufficientQuantity,
			fmt.Sprintf("Only %s can be sold", available.String())).
			WithDetails(map[string]any{"available": available.String(), "requested": l.Quantity.String()})
	}
	realized := l.UnitPrice.Sub(p.AvgBuyPrice).Mul(l.Quantity).Sub(l.Fee)
	return Position{Quantity: p.Quantity.Sub(l.Quantity).Round(quantityScale), AvgBuyPrice: p.AvgBuyPrice, Fees: p.Fees}, realized.Round(priceScale), nil
}

// snapshot is the JSON stored in transactions.position_before / position_after.
type snapshot struct {
	Quantity    string `json:"quantity"`
	AvgBuyPrice string `json:"avgBuyPrice"`
	Fees        string `json:"fees"`
}

// MarshalJSON renders the position as the stored snapshot.
func (p Position) MarshalJSON() ([]byte, error) {
	return json.Marshal(snapshot{Quantity: p.Quantity.String(), AvgBuyPrice: p.AvgBuyPrice.String(), Fees: p.Fees.String()})
}

// ParsePosition reads a stored snapshot.
func ParsePosition(raw []byte) (Position, error) {
	var s snapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		return Position{}, fmt.Errorf("decode position snapshot: %w", err)
	}
	q, err1 := decimal.NewFromString(s.Quantity)
	a, err2 := decimal.NewFromString(s.AvgBuyPrice)
	f, err3 := decimal.NewFromString(s.Fees)
	if err1 != nil || err2 != nil || err3 != nil {
		return Position{}, fmt.Errorf("decode position snapshot: bad numbers")
	}
	return Position{Quantity: q, AvgBuyPrice: a, Fees: f}, nil
}
