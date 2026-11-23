package investments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/JonathanRainee/hzzt/internal/db"
	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
	"github.com/JonathanRainee/hzzt/internal/platform/allocation"
	"github.com/JonathanRainee/hzzt/internal/platform/apperror"
	"github.com/JonathanRainee/hzzt/internal/platform/money"
)

// Participant roles and statuses.
const (
	RoleOwner       = "OWNER"
	RoleParticipant = "PARTICIPANT"

	StatusActive   = "ACTIVE"
	StatusInvited  = "INVITED"
	StatusDeclined = "DECLINED"
	StatusRemoved  = "REMOVED"

	// MaxParticipants is the most people (owner included) one investment can be shared with.
	MaxParticipants = 10
	// TotalBps is 100.00%.
	TotalBps = 10000

	// Ownership of an investment as seen from a space.
	OwnershipOwner    = "OWNER"
	OwnershipSharedIn = "SHARED_IN"
)

// Error codes owned by sharing (see plan Appendix B).
const (
	CodeShareTotalInvalid     = "SHARE_TOTAL_INVALID"
	CodeParticipantLimit      = "PARTICIPANT_LIMIT"
	CodeOwnerOnly             = "OWNER_ONLY"
	CodeShareInvitationNF     = "SHARE_INVITATION_NOT_FOUND"
	CodeShareInvitationClosed = "SHARE_INVITATION_ANSWERED"
	CodeShareOwnerCannotLeave = "SHARE_OWNER_CANNOT_LEAVE"
)

var (
	errInvitationNF     = apperror.New(404, CodeShareInvitationNF, "Invitation not found")
	errInvitationDone   = apperror.Conflict(CodeShareInvitationClosed, "This invitation has already been answered")
	errOwnerCannotLeave = apperror.Unprocessable(CodeShareOwnerCannotLeave, "The owner cannot leave a shared investment; stop sharing instead")
)

// ownerOnlyError is a 403 carrying the OWNER_ONLY code.
func ownerOnlyError() error {
	return &apperror.AppError{Code: CodeOwnerOnly, HTTPStatus: 403, Message: "Only the owner of a shared investment can do this"}
}

// requireOwner rejects writes by anyone but the owner on a shared investment.
func requireOwner(inv sqlc.Investment, userID uuid.UUID) error {
	if inv.IsShared && inv.CreatedByID != userID {
		return ownerOnlyError()
	}
	return nil
}

// ParticipantView is a participant as shown to a viewer.
type ParticipantView struct {
	ID          uuid.UUID
	Role        string
	DisplayName string
	Email       *string // full for the owner, masked for everyone else, nil for name-only
	ShareBps    int
	Status      string
	IsMe        bool
	InvitedAt   time.Time
}

// MaskEmail hides most of an address: jane@example.com → j***@example.com.
func MaskEmail(e string) string {
	at := strings.LastIndex(e, "@")
	if at < 1 {
		return "***"
	}
	return e[:1] + "***" + e[at:]
}

func viewOf(p sqlc.InvestmentParticipant, viewer uuid.UUID, full bool) ParticipantView {
	v := ParticipantView{
		ID: p.ID, Role: p.Role, DisplayName: p.DisplayName, ShareBps: int(p.ShareBps), Status: p.Status,
		IsMe: p.UserID.Valid && p.UserID.UUID == viewer, InvitedAt: p.InvitedAt.UTC(),
	}
	if p.Email != nil {
		e := *p.Email
		if !full {
			e = MaskEmail(e)
		}
		v.Email = &e
	}
	return v
}

// shareOf builds the viewer's Share from an investment's participants.
// isMine picks the viewer's own row.
func shareOf(rows []sqlc.InvestmentParticipant, isMine func(sqlc.InvestmentParticipant) bool) *Share {
	if len(rows) == 0 {
		return nil
	}
	sh := &Share{AllBps: make([]int, len(rows)), MyIndex: -1}
	for i, r := range rows {
		sh.AllBps[i] = int(r.ShareBps)
		if sh.MyIndex < 0 && isMine(r) {
			sh.MyIndex, sh.MyBps = i, int(r.ShareBps)
		}
	}
	if sh.MyIndex < 0 {
		return nil
	}
	return sh
}

// --- participants (owner) ----------------------------------------------------------

