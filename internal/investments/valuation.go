package investments

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/JonathanRainee/hzzt/internal/currency"
	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
	"github.com/JonathanRainee/hzzt/internal/platform/allocation"
	"github.com/JonathanRainee/hzzt/internal/platform/money"
)

// StaleAfter is how old a market price may be before it is reported as stale.
const StaleAfter = 30 * time.Minute

// MarketPrice is the latest provider price of an investment's instrument (P11/P12).
type MarketPrice struct {
	Price decimal.Decimal
	AsOf  time.Time
	// StaleAfter is how old the price may get before it is reported stale;
	// zero means the default (StaleAfter, 30 minutes). Providers differ: crypto
	// refreshes every few minutes, end-of-day stock prices only once a day.
	StaleAfter time.Duration
}

// BaseValues are the valuation figures converted to the space base currency.
type BaseValues struct {
	Currency     string          `json:"currency" example:"IDR"`
	MarketValue  decimal.Decimal `json:"marketValue" swaggertype:"string"`
	CostBasis    decimal.Decimal `json:"costBasis" swaggertype:"string"`
	UnrealizedPL decimal.Decimal `json:"unrealizedPl" swaggertype:"string"`
	// FxRate is the rate used: base-currency units per 1 quote-currency unit.
	FxRate decimal.Decimal `json:"fxRate" swaggertype:"string" example:"16000"`
}

// MarshalJSON renders the amounts with the base currency's display decimals (IDR and JPY: none,
// others: 2); the exchange rate keeps its precision.
func (b BaseValues) MarshalJSON() ([]byte, error) {
	type plain BaseValues
	return json.Marshal(struct {
		plain
		MarketValue  money.Amount `json:"marketValue" swaggertype:"string"`
		CostBasis    money.Amount `json:"costBasis" swaggertype:"string"`
		UnrealizedPL money.Amount `json:"unrealizedPl" swaggertype:"string"`
	}{plain(b), money.NewAmount(b.MarketValue, b.Currency), money.NewAmount(b.CostBasis, b.Currency), money.NewAmount(b.UnrealizedPL, b.Currency)})
}

// Valuation is the computed worth of one investment.
type Valuation struct {
	// Currency is the quote currency of the native figures below (not serialised separately).
	Currency string `json:"-"`

	Price       decimal.Decimal `json:"price" swaggertype:"string"`
	PriceSource string          `json:"priceSource" enums:"MARKET,MANUAL,MANUAL_VALUE"`
	PriceAsOf   *time.Time      `json:"priceAsOf"`
	IsStale     bool            `json:"isStale"`

	// Native figures, in the investment's quote currency.
	MarketValue  decimal.Decimal `json:"marketValue" swaggertype:"string"`
	CostBasis    decimal.Decimal `json:"costBasis" swaggertype:"string"`
	UnrealizedPL decimal.Decimal `json:"unrealizedPl" swaggertype:"string"`
	// UnrealizedPLPct is a percentage (12.5 means +12.5%); null when the cost basis is 0.
	UnrealizedPLPct *decimal.Decimal `json:"unrealizedPlPct" swaggertype:"string"`

	// Base holds the converted figures; null when no FX rate is available (see Warnings).
	Base     *BaseValues        `json:"base"`
	Warnings []currency.Warning `json:"warnings"`
}

// MarshalJSON renders the native figures with the quote currency's display decimals and the
// percentage with 2; the price keeps its precision (crypto needs many places).
func (v Valuation) MarshalJSON() ([]byte, error) {
	type plain Valuation
	return json.Marshal(struct {
		plain
		MarketValue     money.Amount `json:"marketValue" swaggertype:"string"`
		CostBasis       money.Amount `json:"costBasis" swaggertype:"string"`
		UnrealizedPL    money.Amount `json:"unrealizedPl" swaggertype:"string"`
		UnrealizedPLPct *money.Pct   `json:"unrealizedPlPct" swaggertype:"string"`
	}{plain(v), money.NewAmount(v.MarketValue, v.Currency), money.NewAmount(v.CostBasis, v.Currency),
		money.NewAmount(v.UnrealizedPL, v.Currency), money.PctPtr(v.UnrealizedPLPct)})
}

