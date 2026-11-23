//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type historyBody struct {
	Currency string `json:"currency"`
	Range    string `json:"range"`
	Points   []struct {
		Date     string `json:"date"`
		NetWorth string `json:"netWorth"`
		Live     bool   `json:"live"`
	} `json:"points"`
	ChangePct        *string `json:"changePct"`
	NetContributions string  `json:"netContributions"`
	MarketChange     string  `json:"marketChange"`
	PerformancePct   *string `json:"performancePct"`
}

func (s *TestServer) history(u session, sid, query string) historyBody {
	s.t.Helper()
	res := s.Get("/api/v1/spaces/"+sid+"/net-worth/history"+query, bearer(u.AccessToken))
	require.Equal(s.t, http.StatusOK, res.Status, string(res.Body))
	var h historyBody
	res.JSON(s.t, &h)
	return h
}

func (s *TestServer) seedSnapshot(sid, date, currency, netWorth string) {
	s.t.Helper()
	_, err := s.Tx.Exec(context.Background(), `
		INSERT INTO net_worth_snapshots (space_id, date, currency, cash, investments, fixed_assets, receivables, liabilities, net_worth)
		VALUES ($1, $2::date, $3, $4::numeric, 0, 0, 0, 0, $4::numeric)
		ON CONFLICT (space_id, date) DO UPDATE SET net_worth = EXCLUDED.net_worth, currency = EXCLUDED.currency`,
		sid, date, currency, netWorth)
	require.NoError(s.t, err)
}

func (s *TestServer) snapshotCount(sid string) int {
	s.t.Helper()
	var n int
	require.NoError(s.t, s.Tx.QueryRow(context.Background(), `SELECT count(*) FROM net_worth_snapshots WHERE space_id = $1`, sid).Scan(&n))
	return n
}

func TestHistoryOfANewSpaceIsOneLivePoint(t *testing.T) {
	s := NewTestServer(t)
	s.Clock.Set(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC))
	w := newWorld(t, s) // 1000 + 500 USD

	h := s.history(w.u, w.sid, "")
	assert.Equal(t, "USD", h.Currency)
	assert.Equal(t, "1M", h.Range, "the default range")
	require.Len(t, h.Points, 1)
	assert.Equal(t, "2026-11-18", h.Points[0].Date)
	assert.True(t, h.Points[0].Live)
	assert.True(t, dec("1500").Equal(dec(h.Points[0].NetWorth)))
	assert.Nil(t, h.ChangePct, "one point has no change")
	assert.Equal(t, 0, s.snapshotCount(w.sid), "reading the history stores nothing")
}

