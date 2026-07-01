package config

import (
	"log"
	"os"

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