// Share is one person's part of a shared investment, in basis points of 10,000.
type Share struct {
	MyBps   int
	AllBps  []int // every non-removed participant, in the stable order the allocation uses
	MyIndex int   // position of this share in AllBps
}

// OwnershipFactor is the fraction of an investment that belongs to the viewer:
// 1 for an unshared investment, my_bps / 10,000 for a shared one.
func OwnershipFactor(sh *Share) decimal.Decimal {
	if sh == nil {
		return decimal.NewFromInt(1)
	}
	return decimal.NewFromInt(int64(sh.MyBps)).Div(decimal.NewFromInt(10000))
}

// Allocate turns the valuation of a whole investment into the viewer's share.
// Native figures scale by the ownership factor. Base-currency value and cost are
// each divided between all participants with the largest-remainder method at the
// base currency's minor units, so the parts add up to the total exactly; the
// share's profit/loss is value − cost, which then adds up exactly as well.
func Allocate(v Valuation, sh *Share, baseCurrency string) Valuation {
	if sh == nil {
		return v
	}
	f := OwnershipFactor(sh)
	out := v
	out.MarketValue = v.MarketValue.Mul(f)
	out.CostBasis = v.CostBasis.Mul(f)
	out.UnrealizedPL = out.MarketValue.Sub(out.CostBasis)
	if v.Base != nil {
		mu := money.MinorUnits(baseCurrency)
		mv := allocation.Split(v.Base.MarketValue, sh.AllBps, mu)[sh.MyIndex]
		cost := allocation.Split(v.Base.CostBasis, sh.AllBps, mu)[sh.MyIndex]
		b := *v.Base
		b.MarketValue, b.CostBasis, b.UnrealizedPL = mv, cost, mv.Sub(cost)
		out.Base = &b
	}
	return out
}

var hundred = decimal.NewFromInt(100)

// Value computes the valuation of inv.
//
//  1. price = the market price when price_mode is AUTO and one exists, else manual_price.
//  2. marketValue = manual_current_value when set, else quantity × price.
//  3. costBasis = quantity × avg_buy_price + fees.
//  4. unrealizedPl = marketValue − costBasis; unrealizedPlPct = pl / costBasis × 100.
//  5. conversion to the base currency: with auto FX off and a manual_fx_rate on the
//     investment, that rate; otherwise the space's ConversionContext.
//  6. Value returns the figures of the whole investment; Allocate scales them to
//     the viewer's share when the investment is shared.
func Value(inv sqlc.Investment, mp *MarketPrice, cc *currency.ConversionContext, now time.Time) Valuation {
	v := Valuation{Currency: strings.TrimSpace(inv.QuoteCurrency), PriceSource: SourceManual, Warnings: []currency.Warning{}}

	price := inv.ManualPrice
	if inv.PriceMode == PriceModeAuto && mp != nil {
		price = mp.Price
		v.PriceSource = SourceMarket
		asOf := mp.AsOf.UTC()
		v.PriceAsOf = &asOf
		limit := StaleAfter
		if mp.StaleAfter > 0 {
			limit = mp.StaleAfter
		}
		v.IsStale = now.Sub(mp.AsOf) > limit
		if v.IsStale { // a stale market price still counts (better than the typed one), flagged
			v.Warnings = append(v.Warnings, currency.Warning{
				Code: CodePriceStale, Label: inv.Name,
				Message: fmt.Sprintf("The market price of %s is older than %s", inv.Name, limit),
			})
		}
	}
	v.Price = price

	factor := decimal.NewFromInt(1)
	marketValue := inv.Quantity.Mul(price)
	if inv.ManualCurrentValue.Valid {
		marketValue = inv.ManualCurrentValue.Decimal
		v.PriceSource = SourceManualValue
		if inv.PriceMode == PriceModeAuto {
			v.Warnings = append(v.Warnings, currency.Warning{
				Code: CodeManualValueOver, Label: inv.Name,
				Message: inv.Name + " is priced by the market but a manual current value is set and wins: clear it to follow the market",
			})
		}
	}
	cost := inv.Quantity.Mul(inv.AvgBuyPrice).Add(inv.Fees)

	v.MarketValue = marketValue.Mul(factor)
	v.CostBasis = cost.Mul(factor)
	v.UnrealizedPL = v.MarketValue.Sub(v.CostBasis)
	if !v.CostBasis.IsZero() {
		pct := v.UnrealizedPL.Div(v.CostBasis).Mul(hundred)
		v.UnrealizedPLPct = &pct
	}

	quote := strings.TrimSpace(inv.QuoteCurrency)
	rate, err := conversionRate(inv, quote, cc)
	if err != nil {
		v.Warnings = append(v.Warnings, currency.Warning{
			Code: currency.CodeFXRateUnavailable, Label: inv.Name,
			Message: fmt.Sprintf("No %s→%s rate; %s is left out of base-currency totals", quote, cc.BaseCurrency, inv.Name),
		})
		return v
	}
	v.Base = &BaseValues{
		Currency:     strings.TrimSpace(cc.BaseCurrency),
		MarketValue:  v.MarketValue.Mul(rate),
		CostBasis:    v.CostBasis.Mul(rate),
		UnrealizedPL: v.UnrealizedPL.Mul(rate),
		FxRate:       rate,
	}
	return v
}

