package market

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParsePrices(t *testing.T) {
	// multi-symbol
	multi := []byte(`{"AAPL":{"price":"311.78"},"MSFT":{"price":"402.10"},"BAD":{"status":"error","message":"not found"}}`)
	got, err := parsePrices(multi, []string{"AAPL", "MSFT", "BAD"})
	if err != nil {
		t.Fatal(err)
	}
	if got["AAPL"].Price != 311.78 || got["MSFT"].Price != 402.10 {
		t.Errorf("multi prices = %+v", got)
	}
	if _, ok := got["BAD"]; ok {
		t.Error("errored symbol should be skipped")
	}

	// single-symbol shape uses the requested symbol
	single := []byte(`{"price":"99.5"}`)
	got, _ = parsePrices(single, []string{"VWCE"})
	if got["VWCE"].Price != 99.5 {
		t.Errorf("single price = %+v", got)
	}

	// error envelope (bad key / rate limit)
	if _, err := parsePrices([]byte(`{"code":429,"message":"limit","status":"error"}`), []string{"AAPL"}); err == nil {
		t.Error("expected error for error envelope")
	}
}

func TestParseRates(t *testing.T) {
	multi := []byte(`{"USD/EUR":{"rate":0.8588},"GBP/EUR":{"rate":1.17}}`)
	got, err := parseRates(multi)
	if err != nil {
		t.Fatal(err)
	}
	if got["USD"] != 0.8588 || got["GBP"] != 1.17 {
		t.Errorf("rates = %+v", got)
	}
	single := []byte(`{"symbol":"USD/EUR","rate":0.86,"timestamp":1}`)
	if got, _ = parseRates(single); got["USD"] != 0.86 {
		t.Errorf("single rate = %+v", got)
	}
	if _, err := parseRates([]byte(`{"status":"error","message":"bad","code":401}`)); err == nil {
		t.Error("expected error envelope")
	}
}

func TestTwelveDataHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("apikey") != "k" {
			t.Errorf("apikey not forwarded: %s", r.URL.RawQuery)
		}
		switch r.URL.Path {
		case "/price":
			w.Write([]byte(`{"AAPL":{"price":"311.78"}}`))
		case "/exchange_rate":
			w.Write([]byte(`{"USD/EUR":{"rate":0.86}}`))
		}
	}))
	defer srv.Close()
	td := TwelveData{APIKey: "k", BaseURL: srv.URL, Client: srv.Client()}

	q, err := td.Quotes(context.Background(), []string{"AAPL"})
	if err != nil || q["AAPL"].Price != 311.78 {
		t.Fatalf("Quotes = %+v err=%v", q, err)
	}
	rates, err := td.RatesToEUR(context.Background(), []string{"USD", "EUR"})
	if err != nil || rates["USD"] != 0.86 || rates["EUR"] != 1 {
		t.Fatalf("RatesToEUR = %+v err=%v", rates, err)
	}
}

// --- fakes for orchestration ---

type fakePriceStore struct {
	syms    []string
	updated map[string]float64
}

func (f *fakePriceStore) DistinctHoldingSymbols() ([]string, error) { return f.syms, nil }
func (f *fakePriceStore) UpdateHoldingPrice(sym string, price float64) error {
	f.updated[strings.ToUpper(sym)] = price
	return nil
}

type fakeProvider struct{ quotes map[string]Quote }

func (f fakeProvider) Quotes(_ context.Context, _ []string) (map[string]Quote, error) {
	return f.quotes, nil
}

func TestUpdatePrices(t *testing.T) {
	ps := &fakePriceStore{syms: []string{"AAPL", "VWCE"}, updated: map[string]float64{}}
	p := fakeProvider{quotes: map[string]Quote{"AAPL": {Symbol: "AAPL", Price: 311.78}}} // VWCE missing
	n, err := UpdatePrices(context.Background(), p, ps)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if ps.updated["AAPL"] != 311.78 {
		t.Errorf("AAPL not updated: %+v", ps.updated)
	}
	if _, ok := ps.updated["VWCE"]; ok {
		t.Error("missing symbol should not be updated (price preserved)")
	}
}

type fakeFX struct{ err error }

func (f fakeFX) RatesToEUR(ctx context.Context, ccys []string) (map[string]float64, error) {
	if f.err != nil {
		return nil, f.err
	}
	return map[string]float64{"USD": 0.9}, nil
}

type fakePrices struct{}

func (fakePrices) Quotes(ctx context.Context, syms []string) (map[string]Quote, error) {
	out := map[string]Quote{}
	for _, s := range syms {
		out[s] = Quote{Symbol: s, Price: 42}
	}
	return out, nil
}

type memStore struct {
	prices map[string]float64
	rates  map[string]float64
}

func (m *memStore) DistinctHoldingSymbols() ([]string, error) { return []string{"AAPL"}, nil }
func (m *memStore) UpdateHoldingPrice(sym string, p float64) error {
	m.prices[sym] = p
	return nil
}
func (m *memStore) DistinctCurrencies() ([]string, error) { return []string{"US$"}, nil }
func (m *memStore) UpsertFXRate(c string, r float64) error {
	m.rates[c] = r
	return nil
}

// An FX failure (e.g. a currency code the provider rejects) must not stop the
// price update; the error is still reported.
func TestRefreshUpdatesPricesWhenFXFails(t *testing.T) {
	s := &memStore{prices: map[string]float64{}, rates: map[string]float64{}}
	err := Refresh(context.Background(), fakeFX{err: errors.New("unknown symbol US$/EUR")}, s, fakePrices{}, s)
	if s.prices["AAPL"] != 42 {
		t.Errorf("prices not updated after an FX failure: %v", s.prices)
	}
	if err == nil || !strings.Contains(err.Error(), "fx:") {
		t.Errorf("err = %v, want the FX failure reported", err)
	}
	if err := Refresh(context.Background(), fakeFX{}, s, fakePrices{}, s); err != nil || s.rates["USD"] != 0.9 {
		t.Errorf("clean refresh: err %v rates %v", err, s.rates)
	}
}
