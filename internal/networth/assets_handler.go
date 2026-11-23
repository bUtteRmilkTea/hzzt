package networth

import (
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/JonathanRainee/hzzt/internal/auth"
	"github.com/JonathanRainee/hzzt/internal/currency"
	"github.com/JonathanRainee/hzzt/internal/platform/httpx"
	"github.com/JonathanRainee/hzzt/internal/platform/money"
	"github.com/JonathanRainee/hzzt/internal/spaces"
)

// WalletLineDTO is one wallet of the asset page.
type WalletLineDTO struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	Type     string    `json:"type" enums:"BANK,E_WALLET,CASH,OTHER"`
	Currency string    `json:"currency" example:"USD"`
	// Balance is in the wallet's currency, BaseBalance in the space's base currency (null without an FX rate).
	Balance     money.Amount  `json:"balance" swaggertype:"string"`
	BaseBalance *money.Amount `json:"baseBalance" swaggertype:"string"`
	// PctOfTotal is the wallet's share of the cash total; the shares add up to exactly 100.00.
	PctOfTotal *money.Pct `json:"pctOfTotal" swaggertype:"string"`
}

// CashSectionDTO is the cash section.
type CashSectionDTO struct {
	Total         money.Amount    `json:"total" swaggertype:"string"`
	PctOfNetWorth *money.Pct      `json:"pctOfNetWorth" swaggertype:"string"`
	Wallets       []WalletLineDTO `json:"wallets"`
}

// CategoryLineDTO is one investment asset type.
type CategoryLineDTO struct {
	AssetType       string       `json:"assetType" enums:"CRYPTO,ID_STOCK,US_STOCK,MUTUAL_FUND,GLOBAL_ETF"`
	Label           string       `json:"label" example:"Crypto"`
	Value           money.Amount `json:"value" swaggertype:"string"`
	UnrealizedPL    money.Amount `json:"unrealizedPl" swaggertype:"string"`
	UnrealizedPLPct *money.Pct   `json:"unrealizedPlPct" swaggertype:"string"`
	PctOfTotal      *money.Pct   `json:"pctOfTotal" swaggertype:"string"`
}

// InvestmentsSectionDTO is the investments section.
type InvestmentsSectionDTO struct {
	Total           money.Amount      `json:"total" swaggertype:"string"`
	Cost            money.Amount      `json:"cost" swaggertype:"string"`
	UnrealizedPL    money.Amount      `json:"unrealizedPl" swaggertype:"string"`
	UnrealizedPLPct *money.Pct        `json:"unrealizedPlPct" swaggertype:"string"`
	Categories      []CategoryLineDTO `json:"categories"`
}

// AssetLineDTO is one fixed asset, money owed or debt.
type AssetLineDTO struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	Currency string    `json:"currency" example:"IDR"`
	// Value is in the asset's currency, BaseValue in the space's base currency (null without an FX rate).
	Value      money.Amount  `json:"value" swaggertype:"string"`
	BaseValue  *money.Amount `json:"baseValue" swaggertype:"string"`
	PctOfTotal *money.Pct    `json:"pctOfTotal" swaggertype:"string"`
}

// AssetSectionDTO is a manual-asset section.
type AssetSectionDTO struct {
	Total money.Amount   `json:"total" swaggertype:"string"`
	Items []AssetLineDTO `json:"items"`
}

// AssetSummaryDTO is the asset page in one response.
type AssetSummaryDTO struct {
	Currency     string                 `json:"currency" example:"IDR"`
	TrackingMode string                 `json:"trackingMode" enums:"FULL_NET_WORTH,CASH_ONLY"`
	AsOf         time.Time              `json:"asOf"`
	NetWorth     money.Amount           `json:"netWorth" swaggertype:"string"`
	Cash         CashSectionDTO         `json:"cash"`
	Investments  *InvestmentsSectionDTO `json:"investments,omitempty"`
	FixedAssets  *AssetSectionDTO       `json:"fixedAssets,omitempty"`
	Receivables  *AssetSectionDTO       `json:"receivables,omitempty"`
	Liabilities  *AssetSectionDTO       `json:"liabilities,omitempty"`
	Warnings     []currency.Warning     `json:"warnings"`
}

