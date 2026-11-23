package investments

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
	"github.com/JonathanRainee/hzzt/internal/platform/apperror"
	"github.com/JonathanRainee/hzzt/internal/platform/httpx"
)

func decodeCreate(t *testing.T, body string) (Spec, error) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	in, err := httpx.DecodeDiscriminated(req, "assetType", Variants())
	if err != nil {
		return Spec{}, err
	}
	return in.spec("IDR")
}

func fieldsOf(t *testing.T, err error) map[string]any {
	t.Helper()
	ae, ok := apperror.As(err)
	require.True(t, ok, "want an AppError, got %v", err)
	require.Equal(t, apperror.CodeValidationFailed, ae.Code, ae.Message)
	return ae.Details
}

func codeOf(t *testing.T, err error) string {
	t.Helper()
	ae, ok := apperror.As(err)
	require.True(t, ok, "%v", err)
	return ae.Code
}

func TestCreateIDStockDerivesQuantityFromLots(t *testing.T) {
	s, err := decodeCreate(t, `{"assetType":"ID_STOCK","name":"Bank Central Asia","symbol":"bbca","avgBuyPrice":"9200","manualPrice":"9500","details":{"broker":"Stockbit","lots":2}}`)
	require.NoError(t, err)
	assert.True(t, d("200").Equal(s.Quantity), "BBCA with 2 lots → quantity 200")
	assert.Equal(t, "IDR", s.QuoteCurrency)
	assert.Equal(t, "BBCA", *s.Symbol)
	assert.Equal(t, PriceModeManual, s.PriceMode)
	assert.Nil(t, s.EstimatedAnnualChangePct, "no assumed return")
	assert.Equal(t, "🇮🇩", s.Icon)
	assert.Equal(t, idStockDetails{Broker: strp("Stockbit"), Lots: jnp("2")}, s.Details)

	s, err = decodeCreate(t, `{"assetType":"ID_STOCK","name":"X","symbol":"X","details":{"lots":0}}`)
	require.NoError(t, err)
	assert.True(t, s.Quantity.IsZero(), "0 lots is allowed")
}

func strp(s string) *string     { return &s }
func jnp(s string) *json.Number { n := json.Number(s); return &n }

