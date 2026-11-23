package investments

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/JonathanRainee/hzzt/internal/currency"
	"github.com/JonathanRainee/hzzt/internal/db"
	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
	"github.com/JonathanRainee/hzzt/internal/ledger"
	"github.com/JonathanRainee/hzzt/internal/platform/apperror"
)

// DB is what the service needs; *pgxpool.Pool and pgx.Tx both satisfy it.
type DB interface {
	sqlc.DBTX
	db.TxStarter
}

// PriceSource supplies latest market prices (P11 CoinMarketCap, P12 Massive).
// Until a source is installed every AUTO investment falls back to its manual price.
type PriceSource interface {
	MarketPrices(ctx context.Context, instrumentIDs []uuid.UUID) (map[uuid.UUID]MarketPrice, error)
}

// Market is the market-data capability: latest prices, linking crypto
// investments to coins, and on-demand price refresh. It is installed by
// SetMarket; without it AUTO investments use their manual price.
type Market interface {
	PriceSource
	// ResolveInstrument returns the instrument that prices an investment of the
	// asset type: the explicit one (picked from search) when given, otherwise the
	// symbol resolved to the highest-ranked coin / exact ticker.
	ResolveInstrument(ctx context.Context, assetType string, explicit *uuid.UUID, symbol string) (uuid.UUID, error)
	// EnsurePrice fetches the first price of a freshly linked instrument (best effort).
	EnsurePrice(ctx context.Context, instrumentID uuid.UUID)
	// RefreshPrice fetches one instrument's price now.
	RefreshPrice(ctx context.Context, instrumentID uuid.UUID) error
	// SymbolAmbiguous reports whether a symbol does not clearly name one instrument.
	SymbolAmbiguous(ctx context.Context, assetType, symbol string) (bool, error)
}

// Service holds investment logic.
type Service struct {
	db       DB
	q        *sqlc.Queries
	currency *currency.Service
	market   Market
	now      func() time.Time
}

// NewService builds the service; now may be nil.
func NewService(database DB, cur *currency.Service, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{db: database, q: sqlc.New(database), currency: cur, now: now}
}

// SetMarket installs market data (prices and instrument linking).
func (s *Service) SetMarket(m Market) { s.market = m }

// Item is an investment as seen from a space: with the viewer's valuation and,
// for shared investments, the whole investment's valuation and the shares.
type Item struct {
	Investment sqlc.Investment
	// Valuation is the viewer's part: the whole investment when it is not shared.
	Valuation Valuation
	// TotalValuation is the whole investment's valuation; set only for shared investments.
	TotalValuation *Valuation
	Details        any
	Realized       RealizedPL
	// Ownership is OWNER for investments of this space and SHARED_IN for ones shared in.
	Ownership string
	// MyShareBps is the viewer's share in basis points (10000 when not shared).
	MyShareBps   int
	Participants []ParticipantView
}

// ListResult is the portfolio: items with valuations and the base-currency summary.
type ListResult struct {
	Items   []Item
	Summary Summary
}

