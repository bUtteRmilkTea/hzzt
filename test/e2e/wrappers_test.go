//go:build e2e

package e2e

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JonathanRainee/hzzt/internal/currency"
	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
	"github.com/JonathanRainee/hzzt/internal/investments"
	"github.com/JonathanRainee/hzzt/internal/platform/apperror"
)

type targetBody struct {
	AssetType string  `json:"assetType"`
	Wrapper   *string `json:"wrapper"`
	Key       string  `json:"key"`
	TargetPct *string `json:"targetPct"`
	ActualPct *string `json:"actualPct"`
	DriftPct  *string `json:"driftPct"`
	Category  struct {
		AssignedPct    string `json:"assignedPct"`
		UnallocatedPct string `json:"unallocatedPct"`
	} `json:"category"`
}

type allocationsBody struct {
	Categories []struct {
		AssetType      string `json:"assetType"`
		AssignedPct    string `json:"assignedPct"`
		UnallocatedPct string `json:"unallocatedPct"`
		Wrappers       []struct {
			Key       string  `json:"key"`
			Wrapper   *string `json:"wrapper"`
			TargetPct *string `json:"targetPct"`
			ActualPct *string `json:"actualPct"`
			DriftPct  *string `json:"driftPct"`
		} `json:"wrappers"`
	} `json:"categories"`
}

func wrapURL(sid string, rest ...string) string {
	p := invURL(sid, "/wrappers")
	for _, r := range rest {
		p += r
	}
	return p
}

func (e *invEnv) setTarget(assetType, key string, pct any) Response {
	return e.s.Do(http.MethodPatch, wrapURL(e.sid, "/", assetType, "/", key), map[string]any{"targetPct": pct}, bearer(e.u.AccessToken))
}

func (e *invEnv) mustTarget(assetType, key string, pct any) targetBody {
	e.s.t.Helper()
	res := e.setTarget(assetType, key, pct)
	require.Equal(e.s.t, http.StatusOK, res.Status, string(res.Body))
	var b targetBody
	res.JSON(e.s.t, &b)
	return b
}

func (e *invEnv) allocations() allocationsBody {
	e.s.t.Helper()
	res := e.s.Get(wrapURL(e.sid), bearer(e.u.AccessToken))
	require.Equal(e.s.t, http.StatusOK, res.Status, string(res.Body))
	var a allocationsBody
	res.JSON(e.s.t, &a)
	return a
}

