package controller

import (
	"math"
	"testing"
)

func TestParseDecimal(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"1.5", 1.5},
		{"1,5", 1.5},   // European decimal comma — must not become 15
		{"1,50", 1.5},  // cents with a decimal comma
		{"12,5", 12.5}, // two-digit integer part, decimal comma
		{"1234,56", 1234.56},
		{"1234.56", 1234.56},
		{"1,234", 1234},        // lone comma + 3 digits = US thousands
		{"1,234.56", 1234.56},  // US thousands + decimal point
		{"1.234,56", 1234.56},  // EU thousands + decimal comma
		{"1,234,567", 1234567}, // multiple comma groups
		{"1,234,567.89", 1234567.89},
		{"1.234", 1.234}, // lone dot stays decimal (e.g. fractional shares)
		{"-50", -50},
		{"+50", 50},
		{"1 234,56", 1234.56}, // space thousands separator stripped
		{"0", 0},
	}
	for _, tc := range cases {
		got, err := parseDecimal(tc.in)
		if err != nil {
			t.Errorf("parseDecimal(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("parseDecimal(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseDecimalRejects(t *testing.T) {
	for _, in := range []string{"", "   ", "abc", "1.2.3", "NaN", "Inf", "-Inf", "1e400"} {
		if _, err := parseDecimal(in); err == nil {
			// 1e400 / NaN / Inf must never slip through as a non-finite float.
			t.Errorf("parseDecimal(%q) = nil error, want rejection", in)
		}
	}
}

func TestFormMoney(t *testing.T) {
	// Blank is allowed (caller decides what 0 means); garbage is rejected so the
	// caller can avoid silently storing 0.
	if v, ok := formMoney(""); !ok || v != 0 {
		t.Errorf("formMoney(\"\") = (%v,%v), want (0,true)", v, ok)
	}
	if v, ok := formMoney("12o0"); ok {
		t.Errorf("formMoney(\"12o0\") = (%v,true), want ok=false", v)
	}
	if v, ok := formMoney("1.234,56"); !ok || math.Abs(v-1234.56) > 1e-9 {
		t.Errorf("formMoney(\"1.234,56\") = (%v,%v), want (1234.56,true)", v, ok)
	}
}
