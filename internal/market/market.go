// Package market is the external market-data layer: it fetches holding prices and
// EUR FX rates over HTTP behind provider interfaces, keeping network access out of
// model/view/controller. Pure Go (stdlib only), so the static no-cgo build holds.
package market

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Quote is a single instrument price (in the instrument's own currency).
type Quote struct {
	Symbol string
	Price  float64
}

// PriceProvider fetches the latest price for each symbol. Missing/failed symbols
// are simply absent from the result (callers preserve existing prices).
type PriceProvider interface {
	Quotes(ctx context.Context, symbols []string) (map[string]Quote, error)
}

// FXProvider returns the multiplier to convert 1 unit of each currency to EUR.
type FXProvider interface {
	RatesToEUR(ctx context.Context, currencies []string) (map[string]float64, error)
}

// ---------- Twelve Data (prices + forex) ----------

const tdBase = "https://api.twelvedata.com"

// TwelveData implements PriceProvider and FXProvider against api.twelvedata.com.
type TwelveData struct {
	APIKey  string
	BaseURL string       // defaults to tdBase; overridden in tests
	Client  *http.Client // defaults to a 10s-timeout client
}

func (t TwelveData) base() string {
	if t.BaseURL != "" {
		return t.BaseURL
	}
	return tdBase
}

func (t TwelveData) client() *http.Client {
	if t.Client != nil {
		return t.Client
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (t TwelveData) get(ctx context.Context, path string, q url.Values) ([]byte, error) {
	q.Set("apikey", t.APIKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.base()+path+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := t.client().Do(req)
	if err != nil {
		// Transport errors embed the request URL, which carries the apikey query
		// param — strip it so the secret can't leak into logs.
		var ue *url.Error
		if errors.As(err, &ue) {
			ue.URL = t.base() + path
		}
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

func (t TwelveData) Quotes(ctx context.Context, symbols []string) (map[string]Quote, error) {
	if len(symbols) == 0 {
		return map[string]Quote{}, nil
	}
	q := url.Values{"symbol": {strings.Join(symbols, ",")}}
	data, err := t.get(ctx, "/price", q)
	if err != nil {
		return nil, err
	}
	return parsePrices(data, symbols)
}

func (t TwelveData) RatesToEUR(ctx context.Context, currencies []string) (map[string]float64, error) {
	out := map[string]float64{"EUR": 1}
	var pairs []string
	for _, c := range currencies {
		c = strings.ToUpper(strings.TrimSpace(c))
		if c == "" || c == "EUR" {
			continue
		}
		pairs = append(pairs, c+"/EUR")
	}
	if len(pairs) == 0 {
		return out, nil
	}
	q := url.Values{"symbol": {strings.Join(pairs, ",")}}
	data, err := t.get(ctx, "/exchange_rate", q)
	if err != nil {
		return nil, err
	}
	rates, err := parseRates(data)
	if err != nil {
		return nil, err
	}
	for c, r := range rates {
		out[c] = r
	}
	return out, nil
}

// errEnvelope detects Twelve Data's error response, e.g. bad key / rate limit.
func errEnvelope(data []byte) error {
	var env struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		Code    int    `json:"code"`
	}
	if json.Unmarshal(data, &env) == nil && env.Status == "error" {
		return fmt.Errorf("twelvedata: %s (code %d)", env.Message, env.Code)
	}
	return nil
}

// parsePrices handles both the single-symbol shape ({"price":"..."}) and the
// multi-symbol map ({"AAPL":{"price":"..."},...}).
func parsePrices(data []byte, symbols []string) (map[string]Quote, error) {
	if err := errEnvelope(data); err != nil {
		return nil, err
	}
	out := map[string]Quote{}
	var single struct {
		Price string `json:"price"`
	}
	if json.Unmarshal(data, &single) == nil && single.Price != "" {
		if p, err := strconv.ParseFloat(single.Price, 64); err == nil && len(symbols) >= 1 {
			out[symbols[0]] = Quote{Symbol: symbols[0], Price: p}
		}
		return out, nil
	}
	var multi map[string]struct {
		Price  string `json:"price"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &multi); err != nil {
		return nil, fmt.Errorf("twelvedata price: %w", err)
	}
	for sym, v := range multi {
		if v.Status == "error" || v.Price == "" {
			continue
		}
		if p, err := strconv.ParseFloat(v.Price, 64); err == nil {
			out[sym] = Quote{Symbol: sym, Price: p}
		}
	}
	return out, nil
}

// parseRates handles single ({"symbol":"USD/EUR","rate":0.85}) and multi
// ({"USD/EUR":{"rate":0.85},...}) shapes, keying by the FROM currency.
func parseRates(data []byte) (map[string]float64, error) {
	if err := errEnvelope(data); err != nil {
		return nil, err
	}
	out := map[string]float64{}
	var single struct {
		Symbol string  `json:"symbol"`
		Rate   float64 `json:"rate"`
	}
	if json.Unmarshal(data, &single) == nil && single.Symbol != "" && single.Rate != 0 {
		out[fromCurrency(single.Symbol)] = single.Rate
		return out, nil
	}
	var multi map[string]struct {
		Rate   float64 `json:"rate"`
		Status string  `json:"status"`
	}
	if err := json.Unmarshal(data, &multi); err != nil {
		return nil, fmt.Errorf("twelvedata fx: %w", err)
	}
	for pair, v := range multi {
		if v.Status == "error" || v.Rate == 0 {
			continue
		}
		out[fromCurrency(pair)] = v.Rate
	}
	return out, nil
}

// fromCurrency turns "USD/EUR" into "USD".
func fromCurrency(pair string) string {
	if i := strings.IndexByte(pair, '/'); i > 0 {
		return pair[:i]
	}
	return pair
}

// ---------- Orchestration (small local store interfaces; no model import) ----------

type PriceStore interface {
	DistinctHoldingSymbols() ([]string, error)
	UpdateHoldingPrice(symbol string, price float64) error
}

type FXStore interface {
	DistinctCurrencies() ([]string, error)
	UpsertFXRate(currency string, rate float64) error
}

// UpdatePrices fetches quotes for all held symbols and writes the ones that came
// back; unknown/failed symbols keep their existing price. Returns rows updated.
func UpdatePrices(ctx context.Context, p PriceProvider, s PriceStore) (int, error) {
	syms, err := s.DistinctHoldingSymbols()
	if err != nil || len(syms) == 0 {
		return 0, err
	}
	quotes, err := p.Quotes(ctx, syms)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, q := range quotes {
		if q.Price > 0 {
			if err := s.UpdateHoldingPrice(q.Symbol, q.Price); err == nil {
				n++
			}
		}
	}
	return n, nil
}

// UpdateFX fetches EUR rates for all currencies in use and upserts them.
func UpdateFX(ctx context.Context, p FXProvider, s FXStore) (int, error) {
	ccys, err := s.DistinctCurrencies()
	if err != nil {
		return 0, err
	}
	rates, err := p.RatesToEUR(ctx, ccys)
	if err != nil {
		return 0, err
	}
	n := 0
	for c, r := range rates {
		if c == "EUR" || r <= 0 {
			continue
		}
		if err := s.UpsertFXRate(c, r); err == nil {
			n++
		}
	}
	return n, nil
}

// Refresh updates EUR rates and then holding prices. A failure in one doesn't
// skip the other — prices for EUR (or already-rated) holdings are still worth
// fetching when, say, one currency code is unknown to the provider — and both
// errors are returned, joined.
func Refresh(ctx context.Context, fx FXProvider, fs FXStore, pp PriceProvider, ps PriceStore) error {
	_, fxErr := UpdateFX(ctx, fx, fs)
	if fxErr != nil {
		fxErr = fmt.Errorf("fx: %w", fxErr)
	}
	_, priceErr := UpdatePrices(ctx, pp, ps)
	if priceErr != nil {
		priceErr = fmt.Errorf("prices: %w", priceErr)
	}
	return errors.Join(fxErr, priceErr)
}
