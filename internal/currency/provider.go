package currency

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/JonathanRainee/hzzt/internal/platform/money"
)

// RateSet is one provider snapshot: units of each quote currency per 1 USD.
type RateSet struct {
	AsOf   time.Time // date (UTC) the rates are for
	Source string
	Rates  map[string]decimal.Decimal
}

// FxProvider fetches the latest USD-based rates for the supported currencies.
type FxProvider interface {
	Latest(ctx context.Context) (RateSet, error)
}

// httpClient wraps net/http with a timeout and retries on network errors and 5xx/429.
type httpClient struct {
	c       *http.Client
	retries int
	backoff time.Duration
}

func newHTTPClient() *httpClient {
	return &httpClient{c: &http.Client{Timeout: 10 * time.Second}, retries: 2, backoff: 500 * time.Millisecond}
}

func (h *httpClient) get(ctx context.Context, u string, headers map[string]string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= h.retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(h.backoff * time.Duration(attempt)):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		res, err := h.c.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, rerr := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		_ = res.Body.Close()
		if rerr != nil {
			lastErr = rerr
			continue
		}
		if res.StatusCode >= 500 || res.StatusCode == http.StatusTooManyRequests {
			lastErr = fmt.Errorf("provider returned HTTP %d", res.StatusCode)
			continue
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("provider returned HTTP %d", res.StatusCode) // not retryable
		}
		return body, nil
	}
	return nil, lastErr
}

// quotes are the supported currencies except USD itself.
func quotes() []string {
	out := make([]string, 0, len(money.SupportedCurrencies))
	for _, c := range money.SupportedCurrencies {
		if c != "USD" {
			out = append(out, c)
		}
	}
	return out
}

// parseRates converts a JSON object of numbers into decimals without going through float64.
func parseRates(raw map[string]json.RawMessage) (map[string]decimal.Decimal, error) {
	out := make(map[string]decimal.Decimal, len(raw))
	for code, v := range raw {
		d, err := decimal.NewFromString(strings.TrimSpace(string(v)))
		if err != nil {
			return nil, fmt.Errorf("rate for %s: %w", code, err)
		}
		if !d.IsPositive() {
			return nil, fmt.Errorf("rate for %s is not positive", code)
		}
		out[code] = d
	}
	return out, nil
}

// --- Frankfurter ---------------------------------------------------------

// DefaultFrankfurterURL is the current Frankfurter API (v1) base URL.
const DefaultFrankfurterURL = "https://api.frankfurter.dev/v1"

// FrankfurterProvider reads ECB reference rates from frankfurter.dev (no key needed).
type FrankfurterProvider struct {
	BaseURL string
	http    *httpClient
}

// NewFrankfurterProvider builds the provider; baseURL "" uses the default.
func NewFrankfurterProvider(baseURL string) *FrankfurterProvider {
	if baseURL == "" {
		baseURL = DefaultFrankfurterURL
	}
	return &FrankfurterProvider{BaseURL: strings.TrimRight(baseURL, "/"), http: newHTTPClient()}
}

// Latest implements FxProvider.
func (p *FrankfurterProvider) Latest(ctx context.Context) (RateSet, error) {
	u := p.BaseURL + "/latest?base=USD&symbols=" + url.QueryEscape(strings.Join(quotes(), ","))
	body, err := p.http.get(ctx, u, nil)
	if err != nil {
		return RateSet{}, fmt.Errorf("frankfurter: %w", err)
	}
	var resp struct {
		Date  string                     `json:"date"`
		Rates map[string]json.RawMessage `json:"rates"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return RateSet{}, fmt.Errorf("frankfurter: decode: %w", err)
	}
	asOf, err := time.Parse("2006-01-02", resp.Date)
	if err != nil {
		return RateSet{}, fmt.Errorf("frankfurter: bad date %q: %w", resp.Date, err)
	}
	rates, err := parseRates(resp.Rates)
	if err != nil {
		return RateSet{}, fmt.Errorf("frankfurter: %w", err)
	}
	return RateSet{AsOf: asOf, Source: "frankfurter", Rates: rates}, nil
}

// --- freecurrencyapi.com -----------------------------------------------

// DefaultFreeCurrencyURL is the freecurrencyapi.com v1 base URL.
const DefaultFreeCurrencyURL = "https://api.freecurrencyapi.com/v1"

// FreeCurrencyProvider reads rates from freecurrencyapi.com (API key in the `apikey` header).
type FreeCurrencyProvider struct {
	BaseURL string
	APIKey  string
	http    *httpClient
	now     func() time.Time
}

// NewFreeCurrencyProvider builds the provider; baseURL "" uses the default.
func NewFreeCurrencyProvider(baseURL, apiKey string) *FreeCurrencyProvider {
	if baseURL == "" {
		baseURL = DefaultFreeCurrencyURL
	}
	return &FreeCurrencyProvider{BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey, http: newHTTPClient(), now: time.Now}
}

// Latest implements FxProvider. The API has no date field, so as_of is today (UTC).
func (p *FreeCurrencyProvider) Latest(ctx context.Context) (RateSet, error) {
	u := p.BaseURL + "/latest?base_currency=USD&currencies=" + url.QueryEscape(strings.Join(quotes(), ","))
	body, err := p.http.get(ctx, u, map[string]string{"apikey": p.APIKey})
	if err != nil {
		return RateSet{}, fmt.Errorf("freecurrencyapi: %w", err)
	}
	var resp struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return RateSet{}, fmt.Errorf("freecurrencyapi: decode: %w", err)
	}
	rates, err := parseRates(resp.Data)
	if err != nil {
		return RateSet{}, fmt.Errorf("freecurrencyapi: %w", err)
	}
	n := p.now().UTC()
	return RateSet{AsOf: time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC), Source: "freecurrencyapi", Rates: rates}, nil
}

// Quota is what is left of a provider's request allowance.
type Quota struct {
	Remaining int
	Total     int
}

// QuotaReporter is implemented by providers that can say how much of their allowance is left.
// Refresh logs it after each run (best effort, never an error).
type QuotaReporter interface {
	Quota(ctx context.Context) (Quota, error)
}

// Quota implements QuotaReporter through the /status endpoint, which does not use up the allowance.
func (p *FreeCurrencyProvider) Quota(ctx context.Context) (Quota, error) {
	body, err := p.http.get(ctx, p.BaseURL+"/status", map[string]string{"apikey": p.APIKey})
	if err != nil {
		return Quota{}, fmt.Errorf("freecurrencyapi: status: %w", err)
	}
	var resp struct {
		Quotas struct {
			Month struct {
				Total     int `json:"total"`
				Remaining int `json:"remaining"`
			} `json:"month"`
		} `json:"quotas"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return Quota{}, fmt.Errorf("freecurrencyapi: decode status: %w", err)
	}
	return Quota{Remaining: resp.Quotas.Month.Remaining, Total: resp.Quotas.Month.Total}, nil
}