func (s *Service) marketPrices(ctx context.Context, invs []sqlc.Investment) (map[uuid.UUID]MarketPrice, error) {
	if s.market == nil {
		return nil, nil
	}
	var ids []uuid.UUID
	for _, i := range invs {
		if i.PriceMode == PriceModeAuto && i.InstrumentID.Valid {
			ids = append(ids, i.InstrumentID.UUID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return s.market.MarketPrices(ctx, ids)
}

// value values one investment for a viewer. share is nil for an unshared one;
// participants are the people behind a shared one (masked for the viewer).
func (s *Service) value(inv sqlc.Investment, prices map[uuid.UUID]MarketPrice, cc *currency.ConversionContext, now time.Time, share *Share, participants []ParticipantView) (Item, error) {
	var mp *MarketPrice
	if inv.InstrumentID.Valid {
		if p, ok := prices[inv.InstrumentID.UUID]; ok {
			mp = &p
		}
	}
	details, err := parseStoredDetails(inv.AssetType, inv.Details)
	if err != nil {
		return Item{}, err
	}
	whole := Value(inv, mp, cc, now)
	it := Item{Investment: inv, Valuation: whole, Details: details, Ownership: OwnershipOwner, MyShareBps: TotalBps, Participants: participants}
	if share != nil {
		total := whole
		it.TotalValuation = &total
		it.Valuation = Allocate(whole, share, cc.BaseCurrency)
		it.MyShareBps = share.MyBps
	}
	return it, nil
}

// List returns the space's investments (optionally one asset type) with the
// viewer's valuations and a summary. With includeShared, investments other people
// shared into this space are listed too (read-only, flagged SHARED_IN).
func (s *Service) List(ctx context.Context, spaceID uuid.UUID, assetType string, includeShared bool, viewer uuid.UUID) (ListResult, error) {
	cc, err := s.currency.BuildContext(ctx, spaceID)
	if err != nil {
		return ListResult{}, err
	}
	return s.ListWith(ctx, spaceID, assetType, includeShared, viewer, cc, s.now())
}

// ListWith is List with an explicit conversion context and clock, so callers such
// as the net-worth summary value everything with one consistent set of rates.
func (s *Service) ListWith(ctx context.Context, spaceID uuid.UUID, assetType string, includeShared bool, viewer uuid.UUID, cc *currency.ConversionContext, now time.Time) (ListResult, error) {
	var t *string
	if assetType != "" {
		t = &assetType
	}
	rows, err := s.q.ListInvestments(ctx, sqlc.ListInvestmentsParams{SpaceID: spaceID, AssetType: t})
	if err != nil {
		return ListResult{}, err
	}
	invs := append([]sqlc.Investment(nil), rows...)
	myParticipant := map[uuid.UUID]uuid.UUID{} // shared-in investment → my participant row
	var sharedIn []sqlc.Investment
	if includeShared {
		in, err := s.q.ListSharedInInvestments(ctx, sqlc.ListSharedInInvestmentsParams{AttachedSpaceID: uuid.NullUUID{UUID: spaceID, Valid: true}, AssetType: t})
		if err != nil {
			return ListResult{}, err
		}
		for _, r := range in {
			inv := sharedInRow(r)
			sharedIn = append(sharedIn, inv)
			myParticipant[inv.ID] = r.MyParticipantID
			invs = append(invs, inv)
		}
	}

	prices, err := s.marketPrices(ctx, invs)
	if err != nil {
		return ListResult{}, err
	}
	people, err := s.participantsOf(ctx, invs)
	if err != nil {
		return ListResult{}, err
	}
	realized, err := s.realized(ctx, cc, invs)
	if err != nil {
		return ListResult{}, err
	}

	res := ListResult{Items: make([]Item, 0, len(invs))}
	vals := make([]Valuation, 0, len(invs))
	add := func(inv sqlc.Investment, mine func(sqlc.InvestmentParticipant) bool, ownership string) error {
		it, err := s.valueForViewer(inv, prices, cc, now, people[inv.ID], viewer, mine, ownership, realized[inv.ID])
		if err != nil {
			return err
		}
		res.Items = append(res.Items, it)
		vals = append(vals, it.Valuation)
		return nil
	}
	for _, inv := range rows {
		inv := inv
		if err := add(inv, func(p sqlc.InvestmentParticipant) bool { return p.Role == RoleOwner }, OwnershipOwner); err != nil {
			return ListResult{}, err
		}
	}
	for _, inv := range sharedIn {
		mine := myParticipant[inv.ID]
		if err := add(inv, func(p sqlc.InvestmentParticipant) bool { return p.ID == mine }, OwnershipSharedIn); err != nil {
			return ListResult{}, err
		}
	}
	res.Summary = Summarize(cc.BaseCurrency, vals)
	return res, nil
}

// sharedInRow copies the investment columns out of a shared-in listing row.
func sharedInRow(r sqlc.ListSharedInInvestmentsRow) sqlc.Investment {
	return sqlc.Investment{
		ID: r.ID, SpaceID: r.SpaceID, AssetType: r.AssetType, PriceMode: r.PriceMode, Name: r.Name, Symbol: r.Symbol,
		Icon: r.Icon, QuoteCurrency: r.QuoteCurrency, Quantity: r.Quantity, AvgBuyPrice: r.AvgBuyPrice,
		ManualPrice: r.ManualPrice, Fees: r.Fees, ManualCurrentValue: r.ManualCurrentValue, PurchaseDate: r.PurchaseDate,
		EstimatedAnnualChangePct: r.EstimatedAnnualChangePct, ManualFxRate: r.ManualFxRate, Details: r.Details,
		InstrumentID: r.InstrumentID, IsShared: r.IsShared, CreatedByID: r.CreatedByID, CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt, DeletedAt: r.DeletedAt, Wrapper: r.Wrapper,
	}
}

// participantsOf loads the live participants of every shared investment in invs.
func (s *Service) participantsOf(ctx context.Context, invs []sqlc.Investment) (map[uuid.UUID][]sqlc.InvestmentParticipant, error) {
	var ids []uuid.UUID
	for _, i := range invs {
		if i.IsShared {
			ids = append(ids, i.ID)
		}
	}
	out := map[uuid.UUID][]sqlc.InvestmentParticipant{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.q.ListLiveParticipantsForInvestments(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.InvestmentID] = append(out[r.InvestmentID], r)
	}
	return out, nil
}

// valueForViewer values one investment from a viewer's point of view: the owner
// sees their own share, a shared-in participant theirs (mine picks the viewer's
// participant row). It is the single place List and Get share, so they cannot drift.
func (s *Service) valueForViewer(inv sqlc.Investment, prices map[uuid.UUID]MarketPrice, cc *currency.ConversionContext, now time.Time,
	rows []sqlc.InvestmentParticipant, viewer uuid.UUID, mine func(sqlc.InvestmentParticipant) bool, ownership string, realized RealizedPL) (Item, error) {
	var share *Share
	if len(rows) > 0 {
		share = shareOf(rows, mine)
	}
	it, err := s.value(inv, prices, cc, now, share, views(rows, viewer, false))
	if err != nil {
		return Item{}, err
	}
	it.Ownership = ownership
	it.Realized = AllocateRealized(realized, share, cc.BaseCurrency)
	it.Realized.BaseCurrency = strings.TrimSpace(cc.BaseCurrency)
	return it, nil
}

// Get returns one investment with its valuation for the viewer. It resolves
// investments the viewer owns through this space and ones shared into it (the
// latter valued with the viewer's share and base currency).
func (s *Service) Get(ctx context.Context, spaceID, viewer, id uuid.UUID) (Item, error) {
	inv, err := s.q.GetInvestmentForViewer(ctx, sqlc.GetInvestmentForViewerParams{ID: id, SpaceID: spaceID, UserID: uuid.NullUUID{UUID: viewer, Valid: true}})
	if errors.Is(err, pgx.ErrNoRows) {
		return Item{}, errInvestmentNF
	}
	if err != nil {
		return Item{}, err
	}
	if inv.SpaceID == spaceID {
		return s.item(ctx, inv, viewer)
	}
	part, err := s.q.GetViewerParticipation(ctx, sqlc.GetViewerParticipationParams{
		InvestmentID: inv.ID, UserID: uuid.NullUUID{UUID: viewer, Valid: true}, AttachedSpaceID: uuid.NullUUID{UUID: spaceID, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Item{}, errInvestmentNF
	}
	if err != nil {
		return Item{}, err
	}
	return s.itemIn(ctx, inv, spaceID, viewer, func(p sqlc.InvestmentParticipant) bool { return p.ID == part.ID }, OwnershipSharedIn)
}

// item values one owned investment from its owner's point of view.
func (s *Service) item(ctx context.Context, inv sqlc.Investment, viewer uuid.UUID) (Item, error) {
	return s.itemIn(ctx, inv, inv.SpaceID, viewer, func(p sqlc.InvestmentParticipant) bool { return p.Role == RoleOwner }, OwnershipOwner)
}

// itemIn values one investment with the conversion context of ccSpace (the viewer's space).
func (s *Service) itemIn(ctx context.Context, inv sqlc.Investment, ccSpace, viewer uuid.UUID, mine func(sqlc.InvestmentParticipant) bool, ownership string) (Item, error) {
	cc, err := s.currency.BuildContext(ctx, ccSpace)
	if err != nil {
		return Item{}, err
	}
	one := []sqlc.Investment{inv}
	prices, err := s.marketPrices(ctx, one)
	if err != nil {
		return Item{}, err
	}
	people, err := s.participantsOf(ctx, one)
	if err != nil {
		return Item{}, err
	}
	realized, err := s.realized(ctx, cc, one)
	if err != nil {
		return Item{}, err
	}
	return s.valueForViewer(inv, prices, cc, s.now(), people[inv.ID], viewer, mine, ownership, realized[inv.ID])
}

func toNullDecimal(d *decimal.Decimal) decimal.NullDecimal {
	if d == nil {
		return decimal.NullDecimal{}
	}
	return decimal.NullDecimal{Decimal: *d, Valid: true}
}

// Create stores a validated investment spec. With spec.Funding the initial
// position is paid from the wallet in the same database transaction.
func (s *Service) Create(ctx context.Context, spaceID, userID uuid.UUID, spec Spec) (Item, error) {
	details, err := DetailsJSON(spec.Details)
	if err != nil {
		return Item{}, err
	}
	var notices []currency.Warning
	instrument, err := s.linkInstrument(ctx, spec, nil, spec.InstrumentID != nil)
	if ae, ok := apperror.As(err); ok && ae.Code == CodeInstrumentNotFound && spec.AutoDefault {
		// The server chose AUTO and the coin cannot be linked: keep the holding, priced by hand.
		spec.PriceMode, instrument, err = PriceModeManual, uuid.NullUUID{}, nil
		notices = append(notices, currency.Warning{
			Code: CodePriceNotLinked, Label: spec.Name,
			Message: "No unique market instrument matches this symbol: the manual price is used. Pick the coin from the search to price it automatically.",
		})
	}
	if err != nil {
		return Item{}, err
	}
	insert := func(q *sqlc.Queries) (sqlc.Investment, error) {
		return q.CreateInvestment(ctx, sqlc.CreateInvestmentParams{
			SpaceID: spaceID, AssetType: spec.AssetType, PriceMode: spec.PriceMode, Name: spec.Name, Symbol: spec.Symbol,
			Icon: spec.Icon, QuoteCurrency: spec.QuoteCurrency, Quantity: spec.Quantity, AvgBuyPrice: spec.AvgBuyPrice,
			ManualPrice: spec.ManualPrice, Fees: spec.Fees, ManualCurrentValue: toNullDecimal(spec.ManualCurrentValue),
			PurchaseDate: spec.PurchaseDate, EstimatedAnnualChangePct: toNullDecimal(spec.EstimatedAnnualChangePct),
			ManualFxRate: toNullDecimal(spec.ManualFxRate), Details: details, CreatedByID: userID,
			InstrumentID: instrument, Wrapper: spec.Wrapper,
		})
	}

	var inv sqlc.Investment
	if spec.Funding == nil {
		if inv, err = insert(s.q); err != nil {
			return Item{}, err
		}
	} else {
		cc, err := s.currency.BuildContext(ctx, spaceID)
		if err != nil {
			return Item{}, err
		}
		err = db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
			q := s.q.WithTx(tx)
			var err error
			if inv, err = insert(q); err != nil {
				return err
			}
			return s.fundInitialPosition(ctx, tx, q, spaceID, userID, inv, *spec.Funding, cc)
		})
		if err != nil {
			return Item{}, err
		}
	}
	s.firstPrice(ctx, inv)
	it, err := s.item(ctx, inv, userID)
	if err != nil {
		return Item{}, err
	}
	it.Valuation.Warnings = append(it.Valuation.Warnings, notices...)
	return it, nil
}

// fundInitialPosition records the BUY that paid for a new investment's initial
// position: the position goes from nothing to what was just stored.
func (s *Service) fundInitialPosition(ctx context.Context, tx pgx.Tx, q *sqlc.Queries, spaceID, userID uuid.UUID, inv sqlc.Investment, f Funding, cc *currency.ConversionContext) error {
	w, err := loadWallet(ctx, q, spaceID, f.WalletID)
	if err != nil {
		return err
	}
	l := Leg{Quantity: inv.Quantity, UnitPrice: inv.AvgBuyPrice, Fee: inv.Fees}
	if !l.Gross().IsPositive() {
		return validationErr("funding", "needs a quantity and an average buy price to pay for")
	}
	total := l.Gross().Add(l.Fee)
	amount, rate, err := walletSide(w, strings.TrimSpace(inv.QuoteCurrency), total, f.Amount, nil, cc)
	if err != nil {
		return err
	}
	title := "Initial position " + inv.Name
	if inv.Symbol != nil {
		title = "Initial position " + *inv.Symbol
	}
	r := tradeResult{amount: amount, total: total, fxRate: rate, before: Position{}, after: PositionOf(inv)}
	p, err := tradeParams(spaceID, userID, inv, w, ledger.ActionBuy, l, r, s.now(), nil, &title)
	if err != nil {
		return err
	}
	t, err := q.CreateTransaction(ctx, p)
	if err != nil {
		return err
	}
	return ledger.Apply(ctx, tx, t)
}

// RealizedPL is the realized profit/loss of an investment's SELLs.
type RealizedPL struct {
	Amount decimal.Decimal  // in the investment's quote currency
	Base   *decimal.Decimal // converted to the space base currency; nil without an FX rate
	// BaseCurrency is the currency of Base.
	BaseCurrency string
	Sells        int
}

// realized totals each investment's realized profit/loss, scaled to the viewer's
// share for shared investments (base values split with the largest-remainder method).
func (s *Service) realized(ctx context.Context, cc *currency.ConversionContext, invs []sqlc.Investment) (map[uuid.UUID]RealizedPL, error) {
	ids := make([]uuid.UUID, 0, len(invs))
	quote := map[uuid.UUID]string{}
	for _, i := range invs {
		ids = append(ids, i.ID)
		quote[i.ID] = strings.TrimSpace(i.QuoteCurrency)
	}
	if len(ids) == 0 {
		return map[uuid.UUID]RealizedPL{}, nil
	}
	rows, err := s.q.RealizedPLByInvestment(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]RealizedPL, len(rows))
	for _, r := range rows {
		if !r.InvestmentID.Valid {
			continue
		}
		rp := RealizedPL{Amount: r.RealizedPl, Sells: int(r.Sells)}
		if base, err := currency.Convert(r.RealizedPl, quote[r.InvestmentID.UUID], cc.BaseCurrency, cc); err == nil {
			rp.Base = &base
		}
		out[r.InvestmentID.UUID] = rp
	}
	return out, nil
}

// linkInstrument decides which market instrument an auto-priced investment points at.
// An explicit instrument (from search) is validated and used; an AUTO investment
// without one resolves its symbol (highest-ranked coin / exact ticker). cur is the stored
// row on update (nil on create); relink forces a lookup even if cur is linked.
func (s *Service) linkInstrument(ctx context.Context, spec Spec, cur *sqlc.Investment, relink bool) (uuid.NullUUID, error) {
	var current uuid.NullUUID
	if cur != nil {
		current = cur.InstrumentID
	}
	if meta, _ := MetaFor(spec.AssetType); !meta.SupportsAutoPrice || s.market == nil {
		return current, nil
	}
	if spec.InstrumentID != nil { // explicit choice
		id, err := s.market.ResolveInstrument(ctx, spec.AssetType, spec.InstrumentID, "")
		return uuid.NullUUID{UUID: id, Valid: err == nil}, err
	}
	symbolChanged := cur != nil && cur.Symbol != nil && spec.Symbol != nil && !strings.EqualFold(*cur.Symbol, *spec.Symbol)
	switchedToAuto := cur != nil && cur.PriceMode != PriceModeAuto && spec.PriceMode == PriceModeAuto
	if spec.PriceMode == PriceModeAuto && (!current.Valid || relink || symbolChanged || switchedToAuto) {
		sym := ""
		if spec.Symbol != nil {
			sym = *spec.Symbol
		}
		if spec.AutoDefault {
			if amb, err := s.market.SymbolAmbiguous(ctx, spec.AssetType, sym); err != nil {
				return uuid.NullUUID{}, err
			} else if amb {
				return uuid.NullUUID{}, apperror.Unprocessable(CodeInstrumentNotFound, "The symbol "+sym+" matches several instruments")
			}
		}
		id, err := s.market.ResolveInstrument(ctx, spec.AssetType, nil, sym)
		return uuid.NullUUID{UUID: id, Valid: err == nil}, err
	}
	return current, nil
}

// firstPrice gives a newly linked AUTO instrument its first price right away (best effort).
func (s *Service) firstPrice(ctx context.Context, inv sqlc.Investment) {
	if s.market != nil && inv.PriceMode == PriceModeAuto && inv.InstrumentID.Valid {
		s.market.EnsurePrice(ctx, inv.InstrumentID.UUID)
	}
}

// RefreshPrice refreshes the market price of an AUTO investment on demand.
func (s *Service) RefreshPrice(ctx context.Context, spaceID, userID, id uuid.UUID) (Item, error) {
	inv, err := s.q.GetInvestment(ctx, sqlc.GetInvestmentParams{ID: id, SpaceID: spaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Item{}, missOrOwnerOnly(ctx, s.q, spaceID, userID, id)
	}
	if err != nil {
		return Item{}, err
	}
	if err := requireOwner(inv, userID); err != nil {
		return Item{}, err
	}
	meta, _ := MetaFor(inv.AssetType)
	switch {
	case !meta.SupportsAutoPrice:
		return Item{}, errAutoNotSupported
	case inv.PriceMode != PriceModeAuto:
		return Item{}, apperror.Unprocessable(CodePriceModeNotAuto, "Switch the investment to automatic pricing first")
	case !inv.InstrumentID.Valid || s.market == nil:
		return Item{}, apperror.Unprocessable(CodeInstrumentNotLinked, "This investment is not linked to a market instrument yet")
	}
	if err := s.market.RefreshPrice(ctx, inv.InstrumentID.UUID); err != nil {
		return Item{}, err
	}
	return s.item(ctx, inv, userID)
}

// Update applies a patch to an investment; assetType never changes.
func (s *Service) Update(ctx context.Context, space sqlc.Space, userID, id uuid.UUID, p PatchInput) (Item, error) {
	var out sqlc.Investment
	err := db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		cur, err := lockInvestment(ctx, q, space.ID, userID, id)
		if err != nil {
			return err
		}
		if err := requireOwner(cur, userID); err != nil {
			return err
		}
		spec, err := p.apply(cur, strings.TrimSpace(space.BaseCurrency))
		if err != nil {
			return err
		}
		instrument, err := s.linkInstrument(ctx, spec, &cur, p.InstrumentID != nil)
		if err != nil {
			return err
		}
		details, err := DetailsJSON(spec.Details)
		if err != nil {
			return err
		}
		out, err = q.UpdateInvestment(ctx, sqlc.UpdateInvestmentParams{
			ID: id, SpaceID: space.ID, PriceMode: spec.PriceMode, Name: spec.Name, Symbol: spec.Symbol, Icon: spec.Icon,
			QuoteCurrency: spec.QuoteCurrency, Quantity: spec.Quantity, AvgBuyPrice: spec.AvgBuyPrice,
			ManualPrice: spec.ManualPrice, Fees: spec.Fees, ManualCurrentValue: toNullDecimal(spec.ManualCurrentValue),
			PurchaseDate: spec.PurchaseDate, EstimatedAnnualChangePct: toNullDecimal(spec.EstimatedAnnualChangePct),
			ManualFxRate: toNullDecimal(spec.ManualFxRate), Details: details, Wrapper: spec.Wrapper,
		})
		if err != nil {
			return err
		}
		if instrument != cur.InstrumentID {
			if err := q.SetInvestmentInstrument(ctx, sqlc.SetInvestmentInstrumentParams{ID: id, SpaceID: space.ID, InstrumentID: instrument}); err != nil {
				return err
			}
			out.InstrumentID = instrument
		}
		return nil
	})
	if err != nil {
		return Item{}, err
	}
	s.firstPrice(ctx, out)
	return s.item(ctx, out, userID)
}

// Delete soft-deletes an investment.
func (s *Service) Delete(ctx context.Context, spaceID, userID, id uuid.UUID) error {
	return db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		inv, err := lockInvestment(ctx, q, spaceID, userID, id)
		if err != nil {
			return err
		}
		if err := requireOwner(inv, userID); err != nil {
			return err
		}
		_, err = q.SoftDeleteInvestment(ctx, sqlc.SoftDeleteInvestmentParams{ID: id, SpaceID: spaceID})
		return err
	})
}
