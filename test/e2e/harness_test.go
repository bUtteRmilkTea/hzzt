//go:build e2e

// Package e2e holds end-to-end tests that drive the real router over HTTP
// against the test database (TEST_DATABASE_URL, docker service postgres_test).
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/JonathanRainee/hzzt/internal/auth"
	"github.com/JonathanRainee/hzzt/internal/categories"
	"github.com/JonathanRainee/hzzt/internal/config"
	"github.com/JonathanRainee/hzzt/internal/currency"
	"github.com/JonathanRainee/hzzt/internal/dashboard"
	"github.com/JonathanRainee/hzzt/internal/db"
	"github.com/JonathanRainee/hzzt/internal/db/migrations"
	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
	"github.com/JonathanRainee/hzzt/internal/goals"
	"github.com/JonathanRainee/hzzt/internal/investments"
	"github.com/JonathanRainee/hzzt/internal/manualassets"
	"github.com/JonathanRainee/hzzt/internal/marketdata"
	"github.com/JonathanRainee/hzzt/internal/marketdata/jobs"
	"github.com/JonathanRainee/hzzt/internal/networth"
	"github.com/JonathanRainee/hzzt/internal/platform/email"
	"github.com/JonathanRainee/hzzt/internal/platform/events"
	"github.com/JonathanRainee/hzzt/internal/platform/httpx"
	"github.com/JonathanRainee/hzzt/internal/server"
	"github.com/JonathanRainee/hzzt/internal/spaces"
	"github.com/JonathanRainee/hzzt/internal/transactions"
	"github.com/JonathanRainee/hzzt/internal/transfers"
	"github.com/JonathanRainee/hzzt/internal/users"
	"github.com/JonathanRainee/hzzt/internal/wallets"
)

var testPool *pgxpool.Pool

// TestMain connects to the test database once and applies migrations.
func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		fmt.Fprintln(os.Stderr, "e2e: TEST_DATABASE_URL is not set (copy .env.example to .env and run via `make test-e2e`)")
		return 1
	}
	ctx := context.Background()
	pool, err := db.NewPool(ctx, url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: connect to test database (is `make up` running?): %v\n", err)
		return 1
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: migrate test database: %v\n", err)
		return 1
	}
	testPool = pool
	return m.Run()
}

// TestServer is a running copy of the API whose database work happens inside
// a single transaction that is rolled back when the test ends.
type TestServer struct {
	*httptest.Server
	Tx          pgx.Tx // for seeding and asserting data directly
	Email       *email.Memory
	Events      *events.Bus
	Auth        *auth.Service
	Clock       *testClock
	FX          *FakeFX
	Currency    *currency.Service
	Categories  *categories.Service
	Investments *investments.Service
	Market      *marketdata.Service
	CMC         *FakeCMC
	Massive     *FakeMassive
	Networth    *networth.Service
	t           *testing.T
}

const testJWTSecret = "e2e-test-secret-e2e-test-secret-0123"

// generousLimits keeps rate limiting out of the way of ordinary tests.
func generousLimits() auth.Limits {
	return auth.Limits{
		Login:    httpx.NewRateLimiter(1000, time.Minute),
		Register: httpx.NewRateLimiter(1000, time.Minute),
		Forgot:   httpx.NewRateLimiter(1000, time.Minute),
	}
}

// NewTestServer starts the real router on a per-test transaction.
func NewTestServer(t *testing.T) *TestServer {
	t.Helper()
	return NewTestServerWithLimits(t, generousLimits())
}

// NewTestServerWithLimits is NewTestServer with explicit auth rate limits.
func NewTestServerWithLimits(t *testing.T, limits auth.Limits) *TestServer {
	t.Helper()
	return newTestServer(t, limits, users.Limits{VerifyPIN: httpx.NewRateLimiter(1000, time.Minute)}, generousInvLimits())
}

func generousInvLimits() investments.Limits {
	return investments.Limits{RefreshPrice: httpx.NewRateLimiter(1000, time.Minute)}
}

// NewTestServerWithInvestmentLimits is NewTestServer with explicit investment rate limits.
func NewTestServerWithInvestmentLimits(t *testing.T, limits investments.Limits) *TestServer {
	t.Helper()
	return newTestServer(t, generousLimits(), users.Limits{VerifyPIN: httpx.NewRateLimiter(1000, time.Minute)}, limits)
}

