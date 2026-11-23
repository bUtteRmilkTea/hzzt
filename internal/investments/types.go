// Package investments implements the five investment types with their
// type-specific fields, and the valuation engine (price, market value, cost
// basis, unrealized P/L, conversion to the space base currency).
package investments

import "github.com/JonathanRainee/hzzt/internal/platform/apperror"

// Asset types.
const (
	TypeCrypto     = "CRYPTO"
	TypeIDStock    = "ID_STOCK"
	TypeUSStock    = "US_STOCK"
	TypeMutualFund = "MUTUAL_FUND"
	TypeGlobalETF  = "GLOBAL_ETF"
)

// Price modes.
const (
	PriceModeAuto   = "AUTO"
	PriceModeManual = "MANUAL"
)

// Price sources reported by the valuation.
const (
	SourceMarket      = "MARKET"
	SourceManual      = "MANUAL"
	SourceManualValue = "MANUAL_VALUE"
)

// Quantity modes: how the client expresses the position size.
const (
	QuantityUnits = "QUANTITY" // client sends quantity
	QuantityLots  = "LOTS"     // quantity is derived: lots × LotSize
)

// LotSize is the number of shares in one Indonesian stock lot.
const LotSize = 100

// Error codes owned by this package (see plan Appendix B).
const (
	CodeAutoPriceNotSupported = "AUTO_PRICE_NOT_SUPPORTED"
	CodeInvestmentNotFound    = "INVESTMENT_NOT_FOUND"
	CodePriceModeNotAuto      = "PRICE_MODE_NOT_AUTO"

	// Warning codes of a valuation (not errors).
	CodeInstrumentNotFound = "INSTRUMENT_NOT_FOUND" // same value as marketdata.CodeInstrumentNotFound
	CodePriceNotLinked     = "PRICE_NOT_LINKED"
	CodePriceStale         = "PRICE_STALE"
	CodeManualValueOver    = "MANUAL_VALUE_OVERRIDES_MARKET"

	CodeInstrumentNotLinked = "INSTRUMENT_NOT_LINKED"
)

var (
	errInvestmentNF     = apperror.New(404, CodeInvestmentNotFound, "Investment not found")
	errAutoNotSupported = apperror.Unprocessable(CodeAutoPriceNotSupported, "This asset type does not support automatic pricing")
)

// FieldMeta describes one input field for dynamic forms.
type FieldMeta struct {
	Name     string   `json:"name" example:"stakedQuantity"`
	Label    string   `json:"label" example:"Staked quantity"`
	Kind     string   `json:"kind" enums:"TEXT,INTEGER,DECIMAL,DATE,ENUM"`
	Required bool     `json:"required"`
	Options  []string `json:"options,omitempty"`
}

// TypeMeta is the metadata the frontend uses to build the per-type form.
type TypeMeta struct {
	AssetType         string       `json:"assetType"`
	Label             string       `json:"label"`
	QuoteCurrency     *string      `json:"quoteCurrency"` // null: chosen by the client (default: the space base currency)
	QuoteCurrencyFree bool         `json:"quoteCurrencyEditable"`
	SupportsAutoPrice bool         `json:"supportsAutoPrice"`
	QuantityMode      string       `json:"quantityMode" enums:"QUANTITY,LOTS"`
	LotSize           *int         `json:"lotSize,omitempty"`
	SymbolRequired    bool         `json:"symbolRequired"`
	DefaultIcon       string       `json:"defaultIcon"`
	Defaults          TypeDefaults `json:"defaults"`
	// DetailFields are the fields of the `details` object.
	DetailFields []FieldMeta `json:"detailFields"`
}

// TypeDefaults are the defaults applied when a field is omitted.
type TypeDefaults struct {
	PriceMode string `json:"priceMode"`
	// EstimatedAnnualChangePct is always null: there is no assumed return (the field is kept so clients can rely on it).
	EstimatedAnnualChangePct *string `json:"estimatedAnnualChangePct"`
	Fees                     string  `json:"fees"`
}

