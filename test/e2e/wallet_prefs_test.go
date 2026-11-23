//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type walletPrefsBody struct {
	HideLogo bool `json:"hideLogo"`
	HideName bool `json:"hideName"`
	HideType bool `json:"hideType"`
}

type walletWithPrefs struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	DisplayPrefs walletPrefsBody `json:"displayPrefs"`
}

func prefsURL(sid, id string) string { return walURL(sid, "/", id, "/display-prefs") }

func (s *TestServer) walletsWithPrefs(u session, sid string) map[string]walletWithPrefs {
	s.t.Helper()
	res := s.Get(walURL(sid), bearer(u.AccessToken))
	require.Equal(s.t, http.StatusOK, res.Status, string(res.Body))
	var l struct {
		Items []walletWithPrefs `json:"items"`
	}
	res.JSON(s.t, &l)
	out := map[string]walletWithPrefs{}
	for _, w := range l.Items {
		out[w.ID] = w
	}
	return out
}

func (s *TestServer) getWalletPrefs(u session, sid, id string) walletPrefsBody {
	s.t.Helper()
	res := s.Get(walURL(sid, "/", id), bearer(u.AccessToken))
	require.Equal(s.t, http.StatusOK, res.Status, string(res.Body))
	var w walletWithPrefs
	res.JSON(s.t, &w)
	return w.DisplayPrefs
}

func (s *TestServer) prefRows(walletID string) int {
	s.t.Helper()
	var n int
	require.NoError(s.t, s.Tx.QueryRow(context.Background(), `SELECT count(*) FROM user_wallet_prefs WHERE wallet_id = $1`, walletID).Scan(&n))
	return n
}

