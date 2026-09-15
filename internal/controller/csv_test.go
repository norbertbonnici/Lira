package controller

import "testing"

func TestParseCSVDate(t *testing.T) {
	cases := []struct {
		val, layout, want string
		ok                bool
	}{
		{"2026-01-31", "2006-01-02", "2026-01-31", true},
		{"31/01/2026", "02/01/2006", "2026-01-31", true},
		{"01/31/2026", "01/02/2006", "2026-01-31", true},
		{"31.01.2026", "02.01.2006", "2026-01-31", true},
		{"not a date", "2006-01-02", "", false},
		{"", "2006-01-02", "", false},
	}
	for _, c := range cases {
		got, ok := parseCSVDate(c.val, c.layout)
		if ok != c.ok || got != c.want {
			t.Errorf("parseCSVDate(%q,%q) = %q,%v want %q,%v", c.val, c.layout, got, ok, c.want, c.ok)
		}
	}
}

func TestParseCSVAmount(t *testing.T) {
	cases := []struct {
		val          string
		decimalComma bool
		want         float64
		ok           bool
	}{
		{"1234.56", false, 1234.56, true},
		{"-1,234.56", false, -1234.56, true}, // thousands comma
		{"1.234,56", true, 1234.56, true},    // European
		{"€ 99,90", true, 99.90, true},       // currency symbol + comma decimal
		{"(50.00)", false, -50, true},        // accounting negative
		{"", false, 0, false},
		{"abc", false, 0, false},
	}
	for _, c := range cases {
		got, ok := parseCSVAmount(c.val, c.decimalComma)
		if ok != c.ok || (ok && (got < c.want-0.001 || got > c.want+0.001)) {
			t.Errorf("parseCSVAmount(%q,%v) = %v,%v want %v,%v", c.val, c.decimalComma, got, ok, c.want, c.ok)
		}
	}
}
