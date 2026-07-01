package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"apiproxy/internal/api"
	"apiproxy/internal/config"
	"apiproxy/internal/db"
	"apiproxy/internal/proxy"
	redisclient "apiproxy/internal/redis"
	"apiproxy/internal/stats"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

//go:embed web/dist
var webFS embed.FS

func main() {
	cfg := config.Load()

	mysql, err := db.NewMySQL(cfg.MySQL.DSN)
	if err != nil {
		log.Fatalf("mysql: %v", err)
	}
	if err := db.Migrate(mysql); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	rdb, err := redisclient.New(cfg.Redis)
	if err != nil {
		log.Fatalf("redis: %v", err)
	}

	statsStore := stats.NewStore(mysql)
	proxyHandler := proxy.NewHandler(rdb, statsStore, cfg.ProxyURL)
	apiHandler := api.NewHandler(rdb, statsStore, cfg.OCRModel, cfg.OCRAPIKey, cfg.ProxyURL)

	r := gin.Default()

	// CORS: restrict to configured origins only (never wildcard for admin).
	// Always include the Vite dev server origin so `pnpm dev` works out of the box.
	corsOrigins := []string{"http://localhost:" + cfg.Port, "http://localhost:5173"}
	if cfg.AllowedOrigins != "" {
		corsOrigins = append(corsOrigins, splitOrigins(cfg.AllowedOrigins)...)
	}
	r.Use(cors.New(cors.Config{
		AllowOrigins:     corsOrigins,
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Content-Type", "Authorization", "X-Api-Key"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))

	// Session-based admin auth: POST /auth/login exchanges ADMIN_KEY for a
	// short-lived session token stored server-side (here in Redis).
	// The browser only ever holds the opaque session token, not the raw key.
	r.POST("/auth/login", loginHandler(cfg.AdminKey, rdb))
	r.POST("/auth/logout", logoutHandler(rdb))

	admin := r.Group("/admin", sessionAuth(rdb))
	apiHandler.RegisterRoutes(admin)

	// Proxy routes
	r.Any("/v1/*path", gin.WrapF(proxyHandler.ServeProxy))

	// Frontend
	distFS, err := fs.Sub(webFS, "web/dist")
	if err != nil {
		log.Fatalf("embed fs: %v", err)
	}
	r.NoRoute(func(c *gin.Context) {
		http.FileServer(http.FS(distFS)).ServeHTTP(c.Writer, c.Request)
	})

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: r,
	}

	// Graceful shutdown: wait for in-flight stats goroutines.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("listening on :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-quit
	log.Println("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	srv.Shutdown(ctx) //nolint:errcheck

	proxyHandler.Shutdown() // drain in-flight stats goroutines
	log.Println("bye")
}

const sessionCookie = "admin_session"
const sessionTTL = 8 * time.Hour

// loginLimiter tracks per-IP failed attempt counts for brute-force protection.
// After maxLoginAttempts failures the IP is locked out for loginLockDuration.
const (
	maxLoginAttempts = 10
	loginLockDuration = 15 * time.Minute
)

type loginAttempt struct {
	count   int
	lockedUntil time.Time
}

type loginLimiter struct {
	mu      sync.Mutex
	entries map[string]*loginAttempt
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{entries: make(map[string]*loginAttempt)}
}

func (l *loginLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[ip]
	if e == nil {
		return true
	}
	if !e.lockedUntil.IsZero() && time.Now().Before(e.lockedUntil) {
		return false
	}
	return true
}

func (l *loginLimiter) recordFailure(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[ip]
	if e == nil {
		e = &loginAttempt{}
		l.entries[ip] = e
	}
	// Reset after lock expiry
	if !e.lockedUntil.IsZero() && time.Now().After(e.lockedUntil) {
		e.count = 0
		e.lockedUntil = time.Time{}
	}
	e.count++
	if e.count >= maxLoginAttempts {
		e.lockedUntil = time.Now().Add(loginLockDuration)
	}
}

func (l *loginLimiter) recordSuccess(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, ip)
}

func loginHandler(adminKey string, rdb *redisclient.Client) gin.HandlerFunc {
	limiter := newLoginLimiter()
	return func(c *gin.Context) {
		ip := c.ClientIP()
		if !limiter.allow(ip) {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many failed attempts, try again later"})
			return
		}

		var body struct {
			Key string `json:"key" binding:"required"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "key required"})
			return
		}

		// Constant-time comparison to prevent timing attacks.
		if subtle.ConstantTimeCompare([]byte(body.Key), []byte(adminKey)) != 1 {
			limiter.recordFailure(ip)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid key"})
			return
		}
		limiter.recordSuccess(ip)

		token := uuid.NewString()
		if err := rdb.SetSession(c.Request.Context(), token, sessionTTL); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "session error"})
			return
		}

		// secure=true ensures the cookie is only sent over HTTPS.
		// SameSite=Strict added via raw header (Gin's SetCookie does not expose it).
		c.SetCookie(sessionCookie, token, int(sessionTTL.Seconds()), "/", "", true, true)
		// Overwrite with SameSite appended; Gin emits a single Set-Cookie header here.
		existing := c.Writer.Header().Get("Set-Cookie")
		if existing != "" {
			c.Writer.Header().Set("Set-Cookie", existing+"; SameSite=Strict")
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func logoutHandler(rdb *redisclient.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, err := c.Cookie(sessionCookie)
		if err == nil {
			rdb.DeleteSession(c.Request.Context(), token) //nolint:errcheck
		}
		c.SetCookie(sessionCookie, "", -1, "/", "", true, true)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func sessionAuth(rdb *redisclient.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, err := c.Cookie(sessionCookie)
		if err != nil || token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "not logged in"})
			return
		}
		ok, err := rdb.ValidSession(c.Request.Context(), token)
		if err != nil || !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "session expired"})
			return
		}
		c.Next()
	}
}

func splitOrigins(s string) []string {
	var out []string
	for _, o := range strings.Split(s, ",") {
		if t := strings.TrimSpace(o); t != "" {
			out = append(out, t)
		}
	}
	return out
}
