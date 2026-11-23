package investments

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
	"github.com/JonathanRainee/hzzt/internal/platform/apperror"
	"github.com/JonathanRainee/hzzt/internal/platform/httpx"
	"github.com/JonathanRainee/hzzt/internal/platform/money"
)

// Spec is the validated, normalized shape of an investment ready to be stored.
type Spec struct {
	AssetType                string
	PriceMode                string
	Name                     string
	Symbol                   *string
	Icon                     string
	QuoteCurrency            string
	Quantity                 decimal.Decimal
	AvgBuyPrice              decimal.Decimal
	ManualPrice              decimal.Decimal
	Fees                     decimal.Decimal
	ManualCurrentValue       *decimal.Decimal
	ManualFxRate             *decimal.Decimal
	PurchaseDate             *time.Time
	EstimatedAnnualChangePct decimal.Decimal
	// Wrapper is the exchange, broker or platform the position is held on (grouping key); nil when unknown.
	Wrapper *string
	// Details is one of the *Details structs below.
	Details any
	// InstrumentID is the market instrument the client picked from search
	// (CRYPTO, US_STOCK and GLOBAL_ETF); nil means "resolve from the symbol when AUTO".
	InstrumentID *uuid.UUID
	// Funding, when set, pays for the initial position from a wallet (a BUY).
	Funding *Funding
	// AutoDefault marks a price mode the server chose (a CRYPTO created without priceMode is AUTO):
	// if its coin cannot be linked, Create falls back to MANUAL with a PRICE_NOT_LINKED warning
	// instead of failing. An explicit priceMode from the client never falls back.
	AutoDefault bool
}

// Funding asks for the initial position to be paid from a wallet. The wallet is
// debited quantity × avgBuyPrice + fees (converted to the wallet's currency, or
// the given amount) and a BUY titled "Initial position {symbol}" is recorded.
type Funding struct {
	WalletID uuid.UUID        `json:"walletId" swaggertype:"string"`
	Amount   *decimal.Decimal `json:"amount,omitempty" swaggertype:"string" example:"1000000"`
}

// --- per-type details ----------------------------------------------------------

type cryptoDetails struct {
	ExchangeOrWallet *string          `json:"exchangeOrWallet,omitempty" validate:"omitempty,max=60"`
	Network          *string          `json:"network,omitempty" validate:"omitempty,max=40"`
	StakedQuantity   *decimal.Decimal `json:"stakedQuantity,omitempty" swaggertype:"string"`
}

type idStockDetails struct {
	Broker *string `json:"broker,omitempty" validate:"omitempty,max=60"`
	Lots   *int64  `json:"lots" validate:"required,min=0,max=1000000000"`
}

type usStockDetails struct {
	Exchange string  `json:"exchange" validate:"required,min=1,max=20"`
	Platform *string `json:"platform,omitempty" validate:"omitempty,max=60"`
}

type mutualFundDetails struct {
	FundManager *string `json:"fundManager,omitempty" validate:"omitempty,max=80"`
	FundType    string  `json:"fundType" validate:"required,oneof=MONEY_MARKET FIXED_INCOME EQUITY BALANCED OTHER"`
	NavDate     *string `json:"navDate,omitempty" validate:"omitempty,max=10"`
}

type globalETFDetails struct {
	Exchange        string           `json:"exchange" validate:"required,min=1,max=20"`
	Platform        *string          `json:"platform,omitempty" validate:"omitempty,max=60"`
	ExpenseRatioPct *decimal.Decimal `json:"expenseRatioPct,omitempty" swaggertype:"string"`
	AssetClass      *string          `json:"assetClass,omitempty" validate:"omitempty,max=40"`
}

// newDetails returns a pointer to an empty details struct of the asset type.
func newDetails(assetType string) any {
	switch assetType {
	case TypeCrypto:
		return &cryptoDetails{}
	case TypeIDStock:
		return &idStockDetails{}
	case TypeUSStock:
		return &usStockDetails{}
	case TypeMutualFund:
		return &mutualFundDetails{}
	case TypeGlobalETF:
		return &globalETFDetails{}
	}
	return nil
}