// conversionRate picks the quote→base rate for one investment.
func conversionRate(inv sqlc.Investment, quote string, cc *currency.ConversionContext) (decimal.Decimal, error) {
	base := strings.TrimSpace(cc.BaseCurrency)
	if strings.EqualFold(quote, base) {
		return decimal.NewFromInt(1), nil
	}
	if !cc.AutoFX && inv.ManualFxRate.Valid {
		return inv.ManualFxRate.Decimal, nil // the per-investment override, only in manual FX mode
	}
	return cc.Rate(quote, base)
}

// Summary totals a portfolio in the base currency.
type Summary struct {
	Currency     string          `json:"currency" example:"IDR"`
	TotalValue   decimal.Decimal `json:"totalValue" swaggertype:"string"`
	TotalCost    decimal.Decimal `json:"totalCost" swaggertype:"string"`
	UnrealizedPL decimal.Decimal `json:"unrealizedPl" swaggertype:"string"`
	// UnrealizedPLPct is a percentage; null when the total cost is 0.
	UnrealizedPLPct *decimal.Decimal   `json:"unrealizedPlPct" swaggertype:"string"`
	Warnings        []currency.Warning `json:"warnings"`
}

// MarshalJSON renders the totals with the base currency's display decimals and the percentage with 2.
func (s Summary) MarshalJSON() ([]byte, error) {
	type plain Summary
	return json.Marshal(struct {
		plain
		TotalValue      money.Amount `json:"totalValue" swaggertype:"string"`
		TotalCost       money.Amount `json:"totalCost" swaggertype:"string"`
		UnrealizedPL    money.Amount `json:"unrealizedPl" swaggertype:"string"`
		UnrealizedPLPct *money.Pct   `json:"unrealizedPlPct" swaggertype:"string"`
	}{plain(s), money.NewAmount(s.TotalValue, s.Currency), money.NewAmount(s.TotalCost, s.Currency),
		money.NewAmount(s.UnrealizedPL, s.Currency), money.PctPtr(s.UnrealizedPLPct)})
}

// Summarize adds up the base-currency figures of the valuations. Investments
// without a usable FX rate are left out and reported once each in Warnings.
func Summarize(baseCurrency string, vals []Valuation) Summary {
	s := Summary{Currency: strings.TrimSpace(baseCurrency), Warnings: []currency.Warning{}}
	seen := map[string]bool{}
	for _, v := range vals {
		if v.Base == nil {
			for _, w := range v.Warnings {
				if w.Code == currency.CodeFXRateUnavailable && !seen[w.Message] {
					seen[w.Message] = true
					s.Warnings = append(s.Warnings, w)
				}
			}
			continue
		}
		s.TotalValue = s.TotalValue.Add(v.Base.MarketValue)
		s.TotalCost = s.TotalCost.Add(v.Base.CostBasis)
		s.UnrealizedPL = s.UnrealizedPL.Add(v.Base.UnrealizedPL)
	}
	if !s.TotalCost.IsZero() {
		pct := s.UnrealizedPL.Div(s.TotalCost).Mul(hundred)
		s.UnrealizedPLPct = &pct
	}
	sort.SliceStable(s.Warnings, func(i, j int) bool { return s.Warnings[i].Message < s.Warnings[j].Message })
	return s
}