func toAssetSectionDTO(sec *AssetSection, base string) *AssetSectionDTO {
	if sec == nil {
		return nil
	}
	out := &AssetSectionDTO{Total: money.NewAmount(sec.Total, base), Items: make([]AssetLineDTO, 0, len(sec.Items))}
	for _, l := range sec.Items {
		cur := strings.TrimSpace(l.Asset.Currency)
		out.Items = append(out.Items, AssetLineDTO{ID: l.Asset.ID, Name: l.Asset.Name, Currency: cur, Value: money.NewAmount(l.Asset.CurrentValue, cur),
			BaseValue: money.AmountPtr(l.Base, base), PctOfTotal: money.PctPtr(l.Pct)})
	}
	return out
}

func toAssetSummaryDTO(a AssetSummary) AssetSummaryDTO {
	d := AssetSummaryDTO{Currency: a.Currency, TrackingMode: a.TrackingMode, AsOf: a.AsOf, NetWorth: money.NewAmount(a.NetWorth, a.Currency), Warnings: a.Warnings,
		Cash: CashSectionDTO{Total: money.NewAmount(a.CashTotal, a.Currency), PctOfNetWorth: money.PctPtr(a.CashPctOfWorth), Wallets: make([]WalletLineDTO, 0, len(a.Wallets))}}
	for _, l := range a.Wallets {
		cur := strings.TrimSpace(l.Wallet.Currency)
		d.Cash.Wallets = append(d.Cash.Wallets, WalletLineDTO{ID: l.Wallet.ID, Name: l.Wallet.Name, Type: l.Wallet.Type, Currency: cur,
			Balance: money.NewAmount(l.Wallet.Balance, cur), BaseBalance: money.AmountPtr(l.Base, a.Currency), PctOfTotal: money.PctPtr(l.Pct)})
	}
	if a.Investments != nil {
		inv := &InvestmentsSectionDTO{Total: money.NewAmount(a.Investments.TotalValue, a.Currency), Cost: money.NewAmount(a.Investments.TotalCost, a.Currency),
			UnrealizedPL: money.NewAmount(a.Investments.UnrealizedPL, a.Currency), UnrealizedPLPct: money.PctPtr(a.Investments.UnrealizedPLPct),
			Categories: make([]CategoryLineDTO, 0, len(a.Categories))}
		for _, c := range a.Categories {
			inv.Categories = append(inv.Categories, CategoryLineDTO{AssetType: c.AssetType, Label: c.Label, Value: money.NewAmount(c.Value, a.Currency),
				UnrealizedPL: money.NewAmount(c.PL, a.Currency), UnrealizedPLPct: money.PctPtr(c.PLPct), PctOfTotal: money.PctPtr(c.Pct)})
		}
		d.Investments = inv
	}
	d.FixedAssets, d.Receivables, d.Liabilities = toAssetSectionDTO(a.Fixed, a.Currency), toAssetSectionDTO(a.Rec, a.Currency), toAssetSectionDTO(a.Liab, a.Currency)
	return d
}

// assetSummary godoc
//
//	@Summary		The asset page in one response
//	@Description	Cash per wallet (with its base-currency balance and share of the cash total), investments per asset type (value, unrealised profit/loss, share of the investments total), and fixed assets, money owed (receivables) and debts (liabilities) with their items, all converted to the space base currency with one FX snapshot, so the totals equal GET /net-worth/summary. Investments count my share, shared-in ones included. Anything without a usable FX rate has a null base value, is left out of the totals and reported in `warnings`. Amounts show no decimals for IDR and JPY and 2 for every other currency; percentages have 2 decimals and the slices of a section add up to exactly 100.00. In CASH_ONLY tracking mode only `cash` is returned.
//	@Tags			net-worth
//	@Security		BearerAuth
//	@Produce		json
//	@Param			spaceId	path		string	true	"Space ID"
//	@Success		200		{object}	AssetSummaryDTO
//	@Failure		404		{object}	httpx.ErrorEnvelope
//	@Router			/spaces/{spaceId}/assets/summary [get]
func (h *Handler) assetSummary(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	a, err := h.svc.AssetSummary(r.Context(), spaces.SpaceIDFromContext(r.Context()), user.ID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toAssetSummaryDTO(a))
}