// decodeDetails strictly decodes and validates a details object for the asset type.
func decodeDetails(assetType string, raw json.RawMessage) (any, error) {
	d := newDetails(assetType)
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		raw = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(d); err != nil {
		return nil, apperror.Validation("Validation failed", map[string]any{"details": "malformed: " + err.Error()})
	}
	if err := httpx.Validate(d); err != nil {
		return nil, prefixDetails(err)
	}
	return deref(d), nil
}

func prefixDetails(err error) error {
	ae, ok := apperror.As(err)
	if !ok || ae.Code != apperror.CodeValidationFailed {
		return err
	}
	f := make(map[string]any, len(ae.Details))
	for k, v := range ae.Details {
		f["details."+k] = v
	}
	return apperror.Validation("Validation failed", f)
}

// deref turns *T into T so Spec.Details holds plain values.
func deref(p any) any {
	switch v := p.(type) {
	case *cryptoDetails:
		return *v
	case *idStockDetails:
		return *v
	case *usStockDetails:
		return *v
	case *mutualFundDetails:
		return *v
	case *globalETFDetails:
		return *v
	}
	return p
}

// parseStoredDetails reads the stored JSON back into the typed value (lenient).
func parseStoredDetails(assetType string, raw []byte) (any, error) {
	d := newDetails(assetType)
	if d == nil {
		return nil, fmt.Errorf("unknown asset type %q", assetType)
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, d); err != nil {
			return nil, fmt.Errorf("decode %s details: %w", assetType, err)
		}
	}
	return deref(d), nil
}

// --- shared fields ---------------------------------------------------------------

// base holds the fields every asset type shares. It is embedded in each create
// input and carries no validate tags (they would surface as "base.<field>");
// buildSpec validates these fields by hand.
type base struct {
	PriceMode     *string `json:"priceMode"`
	Symbol        *string `json:"symbol"`
	Icon          *string `json:"icon"`
	QuoteCurrency *string `json:"quoteCurrency"`
	// Wrapper is the exchange, broker or platform holding the position (1 to 60 characters). When omitted it is derived from details (exchangeOrWallet, broker or platform).
	Wrapper                  *string          `json:"wrapper" example:"Binance"`
	AvgBuyPrice              *decimal.Decimal `json:"avgBuyPrice" swaggertype:"string" example:"9200"`
	ManualPrice              *decimal.Decimal `json:"manualPrice" swaggertype:"string" example:"9500"`
	Fees                     *decimal.Decimal `json:"fees" swaggertype:"string"`
	ManualCurrentValue       *decimal.Decimal `json:"manualCurrentValue" swaggertype:"string"`
	PurchaseDate             *string          `json:"purchaseDate" example:"2026-01-31"`
	EstimatedAnnualChangePct *decimal.Decimal `json:"estimatedAnnualChangePct" swaggertype:"string" example:"5.5"`
	ManualFxRate             *decimal.Decimal `json:"manualFxRate" swaggertype:"string" example:"16000"`
	// Funding pays the initial position from a wallet (optional).
	Funding *Funding `json:"funding"`
}

// Create inputs, one per asset type. The request is decoded in two passes keyed on assetType.
type cryptoInput struct {
	AssetType    string           `json:"assetType"`
	Name         string           `json:"name" validate:"required,min=1,max=120"`
	Quantity     *decimal.Decimal `json:"quantity" validate:"required" swaggertype:"string"`
	InstrumentID *uuid.UUID       `json:"instrumentId" swaggertype:"string"`
	Details      cryptoDetails    `json:"details"`
	base
}

type idStockInput struct {
	AssetType string         `json:"assetType"`
	Name      string         `json:"name" validate:"required,min=1,max=120"`
	Details   idStockDetails `json:"details"`
	base
}