func TestCreateValidationPerType(t *testing.T) {
	cases := []struct {
		name, body, field string
	}{
		{"unknown asset type", `{"assetType":"BOND","name":"x"}`, "assetType"},
		{"missing asset type", `{"name":"x"}`, "assetType"},
		{"missing name", `{"assetType":"CRYPTO","symbol":"BTC","quantity":"1"}`, "name"},
		{"blank name", `{"assetType":"CRYPTO","name":"  ","symbol":"BTC","quantity":"1"}`, "name"},
		{"long name", `{"assetType":"CRYPTO","name":"` + strings.Repeat("x", 61) + `","symbol":"BTC","quantity":"1"}`, "name"},
		{"crypto: missing quantity", `{"assetType":"CRYPTO","name":"Bitcoin","symbol":"BTC"}`, "quantity"},
		{"crypto: missing symbol", `{"assetType":"CRYPTO","name":"Bitcoin","quantity":"1"}`, "symbol"},
		{"crypto: negative quantity", `{"assetType":"CRYPTO","name":"Bitcoin","symbol":"BTC","quantity":"-1"}`, "quantity"},
		{"crypto: 19 decimals", `{"assetType":"CRYPTO","name":"Bitcoin","symbol":"BTC","quantity":"0.1234567890123456789"}`, "quantity"},
		{"crypto: staked > quantity", `{"assetType":"CRYPTO","name":"Ethereum","symbol":"ETH","quantity":"2","details":{"stakedQuantity":"2.5"}}`, "details.stakedQuantity"},
		{"crypto: unknown detail", `{"assetType":"CRYPTO","name":"Ethereum","symbol":"ETH","quantity":"2","details":{"lots":2}}`, "details"},
		{"crypto: wrong quote currency", `{"assetType":"CRYPTO","name":"Bitcoin","symbol":"BTC","quantity":"1","quoteCurrency":"IDR"}`, "quoteCurrency"},
		{"id stock: missing lots", `{"assetType":"ID_STOCK","name":"BBCA","symbol":"BBCA","details":{}}`, "details.lots"},
		{"id stock: no details", `{"assetType":"ID_STOCK","name":"BBCA","symbol":"BBCA"}`, "details.lots"},
		{"id stock: negative lots", `{"assetType":"ID_STOCK","name":"BBCA","symbol":"BBCA","details":{"lots":-1}}`, "details.lots"},
		{"id stock: lots with 2 decimals", `{"assetType":"ID_STOCK","name":"BBCA","symbol":"BBCA","details":{"lots":1.55}}`, "details.lots"},
		{"crypto: 9 decimals", `{"assetType":"CRYPTO","name":"Bitcoin","symbol":"BTC","quantity":"0.123456789"}`, "quantity"},
		{"us stock: 2 decimals", `{"assetType":"US_STOCK","name":"Apple","symbol":"AAPL","quantity":"1.25","details":{"exchange":"NASDAQ"}}`, "quantity"},
		{"crypto: staked with 9 decimals", `{"assetType":"CRYPTO","name":"Ethereum","symbol":"ETH","quantity":"2","details":{"stakedQuantity":"0.123456789"}}`, "details.stakedQuantity"},
		{"id stock: quantity is derived, not sent", `{"assetType":"ID_STOCK","name":"BBCA","symbol":"BBCA","quantity":"200","details":{"lots":2}}`, "assetType"},
		{"id stock: wrong quote", `{"assetType":"ID_STOCK","name":"BBCA","symbol":"BBCA","quoteCurrency":"USD","details":{"lots":1}}`, "quoteCurrency"},
		{"us stock: missing exchange", `{"assetType":"US_STOCK","name":"Apple","symbol":"AAPL","quantity":"1","details":{}}`, "details.exchange"},
		{"mutual fund: missing fund type", `{"assetType":"MUTUAL_FUND","name":"Fund","quantity":"1","details":{}}`, "details.fundType"},
		{"mutual fund: bad fund type", `{"assetType":"MUTUAL_FUND","name":"Fund","quantity":"1","details":{"fundType":"CRYPTO"}}`, "details.fundType"},
		{"mutual fund: bad nav date", `{"assetType":"MUTUAL_FUND","name":"Fund","quantity":"1","details":{"fundType":"EQUITY","navDate":"31/01/2026"}}`, "details.navDate"},
		{"mutual fund: unsupported currency", `{"assetType":"MUTUAL_FUND","name":"Fund","quantity":"1","quoteCurrency":"XXX","details":{"fundType":"EQUITY"}}`, "quoteCurrency"},
		{"etf: missing exchange", `{"assetType":"GLOBAL_ETF","name":"VOO","symbol":"VOO","quantity":"1","details":{}}`, "details.exchange"},
		{"etf: expense ratio over 100", `{"assetType":"GLOBAL_ETF","name":"VOO","symbol":"VOO","quantity":"1","details":{"exchange":"NYSE","expenseRatioPct":"101"}}`, "details.expenseRatioPct"},
		{"negative price", `{"assetType":"US_STOCK","name":"A","symbol":"A","quantity":"1","manualPrice":"-1","details":{"exchange":"NYSE"}}`, "manualPrice"},
		{"negative fees", `{"assetType":"US_STOCK","name":"A","symbol":"A","quantity":"1","fees":"-1","details":{"exchange":"NYSE"}}`, "fees"},
		{"too many decimals", `{"assetType":"US_STOCK","name":"A","symbol":"A","quantity":"1","avgBuyPrice":"1.123456789","details":{"exchange":"NYSE"}}`, "avgBuyPrice"},
		{"huge value", `{"assetType":"US_STOCK","name":"A","symbol":"A","quantity":"1","manualCurrentValue":"100000000000000000000","details":{"exchange":"NYSE"}}`, "manualCurrentValue"},
		{"zero fx rate", `{"assetType":"US_STOCK","name":"A","symbol":"A","quantity":"1","manualFxRate":"0","details":{"exchange":"NYSE"}}`, "manualFxRate"},
		{"change below -100", `{"assetType":"US_STOCK","name":"A","symbol":"A","quantity":"1","estimatedAnnualChangePct":"-100.5","details":{"exchange":"NYSE"}}`, "estimatedAnnualChangePct"},
		{"change above 1000", `{"assetType":"US_STOCK","name":"A","symbol":"A","quantity":"1","estimatedAnnualChangePct":"1000.1","details":{"exchange":"NYSE"}}`, "estimatedAnnualChangePct"},
		{"bad price mode", `{"assetType":"US_STOCK","name":"A","symbol":"A","quantity":"1","priceMode":"LIVE","details":{"exchange":"NYSE"}}`, "priceMode"},
		{"bad purchase date", `{"assetType":"US_STOCK","name":"A","symbol":"A","quantity":"1","purchaseDate":"yesterday","details":{"exchange":"NYSE"}}`, "purchaseDate"},
		{"bad icon", `{"assetType":"US_STOCK","name":"A","symbol":"A","quantity":"1","icon":"","details":{"exchange":"NYSE"}}`, "icon"},
		{"long symbol", `{"assetType":"US_STOCK","name":"A","symbol":"` + strings.Repeat("A", 21) + `","quantity":"1","details":{"exchange":"NYSE"}}`, "symbol"},
		{"unknown top-level field", `{"assetType":"US_STOCK","name":"A","symbol":"A","quantity":"1","isShared":true,"details":{"exchange":"NYSE"}}`, "assetType"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := decodeCreate(t, c.body)
			require.Error(t, err)
			ae, ok := apperror.As(err)
			require.True(t, ok)
			require.Equal(t, apperror.CodeValidationFailed, ae.Code, ae.Message)
			if len(ae.Details) > 0 { // malformed JSON (unknown field, wrong type) has only a message
				assert.Contains(t, ae.Details, c.field)
			} else {
				assert.Contains(t, ae.Message, "Malformed JSON")
			}
		})
	}
}

