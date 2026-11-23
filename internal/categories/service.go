// Package categories implements per-space expense/income categories,
// default seeds, suggestions and optional monthly budgets.
package categories

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"

	"github.com/JonathanRainee/hzzt/internal/currency"
	"github.com/JonathanRainee/hzzt/internal/db"
	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
	"github.com/JonathanRainee/hzzt/internal/platform/apperror"
)

// Category types.
const (
	TypeExpense = "EXPENSE"
	TypeIncome  = "INCOME"
)

// Error codes owned by this package (see plan Appendix B).
const (
	CodeCategoryNameTaken  = "CATEGORY_NAME_TAKEN"
	CodeExistsSimilar      = "CATEGORY_EXISTS_SIMILAR"
	CodeBudgetNotAllowed   = "BUDGET_NOT_ALLOWED"
	CodeCategoryTypeLocked = "CATEGORY_TYPE_LOCKED"
	CodeCategoryNotFound   = "CATEGORY_NOT_FOUND"
)

var (
	errNameTaken   = apperror.Conflict(CodeCategoryNameTaken, "A category with this name already exists")
	errBudgetNotOK = apperror.Unprocessable(CodeBudgetNotAllowed, "Only expense categories can have a budget")
	errTypeLocked  = apperror.Unprocessable(CodeCategoryTypeLocked, "The type cannot change once transactions use this category")
	errCategoryNF  = apperror.New(404, CodeCategoryNotFound, "Category not found")
)

func errExistsSimilar(existing sqlc.Category) error {
	return apperror.Conflict(CodeExistsSimilar, "A category with the same meaning already exists").
		WithDetails(map[string]any{"existingId": existing.ID, "existingName": existing.Name})
}

// similar returns a live category of the same type whose name means the same as name (they are
// aliases of one default category, such as Gaji and Salary), other than `except`.
func similar(ctx context.Context, q *sqlc.Queries, spaceID uuid.UUID, typ, name string, except uuid.UUID) (*sqlc.Category, error) {
	group := aliasGroup(name)
	if group == "" {
		return nil, nil
	}
	rows, err := q.ListCategories(ctx, sqlc.ListCategoriesParams{SpaceID: spaceID, Type: &typ})
	if err != nil {
		return nil, err
	}
	for _, c := range rows {
		if c.ID == except {
			continue
		}
		if (c.SystemKey != nil && *c.SystemKey == group) || aliasGroup(c.Name) == group {
			return &c, nil
		}
	}
	return nil, nil
}

// DB is what the service needs; *pgxpool.Pool and pgx.Tx both satisfy it.
type DB interface {
	sqlc.DBTX
	db.TxStarter
}

// UsageChecker reports whether any transaction references the category.
// Transactions arrive in P7/P8, which install the real checker with SetUsageChecker.
type UsageChecker func(ctx context.Context, spaceID, categoryID uuid.UUID) (bool, error)

// Service holds category logic.
type Service struct {
	db       DB
	q        *sqlc.Queries
	usage    UsageChecker
	currency *currency.Service
	now      func() time.Time
}

// NewService builds the service; cur is used to convert usage stats to the base currency.
func NewService(database DB, cur *currency.Service) *Service {
	return &Service{
		db: database, q: sqlc.New(database), currency: cur, now: time.Now,
		usage: func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return false, nil },
	}
}

// SetClock overrides the clock used to resolve periods (tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// SetUsageChecker installs the "is this category used by transactions" check.
func (s *Service) SetUsageChecker(c UsageChecker) { s.usage = c }

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// List returns the space's live categories ordered by sort_order; typ "" means both types.
func (s *Service) List(ctx context.Context, spaceID uuid.UUID, typ string) ([]sqlc.Category, error) {
	var t *string
	if typ != "" {
		t = &typ
	}
	return s.q.ListCategories(ctx, sqlc.ListCategoriesParams{SpaceID: spaceID, Type: t})
}

// CreateInput describes a new category.
type CreateInput struct {
	Type, Name, Icon, Color string
	Budget                  *decimal.Decimal
	// Force skips the CATEGORY_EXISTS_SIMILAR check (an exact duplicate is still refused).
	Force bool
}

// Create appends a category at the end of its type's order.
func (s *Service) Create(ctx context.Context, spaceID, userID uuid.UUID, in CreateInput) (sqlc.Category, error) {
	if in.Budget != nil && in.Type != TypeExpense {
		return sqlc.Category{}, errBudgetNotOK
	}
	var out sqlc.Category
	err := db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if !in.Force {
			if c, err := similar(ctx, q, spaceID, in.Type, in.Name, uuid.Nil); err != nil {
				return err
			} else if c != nil && !strings.EqualFold(strings.TrimSpace(c.Name), strings.TrimSpace(in.Name)) {
				return errExistsSimilar(*c)
			}
		}
		next, err := q.NextCategorySortOrder(ctx, sqlc.NextCategorySortOrderParams{SpaceID: spaceID, Type: in.Type})
		if err != nil {
			return err
		}
		out, err = q.CreateCategory(ctx, createParams(spaceID, userID, in, next))
		return err
	})
	if isUniqueViolation(err) {
		return sqlc.Category{}, errNameTaken
	}
	return out, err
}

