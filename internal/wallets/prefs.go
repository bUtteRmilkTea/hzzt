package wallets

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JonathanRainee/hzzt/internal/db"
	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
)

// DisplayPrefs are one user's display choices for a wallet card. They are private to the
// user and synced across their devices; the zero value (nothing hidden) is the default.
type DisplayPrefs struct {
	HideLogo bool `json:"hideLogo"`
	HideName bool `json:"hideName"`
	HideType bool `json:"hideType"`
}

func prefsOf(p sqlc.UserWalletPref) DisplayPrefs {
	return DisplayPrefs{HideLogo: p.HideLogo, HideName: p.HideName, HideType: p.HideType}
}

// PrefsPatch changes some of the display preferences; nil leaves one as it is.
type PrefsPatch struct {
	HideLogo, HideName, HideType *bool
}

func (p PrefsPatch) apply(cur DisplayPrefs) DisplayPrefs {
	if p.HideLogo != nil {
		cur.HideLogo = *p.HideLogo
	}
	if p.HideName != nil {
		cur.HideName = *p.HideName
	}
	if p.HideType != nil {
		cur.HideType = *p.HideType
	}
	return cur
}

// Prefs returns the user's display preferences by wallet for the live wallets of a space
// (wallets without a row are absent: they use the defaults).
func (s *Service) Prefs(ctx context.Context, userID, spaceID uuid.UUID) (map[uuid.UUID]DisplayPrefs, error) {
	rows, err := s.q.ListWalletPrefsInSpace(ctx, sqlc.ListWalletPrefsInSpaceParams{UserID: userID, SpaceID: spaceID})
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]DisplayPrefs, len(rows))
	for _, r := range rows {
		out[r.WalletID] = prefsOf(r)
	}
	return out, nil
}

// PrefsOf returns the user's display preferences of one wallet (defaults when none are stored).
func (s *Service) PrefsOf(ctx context.Context, userID, walletID uuid.UUID) (DisplayPrefs, error) {
	p, err := s.q.GetWalletPref(ctx, sqlc.GetWalletPrefParams{UserID: userID, WalletID: walletID})
	if errors.Is(err, pgx.ErrNoRows) {
		return DisplayPrefs{}, nil
	}
	if err != nil {
		return DisplayPrefs{}, err
	}
	return prefsOf(p), nil
}

// UpdatePrefs merges a patch into the user's display preferences of a wallet of the space and
// returns the result. Preferences that end up all false are not stored (they are the default).
func (s *Service) UpdatePrefs(ctx context.Context, spaceID, userID, walletID uuid.UUID, patch PrefsPatch) (DisplayPrefs, error) {
	var out DisplayPrefs
	err := db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if _, err := q.GetWallet(ctx, sqlc.GetWalletParams{ID: walletID, SpaceID: spaceID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errWalletNF
			}
			return err
		}
		var cur DisplayPrefs
		if p, err := q.GetWalletPref(ctx, sqlc.GetWalletPrefParams{UserID: userID, WalletID: walletID}); err == nil {
			cur = prefsOf(p)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		out = patch.apply(cur)
		if out == (DisplayPrefs{}) {
			return q.DeleteWalletPref(ctx, sqlc.DeleteWalletPrefParams{UserID: userID, WalletID: walletID})
		}
		_, err := q.UpsertWalletPref(ctx, sqlc.UpsertWalletPrefParams{
			UserID: userID, WalletID: walletID, HideLogo: out.HideLogo, HideName: out.HideName, HideType: out.HideType,
		})
		return err
	})
	return out, err
}
