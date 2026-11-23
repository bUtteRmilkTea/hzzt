package networth

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog"
	"github.com/shopspring/decimal"

	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
	"github.com/JonathanRainee/hzzt/internal/marketdata/jobs"
	"github.com/JonathanRainee/hzzt/internal/platform/apperror"
)

// JobSnapshot is the job_runs name of the daily snapshot.
const JobSnapshot = "networth.snapshot"

// SnapshotSchedule runs the snapshot daily at 00:15 UTC.
const SnapshotSchedule = "15 0 * * *"

// MaxHistoryPoints bounds how many points a history response carries.
const MaxHistoryPoints = 120

// SetRunner installs the job runner used by SnapshotAll.
func (s *Service) SetRunner(r *jobs.Runner) { s.runner = r }

// utcDate is the UTC calendar date of t (TD-04: snapshots belong to UTC days).
func utcDate(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// snapshotSpace computes and stores today's snapshot of one space (an upsert, so
// running it twice in a day just refreshes the row).
func (s *Service) snapshotSpace(ctx context.Context, q *sqlc.Queries, spaceID uuid.UUID, now time.Time) error {
	sum, err := s.Compute(ctx, spaceID, &now)
	if err != nil {
		return err
	}
	zero := decimal.Zero
	val := func(d *decimal.Decimal) decimal.Decimal {
		if d == nil {
			return zero
		}
		return *d
	}
	inv := zero
	if sum.Investments != nil {
		inv = sum.Investments.Value
	}
	return q.UpsertNetWorthSnapshot(ctx, sqlc.UpsertNetWorthSnapshotParams{
		SpaceID: spaceID, Date: utcDate(now), Currency: sum.Currency, Cash: sum.Cash, Investments: inv,
		FixedAssets: val(sum.FixedAssets), Receivables: val(sum.Receivables), Liabilities: val(sum.Liabilities), NetWorth: sum.NetWorth,
	})
}

// SnapshotAll stores today's (UTC date) net-worth snapshot of every non-deleted
// space. It is idempotent per day; one failing space is logged and skipped, and
// only a run in which nothing succeeded counts as failed.
func (s *Service) SnapshotAll(ctx context.Context, log zerolog.Logger) (jobs.Result, error) {
	if s.runner == nil {
		return jobs.Result{}, fmt.Errorf("networth: no job runner configured")
	}
	return s.runner.Run(ctx, JobSnapshot, func(ctx context.Context, tx pgx.Tx) (int, error) {
		q := sqlc.New(tx)
		ids, err := q.ListActiveSpaceIDs(ctx)
		if err != nil {
			return 0, err
		}
		now := s.now()
		done := 0
		var lastErr error
		for _, id := range ids {
			// a savepoint per space: one failure cannot poison the others
			if err := pgxNested(ctx, tx, func(sp pgx.Tx) error { return s.snapshotSpace(ctx, sqlc.New(sp), id, now) }); err != nil {
				log.Error().Err(err).Str("spaceId", id.String()).Msg("net-worth snapshot failed")
				lastErr = err
				continue
			}
			done++
		}
		if done == 0 && lastErr != nil {
			return 0, lastErr
		}
		return done, nil
	})
}

func pgxNested(ctx context.Context, tx pgx.Tx, fn func(pgx.Tx) error) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(sp); err != nil {
		_ = sp.Rollback(context.WithoutCancel(ctx))
		return err
	}
	return sp.Commit(ctx)
}

// StartJobs schedules the daily snapshot (00:15 UTC) and runs one at startup so a
// fresh deployment has today's point. The returned function stops the scheduler.
func (s *Service) StartJobs(ctx context.Context, log zerolog.Logger) (stop func(), err error) {
	c := cron.New(cron.WithLocation(time.UTC), cron.WithChain(cron.SkipIfStillRunning(cron.DiscardLogger)))
	run := func() {
		jctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		if _, err := s.SnapshotAll(jctx, log); err != nil {
			log.Error().Err(err).Str("job", JobSnapshot).Msg("net-worth snapshot job failed")
		}
	}
	if _, err := c.AddFunc(SnapshotSchedule, run); err != nil {
		return nil, err
	}
	c.Start()
	go run()
	return func() { <-c.Stop().Done() }, nil
}