func TestHistoryRangesAndChange(t *testing.T) {
	s := NewTestServer(t)
	s.Clock.Set(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC))
	w := newWorld(t, s)
	s.seedSnapshot(w.sid, "2026-04-01", "USD", "500")  // outside 6M (from 18 May)
	s.seedSnapshot(w.sid, "2026-06-01", "USD", "800")  // within 6M, not 3M
	s.seedSnapshot(w.sid, "2026-09-01", "USD", "1000") // within 3M (from 18 Aug), not 1M
	s.seedSnapshot(w.sid, "2026-10-20", "USD", "1200")
	s.seedSnapshot(w.sid, "2026-11-17", "USD", "1250")
	s.seedSnapshot(w.sid, "2025-11-01", "USD", "100") // outside 1Y

	dates := func(h historyBody) []string {
		var d []string
		for _, p := range h.Points {
			d = append(d, p.Date)
		}
		return d
	}
	assert.Equal(t, []string{"2026-10-20", "2026-11-17", "2026-11-18"}, dates(s.history(w.u, w.sid, "?range=1M")))
	assert.Equal(t, []string{"2026-09-01", "2026-10-20", "2026-11-17", "2026-11-18"}, dates(s.history(w.u, w.sid, "?range=3M")))
	assert.Equal(t, []string{"2026-06-01", "2026-09-01", "2026-10-20", "2026-11-17", "2026-11-18"}, dates(s.history(w.u, w.sid, "?range=6M")))
	assert.Len(t, s.history(w.u, w.sid, "?range=1Y").Points, 6, "2026-04-01 and later")
	all := s.history(w.u, w.sid, "?range=all")
	assert.Equal(t, "ALL", all.Range)
	assert.Len(t, all.Points, 7)
	assert.Equal(t, "2025-11-01", all.Points[0].Date)

	h := s.history(w.u, w.sid, "?range=3M")
	require.NotNil(t, h.ChangePct)
	assert.True(t, dec("50").Equal(dec(*h.ChangePct)), "1000 → 1500: +50%%, got %s", *h.ChangePct)
	assert.True(t, h.Points[len(h.Points)-1].Live)

	t.Run("a snapshot of today is replaced by the live value", func(t *testing.T) {
		s.seedSnapshot(w.sid, "2026-11-18", "USD", "42")
		h := s.history(w.u, w.sid, "?range=1M")
		require.Len(t, h.Points, 3)
		assert.True(t, dec("1500").Equal(dec(h.Points[2].NetWorth)))
		assert.True(t, h.Points[2].Live)
	})
	t.Run("snapshots in another base currency are left out", func(t *testing.T) {
		s.seedSnapshot(w.sid, "2026-11-10", "EUR", "99999")
		h := s.history(w.u, w.sid, "?range=1M")
		assert.Len(t, h.Points, 3)
		for _, p := range h.Points {
			assert.NotEqual(t, "2026-11-10", p.Date)
		}
	})
	t.Run("an invalid range is a 400", func(t *testing.T) {
		for _, q := range []string{"?range=2W", "?range=", "?range=forever"} {
			res := s.Get("/api/v1/spaces/"+w.sid+"/net-worth/history"+q, bearer(w.u.AccessToken))
			if q == "?range=" {
				assert.Equal(t, http.StatusOK, res.Status, "empty means the default")
				continue
			}
			assert.Equal(t, http.StatusBadRequest, res.Status, q)
		}
	})
	t.Run("a zero first value gives a null change", func(t *testing.T) {
		s.seedSnapshot(w.sid, "2025-10-01", "USD", "0")
		assert.Nil(t, s.history(w.u, w.sid, "?range=ALL").ChangePct)
	})
}

func TestHistoryIsDownsampled(t *testing.T) {
	s := NewTestServer(t)
	s.Clock.Set(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC))
	w := newWorld(t, s)
	_, err := s.Tx.Exec(context.Background(), `
		INSERT INTO net_worth_snapshots (space_id, date, currency, cash, investments, fixed_assets, receivables, liabilities, net_worth)
		SELECT $1, d::date, 'USD', 1, 0, 0, 0, 0, 1
		FROM generate_series('2026-01-01'::date, '2026-11-17'::date, interval '1 day') d`, w.sid)
	require.NoError(t, err)
	h := s.history(w.u, w.sid, "?range=1Y")
	assert.LessOrEqual(t, len(h.Points), 120)
	assert.Equal(t, "2026-01-01", h.Points[0].Date, "the first point is kept")
	assert.Equal(t, "2026-11-18", h.Points[len(h.Points)-1].Date, "and so is the last")
	assert.True(t, h.Points[len(h.Points)-1].Live)
	for i := 1; i < len(h.Points); i++ {
		assert.Less(t, h.Points[i-1].Date, h.Points[i].Date, "still in order")
	}
}

