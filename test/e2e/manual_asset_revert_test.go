//go:build e2e

package e2e

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func changeURL(sid, asset, change string) string {
	return assetURL(sid, "/", asset, "/value-changes/", change)
}

func (e *assetEnv) revert(asset, change string) Response {
	return e.s.Do(http.MethodDelete, changeURL(e.sid, asset, change), nil, bearer(e.u.AccessToken))
}

func TestRevertingAValueChange(t *testing.T) {
	s := NewTestServer(t)
	e := newAssetEnv(t, s)
	house := e.must(map[string]any{"kind": "FIXED_ASSET", "name": "House", "currentValue": "300000"})
	require.Equal(t, http.StatusOK, e.patch(house.ID, map[string]any{"currentValue": "310000"}).Status)
	require.Equal(t, http.StatusOK, e.patch(house.ID, map[string]any{"currentValue": "330000"}).Status)

	t.Run("each entry has an id and only the newest value change can be reverted", func(t *testing.T) {
		h := e.get(house.ID).History
		require.Len(t, h, 3)
		for _, c := range h {
			_, err := uuid.Parse(c.ID)
			require.NoError(t, err)
		}
		assert.True(t, h[0].CanRevert, "newest")
		assert.False(t, h[1].CanRevert, "an older one")
		assert.False(t, h[2].CanRevert, "the first value is the creation, not a change")
	})

	t.Run("reverting an older one is 409", func(t *testing.T) {
		h := e.get(house.ID).History
		res := e.revert(house.ID, h[1].ID)
		require.Equal(t, http.StatusConflict, res.Status, string(res.Body))
		assert.Equal(t, "VALUE_CHANGED_SINCE", errCode(t, res))
		assert.True(t, dec("330000").Equal(dec(e.get(house.ID).CurrentValue)), "nothing changed")
		assert.Len(t, e.get(house.ID).History, 3)
	})

	t.Run("the creation entry cannot be reverted", func(t *testing.T) {
		solo := e.must(map[string]any{"kind": "FIXED_ASSET", "name": "Solo", "currentValue": "10"})
		h := e.get(solo.ID).History
		require.Len(t, h, 1)
		assert.False(t, h[0].CanRevert)
		res := e.revert(solo.ID, h[0].ID)
		require.Equal(t, http.StatusUnprocessableEntity, res.Status, string(res.Body))
		assert.Equal(t, "CANNOT_REVERT_INITIAL_VALUE", errCode(t, res))
	})

	t.Run("reverting the newest restores the value and the net worth follows", func(t *testing.T) {
		before := dec(*s.networth(e.u, e.sid).FixedAssets)
		h := e.get(house.ID).History
		res := e.revert(house.ID, h[0].ID)
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		var got assetBody
		res.JSON(t, &got)
		assert.True(t, dec("310000").Equal(dec(got.CurrentValue)))
		assert.Len(t, got.History, 2, "the entry is gone")
		assert.True(t, got.History[0].CanRevert, "the previous change is the newest now")
		after := dec(*s.networth(e.u, e.sid).FixedAssets)
		assert.True(t, before.Sub(after).Equal(dec("20000")), "net worth: %s -> %s", before, after)

		// and again, down to the creation entry
		res = e.revert(house.ID, got.History[0].ID)
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		assert.True(t, dec("300000").Equal(dec(e.get(house.ID).CurrentValue)))
	})

	t.Run("a reverted entry is gone: a second revert is 404", func(t *testing.T) {
		other := e.must(map[string]any{"kind": "FIXED_ASSET", "name": "Car", "currentValue": "100"})
		require.Equal(t, http.StatusOK, e.patch(other.ID, map[string]any{"currentValue": "90"}).Status)
		id := e.get(other.ID).History[0].ID
		require.Equal(t, http.StatusOK, e.revert(other.ID, id).Status)
		res := e.revert(other.ID, id)
		require.Equal(t, http.StatusNotFound, res.Status)
		assert.Equal(t, "VALUE_CHANGE_NOT_FOUND", errCode(t, res))
	})

	t.Run("not found cases", func(t *testing.T) {
		id := e.get(house.ID).History[0].ID
		res := e.revert(uuid.NewString(), id)
		require.Equal(t, http.StatusNotFound, res.Status)
		assert.Equal(t, "MANUAL_ASSET_NOT_FOUND", errCode(t, res))
		assert.Equal(t, http.StatusNotFound, e.revert(house.ID, uuid.NewString()).Status)
		assert.Equal(t, http.StatusNotFound, e.revert(house.ID, "nope").Status)
		// an entry of another asset
		res = e.revert(e.must(map[string]any{"kind": "FIXED_ASSET", "name": "Elsewhere", "currentValue": "1"}).ID, id)
		assert.Equal(t, http.StatusNotFound, res.Status)
	})

	t.Run("a deleted asset answers 404", func(t *testing.T) {
		gone := e.must(map[string]any{"kind": "FIXED_ASSET", "name": "Gone", "currentValue": "5"})
		require.Equal(t, http.StatusOK, e.patch(gone.ID, map[string]any{"currentValue": "6"}).Status)
		id := e.get(gone.ID).History[0].ID
		require.Equal(t, http.StatusNoContent, s.Do(http.MethodDelete, assetURL(e.sid, "/", gone.ID), nil, bearer(e.u.AccessToken)).Status)
		res := e.revert(gone.ID, id)
		require.Equal(t, http.StatusNotFound, res.Status)
		assert.Equal(t, "MANUAL_ASSET_NOT_FOUND", errCode(t, res))
	})

	t.Run("unauthenticated and other spaces", func(t *testing.T) {
		id := e.get(house.ID).History[0].ID
		assert.Equal(t, http.StatusUnauthorized, s.Do(http.MethodDelete, changeURL(e.sid, house.ID, id), nil).Status)
		outsider := s.register("mallory@example.com")
		expectCrossSpace404(t, s, outsider, http.MethodDelete, changeURL(e.sid, house.ID, id), nil)
	})
}