// NewTestServerWithUserLimits is NewTestServer with explicit /me rate limits.
func NewTestServerWithUserLimits(t *testing.T, limits users.Limits) *TestServer {
	t.Helper()
	return newTestServer(t, generousLimits(), limits, generousInvLimits())
}

func newTestServer(t *testing.T, limits auth.Limits, userLimits users.Limits, invLimits investments.Limits) *TestServer {
	t.Helper()
	ctx := context.Background()

	tx, err := testPool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		// Roll back with a fresh context: the test context may be done already.
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tx.Rollback(rctx); err != nil && err != pgx.ErrTxClosed {
			t.Errorf("rollback test transaction: %v", err)
		}
	})

	cfg := &config.Config{
		Env:         "test",
		CORSOrigins: []string{"http://localhost:3000"},
	}
	log := zerolog.Nop()
	clock := &testClock{}
	mail := &email.Memory{}
	bus := events.NewBus(log)
	svc := auth.NewService(tx, auth.Options{
		AccessSecret: testJWTSecret,
		AccessTTL:    15 * time.Minute,
		RefreshTTL:   30 * 24 * time.Hour,
		AppURL:       "http://localhost:3000",
		Email:        mail,
		Events:       bus,
		Log:          log,
	})
	handler := auth.NewHandler(svc, limits, false)
	spacesSvc := spaces.NewService(tx, spaces.Options{AppURL: "http://localhost:3000", Events: bus, Log: log, Now: clock.Now})
	spacesSvc.Subscribe(bus)
	spacesHandler := spaces.NewHandler(spacesSvc, svc)
	usersSvc := users.NewService(tx, users.Options{Now: clock.Now})
	usersSvc.Subscribe(bus)
	usersHandler := users.NewHandler(usersSvc, svc, userLimits)
	fx := &FakeFX{Set: defaultRates(clock), Clock: clock}
	// Moving the clock back re-dates FX rates that are dated after the new "now".
	clock.onSet = func(now time.Time) {
		_, _ = tx.Exec(context.Background(), `UPDATE fx_rates SET as_of = $1 WHERE as_of > $1`, now.UTC().Truncate(24*time.Hour))
	}
	currencySvc := currency.NewService(tx, currency.Options{Provider: fx, Log: log, Now: clock.Now})
	currencyHandler := currency.NewHandler(currencySvc, svc, spacesSvc)
	categoriesSvc := categories.NewService(tx, currencySvc)
	categoriesSvc.SetClock(clock.Now)
	categoriesSvc.Subscribe(bus)
	categoriesHandler := categories.NewHandler(categoriesSvc, svc, spacesSvc)
	walletsSvc := wallets.NewService(tx, currencySvc, clock.Now)
	walletsHandler := wallets.NewHandler(walletsSvc, svc, spacesSvc)
	transactionsSvc := transactions.NewService(tx, currencySvc, clock.Now)
	categoriesSvc.SetUsageChecker(transactionsSvc.CategoryInUse)
	transactionsHandler := transactions.NewHandler(transactionsSvc, svc, spacesSvc)
	cmc := &FakeCMC{now: clock.Now, Coins: defaultCoins(), Quotes: map[int64]string{1: "63000", 1027: "1800", 825: "1", 5426: "150"}}
	mas := newFakeMassive()
	marketSvc := marketdata.NewService(tx, marketdata.Options{
		Crypto: cmc, Stocks: mas, Strategy: &marketdata.EodStrategy{Provider: mas}, Log: log, Now: clock.Now,
	})
	_, err = marketSvc.SyncCoinMap(ctx)
	require.NoError(t, err)
	cmc.MapCalls, cmc.PriceCalls = 0, 0
	investmentsSvc := investments.NewService(tx, currencySvc, clock.Now)
	investmentsSvc.SetMarket(marketSvc)
	investmentsHandler := investments.NewHandler(investmentsSvc, svc, spacesSvc, invLimits)
	goalsSvc := goals.NewService(tx, currencySvc, clock.Now)
	goalsHandler := goals.NewHandler(goalsSvc, svc, spacesSvc)
	manualAssetsSvc := manualassets.NewService(tx, currencySvc, transactionsSvc, clock.Now)
	manualAssetsHandler := manualassets.NewHandler(manualAssetsSvc, svc, spacesSvc)
	networthSvc := networth.NewService(tx, currencySvc, investmentsSvc, manualAssetsSvc, clock.Now)
	networthSvc.SetRunner(jobs.New(tx, log, clock.Now))
	dashboardHandler := dashboard.NewHandler(dashboard.NewService(tx, currencySvc, clock.Now), svc, spacesSvc)
	networthHandler := networth.NewHandler(networthSvc, svc, spacesSvc)
	transfersHandler := transfers.NewHandler(transfers.NewService(tx, transactionsSvc, investmentsSvc, manualAssetsSvc, clock.Now), svc, spacesSvc)
	marketHandler := marketdata.NewHandler(marketSvc, svc, marketLimits())
	srv := httptest.NewServer(server.NewRouter(cfg, log, sqlc.New(tx), handler.Mount, usersHandler.Mount, spacesHandler.Mount, currencyHandler.Mount, categoriesHandler.Mount, walletsHandler.Mount, transactionsHandler.Mount, transfersHandler.Mount, investmentsHandler.Mount, marketHandler.Mount, manualAssetsHandler.Mount, networthHandler.Mount, goalsHandler.Mount, dashboardHandler.Mount))
	t.Cleanup(srv.Close)

	ts := &TestServer{Server: srv, Tx: tx, Email: mail, Events: bus, Auth: svc, Clock: clock, FX: fx, Currency: currencySvc, Categories: categoriesSvc, Investments: investmentsSvc, Market: marketSvc, CMC: cmc, Massive: mas, Networth: networthSvc, t: t}
	// Registered after the rollback cleanup, so it runs first: the ledger
	// invariant must hold at the end of every test.
	t.Cleanup(func() {
		if !t.Failed() {
			ts.ExpectAllLedgersConsistent()
		}
	})
	return ts
}