type usStockInput struct {
	AssetType    string           `json:"assetType"`
	Name         string           `json:"name" validate:"required,min=1,max=120"`
	Quantity     *decimal.Decimal `json:"quantity" validate:"required" swaggertype:"string"`
	InstrumentID *uuid.UUID       `json:"instrumentId" swaggertype:"string"`
	Details      usStockDetails   `json:"details"`
	base
}

type mutualFundInput struct {
	AssetType string            `json:"assetType"`
	Name      string            `json:"name" validate:"required,min=1,max=120"`
	Quantity  *decimal.Decimal  `json:"quantity" validate:"required" swaggertype:"string"`
	Details   mutualFundDetails `json:"details"`
	base
}

type globalETFInput struct {
	AssetType    string           `json:"assetType"`
	Name         string           `json:"name" validate:"required,min=1,max=120"`
	Quantity     *decimal.Decimal `json:"quantity" validate:"required" swaggertype:"string"`
	InstrumentID *uuid.UUID       `json:"instrumentId" swaggertype:"string"`
	Details      globalETFDetails `json:"details"`
	base
}

// CreateInput is implemented by the per-type create inputs.
type CreateInput interface {
	spec(baseCurrency string) (Spec, error)
}

func (in *cryptoInput) spec(bc string) (Spec, error) {
	s, err := buildSpec(TypeCrypto, in.Name, &in.base, in.Quantity, in.Details, bc)
	s.InstrumentID = in.InstrumentID
	return s, err
}
func (in *idStockInput) spec(bc string) (Spec, error) {
	return buildSpec(TypeIDStock, in.Name, &in.base, nil, in.Details, bc)
}
func (in *usStockInput) spec(bc string) (Spec, error) {
	s, err := buildSpec(TypeUSStock, in.Name, &in.base, in.Quantity, in.Details, bc)
	s.InstrumentID = in.InstrumentID
	return s, err
}
func (in *mutualFundInput) spec(bc string) (Spec, error) {
	return buildSpec(TypeMutualFund, in.Name, &in.base, in.Quantity, in.Details, bc)
}
func (in *globalETFInput) spec(bc string) (Spec, error) {
	s, err := buildSpec(TypeGlobalETF, in.Name, &in.base, in.Quantity, in.Details, bc)
	s.InstrumentID = in.InstrumentID
	return s, err
}

// Variants maps assetType to its create input for httpx.DecodeDiscriminated.
func Variants() map[string]func() CreateInput {
	return map[string]func() CreateInput{
		TypeCrypto:     func() CreateInput { return &cryptoInput{} },
		TypeIDStock:    func() CreateInput { return &idStockInput{} },
		TypeUSStock:    func() CreateInput { return &usStockInput{} },
		TypeMutualFund: func() CreateInput { return &mutualFundInput{} },
		TypeGlobalETF:  func() CreateInput { return &globalETFInput{} },
	}
}

// --- validation helpers ------------------------------------------------------------

var maxValue = decimal.New(1, 20)

// checkDec validates a non-negative decimal with at most `scale` decimal places
// (and strictly positive when positive is set).
func checkDec(f map[string]any, field string, d *decimal.Decimal, scale int32, positive bool) {
	switch {
	case d == nil:
	case d.IsNegative():
		f[field] = "must not be negative"
	case positive && !d.IsPositive():
		f[field] = "must be greater than 0"
	case !d.Round(scale).Equal(*d):
		f[field] = fmt.Sprintf("must have at most %d decimal places", scale)
	case d.GreaterThanOrEqual(maxValue):
		f[field] = "is too large"
	}
}

func fieldsErr(f map[string]any) error {
	if len(f) == 0 {
		return nil
	}
	return apperror.Validation("Validation failed", f)
}

func parseDate(f map[string]any, field string, s *string) *time.Time {
	if s == nil {
		return nil
	}
	t, err := time.Parse("2006-01-02", strings.TrimSpace(*s))
	if err != nil {
		f[field] = "must be a date like 2026-01-31"
		return nil
	}
	return &t
}