func TestWrapperTargets(t *testing.T) {
	s := NewTestServer(t)
	e := newInvEnv(s)
	e.setSpace(map[string]any{"baseCurrency": "USD"})
	e.must(crypto("Bitcoin", "Binance", "1", "300"))
	e.must(crypto("Ether", "Ledger Nano", "1", "100"))
	e.must(crypto("Solana", "Kraken", "1", "100"))
	e.must(map[string]any{"assetType": "US_STOCK", "name": "Apple", "symbol": "AAPL", "quantity": "1", "manualPrice": "10", "wrapper": "Robinhood",
		"details": map[string]any{"exchange": "NASDAQ"}})

	t.Run("unset wrappers have no percentage, not 0", func(t *testing.T) {
		a := e.allocations()
		require.Len(t, a.Categories, 2)
		c := a.Categories[0]
		assert.Equal(t, "0.00", c.AssignedPct)
		assert.Equal(t, "100.00", c.UnallocatedPct)
		require.Len(t, c.Wrappers, 3)
		for _, w := range c.Wrappers {
			assert.Nil(t, w.TargetPct)
			assert.Nil(t, w.DriftPct)
			require.NotNil(t, w.ActualPct)
		}
		assert.Equal(t, "60.00", *c.Wrappers[0].ActualPct, "300 of 500")
	})

	t.Run("set, with the remainder reported", func(t *testing.T) {
		b := e.mustTarget("CRYPTO", "binance", "40")
		assert.Equal(t, "binance", b.Key)
		assert.Equal(t, "Binance", *b.Wrapper)
		assert.Equal(t, "40.00", *b.TargetPct)
		assert.Equal(t, "60.00", *b.ActualPct)
		assert.Equal(t, "20.00", *b.DriftPct)
		assert.Equal(t, "40.00", b.Category.AssignedPct)
		assert.Equal(t, "60.00", b.Category.UnallocatedPct)
		b = e.mustTarget("CRYPTO", "ledger%20nano", "30.5") // url-encoded key
		assert.Equal(t, "Ledger Nano", *b.Wrapper)
		assert.Equal(t, "70.50", b.Category.AssignedPct)
		assert.Equal(t, "29.50", b.Category.UnallocatedPct)
	})

	t.Run("above 100 is 422 with what is available", func(t *testing.T) {
		res := e.setTarget("CRYPTO", "kraken", "30")
		require.Equal(t, http.StatusUnprocessableEntity, res.Status, string(res.Body))
		var env struct {
			Error struct {
				Code    string            `json:"code"`
				Details map[string]string `json:"details"`
			} `json:"error"`
		}
		res.JSON(t, &env)
		assert.Equal(t, "ALLOCATION_EXCEEDS_100", env.Error.Code)
		assert.Equal(t, "70.50", env.Error.Details["assigned"])
		assert.Equal(t, "29.50", env.Error.Details["available"])
		assert.Nil(t, e.allocations().Categories[0].Wrappers[1].TargetPct, "nothing was stored (kraken)")
		assert.Equal(t, "0.00", e.mustTarget("CRYPTO", "kraken", "29.5").Category.UnallocatedPct, "exactly what is left fits")
	})

	t.Run("updating excludes the wrapper's own old value; clearing frees it", func(t *testing.T) {
		assert.Equal(t, http.StatusUnprocessableEntity, e.setTarget("CRYPTO", "binance", "45").Status, "60 elsewhere + 45")
		assert.Equal(t, "0.00", e.mustTarget("CRYPTO", "binance", "40.00").Category.UnallocatedPct, "same value again")
		b := e.mustTarget("CRYPTO", "binance", "35")
		assert.Equal(t, "95.00", b.Category.AssignedPct)
		assert.Equal(t, "5.00", b.Category.UnallocatedPct)
		b = e.mustTarget("CRYPTO", "KRAKEN", nil) // any casing
		assert.Nil(t, b.TargetPct)
		assert.Equal(t, "65.50", b.Category.AssignedPct)
		assert.Equal(t, "34.50", b.Category.UnallocatedPct)
		assert.Nil(t, e.mustTarget("CRYPTO", "kraken", nil).TargetPct, "clearing twice is harmless")
		z := e.mustTarget("CRYPTO", "binance", "0")
		require.NotNil(t, z.TargetPct)
		assert.Equal(t, "0.00", *z.TargetPct, "zero is a valid, set percentage")
		e.mustTarget("CRYPTO", "binance", "40")
	})

	t.Run("a wrapper that lost its investments keeps its percentage, listed with 0.00 actual", func(t *testing.T) {
		eth := e.must(crypto("Cold", "Trezor", "1", "10"))
		e.mustTarget("CRYPTO", "trezor", "10")
		require.Equal(t, http.StatusOK, e.patch(eth.ID, map[string]any{"wrapper": "Moved"}).Status)
		var orphan *struct {
			Key       string  `json:"key"`
			Wrapper   *string `json:"wrapper"`
			TargetPct *string `json:"targetPct"`
			ActualPct *string `json:"actualPct"`
			DriftPct  *string `json:"driftPct"`
		}
		for _, w := range e.allocations().Categories[0].Wrappers {
			w := w
			if w.Key == "trezor" {
				orphan = &w
			}
		}
		require.NotNil(t, orphan)
		assert.Equal(t, "10.00", *orphan.TargetPct)
		assert.Equal(t, "0.00", *orphan.ActualPct)
		assert.Equal(t, "-10.00", *orphan.DriftPct)
		b := e.mustTarget("CRYPTO", "trezor", nil)
		assert.Nil(t, b.TargetPct)
		assert.Equal(t, http.StatusNotFound, e.setTarget("CRYPTO", "trezor", "5").Status, "no investment and no row any more")
	})

	t.Run("unknown wrapper and bad input", func(t *testing.T) {
		res := e.setTarget("CRYPTO", "nowhere", "10")
		require.Equal(t, http.StatusNotFound, res.Status)
		assert.Equal(t, "WRAPPER_NOT_FOUND", errCode(t, res))
		assert.Equal(t, http.StatusNotFound, e.setTarget("ID_STOCK", "binance", "10").Status, "binance holds no ID_STOCK")
		for name, c := range map[string]struct {
			assetType, key string
			body           any
		}{
			"3 decimals": {"CRYPTO", "binance", map[string]any{"targetPct": "12.345"}},
			"over 100":   {"CRYPTO", "binance", map[string]any{"targetPct": "100.01"}},
			"negative":   {"CRYPTO", "binance", map[string]any{"targetPct": "-1"}},
			"not a pct":  {"CRYPTO", "binance", map[string]any{"targetPct": "abc"}},
			"a number":   {"CRYPTO", "binance", map[string]any{"targetPct": 40}},
			"missing":    {"CRYPTO", "binance", map[string]any{}},
			"unknown":    {"CRYPTO", "binance", map[string]any{"targetPct": "1", "x": 1}},
			"asset type": {"NOPE", "binance", map[string]any{"targetPct": "1"}},
			"unassigned": {"CRYPTO", "_unassigned", map[string]any{"targetPct": "1"}},
			"blank key":  {"CRYPTO", "%20", map[string]any{"targetPct": "1"}},
		} {
			res := s.Do(http.MethodPatch, wrapURL(e.sid, "/", c.assetType, "/", c.key), c.body, bearer(e.u.AccessToken))
			assert.Equal(t, http.StatusBadRequest, res.Status, "%s: %s", name, res.Body)
		}
		assert.Equal(t, http.StatusUnauthorized, s.Do(http.MethodPatch, wrapURL(e.sid, "/CRYPTO/binance"), map[string]any{"targetPct": "1"}).Status)
	})

	t.Run("space members only; any member may edit", func(t *testing.T) {
		outsider := s.register("out@example.com")
		expectCrossSpace404(t, s, outsider, http.MethodPatch, wrapURL(e.sid, "/CRYPTO/binance"), map[string]any{"targetPct": "1"})
		expectCrossSpace404(t, s, outsider, http.MethodGet, wrapURL(e.sid), nil)
		member := s.register("member@example.com")
		s.join(e.u, e.sid, member)
		res := s.Do(http.MethodPatch, wrapURL(e.sid, "/CRYPTO/binance"), map[string]any{"targetPct": "20"}, bearer(member.AccessToken))
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		assert.Equal(t, http.StatusOK, s.Get(wrapURL(e.sid), bearer(member.AccessToken)).Status)
	})

	t.Run("the grouped view embeds the same fields", func(t *testing.T) {
		g := s.grouped(e.u, e.sid, "")
		c := g.Categories[0]
		assert.Equal(t, "CRYPTO", c.AssetType)
		assert.Equal(t, "20.00", *c.Wrappers[0].TargetPct)
		assert.Equal(t, "38.82", *c.Wrappers[0].DriftPct, "300 of 510 is 58.82%")
		assert.Equal(t, "50.50", c.AssignedPct)
		assert.Equal(t, "49.50", c.UnallocatedPct)
	})
}

