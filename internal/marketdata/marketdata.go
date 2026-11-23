// Package marketdata owns market instruments (searchable coin/stock lists),
// the latest provider prices and the background jobs that keep them fresh.
// Crypto comes from CoinMarketCap; US stocks and ETFs from Massive.
package marketdata

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog"

	"github.com/JonathanRainee/hzzt/internal/db"
	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
	"github.com/JonathanRainee/hzzt/internal/investments"
	"github.com/JonathanRainee/hzzt/internal/marketdata/coinmarketcap"
	"github.com/JonathanRainee/hzzt/internal/marketdata/jobs"
	"github.com/JonathanRainee/hzzt/internal/marketdata/massive"
	"github.com/JonathanRainee/hzzt/internal/platform/apperror"
)

// Providers and kinds stored in market_instruments.
const (
	ProviderCMC     = "CMC"
	ProviderMassive = "MASSIVE"

	KindCrypto = "CRYPTO"
	KindStock  = "STOCK"
	KindETF    = "ETF"
)

// Job names recorded in job_runs.
const (
	JobCoinMap       = "cmc.coin_map"
	JobCryptoPrice   = "cmc.crypto_prices"
	JobStockEOD      = "massive.eod_prices"
	JobStockSnapshot = "massive.snapshot_prices"
)

// Error codes owned by this package (see plan Appendix B).
const (
	CodeInstrumentNotFound = "INSTRUMENT_NOT_FOUND"
)

// DefaultRefreshMinutes is how often crypto prices are refreshed (CMC_REFRESH_MINUTES).
const DefaultRefreshMinutes = 10

const (
	// searchRemoteBelow: below this many local hits a stock search also asks Massive.
	searchRemoteBelow = 5
	// searchMaxWait is the longest a user-facing search waits for the Massive throttle.
	searchMaxWait = 2 * time.Second
	// resolveMaxWait is the longest creating an investment waits for it.
	resolveMaxWait = 20 * time.Second
	// remoteSearchTTL is how long the same remote search is not repeated.
	remoteSearchTTL = 10 * time.Minute
)

// Crypto is the CoinMarketCap capability the service needs; *coinmarketcap.Client satisfies it.
type Crypto interface {
	Map(ctx context.Context, limit int) ([]coinmarketcap.Coin, error)
	Prices(ctx context.Context, ids []int64) (map[int64]coinmarketcap.Quote, int, error)
}

// DB is what the service needs; *pgxpool.Pool and pgx.Tx both satisfy it.
type DB interface {
	sqlc.DBTX
	db.TxStarter
}

// Options configures the service. Either provider may be nil when its key is not configured.
type Options struct {
	Crypto Crypto
	// Stocks and Strategy are set together; with Stocks set and no Strategy, EOD is used.
	Stocks         StockProvider
	Strategy       PriceStrategy
	Log            zerolog.Logger
	RefreshMinutes int
	Now            func() time.Time
}

// Service holds market data logic.
type Service struct {
	db     DB
	q      *sqlc.Queries
	runner *jobs.Runner
	opts   Options
	now    func() time.Time

	mu       sync.Mutex
	searched map[string]time.Time // remote stock searches already done, by kind|query
}

// NewService builds the service.
func NewService(database DB, o Options) *Service {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	if o.RefreshMinutes <= 0 {
		o.RefreshMinutes = DefaultRefreshMinutes
	}
	if o.Stocks != nil && o.Strategy == nil {
		o.Strategy = &EodStrategy{Provider: o.Stocks}
	}
	return &Service{
		db: database, q: sqlc.New(database), runner: jobs.New(database, o.Log, now), opts: o, now: now,
		searched: map[string]time.Time{},
	}
}

// Enabled reports whether any provider is configured.
func (s *Service) Enabled() bool { return s.opts.Crypto != nil || s.opts.Stocks != nil }

var errDisabled = apperror.Upstream("Market data is not configured on this server")

// providerKind maps an investment asset type to the instrument provider and kind that price it.
func providerKind(assetType string) (provider, kind string, ok bool) {
	switch assetType {
	case investments.TypeCrypto:
		return ProviderCMC, KindCrypto, true
	case investments.TypeUSStock:
		return ProviderMassive, KindStock, true
	case investments.TypeGlobalETF:
		return ProviderMassive, KindETF, true
	}
	return "", "", false
}

// --- coin map ---------------------------------------------------------------------