// ParticipantInput is one entry of the full replacement list.
type ParticipantInput struct {
	ParticipantID *uuid.UUID
	Email         string // normalized (trimmed, lower-case) or ""
	DisplayName   string
	SharePct      decimal.Decimal
	IsOwner       bool
}

func validationField(field, msg string) error {
	return apperror.Validation("Validation failed", map[string]any{field: msg})
}

// shareBps converts a percentage with up to 2 decimals to basis points.
func shareBps(pct decimal.Decimal) (int, bool) {
	if !pct.Round(2).Equal(pct) || pct.LessThan(decimal.RequireFromString("0.01")) || pct.GreaterThan(decimal.NewFromInt(100)) {
		return 0, false
	}
	return int(pct.Mul(decimal.NewFromInt(100)).IntPart()), true
}

func snapshotOf(rows []sqlc.InvestmentParticipant) []byte {
	type row struct {
		ID          uuid.UUID `json:"id"`
		Role        string    `json:"role"`
		DisplayName string    `json:"displayName"`
		Email       *string   `json:"email,omitempty"`
		ShareBps    int32     `json:"shareBps"`
		Status      string    `json:"status"`
	}
	out := make([]row, 0, len(rows))
	for _, r := range rows {
		out = append(out, row{r.ID, r.Role, r.DisplayName, r.Email, r.ShareBps, r.Status})
	}
	b, _ := json.Marshal(out)
	return b
}

// Participants lists the participants of an owned investment (owner only), with full emails.
func (s *Service) Participants(ctx context.Context, spaceID, invID uuid.UUID, user sqlc.User) ([]ParticipantView, error) {
	inv, err := s.q.GetInvestment(ctx, sqlc.GetInvestmentParams{ID: invID, SpaceID: spaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, missOrOwnerOnly(ctx, s.q, spaceID, user.ID, invID)
	}
	if err != nil {
		return nil, err
	}
	if inv.CreatedByID != user.ID {
		return nil, ownerOnlyError()
	}
	rows, err := s.q.ListLiveParticipants(ctx, invID)
	if err != nil {
		return nil, err
	}
	return views(rows, user.ID, true), nil
}

func views(rows []sqlc.InvestmentParticipant, viewer uuid.UUID, full bool) []ParticipantView {
	out := make([]ParticipantView, 0, len(rows))
	for _, r := range rows {
		out = append(out, viewOf(r, viewer, full))
	}
	return out
}

// SetParticipants replaces the participant list of an investment (owner only).
//
// The shares of all non-removed participants must add up to exactly 100.00%
// (SHARE_TOTAL_INVALID), the owner is one of them with a share above 0, and there
// are at most 10 (PARTICIPANT_LIMIT). People with an email become INVITED, name-only
// participants ACTIVE; nobody is looked up, so the response is identical whether or
// not an email belongs to an account. Entries missing from the list are removed. The
// change is audited. Everything happens in one database transaction.
func (s *Service) SetParticipants(ctx context.Context, spaceID, invID uuid.UUID, user sqlc.User, in []ParticipantInput) ([]ParticipantView, error) {
	var out []sqlc.InvestmentParticipant
	err := db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		inv, err := lockInvestment(ctx, q, spaceID, user.ID, invID)
		if err != nil {
			return err
		}
		if inv.CreatedByID != user.ID {
			return ownerOnlyError()
		}
		existing, err := q.ListLiveParticipants(ctx, invID)
		if err != nil {
			return err
		}
		plan, err := planParticipants(inv, user, existing, in)
		if err != nil {
			return err
		}

		byID := map[uuid.UUID]sqlc.InvestmentParticipant{}
		for _, e := range existing {
			byID[e.ID] = e
		}
		kept := map[uuid.UUID]bool{}
		for _, p := range plan {
			switch {
			case p.existingID != uuid.Nil:
				kept[p.existingID] = true
				if err := q.UpdateParticipantShare(ctx, sqlc.UpdateParticipantShareParams{ID: p.existingID, DisplayName: p.name, ShareBps: int32(p.bps)}); err != nil {
					return err
				}
			default:
				params := sqlc.InsertParticipantParams{
					InvestmentID: invID, Role: p.role, DisplayName: p.name, ShareBps: int32(p.bps), Status: p.status, InvitedAt: s.now().UTC(),
				}
				if p.userID != uuid.Nil {
					params.UserID = uuid.NullUUID{UUID: p.userID, Valid: true}
				}
				if p.email != "" {
					e := p.email
					params.Email = &e
				}
				if _, err := q.InsertParticipant(ctx, params); err != nil {
					return err
				}
			}
		}
		for id := range byID {
			if !kept[id] {
				if err := q.RemoveParticipant(ctx, id); err != nil {
					return err
				}
			}
		}

		out, err = q.ListLiveParticipants(ctx, invID)
		if err != nil {
			return err
		}
		if err := q.SetInvestmentShared(ctx, sqlc.SetInvestmentSharedParams{ID: invID, IsShared: len(out) > 1}); err != nil {
			return err
		}
		return q.InsertShareChange(ctx, sqlc.InsertShareChangeParams{
			InvestmentID: invID, ChangedByID: user.ID, Before: snapshotOf(existing), After: snapshotOf(out),
		})
	})
	if err != nil {
		return nil, err
	}
	return views(out, user.ID, true), nil
}

