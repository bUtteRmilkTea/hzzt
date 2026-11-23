package networth

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/JonathanRainee/hzzt/internal/currency"
	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
	"github.com/JonathanRainee/hzzt/internal/investments"
	"github.com/JonathanRainee/hzzt/internal/manualassets"
	"github.com/JonathanRainee/hzzt/internal/platform/allocation"
	"github.com/JonathanRainee/hzzt/internal/platform/apperror"
)

// WalletLine is one wallet of the asset page.
type WalletLine struct {
	Wallet sqlc.Wallet
	// Base is the balance in the base currency; nil without an FX rate.
	Base *decimal.Decimal
	Pct  *decimal.Decimal
}

// AssetLine is one manual asset (fixed asset, money owed or debt) of the asset page.
type AssetLine struct {
	Asset sqlc.ManualAsset
	Base  *decimal.Decimal
	Pct   *decimal.Decimal
}

// AssetSection is one manual-asset kind with its converted total.
type AssetSection struct {
	Total decimal.Decimal
	Items []AssetLine
}

// CategoryLine is one investment asset type.
type CategoryLine struct {
	AssetType, Label string
	Value, Cost, PL  decimal.Decimal
	PLPct, Pct       *decimal.Decimal
}

// AssetSummary is everything the asset page shows, valued with one FX snapshot.
type AssetSummary struct {
	Currency     string
	TrackingMode string
	AsOf         time.Time
	NetWorth     decimal.Decimal

	CashTotal        decimal.Decimal
	CashPctOfWorth   *decimal.Decimal
	Wallets          []WalletLine
	Investments      *investments.Summary
	Categories       []CategoryLine
	Fixed, Rec, Liab *AssetSection
	Warnings         []currency.Warning
}

// pctOf is part / whole as a percentage; nil unless the whole is positive.
func pctOf(part, whole decimal.Decimal) *decimal.Decimal {
	if !whole.IsPositive() {
		return nil
	}
	p := part.Mul(decimal.NewFromInt(100)).Div(whole)
	return &p
}

// AssetSummary builds the asset page of a space: cash per wallet, investments per asset type,
// and fixed assets, money owed and debts, all in the space's base currency from one conversion
// context, so the totals equal Compute's. In CASH_ONLY mode only cash is returned.
func (s *Service) AssetSummary(ctx context.Context, spaceID, viewer uuid.UUID) (AssetSummary, error) {
	ref := s.now()
	space, err := s.q.GetSpaceByID(ctx, spaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return AssetSummary{}, apperror.NotFound("Space not found")
	}
	if err != nil {
		return AssetSummary{}, err
	}
	cc, err := s.currency.BuildContextAt(ctx, spaceID, ref)
	if err != nil {
		return AssetSummary{}, err
	}
	out := AssetSummary{Currency: strings.TrimSpace(space.BaseCurrency), TrackingMode: space.TrackingMode, AsOf: ref.UTC(), Warnings: []currency.Warning{}}
	seen := map[string]bool{}
	warn := func(ws []currency.Warning) {
		for _, w := range ws {
			if !seen[w.Message] {
				seen[w.Message] = true
				out.Warnings = append(out.Warnings, w)
			}
		}
	}

	wallets, err := s.q.ListWallets(ctx, spaceID)
	if err != nil {
		return AssetSummary{}, err
	}
	items := make([]currency.MoneyItem, 0, len(wallets))
	var idx []int
	var vals []decimal.Decimal
	for i, w := range wallets {
		items = append(items, currency.MoneyItem{Label: w.Name, Amount: w.Balance, Currency: w.Currency})
		line := WalletLine{Wallet: w}
		if v, err := currency.Convert(w.Balance, strings.TrimSpace(w.Currency), cc.BaseCurrency, cc); err == nil {
			line.Base = &v
			idx, vals = append(idx, i), append(vals, v)
		}
		out.Wallets = append(out.Wallets, line)
	}
	for k, p := range allocation.SplitPct(vals) {
		out.Wallets[idx[k]].Pct = p
	}
	cash := currency.Aggregate(items, cc)
	out.CashTotal = cash.Total
	warn(cash.Warnings)

	if space.TrackingMode == TrackingCashOnly {
		out.NetWorth = cash.Total
		out.CashPctOfWorth = pctOf(out.CashTotal, out.NetWorth)
		return out, nil
	}

	inv, err := s.investments.ListWith(ctx, spaceID, "", true, viewer, cc, ref)
	if err != nil {
		return AssetSummary{}, err
	}
	sum := inv.Summary
	out.Investments = &sum
	warn(sum.Warnings)
	for _, c := range investments.Group(out.Currency, inv.Items).Categories {
		out.Categories = append(out.Categories, CategoryLine{AssetType: c.AssetType, Label: c.Label, Value: c.Value, Cost: c.Cost,
			PL: c.UnrealizedPL, PLPct: c.UnrealizedPLPct, Pct: c.PctOfTotal})
	}

	rows, err := s.q.ListManualAssets(ctx, sqlc.ListManualAssetsParams{SpaceID: spaceID})
	if err != nil {
		return AssetSummary{}, err
	}
	section := func(kind string) *AssetSection {
		sec := &AssetSection{Items: []AssetLine{}}
		var money []currency.MoneyItem
		var ix []int
		var vs []decimal.Decimal
		for _, r := range rows {
			if r.Kind != kind {
				continue
			}
			money = append(money, currency.MoneyItem{Label: r.Name, Amount: r.CurrentValue, Currency: r.Currency})
			line := AssetLine{Asset: r}
			if v, err := currency.Convert(r.CurrentValue, strings.TrimSpace(r.Currency), cc.BaseCurrency, cc); err == nil {
				line.Base = &v
				ix, vs = append(ix, len(sec.Items)), append(vs, v)
			}
			sec.Items = append(sec.Items, line)
		}
		for k, p := range allocation.SplitPct(vs) {
			sec.Items[ix[k]].Pct = p
		}
		agg := currency.Aggregate(money, cc)
		sec.Total = agg.Total
		warn(agg.Warnings)
		return sec
	}
	out.Fixed, out.Rec, out.Liab = section(manualassets.KindFixedAsset), section(manualassets.KindReceivable), section(manualassets.KindLiability)
	out.NetWorth = out.CashTotal.Add(sum.TotalValue).Add(out.Fixed.Total).Add(out.Rec.Total).Sub(out.Liab.Total)
	out.CashPctOfWorth = pctOf(out.CashTotal, out.NetWorth)
	return out, nil
}
