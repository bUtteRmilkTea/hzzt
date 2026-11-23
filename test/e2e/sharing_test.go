//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type participantBody struct {
	ID          string  `json:"id"`
	Role        string  `json:"role"`
	DisplayName string  `json:"displayName"`
	Email       *string `json:"email"`
	SharePct    string  `json:"sharePct"`
	ShareBps    int     `json:"shareBps"`
	Status      string  `json:"status"`
	IsMe        bool    `json:"isMe"`
}

type participantsResp struct {
	Items []participantBody `json:"items"`
}

type shareListItem struct {
	investmentBody
	Ownership      string            `json:"ownership"`
	MyShareBps     int               `json:"myShareBps"`
	Participants   []participantBody `json:"participants"`
	TotalValuation *valuationBody    `json:"totalValuation"`
}

type shareList struct {
	Items   []shareListItem `json:"items"`
	Summary struct {
		TotalValue   string `json:"totalValue"`
		TotalCost    string `json:"totalCost"`
		UnrealizedPL string `json:"unrealizedPl"`
	} `json:"summary"`
}

type invitationBody struct {
	ParticipantID string `json:"participantId"`
	OwnerName     string `json:"ownerName"`
	Investment    struct {
		Name      string  `json:"name"`
		Symbol    *string `json:"symbol"`
		AssetType string  `json:"assetType"`
	} `json:"investment"`
	SharePct string `json:"sharePct"`
	ShareBps int    `json:"shareBps"`
}

type shareEnv struct {
	s     *TestServer
	owner session
	inv   investmentBody
	sid   string
}

// newShareEnv registers the owner and creates an AAPL position worth 1,000.01 USD (cost 500).
func newShareEnv(t *testing.T, s *TestServer) *shareEnv {
	t.Helper()
	owner := s.register("jane@example.com")
	e := &invEnv{s: s, u: owner, sid: spaceIDOf(owner)}
	inv := e.must(map[string]any{"assetType": "US_STOCK", "name": "Apple", "symbol": "AAPL", "quantity": "1",
		"avgBuyPrice": "500", "manualPrice": "1000.01", "details": map[string]any{"exchange": "NASDAQ"}})
	return &shareEnv{s: s, owner: owner, inv: inv, sid: e.sid}
}

func (e *shareEnv) partsURL() string { return invURL(e.sid, "/", e.inv.ID, "/participants") }

// entry builds one participant of the replacement list.
func owner(pct string) map[string]any {
	return map[string]any{"isOwner": true, "displayName": "Jane", "sharePct": pct}
}
func emailP(email, name, pct string) map[string]any {
	return map[string]any{"email": email, "displayName": name, "sharePct": pct}
}
func nameP(name, pct string) map[string]any {
	return map[string]any{"displayName": name, "sharePct": pct}
}

func (e *shareEnv) put(u session, list ...map[string]any) Response {
	return e.s.Do(http.MethodPut, e.partsURL(), map[string]any{"participants": list}, bearer(u.AccessToken))
}

func (e *shareEnv) mustPut(list ...map[string]any) participantsResp {
	e.s.t.Helper()
	res := e.put(e.owner, list...)
	require.Equal(e.s.t, http.StatusOK, res.Status, string(res.Body))
	var out participantsResp
	res.JSON(e.s.t, &out)
	return out
}

func (e *shareEnv) participants() []participantBody {
	e.s.t.Helper()
	res := e.s.Get(e.partsURL(), bearer(e.owner.AccessToken))
	require.Equal(e.s.t, http.StatusOK, res.Status, string(res.Body))
	var out participantsResp
	res.JSON(e.s.t, &out)
	return out.Items
}

func byName(ps []participantBody, name string) participantBody {
	for _, p := range ps {
		if p.DisplayName == name {
			return p
		}
	}
	return participantBody{}
}

func (s *TestServer) sharedList(u session, sid, query string) shareList {
	s.t.Helper()
	res := s.Get(invURL(sid)+query, bearer(u.AccessToken))
	require.Equal(s.t, http.StatusOK, res.Status, string(res.Body))
	var l shareList
	res.JSON(s.t, &l)
	return l
}

func (s *TestServer) shareInvitations(u session) []invitationBody {
	s.t.Helper()
	res := s.Get("/api/v1/me/shared-investment-invitations", bearer(u.AccessToken))
	require.Equal(s.t, http.StatusOK, res.Status, string(res.Body))
	var out []invitationBody
	res.JSON(s.t, &out)
	return out
}

func (s *TestServer) acceptShare(u session, id, space string) Response {
	return s.Do(http.MethodPost, "/api/v1/me/shared-investment-invitations/"+id+"/accept", map[string]any{"spaceId": space}, bearer(u.AccessToken))
}

func (s *TestServer) declineShare(u session, id string) Response {
	return s.Do(http.MethodPost, "/api/v1/me/shared-investment-invitations/"+id+"/decline", nil, bearer(u.AccessToken))
}

func (s *TestServer) leaveShare(u session, id string) Response {
	return s.Do(http.MethodPost, "/api/v1/me/shared-investments/"+id+"/leave", nil, bearer(u.AccessToken))
}

func ownerItem(l shareList, id string) shareListItem {
	for _, it := range l.Items {
		if it.ID == id {
			return it
		}
	}
	return shareListItem{}
}