type plannedParticipant struct {
	existingID uuid.UUID // set when updating an existing row
	role       string
	userID     uuid.UUID
	name       string
	email      string
	bps        int
	status     string
}

// planParticipants validates the replacement list against the stored participants
// and works out what each entry becomes.
func planParticipants(inv sqlc.Investment, owner sqlc.User, existing []sqlc.InvestmentParticipant, in []ParticipantInput) ([]plannedParticipant, error) {
	if len(in) == 0 {
		return nil, validationField("participants", "must contain at least the owner")
	}
	if len(in) > MaxParticipants {
		return nil, apperror.Unprocessable(CodeParticipantLimit, fmt.Sprintf("An investment can be shared with at most %d people, including you", MaxParticipants)).
			WithDetails(map[string]any{"max": MaxParticipants})
	}
	byID := map[uuid.UUID]sqlc.InvestmentParticipant{}
	var ownerRow *sqlc.InvestmentParticipant
	for i, e := range existing {
		byID[e.ID] = e
		if e.Role == RoleOwner {
			ownerRow = &existing[i]
		}
	}
	ownerEmail := strings.ToLower(owner.Email)

	var plan []plannedParticipant
	seenIDs, seenEmails := map[uuid.UUID]bool{}, map[string]bool{}
	total, owners := 0, 0
	for i, p := range in {
		field := fmt.Sprintf("participants[%d]", i)
		name := strings.TrimSpace(p.DisplayName)
		if name == "" || utf8.RuneCountInString(name) > 60 {
			return nil, validationField(field+".displayName", "must be 1 to 60 characters")
		}
		bps, ok := shareBps(p.SharePct)
		if !ok {
			return nil, validationField(field+".sharePct", "must be between 0.01 and 100 with at most 2 decimals")
		}
		if p.Email != "" {
			if _, err := mail.ParseAddress(p.Email); err != nil || len(p.Email) > 254 {
				return nil, validationField(field+".email", "must be a valid email address")
			}
			if seenEmails[p.Email] {
				return nil, validationField(field+".email", "appears more than once")
			}
			seenEmails[p.Email] = true
		}

		isOwner := p.IsOwner || (p.Email != "" && p.Email == ownerEmail) ||
			(p.ParticipantID != nil && ownerRow != nil && *p.ParticipantID == ownerRow.ID)
		var cur *sqlc.InvestmentParticipant
		if p.ParticipantID != nil {
			e, ok := byID[*p.ParticipantID]
			if !ok || seenIDs[*p.ParticipantID] {
				return nil, validationField(field+".participantId", "is not a participant of this investment")
			}
			cur = &e
			seenIDs[e.ID] = true
			if isOwner != (e.Role == RoleOwner) {
				return nil, validationField(field+".participantId", "does not match the owner flag")
			}
			if p.Email != "" && (e.Email == nil || !strings.EqualFold(*e.Email, p.Email)) && e.Role != RoleOwner {
				return nil, validationField(field+".email", "cannot be changed; remove the participant and add a new one")
			}
		}

		pp := plannedParticipant{name: name, bps: bps, email: p.Email}
		switch {
		case isOwner:
			owners++
			pp.role, pp.status, pp.userID, pp.email = RoleOwner, StatusActive, owner.ID, owner.Email
			if ownerRow != nil {
				pp.existingID = ownerRow.ID
				seenIDs[ownerRow.ID] = true
			}
		case cur != nil:
			pp.existingID, pp.role, pp.status = cur.ID, RoleParticipant, cur.Status
		default:
			pp.role = RoleParticipant
			if p.Email != "" {
				pp.status = StatusInvited
			} else {
				pp.status = StatusActive
			}
		}
		total += bps
		plan = append(plan, pp)
	}
	if owners != 1 {
		return nil, validationField("participants", "must include the owner exactly once (isOwner: true, or your own email)")
	}
	if total != TotalBps {
		return nil, apperror.Unprocessable(CodeShareTotalInvalid, fmt.Sprintf("Shares must add up to exactly 100.00%% (got %s%%)", bpsPct(total))).
			WithDetails(map[string]any{"total": bpsPct(total)})
	}
	return plan, nil
}