func cleanSymbol(f map[string]any, s *string) *string {
	if s == nil {
		return nil
	}
	v := strings.ToUpper(strings.TrimSpace(*s))
	if v == "" {
		return nil
	}
	if utf8.RuneCountInString(v) > 20 {
		f["symbol"] = "must be at most 20 characters"
	}
	return &v
}

func cleanName(f map[string]any, name string) string {
	n := strings.TrimSpace(name)
	if n == "" || utf8.RuneCountInString(n) > 60 {
		f["name"] = "must be 1 to 60 characters"
	}
	return n
}

func cleanIcon(f map[string]any, icon *string, def string) string {
	if icon == nil {
		return def
	}
	i := strings.TrimSpace(*icon)
	if i == "" || utf8.RuneCountInString(i) > 16 {
		f["icon"] = "must be an emoji (1 to 16 characters)"
		return def
	}
	return i
}

// NormalizeWrapper trims a wrapper name and collapses inner whitespace runs to one space.
func NormalizeWrapper(s string) string { return strings.Join(strings.Fields(s), " ") }

// WrapperKey is the grouping key of a wrapper: case and whitespace insensitive.
func WrapperKey(s string) string { return strings.ToLower(NormalizeWrapper(s)) }

// cleanWrapper validates an explicit wrapper; blank counts as not given.
func cleanWrapper(f map[string]any, w *string) *string {
	if w == nil {
		return nil
	}
	v := NormalizeWrapper(*w)
	if v == "" {
		return nil
	}
	if utf8.RuneCountInString(v) > 60 {
		f["wrapper"] = "must be at most 60 characters"
	}
	return &v
}

// DetailsWrapper derives the wrapper from the type-specific details: exchangeOrWallet
// (CRYPTO), broker (ID_STOCK) or platform (US_STOCK, GLOBAL_ETF; their `exchange` is the
// listing venue, not where the position is held). MUTUAL_FUND has none.
func DetailsWrapper(details any) *string {
	var raw *string
	switch d := details.(type) {
	case cryptoDetails:
		raw = d.ExchangeOrWallet
	case idStockDetails:
		raw = d.Broker
	case usStockDetails:
		raw = d.Platform
	case globalETFDetails:
		raw = d.Platform
	}
	if raw == nil {
		return nil
	}
	v := NormalizeWrapper(*raw)
	if v == "" {
		return nil
	}
	return &v
}

func quoteCurrency(f map[string]any, meta TypeMeta, given *string, baseCurrency string) string {
	if meta.QuoteCurrency != nil { // fixed by the asset type
		if given != nil && !strings.EqualFold(strings.TrimSpace(*given), *meta.QuoteCurrency) {
			f["quoteCurrency"] = "must be " + *meta.QuoteCurrency + " for " + meta.AssetType
		}
		return *meta.QuoteCurrency
	}
	if given == nil {
		return strings.TrimSpace(baseCurrency)
	}
	c := strings.ToUpper(strings.TrimSpace(*given))
	if !money.IsSupportedCurrency(c) {
		f["quoteCurrency"] = "must be a supported ISO 4217 currency code"
	}
	return c
}

func dOrZero(d *decimal.Decimal) decimal.Decimal {
	if d == nil {
		return decimal.Zero
	}
	return *d
}