func TestCreateValidPerType(t *testing.T) {
	t.Run("crypto with staking", func(t *testing.T) {
		s, err := decodeCreate(t, `{"assetType":"CRYPTO","name":"Ethereum","symbol":"eth","quantity":"2","avgBuyPrice":"3000","manualPrice":"3500","priceMode":"AUTO","details":{"exchangeOrWallet":"Ledger","network":"Ethereum","stakedQuantity":"1.5"}}`)
		require.NoError(t, err)
		assert.Equal(t, "USD", s.QuoteCurrency)
		assert.Equal(t, PriceModeAuto, s.PriceMode, "AUTO is accepted for crypto")
		assert.Equal(t, "ETH", *s.Symbol)
		assert.Equal(t, "🪙", s.Icon)
		assert.True(t, d("2").Equal(s.Quantity))
		assert.True(t, d("1.5").Equal(*s.Details.(cryptoDetails).StakedQuantity))

		_, err = decodeCreate(t, `{"assetType":"CRYPTO","name":"ETH","symbol":"ETH","quantity":"2","details":{"stakedQuantity":"2"}}`)
		assert.NoError(t, err, "staked == quantity is fine")
	})
	t.Run("us stock", func(t *testing.T) {
		s, err := decodeCreate(t, `{"assetType":"US_STOCK","name":"Apple","symbol":"AAPL","quantity":"12.5","fees":"1.25","manualCurrentValue":"3000","manualFxRate":"16000","purchaseDate":"2026-01-31","details":{"exchange":"NASDAQ","platform":"IBKR"}}`)
		require.NoError(t, err)
		assert.Equal(t, "USD", s.QuoteCurrency)
		assert.True(t, d("3000").Equal(*s.ManualCurrentValue))
		assert.True(t, d("16000").Equal(*s.ManualFxRate))
		assert.Equal(t, "2026-01-31", s.PurchaseDate.Format("2006-01-02"))
	})
	t.Run("mutual fund: quote currency defaults to the space base, or is chosen", func(t *testing.T) {
		s, err := decodeCreate(t, `{"assetType":"MUTUAL_FUND","name":"Fund","quantity":"1000.5","avgBuyPrice":"1500","manualPrice":"1600","details":{"fundManager":"Bareksa","fundType":"MONEY_MARKET","navDate":"2026-11-17"}}`)
		require.NoError(t, err)
		assert.Equal(t, "IDR", s.QuoteCurrency)
		assert.Nil(t, s.Symbol, "mutual funds have no symbol")
		s, err = decodeCreate(t, `{"assetType":"MUTUAL_FUND","name":"Fund","quantity":"1","quoteCurrency":"usd","details":{"fundType":"EQUITY"}}`)
		require.NoError(t, err)
		assert.Equal(t, "USD", s.QuoteCurrency)
	})
	t.Run("global ETF", func(t *testing.T) {
		s, err := decodeCreate(t, `{"assetType":"GLOBAL_ETF","name":"Vanguard S&P 500","symbol":"voo","quantity":"2","priceMode":"AUTO","details":{"exchange":"NYSE","expenseRatioPct":"0.03","assetClass":"Equity"}}`)
		require.NoError(t, err)
		assert.Equal(t, "USD", s.QuoteCurrency)
		assert.Equal(t, PriceModeAuto, s.PriceMode)
	})
	t.Run("estimated annual change bounds", func(t *testing.T) {
		for _, v := range []string{"-100", "0", "1000", "7.25"} {
			_, err := decodeCreate(t, `{"assetType":"US_STOCK","name":"A","symbol":"A","quantity":"1","estimatedAnnualChangePct":"`+v+`","details":{"exchange":"NYSE"}}`)
			assert.NoError(t, err, v)
		}
	})
}

