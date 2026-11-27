//go:build e2e

package e2e

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- shared test bodies (mirrors sharing_test.go's investment shapes) --------------

type walletShareListItem struct {
	walletBody
	Ownership  string  `json:"ownership"`
	IsShared   bool    `json:"isShared"`
	MyShareBps *int    `json:"myShareBps"`
	MyBalance  *string `json:"myBalance"`
}

type walletSharedListBody struct {
	Items []walletShareListItem `json:"items"`
}

type walletInvitationBody struct {
	ParticipantID string `json:"participantId"`
	OwnerName     string `json:"ownerName"`
	Wallet        struct {
		Name     string `json:"name"`
		Currency string `json:"currency"`
	} `json:"wallet"`
	SharePct string `json:"sharePct"`
	ShareBps int    `json:"shareBps"`
}

type txnSplitBody struct {
	ParticipantID string `json:"participantId"`
	DisplayName   string `json:"displayName"`
	ShareBps      int    `json:"shareBps"`
	Amount        string `json:"amount"`
}

func wPartsURL(sid, walletID string) string { return walURL(sid, "/", walletID, "/participants") }

func (s *TestServer) wPut(u session, sid, walletID string, list ...map[string]any) Response {
	return s.Do(http.MethodPut, wPartsURL(sid, walletID), map[string]any{"participants": list}, bearer(u.AccessToken))
}

func (s *TestServer) wMustPut(u session, sid, walletID string, list ...map[string]any) participantsResp {
	s.t.Helper()
	res := s.wPut(u, sid, walletID, list...)
	require.Equal(s.t, http.StatusOK, res.Status, string(res.Body))
	var out participantsResp
	res.JSON(s.t, &out)
	return out
}

func (s *TestServer) wParticipants(u session, sid, walletID string) []participantBody {
	s.t.Helper()
	res := s.Get(wPartsURL(sid, walletID), bearer(u.AccessToken))
	require.Equal(s.t, http.StatusOK, res.Status, string(res.Body))
	var out participantsResp
	res.JSON(s.t, &out)
	return out.Items
}

func (s *TestServer) wSharedList(u session, sid string) walletSharedListBody {
	s.t.Helper()
	res := s.Get(walURL(sid)+"?include=shared", bearer(u.AccessToken))
	require.Equal(s.t, http.StatusOK, res.Status, string(res.Body))
	var l walletSharedListBody
	res.JSON(s.t, &l)
	return l
}

func (s *TestServer) wInvitations(u session) []walletInvitationBody {
	s.t.Helper()
	res := s.Get("/api/v1/me/shared-wallet-invitations", bearer(u.AccessToken))
	require.Equal(s.t, http.StatusOK, res.Status, string(res.Body))
	var out []walletInvitationBody
	res.JSON(s.t, &out)
	return out
}

func (s *TestServer) wAcceptShare(u session, id, space string) Response {
	return s.Do(http.MethodPost, "/api/v1/me/shared-wallet-invitations/"+id+"/accept", map[string]any{"spaceId": space}, bearer(u.AccessToken))
}

func (s *TestServer) wDeclineShare(u session, id string) Response {
	return s.Do(http.MethodPost, "/api/v1/me/shared-wallet-invitations/"+id+"/decline", nil, bearer(u.AccessToken))
}

func (s *TestServer) wLeaveShare(u session, id string) Response {
	return s.Do(http.MethodPost, "/api/v1/me/shared-wallets/"+id+"/leave", nil, bearer(u.AccessToken))
}

// --- tests --------------------------------------------------------------------------

func TestWalletSharing_ParticipantsAndValidation(t *testing.T) {
	s := NewTestServer(t)
	jane := s.register("jane@example.com")
	sid := spaceIDOf(jane)
	w := s.newWallet(jane, sid, wal("Joint"))

	t.Run("sharing sets isShared and the owner can list participants", func(t *testing.T) {
		out := s.wMustPut(jane, sid, w.ID, owner("70"), nameP("Alex", "30"))
		require.Len(t, out.Items, 2)
		ps := s.wParticipants(jane, sid, w.ID)
		require.Len(t, ps, 2)
		assert.Equal(t, "ACTIVE", byName(ps, "Alex").Status)
	})

	t.Run("shares must sum to exactly 100.00", func(t *testing.T) {
		res := s.wPut(jane, sid, w.ID, owner("70"), nameP("Alex", "29.99"))
		assert.Equal(t, http.StatusUnprocessableEntity, res.Status)
		assert.Equal(t, "SHARE_TOTAL_INVALID", errCode(t, res))
	})

	t.Run("a non-owner member of the same space gets OWNER_ONLY", func(t *testing.T) {
		mallory := s.register("mallory@example.com")
		s.join(jane, sid, mallory)
		res := s.wPut(mallory, sid, w.ID, owner("70"), nameP("Alex", "30"))
		assert.Equal(t, http.StatusForbidden, res.Status)
		assert.Equal(t, "OWNER_ONLY", errCode(t, res))
	})

	t.Run("stop sharing returns 100% to the owner", func(t *testing.T) {
		res := s.Do(http.MethodDelete, wPartsURL(sid, w.ID), nil, bearer(jane.AccessToken))
		require.Equal(t, http.StatusNoContent, res.Status)
		ps := s.wParticipants(jane, sid, w.ID)
		require.Len(t, ps, 1)
		assert.Equal(t, 10000, ps[0].ShareBps)
	})
}

func TestWalletSharing_TransactionSplits(t *testing.T) {
	s := NewTestServer(t)
	jane := s.register("jane@example.com")
	sid := spaceIDOf(jane)
	w := s.newWallet(jane, sid, map[string]any{"type": "BANK", "name": "Joint", "icon": "🏦", "initialBalance": "1000"})
	s.wMustPut(jane, sid, w.ID, owner("70"), nameP("Alex", "30"))

	res := s.Do(http.MethodPost, txURL(sid), exp(w, "100", ""), bearer(jane.AccessToken))
	require.Equal(t, http.StatusCreated, res.Status, string(res.Body))
	var created struct {
		ID     string         `json:"id"`
		Splits []txnSplitBody `json:"splits"`
	}
	res.JSON(t, &created)
	require.Len(t, created.Splits, 2)

	byShare := map[int]txnSplitBody{}
	for _, sp := range created.Splits {
		byShare[sp.ShareBps] = sp
	}
	assert.True(t, decimal.RequireFromString("-70").Equal(decimal.RequireFromString(byShare[7000].Amount)), "70%% of a -100 expense")
	assert.True(t, decimal.RequireFromString("-30").Equal(decimal.RequireFromString(byShare[3000].Amount)), "30%% of a -100 expense")

	t.Run("the list endpoint carries the same splits", func(t *testing.T) {
		lres := s.Get(txURL(sid), bearer(jane.AccessToken))
		require.Equal(t, http.StatusOK, lres.Status)
		var list struct {
			Items []struct {
				ID     string         `json:"id"`
				Splits []txnSplitBody `json:"splits"`
			} `json:"items"`
		}
		lres.JSON(t, &list)
		var found bool
		for _, it := range list.Items {
			if it.ID == created.ID {
				found = true
				require.Len(t, it.Splits, 2)
			}
		}
		assert.True(t, found)
	})

	t.Run("an unshared wallet's transactions carry no splits", func(t *testing.T) {
		plain := s.newWallet(jane, sid, wal("Solo"))
		pres := s.Do(http.MethodPost, txURL(sid), exp(plain, "50", ""), bearer(jane.AccessToken))
		require.Equal(t, http.StatusCreated, pres.Status)
		var body struct {
			Splits []txnSplitBody `json:"splits"`
		}
		pres.JSON(t, &body)
		assert.Empty(t, body.Splits)
	})
}

func TestWalletSharing_InvitationLifecycle(t *testing.T) {
	s := NewTestServer(t)
	jane := s.register("jane@example.com")
	bob := s.register("bob@example.com")
	sid := spaceIDOf(jane)
	w := s.newWallet(jane, sid, map[string]any{"type": "BANK", "name": "Joint", "icon": "🏦", "initialBalance": "1000"})

	out := s.wMustPut(jane, sid, w.ID, owner("60"), emailP("bob@example.com", "Bob", "40"))
	id := byName(out.Items, "Bob").ID

	t.Run("bob sees the invitation and must attach it to his own space", func(t *testing.T) {
		inv := s.wInvitations(bob)
		require.Len(t, inv, 1)
		assert.Equal(t, "Joint", inv[0].Wallet.Name)
		assert.Equal(t, 4000, inv[0].ShareBps)

		assert.Equal(t, http.StatusNotFound, s.wAcceptShare(bob, id, uuid.NewString()).Status)
		require.Equal(t, http.StatusOK, s.wAcceptShare(bob, id, spaceIDOf(bob)).Status)
	})

	t.Run("bob's space lists it read-only with his share of the balance", func(t *testing.T) {
		got := s.wSharedList(bob, spaceIDOf(bob))
		require.Len(t, got.Items, 1)
		item := got.Items[0]
		assert.Equal(t, "SHARED_IN", item.Ownership)
		require.NotNil(t, item.MyShareBps)
		assert.Equal(t, 4000, *item.MyShareBps)
		require.NotNil(t, item.MyBalance)
		assert.True(t, decimal.RequireFromString("400").Equal(decimal.RequireFromString(*item.MyBalance)), "40%% of 1000")
	})

	t.Run("bob cannot write to it from his own space", func(t *testing.T) {
		res := s.Do(http.MethodPatch, walURL(spaceIDOf(bob), "/", w.ID), map[string]any{"name": "Hijacked"}, bearer(bob.AccessToken))
		assert.Equal(t, http.StatusNotFound, res.Status)
	})

	t.Run("leaving keeps the share but detaches bob", func(t *testing.T) {
		require.Equal(t, http.StatusNoContent, s.wLeaveShare(bob, id).Status)
		assert.Empty(t, s.wSharedList(bob, spaceIDOf(bob)).Items)
		ps := s.wParticipants(jane, sid, w.ID)
		assert.Equal(t, 4000, byName(ps, "Bob").ShareBps, "the share stays allocated")
	})
}

func TestWalletSharing_Decline(t *testing.T) {
	s := NewTestServer(t)
	jane := s.register("jane@example.com")
	bob := s.register("bob@example.com")
	sid := spaceIDOf(jane)
	w := s.newWallet(jane, sid, wal("Joint"))
	out := s.wMustPut(jane, sid, w.ID, owner("60"), emailP("bob@example.com", "Bob", "40"))
	id := byName(out.Items, "Bob").ID

	require.Equal(t, http.StatusNoContent, s.wDeclineShare(bob, id).Status)
	assert.Equal(t, http.StatusConflict, s.wDeclineShare(bob, id).Status, "already answered")
	assert.Empty(t, s.wSharedList(bob, spaceIDOf(bob)).Items)
}

func TestWalletSharing_NetWorthAttribution(t *testing.T) {
	s := NewTestServer(t)
	jane := s.register("jane@example.com")
	bob := s.register("bob@example.com")
	sid, bsid := spaceIDOf(jane), spaceIDOf(bob)
	w := s.newWallet(jane, sid, map[string]any{"type": "BANK", "name": "Joint", "icon": "🏦", "initialBalance": "1000"})

	before := s.networth(jane, sid)
	assert.True(t, decimal.RequireFromString("1000").Equal(decimal.RequireFromString(before.Cash)), "unshared: the owner's space counts it all")

	out := s.wMustPut(jane, sid, w.ID, owner("70"), emailP("bob@example.com", "Bob", "30"))
	id := byName(out.Items, "Bob").ID
	require.Equal(t, http.StatusOK, s.wAcceptShare(bob, id, bsid).Status)

	ownerNW := s.networth(jane, sid)
	assert.True(t, decimal.RequireFromString("700").Equal(decimal.RequireFromString(ownerNW.Cash)), "shared: the owner's own space only counts the owner's 70%%")

	bobNW := s.networth(bob, bsid)
	assert.True(t, decimal.RequireFromString("300").Equal(decimal.RequireFromString(bobNW.Cash)), "bob's space counts his 30%% share, nothing of his own")
}
