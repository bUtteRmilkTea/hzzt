package investments

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The expected yearly change and the staked quantity were removed. The only places that may still name them are
// the tolerant, ignored request fields kept for one release. Delete this test together with those fields.
func TestRemovedFieldsDoNotComeBack(t *testing.T) {
	allowed := map[string]bool{
		"internal/investments/inputs.go":              true, // ignored request fields
		"internal/manualassets/handlers.go":           true, // ignored request fields
		"internal/investments/inputs_test.go":         true,
		"internal/investments/removed_fields_test.go": true,
	}
	forbidden := []string{"estimated_annual", "EstimatedChange", "stakedOf", "StakedQuantity", "stakedQuantity", "EstimatedAnnualChangePct", "estimatedAnnualChangePct"}
	root := filepath.Join("..", "..")
	var hits []string
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() && rel == "internal/db/migrations" {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || allowed[rel] {
			return nil
		}
		b, err := os.ReadFile(path)
		require.NoError(t, err)
		for _, w := range forbidden {
			if strings.Contains(string(b), w) {
				hits = append(hits, rel+": "+w)
			}
		}
		return nil
	})
	require.NoError(t, err)
	assert.Empty(t, hits, "removed fields must not come back")
}