func TestAutoPriceOnlyWhereSupported(t *testing.T) {
	for _, body := range []string{
		`{"assetType":"ID_STOCK","name":"BBCA","symbol":"BBCA","priceMode":"AUTO","details":{"lots":1}}`,
		`{"assetType":"MUTUAL_FUND","name":"Fund","quantity":"1","priceMode":"AUTO","details":{"fundType":"EQUITY"}}`,
	} {
		_, err := decodeCreate(t, body)
		require.Error(t, err)
		assert.Equal(t, CodeAutoPriceNotSupported, codeOf(t, err), body)
	}
	for _, body := range []string{
		`{"assetType":"CRYPTO","name":"B","symbol":"BTC","quantity":"1","priceMode":"AUTO"}`,
		`{"assetType":"US_STOCK","name":"A","symbol":"A","quantity":"1","priceMode":"AUTO","details":{"exchange":"NYSE"}}`,
		`{"assetType":"GLOBAL_ETF","name":"V","symbol":"V","quantity":"1","priceMode":"AUTO","details":{"exchange":"NYSE"}}`,
	} {
		_, err := decodeCreate(t, body)
		assert.NoError(t, err, body)
	}
}

func TestMetasMirrorTheInputs(t *testing.T) {
	require.Len(t, Metas, 5)
	for _, m := range Metas {
		_, ok := Variants()[m.AssetType]
		assert.True(t, ok, m.AssetType)
		assert.NotNil(t, newDetails(m.AssetType), m.AssetType)
		assert.NotEmpty(t, m.DetailFields)
		if m.AssetType == TypeCrypto { // D-B4: crypto is priced by the market unless told otherwise
			assert.Equal(t, PriceModeAuto, m.Defaults.PriceMode)
		} else {
			assert.Equal(t, PriceModeManual, m.Defaults.PriceMode)
		}
		assert.Nil(t, m.Defaults.EstimatedAnnualChangePct, "no assumed return")
		// auto pricing is only advertised where creating AUTO works
		_, err := buildSpec(m.AssetType, "n", &base{PriceMode: strp("AUTO"), Symbol: strp("S")}, dp("1"), validDetails(m.AssetType), "IDR")
		if m.SupportsAutoPrice {
			assert.NoError(t, err, m.AssetType)
		} else {
			assert.Equal(t, CodeAutoPriceNotSupported, codeOf(t, err), m.AssetType)
		}
		if m.QuoteCurrency != nil {
			assert.False(t, m.QuoteCurrencyFree)
		} else {
			assert.True(t, m.QuoteCurrencyFree)
		}
		for _, f := range m.DetailFields {
			assert.NotEmpty(t, f.Name)
			assert.NotEmpty(t, f.Label)
		}
	}
	id, _ := MetaFor(TypeIDStock)
	assert.Equal(t, QuantityLots, id.QuantityMode)
	require.NotNil(t, id.LotSize)
	assert.Equal(t, 100, *id.LotSize)
	mf, _ := MetaFor(TypeMutualFund)
	assert.False(t, mf.SymbolRequired)
	_, ok := MetaFor("BOND")
	assert.False(t, ok)
}

