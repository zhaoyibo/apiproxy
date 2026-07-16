package config

import (
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	Port           string
	MySQL          MySQLConfig
	Redis          RedisConfig
	ProxyURL       string
	AdminKey       string
	AllowedOrigins string // comma-separated, e.g. "http://localhost:5173"
	OCRModel       string // VLM model used for price screenshot OCR
	OCRAPIKey      string // upstream API key used for OCR calls
	CookieSecure   bool   // set false for local HTTP dev (COOKIE_SECURE=false)

	// Upstream "quota exhausted" detection: when a response's status is in
	// ExhaustedStatuses AND its body contains any of ExhaustedPatterns (case
	// -insensitive), the proxy marks that root key exhausted and fails over to
	// the next bound root. Tunable via env without recompiling.
	ExhaustedStatuses []int
	ExhaustedPatterns []string

	// BillingLocation is the timezone whose natural-month boundary resets a root
	// key's exhaustion flag. Should match the upstream provider's billing TZ.
	BillingLocation *time.Location
}

type MySQLConfig struct {
	DSN string
}

type RedisConfig struct {
	Addr     string
	Password string
	DB       int
}

func Load() *Config {
	// Load .env if present; ignore error if file doesn't exist.
	godotenv.Load() //nolint:errcheck

	adminKey := os.Getenv("ADMIN_KEY")
	if adminKey == "" {
		log.Fatal("ADMIN_KEY env var must be set")
	}

	proxyURL := os.Getenv("PROXY_URL")
	if proxyURL == "" {
		log.Fatal("PROXY_URL env var must be set")
	}

	return &Config{
		Port:           getEnv("PORT", "8080"),
		AdminKey:       adminKey,
		ProxyURL:       proxyURL,
		AllowedOrigins: getEnv("ALLOWED_ORIGINS", ""),
		OCRModel:       getEnv("OCR_MODEL", "claude-opus-4-8"),
		OCRAPIKey:      getEnv("OCR_API_KEY", ""),
		CookieSecure:   getEnv("COOKIE_SECURE", "true") != "false",
		ExhaustedStatuses: parseIntList(getEnv("UPSTREAM_EXHAUSTED_STATUS", "429,403")),
		ExhaustedPatterns: parseLowerList(getEnv("UPSTREAM_EXHAUSTED_PATTERNS", "quota,insufficient,exceeded,余额,额度,欠费,balance,arrears")),
		BillingLocation:   loadLocation(getEnv("BILLING_TIMEZONE", "Asia/Shanghai")),
		MySQL: MySQLConfig{
			DSN: getEnv("MYSQL_DSN", "root:password@tcp(127.0.0.1:3306)/apiproxy?parseTime=true"),
		},
		Redis: RedisConfig{
			Addr:     getEnv("REDIS_ADDR", "127.0.0.1:6379"),
			Password: getEnv("REDIS_PASSWORD", ""),
			DB:       0,
		},
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parseIntList parses a comma-separated list of ints, skipping invalid entries.
func parseIntList(s string) []int {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if n, err := strconv.Atoi(part); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// loadLocation loads a timezone, falling back to local time (with a warning) if
// the name is invalid or the tzdata is unavailable.
func loadLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		log.Printf("config: invalid BILLING_TIMEZONE %q (%v); using local time", name, err)
		return time.Local
	}
	return loc
}

// parseLowerList parses a comma-separated list, trimming and lowercasing entries.
func parseLowerList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(strings.ToLower(part))
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