// SyncCoinMap refreshes the local coin list (used for search, costs no credits).
func (s *Service) SyncCoinMap(ctx context.Context) (jobs.Result, error) {
	if s.opts.Crypto == nil {
		return jobs.Result{}, errDisabled
	}
	return s.runner.Run(ctx, JobCoinMap, func(ctx context.Context, tx pgx.Tx) (int, error) {
		coins, err := s.opts.Crypto.Map(ctx, coinmarketcap.MapLimit)
		if err != nil {
			return 0, err
		}
		if len(coins) == 0 {
			return 0, errors.New("coin map came back empty")
		}
		p := sqlc.UpsertInstrumentsParams{Provider: ProviderCMC, Kind: KindCrypto, Currency: "USD"}
		for _, c := range coins {
			p.ExternalIds = append(p.ExternalIds, strconv.FormatInt(c.ID, 10))
			p.Symbols = append(p.Symbols, c.Symbol)
			p.Names = append(p.Names, c.Name)
			p.Ranks = append(p.Ranks, int32(c.Rank))
			p.Actives = append(p.Actives, c.Active)
		}
		if err := sqlc.New(tx).UpsertInstruments(ctx, p); err != nil {
			return 0, err
		}
		return len(coins), nil
	})
}

// EnsureCoinMap syncs the coin map only when there is none yet.
func (s *Service) EnsureCoinMap(ctx context.Context) error {
	n, err := s.q.CountInstruments(ctx, sqlc.CountInstrumentsParams{Provider: ProviderCMC, Kind: KindCrypto})
	if err != nil || n > 0 {
		return err
	}
	res, err := s.SyncCoinMap(ctx)
	if err != nil {
		return err
	}
	return res.ErrOrNil()
}

// --- search ------------------------------------------------------------------------

// Instrument is a searchable market instrument.
type Instrument struct {
	ID     uuid.UUID
	Symbol string
	Name   string
	Rank   *int
	Kind   string
}

func toInstrument(m sqlc.MarketInstrument) Instrument {
	i := Instrument{ID: m.ID, Symbol: m.Symbol, Name: m.Name, Kind: m.Kind}
	if m.Rank != nil {
		r := int(*m.Rank)
		i.Rank = &r
	}
	return i
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (s *Service) searchLocal(ctx context.Context, provider, kind, query string, limit int) ([]Instrument, error) {
	rows, err := s.q.SearchInstruments(ctx, sqlc.SearchInstrumentsParams{
		Provider: provider, Kind: kind, Q: query, Prefix: escapeLike(query), QEscaped: escapeLike(query), RowLimit: int32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Instrument, 0, len(rows))
	for _, r := range rows {
		out = append(out, toInstrument(r))
	}
	return out, nil
}

// SearchCrypto finds coins by symbol or name: exact symbol first, then symbol
// prefix, then name matches, each tier ordered by market-cap rank.
func (s *Service) SearchCrypto(ctx context.Context, query string, limit int) ([]Instrument, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return []Instrument{}, nil
	}
	return s.searchLocal(ctx, ProviderCMC, KindCrypto, q, limit)
}

// SearchStocks finds US tickers of one kind (KindStock or KindETF). Local
// instruments are checked first; when there are fewer than five, Massive is
// asked too (throttled, and not repeated for the same query within ten
// minutes) and its results are cached as instruments. A throttled or failing
// Massive call never fails the search: the local hits are returned.
func (s *Service) SearchStocks(ctx context.Context, query, kind string, limit int) ([]Instrument, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return []Instrument{}, nil
	}
	local, err := s.searchLocal(ctx, ProviderMassive, kind, q, limit)
	if err != nil || len(local) >= searchRemoteBelow || s.opts.Stocks == nil {
		return local, err
	}
	key := kind + "|" + strings.ToLower(q)
	if !s.shouldSearchRemote(key) {
		return local, nil
	}
	tickers, err := s.opts.Stocks.SearchTickers(ctx, q, kind, false, 25, searchMaxWait)
	if err != nil {
		if !errors.Is(err, massive.ErrThrottled) {
			s.markSearched(key) // do not hammer a failing provider
			s.opts.Log.Warn().Err(err).Msg("remote ticker search failed; serving local results")
		}
		return local, nil
	}
	s.markSearched(key)
	if err := s.upsertTickers(ctx, s.q, kind, tickers); err != nil {
		return nil, err
	}
	return s.searchLocal(ctx, ProviderMassive, kind, q, limit)
}

func (s *Service) shouldSearchRemote(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.searched[key]
	return !ok || s.now().Sub(at) > remoteSearchTTL
}

func (s *Service) markSearched(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.searched[key] = s.now()
}

