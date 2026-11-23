package currency

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/JonathanRainee/hzzt/internal/platform/money"
)

// ErrRateUnavailable means no usable rate exists for the requested pair.
var ErrRateUnavailable = errors.New("fx rate unavailable")

const rateDivPrecision = 18

// ConversionContext is everything needed to convert money for one space.
// Build it once per request (Service.BuildContext) and pass it to aggregations.
type ConversionContext struct {
	BaseCurrency string
	// AutoFX selects live provider rates (true) or the space's manual rates (false).
	AutoFX bool
	// ManualRates holds the space's manual rates keyed by "BASE/QUOTE".
	ManualRates map[string]decimal.Decimal
	// Rates are live USD-based rates (units of QUOTE per 1 USD); USD itself is implicit.
	Rates map[string]decimal.Decimal
	// AsOf is the date of the newest live rate loaded (zero if none).
	AsOf time.Time
}

func pairKey(base, quote string) string { return base + "/" + quote }

func norm(c string) string { return strings.ToUpper(strings.TrimSpace(c)) }

// Rate returns how many `to` units one `from` unit is worth.
func (c *ConversionContext) Rate(from, to string) (decimal.Decimal, error) {
	from, to = norm(from), norm(to)
	if from == to {
		return decimal.NewFromInt(1), nil
	}
	if !c.AutoFX {
		if r, ok := c.ManualRates[pairKey(from, to)]; ok {
			return r, nil
		}
		if r, ok := c.ManualRates[pairKey(to, from)]; ok && r.IsPositive() {
			return decimal.NewFromInt(1).DivRound(r, rateDivPrecision), nil
		}
		return decimal.Zero, fmt.Errorf("%w: no manual rate for %s/%s", ErrRateUnavailable, from, to)
	}
	// Live rates are USD-based; a cross rate goes through USD.
	perUSD := func(code string) (decimal.Decimal, bool) {
		if code == "USD" {
			return decimal.NewFromInt(1), true
		}
		r, ok := c.Rates[code]
		return r, ok && r.IsPositive()
	}
	f, okF := perUSD(from)
	t, okT := perUSD(to)
	if !okF || !okT {
		return decimal.Zero, fmt.Errorf("%w: no live rate for %s/%s", ErrRateUnavailable, from, to)
	}
	return t.DivRound(f, rateDivPrecision), nil
}

// Convert converts amount from one currency to another using the context.
// The result is not rounded (rounding is for presentation and allocation only).
func Convert(amount decimal.Decimal, from, to string, cc *ConversionContext) (decimal.Decimal, error) {
	if norm(from) == norm(to) {
		return amount, nil
	}
	r, err := cc.Rate(from, to)
	if err != nil {
		return decimal.Zero, err
	}
	return amount.Mul(r), nil
}

// MoneyItem is one amount in its native currency.
type MoneyItem struct {
	Label    string
	Amount   decimal.Decimal
	Currency string
}

// BreakdownEntry totals one native currency.
type BreakdownEntry struct {
	Currency  string          `json:"currency"`
	Original  decimal.Decimal `json:"original"`
	Converted decimal.Decimal `json:"converted"`
}

// Warning reports a value that was left out of a total.
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Label   string `json:"label,omitempty"`
}

// CodeFXRateUnavailable is the warning code for items excluded for lack of a rate.
const CodeFXRateUnavailable = "FX_RATE_UNAVAILABLE"

// AggregateResult is a total in the base currency plus how it was made up.
type AggregateResult struct {
	Total     decimal.Decimal  `json:"total"`
	Currency  string           `json:"currency"`
	Breakdown []BreakdownEntry `json:"breakdown"`
	Warnings  []Warning        `json:"warnings"`
}

// MarshalJSON renders the amounts with the currencies' display decimals (IDR and JPY: none,
// others: 2): the total and each converted figure in the base currency, each original in its own.
func (r AggregateResult) MarshalJSON() ([]byte, error) {
	type entry struct {
		Currency  string       `json:"currency"`
		Original  money.Amount `json:"original" swaggertype:"string"`
		Converted money.Amount `json:"converted" swaggertype:"string"`
	}
	bd := make([]entry, 0, len(r.Breakdown))
	for _, e := range r.Breakdown {
		bd = append(bd, entry{e.Currency, money.NewAmount(e.Original, e.Currency), money.NewAmount(e.Converted, r.Currency)})
	}
	return json.Marshal(struct {
		Total     money.Amount `json:"total" swaggertype:"string"`
		Currency  string       `json:"currency"`
		Breakdown []entry      `json:"breakdown"`
		Warnings  []Warning    `json:"warnings"`
	}{money.NewAmount(r.Total, r.Currency), r.Currency, bd, r.Warnings})
}

// Aggregate converts every item to the base currency and sums them. An item
// without a usable rate is excluded and reported in Warnings; it never fails the total.
func Aggregate(items []MoneyItem, cc *ConversionContext) AggregateResult {
	res := AggregateResult{Total: decimal.Zero, Currency: cc.BaseCurrency, Breakdown: []BreakdownEntry{}, Warnings: []Warning{}}
	by := map[string]*BreakdownEntry{}
	for _, it := range items {
		cur := norm(it.Currency)
		conv, err := Convert(it.Amount, cur, cc.BaseCurrency, cc)
		if err != nil {
			res.Warnings = append(res.Warnings, Warning{
				Code: CodeFXRateUnavailable, Label: it.Label,
				Message: fmt.Sprintf("No %s→%s rate; %s excluded from the total", cur, cc.BaseCurrency, labelOr(it.Label, cur)),
			})
			continue
		}
		e := by[cur]
		if e == nil {
			e = &BreakdownEntry{Currency: cur}
			by[cur] = e
		}
		e.Original = e.Original.Add(it.Amount)
		e.Converted = e.Converted.Add(conv)
		res.Total = res.Total.Add(conv)
	}
	for _, e := range by {
		res.Breakdown = append(res.Breakdown, *e)
	}
	sort.Slice(res.Breakdown, func(i, j int) bool { return res.Breakdown[i].Currency < res.Breakdown[j].Currency })
	return res
}

func labelOr(label, fallback string) string {
	if label != "" {
		return label
	}
	return fallback + " amount"
}
