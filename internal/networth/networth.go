// Package networth computes a space's net-worth summary from its wallets,
// investments (the viewer's share, shared-in ones included) and manual assets.
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
	"github.com/JonathanRainee/hzzt/internal/marketdata/jobs"
	"github.com/JonathanRainee/hzzt/internal/platform/apperror"
	"github.com/JonathanRainee/hzzt/internal/wallets"
)

// Tracking modes of a space.
const (
	TrackingFull     = "FULL_NET_WORTH"
	TrackingCashOnly = "CASH_ONLY"
)

// DB is what the service needs.
type DB interface{ sqlc.DBTX }

// InvestmentsPart is the investments section of the summary.
type InvestmentsPart struct {
	Value        decimal.Decimal
	Cost         decimal.Decimal
	UnrealizedPL decimal.Decimal
	// UnrealizedPLPct is a percentage; nil when the cost is 0.
	UnrealizedPLPct *decimal.Decimal
}

// Summary is a space's net worth in its base currency.
//
// netWorth = cash + investments (my share) + fixedAssets + receivables − liabilities.
// In CASH_ONLY tracking mode only Cash and NetWorth (= Cash) are set.
type Summary struct {
	Currency     string
	TrackingMode string
	AsOf         time.Time
	Cash         decimal.Decimal
	Investments  *InvestmentsPart
	FixedAssets  *decimal.Decimal
	Receivables  *decimal.Decimal
	Liabilities  *decimal.Decimal
	NetWorth     decimal.Decimal
	// Warnings lists anything left out of the totals (typically a missing FX rate).
	Warnings []currency.Warning
}

// Service computes summaries.
type Service struct {
	q           *sqlc.Queries
	currency    *currency.Service
	investments *investments.Service
	wallets     *wallets.Service
	assets      *manualassets.Service
	runner      *jobs.Runner
	now         func() time.Time
}

// NewService builds the service; now may be nil.
func NewService(database DB, cur *currency.Service, inv *investments.Service, w *wallets.Service, assets *manualassets.Service, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{q: sqlc.New(database), currency: cur, investments: inv, wallets: w, assets: assets, now: now}
}

// Compute builds the net-worth summary of a space. at is the reference time for
// FX rates and price freshness (nil means now); wallet balances and asset values
// are always the current ones, so history is built from stored snapshots (P17),
// not by recomputing the past.
func (s *Service) Compute(ctx context.Context, spaceID uuid.UUID, at *time.Time) (Summary, error) {
	ref := s.now()
	if at != nil {
		ref = *at
	}
	space, err := s.q.GetSpaceByID(ctx, spaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Summary{}, apperror.NotFound("Space not found")
	}
	if err != nil {
		return Summary{}, err
	}
	cc, err := s.currency.BuildContextAt(ctx, spaceID, ref)
	if err != nil {
		return Summary{}, err
	}

	out := Summary{Currency: strings.TrimSpace(space.BaseCurrency), TrackingMode: space.TrackingMode, AsOf: ref.UTC(), Warnings: []currency.Warning{}}
	seen := map[string]bool{}
	warn := func(ws []currency.Warning) {
		for _, w := range ws {
			if !seen[w.Message] {
				seen[w.Message] = true
				out.Warnings = append(out.Warnings, w)
			}
		}
	}

	items, err := s.wallets.NetWorthItems(ctx, spaceID)
	if err != nil {
		return Summary{}, err
	}
	cash := currency.Aggregate(items, cc)
	out.Cash = cash.Total
	warn(cash.Warnings)

	if space.TrackingMode == TrackingCashOnly {
		out.NetWorth = out.Cash
		return out, nil
	}

	inv, err := s.investments.ListWith(ctx, spaceID, "", true, uuid.Nil, cc, ref)
	if err != nil {
		return Summary{}, err
	}
	out.Investments = &InvestmentsPart{
		Value: inv.Summary.TotalValue, Cost: inv.Summary.TotalCost, UnrealizedPL: inv.Summary.UnrealizedPL,
		UnrealizedPLPct: inv.Summary.UnrealizedPLPct,
	}
	warn(inv.Summary.Warnings)

	totals, err := s.assets.Totals(ctx, spaceID, cc)
	if err != nil {
		return Summary{}, err
	}
	fixed, rec, liab := totals[manualassets.KindFixedAsset], totals[manualassets.KindReceivable], totals[manualassets.KindLiability]
	out.FixedAssets, out.Receivables, out.Liabilities = &fixed.Total, &rec.Total, &liab.Total
	warn(fixed.Warnings)
	warn(rec.Warnings)
	warn(liab.Warnings)

	out.NetWorth = out.Cash.Add(inv.Summary.TotalValue).Add(fixed.Total).Add(rec.Total).Sub(liab.Total)
	return out, nil
}