func (s *Service) upsertTickers(ctx context.Context, q *sqlc.Queries, kind string, ts []massive.Ticker) error {
	if len(ts) == 0 {
		return nil
	}
	p := sqlc.UpsertInstrumentsParams{Provider: ProviderMassive, Kind: kind, Currency: "USD"}
	for _, t := range ts {
		p.ExternalIds = append(p.ExternalIds, t.Symbol)
		p.Symbols = append(p.Symbols, t.Symbol)
		p.Names = append(p.Names, t.Name)
		p.Ranks = append(p.Ranks, 0) // stocks are not ranked
		p.Actives = append(p.Actives, t.Active)
	}
	return q.UpsertInstruments(ctx, p)
}

// ResolveInstrument picks the instrument that prices an investment of the given
// asset type: the explicit one (from search) when given, otherwise the exact
// symbol — the highest-ranked coin for crypto, the exact ticker for stocks and
// ETFs (looked up at Massive when it is not cached yet).
func (s *Service) ResolveInstrument(ctx context.Context, assetType string, explicit *uuid.UUID, symbol string) (uuid.UUID, error) {
	provider, kind, ok := providerKind(assetType)
	if !ok {
		return uuid.Nil, apperror.Unprocessable(CodeInstrumentNotFound, "This asset type has no market instruments")
	}
	if explicit != nil {
		m, err := s.q.GetInstrument(ctx, *explicit)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (m.Provider != provider || m.Kind != kind || !m.Active)) {
			return uuid.Nil, apperror.Unprocessable(CodeInstrumentNotFound, "That instrument is not available for this asset type; pick one from the search")
		}
		if err != nil {
			return uuid.Nil, err
		}
		return m.ID, nil
	}

	best := func() (sqlc.MarketInstrument, error) {
		return s.q.BestInstrumentBySymbol(ctx, sqlc.BestInstrumentBySymbolParams{Provider: provider, Kind: kind, Symbol: symbol})
	}
	m, err := best()
	if errors.Is(err, pgx.ErrNoRows) && provider == ProviderMassive && s.opts.Stocks != nil {
		// not cached yet: look the exact ticker up (this one is worth waiting for the throttle)
		ts, rerr := s.opts.Stocks.SearchTickers(ctx, symbol, kind, true, 5, resolveMaxWait)
		if rerr != nil {
			s.opts.Log.Warn().Err(rerr).Str("symbol", symbol).Msg("remote ticker lookup failed")
		} else if uerr := s.upsertTickers(ctx, s.q, kind, ts); uerr != nil {
			return uuid.Nil, uerr
		} else {
			m, err = best()
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, apperror.Unprocessable(CodeInstrumentNotFound,
			fmt.Sprintf("No instrument matches the symbol %s; pick one from the search and send its instrumentId", symbol))
	}
	if err != nil {
		return uuid.Nil, err
	}
	return m.ID, nil
}

// ambiguousRankLimit is the market-cap rank beyond which a shared symbol is not trusted: when
// several coins carry the symbol and even the best of them ranks lower than this, the symbol
// does not clearly name one coin (a well-known coin such as BTC, ranked 1, is never ambiguous).
const ambiguousRankLimit = 500

// SymbolAmbiguous reports whether a symbol does not clearly name one instrument: several active
// instruments share it and the best-ranked one has no rank or ranks below ambiguousRankLimit.
// It is used to decide whether a default AUTO price can be linked without asking the user.
func (s *Service) SymbolAmbiguous(ctx context.Context, assetType, symbol string) (bool, error) {
	provider, kind, ok := providerKind(assetType)
	if !ok {
		return false, nil
	}
	m, err := s.q.SymbolMatches(ctx, sqlc.SymbolMatchesParams{Provider: provider, Kind: kind, Symbol: symbol})
	if err != nil {
		return false, err
	}
	if m.N < 2 {
		return false, nil
	}
	return m.BestRank == 0 || m.BestRank > ambiguousRankLimit, nil
}

// --- prices ------------------------------------------------------------------------

type target struct {
	id         uuid.UUID
	provider   string
	externalID string
	symbol     string
}

func (s *Service) targets(ctx context.Context, q *sqlc.Queries, provider string) ([]target, error) {
	rows, err := q.AutoPricedByProvider(ctx, provider)
	if err != nil {
		return nil, err
	}
	out := make([]target, 0, len(rows))
	for _, r := range rows {
		out = append(out, target{id: r.ID, provider: provider, externalID: r.ExternalID, symbol: r.Symbol})
	}
	return out, nil
}

