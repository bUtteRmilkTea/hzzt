package categories

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/JonathanRainee/hzzt/internal/db"
	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
	"github.com/JonathanRainee/hzzt/internal/platform/events"
	"github.com/JonathanRainee/hzzt/internal/spaces"
)

type seed struct{ typ, name, icon, color, key string }

// DefaultSeeds are created in every new space (7 expense, 4 income).
var DefaultSeeds = []seed{
	{TypeExpense, "Transport", "🚗", "#3B82F6", "TRANSPORT"},
	{TypeExpense, "Shopping", "🛍️", "#EC4899", "SHOPPING"},
	{TypeExpense, "Subscriptions", "🔁", "#8B5CF6", "SUBSCRIPTIONS"},
	{TypeExpense, "Health", "💊", "#EF4444", "HEALTH"},
	{TypeExpense, "Gifts", "🎁", "#F59E0B", "GIFTS"},
	{TypeExpense, "Snacks", "🍿", "#F97316", "SNACKS"},
	{TypeExpense, "Family", "👨‍👩‍👧", "#14B8A6", "FAMILY"},
	{TypeIncome, "Salary", "💰", "#22C55E", "SALARY"},
	{TypeIncome, "Freelance", "💼", "#0EA5E9", "FREELANCE"},
	{TypeIncome, "Investment", "📈", "#6366F1", "INVESTMENT"},
	{TypeIncome, "Dividend", "🏦", "#84CC16", "DIVIDEND"},
}

// Suggestions are the static name suggestions offered when creating a category.
var Suggestions = map[string][]string{
	TypeIncome:  {"Salary", "Bonus", "Freelance", "Investment", "Gift", "Refund"},
	TypeExpense: {"Food", "Coffee", "Transport", "Groceries", "Bills", "Entertainment"},
}

// Subscribe seeds default categories whenever a space is created.
func (s *Service) Subscribe(bus *events.Bus) {
	events.On(bus, func(ctx context.Context, e spaces.SpaceCreated) error {
		return db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
			q := s.q.WithTx(tx)
			order := map[string]int32{}
			for _, sd := range DefaultSeeds {
				sd := sd
				if _, err := q.CreateCategory(ctx, sqlc.CreateCategoryParams{
					SpaceID: e.SpaceID, Type: sd.typ, Name: sd.name, Icon: sd.icon, Color: sd.color,
					SortOrder: order[sd.typ], CreatedByID: e.UserID, SystemKey: &sd.key,
				}); err != nil {
					return err
				}
				order[sd.typ]++
			}
			return nil
		})
	})
}
