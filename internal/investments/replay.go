package investments

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/JonathanRainee/hzzt/internal/ledger"
	"github.com/JonathanRainee/hzzt/internal/platform/apperror"
)

// CodeTradeDeleteBreaksLaterSell is the 422 answered when deleting a trade would leave a later SELL without enough quantity.
const CodeTradeDeleteBreaksLaterSell = "TRADE_DELETE_BREAKS_LATER_SELL"

// TradeStep is one stored trade to replay: its action and the investment side of it.
type TradeStep struct {
	ID     uuid.UUID
	Action string
	Leg    Leg
}

// ReplayedTrade is the position before and after a trade once it is applied on top of the ones before it, and its
// realised profit or loss (nil for a BUY).
type ReplayedTrade struct {
	ID       uuid.UUID
	Before   Position
	After    Position
	Realized *decimal.Decimal
}

// ReplayPositions applies the steps in order on top of start with the same Position.Buy and Position.Sell the live
// trades use, so each trade's position and realised profit come out as if the history had been entered that way.
// A SELL that no longer fits answers 422 TRADE_DELETE_BREAKS_LATER_SELL naming that trade.
func ReplayPositions(start Position, steps []TradeStep) ([]ReplayedTrade, error) {
	out := make([]ReplayedTrade, 0, len(steps))
	cur := start
	for _, st := range steps {
		rt := ReplayedTrade{ID: st.ID, Before: cur}
		switch st.Action {
		case ledger.ActionBuy:
			rt.After = cur.Buy(st.Leg)
		case ledger.ActionSell:
			after, realized, err := cur.Sell(st.Leg)
			if err != nil {
				return nil, apperror.Unprocessable(CodeTradeDeleteBreaksLaterSell,
					fmt.Sprintf("A later sale would no longer have enough quantity (only %s left at that point)", cur.Quantity.String())).
					WithDetails(map[string]any{"tradeId": st.ID.String(), "available": cur.Quantity.String(), "requested": st.Leg.Quantity.String()})
			}
			rt.After, rt.Realized = after, &realized
		default:
			return nil, fmt.Errorf("replay: unknown trade action %q", st.Action)
		}
		cur = rt.After
		out = append(out, rt)
	}
	return out, nil
}