func TestSettlementValueChangesPointToTheSettlementDelete(t *testing.T) {
	s := NewTestServer(t)
	e := newAssetEnv(t, s)
	card := e.must(debt("Card", "500"))
	res := e.settle(card.ID, map[string]any{"walletId": e.a.ID, "amount": "200"})
	require.Equal(t, http.StatusCreated, res.Status, string(res.Body))
	var p settleBody
	res.JSON(t, &p)

	h := e.get(card.ID).History
	require.Len(t, h, 2)
	assert.Equal(t, "SETTLEMENT", h[0].Reason)
	assert.True(t, h[0].CanRevert, "covered by the settlement delete")

	res = e.revert(card.ID, h[0].ID)
	require.Equal(t, http.StatusConflict, res.Status, string(res.Body))
	assert.Equal(t, "USE_SETTLEMENT_DELETE", errCode(t, res))
	var er struct {
		Error struct {
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	res.JSON(t, &er)
	assert.Equal(t, p.Transaction.ID, er.Error.Details["transactionId"], "the id to send to the settlement delete")
	assert.True(t, dec("300").Equal(dec(e.get(card.ID).CurrentValue)), "nothing changed")

	// the settlement delete keeps working; its own reversal entry is then not revertible
	require.Equal(t, http.StatusNoContent, s.Do(http.MethodDelete, assetURL(e.sid, "/", card.ID, "/settlements/", p.Transaction.ID), nil, bearer(e.u.AccessToken)).Status)
	h = e.get(card.ID).History
	assert.Equal(t, "SETTLEMENT", h[0].Reason)
	assert.False(t, h[0].CanRevert, "the settlement it belongs to is already gone")
	res = e.revert(card.ID, h[0].ID)
	require.Equal(t, http.StatusConflict, res.Status)
	assert.Equal(t, "USE_SETTLEMENT_DELETE", errCode(t, res))
}