func validDetails(assetType string) any {
	switch assetType {
	case TypeIDStock:
		return idStockDetails{Lots: jnp("1")}
	case TypeUSStock:
		return usStockDetails{Exchange: "NYSE"}
	case TypeMutualFund:
		return mutualFundDetails{FundType: "EQUITY"}
	case TypeGlobalETF:
		return globalETFDetails{Exchange: "NYSE"}
	}
	return cryptoDetails{}
}

// --- PATCH ---------------------------------------------------------------------

func rowFor(t *testing.T, body string) sqlc.Investment {
	t.Helper()
	s, err := decodeCreate(t, body)
	require.NoError(t, err)
	details, err := DetailsJSON(s.Details)
	require.NoError(t, err)
	r := sqlc.Investment{
		AssetType: s.AssetType, PriceMode: s.PriceMode, Name: s.Name, Symbol: s.Symbol, Icon: s.Icon,
		QuoteCurrency: s.QuoteCurrency, Quantity: s.Quantity, AvgBuyPrice: s.AvgBuyPrice, ManualPrice: s.ManualPrice,
		Fees: s.Fees, PurchaseDate: s.PurchaseDate, EstimatedAnnualChangePct: toNullDecimal(s.EstimatedAnnualChangePct), Details: details,
	}
	if s.ManualCurrentValue != nil {
		r.ManualCurrentValue = nd(s.ManualCurrentValue.String())
	}
	return r
}

func patch(t *testing.T, body string) PatchInput {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(body))
	p, err := httpx.DecodeAndValidate[PatchInput](req)
	require.NoError(t, err)
	return p
}