func TestWalletDisplayPrefs(t *testing.T) {
	s := NewTestServer(t)
	jane := s.register("jane@example.com")
	sid := spaceIDOf(jane)
	a := s.newWallet(jane, sid, wal("A"))
	b := s.newWallet(jane, sid, wal("B"))
	max := s.register("max@example.com")
	s.join(jane, sid, max)

	t.Run("defaults: nothing hidden, in the list and the single wallet", func(t *testing.T) {
		assert.Equal(t, walletPrefsBody{}, s.walletsWithPrefs(jane, sid)[a.ID].DisplayPrefs)
		assert.Equal(t, walletPrefsBody{}, s.getWalletPrefs(jane, sid, a.ID))
	})

	t.Run("set, read back, partial patches keep the rest", func(t *testing.T) {
		res := s.Do(http.MethodPatch, prefsURL(sid, a.ID), map[string]any{"hideLogo": true, "hideName": false}, bearer(jane.AccessToken))
		require.Equal(t, http.StatusOK, res.Status, string(res.Body))
		var got walletPrefsBody
		res.JSON(t, &got)
		assert.Equal(t, walletPrefsBody{HideLogo: true}, got)
		assert.Equal(t, walletPrefsBody{HideLogo: true}, s.getWalletPrefs(jane, sid, a.ID))
		assert.Equal(t, walletPrefsBody{HideLogo: true}, s.walletsWithPrefs(jane, sid)[a.ID].DisplayPrefs)
		assert.Equal(t, walletPrefsBody{}, s.walletsWithPrefs(jane, sid)[b.ID].DisplayPrefs, "per wallet")

		res = s.Do(http.MethodPatch, prefsURL(sid, a.ID), map[string]any{"hideType": true}, bearer(jane.AccessToken))
		require.Equal(t, http.StatusOK, res.Status)
		assert.Equal(t, walletPrefsBody{HideLogo: true, HideType: true}, s.getWalletPrefs(jane, sid, a.ID), "hideLogo kept")
		// the other wallet-returning calls carry them too
		res = s.Do(http.MethodPatch, walURL(sid, "/", a.ID), map[string]any{"name": "A2"}, bearer(jane.AccessToken))
		var w walletWithPrefs
		res.JSON(t, &w)
		assert.Equal(t, walletPrefsBody{HideLogo: true, HideType: true}, w.DisplayPrefs)
		assert.Equal(t, 1, s.prefRows(a.ID))
	})

	t.Run("private to the user: another member sees defaults and has their own", func(t *testing.T) {
		assert.Equal(t, walletPrefsBody{}, s.getWalletPrefs(max, sid, a.ID))
		assert.Equal(t, walletPrefsBody{}, s.walletsWithPrefs(max, sid)[a.ID].DisplayPrefs)
		require.Equal(t, http.StatusOK, s.Do(http.MethodPatch, prefsURL(sid, a.ID), map[string]any{"hideName": true}, bearer(max.AccessToken)).Status)
		assert.Equal(t, walletPrefsBody{HideName: true}, s.getWalletPrefs(max, sid, a.ID))
		assert.Equal(t, walletPrefsBody{HideLogo: true, HideType: true}, s.getWalletPrefs(jane, sid, a.ID), "Jane's are untouched")
		assert.Equal(t, 2, s.prefRows(a.ID))
	})

	t.Run("all false is the default and deletes the row", func(t *testing.T) {
		res := s.Do(http.MethodPatch, prefsURL(sid, a.ID), map[string]any{"hideLogo": false, "hideName": false, "hideType": false}, bearer(jane.AccessToken))
		require.Equal(t, http.StatusOK, res.Status)
		var got walletPrefsBody
		res.JSON(t, &got)
		assert.Equal(t, walletPrefsBody{}, got)
		assert.Equal(t, 1, s.prefRows(a.ID), "only Max's row is left")
		assert.Equal(t, http.StatusOK, s.Do(http.MethodPatch, prefsURL(sid, a.ID), map[string]any{}, bearer(jane.AccessToken)).Status, "an empty patch is harmless")
	})

	t.Run("errors", func(t *testing.T) {
		stranger := s.register("stranger@example.com")
		expectCrossSpace404(t, s, stranger, http.MethodPatch, prefsURL(sid, a.ID), map[string]any{"hideLogo": true})
		// a wallet of another space, through my own space
		other := s.newWallet(stranger, spaceIDOf(stranger), wal("Theirs"))
		res := s.Do(http.MethodPatch, prefsURL(sid, other.ID), map[string]any{"hideLogo": true}, bearer(jane.AccessToken))
		require.Equal(t, http.StatusNotFound, res.Status)
		assert.Equal(t, "WALLET_NOT_FOUND", errCode(t, res))
		assert.Equal(t, http.StatusNotFound, s.Do(http.MethodPatch, prefsURL(sid, "00000000-0000-0000-0000-000000000000"), map[string]any{}, bearer(jane.AccessToken)).Status)
		assert.Equal(t, http.StatusNotFound, s.Do(http.MethodPatch, prefsURL(sid, "nope"), map[string]any{}, bearer(jane.AccessToken)).Status)
		assert.Equal(t, http.StatusBadRequest, s.Do(http.MethodPatch, prefsURL(sid, a.ID), map[string]any{"hideLogo": "yes"}, bearer(jane.AccessToken)).Status)
		assert.Equal(t, http.StatusBadRequest, s.Do(http.MethodPatch, prefsURL(sid, a.ID), map[string]any{"hideEverything": true}, bearer(jane.AccessToken)).Status)
		assert.Equal(t, http.StatusUnauthorized, s.Do(http.MethodPatch, prefsURL(sid, a.ID), map[string]any{}).Status)
		assert.Equal(t, 0, s.prefRows(other.ID), "nothing stored for it")
	})

	t.Run("deleting the wallet cascades", func(t *testing.T) {
		require.Equal(t, http.StatusOK, s.Do(http.MethodPatch, prefsURL(sid, b.ID), map[string]any{"hideLogo": true}, bearer(jane.AccessToken)).Status)
		require.Equal(t, 1, s.prefRows(b.ID))
		_, err := s.Tx.Exec(context.Background(), `DELETE FROM wallets WHERE id = $1`, b.ID)
		require.NoError(t, err)
		assert.Equal(t, 0, s.prefRows(b.ID))
	})
}
