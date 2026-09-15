package controller

import (
	"encoding/base32"
	"testing"
)

func TestHOTPRFC6238(t *testing.T) {
	key := []byte("12345678901234567890")
	vectors := []struct {
		time int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1234567890, "005924"},
		{2000000000, "279037"},
	}
	for _, v := range vectors {
		if got := hotp(key, uint64(v.time/30)); got != v.want {
			t.Errorf("hotp @%d = %s, want %s", v.time, got, v.want)
		}
	}
}

func TestValidateTOTPWindow(t *testing.T) {
	key := []byte("12345678901234567890")
	sec := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(key)
	if _, ok := validateTOTP(sec, "287082", 59); !ok {
		t.Error("expected valid code at t=59")
	}
	if _, ok := validateTOTP(sec, "287082", 80); !ok {
		t.Error("expected valid within +1 step window")
	}
	if _, ok := validateTOTP(sec, "999999", 59); ok {
		t.Error("expected wrong code rejected")
	}
	if _, ok := validateTOTP(sec, "287082", 200); ok {
		t.Error("expected code outside window rejected")
	}
	// The matched step is returned so callers can guard against replay.
	if step, ok := validateTOTP(sec, "287082", 59); !ok || step != 59/30 {
		t.Errorf("matched step = %d, want %d", step, int64(59/30))
	}
}