// buildSpec validates a create input and produces its Spec.
func buildSpec(assetType, name string, b *base, quantity *decimal.Decimal, details any, baseCurrency string) (Spec, error) {
	meta, _ := MetaFor(assetType)
	f := map[string]any{}

	s := Spec{
		AssetType: assetType, PriceMode: PriceModeManual, Name: cleanName(f, name),
		Symbol: cleanSymbol(f, b.Symbol), Icon: cleanIcon(f, b.Icon, meta.DefaultIcon),
		QuoteCurrency: quoteCurrency(f, meta, b.QuoteCurrency, baseCurrency),
		AvgBuyPrice:   dOrZero(b.AvgBuyPrice), ManualPrice: dOrZero(b.ManualPrice), Fees: dOrZero(b.Fees),
		ManualCurrentValue: b.ManualCurrentValue, ManualFxRate: b.ManualFxRate,
		PurchaseDate:             parseDate(f, "purchaseDate", b.PurchaseDate),
		EstimatedAnnualChangePct: decimal.RequireFromString(DefaultEstimatedAnnualChangePct),
		Details:                  details,
		Wrapper:                  cleanWrapper(f, b.Wrapper),
	}
	if b.PriceMode != nil {
		if *b.PriceMode != PriceModeAuto && *b.PriceMode != PriceModeManual {
			f["priceMode"] = "must be one of: AUTO MANUAL"
		}
		s.PriceMode = *b.PriceMode
	} else if meta.Defaults.PriceMode == PriceModeAuto {
		s.PriceMode, s.AutoDefault = PriceModeAuto, true // D-B4: crypto is priced by the market unless told otherwise
	}
	if b.EstimatedAnnualChangePct != nil {
		s.EstimatedAnnualChangePct = *b.EstimatedAnnualChangePct
	}
	if quantity != nil {
		s.Quantity = *quantity
	}
	if b.Funding != nil {
		if b.Funding.WalletID == uuid.Nil {
			f["funding.walletId"] = "is required"
		}
		checkDec(f, "funding.amount", b.Funding.Amount, 8, true)
		s.Funding = b.Funding
	}
	if err := checkMoneyFields(f, &s, b, quantity); err != nil {
		return Spec{}, err
	}
	return finalize(s, f)
}

// checkMoneyFields validates magnitudes and scales of all numeric inputs.
func checkMoneyFields(f map[string]any, s *Spec, b *base, quantity *decimal.Decimal) error {
	checkDec(f, "quantity", quantity, 18, false)
	checkDec(f, "avgBuyPrice", b.AvgBuyPrice, 8, false)
	checkDec(f, "manualPrice", b.ManualPrice, 8, false)
	checkDec(f, "fees", b.Fees, 8, false)
	checkDec(f, "manualCurrentValue", b.ManualCurrentValue, 8, false)
	checkDec(f, "manualFxRate", b.ManualFxRate, 12, true)
	if p := b.EstimatedAnnualChangePct; p != nil {
		if p.LessThan(decimal.NewFromInt(-100)) || p.GreaterThan(decimal.NewFromInt(1000)) {
			f["estimatedAnnualChangePct"] = "must be between -100 and 1000"
		} else if !p.Round(4).Equal(*p) {
			f["estimatedAnnualChangePct"] = "must have at most 4 decimal places"
		}
	}
	return nil
}

// finalize derives computed fields and enforces the cross-field rules.
// Field problems found so far arrive in f; business rules (422) come after validation (400).
func finalize(s Spec, f map[string]any) (Spec, error) {
	switch d := s.Details.(type) {
	case idStockDetails:
		if d.Lots != nil {
			s.Quantity = decimal.NewFromInt(*d.Lots).Mul(decimal.NewFromInt(LotSize)) // quantity = lots × 100
		}
	case cryptoDetails:
		if d.StakedQuantity != nil {
			checkDec(f, "details.stakedQuantity", d.StakedQuantity, 18, false)
			if _, bad := f["details.stakedQuantity"]; !bad && d.StakedQuantity.GreaterThan(s.Quantity) {
				f["details.stakedQuantity"] = "must not exceed quantity"
			}
		}
	case mutualFundDetails:
		if d.NavDate != nil {
			parseDate(f, "details.navDate", d.NavDate)
		}
	case globalETFDetails:
		if d.ExpenseRatioPct != nil {
			if d.ExpenseRatioPct.IsNegative() || d.ExpenseRatioPct.GreaterThan(decimal.NewFromInt(100)) {
				f["details.expenseRatioPct"] = "must be between 0 and 100"
			}
		}
	}
	if s.Wrapper == nil {
		s.Wrapper = DetailsWrapper(s.Details)
	}
	meta, _ := MetaFor(s.AssetType)
	if meta.SymbolRequired && s.Symbol == nil {
		f["symbol"] = "is required for " + s.AssetType
	}
	if err := fieldsErr(f); err != nil {
		return Spec{}, err
	}
	if s.PriceMode == PriceModeAuto && !meta.SupportsAutoPrice {
		return Spec{}, errAutoNotSupported
	}
	return s, nil
}