// storeQuote upserts one price.
func storeQuote(ctx context.Context, q *sqlc.Queries, id uuid.UUID, qt Quote) error {
	return q.UpsertMarketPrice(ctx, sqlc.UpsertMarketPriceParams{InstrumentID: id, Price: qt.Price, Currency: "USD", AsOf: qt.AsOf})
}

// fetchAndStore gets prices for the targets from their providers and upserts
// them; it returns how many were stored.
func (s *Service) fetchAndStore(ctx context.Context, q *sqlc.Queries, ts []target) (int, error) {
	var cmc, stocks []target
	for _, t := range ts {
		switch t.provider {
		case ProviderCMC:
			cmc = append(cmc, t)
		case ProviderMassive:
			stocks = append(stocks, t)
		}
	}
	stored := 0

	if len(cmc) > 0 {
		if s.opts.Crypto == nil {
			return stored, errors.New("crypto prices are not configured (CMC_API_KEY)")
		}
		ids := make([]int64, 0, len(cmc))
		byExt := map[int64]target{}
		for _, t := range cmc {
			n, err := strconv.ParseInt(t.externalID, 10, 64)
			if err != nil {
				continue
			}
			ids = append(ids, n)
			byExt[n] = t
		}
		quotes, calls, err := s.opts.Crypto.Prices(ctx, ids)
		// One CMC call carries up to coinmarketcap.BatchSize coins and costs about one credit, so
		// the credits of a run are the number of calls, not the number of holdings.
		s.opts.Log.Info().Int("coins", len(ids)).Int("calls", calls).Int("quotes", len(quotes)).Msg("cmc price refresh (credits used ~ calls)")
		if err != nil {
			return stored, err
		}
		for id, qt := range quotes {
			if err := storeQuote(ctx, q, byExt[id].id, Quote{Price: qt.Price, AsOf: qt.AsOf}); err != nil {
				return stored, err
			}
			stored++
		}
	}

	if len(stocks) > 0 {
		if s.opts.Stocks == nil {
			return stored, errors.New("stock prices are not configured (MASSIVE_API_KEY)")
		}
		syms := make([]string, len(stocks))
		bySym := map[string]target{}
		for i, t := range stocks {
			syms[i] = t.externalID
			bySym[t.externalID] = t
		}
		quotes, err := s.opts.Strategy.Quotes(ctx, syms, s.now())
		if err != nil {
			return stored, err
		}
		for sym, qt := range quotes {
			if t, ok := bySym[sym]; ok {
				if err := storeQuote(ctx, q, t.id, qt); err != nil {
					return stored, err
				}
				stored++
			}
		}
	}
	return stored, nil
}

// RefreshCryptoPrices is the scheduled crypto job: the distinct coins of every
// live AUTO crypto investment, fetched in batches of 100 (one call each) and
// stored. On any provider error the cycle fails as a whole, the previous prices
// stay (they turn stale) and a FAILED job_runs row records why.
func (s *Service) RefreshCryptoPrices(ctx context.Context) (jobs.Result, error) {
	if s.opts.Crypto == nil {
		return jobs.Result{}, errDisabled
	}
	return s.runner.Run(ctx, JobCryptoPrice, func(ctx context.Context, tx pgx.Tx) (int, error) {
		q := sqlc.New(tx)
		ts, err := s.targets(ctx, q, ProviderCMC)
		if err != nil {
			return 0, err
		}
		return s.fetchAndStore(ctx, q, ts)
	})
}

// RefreshStockPrices is the scheduled stock job, run through the configured
// strategy (EOD or snapshot). A snapshot run outside market hours does nothing.
func (s *Service) RefreshStockPrices(ctx context.Context) (jobs.Result, error) {
	if s.opts.Stocks == nil {
		return jobs.Result{}, errDisabled
	}
	st := s.opts.Strategy
	return s.runner.Run(ctx, st.JobName(), func(ctx context.Context, tx pgx.Tx) (int, error) {
		if !st.Active(s.now()) {
			return 0, nil
		}
		q := sqlc.New(tx)
		ts, err := s.targets(ctx, q, ProviderMassive)
		if err != nil {
			return 0, err
		}
		return s.fetchAndStore(ctx, q, ts)
	})
}

