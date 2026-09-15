// Package config loads runtime configuration from the environment.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr          string
	DBPath        string
	SecureCookies bool
	MaxUsers      int
	SessionTTL    time.Duration
	Issuer        string
	// BaseURL is the public origin (e.g. https://lira.example.com) used to build
	// absolute invite links. Empty falls back to the request's scheme+host.
	BaseURL string
	// MarketAPIKey enables the Twelve Data price/FX fetcher when non-empty.
	MarketAPIKey string
	// FetchInterval is how often the background fetcher refreshes prices + FX.
	FetchInterval time.Duration
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getenvBool(key string, def bool) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func Load() Config {
	return Config{
		Addr:          getenv("LIRA_ADDR", ":8080"),
		DBPath:        getenv("LIRA_DB", "./lira.db"),
		SecureCookies: getenvBool("LIRA_SECURE_COOKIES", true),
		MaxUsers:      getenvInt("LIRA_MAX_USERS", 2),
		SessionTTL:    time.Duration(getenvInt("LIRA_SESSION_TTL_HOURS", 168)) * time.Hour,
		Issuer:        getenv("LIRA_ISSUER", "Lira"),
		BaseURL:       strings.TrimRight(getenv("LIRA_BASE_URL", ""), "/"),
		MarketAPIKey:  getenv("LIRA_MARKET_API_KEY", ""),
		FetchInterval: time.Duration(getenvInt("LIRA_FETCH_INTERVAL_HOURS", 24)) * time.Hour,
	}
}