// --- PATCH ---------------------------------------------------------------------------

// nullable decimal that distinguishes "absent" from an explicit null.
type nullDec struct {
	Set   bool
	Value *decimal.Decimal
}

func (n *nullDec) UnmarshalJSON(b []byte) error {
	n.Set = true
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return nil
	}
	var d decimal.Decimal
	if err := json.Unmarshal(b, &d); err != nil {
		return err
	}
	n.Value = &d
	return nil
}

type nullStr struct {
	Set   bool
	Value *string
}

func (n *nullStr) UnmarshalJSON(b []byte) error {
	n.Set = true
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	n.Value = &s
	return nil
}

// PatchInput is a partial update. assetType is immutable: it may be sent only if unchanged.
type PatchInput struct {
	AssetType                *string          `json:"assetType"`
	PriceMode                *string          `json:"priceMode" validate:"omitempty,oneof=AUTO MANUAL"`
	Name                     *string          `json:"name" validate:"omitempty,min=1,max=120"`
	Symbol                   nullStr          `json:"symbol" swaggertype:"string"`
	Icon                     *string          `json:"icon" validate:"omitempty,max=64"`
	QuoteCurrency            *string          `json:"quoteCurrency" validate:"omitempty,max=8"`
	Quantity                 *decimal.Decimal `json:"quantity" swaggertype:"string"`
	AvgBuyPrice              *decimal.Decimal `json:"avgBuyPrice" swaggertype:"string"`
	ManualPrice              *decimal.Decimal `json:"manualPrice" swaggertype:"string"`
	Fees                     *decimal.Decimal `json:"fees" swaggertype:"string"`
	ManualCurrentValue       nullDec          `json:"manualCurrentValue" swaggertype:"string"`
	PurchaseDate             nullStr          `json:"purchaseDate" swaggertype:"string"`
	EstimatedAnnualChangePct *decimal.Decimal `json:"estimatedAnnualChangePct" swaggertype:"string"`
	ManualFxRate             nullDec          `json:"manualFxRate" swaggertype:"string"`
	// Wrapper sets the exchange, broker or platform holding the position; null re-derives it from details.
	Wrapper nullStr `json:"wrapper" swaggertype:"string"`
	// InstrumentID relinks an auto-priced investment to an instrument picked from search.
	InstrumentID *uuid.UUID      `json:"instrumentId" swaggertype:"string"`
	Details      json.RawMessage `json:"details" swaggertype:"object"`
}

// specFromRow rebuilds a Spec from a stored investment.
func specFromRow(inv sqlc.Investment) (Spec, error) {
	d, err := parseStoredDetails(inv.AssetType, inv.Details)
	if err != nil {
		return Spec{}, err
	}
	s := Spec{
		AssetType: inv.AssetType, PriceMode: inv.PriceMode, Name: inv.Name, Symbol: inv.Symbol, Icon: inv.Icon,
		QuoteCurrency: strings.TrimSpace(inv.QuoteCurrency), Quantity: inv.Quantity, AvgBuyPrice: inv.AvgBuyPrice,
		ManualPrice: inv.ManualPrice, Fees: inv.Fees, PurchaseDate: inv.PurchaseDate,
		EstimatedAnnualChangePct: inv.EstimatedAnnualChangePct, Details: d, Wrapper: inv.Wrapper,
	}
	if inv.ManualCurrentValue.Valid {
		v := inv.ManualCurrentValue.Decimal
		s.ManualCurrentValue = &v
	}
	if inv.ManualFxRate.Valid {
		v := inv.ManualFxRate.Decimal
		s.ManualFxRate = &v
	}
	return s, nil
}