// RefreshPrice fetches one instrument's price now (on-demand refresh).
func (s *Service) RefreshPrice(ctx context.Context, instrumentID uuid.UUID) error {
	if !s.Enabled() {
		return errDisabled
	}
	m, err := s.q.GetInstrument(ctx, instrumentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return apperror.Unprocessable(CodeInstrumentNotFound, "Instrument not found")
	}
	if err != nil {
		return err
	}
	n, err := s.fetchAndStore(ctx, s.q, []target{{id: m.ID, provider: m.Provider, externalID: m.ExternalID, symbol: m.Symbol}})
	if err != nil {
		s.opts.Log.Warn().Err(err).Str("instrument", m.Symbol).Msg("on-demand price refresh failed")
		return apperror.Upstream("The price provider could not be reached; the last known price is kept")
	}
	if n == 0 {
		return apperror.Upstream("The price provider returned no price for this instrument")
	}
	return nil
}

// EnsurePrice fetches the price of a freshly linked instrument if it has none
// yet, so a new AUTO investment shows a market price right away (EOD uses the
// previous close). It is best effort: failures are logged and the next
// scheduled cycle retries.
func (s *Service) EnsurePrice(ctx context.Context, instrumentID uuid.UUID) {
	if !s.Enabled() {
		return
	}
	has, err := s.q.HasMarketPrice(ctx, instrumentID)
	if err != nil || has {
		return
	}
	if err := s.RefreshPrice(ctx, instrumentID); err != nil {
		s.opts.Log.Warn().Err(err).Msg("could not fetch the first price of a new instrument")
	}
}

// MarketPrices implements investments.PriceSource, with provider-specific staleness.
func (s *Service) MarketPrices(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]investments.MarketPrice, error) {
	rows, err := s.q.GetMarketPricesWithProvider(ctx, ids)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := make(map[uuid.UUID]investments.MarketPrice, len(rows))
	for _, r := range rows {
		mp := investments.MarketPrice{Price: r.Price, AsOf: r.AsOf}
		if r.Provider == ProviderMassive && s.opts.Strategy != nil {
			mp.StaleAfter = s.opts.Strategy.StaleAfter(now)
		}
		out[r.InstrumentID] = mp
	}
	return out, nil
}

// --- scheduling ---------------------------------------------------------------------

// StartJobs schedules the crypto jobs (coin map daily, prices every
// CMC_REFRESH_MINUTES) and the stock job (per the MASSIVE_MODE strategy), and on
// boot syncs the coin map if it is empty and refreshes prices once. The
// returned function stops the scheduler.
func (s *Service) StartJobs(ctx context.Context, log zerolog.Logger) (stop func(), err error) {
	if !s.Enabled() {
		log.Info().Msg("market data disabled: neither CMC_API_KEY nor MASSIVE_API_KEY is set")
		return func() {}, nil
	}
	run := func(name string, fn func(context.Context) error) {
		jctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		if err := fn(jctx); err != nil {
			log.Error().Err(err).Str("job", name).Msg("market data job failed")
		}
	}
	c := cron.New(cron.WithLocation(time.UTC), cron.WithChain(cron.SkipIfStillRunning(cron.DiscardLogger)))
	add := func(spec, name string, fn func(context.Context) error) error {
		_, err := c.AddFunc(spec, func() { run(name, fn) })
		return err
	}
	if s.opts.Crypto != nil {
		if err := add("@daily", JobCoinMap, func(ctx context.Context) error { _, err := s.SyncCoinMap(ctx); return err }); err != nil {
			return nil, err
		}
		if err := add(fmt.Sprintf("@every %dm", s.opts.RefreshMinutes), JobCryptoPrice,
			func(ctx context.Context) error { _, err := s.RefreshCryptoPrices(ctx); return err }); err != nil {
			return nil, err
		}
	}
	if s.opts.Stocks != nil {
		st := s.opts.Strategy
		if err := add(st.Schedule(), st.JobName(), func(ctx context.Context) error { _, err := s.RefreshStockPrices(ctx); return err }); err != nil {
			return nil, err
		}
		log.Info().Str("mode", st.Mode()).Str("schedule", st.Schedule()).Msg("stock price refresh scheduled")
	}
	c.Start()
	go func() { // never block startup on a provider
		if s.opts.Crypto != nil {
			run("cmc.boot", func(ctx context.Context) error {
				if err := s.EnsureCoinMap(ctx); err != nil {
					return err
				}
				_, err := s.RefreshCryptoPrices(ctx)
				return err
			})
		}
		if s.opts.Stocks != nil {
			run("massive.boot", func(ctx context.Context) error { _, err := s.RefreshStockPrices(ctx); return err })
		}
	}()
	return func() { <-c.Stop().Done() }, nil
}