func TestSnapshotJob(t *testing.T) {
	s := NewTestServer(t)
	s.Clock.Set(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC))
	e, _ := buildFixture(t, s) // net worth 12775
	other := s.register("second@example.com")
	otherSpace := spaceIDOf(other)
	log := zerolog.Nop()

	res, err := s.Networth.SnapshotAll(context.Background(), log)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Items, "both spaces")
	assert.Equal(t, 1, s.snapshotCount(e.sid))
	assert.Equal(t, 1, s.snapshotCount(otherSpace))

	var cash, inv, fixed, recv, liab, nw string
	var cur string
	require.NoError(t, s.Tx.QueryRow(context.Background(), `
		SELECT currency, cash::text, investments::text, fixed_assets::text, receivables::text, liabilities::text, net_worth::text
		FROM net_worth_snapshots WHERE space_id = $1 AND date = '2026-11-18'`, e.sid).Scan(&cur, &cash, &inv, &fixed, &recv, &liab, &nw))
	assert.Equal(t, "USD", cur)
	assert.True(t, dec("2000").Equal(dec(cash)), cash)
	assert.True(t, dec("1150").Equal(dec(inv)), inv)
	assert.True(t, dec("10000").Equal(dec(fixed)), fixed)
	assert.True(t, dec("125").Equal(dec(recv)), recv)
	assert.True(t, dec("500").Equal(dec(liab)), liab)
	assert.True(t, dec("12775").Equal(dec(nw)), nw)

	var runs int
	var status string
	require.NoError(t, s.Tx.QueryRow(context.Background(), `SELECT count(*), max(status) FROM job_runs WHERE name = 'networth.snapshot'`).Scan(&runs, &status))
	assert.Equal(t, 1, runs, "a job_runs row is written")
	assert.Equal(t, "SUCCESS", status)

	t.Run("idempotent within a day", func(t *testing.T) {
		_, err := s.Networth.SnapshotAll(context.Background(), log)
		require.NoError(t, err)
		assert.Equal(t, 1, s.snapshotCount(e.sid))
		// and it refreshes today's row
		require.Equal(t, http.StatusOK, s.Do(http.MethodPut, walURL(e.sid, "/", e.a.ID, "/balance"), map[string]any{"newBalance": "1100"}, bearer(e.u.AccessToken)).Status)
		_, err = s.Networth.SnapshotAll(context.Background(), log)
		require.NoError(t, err)
		require.NoError(t, s.Tx.QueryRow(context.Background(), `SELECT net_worth::text FROM net_worth_snapshots WHERE space_id = $1`, e.sid).Scan(&nw))
		assert.True(t, dec("12875").Equal(dec(nw)), nw)
	})
	t.Run("a new day adds a row", func(t *testing.T) {
		s.Clock.Set(time.Date(2026, 11, 19, 0, 20, 0, 0, time.UTC))
		_, err := s.Networth.SnapshotAll(context.Background(), log)
		require.NoError(t, err)
		assert.Equal(t, 2, s.snapshotCount(e.sid))
		h := s.history(e.u, e.sid, "?range=1M")
		require.Len(t, h.Points, 2)
		assert.Equal(t, "2026-11-18", h.Points[0].Date)
		assert.Equal(t, "2026-11-19", h.Points[1].Date)
		assert.True(t, h.Points[1].Live)
	})
	t.Run("a deleted space is skipped", func(t *testing.T) {
		before := s.snapshotCount(otherSpace)
		_, err := s.Tx.Exec(context.Background(), `UPDATE spaces SET deleted_at = now() WHERE id = $1`, otherSpace)
		require.NoError(t, err)
		s.Clock.Set(time.Date(2026, 11, 20, 0, 20, 0, 0, time.UTC))
		res, err := s.Networth.SnapshotAll(context.Background(), log)
		require.NoError(t, err)
		assert.Equal(t, 1, res.Items)
		assert.Equal(t, before, s.snapshotCount(otherSpace), "nothing new")
		assert.Equal(t, 3, s.snapshotCount(e.sid))
	})
	t.Run("the UTC date decides the day", func(t *testing.T) {
		require.Equal(t, http.StatusOK, s.Do(http.MethodPatch, "/api/v1/me", map[string]any{"timezone": "Asia/Jakarta"}, bearer(e.u.AccessToken)).Status)
		s.Clock.Set(time.Date(2026, 11, 20, 23, 0, 0, 0, time.UTC)) // 06:00 on the 21st in Jakarta
		_, err := s.Networth.SnapshotAll(context.Background(), log)
		require.NoError(t, err)
		var n int
		require.NoError(t, s.Tx.QueryRow(context.Background(), `SELECT count(*) FROM net_worth_snapshots WHERE space_id = $1 AND date = '2026-11-20'`, e.sid).Scan(&n))
		assert.Equal(t, 1, n)
	})
}