func TestAcceptanceSharedAllocationAcrossSpaces(t *testing.T) {
	s := NewTestServer(t)
	e := newShareEnv(t, s)
	bob := s.register("bob@example.com")
	carol := s.register("carol@example.com")

	// Jane 50, Bob 30 (by email), Carol 20 (by email)
	got := e.mustPut(owner("50"), emailP("bob@example.com", "Bob", "30"), emailP("carol@example.com", "Carol", "20.00"))
	require.Len(t, got.Items, 3)
	assert.Equal(t, "OWNER", got.Items[0].Role, "the owner comes first")
	assert.Equal(t, 5000, got.Items[0].ShareBps)
	assert.Equal(t, "30.00", byName(got.Items, "Bob").SharePct)
	assert.Equal(t, "INVITED", byName(got.Items, "Bob").Status)
	assert.Equal(t, "ACTIVE", got.Items[0].Status)

	t.Run("the owner's space counts the owner's 50% only, shares allocated whatever the status", func(t *testing.T) {
		l := s.sharedList(e.owner, e.sid, "")
		it := ownerItem(l, e.inv.ID)
		assert.Equal(t, "OWNER", it.Ownership)
		assert.Equal(t, 5000, it.MyShareBps)
		assert.True(t, dec("500.01").Equal(dec(it.Valuation.Base.MarketValue)), "50%% of 1000.01 = %s", it.Valuation.Base.MarketValue)
		require.NotNil(t, it.TotalValuation)
		assert.True(t, dec("1000.01").Equal(dec(it.TotalValuation.Base.MarketValue)))
		assert.True(t, dec("500.01").Equal(dec(l.Summary.TotalValue)), "the summary uses my share")
		assert.True(t, dec("250").Equal(dec(l.Summary.TotalCost)))
		assert.True(t, dec("250.01").Equal(dec(l.Summary.UnrealizedPL)))
		assert.True(t, e.inv.IsShared || ownerItem(l, e.inv.ID).IsShared)
	})

	t.Run("invited people see the invitation, not the investment, until they accept", func(t *testing.T) {
		assert.Empty(t, s.sharedList(bob, spaceIDOf(bob), "?include=shared").Items)
		inv := s.shareInvitations(bob)
		require.Len(t, inv, 1)
		assert.Equal(t, "Apple", inv[0].Investment.Name)
		assert.Equal(t, "AAPL", *inv[0].Investment.Symbol)
		assert.Equal(t, "30.00", inv[0].SharePct)
		assert.Equal(t, "Jane", inv[0].OwnerName)
	})

	bobInv := s.shareInvitations(bob)[0]
	carolInv := s.shareInvitations(carol)[0]
	require.Equal(t, http.StatusOK, s.acceptShare(bob, bobInv.ParticipantID, spaceIDOf(bob)).Status)
	require.Equal(t, http.StatusOK, s.acceptShare(carol, carolInv.ParticipantID, spaceIDOf(carol)).Status)

	t.Run("acceptance: 50/30/20 of 1,000.01 → 500.01 / 300.00 / 200.00, summing exactly", func(t *testing.T) {
		bobList := s.sharedList(bob, spaceIDOf(bob), "?include=shared")
		require.Len(t, bobList.Items, 1)
		b := bobList.Items[0]
		assert.Equal(t, "SHARED_IN", b.Ownership)
		assert.Equal(t, 3000, b.MyShareBps)
		assert.True(t, dec("300").Equal(dec(b.Valuation.Base.MarketValue)), b.Valuation.Base.MarketValue)
		assert.True(t, dec("1000.01").Equal(dec(b.TotalValuation.Base.MarketValue)))

		carolList := s.sharedList(carol, spaceIDOf(carol), "?include=shared")
		c := carolList.Items[0]
		assert.True(t, dec("200").Equal(dec(c.Valuation.Base.MarketValue)))

		owners := ownerItem(s.sharedList(e.owner, e.sid, ""), e.inv.ID)
		sum := dec(owners.Valuation.Base.MarketValue).Add(dec(b.Valuation.Base.MarketValue)).Add(dec(c.Valuation.Base.MarketValue))
		assert.True(t, dec("1000.01").Equal(sum), "the parts add up exactly: %s", sum)
	})

	t.Run("the participant's attached space counts their share only after acceptance, and only with include=shared", func(t *testing.T) {
		withShared := s.sharedList(bob, spaceIDOf(bob), "?include=shared")
		assert.True(t, dec("300").Equal(dec(withShared.Summary.TotalValue)), "30%% after acceptance")
		assert.Empty(t, s.sharedList(bob, spaceIDOf(bob), "").Items, "not part of the plain list")
		assert.True(t, dec("0").Equal(dec(s.sharedList(bob, spaceIDOf(bob), "").Summary.TotalValue)))
		assert.Empty(t, s.shareInvitations(bob), "the invitation is gone from the inbox")
	})

	t.Run("a participant sees names and shares but not the owner's space nor emails", func(t *testing.T) {
		b := s.sharedList(bob, spaceIDOf(bob), "?include=shared").Items[0]
		require.Len(t, b.Participants, 3)
		jane, me := byName(b.Participants, "Jane"), byName(b.Participants, "Bob")
		assert.Equal(t, "50.00", jane.SharePct)
		assert.True(t, me.IsMe)
		assert.False(t, byName(b.Participants, "Carol").IsMe)
		require.NotNil(t, byName(b.Participants, "Carol").Email)
		assert.Equal(t, "c***@example.com", *byName(b.Participants, "Carol").Email, "other people's emails are masked")
		raw := fmt.Sprint(b)
		assert.NotContains(t, raw, e.sid, "the owner's space id is never exposed")
		assert.NotContains(t, raw, "carol@example.com")
		assert.NotContains(t, raw, "jane@example.com")

		// the owner sees full addresses on the participants endpoint
		full := e.participants()
		require.NotNil(t, byName(full, "Carol").Email)
		assert.Equal(t, "carol@example.com", *byName(full, "Carol").Email)
	})
}

func TestUnregisteredAndRegisteredEmailsLookIdentical(t *testing.T) {
	s := NewTestServer(t)
	e := newShareEnv(t, s)
	registered := s.register("bob@example.com")

	// two separate investments so both responses can be compared like for like
	inv2 := (&invEnv{s: s, u: e.owner, sid: e.sid}).must(map[string]any{"assetType": "US_STOCK", "name": "Apple 2", "symbol": "AAPL", "quantity": "1",
		"avgBuyPrice": "500", "manualPrice": "1000", "details": map[string]any{"exchange": "NASDAQ"}})
	a := e.mustPut(owner("50"), emailP("bob@example.com", "Friend", "50"))
	res := s.Do(http.MethodPut, invURL(e.sid, "/", inv2.ID, "/participants"),
		map[string]any{"participants": []map[string]any{owner("50"), emailP("ghost@example.com", "Friend", "50")}}, bearer(e.owner.AccessToken))
	require.Equal(t, http.StatusOK, res.Status, string(res.Body))
	var b participantsResp
	res.JSON(t, &b)

	shape := func(ps []participantBody) []string {
		var out []string
		for _, p := range ps {
			out = append(out, fmt.Sprintf("%s|%s|%s|%s|%v|%v", p.Role, p.DisplayName, p.SharePct, p.Status, p.Email != nil, p.IsMe))
		}
		return out
	}
	assert.Equal(t, shape(a.Items), shape(b.Items), "same response whether or not the email has an account")
	assert.Equal(t, "INVITED", byName(a.Items, "Friend").Status)
	assert.Equal(t, "INVITED", byName(b.Items, "Friend").Status)

	t.Run("after the address registers, the invitation appears", func(t *testing.T) {
		ghost := s.register("ghost@example.com")
		inv := s.shareInvitations(ghost)
		require.Len(t, inv, 1)
		assert.Equal(t, "Apple 2", inv[0].Investment.Name)
		require.Equal(t, http.StatusOK, s.acceptShare(ghost, inv[0].ParticipantID, spaceIDOf(ghost)).Status)
		got := s.sharedList(ghost, spaceIDOf(ghost), "?include=shared")
		require.Len(t, got.Items, 1)
		assert.Equal(t, 5000, got.Items[0].MyShareBps)
	})

	t.Run("matching is case-insensitive and per person", func(t *testing.T) {
		assert.Len(t, s.shareInvitations(registered), 1)
		stranger := s.register("stranger@example.com")
		assert.Empty(t, s.shareInvitations(stranger))
		id := s.shareInvitations(registered)[0].ParticipantID
		assert.Equal(t, http.StatusNotFound, s.acceptShare(stranger, id, spaceIDOf(stranger)).Status, "someone else's invitation")
		assert.Equal(t, http.StatusNotFound, s.declineShare(stranger, id).Status)
		// the owner types the address with capitals: it is normalized
		inv3 := (&invEnv{s: s, u: e.owner, sid: e.sid}).must(clone(aapl, "name", "Apple 3"))
		res := s.Do(http.MethodPut, invURL(e.sid, "/", inv3.ID, "/participants"),
			map[string]any{"participants": []map[string]any{owner("90"), emailP("  STRANGER@Example.COM ", "Stranger", "10")}}, bearer(e.owner.AccessToken))
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		assert.Len(t, s.shareInvitations(stranger), 1)
	})
}