func bpsPct(bps int) string {
	return decimal.NewFromInt(int64(bps)).Div(decimal.NewFromInt(100)).StringFixed(2)
}

// StopSharing returns 100% to the owner and marks every other participant REMOVED (owner only).
func (s *Service) StopSharing(ctx context.Context, spaceID, invID uuid.UUID, user sqlc.User) error {
	return db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		inv, err := lockInvestment(ctx, q, spaceID, user.ID, invID)
		if err != nil {
			return err
		}
		if inv.CreatedByID != user.ID {
			return ownerOnlyError()
		}
		existing, err := q.ListLiveParticipants(ctx, invID)
		if err != nil {
			return err
		}
		for _, p := range existing {
			if p.Role == RoleOwner {
				if err := q.UpdateParticipantShare(ctx, sqlc.UpdateParticipantShareParams{ID: p.ID, DisplayName: p.DisplayName, ShareBps: TotalBps}); err != nil {
					return err
				}
				continue
			}
			if err := q.RemoveParticipant(ctx, p.ID); err != nil {
				return err
			}
		}
		after, err := q.ListLiveParticipants(ctx, invID)
		if err != nil {
			return err
		}
		if err := q.SetInvestmentShared(ctx, sqlc.SetInvestmentSharedParams{ID: invID, IsShared: false}); err != nil {
			return err
		}
		if len(existing) == 0 {
			return nil
		}
		return q.InsertShareChange(ctx, sqlc.InsertShareChangeParams{
			InvestmentID: invID, ChangedByID: user.ID, Before: snapshotOf(existing), After: snapshotOf(after),
		})
	})
}

// --- invitations (invitee) -----------------------------------------------------------

// Invitation is a pending invitation to co-own an investment.
type Invitation struct {
	ParticipantID uuid.UUID
	ShareBps      int
	InvitedAt     time.Time
	OwnerName     string
	Name          string
	Symbol        *string
	Icon          string
	AssetType     string
}

// Invitations lists pending invitations addressed to the user's id or account email.
func (s *Service) Invitations(ctx context.Context, user sqlc.User) ([]Invitation, error) {
	rows, err := s.q.ListInvitationsForUser(ctx, sqlc.ListInvitationsForUserParams{UserID: uuid.NullUUID{UUID: user.ID, Valid: true}, Column2: user.Email})
	if err != nil {
		return nil, err
	}
	out := make([]Invitation, 0, len(rows))
	for _, r := range rows {
		out = append(out, Invitation{
			ParticipantID: r.ID, ShareBps: int(r.ShareBps), InvitedAt: r.InvitedAt.UTC(), OwnerName: r.OwnerName,
			Name: r.InvestmentName, Symbol: r.InvestmentSymbol, Icon: r.InvestmentIcon, AssetType: r.InvestmentAssetType,
		})
	}
	return out, nil
}

func (s *Service) invitationFor(ctx context.Context, q *sqlc.Queries, user sqlc.User, id uuid.UUID) (sqlc.InvestmentParticipant, error) {
	p, err := q.GetInvitationForUser(ctx, sqlc.GetInvitationForUserParams{ID: id, UserID: uuid.NullUUID{UUID: user.ID, Valid: true}, Column3: user.Email})
	if errors.Is(err, pgx.ErrNoRows) {
		return p, errInvitationNF
	}
	return p, err
}