// The e2e harness runs each test on one transaction (one connection), so real concurrency is
// tested here against committed rows of the test database, which are removed afterwards.
func TestConcurrentWrapperEditsCannotExceed100(t *testing.T) {
	ctx := context.Background()
	var userID, spaceID uuid.UUID
	require.NoError(t, testPool.QueryRow(ctx, `INSERT INTO users (email, password_hash, name) VALUES ($1, 'x', 'Race') RETURNING id`,
		"race-"+uuid.NewString()+"@example.com").Scan(&userID))
	require.NoError(t, testPool.QueryRow(ctx, `INSERT INTO spaces (name, created_by_id) VALUES ('Race', $1) RETURNING id`, userID).Scan(&spaceID))
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM spaces WHERE id = $1`, spaceID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM fx_rates`) // the FX tests expect an empty table
	})
	q := sqlc.New(testPool)
	keys := []string{"a", "b", "c", "d", "e", "f"}
	for _, k := range keys {
		w := k
		_, err := q.CreateInvestment(ctx, sqlc.CreateInvestmentParams{
			SpaceID: spaceID, AssetType: "US_STOCK", PriceMode: "MANUAL", Name: "Stock " + k, Symbol: &w, Icon: "x", QuoteCurrency: "USD",
			Quantity: dec("1"), AvgBuyPrice: dec("10"), ManualPrice: dec("10"), Fees: dec("0"), EstimatedAnnualChangePct: nullDec("5.5"),
			Details: []byte(`{"exchange":"NASDAQ"}`), CreatedByID: userID, Wrapper: &w,
		})
		require.NoError(t, err)
	}
	clock := &testClock{}
	cur := currency.NewService(testPool, currency.Options{Provider: &FakeFX{Set: defaultRates(clock)}, Log: zerolog.Nop(), Now: clock.Now})
	require.NoError(t, cur.Refresh(ctx))
	svc := investments.NewService(testPool, cur, clock.Now)

	var wg sync.WaitGroup
	errs := make([]error, len(keys))
	for i, k := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			target := dec("30")
			_, _, errs[i] = svc.SetWrapperTarget(ctx, spaceID, userID, "US_STOCK", k, &target)
		}()
	}
	wg.Wait()
	ok, rejected := 0, 0
	for _, err := range errs {
		var ae *apperror.AppError
		switch {
		case err == nil:
			ok++
		case errors.As(err, &ae) && ae.Code == "ALLOCATION_EXCEEDS_100":
			rejected++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	assert.Equal(t, 3, ok, "%v", errs)
	assert.Equal(t, 3, rejected, "%v", errs)
	var assigned int
	require.NoError(t, testPool.QueryRow(ctx, `SELECT COALESCE(sum(pct_bps), 0) FROM investment_wrapper_allocations WHERE space_id = $1`, spaceID).Scan(&assigned))
	assert.Equal(t, 9000, assigned)
}
