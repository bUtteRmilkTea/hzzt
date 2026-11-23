package networth

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/JonathanRainee/hzzt/internal/currency"
	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
)

// Directions of a series over the requested range.
const (
	DirectionRising  = "RISING"
	DirectionFalling = "FALLING"
	DirectionFlat    = "FLAT"
)

// snapshotClock is the time of the UTC day at which the daily snapshot runs (00:15).
const snapshotClock = 15 * time.Minute

// FlatBandPct is the absolute change, in percent, below which a series is FLAT (plan question Q11: 0.05 until answered).
var FlatBandPct = decimal.RequireFromString("0.05")

// SeriesStat is the verdict of one line of the graph over the range.
type SeriesStat struct {
	First, Last, Change decimal.Decimal
	// ChangePct is first to last of the value; nil when the first value is 0.
	ChangePct *decimal.Decimal
	// NetContributions is the money put into (or taken out of) the line from outside in the range; MarketChange is the
	// rest of the change: (last - first) - NetContributions.
	NetContributions, MarketChange decimal.Decimal
	// PerformancePct is MarketChange / (first + NetContributions), nil when that base is not positive.
	PerformancePct *decimal.Decimal
	Direction      string
}

// Series holds the four lines the client draws.
type Series struct {
	NetWorth, Investments, FixedAssets, Assets SeriesStat
}

// Coverage tells how much of the requested range the stored history covers.
type Coverage struct {
	// FirstPointDate is the date of the first point returned.
	FirstPointDate string
	// RequestedFrom is the first date of the requested range; nil for ALL.
	RequestedFrom *string
	// Short is true when the history starts later than the range asked for.
	Short bool
}

func coverage(firstDate string, from time.Time, bounded bool) Coverage {
	c := Coverage{FirstPointDate: firstDate}
	if bounded {
		rf := from.Format("2006-01-02")
		c.RequestedFrom = &rf
		c.Short = firstDate > rf
	}
	return c
}

func directionOf(change decimal.Decimal, pct *decimal.Decimal) string {
	if pct != nil {
		switch {
		case pct.Abs().LessThan(FlatBandPct):
			return DirectionFlat
		case pct.IsPositive():
			return DirectionRising
		default:
			return DirectionFalling
		}
	}
	switch {
	case change.IsPositive():
		return DirectionRising
	case change.IsNegative():
		return DirectionFalling
	}
	return DirectionFlat
}

func statOf(first, last, contrib decimal.Decimal) SeriesStat {
	st := SeriesStat{First: first, Last: last, Change: last.Sub(first), NetContributions: contrib}
	hundred := decimal.NewFromInt(100)
	if !first.IsZero() {
		pct := st.Change.Div(first.Abs()).Mul(hundred).Round(2)
		st.ChangePct = &pct
	}
	st.MarketChange = st.Change.Sub(contrib)
	if base := first.Add(contrib); base.IsPositive() {
		pct := st.MarketChange.Div(base).Mul(hundred).Round(2)
		st.PerformancePct = &pct
	}
	st.Direction = directionOf(st.Change, st.ChangePct)
	return st
}

// flows are the money movements from outside between the first point and now, in the base currency.
type flows struct {
	// cash is what was put into or taken out of the wallets (adjustments, top ups, opening balances).
	cash decimal.Decimal
	// investments is BUY minus SELL (fees as a cost) of the trades in the range.
	investments decimal.Decimal
	// unfunded is the cost of investments added without any trade (no funding wallet).
	unfunded decimal.Decimal
	// fixed is the purchases minus the disposals of fixed assets.
	fixed    decimal.Decimal
	warnings []currency.Warning
}

func buildSeries(first, last Point, f flows) Series {
	if first.Date == last.Date { // one single point: nothing to compare
		one := func(v decimal.Decimal) SeriesStat {
			return SeriesStat{First: v, Last: v, Direction: DirectionFlat}
		}
		return Series{NetWorth: one(last.NetWorth), Investments: one(last.Investments), FixedAssets: one(last.FixedAssets), Assets: one(last.Assets)}
	}
	return Series{
		// Money entering the wallets, fixed assets bought and holdings added by hand are not market returns.
		NetWorth:    statOf(first.NetWorth, last.NetWorth, f.cash.Add(f.fixed).Add(f.unfunded)),
		Investments: statOf(first.Investments, last.Investments, f.investments.Add(f.unfunded)),
		FixedAssets: statOf(first.FixedAssets, last.FixedAssets, f.fixed),
		// Buying an investment moves cash into it: neutral for everything owned, so trades are not counted here.
		Assets: statOf(first.Assets, last.Assets, f.cash.Add(f.fixed).Add(f.unfunded)),
	}
}

