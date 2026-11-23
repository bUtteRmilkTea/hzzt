package currency

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"
	"github.com/shopspring/decimal"

	"github.com/JonathanRainee/hzzt/internal/db"
	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
	"github.com/JonathanRainee/hzzt/internal/platform/apperror"
	"github.com/JonathanRainee/hzzt/internal/platform/money"
)

// ErrRefreshBusy is returned when another instance holds the refresh lock.
var ErrRefreshBusy = errors.New("fx refresh already running elsewhere")

// DB is what the service needs; *pgxpool.Pool and pgx.Tx both satisfy it.
type DB interface {
	sqlc.DBTX
	db.TxStarter
}

// Options configures the service.
type Options struct {
	Provider FxProvider
	Log      zerolog.Logger
	Now      func() time.Time
}

// Service holds currency and FX logic.
type Service struct {
	db   DB
	q    *sqlc.Queries
	opts Options
	now  func() time.Time
}

// NewService builds the service.
func NewService(database DB, o Options) *Service {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &Service{db: database, q: sqlc.New(database), opts: o, now: now}
}

func today(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// Refresh fetches the latest rates and upserts them by (quote, as_of), so
// running it twice on the same day is idempotent. Only one instance runs it
// at a time (transaction-scoped advisory lock); others get ErrRefreshBusy.
func (s *Service) Refresh(ctx context.Context) error {
	if s.opts.Provider == nil {
		return errors.New("no FX provider configured")
	}
	return db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		locked, err := q.TryFxRefreshLock(ctx)
		if err != nil {
			return fmt.Errorf("advisory lock: %w", err)
		}
		if !locked {
			return ErrRefreshBusy
		}
		set, err := s.opts.Provider.Latest(ctx)
		if err != nil {
			return err
		}
		stored := 0
		for _, code := range money.SupportedCurrencies {
			if code == "USD" {
				continue
			}
			rate, ok := set.Rates[code]
			if !ok || !rate.IsPositive() {
				s.opts.Log.Warn().Str("currency", code).Str("source", set.Source).Msg("fx provider returned no rate")
				continue
			}
			if err := q.UpsertFxRate(ctx, sqlc.UpsertFxRateParams{
				Quote: code, Rate: rate.Round(12), AsOf: set.AsOf, Source: set.Source,
			}); err != nil {
				return fmt.Errorf("store %s rate: %w", code, err)
			}
			stored++
		}
		if stored == 0 {
			return errors.New("fx provider returned no usable rates")
		}
		// One provider call per run (USD base, every supported currency); cross rates are derived at convert time.
		ev := s.opts.Log.Info().Int("rates", stored).Str("source", set.Source).Time("asOf", set.AsOf).Int("providerCalls", 1)
		if qr, ok := s.opts.Provider.(QuotaReporter); ok {
			if q, qerr := qr.Quota(ctx); qerr != nil {
				s.opts.Log.Warn().Err(qerr).Msg("fx provider quota unavailable")
			} else {
				ev = ev.Int("quotaRemaining", q.Remaining).Int("quotaTotal", q.Total)
			}
		}
		ev.Msg("fx rates refreshed")
		return nil
	})
}

// EnsureRates refreshes only when no rates exist yet (used on boot).
func (s *Service) EnsureRates(ctx context.Context) error {
	n, err := s.q.CountFxRates(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	return s.Refresh(ctx)
}

// liveRates loads the newest USD-based rate per quote.
func (s *Service) liveRates(ctx context.Context, at time.Time) (map[string]decimal.Decimal, time.Time, error) {
	rows, err := s.q.LatestFxRates(ctx, today(at))
	if err != nil {
		return nil, time.Time{}, err
	}
	rates := make(map[string]decimal.Decimal, len(rows))
	var asOf time.Time
	for _, r := range rows {
		rates[r.Quote] = r.Rate
		if r.AsOf.After(asOf) {
			asOf = r.AsOf
		}
	}
	return rates, asOf, nil
}

// BuildContext loads the space's currency settings, manual rates and live
// rates once, for use by every aggregation in a request.
func (s *Service) BuildContext(ctx context.Context, spaceID uuid.UUID) (*ConversionContext, error) {
	return s.BuildContextAt(ctx, spaceID, s.now())
}

// BuildContextAt is BuildContext using the live rates in force at `at` (the
// newest rate with as_of on or before that date).
func (s *Service) BuildContextAt(ctx context.Context, spaceID uuid.UUID, at time.Time) (*ConversionContext, error) {
	sp, err := s.q.GetSpaceByID(ctx, spaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperror.NotFound("Space not found")
	}
	if err != nil {
		return nil, err
	}
	manual, err := s.q.ListManualFxRates(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	cc := &ConversionContext{
		BaseCurrency: sp.BaseCurrency,
		AutoFX:       sp.AutoFxConversion,
		ManualRates:  make(map[string]decimal.Decimal, len(manual)),
	}
	for _, m := range manual {
		cc.ManualRates[pairKey(m.Base, m.Quote)] = m.Rate
	}
	cc.Rates, cc.AsOf, err = s.liveRates(ctx, at)
	if err != nil {
		return nil, err
	}
	return cc, nil
}

// Latest is the live rate table re-based to `base`.
type Latest struct {
	Base   string
	AsOf   *time.Time
	Rates  map[string]decimal.Decimal
	Source string
}

// LatestRebased returns live rates as "units of quote per 1 base".
// With no stored rates yet, Rates is empty.
func (s *Service) LatestRebased(ctx context.Context, base string) (Latest, error) {
	rows, err := s.q.LatestFxRates(ctx, today(s.now()))
	if err != nil {
		return Latest{}, err
	}
	cc := &ConversionContext{AutoFX: true, Rates: map[string]decimal.Decimal{}}
	out := Latest{Base: base, Rates: map[string]decimal.Decimal{}}
	for _, r := range rows {
		cc.Rates[r.Quote] = r.Rate
		out.Source = r.Source
		if out.AsOf == nil || r.AsOf.After(*out.AsOf) {
			t := r.AsOf
			out.AsOf = &t
		}
	}
	for _, code := range money.SupportedCurrencies {
		if code == base {
			continue
		}
		if r, err := cc.Rate(base, code); err == nil {
			out.Rates[code] = r.Round(12)
		}
	}
	return out, nil
}

// ManualRates lists the space's manual rates.
func (s *Service) ManualRates(ctx context.Context, spaceID uuid.UUID) ([]sqlc.ManualFxRate, error) {
	rows, err := s.q.ListManualFxRates(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Base != rows[j].Base {
			return rows[i].Base < rows[j].Base
		}
		return rows[i].Quote < rows[j].Quote
	})
	return rows, nil
}

// SetManualRate creates or replaces a manual rate.
func (s *Service) SetManualRate(ctx context.Context, spaceID, userID uuid.UUID, base, quote string, rate decimal.Decimal) (sqlc.ManualFxRate, error) {
	return s.q.UpsertManualFxRate(ctx, sqlc.UpsertManualFxRateParams{
		SpaceID: spaceID, Base: base, Quote: quote, Rate: rate, UpdatedByID: userID,
	})
}

// DeleteManualRate removes a manual rate; it reports NotFound if absent.
func (s *Service) DeleteManualRate(ctx context.Context, spaceID uuid.UUID, base, quote string) error {
	n, err := s.q.DeleteManualFxRate(ctx, sqlc.DeleteManualFxRateParams{SpaceID: spaceID, Base: base, Quote: quote})
	if err != nil {
		return err
	}
	if n == 0 {
		return apperror.NotFound("Manual rate not found")
	}
	return nil
}