func TestSnapshotOfACashOnlySpace(t *testing.T) {
	s := NewTestServer(t)
	s.Clock.Set(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC))
	e, _ := buildFixture(t, s)
	_, err := s.Tx.Exec(context.Background(), `UPDATE spaces SET tracking_mode = 'CASH_ONLY' WHERE id = $1`, e.sid)
	require.NoError(t, err)
	_, err = s.Networth.SnapshotAll(context.Background(), zerolog.Nop())
	require.NoError(t, err)
	var inv, fixed, recv, liab, nw string
	require.NoError(t, s.Tx.QueryRow(context.Background(), `
		SELECT investments::text, fixed_assets::text, receivables::text, liabilities::text, net_worth::text FROM net_worth_snapshots WHERE space_id = $1`, e.sid).
		Scan(&inv, &fixed, &recv, &liab, &nw))
	for _, v := range []string{inv, fixed, recv, liab} {
		assert.True(t, dec("0").Equal(dec(v)), "the parts beyond cash are 0, got %s", v)
	}
	assert.True(t, dec("2000").Equal(dec(nw)), "net worth is cash: %s", nw)
}

func TestHistoryIsSpaceScoped(t *testing.T) {
	s := NewTestServer(t)
	w := newWorld(t, s)
	outsider := s.register("outsider@example.com")
	url := "/api/v1/spaces/" + w.sid + "/net-worth/history"
	assert.Equal(t, http.StatusUnauthorized, s.Get(url).Status)
	expectCrossSpace404(t, s, outsider, http.MethodGet, url, nil)
}

// A deposit is not growth: a top up moves the value and the contributions, not the market change.
func TestHistoryContributionsVersusMarketChange(t *testing.T) {
	s := NewTestServer(t)
	s.Clock.Set(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	w := newWorld(t, s) // 1500 USD, opened on the 10th
	s.seedSnapshot(w.sid, "2026-11-11", "USD", "1500")
	s.Clock.Set(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC))

	t.Run("no deposit and no price move: all zero", func(t *testing.T) {
		h := s.history(w.u, w.sid, "?range=1M")
		assert.True(t, dec("0").Equal(dec(h.NetContributions)))
		assert.True(t, dec("0").Equal(dec(h.MarketChange)))
		require.NotNil(t, h.PerformancePct)
		assert.True(t, dec("0").Equal(dec(*h.PerformancePct)))
	})

	t.Run("a top up is a contribution, the market change stays 0", func(t *testing.T) {
		require.Equal(t, http.StatusCreated, s.Do(http.MethodPost, walURL(w.sid, "/", w.a.ID, "/top-up"), map[string]any{"amount": "500"}, bearer(w.u.AccessToken)).Status)
		h := s.history(w.u, w.sid, "?range=1M")
		require.NotNil(t, h.ChangePct)
		assert.True(t, dec("33.33").Equal(dec(*h.ChangePct)), "the value grew by a third: %s", *h.ChangePct)
		assert.True(t, dec("500").Equal(dec(h.NetContributions)), h.NetContributions)
		assert.True(t, dec("0").Equal(dec(h.MarketChange)), h.MarketChange)
		require.NotNil(t, h.PerformancePct)
		assert.True(t, dec("0").Equal(dec(*h.PerformancePct)))
	})

	t.Run("a price move is market change, the contributions do not move", func(t *testing.T) {
		s.seedSnapshot(w.sid, "2026-11-11", "USD", "2000") // baseline now includes the top up
		_, err := s.Tx.Exec(context.Background(), `UPDATE transactions SET occurred_at = $2 WHERE space_id = $1 AND system_key = 'BALANCE_TOP_UP'`, w.sid, time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC))
		require.NoError(t, err)
		// the cash now sits at 2000; an extra balance edit is not a deposit either way: use a gain via an INCOME
		w.must(map[string]any{"type": "INCOME", "walletId": w.a.ID, "amount": "100", "title": "Interest"})
		h := s.history(w.u, w.sid, "?range=1M")
		assert.True(t, dec("0").Equal(dec(h.NetContributions)), h.NetContributions)
		assert.True(t, dec("100").Equal(dec(h.MarketChange)), h.MarketChange)
		require.NotNil(t, h.PerformancePct)
		assert.True(t, dec("5").Equal(dec(*h.PerformancePct)), *h.PerformancePct)
	})
}