func TestPatch(t *testing.T) {
	bbca := rowFor(t, `{"assetType":"ID_STOCK","name":"BBCA","symbol":"BBCA","avgBuyPrice":"9200","manualPrice":"9500","manualCurrentValue":"2000000","details":{"broker":"Stockbit","lots":2}}`)
	btc := rowFor(t, `{"assetType":"CRYPTO","name":"Bitcoin","symbol":"BTC","quantity":"2","details":{"stakedQuantity":"1"}}`)

	t.Run("partial: untouched fields stay", func(t *testing.T) {
		s, err := patch(t, `{"manualPrice":"9800"}`).apply(bbca, "IDR")
		require.NoError(t, err)
		assert.True(t, d("9800").Equal(s.ManualPrice))
		assert.Equal(t, "BBCA", s.Name)
		assert.True(t, d("200").Equal(s.Quantity))
		assert.True(t, d("2000000").Equal(*s.ManualCurrentValue))
	})
	t.Run("null clears manualCurrentValue", func(t *testing.T) {
		s, err := patch(t, `{"manualCurrentValue":null}`).apply(bbca, "IDR")
		require.NoError(t, err)
		assert.Nil(t, s.ManualCurrentValue)
	})
	t.Run("details replace the object and re-derive quantity", func(t *testing.T) {
		s, err := patch(t, `{"details":{"lots":5}}`).apply(bbca, "IDR")
		require.NoError(t, err)
		assert.True(t, d("500").Equal(s.Quantity))
		assert.Nil(t, s.Details.(idStockDetails).Broker, "details are replaced, not merged")
	})
	t.Run("asset type is immutable", func(t *testing.T) {
		_, err := patch(t, `{"assetType":"US_STOCK"}`).apply(bbca, "IDR")
		assert.Contains(t, fieldsOf(t, err), "assetType")
		_, err = patch(t, `{"assetType":"ID_STOCK","name":"BBCA 2"}`).apply(bbca, "IDR")
		assert.NoError(t, err, "re-sending the same type is fine")
	})
	t.Run("id stock quantity comes from lots", func(t *testing.T) {
		_, err := patch(t, `{"quantity":"300"}`).apply(bbca, "IDR")
		assert.Contains(t, fieldsOf(t, err), "quantity")
	})
	t.Run("crypto: staked must stay within quantity", func(t *testing.T) {
		_, err := patch(t, `{"quantity":"0.5"}`).apply(btc, "IDR")
		assert.Contains(t, fieldsOf(t, err), "details.stakedQuantity", "shrinking quantity below the stored stake")
		_, err = patch(t, `{"details":{"stakedQuantity":"3"}}`).apply(btc, "IDR")
		assert.Contains(t, fieldsOf(t, err), "details.stakedQuantity")
		s, err := patch(t, `{"quantity":"5","details":{"stakedQuantity":"3"}}`).apply(btc, "IDR")
		require.NoError(t, err)
		assert.True(t, d("5").Equal(s.Quantity))
	})
	t.Run("auto price rules apply on update too", func(t *testing.T) {
		_, err := patch(t, `{"priceMode":"AUTO"}`).apply(bbca, "IDR")
		assert.Equal(t, CodeAutoPriceNotSupported, codeOf(t, err))
		s, err := patch(t, `{"priceMode":"AUTO"}`).apply(btc, "IDR")
		require.NoError(t, err)
		assert.Equal(t, PriceModeAuto, s.PriceMode)
	})
	t.Run("quote currency is fixed except for mutual funds", func(t *testing.T) {
		_, err := patch(t, `{"quoteCurrency":"EUR"}`).apply(btc, "IDR")
		assert.Contains(t, fieldsOf(t, err), "quoteCurrency")
		mf := rowFor(t, `{"assetType":"MUTUAL_FUND","name":"Fund","quantity":"1","details":{"fundType":"EQUITY"}}`)
		s, err := patch(t, `{"quoteCurrency":"eur"}`).apply(mf, "IDR")
		require.NoError(t, err)
		assert.Equal(t, "EUR", s.QuoteCurrency)
	})
	t.Run("symbol can be cleared only where optional", func(t *testing.T) {
		_, err := patch(t, `{"symbol":null}`).apply(btc, "IDR")
		assert.Contains(t, fieldsOf(t, err), "symbol")
		mf := rowFor(t, `{"assetType":"MUTUAL_FUND","name":"Fund","symbol":"FND","quantity":"1","details":{"fundType":"EQUITY"}}`)
		s, err := patch(t, `{"symbol":null}`).apply(mf, "IDR")
		require.NoError(t, err)
		assert.Nil(t, s.Symbol)
	})
	t.Run("validation", func(t *testing.T) {
		for name, body := range map[string]string{
			"negative price": `{"manualPrice":"-1"}`, "bad fx rate": `{"manualFxRate":"0"}`, "bad change": `{"estimatedAnnualChangePct":"5000"}`,
			"bad date": `{"purchaseDate":"x"}`, "bad details": `{"details":{"lots":-3}}`, "unknown detail": `{"details":{"exchange":"NYSE"}}`,
			"blank name": `{"name":"  "}`,
		} {
			_, err := patch(t, body).apply(bbca, "IDR")
			require.Error(t, err, name)
			assert.Equal(t, apperror.CodeValidationFailed, codeOf(t, err), name)
		}
	})
}