// Point is one day of the net-worth history.
type Point struct {
	Date     string
	NetWorth decimal.Decimal
	// Investments is the portfolio value of that day, in the same currency as NetWorth.
	Investments decimal.Decimal
	// Live marks the point computed now for today (rather than read from a snapshot).
	Live bool
}

// History is the net-worth graph of a space.
type History struct {
	Currency string
	Range    string
	Points   []Point
	// ChangePct is the percentage change from the first to the last point; nil with
	// fewer than two points or a first value of 0.
	ChangePct *decimal.Decimal
	// InvestmentsChangePct is the same for the portfolio value.
	InvestmentsChangePct *decimal.Decimal
}

// Downsample keeps at most max points, always the first and the last, picking the
// rest evenly across the series.
func Downsample(points []Point, max int) []Point {
	n := len(points)
	if max < 2 || n <= max {
		return points
	}
	out := make([]Point, 0, max)
	last := -1
	for i := 0; i < max; i++ {
		idx := (i*(n-1) + (max-1)/2) / (max - 1)
		if i == max-1 {
			idx = n - 1
		}
		if idx != last {
			out = append(out, points[idx])
			last = idx
		}
	}
	return out
}

// rangeStart returns the first date of a range label, or ok=false for ALL.
func rangeStart(rng string, today time.Time) (from time.Time, ok bool, err error) {
	switch strings.ToUpper(strings.TrimSpace(rng)) {
	case "1M":
		return today.AddDate(0, -1, 0), true, nil
	case "3M":
		return today.AddDate(0, -3, 0), true, nil
	case "6M":
		return today.AddDate(0, -6, 0), true, nil
	case "1Y":
		return today.AddDate(-1, 0, 0), true, nil
	case "ALL":
		return time.Time{}, false, nil
	default:
		return time.Time{}, false, apperror.Validation("Validation failed", map[string]any{"range": "must be one of: 1M 3M 6M 1Y ALL"})
	}
}

// History returns the stored daily snapshots of the range (in the space's current
// base currency) plus a live point for today, downsampled to at most about 120
// points. A space created today has just the live point.
func (s *Service) History(ctx context.Context, spaceID uuid.UUID, rng string) (History, error) {
	today := utcDate(s.now())
	from, bounded, err := rangeStart(rng, today)
	if err != nil {
		return History{}, err
	}
	live, err := s.Compute(ctx, spaceID, nil)
	if err != nil {
		return History{}, err
	}
	var fromDate *time.Time
	if bounded {
		fromDate = &from
	}
	snaps, err := s.q.ListNetWorthSnapshots(ctx, sqlc.ListNetWorthSnapshotsParams{SpaceID: spaceID, FromDate: fromDate})
	if err != nil {
		return History{}, err
	}
	todayStr := today.Format("2006-01-02")
	h := History{Currency: live.Currency, Range: strings.ToUpper(strings.TrimSpace(rng))}
	for _, sn := range snaps {
		if strings.TrimSpace(sn.Currency) != live.Currency { // another base currency: not comparable
			continue
		}
		d := sn.Date.Format("2006-01-02")
		if d == todayStr { // replaced by the live point below
			continue
		}
		h.Points = append(h.Points, Point{Date: d, NetWorth: sn.NetWorth, Investments: sn.Investments})
	}
	liveInv := decimal.Zero
	if live.Investments != nil {
		liveInv = live.Investments.Value
	}
	h.Points = append(h.Points, Point{Date: todayStr, NetWorth: live.NetWorth, Investments: liveInv, Live: true})
	h.Points = Downsample(h.Points, MaxHistoryPoints)

	if len(h.Points) >= 2 {
		pctOf := func(first, last decimal.Decimal) *decimal.Decimal {
			if first.IsZero() {
				return nil
			}
			pct := last.Sub(first).Div(first.Abs()).Mul(decimal.NewFromInt(100)).Round(2)
			return &pct
		}
		n := len(h.Points) - 1
		h.ChangePct = pctOf(h.Points[0].NetWorth, h.Points[n].NetWorth)
		h.InvestmentsChangePct = pctOf(h.Points[0].Investments, h.Points[n].Investments)
	}
	return h, nil
}