// flowsBetween collects the external money flows of the space from the first point's snapshot (00:15 UTC of its
// date) to now. Each flow is converted with the FX rate of its own date; one without a rate is left out and reported.
func (s *Service) flowsBetween(ctx context.Context, spaceID uuid.UUID, firstDate string, now time.Time) (flows, error) {
	d0, err := time.Parse("2006-01-02", firstDate)
	if err != nil {
		return flows{}, err
	}
	from := d0.Add(snapshotClock)
	var out flows
	ccByDay := map[string]*currency.ConversionContext{}
	seen := map[string]bool{}
	convert := func(amount decimal.Decimal, cur string, at time.Time) (decimal.Decimal, bool, error) {
		day := utcDate(at).Format("2006-01-02")
		cc := ccByDay[day]
		if cc == nil {
			var err error
			if cc, err = s.currency.BuildContextAt(ctx, spaceID, at); err != nil {
				return decimal.Zero, false, err
			}
			ccByDay[day] = cc
		}
		v, err := currency.Convert(amount, cur, cc.BaseCurrency, cc)
		if err != nil {
			msg := fmt.Sprintf("No %s/%s rate for %s: a movement of money was left out of the contributions", strings.TrimSpace(cur), strings.TrimSpace(cc.BaseCurrency), day)
			if !seen[msg] {
				seen[msg] = true
				out.warnings = append(out.warnings, currency.Warning{Code: currency.CodeFXRateUnavailable, Message: msg})
			}
			return decimal.Zero, false, nil
		}
		return v, true, nil
	}

	// wallets: adjustments, top ups and opening balances
	cashRows, err := s.q.ListNetWorthContributions(ctx, sqlc.ListNetWorthContributionsParams{SpaceID: spaceID, OccurredAt: from, OccurredAt_2: now})
	if err != nil {
		return flows{}, err
	}
	for _, r := range cashRows {
		v, ok, err := convert(r.Amount, r.Currency, r.OccurredAt)
		if err != nil {
			return flows{}, err
		}
		if !ok {
			continue
		}
		if r.Direction != nil && *r.Direction == "OUT" {
			v = v.Neg()
		}
		out.cash = out.cash.Add(v)
	}

	// investments: BUY adds gross + fee, SELL takes out gross - fee (the fee is a cost, never a gain)
	trades, err := s.q.ListInvestmentTradeFlows(ctx, sqlc.ListInvestmentTradeFlowsParams{SpaceID: spaceID, FromTs: from, ToTs: now})
	if err != nil {
		return flows{}, err
	}
	for _, r := range trades {
		if r.InvestmentAction == nil || !r.Quantity.Valid || !r.UnitPrice.Valid {
			continue
		}
		gross := r.Quantity.Decimal.Mul(r.UnitPrice.Decimal)
		fee := decimal.Zero
		if r.Fee.Valid {
			fee = r.Fee.Decimal
		}
		amount := gross.Add(fee) // BUY
		if *r.InvestmentAction == "SELL" {
			amount = gross.Sub(fee).Neg()
		}
		v, ok, err := convert(amount, r.QuoteCurrency, r.OccurredAt)
		if err != nil {
			return flows{}, err
		}
		if ok {
			out.investments = out.investments.Add(v)
		}
	}
	unfunded, err := s.q.ListUnfundedInvestmentCreations(ctx, sqlc.ListUnfundedInvestmentCreationsParams{SpaceID: spaceID, FromTs: from, ToTs: now})
	if err != nil {
		return flows{}, err
	}
	for _, r := range unfunded {
		v, ok, err := convert(r.Quantity.Mul(r.AvgBuyPrice).Add(r.Fees), r.QuoteCurrency, r.CreatedAt)
		if err != nil {
			return flows{}, err
		}
		if ok {
			out.unfunded = out.unfunded.Add(v)
		}
	}

	// fixed assets: bought minus disposed of
	assets, err := s.q.ListFixedAssetFlows(ctx, sqlc.ListFixedAssetFlowsParams{SpaceID: spaceID, FromTs: from, ToTs: now})
	if err != nil {
		return flows{}, err
	}
	for _, r := range assets {
		v, ok, err := convert(r.Value, r.Currency, r.At)
		if err != nil {
			return flows{}, err
		}
		if !ok {
			continue
		}
		if r.Flow == "DISPOSAL" {
			v = v.Neg()
		}
		out.fixed = out.fixed.Add(v)
	}
	return out, nil
}
