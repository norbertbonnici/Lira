package controller

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// generateTOTPSecret returns a fresh base32 (no padding) secret.
func generateTOTPSecret() (string, error) {
	buf := make([]byte, 20) // 160-bit secret
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return b32.EncodeToString(buf), nil
}

// otpauthURL builds the otpauth:// provisioning URI for authenticator apps.
func otpauthURL(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", "6")
	q.Set("period", "30")
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// hotp implements RFC 4226 with 6 digits.
func hotp(key []byte, counter uint64) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	code := (uint32(sum[offset]&0x7f) << 24) |
		(uint32(sum[offset+1]) << 16) |
		(uint32(sum[offset+2]) << 8) |
		uint32(sum[offset+3])
	return fmt.Sprintf("%06d", code%1000000)
}

// validateTOTP checks a 6-digit code against the secret with a ±1 step window.
// On a match it returns the matched step and true; callers persist that step to
// reject replays of the same code. The full window is always scanned so timing
// does not leak which step matched.
func validateTOTP(secret, code string, unixTime int64) (step int64, ok bool) {
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return 0, false
	}
	key, err := b32.DecodeString(strings.ToUpper(strings.ReplaceAll(secret, " ", "")))
	if err != nil {
		return 0, false
	}
	now := unixTime / 30
	var matched int64
	found := 0
	for _, ctr := range []int64{now - 1, now, now + 1} {
		cand := hotp(key, uint64(ctr))
		if subtle.ConstantTimeCompare([]byte(cand), []byte(code)) == 1 {
			matched = ctr
			found = 1
		}
	}
	return matched, found == 1
}

// generateRecoveryCodes returns n human-friendly codes (plaintext) for one-time
// display. Store only their hashes.
func generateRecoveryCodes(n int) ([]string, error) {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789" // no ambiguous chars
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		var sb strings.Builder
		for j := 0; j < 10; j++ {
			if j == 5 {
				sb.WriteByte('-')
			}
			idx, err := randIndex(len(alphabet))
			if err != nil {
				return nil, err
			}
			sb.WriteByte(alphabet[idx])
		}
		out = append(out, sb.String())
	}
	return out, nil
}

// randIndex returns a uniform random integer in [0,n) using rejection sampling,
// avoiding the modulo bias of a plain byte%n.
func randIndex(n int) (int, error) {
	limit := 256 - (256 % n) // discard bytes at/above this to keep it uniform
	var b [1]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			return 0, err
		}
		if int(b[0]) < limit {
			return int(b[0]) % n, nil
		}
	}
}

// groupSecret formats a secret in blocks of 4 for easier manual entry.
func groupSecret(secret string) string {
	var sb strings.Builder
	for i, ch := range secret {
		if i > 0 && i%4 == 0 {
			sb.WriteByte(' ')
		}
		sb.WriteRune(ch)
	}
	return sb.String()
}
