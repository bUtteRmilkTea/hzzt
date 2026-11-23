package currency

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JonathanRainee/hzzt/internal/platform/money"
)

func fastHTTP(c *httpClient) { c.backoff = time.Millisecond }

func TestCatalog(t *testing.T) {
	cat := Catalog()
	require.Len(t, cat, len(money.SupportedCurrencies))
	seen := map[string]bool{}
	for _, c := range cat {
		assert.False(t, seen[c.Code], "duplicate %s", c.Code)
		seen[c.Code] = true
		assert.NotEqual(t, c.Code, c.Name, "%s needs a real name", c.Code)
		assert.NotEmpty(t, c.Symbol)
	}
	assert.True(t, seen["USD"] && seen["IDR"] && seen["EUR"])
	for _, c := range cat {
		if c.Code == "IDR" || c.Code == "JPY" {
			assert.Equal(t, int32(0), c.MinorUnits)
		}
		if c.Code == "USD" {
			assert.Equal(t, int32(2), c.MinorUnits)
		}
	}
}

func TestFrankfurterProvider(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		assert.Equal(t, "/latest", r.URL.Path)
		_, _ = w.Write([]byte(`{"amount":1.0,"base":"USD","date":"2026-11-17","rates":{"IDR":16234.5678,"EUR":0.86}}`))
	}))
	defer srv.Close()

	p := NewFrankfurterProvider(srv.URL)
	set, err := p.Latest(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "frankfurter", set.Source)
	assert.Equal(t, "2026-11-17", set.AsOf.Format("2006-01-02"))
	assert.Equal(t, "16234.5678", set.Rates["IDR"].String(), "no float rounding")
	assert.Contains(t, gotQuery, "base=USD")
	assert.NotContains(t, gotQuery, "USD%2C", "USD is not requested as a quote")
	assert.Contains(t, gotQuery, "IDR")
}

func TestFreeCurrencyProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "secret-key", r.Header.Get("apikey"), "key travels in the apikey header")
		assert.Empty(t, r.URL.Query().Get("apikey"), "never in the URL")
		assert.Equal(t, "USD", r.URL.Query().Get("base_currency"))
		_, _ = w.Write([]byte(`{"data":{"EUR":0.9096371027,"IDR":18265.702226476}}`))
	}))
	defer srv.Close()

	p := NewFreeCurrencyProvider(srv.URL, "secret-key")
	p.now = func() time.Time { return time.Date(2026, 11, 18, 23, 59, 0, 0, time.UTC) }
	set, err := p.Latest(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "freecurrencyapi", set.Source)
	assert.Equal(t, "2026-11-18", set.AsOf.Format("2006-01-02"))
	assert.Equal(t, "18265.702226476", set.Rates["IDR"].String())
}

func TestProviderRetriesServerErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"EUR":0.9}}`))
	}))
	defer srv.Close()

	p := NewFreeCurrencyProvider(srv.URL, "k")
	fastHTTP(p.http)
	_, err := p.Latest(context.Background())
	require.NoError(t, err)
	assert.EqualValues(t, 3, calls.Load(), "1 try + 2 retries")
}

func TestProviderGivesUpAfterTwoRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	p := NewFrankfurterProvider(srv.URL)
	fastHTTP(p.http)
	_, err := p.Latest(context.Background())
	require.Error(t, err)
	assert.EqualValues(t, 3, calls.Load())
}

func TestProviderDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	p := NewFreeCurrencyProvider(srv.URL, "bad")
	fastHTTP(p.http)
	_, err := p.Latest(context.Background())
	require.Error(t, err)
	assert.EqualValues(t, 1, calls.Load())
	assert.NotContains(t, err.Error(), "bad", "the key must not leak into errors")
}

func TestProviderRejectsBadPayloads(t *testing.T) {
	for name, body := range map[string]string{
		"not json":      `<html>`,
		"zero rate":     `{"data":{"EUR":0}}`,
		"string rate":   `{"data":{"EUR":"abc"}}`,
		"negative rate": `{"data":{"EUR":-1}}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer srv.Close()
			_, err := NewFreeCurrencyProvider(srv.URL, "k").Latest(context.Background())
			assert.Error(t, err)
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"date":"yesterday","rates":{"EUR":0.9}}`))
	}))
	defer srv.Close()
	_, err := NewFrankfurterProvider(srv.URL).Latest(context.Background())
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "bad date"))
}

func TestProviderHonoursContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := NewFrankfurterProvider(srv.URL).Latest(ctx)
	assert.Error(t, err)
}

func TestFreeCurrencyQuota(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/status", r.URL.Path)
		assert.NotEmpty(t, r.Header.Get("apikey"))
		_, _ = w.Write([]byte(`{"quotas":{"month":{"total":5000,"used":120,"remaining":4880},"grace":{"total":0,"used":0,"remaining":0}}}`))
	}))
	defer srv.Close()
	var qr QuotaReporter = NewFreeCurrencyProvider(srv.URL, "test-key")
	q, err := qr.Quota(context.Background())
	require.NoError(t, err)
	assert.Equal(t, Quota{Remaining: 4880, Total: 5000}, q)
}