// apply merges a patch into the stored spec and re-validates the result.
func (p PatchInput) apply(inv sqlc.Investment, baseCurrency string) (Spec, error) {
	s, err := specFromRow(inv)
	if err != nil {
		return Spec{}, err
	}
	meta, _ := MetaFor(s.AssetType)
	f := map[string]any{}

	if p.AssetType != nil && *p.AssetType != s.AssetType {
		f["assetType"] = "cannot be changed"
	}
	if p.PriceMode != nil {
		s.PriceMode = *p.PriceMode
	}
	if p.InstrumentID != nil {
		if m, _ := MetaFor(s.AssetType); !m.SupportsAutoPrice {
			f["instrumentId"] = "is only supported for auto-priced asset types (CRYPTO, US_STOCK, GLOBAL_ETF)"
		}
		s.InstrumentID = p.InstrumentID
	}
	if p.Name != nil {
		s.Name = cleanName(f, *p.Name)
	}
	if p.Symbol.Set {
		s.Symbol = cleanSymbol(f, p.Symbol.Value)
	}
	if p.Icon != nil {
		s.Icon = cleanIcon(f, p.Icon, s.Icon)
	}
	if p.QuoteCurrency != nil {
		s.QuoteCurrency = quoteCurrency(f, meta, p.QuoteCurrency, baseCurrency)
	}
	if p.Quantity != nil {
		if meta.QuantityMode == QuantityLots {
			f["quantity"] = "is derived from details.lots for " + s.AssetType
		} else {
			checkDec(f, "quantity", p.Quantity, 18, false)
			s.Quantity = *p.Quantity
		}
	}
	if p.AvgBuyPrice != nil {
		s.AvgBuyPrice = *p.AvgBuyPrice
	}
	if p.ManualPrice != nil {
		s.ManualPrice = *p.ManualPrice
	}
	if p.Fees != nil {
		s.Fees = *p.Fees
	}
	if p.ManualCurrentValue.Set {
		s.ManualCurrentValue = p.ManualCurrentValue.Value
	}
	if p.ManualFxRate.Set {
		s.ManualFxRate = p.ManualFxRate.Value
	}
	if p.PurchaseDate.Set {
		s.PurchaseDate = parseDate(f, "purchaseDate", p.PurchaseDate.Value)
	}
	if p.EstimatedAnnualChangePct != nil {
		s.EstimatedAnnualChangePct = *p.EstimatedAnnualChangePct
	}
	checkDec(f, "avgBuyPrice", p.AvgBuyPrice, 8, false)
	checkDec(f, "manualPrice", p.ManualPrice, 8, false)
	checkDec(f, "fees", p.Fees, 8, false)
	checkDec(f, "manualCurrentValue", p.ManualCurrentValue.Value, 8, false)
	checkDec(f, "manualFxRate", p.ManualFxRate.Value, 12, true)
	if v := p.EstimatedAnnualChangePct; v != nil {
		if v.LessThan(decimal.NewFromInt(-100)) || v.GreaterThan(decimal.NewFromInt(1000)) {
			f["estimatedAnnualChangePct"] = "must be between -100 and 1000"
		} else if !v.Round(4).Equal(*v) {
			f["estimatedAnnualChangePct"] = "must have at most 4 decimal places"
		}
	}
	if len(bytes.TrimSpace(p.Details)) > 0 && string(bytes.TrimSpace(p.Details)) != "null" {
		d, err := decodeDetails(s.AssetType, p.Details)
		if err != nil {
			if ae, ok := apperror.As(err); ok {
				for k, v := range ae.Details {
					f[k] = v
				}
			} else {
				return Spec{}, err
			}
		} else {
			if !p.Wrapper.Set && !equalPtr(DetailsWrapper(s.Details), DetailsWrapper(d)) {
				s.Wrapper = nil // the broker/exchange in details changed: follow it
			}
			s.Details = d
		}
	}
	if p.Wrapper.Set {
		s.Wrapper = cleanWrapper(f, p.Wrapper.Value)
	}
	return finalize(s, f)
}

// DetailsJSON renders the typed details as stored/returned JSON.
func DetailsJSON(d any) (json.RawMessage, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func equalPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
