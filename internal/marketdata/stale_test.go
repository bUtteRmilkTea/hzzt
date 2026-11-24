package marketdata

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestCryptoStaleAfterIsDerivedFromTheInterval(t *testing.T) {
	assert.Equal(t, 30*time.Minute, CryptoStaleAfter(15)) // two missed refreshes still fit the floor
	assert.Equal(t, 30*time.Minute, CryptoStaleAfter(10))
	assert.Equal(t, 120*time.Minute, CryptoStaleAfter(60)) // 2 x interval
	assert.Equal(t, 30*time.Minute, CryptoStaleAfter(0))
}

func TestDefaultRefreshIs15Minutes(t *testing.T) {
	assert.Equal(t, 15, DefaultRefreshMinutes)
}
