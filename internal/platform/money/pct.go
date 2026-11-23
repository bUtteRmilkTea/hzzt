package money

import (
	"encoding/json"

	"github.com/shopspring/decimal"
)

// Pct is a percentage (12.5 means 12.5%) that always serialises as a quoted
// decimal with exactly 2 places ("12.50"), rounded half away from zero. Rounding
// happens only when marshalling; the value keeps full precision.
type Pct struct{ decimal.Decimal }

// NewPct wraps d as a percentage.
func NewPct(d decimal.Decimal) Pct { return Pct{d} }

// PctPtr wraps an optional percentage; nil stays nil (JSON null).
func PctPtr(d *decimal.Decimal) *Pct {
	if d == nil {
		return nil
	}
	p := NewPct(*d)
	return &p
}

// String is the 2-decimal form.
func (p Pct) String() string { return p.Decimal.Round(2).StringFixed(2) }

// MarshalJSON implements json.Marshaler.
func (p Pct) MarshalJSON() ([]byte, error) { return json.Marshal(p.String()) }

// UnmarshalJSON implements json.Unmarshaler.
func (p *Pct) UnmarshalJSON(b []byte) error { return p.Decimal.UnmarshalJSON(b) }