func createParams(spaceID, userID uuid.UUID, in CreateInput, order int32) sqlc.CreateCategoryParams {
	p := sqlc.CreateCategoryParams{
		SpaceID: spaceID, Type: in.Type, Name: in.Name, Icon: in.Icon, Color: in.Color,
		SortOrder: order, CreatedByID: userID,
	}
	if in.Budget != nil {
		p.BudgetAmount = decimal.NullDecimal{Decimal: *in.Budget, Valid: true}
	}
	return p
}

// Get returns one live category of the space.
func (s *Service) Get(ctx context.Context, spaceID, id uuid.UUID) (sqlc.Category, error) {
	c, err := s.q.GetCategory(ctx, sqlc.GetCategoryParams{ID: id, SpaceID: spaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return c, errCategoryNF
	}
	return c, err
}

// UpdateInput is a partial update; nil pointers mean "not sent".
type UpdateInput struct {
	Type, Name, Icon, Color *string
	BudgetSet               bool             // budgetAmount was present in the body
	Budget                  *decimal.Decimal // nil with BudgetSet clears the budget
	Force                   bool             // skip the CATEGORY_EXISTS_SIMILAR check
}

// Update applies a partial update.
func (s *Service) Update(ctx context.Context, spaceID, id uuid.UUID, in UpdateInput) (sqlc.Category, error) {
	var out sqlc.Category
	err := db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		cur, err := q.GetCategory(ctx, sqlc.GetCategoryParams{ID: id, SpaceID: spaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return errCategoryNF
		}
		if err != nil {
			return err
		}
		finalType := cur.Type
		if in.Type != nil && *in.Type != cur.Type {
			used, err := s.usage(ctx, spaceID, id)
			if err != nil {
				return err
			}
			if used {
				return errTypeLocked
			}
			finalType = *in.Type
		}
		if !in.Force && (in.Name != nil || finalType != cur.Type) {
			name := cur.Name
			if in.Name != nil {
				name = *in.Name
			}
			if c, err := similar(ctx, q, spaceID, finalType, name, id); err != nil {
				return err
			} else if c != nil && !strings.EqualFold(strings.TrimSpace(c.Name), strings.TrimSpace(name)) {
				return errExistsSimilar(*c)
			}
		}
		p := sqlc.UpdateCategoryParams{
			ID: id, SpaceID: spaceID, Type: in.Type, Name: in.Name, Icon: in.Icon, Color: in.Color,
			SetBudget: in.BudgetSet,
		}
		switch {
		case in.BudgetSet && in.Budget != nil:
			if finalType != TypeExpense {
				return errBudgetNotOK
			}
			p.BudgetAmount = decimal.NullDecimal{Decimal: *in.Budget, Valid: true}
		case !in.BudgetSet && finalType != TypeExpense && cur.BudgetAmount.Valid:
			p.SetBudget = true // turning into an income category drops its budget
		}
		out, err = q.UpdateCategory(ctx, p)
		return err
	})
	if isUniqueViolation(err) {
		return sqlc.Category{}, errNameTaken
	}
	return out, err
}

// Delete soft-deletes a category; transactions keep their link for history.
func (s *Service) Delete(ctx context.Context, spaceID, id uuid.UUID) error {
	n, err := s.q.SoftDeleteCategory(ctx, sqlc.SoftDeleteCategoryParams{ID: id, SpaceID: spaceID})
	if err != nil {
		return err
	}
	if n == 0 {
		return errCategoryNF
	}
	return nil
}

// Reorder sets the order of one type; ids must list every live category of the type exactly once.
func (s *Service) Reorder(ctx context.Context, spaceID uuid.UUID, typ string, ids []uuid.UUID) ([]sqlc.Category, error) {
	err := db.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		have, err := q.ListCategoryIDsByType(ctx, sqlc.ListCategoryIDsByTypeParams{SpaceID: spaceID, Type: typ})
		if err != nil {
			return err
		}
		want := slices.Clone(ids)
		slices.SortFunc(want, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
		sorted := slices.Clone(have)
		slices.SortFunc(sorted, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
		if !slices.Equal(want, sorted) {
			return apperror.Validation("Validation failed", map[string]any{
				"ids": fmt.Sprintf("must list every %s category of this space exactly once", typ),
			})
		}
		for i, id := range ids {
			if err := q.SetCategorySortOrder(ctx, sqlc.SetCategorySortOrderParams{ID: id, SpaceID: spaceID, SortOrder: int32(i)}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.List(ctx, spaceID, typ)
}
