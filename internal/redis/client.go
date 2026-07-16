package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"apiproxy/internal/config"

	rdb "github.com/redis/go-redis/v9"
)

type Client struct {
	rdb *rdb.Client
}

// ParentRef pairs a root key's id with its key_code for the proxy failover list.
type ParentRef struct {
	RootID  int64  `json:"root_id"`
	KeyCode string `json:"key_code"` // upstream key_code to forward with
}

// KeyInfoSchema is the current KeyInfo cache-format version. Bump it whenever the
// cached shape changes so lookups can tell an authoritative entry from a stale
// one written by an older binary (which unmarshals to V == 0 and is re-derived).
const KeyInfoSchema = 1

// KeyInfo is the minimal proxy-side view of a sub-key, cached under key:code:{key_code}.
// It contains everything the proxy needs per request without hitting MySQL.
type KeyInfo struct {
	V        int         `json:"v,omitempty"` // cache-format version; see KeyInfoSchema
	ID       int64       `json:"id"`
	IsActive bool        `json:"is_active"`
	QuotaCNY string      `json:"quota_cny"` // unit: yuan (元); "-1" = unlimited
	Parents  []ParentRef `json:"parents"`   // upstream roots to try, in priority order
}

const (
	keyCode       = "key:code:"      // STRING key:code:{key_code}   -> JSON KeyInfo
	keyUsed       = "key:used:"      // STRING key:used:{key_code}   -> float64
	keyExhausted  = "key:exhausted:" // STRING key:exhausted:{rootID} -> "1" (TTL to month end)
	priceKey      = "price:model:"   // STRING price:model:{model}   -> JSON []ModelPrice
	sessionPrefix = "session:"       // STRING session:{token}       -> "1"
)

const priceCacheTTL = 5 * time.Minute

func New(cfg config.RedisConfig) (*Client, error) {
	r := rdb.NewClient(&rdb.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	})
	if err := r.Ping(context.Background()).Err(); err != nil {
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return &Client{rdb: r}, nil
}

// --- key:code cache ---

func (c *Client) SetKeyInfo(ctx context.Context, kc string, info KeyInfo) error {
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return c.rdb.Set(ctx, keyCode+kc, data, 0).Err()
}

func (c *Client) GetKeyInfo(ctx context.Context, kc string) (*KeyInfo, error) {
	data, err := c.rdb.Get(ctx, keyCode+kc).Bytes()
	if err == rdb.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var info KeyInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func (c *Client) DeleteKeyInfo(ctx context.Context, kc string) error {
	return c.rdb.Del(ctx, keyCode+kc).Err()
}

// --- key:used counter (keyed by key_code) ---

func (c *Client) IncrUsed(ctx context.Context, kc, cost string) error {
	f, err := strconv.ParseFloat(cost, 64)
	if err != nil {
		return fmt.Errorf("IncrUsed: invalid cost %q: %w", cost, err)
	}
	return c.rdb.IncrByFloat(ctx, keyUsed+kc, f).Err()
}

func (c *Client) GetUsed(ctx context.Context, kc string) (string, error) {
	v, err := c.rdb.Get(ctx, keyUsed+kc).Result()
	if err == rdb.Nil {
		return "0", nil
	}
	return v, err
}

func (c *Client) DeleteUsed(ctx context.Context, kc string) error {
	return c.rdb.Del(ctx, keyUsed+kc).Err()
}

func (c *Client) SetUsed(ctx context.Context, kc string, total float64) error {
	return c.rdb.Set(ctx, keyUsed+kc, total, 0).Err()
}

// --- root key exhaustion (keyed by root id, TTL to natural-month boundary) ---

func (c *Client) MarkRootExhausted(ctx context.Context, rootID int64, ttl time.Duration) error {
	return c.rdb.Set(ctx, keyExhausted+strconv.FormatInt(rootID, 10), "1", ttl).Err()
}

func (c *Client) IsRootExhausted(ctx context.Context, rootID int64) (bool, error) {
	n, err := c.rdb.Exists(ctx, keyExhausted+strconv.FormatInt(rootID, 10)).Result()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func (c *Client) ClearRootExhausted(ctx context.Context, rootID int64) error {
	return c.rdb.Del(ctx, keyExhausted+strconv.FormatInt(rootID, 10)).Err()
}

// --- price cache ---

func (c *Client) GetPriceCacheRaw(ctx context.Context, model string) ([]byte, error) {
	data, err := c.rdb.Get(ctx, priceKey+model).Bytes()
	if err == rdb.Nil {
		return nil, nil
	}
	return data, err
}

func (c *Client) SetPriceCacheRaw(ctx context.Context, model string, data []byte) error {
	return c.rdb.Set(ctx, priceKey+model, data, priceCacheTTL).Err()
}

func (c *Client) DeletePriceCache(ctx context.Context, model string) error {
	return c.rdb.Del(ctx, priceKey+model).Err()
}

// --- sessions ---

func (c *Client) SetSession(ctx context.Context, token string, ttl time.Duration) error {
	return c.rdb.Set(ctx, sessionPrefix+token, "1", ttl).Err()
}

func (c *Client) ValidSession(ctx context.Context, token string) (bool, error) {
	n, err := c.rdb.Exists(ctx, sessionPrefix+token).Result()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func (c *Client) DeleteSession(ctx context.Context, token string) error {
	return c.rdb.Del(ctx, sessionPrefix+token).Err()
}
