package categories

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSeedIconsAndKeysAreUnique(t *testing.T) {
	icons, keys, names := map[string]string{}, map[string]bool{}, map[string]bool{}
	for _, sd := range DefaultSeeds {
		if other, dup := icons[sd.icon]; dup {
			t.Errorf("%s and %s share the icon %s", sd.name, other, sd.icon)
		}
		icons[sd.icon] = sd.name
		assert.NotEmpty(t, sd.key, sd.name)
		assert.False(t, keys[sd.key], "duplicate key %s", sd.key)
		keys[sd.key] = true
		names[sd.name] = true
		assert.NotEqual(t, "✨", sd.icon, "%s must not use the client's fallback icon", sd.name)
		assert.Equal(t, sd.key, aliasGroup(sd.name), "%s must be in its own alias group", sd.name)
	}
	assert.Len(t, DefaultSeeds, 11)
}

func TestAliasGroup(t *testing.T) {
	assert.Equal(t, "SALARY", aliasGroup("  Gaji "))
	assert.Equal(t, "SALARY", aliasGroup("Pendapatan"))
	assert.Equal(t, "", aliasGroup("Bonus"))
}