// AcceptInvitation attaches the shared investment to one of the invitee's spaces.
// The invitee must be a member of that space.
func (s *Service) AcceptInvitation(ctx context.Context, user sqlc.User, participantID, spaceID uuid.UUID) (uuid.UUID, error) {
	var invID uuid.UUID
	err := db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		p, err := s.invitationFor(ctx, q, user, participantID)
		if err != nil {
			return err
		}
		if p.Status != StatusInvited {
			return errInvitationDone
		}
		if _, err := q.GetSpaceMembership(ctx, sqlc.GetSpaceMembershipParams{ID: spaceID, UserID: user.ID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return apperror.NotFound("Space not found")
			}
			return err
		}
		inv, err := q.GetInvestmentByID(ctx, p.InvestmentID)
		if err != nil {
			return err
		}
		if inv.SpaceID == spaceID {
			return validationField("spaceId", "must be one of your own spaces, not the space the investment lives in")
		}
		if _, err := q.AcceptInvitation(ctx, sqlc.AcceptInvitationParams{ID: participantID, UserID: uuid.NullUUID{UUID: user.ID, Valid: true}, AttachedSpaceID: uuid.NullUUID{UUID: spaceID, Valid: true}}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errInvitationDone
			}
			return err
		}
		invID = inv.ID
		return s.auditParticipant(ctx, q, user, p)
	})
	return invID, err
}

// DeclineInvitation declines an invitation. The share stays allocated until the owner changes it.
func (s *Service) DeclineInvitation(ctx context.Context, user sqlc.User, participantID uuid.UUID) error {
	return db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		p, err := s.invitationFor(ctx, q, user, participantID)
		if err != nil {
			return err
		}
		if p.Status != StatusInvited {
			return errInvitationDone
		}
		if _, err := q.DeclineInvitation(ctx, participantID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errInvitationDone
			}
			return err
		}
		return s.auditParticipant(ctx, q, user, p)
	})
}

// LeaveSharedInvestment turns the user's participation into a name-only one: the
// share stays allocated, the user no longer sees the investment and the owner
// finds the change in the audit trail.
func (s *Service) LeaveSharedInvestment(ctx context.Context, user sqlc.User, participantID uuid.UUID) error {
	return db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		p, err := q.GetMyParticipation(ctx, sqlc.GetMyParticipationParams{ID: participantID, UserID: uuid.NullUUID{UUID: user.ID, Valid: true}})
		if errors.Is(err, pgx.ErrNoRows) {
			return errInvitationNF
		}
		if err != nil {
			return err
		}
		if p.Role == RoleOwner {
			return errOwnerCannotLeave
		}
		if _, err := q.LeaveSharedInvestment(ctx, participantID); err != nil {
			return err
		}
		return s.auditParticipant(ctx, q, user, p)
	})
}

// auditParticipant records a participant-side change (accept, decline, leave) in the audit trail.
func (s *Service) auditParticipant(ctx context.Context, q *sqlc.Queries, user sqlc.User, before sqlc.InvestmentParticipant) error {
	rows, err := q.ListLiveParticipants(ctx, before.InvestmentID)
	if err != nil {
		return err
	}
	prev := make([]sqlc.InvestmentParticipant, 0, len(rows))
	for _, r := range rows {
		if r.ID == before.ID {
			r = before
		}
		prev = append(prev, r)
	}
	return q.InsertShareChange(ctx, sqlc.InsertShareChangeParams{
		InvestmentID: before.InvestmentID, ChangedByID: user.ID, Before: snapshotOf(prev), After: snapshotOf(rows),
	})
}

// ShareChanges returns the audit trail of an owned investment (newest first).
func (s *Service) ShareChanges(ctx context.Context, spaceID, invID uuid.UUID, userID uuid.UUID) ([]sqlc.InvestmentShareChange, error) {
	inv, err := s.q.GetInvestment(ctx, sqlc.GetInvestmentParams{ID: invID, SpaceID: spaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, missOrOwnerOnly(ctx, s.q, spaceID, userID, invID)
	}
	if err != nil {
		return nil, err
	}
	if inv.CreatedByID != userID {
		return nil, ownerOnlyError()
	}
	return s.q.ListShareChanges(ctx, invID)
}

// AllocateRealized scales realized profit/loss to the viewer's share.
func AllocateRealized(rp RealizedPL, sh *Share, baseCurrency string) RealizedPL {
	if sh == nil {
		return rp
	}
	out := rp
	out.Amount = rp.Amount.Mul(OwnershipFactor(sh))
	if rp.Base != nil {
		part := allocation.Split(*rp.Base, sh.AllBps, money.MinorUnits(baseCurrency))[sh.MyIndex]
		out.Base = &part
	}
	return out
}