// FundTypes are the allowed mutual fund types.
var FundTypes = []string{"MONEY_MARKET", "FIXED_INCOME", "EQUITY", "BALANCED", "OTHER"}

func sp(s string) *string { return &s }
func ip(i int) *int       { return &i }

// Metas is the static type metadata; it mirrors the per-type detail structs in inputs.go.
var Metas = []TypeMeta{
	{
		AssetType: TypeCrypto, Label: "Crypto", QuoteCurrency: sp("USD"), SupportsAutoPrice: true,
		QuantityMode: QuantityUnits, SymbolRequired: true, DefaultIcon: "🪙",
		DetailFields: []FieldMeta{
			{Name: "exchangeOrWallet", Label: "Exchange or wallet", Kind: "TEXT"},
			{Name: "network", Label: "Network", Kind: "TEXT"},
			{Name: "stakedQuantity", Label: "Staked quantity", Kind: "DECIMAL"},
		},
	},
	{
		AssetType: TypeIDStock, Label: "Indonesian stock", QuoteCurrency: sp("IDR"), SupportsAutoPrice: false,
		QuantityMode: QuantityLots, LotSize: ip(LotSize), SymbolRequired: true, DefaultIcon: "🇮🇩",
		DetailFields: []FieldMeta{
			{Name: "broker", Label: "Broker", Kind: "TEXT"},
			{Name: "lots", Label: "Lots", Kind: "INTEGER", Required: true},
		},
	},
	{
		AssetType: TypeUSStock, Label: "US stock", QuoteCurrency: sp("USD"), SupportsAutoPrice: true,
		QuantityMode: QuantityUnits, SymbolRequired: true, DefaultIcon: "🇺🇸",
		DetailFields: []FieldMeta{
			{Name: "exchange", Label: "Exchange", Kind: "TEXT", Required: true},
			{Name: "platform", Label: "Platform", Kind: "TEXT"},
		},
	},
	{
		AssetType: TypeMutualFund, Label: "Mutual fund", QuoteCurrency: nil, QuoteCurrencyFree: true, SupportsAutoPrice: false,
		QuantityMode: QuantityUnits, SymbolRequired: false, DefaultIcon: "💼",
		DetailFields: []FieldMeta{
			{Name: "fundManager", Label: "Fund manager", Kind: "TEXT"},
			{Name: "fundType", Label: "Fund type", Kind: "ENUM", Required: true, Options: FundTypes},
			{Name: "navDate", Label: "NAV date", Kind: "DATE"},
		},
	},
	{
		AssetType: TypeGlobalETF, Label: "Global ETF", QuoteCurrency: sp("USD"), SupportsAutoPrice: true,
		QuantityMode: QuantityUnits, SymbolRequired: true, DefaultIcon: "🌐",
		DetailFields: []FieldMeta{
			{Name: "exchange", Label: "Exchange", Kind: "TEXT", Required: true},
			{Name: "platform", Label: "Platform", Kind: "TEXT"},
			{Name: "expenseRatioPct", Label: "Expense ratio (%)", Kind: "DECIMAL"},
			{Name: "assetClass", Label: "Asset class", Kind: "TEXT"},
		},
	},
}

func init() {
	for i := range Metas {
		Metas[i].Defaults = TypeDefaults{PriceMode: PriceModeManual, Fees: "0"}
		if Metas[i].AssetType == TypeCrypto { // D-B4: priced by the market unless the client says otherwise
			Metas[i].Defaults.PriceMode = PriceModeAuto
		}
	}
}

// MetaFor returns the metadata of an asset type.
func MetaFor(assetType string) (TypeMeta, bool) {
	for _, m := range Metas {
		if m.AssetType == assetType {
			return m, true
		}
	}
	return TypeMeta{}, false
}
