package money

import (
	"encoding/json"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecimalAddition(t *testing.T) {
	a, err := decimal.NewFromString("0.1")
	require.NoError(t, err)
	b, err := decimal.NewFromString("0.2")
	require.NoError(t, err)
	assert.Equal(t, "0.3", a.Add(b).String())
}

func TestMinorUnits(t *testing.T) {
	assert.Equal(t, int32(0), MinorUnits("IDR"))
	assert.Equal(t, int32(0), MinorUnits("idr"))
	assert.Equal(t, int32(0), MinorUnits("JPY"))
	assert.Equal(t, int32(2), MinorUnits("USD"))
	assert.Equal(t, int32(3), MinorUnits("KWD"))
	assert.Equal(t, int32(2), MinorUnits("ZZZ"))
}

func TestRoundToMinor(t *testing.T) {
	d := decimal.RequireFromString("1234.5678")
	assert.Equal(t, "1235", RoundToMinor(d, "IDR").String())
	assert.Equal(t, "1234.57", RoundToMinor(d, "USD").String())
	assert.Equal(t, "2", RoundToMinor(decimal.RequireFromString("1.5"), "IDR").String())
}

func TestParse(t *testing.T) {
	for _, ok := range []string{"0", "134.26", "-5.5", "0.00000001"} {
		_, err := Parse(ok)
		assert.NoError(t, err, ok)
	}
	for _, bad := range []string{"", " 1", "1e3", "1,000", "1.", ".5", "abc", "+1"} {
		_, err := Parse(bad)
		assert.Error(t, err, bad)
	}
}

func TestJSONRoundTripIsString(t *testing.T) {
	type dto struct {
		Amount decimal.Decimal `json:"amount"`
	}
	b, err := json.Marshal(dto{Amount: decimal.RequireFromString("134.26")})
	require.NoError(t, err)
	assert.JSONEq(t, `{"amount":"134.26"}`, string(b))

	var out dto
	require.NoError(t, json.Unmarshal([]byte(`{"amount":"0.1"}`), &out))
	assert.Equal(t, "0.1", out.Amount.String())
}

func TestPctAlwaysHasTwoDecimals(t *testing.T) {
	for in, want := range map[string]string{
		"0": "0.00", "40": "40.00", "33.333333": "33.33", "66.666666": "66.67", "12.345": "12.35", "-12.345": "-12.35", "100": "100.00",
	} {
		b, err := json.Marshal(NewPct(decimal.RequireFromString(in)))
		require.NoError(t, err)
		assert.Equal(t, `"`+want+`"`, string(b), in)
	}
	var null *Pct
	b, _ := json.Marshal(null)
	assert.Equal(t, "null", string(b))
	var back Pct
	require.NoError(t, json.Unmarshal([]byte(`"12.50"`), &back))
	assert.True(t, back.Equal(decimal.RequireFromString("12.5")))
}

func TestAmountDisplayDecimals(t *testing.T) {
	cases := []struct{ in, cur, want string }{
		{"150000", "IDR", "150000"},
		{"150000.5", "IDR", "150001"}, // half away from zero
		{"149999.4999", "IDR", "149999"},
		{"-150000.5", "IDR", "-150001"},
		{"1234.5", "JPY", "1235"},
		{"12.5", "USD", "12.50"},
		{"12.345", "USD", "12.35"},
		{"-12.345", "EUR", "-12.35"},
		{"0.005", "GBP", "0.01"},
		{"0.004", "GBP", "0.00"},
		{"0", "IDR", "0"},
		{"0", "USD", "0.00"},
		{"1.2345", "KWD", "1.23"},  // others are 2 even where ISO says 3
		{"1500", "KRW", "1500.00"}, // and where ISO says 0 (open question Q1)
		{"1.5", "idr ", "2"},       // case and blanks
		{"2.5", "", "2.50"},
	}
	for _, c := range cases {
		b, err := json.Marshal(NewAmount(decimal.RequireFromString(c.in), c.cur))
		require.NoError(t, err)
		assert.Equal(t, `"`+c.want+`"`, string(b), "%s %s", c.in, c.cur)
	}
	var null *Amount
	b, _ := json.Marshal(AmountPtr(nil, "USD"))
	assert.Equal(t, "null", string(b))
	_ = null
	assert.Equal(t, int32(0), DisplayDecimals("JPY"))
	assert.Equal(t, int32(2), DisplayDecimals("USD"))
	// the amount keeps full precision until it is marshalled
	a := NewAmount(decimal.RequireFromString("0.004"), "USD").D.Add(decimal.RequireFromString("0.004"))
	assert.Equal(t, "0.01", NewAmount(a, "USD").String(), "totals are summed unrounded")
}