func TestShareValidation(t *testing.T) {
	s := NewTestServer(t)
	e := newShareEnv(t, s)

	t.Run("the total must be exactly 100.00", func(t *testing.T) {
		for _, c := range []struct{ owner, other string }{{"50", "49.99"}, {"50.01", "50"}, {"60", "60"}, {"10", "10"}} {
			res := e.put(e.owner, owner(c.owner), nameP("Friend", c.other))
			require.Equal(t, http.StatusUnprocessableEntity, res.Status, "%+v: %s", c, res.Body)
			assert.Equal(t, "SHARE_TOTAL_INVALID", errCode(t, res))
		}
		res := e.put(e.owner, owner("50"), nameP("Friend", "49.99"))
		var er struct {
			Error struct {
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		res.JSON(t, &er)
		assert.Equal(t, "99.99", er.Error.Details["total"])
		assert.Equal(t, http.StatusOK, e.put(e.owner, owner("50.01"), nameP("Friend", "49.99")).Status, "99.99 + 0.01 style totals that do reach 100 work")
	})

	t.Run("at most 10 participants", func(t *testing.T) {
		list := []map[string]any{owner("10")}
		for i := 1; i <= 10; i++ {
			list = append(list, nameP(fmt.Sprintf("P%d", i), "9"))
		}
		res := e.put(e.owner, list...)
		require.Equal(t, http.StatusUnprocessableEntity, res.Status, string(res.Body))
		assert.Equal(t, "PARTICIPANT_LIMIT", errCode(t, res))
		// exactly 10 is fine
		list = []map[string]any{owner("10")}
		for i := 1; i <= 9; i++ {
			list = append(list, nameP(fmt.Sprintf("P%d", i), "10"))
		}
		assert.Equal(t, http.StatusOK, e.put(e.owner, list...).Status)
	})

	t.Run("field problems are 400", func(t *testing.T) {
		for name, list := range map[string][]map[string]any{
			"no owner":            {nameP("A", "50"), nameP("B", "50")},
			"two owners":          {owner("50"), owner("50")},
			"owner at 0":          {owner("0"), nameP("B", "100")},
			"3 decimals":          {owner("50.005"), nameP("B", "49.995")},
			"negative":            {owner("-50"), nameP("B", "150")},
			"over 100":            {owner("101")},
			"not a number":        {owner("half"), nameP("B", "50")},
			"blank name":          {owner("50"), nameP("  ", "50")},
			"long name":           {owner("50"), nameP(strings.Repeat("x", 61), "50")},
			"bad email":           {owner("50"), emailP("not-an-email", "B", "50")},
			"duplicate email":     {owner("40"), emailP("b@example.com", "B", "30"), emailP("B@example.com", "C", "30")},
			"unknown participant": {owner("50"), {"participantId": uuid.NewString(), "displayName": "B", "sharePct": "50"}},
			"missing share":       {owner("50"), {"displayName": "B"}},
		} {
			res := e.put(e.owner, list...)
			assert.Equal(t, http.StatusBadRequest, res.Status, "%s: %s", name, res.Body)
		}
		res := e.s.Do(http.MethodPut, e.partsURL(), map[string]any{"participants": []any{}}, bearer(e.owner.AccessToken))
		assert.Equal(t, http.StatusBadRequest, res.Status, "an empty list")
		res = e.s.Do(http.MethodPut, e.partsURL(), map[string]any{}, bearer(e.owner.AccessToken))
		assert.Equal(t, http.StatusBadRequest, res.Status)
	})

	t.Run("a failed replacement changes nothing", func(t *testing.T) {
		before := e.participants()
		assert.Equal(t, http.StatusUnprocessableEntity, e.put(e.owner, owner("50"), nameP("X", "10")).Status)
		assert.Equal(t, len(before), len(e.participants()))
	})

	t.Run("not found and unauthenticated", func(t *testing.T) {
		assert.Equal(t, http.StatusUnauthorized, s.Do(http.MethodPut, e.partsURL(), map[string]any{"participants": []any{}}).Status)
		res := s.Do(http.MethodPut, invURL(e.sid, "/", uuid.NewString(), "/participants"), map[string]any{"participants": []map[string]any{owner("100")}}, bearer(e.owner.AccessToken))
		assert.Equal(t, http.StatusNotFound, res.Status)
		assert.Equal(t, http.StatusNotFound, s.Get(invURL(e.sid, "/nope/participants"), bearer(e.owner.AccessToken)).Status)
	})
}

func TestReplacementListSemantics(t *testing.T) {
	s := NewTestServer(t)
	e := newShareEnv(t, s)
	first := e.mustPut(owner("50"), nameP("Alice", "30"), nameP("Bob", "20"))
	alice, bob := byName(first.Items, "Alice"), byName(first.Items, "Bob")
	assert.Equal(t, "ACTIVE", alice.Status, "a name-only participant is active at once")
	assert.Nil(t, alice.Email)

	t.Run("participantId updates an existing entry", func(t *testing.T) {
		out := e.mustPut(owner("40"), map[string]any{"participantId": alice.ID, "displayName": "Alice B.", "sharePct": "40"}, map[string]any{"participantId": bob.ID, "displayName": "Bob", "sharePct": "20"})
		assert.Equal(t, alice.ID, byName(out.Items, "Alice B.").ID, "same row, new name and share")
		assert.Equal(t, 4000, byName(out.Items, "Alice B.").ShareBps)
	})

	t.Run("entries left out are removed and the share goes with them", func(t *testing.T) {
		out := e.mustPut(owner("80"), map[string]any{"participantId": bob.ID, "displayName": "Bob", "sharePct": "20"})
		require.Len(t, out.Items, 2)
		assert.Empty(t, byName(out.Items, "Alice B.").ID)
		var status string
		require.NoError(t, s.Tx.QueryRow(context.Background(), `SELECT status FROM investment_participants WHERE id = $1`, alice.ID).Scan(&status))
		assert.Equal(t, "REMOVED", status, "the row is kept")
	})

	t.Run("the owner entry may be identified by id or by account email", func(t *testing.T) {
		ownerID := e.participants()[0].ID
		assert.Equal(t, http.StatusOK, e.put(e.owner, map[string]any{"participantId": ownerID, "displayName": "Jane", "sharePct": "70"}, map[string]any{"participantId": bob.ID, "displayName": "Bob", "sharePct": "30"}).Status)
		assert.Equal(t, http.StatusOK, e.put(e.owner, emailP("jane@example.com", "Jane", "60"), map[string]any{"participantId": bob.ID, "displayName": "Bob", "sharePct": "40"}).Status)
		assert.Equal(t, 6000, e.participants()[0].ShareBps)
	})

	t.Run("an invited person's email cannot be swapped", func(t *testing.T) {
		out := e.mustPut(owner("50"), emailP("x@example.com", "X", "50"))
		x := byName(out.Items, "X")
		res := e.put(e.owner, owner("50"), map[string]any{"participantId": x.ID, "email": "y@example.com", "displayName": "X", "sharePct": "50"})
		assert.Equal(t, http.StatusBadRequest, res.Status, string(res.Body))
		assert.Equal(t, http.StatusOK, e.put(e.owner, owner("50"), map[string]any{"participantId": x.ID, "email": "X@example.com", "displayName": "Xavier", "sharePct": "50"}).Status, "same email, any case")
	})

	t.Run("a removed person can be invited again", func(t *testing.T) {
		bobU := s.register("bob@example.com")
		out := e.mustPut(owner("50"), emailP("bob@example.com", "Bob", "50"))
		id := byName(out.Items, "Bob").ID
		require.Equal(t, http.StatusOK, s.acceptShare(bobU, id, spaceIDOf(bobU)).Status)
		e.mustPut(owner("100")) // removes Bob
		assert.Empty(t, s.sharedList(bobU, spaceIDOf(bobU), "?include=shared").Items)
		out = e.mustPut(owner("50"), emailP("bob@example.com", "Bob", "50"))
		require.Len(t, s.shareInvitations(bobU), 1, "a fresh invitation")
		assert.NotEqual(t, id, byName(out.Items, "Bob").ID)
		require.Equal(t, http.StatusOK, s.acceptShare(bobU, s.shareInvitations(bobU)[0].ParticipantID, spaceIDOf(bobU)).Status)
	})
}

func TestSharedInvestmentInvitationLifecycle(t *testing.T) {
	s := NewTestServer(t)
	e := newShareEnv(t, s)
	bob := s.register("bob@example.com")
	other := s.register("other@example.com")
	out := e.mustPut(owner("60"), emailP("bob@example.com", "Bob", "40"))
	id := byName(out.Items, "Bob").ID

	t.Run("accepting needs a space of mine that is not the owner's", func(t *testing.T) {
		assert.Equal(t, http.StatusUnauthorized, s.Do(http.MethodPost, "/api/v1/me/shared-investment-invitations/"+id+"/accept", map[string]any{"spaceId": spaceIDOf(bob)}).Status)
		for name, body := range map[string]map[string]any{
			"missing space": {},
			"bad space id":  {"spaceId": "x"},
			"unknown field": {"spaceId": spaceIDOf(bob), "role": "OWNER"},
		} {
			res := s.Do(http.MethodPost, "/api/v1/me/shared-investment-invitations/"+id+"/accept", body, bearer(bob.AccessToken))
			assert.Equal(t, http.StatusBadRequest, res.Status, "%s: %s", name, res.Body)
		}
		assert.Equal(t, http.StatusNotFound, s.acceptShare(bob, id, spaceIDOf(other)).Status, "a space I am not a member of")
		assert.Equal(t, http.StatusNotFound, s.acceptShare(bob, id, uuid.NewString()).Status)
		assert.Equal(t, http.StatusNotFound, s.acceptShare(bob, uuid.NewString(), spaceIDOf(bob)).Status)
		assert.Equal(t, http.StatusNotFound, s.acceptShare(bob, "nope", spaceIDOf(bob)).Status)

		// make Bob a member of the owner's space: attaching it there is refused, it already lives there
		s.join(e.owner, e.sid, bob)
		res := s.acceptShare(bob, id, e.sid)
		require.Equal(t, http.StatusBadRequest, res.Status, string(res.Body))
		assert.Empty(t, s.sharedList(bob, spaceIDOf(bob), "?include=shared").Items, "nothing was attached")
	})

	t.Run("accept, then it cannot be answered again", func(t *testing.T) {
		require.Equal(t, http.StatusOK, s.acceptShare(bob, id, spaceIDOf(bob)).Status)
		res := s.acceptShare(bob, id, spaceIDOf(bob))
		require.Equal(t, http.StatusConflict, res.Status)
		assert.Equal(t, "SHARE_INVITATION_ANSWERED", errCode(t, res))
		assert.Equal(t, http.StatusConflict, s.declineShare(bob, id).Status)
		assert.Equal(t, "ACTIVE", byName(e.participants(), "Bob").Status)
	})

	t.Run("the attached space is the one chosen", func(t *testing.T) {
		second := s.Do(http.MethodPost, "/api/v1/spaces", map[string]any{"name": "Second"}, bearer(bob.AccessToken))
		var sp spaceBody
		second.JSON(t, &sp)
		assert.Len(t, s.sharedList(bob, spaceIDOf(bob), "?include=shared").Items, 1)
		assert.Empty(t, s.sharedList(bob, sp.ID, "?include=shared").Items, "only the attached space counts it")
	})
}

func TestDeclineKeepsTheShareAllocated(t *testing.T) {
	s := NewTestServer(t)
	e := newShareEnv(t, s)
	bob := s.register("bob@example.com")
	out := e.mustPut(owner("50"), emailP("bob@example.com", "Bob", "50"))
	id := byName(out.Items, "Bob").ID

	require.Equal(t, http.StatusNoContent, s.declineShare(bob, id).Status)
	assert.Equal(t, "DECLINED", byName(e.participants(), "Bob").Status)
	assert.Empty(t, s.shareInvitations(bob))
	assert.Empty(t, s.sharedList(bob, spaceIDOf(bob), "?include=shared").Items)
	it := ownerItem(s.sharedList(e.owner, e.sid, ""), e.inv.ID)
	assert.Equal(t, 5000, it.MyShareBps, "the owner still holds 50%: Bob's declined share is not theirs to take")
	assert.True(t, dec("500.01").Equal(dec(it.Valuation.Base.MarketValue)))
	res := s.acceptShare(bob, id, spaceIDOf(bob))
	assert.Equal(t, http.StatusConflict, res.Status, "a declined invitation stays declined")
	// the owner can resolve it by re-splitting
	e.mustPut(owner("100"))
}

func TestLeavingASharedInvestment(t *testing.T) {
	s := NewTestServer(t)
	e := newShareEnv(t, s)
	bob := s.register("bob@example.com")
	out := e.mustPut(owner("50"), emailP("bob@example.com", "Bob", "50"))
	id := byName(out.Items, "Bob").ID

	t.Run("only accepted participants can leave", func(t *testing.T) {
		assert.Equal(t, http.StatusNotFound, s.leaveShare(bob, id).Status, "still just invited")
		require.Equal(t, http.StatusOK, s.acceptShare(bob, id, spaceIDOf(bob)).Status)
		assert.Equal(t, http.StatusNotFound, s.leaveShare(e.owner, id).Status, "not mine")
		ownerPart := e.participants()[0].ID
		res := s.leaveShare(e.owner, ownerPart)
		require.Equal(t, http.StatusUnprocessableEntity, res.Status)
		assert.Equal(t, "SHARE_OWNER_CANNOT_LEAVE", errCode(t, res))
		assert.Equal(t, http.StatusUnauthorized, s.Do(http.MethodPost, "/api/v1/me/shared-investments/"+id+"/leave", nil).Status)
	})

	t.Run("leaving makes the share a name-only participation", func(t *testing.T) {
		require.Equal(t, http.StatusNoContent, s.leaveShare(bob, id).Status)
		assert.Empty(t, s.sharedList(bob, spaceIDOf(bob), "?include=shared").Items, "gone from Bob's space")
		b := byName(e.participants(), "Bob")
		assert.Equal(t, "ACTIVE", b.Status)
		assert.Equal(t, 5000, b.ShareBps, "the share stays allocated")
		assert.Nil(t, b.Email, "no longer tied to an account")
		assert.False(t, b.IsMe)
		it := ownerItem(s.sharedList(e.owner, e.sid, ""), e.inv.ID)
		assert.Equal(t, 5000, it.MyShareBps)
		assert.Equal(t, http.StatusNotFound, s.leaveShare(bob, id).Status, "a second time")
	})

	t.Run("the owner finds it in the audit trail", func(t *testing.T) {
		res := s.Get(invURL(e.sid, "/", e.inv.ID, "/share-changes"), bearer(e.owner.AccessToken))
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		var changes []struct {
			ChangedBy string `json:"changedById"`
			Before    []map[string]any
			After     []map[string]any
		}
		res.JSON(t, &changes)
		require.GreaterOrEqual(t, len(changes), 3, "share, accept, leave")
		last := changes[0]
		assert.Equal(t, bob.User.ID, last.ChangedBy, "the participant made the last change")
		var before, after map[string]any
		for _, c := range last.Before {
			if c["displayName"] == "Bob" {
				before = c
			}
		}
		for _, c := range last.After {
			if c["displayName"] == "Bob" {
				after = c
			}
		}
		assert.NotNil(t, before["email"])
		assert.Nil(t, after["email"])
	})
}

func TestStopSharingReturnsEverythingToTheOwner(t *testing.T) {
	s := NewTestServer(t)
	e := newShareEnv(t, s)
	bob := s.register("bob@example.com")
	max := s.register("max@example.com")
	s.join(e.owner, e.sid, max)
	out := e.mustPut(owner("50"), emailP("bob@example.com", "Bob", "30"), nameP("Mum", "20"))
	require.Equal(t, http.StatusOK, s.acceptShare(bob, byName(out.Items, "Bob").ID, spaceIDOf(bob)).Status)

	res := s.Do(http.MethodDelete, e.partsURL(), nil, bearer(e.owner.AccessToken))
	require.Equal(t, http.StatusNoContent, res.Status)

	assert.Empty(t, s.sharedList(bob, spaceIDOf(bob), "?include=shared").Items, "participants lose it")
	assert.Empty(t, s.shareInvitations(bob))
	it := ownerItem(s.sharedList(e.owner, e.sid, ""), e.inv.ID)
	assert.Equal(t, 10000, it.MyShareBps, "100% back to the owner")
	assert.True(t, dec("1000.01").Equal(dec(it.Valuation.Base.MarketValue)))
	assert.Nil(t, it.TotalValuation, "no longer shared")
	require.Len(t, e.participants(), 1)
	assert.Equal(t, 10000, e.participants()[0].ShareBps)
	var removed int
	require.NoError(t, s.Tx.QueryRow(context.Background(), `SELECT count(*) FROM investment_participants WHERE investment_id = $1 AND status = 'REMOVED'`, e.inv.ID).Scan(&removed))
	assert.Equal(t, 2, removed)

	t.Run("ordinary rules apply again and it can be shared anew", func(t *testing.T) {
		assert.Equal(t, http.StatusOK, s.Do(http.MethodPatch, invURL(e.sid, "/", e.inv.ID), map[string]any{"name": "Apple Inc"}, bearer(max.AccessToken)).Status,
			"an unshared investment can be edited by any member of the space")
		e.mustPut(owner("90"), nameP("Mum", "10"))
	})
	t.Run("stopping when nothing was shared is fine", func(t *testing.T) {
		assert.Equal(t, http.StatusNoContent, s.Do(http.MethodDelete, e.partsURL(), nil, bearer(e.owner.AccessToken)).Status)
		assert.Equal(t, http.StatusNoContent, s.Do(http.MethodDelete, e.partsURL(), nil, bearer(e.owner.AccessToken)).Status)
	})
}

func TestDeletingTheInvestmentHidesItForEveryone(t *testing.T) {
	s := NewTestServer(t)
	e := newShareEnv(t, s)
	bob, carol := s.register("bob@example.com"), s.register("carol@example.com")
	out := e.mustPut(owner("50"), emailP("bob@example.com", "Bob", "30"), emailP("carol@example.com", "Carol", "20"))
	require.Equal(t, http.StatusOK, s.acceptShare(bob, byName(out.Items, "Bob").ID, spaceIDOf(bob)).Status)

	require.Equal(t, http.StatusNoContent, s.Do(http.MethodDelete, invURL(e.sid, "/", e.inv.ID), nil, bearer(e.owner.AccessToken)).Status)
	assert.Empty(t, s.sharedList(bob, spaceIDOf(bob), "?include=shared").Items)
	assert.Empty(t, s.shareInvitations(carol), "a pending invitation disappears too")
	assert.Equal(t, http.StatusNotFound, s.acceptShare(carol, byName(out.Items, "Carol").ID, spaceIDOf(carol)).Status)
	assert.Empty(t, s.sharedList(e.owner, e.sid, "").Items)
}

func TestSharedInvestmentsAreOwnerOnlyAndReadOnlyForOthers(t *testing.T) {
	ctx := context.Background()
	s := NewTestServer(t)
	e := newShareEnv(t, s)
	bob := s.register("bob@example.com")
	max := s.register("max@example.com")
	s.join(e.owner, e.sid, max) // a MEMBER of the owner's space
	out := e.mustPut(owner("50"), emailP("bob@example.com", "Bob", "50"))
	require.Equal(t, http.StatusOK, s.acceptShare(bob, byName(out.Items, "Bob").ID, spaceIDOf(bob)).Status)
	w := s.newWallet(e.owner, e.sid, wal("Cash"))
	require.Equal(t, http.StatusOK, s.Do(http.MethodPut, walURL(e.sid, "/", w.ID, "/balance"), map[string]any{"newBalance": "10000"}, bearer(e.owner.AccessToken)).Status)

	t.Run("a participant can open it but not change it: writes are 403 OWNER_ONLY", func(t *testing.T) {
		mine := spaceIDOf(bob)
		wb := s.newWallet(bob, mine, wal("Bob cash"))
		for _, c := range []struct {
			method, path string
			body         any
		}{
			{http.MethodPatch, invURL(mine, "/", e.inv.ID), map[string]any{"name": "Mine now"}},
			{http.MethodDelete, invURL(mine, "/", e.inv.ID), nil},
			{http.MethodPost, invURL(mine, "/", e.inv.ID, "/refresh-price"), nil},
			{http.MethodPut, invURL(mine, "/", e.inv.ID, "/participants"), map[string]any{"participants": []map[string]any{owner("100")}}},
			{http.MethodGet, invURL(mine, "/", e.inv.ID, "/participants"), nil},
			{http.MethodDelete, invURL(mine, "/", e.inv.ID, "/participants"), nil},
			{http.MethodGet, invURL(mine, "/", e.inv.ID, "/share-changes"), nil},
			{http.MethodPost, trURL(mine), map[string]any{"from": walletEnd(wb), "to": leg(e.inv.ID, "1", "100", "")}},
		} {
			res := s.Do(c.method, c.path, c.body, bearer(bob.AccessToken))
			require.Equal(t, http.StatusForbidden, res.Status, "%s %s: %s", c.method, c.path, res.Body)
			assert.Equal(t, "OWNER_ONLY", errCode(t, res), "%s %s", c.method, c.path)
		}
		// and through the owner's space he is simply not a member
		expectCrossSpace404(t, s, bob, http.MethodPatch, invURL(e.sid, "/", e.inv.ID), map[string]any{"name": "x"})
		expectCrossSpace404(t, s, bob, http.MethodGet, e.partsURL(), nil)
		assert.Equal(t, "Apple", e.invEnv().get(e.inv.ID).Name, "unchanged")
		// a random id stays a plain 404, no leak either way
		assert.Equal(t, http.StatusNotFound, s.Do(http.MethodPatch, invURL(mine, "/", uuid.NewString()), map[string]any{"name": "x"}, bearer(bob.AccessToken)).Status)
	})

	t.Run("another member of the owner's space reads but cannot write: 403 OWNER_ONLY", func(t *testing.T) {
		assert.Equal(t, http.StatusOK, s.Get(invURL(e.sid, "/", e.inv.ID), bearer(max.AccessToken)).Status)
		assert.Len(t, s.sharedList(max, e.sid, "").Items, 1)
		for _, c := range []struct {
			name, method, path string
			body               any
		}{
			{"patch", http.MethodPatch, invURL(e.sid, "/", e.inv.ID), map[string]any{"name": "Max's"}},
			{"delete", http.MethodDelete, invURL(e.sid, "/", e.inv.ID), nil},
			{"refresh", http.MethodPost, invURL(e.sid, "/", e.inv.ID, "/refresh-price"), nil},
			{"participants get", http.MethodGet, e.partsURL(), nil},
			{"participants put", http.MethodPut, e.partsURL(), map[string]any{"participants": []map[string]any{owner("100")}}},
			{"participants delete", http.MethodDelete, e.partsURL(), nil},
			{"audit trail", http.MethodGet, invURL(e.sid, "/", e.inv.ID, "/share-changes"), nil},
			{"buy", http.MethodPost, trURL(e.sid), map[string]any{"from": walletEnd(w), "to": leg(e.inv.ID, "1", "100", "")}},
			{"sell", http.MethodPost, trURL(e.sid), map[string]any{"from": leg(e.inv.ID, "1", "100", ""), "to": walletEnd(w)}},
		} {
			res := s.Do(c.method, c.path, c.body, bearer(max.AccessToken))
			require.Equal(t, http.StatusForbidden, res.Status, "%s: %s", c.name, res.Body)
			assert.Equal(t, "OWNER_ONLY", errCode(t, res), c.name)
		}
		assert.Equal(t, "Apple", e.invEnv().get(e.inv.ID).Name)
	})

	t.Run("the owner can still trade, and allocation follows", func(t *testing.T) {
		res := s.Do(http.MethodPost, trURL(e.sid), map[string]any{"from": walletEnd(w), "to": leg(e.inv.ID, "1", "1000", "")}, bearer(e.owner.AccessToken))
		require.Equal(t, http.StatusCreated, res.Status, string(res.Body))
		var buy tradeBody
		res.JSON(t, &buy)
		// a trade by someone else on the owner's trade is refused too
		assert.Equal(t, http.StatusForbidden, s.Do(http.MethodPatch, trURL(e.sid, "/", buy.ID), map[string]any{"note": "x"}, bearer(max.AccessToken)).Status)
		assert.Equal(t, http.StatusForbidden, s.Do(http.MethodDelete, trURL(e.sid, "/", buy.ID), nil, bearer(max.AccessToken)).Status)
		assert.Equal(t, http.StatusOK, s.Do(http.MethodPatch, trURL(e.sid, "/", buy.ID), map[string]any{"note": "mine"}, bearer(e.owner.AccessToken)).Status)

		// quantity is now 2 at average 750 → 1500 cost; a sale realizes profit, split 50/50 by share
		res = s.Do(http.MethodPost, trURL(e.sid), map[string]any{"from": leg(e.inv.ID, "1", "1001", ""), "to": walletEnd(w)}, bearer(e.owner.AccessToken))
		require.Equal(t, http.StatusCreated, res.Status, string(res.Body))
		var sale tradeBody
		res.JSON(t, &sale)
		require.NotNil(t, sale.RealizedPL)
		assert.True(t, dec("251").Equal(dec(*sale.RealizedPL)), "(1001 − 750) × 1: %s", *sale.RealizedPL)

		whole := ownerItem(s.sharedList(e.owner, e.sid, ""), e.inv.ID)
		assert.True(t, dec("125.5").Equal(dec(*whole.RealizedPL.Base)), "the owner's half of 251: %s", *whole.RealizedPL.Base)
		theirs := s.sharedList(bob, spaceIDOf(bob), "?include=shared").Items[0]
		assert.True(t, dec("125.5").Equal(dec(*theirs.RealizedPL.Base)), "Bob's half")
		assert.Equal(t, 1, theirs.RealizedPL.Sells)
		_ = ctx
	})
}

func (e *shareEnv) invEnv() *invEnv { return &invEnv{s: e.s, u: e.owner, sid: e.sid} }

func TestSharedRealizedPLAddsUpToTheCent(t *testing.T) {
	s := NewTestServer(t)
	e := newShareEnv(t, s)
	ie := e.invEnv()
	inv := ie.must(map[string]any{"assetType": "CRYPTO", "name": "Bitcoin", "symbol": "BTC", "quantity": "3", "avgBuyPrice": "1", "manualPrice": "1"})
	bob, carol := s.register("bob@example.com"), s.register("carol@example.com")
	out := s.Do(http.MethodPut, invURL(e.sid, "/", inv.ID, "/participants"), map[string]any{"participants": []map[string]any{
		owner("33.33"), emailP("bob@example.com", "Bob", "33.33"), emailP("carol@example.com", "Carol", "33.34")}}, bearer(e.owner.AccessToken))
	require.Equal(t, http.StatusOK, out.Status, string(out.Body))
	var ps participantsResp
	out.JSON(t, &ps)
	require.Equal(t, http.StatusOK, s.acceptShare(bob, byName(ps.Items, "Bob").ID, spaceIDOf(bob)).Status)
	require.Equal(t, http.StatusOK, s.acceptShare(carol, byName(ps.Items, "Carol").ID, spaceIDOf(carol)).Status)

	w := s.newWallet(e.owner, e.sid, wal("Cash"))
	res := s.Do(http.MethodPost, trURL(e.sid), map[string]any{"from": leg(inv.ID, "1", "1.01", ""), "to": walletEnd(w)}, bearer(e.owner.AccessToken)) // realized 0.01
	require.Equal(t, http.StatusCreated, res.Status, string(res.Body))
	res = s.Do(http.MethodPost, trURL(e.sid), map[string]any{"from": leg(inv.ID, "1", "100", ""), "to": walletEnd(w)}, bearer(e.owner.AccessToken)) // realized 99
	require.Equal(t, http.StatusCreated, res.Status, string(res.Body))

	get := func(u session, sid, inc string) shareListItem {
		for _, it := range s.sharedList(u, sid, inc).Items {
			if it.ID == inv.ID {
				return it
			}
		}
		t.Fatal("investment not listed")
		return shareListItem{}
	}
	o := get(e.owner, e.sid, "")
	b := get(bob, spaceIDOf(bob), "?include=shared")
	c := get(carol, spaceIDOf(carol), "?include=shared")
	total := dec(*o.RealizedPL.Base).Add(dec(*b.RealizedPL.Base)).Add(dec(*c.RealizedPL.Base))
	assert.True(t, dec("99.01").Equal(total), "realized parts: %s + %s + %s = %s", *o.RealizedPL.Base, *b.RealizedPL.Base, *c.RealizedPL.Base, total)
	// the same holds for value and cost
	for _, pick := range []func(v valuationBody) string{
		func(v valuationBody) string { return v.Base.MarketValue }, func(v valuationBody) string { return v.Base.CostBasis },
	} {
		sum := dec(pick(o.Valuation)).Add(dec(pick(b.Valuation))).Add(dec(pick(c.Valuation)))
		assert.True(t, dec(pick(*o.TotalValuation)).Equal(sum), "%s vs %s", pick(*o.TotalValuation), sum)
	}
}

func TestSharedInListingFiltersAndIsolation(t *testing.T) {
	s := NewTestServer(t)
	e := newShareEnv(t, s)
	bob := s.register("bob@example.com")
	out := e.mustPut(owner("50"), emailP("bob@example.com", "Bob", "50"))
	require.Equal(t, http.StatusOK, s.acceptShare(bob, byName(out.Items, "Bob").ID, spaceIDOf(bob)).Status)
	own := (&invEnv{s: s, u: bob, sid: spaceIDOf(bob)}).must(clone(aapl, "name", "Bob's own", "manualPrice", "10"))

	l := s.sharedList(bob, spaceIDOf(bob), "?include=shared")
	require.Len(t, l.Items, 2)
	owned := 0
	for _, it := range l.Items {
		if it.Ownership == "OWNER" {
			owned++
			assert.Equal(t, own.ID, it.ID)
		}
	}
	assert.Equal(t, 1, owned)
	assert.True(t, dec("600").Equal(dec(l.Summary.TotalValue)), "own holding 100 + my share 500.00 (the odd cent goes to the owner, who comes first): %s", l.Summary.TotalValue)
	assert.Len(t, s.sharedList(bob, spaceIDOf(bob), "?include=shared&assetType=CRYPTO").Items, 0, "the asset type filter applies to shared-in items too")
	assert.Len(t, s.sharedList(bob, spaceIDOf(bob), "?include=shared&assetType=US_STOCK").Items, 2)
	assert.Equal(t, http.StatusBadRequest, s.Get(invURL(spaceIDOf(bob))+"?include=everything", bearer(bob.AccessToken)).Status)
	assert.Len(t, s.sharedList(bob, spaceIDOf(bob), "?include=SHARED").Items, 2, "case-insensitive")

	t.Run("outsiders and unrelated users see nothing", func(t *testing.T) {
		stranger := s.register("stranger@example.com")
		assert.Empty(t, s.sharedList(stranger, spaceIDOf(stranger), "?include=shared").Items)
		expectCrossSpace404(t, s, stranger, http.MethodGet, invURL(e.sid)+"?include=shared", nil)
	})
}

func TestShareChangeAuditTrail(t *testing.T) {
	s := NewTestServer(t)
	e := newShareEnv(t, s)
	e.mustPut(owner("50"), nameP("Mum", "50"))
	e.mustPut(owner("70"), nameP("Mum", "30"))
	res := s.Get(invURL(e.sid, "/", e.inv.ID, "/share-changes"), bearer(e.owner.AccessToken))
	require.Equal(t, http.StatusOK, res.Status)
	var changes []struct {
		ID            string `json:"id"`
		ChangedBy     string `json:"changedById"`
		Before, After []map[string]any
	}
	res.JSON(t, &changes)
	require.Len(t, changes, 2)
	newest := changes[0]
	assert.Len(t, newest.Before, 2)
	assert.EqualValues(t, 5000, newest.Before[0]["shareBps"])
	assert.EqualValues(t, 7000, newest.After[0]["shareBps"])
	assert.Empty(t, changes[1].Before, "the first change started from nothing")

	assert.Equal(t, http.StatusNotFound, s.Get(invURL(e.sid, "/", uuid.NewString(), "/share-changes"), bearer(e.owner.AccessToken)).Status)
}

func TestSharedInInvestmentDetailForTheParticipant(t *testing.T) {
	s := NewTestServer(t)
	require.NoError(t, s.Currency.Refresh(context.Background())) // 1 USD = 16000 IDR
	e := newShareEnv(t, s)
	bob := s.register("bob@example.com")
	carol := s.register("carol@example.com") // stays INVITED
	dave := s.register("dave@example.com")   // declines
	erin := s.register("erin@example.com")   // leaves
	fred := s.register("fred@example.com")   // gets removed
	out := e.mustPut(owner("30"), emailP("bob@example.com", "Bob", "20"), emailP("carol@example.com", "Carol", "10"),
		emailP("dave@example.com", "Dave", "10"), emailP("erin@example.com", "Erin", "10"), emailP("fred@example.com", "Fred", "20"))
	acc := func(u session, name string) {
		require.Equal(t, http.StatusOK, s.acceptShare(u, byName(out.Items, name).ID, spaceIDOf(u)).Status)
	}
	acc(bob, "Bob")
	acc(erin, "Erin")
	acc(fred, "Fred")
	require.Equal(t, http.StatusNoContent, s.declineShare(dave, byName(out.Items, "Dave").ID).Status)
	require.Equal(t, http.StatusNoContent, s.leaveShare(erin, byName(out.Items, "Erin").ID).Status)
	// Fred is left out: removed; the others stay by id and the owner takes Fred's 20
	keep := func(name, email, pct string) map[string]any {
		m := emailP(email, name, pct)
		m["participantId"] = byName(out.Items, name).ID
		return m
	}
	e.mustPut(owner("50"), keep("Bob", "bob@example.com", "20"), keep("Carol", "carol@example.com", "10"),
		keep("Dave", "dave@example.com", "10"), map[string]any{"participantId": byName(out.Items, "Erin").ID, "displayName": "Erin", "sharePct": "10"})

	t.Run("an active participant opens it in their attached space", func(t *testing.T) {
		require.Equal(t, http.StatusOK, s.Do(http.MethodPatch, "/api/v1/spaces/"+spaceIDOf(bob), map[string]any{"baseCurrency": "IDR"}, bearer(bob.AccessToken)).Status)
		res := s.Get(invURL(spaceIDOf(bob), "/", e.inv.ID), bearer(bob.AccessToken))
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		var it shareListItem
		res.JSON(t, &it)
		assert.Equal(t, "SHARED_IN", it.Ownership)
		assert.Equal(t, 2000, it.MyShareBps)
		require.NotNil(t, it.TotalValuation)
		assert.True(t, dec("1000.01").Equal(dec(it.TotalValuation.MarketValue)), "the whole in the quote currency")
		assert.Equal(t, "200.00", it.Valuation.MarketValue, "Bob's 20%% of 1000.01, shown with 2 decimals")
		require.NotNil(t, it.Valuation.Base)
		assert.Equal(t, "IDR", it.Valuation.Base.Currency, "valued in Bob's base currency, not the owner's")
		assert.True(t, dec("3200032").Equal(dec(it.Valuation.Base.MarketValue)), it.Valuation.Base.MarketValue)
		raw := fmt.Sprint(it)
		assert.NotContains(t, raw, "jane@example.com")
		assert.NotContains(t, raw, e.sid)
		me := byName(it.Participants, "Bob")
		assert.True(t, me.IsMe)
		require.NotNil(t, byName(it.Participants, "Carol").Email)
		assert.Equal(t, "c***@example.com", *byName(it.Participants, "Carol").Email)
		// it equals what the list shows
		l := s.sharedList(bob, spaceIDOf(bob), "?include=shared")
		assert.Equal(t, l.Items[0].Valuation.Base.MarketValue, it.Valuation.Base.MarketValue)
	})

	t.Run("everyone else gets 404", func(t *testing.T) {
		for name, u := range map[string]session{"invited": carol, "declined": dave, "left": erin, "removed": fred} {
			assert.Equal(t, http.StatusNotFound, s.Get(invURL(spaceIDOf(u), "/", e.inv.ID), bearer(u.AccessToken)).Status, name)
		}
		// the owner's space is off limits to a non-member, and unknown ids are 404
		expectCrossSpace404(t, s, bob, http.MethodGet, invURL(e.sid, "/", e.inv.ID), nil)
		assert.Equal(t, http.StatusNotFound, s.Get(invURL(spaceIDOf(bob), "/", uuid.NewString()), bearer(bob.AccessToken)).Status)
		assert.Equal(t, http.StatusNotFound, s.Get(invURL(spaceIDOf(bob), "/", "nope"), bearer(bob.AccessToken)).Status)
	})

	t.Run("only the attached space opens it", func(t *testing.T) {
		res := s.Do(http.MethodPost, "/api/v1/spaces", map[string]any{"name": "Second"}, bearer(bob.AccessToken))
		var sp spaceBody
		res.JSON(t, &sp)
		assert.Equal(t, http.StatusNotFound, s.Get(invURL(sp.ID, "/", e.inv.ID), bearer(bob.AccessToken)).Status)
	})

	t.Run("the owner still sees their own view", func(t *testing.T) {
		res := s.Get(invURL(e.sid, "/", e.inv.ID), bearer(e.owner.AccessToken))
		require.Equal(t, http.StatusOK, res.Status)
		var it shareListItem
		res.JSON(t, &it)
		assert.Equal(t, "OWNER", it.Ownership)
		assert.Equal(t, 5000, it.MyShareBps)
	})
}

func TestShareFlowResponsesLetTheClientUpdateInPlace(t *testing.T) {
	s := NewTestServer(t)
	e := newShareEnv(t, s)
	bob := s.register("bob@example.com")

	t.Run("PUT participants returns the investment as it is now", func(t *testing.T) {
		res := e.put(e.owner, owner("60"), emailP("bob@example.com", "Bob", "40"))
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		var out struct {
			Items      []participantBody `json:"items"`
			Investment shareListItem     `json:"investment"`
		}
		res.JSON(t, &out)
		require.Len(t, out.Items, 2)
		assert.Equal(t, e.inv.ID, out.Investment.ID)
		assert.True(t, out.Investment.IsShared, "fresh isShared")
		assert.Len(t, out.Investment.Participants, 2)
		assert.Equal(t, 6000, out.Investment.MyShareBps)
		require.NotNil(t, out.Investment.TotalValuation)
		assert.True(t, dec("600.01").Equal(dec(out.Investment.Valuation.MarketValue)) || dec("600").Equal(dec(out.Investment.Valuation.MarketValue)), out.Investment.Valuation.MarketValue)
		// GET participants keeps its shape
		assert.NotContains(t, string(s.Get(e.partsURL(), bearer(e.owner.AccessToken)).Body), `"investment"`)
	})

	t.Run("accept returns where to open it, and the participant can", func(t *testing.T) {
		inv := s.shareInvitations(bob)[0]
		res := s.acceptShare(bob, inv.ParticipantID, spaceIDOf(bob))
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		var got struct {
			SpaceID      string `json:"spaceId"`
			InvestmentID string `json:"investmentId"`
		}
		res.JSON(t, &got)
		assert.Equal(t, spaceIDOf(bob), got.SpaceID)
		assert.Equal(t, e.inv.ID, got.InvestmentID)
		open := s.Get(invURL(got.SpaceID, "/", got.InvestmentID), bearer(bob.AccessToken))
		require.Equal(t, http.StatusOK, open.Status, string(open.Body))
		var it shareListItem
		open.JSON(t, &it)
		assert.Equal(t, "SHARED_IN", it.Ownership)
		assert.Equal(t, 4000, it.MyShareBps)
	})

	t.Run("stop sharing: 204 by default, the investment with ?return=investment", func(t *testing.T) {
		bad := s.Do(http.MethodDelete, e.partsURL()+"?return=nope", nil, bearer(e.owner.AccessToken))
		assert.Equal(t, http.StatusBadRequest, bad.Status)
		assert.True(t, e.invEnv().get(e.inv.ID).IsShared, "a rejected call changed nothing")

		res := s.Do(http.MethodDelete, e.partsURL()+"?return=investment", nil, bearer(e.owner.AccessToken))
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		var it shareListItem
		res.JSON(t, &it)
		assert.Equal(t, e.inv.ID, it.ID)
		assert.False(t, it.IsShared)
		assert.Equal(t, 10000, it.MyShareBps)

		e.mustPut(owner("50"), emailP("bob@example.com", "Bob", "50"))
		assert.Equal(t, http.StatusNoContent, s.Do(http.MethodDelete, e.partsURL(), nil, bearer(e.owner.AccessToken)).Status)
	})
}