// Response is a fully read HTTP response.
type Response struct {
	Status int
	Body   []byte
	Header http.Header
}

// Cookie returns the Set-Cookie with the given name, or nil.
func (r Response) Cookie(name string) *http.Cookie {
	for _, c := range (&http.Response{Header: r.Header}).Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// JSON decodes the body into v.
func (r Response) JSON(t *testing.T, v any) {
	t.Helper()
	require.NoError(t, json.Unmarshal(r.Body, v), "body: %s", r.Body)
}

// Do sends a request; body (if non-nil) is JSON-encoded. headers are optional.
func (s *TestServer) Do(method, path string, body any, headers ...[2]string) Response {
	s.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(s.t, err)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.URL+path, rdr)
	require.NoError(s.t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, h := range headers {
		req.Header.Set(h[0], h[1])
	}
	res, err := s.Client().Do(req)
	require.NoError(s.t, err)
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	require.NoError(s.t, err)
	return Response{Status: res.StatusCode, Body: b, Header: res.Header}
}

// Get is shorthand for Do(GET, path, nil).
func (s *TestServer) Get(path string, headers ...[2]string) Response {
	s.t.Helper()
	return s.Do(http.MethodGet, path, nil, headers...)
}

// testClock lets a test move "now" forward (e.g. to expire invitations).
type testClock struct {
	mu     sync.Mutex
	offset time.Duration
	// onSet runs after Set, so the harness can re-date data in the new "future".
	onSet func(now time.Time)
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

// Set makes "now" equal t (give or take the test's own runtime).
func (c *testClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset = time.Until(t)
	if c.onSet != nil {
		c.onSet(t)
	}
}

// Advance moves the clock forward.
func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

// testMarketLimits overrides the market endpoints' rate limits for one test.
var testMarketLimits *marketdata.Limits

func marketLimits() marketdata.Limits {
	if testMarketLimits != nil {
		return *testMarketLimits
	}
	return marketdata.Limits{StockSearch: httpx.NewRateLimiter(1000, time.Minute)}
}

// NewTestServerWithMarketLimits is NewTestServer with explicit market endpoint rate limits.
func NewTestServerWithMarketLimits(t *testing.T, limits marketdata.Limits) *TestServer {
	t.Helper()
	testMarketLimits = &limits
	t.Cleanup(func() { testMarketLimits = nil })
	return NewTestServer(t)
}

// runMigrationUp executes the Up section of an embedded migration file inside the test transaction, so a
// data migration can be tested against seeded rows (schema changes of the file must already be applied).
func (s *TestServer) runMigrationUp(file string) {
	s.t.Helper()
	b, err := migrations.FS.ReadFile(file)
	require.NoError(s.t, err)
	up := string(b)
	if i := strings.Index(up, "-- +goose Down"); i >= 0 {
		up = up[:i]
	}
	_, err = s.Tx.Exec(context.Background(), up)
	require.NoError(s.t, err)
}