func TestCryptoWithoutPriceModeDefaultsToAuto(t *testing.T) {
	spec, err := buildSpec(TypeCrypto, "Bitcoin", &base{Symbol: strp("BTC")}, dp("1"), validDetails(TypeCrypto), "IDR")
	require.NoError(t, err)
	assert.Equal(t, PriceModeAuto, spec.PriceMode)
	assert.True(t, spec.AutoDefault)

	spec, err = buildSpec(TypeCrypto, "Bitcoin", &base{Symbol: strp("BTC"), PriceMode: strp("MANUAL")}, dp("1"), validDetails(TypeCrypto), "IDR")
	require.NoError(t, err)
	assert.Equal(t, PriceModeManual, spec.PriceMode)
	assert.False(t, spec.AutoDefault)

	spec, err = buildSpec(TypeUSStock, "Apple", &base{Symbol: strp("AAPL")}, dp("1"), validDetails(TypeUSStock), "IDR")
	require.NoError(t, err)
	assert.Equal(t, PriceModeManual, spec.PriceMode, "other types stay manual by default")
}

func TestQuantityDecimalLimits(t *testing.T) {
	ok := func(body string) Spec {
		s, err := decodeCreate(t, body)
		require.NoError(t, err)
		return s
	}
	assert.True(t, d("0.12345678").Equal(ok(`{"assetType":"CRYPTO","name":"Bitcoin","symbol":"BTC","quantity":"0.12345678"}`).Quantity), "crypto keeps 8 decimals")
	assert.True(t, d("1.5").Equal(ok(`{"assetType":"US_STOCK","name":"Apple","symbol":"AAPL","quantity":"1.5","details":{"exchange":"NASDAQ"}}`).Quantity), "US stock keeps 1 decimal")
	assert.True(t, d("50").Equal(ok(`{"assetType":"ID_STOCK","name":"BBCA","symbol":"BBCA","details":{"lots":0.5}}`).Quantity), "0.5 lot = 50 shares")
	assert.True(t, d("250").Equal(ok(`{"assetType":"ID_STOCK","name":"BBCA","symbol":"BBCA","details":{"lots":"2.5"}}`).Quantity), "lots may also arrive as a string")
	assert.Equal(t, int32(8), QuantityScaleFor(TypeCrypto))
	assert.Equal(t, int32(1), QuantityScaleFor(TypeUSStock))
	assert.Equal(t, int32(18), QuantityScaleFor(TypeGlobalETF))
}

func TestTradeQuantityLimits(t *testing.T) {
	leg := func(q string) Leg { return Leg{Quantity: d(q), UnitPrice: d("1"), Fee: d("0")} }
	ok := func(assetType, q string) bool {
		return checkLeg(sqlc.Investment{AssetType: assetType}, leg(q)) == nil
	}
	assert.True(t, ok(TypeCrypto, "0.00000001"))
	assert.False(t, ok(TypeCrypto, "0.000000001"), "crypto: 9 decimals")
	assert.True(t, ok(TypeUSStock, "2.5"))
	assert.False(t, ok(TypeUSStock, "2.55"), "US stock: 2 decimals")
	assert.True(t, ok(TypeIDStock, "50"), "0.5 lot")
	assert.True(t, ok(TypeIDStock, "250"), "2.5 lots")
	assert.False(t, ok(TypeIDStock, "55"), "0.55 lot is two decimals")
	assert.False(t, ok(TypeIDStock, "50.5"), "shares are whole")
}
