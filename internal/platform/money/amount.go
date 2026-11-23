package money

import (
	"encoding/json"
	"strings"

	"github.com/shopspring/decimal"
)

// DisplayDecimals is how many decimal places an amount of the currency shows in API
// responses: IDR and JPY have none, every other currency 2. It is a presentation rule
// and deliberately differs from MinorUnits, which the ledger and allocations rely on.
func DisplayDecimals(currency string) int32 {
	switch strings.ToUpper(strings.TrimSpace(currency)) {
	case "IDR", "JPY":
		return 0
	}
	return 2
}

// Display rounds d (half away from zero) to the currency's display decimals.
func Display(d decimal.Decimal, currency string) decimal.Decimal {
	return d.Round(DisplayDecimals(currency))
}

// Amount is a money amount in a currency that serialises as a quoted decimal with the
// currency's display decimals ("150000" for IDR and JPY, "12.50" otherwise). Rounding
// happens only when marshalling; D keeps full precision, so totals can be summed unrounded.
type Amount struct {
	D   decimal.Decimal
	Cur string
}

// NewAmount builds an Amount.
func NewAmount(d decimal.Decimal, currency string) Amount {
	return Amount{D: d, Cur: strings.TrimSpace(currency)}
}

// AmountPtr wraps an optional amount; nil stays nil (JSON null).
func AmountPtr(d *decimal.Decimal, currency string) *Amount {
	if d == nil {
		return nil
	}
	a := NewAmount(*d, currency)
	return &a
}

// String is the display form.
func (a Amount) String() string {
	n := DisplayDecimals(a.Cur)
	return a.D.Round(n).StringFixed(n)
}

// MarshalJSON implements json.Marshaler.
func (a Amount) MarshalJSON() ([]byte, error) { return json.Marshal(a.String()) }

// UnmarshalJSON implements json.Unmarshaler (the currency is not carried in the JSON).
func (a *Amount) UnmarshalJSON(b []byte) error { return a.D.UnmarshalJSON(b) }
