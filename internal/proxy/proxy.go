package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"apiproxy/internal/redis"
	"apiproxy/internal/stats"

	"github.com/shopspring/decimal"
	"github.com/tidwall/gjson"
)

const maxRequestBodySize = 32 * 1024 * 1024 // 32 MB

type Handler struct {
	redisClient *redis.Client
	statsStore  *stats.Store
	proxyURL    string
	httpClient  *http.Client
	wg          sync.WaitGroup
}

func NewHandler(r *redis.Client, s *stats.Store, proxyURL string) *Handler {
	return &Handler{
		redisClient: r,
		statsStore:  s,
		proxyURL:    proxyURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Minute,
		},
	}
}

// Shutdown waits for all in-flight recordUsage goroutines to finish.
func (h *Handler) Shutdown() {
	h.wg.Wait()
}

type tokenUsage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheWriteTokens int64
	CacheHitTokens   int64
}

func (h *Handler) ServeProxy(w http.ResponseWriter, r *http.Request) {
	kc := extractKey(r)
	if kc == "" {
		http.Error(w, `{"error":"missing api key"}`, http.StatusUnauthorized)
		return
	}

	ctx := r.Context()

	info, err := h.lookupKeyInfo(ctx, kc)
	if err != nil || info == nil || !info.IsActive {
		http.Error(w, `{"error":"invalid api key"}`, http.StatusUnauthorized)
		return
	}
	if info.ParentKeyCode == "" {
		http.Error(w, `{"error":"cannot use root key directly"}`, http.StatusForbidden)
		return
	}

	// Soft quota check: Redis used counter vs configured quota ("-1" = unlimited).
	if info.QuotaCNY != "-1" {
		used, err := h.redisClient.GetUsed(ctx, kc)
		if err != nil {
			log.Printf("quota check error for key %s: %v", kc, err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		usedDec := decimal.RequireFromString(used)
		quotaDec := decimal.RequireFromString(info.QuotaCNY)
		if usedDec.GreaterThanOrEqual(quotaDec) {
			http.Error(w, `{"error":"quota exceeded"}`, http.StatusTooManyRequests)
			return
		}
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodySize))
	if err != nil {
		http.Error(w, `{"error":"failed to read request"}`, http.StatusBadRequest)
		return
	}

	model := extractModel(body)
	if model != "" {
		if configured, err := h.modelHasPrices(ctx, model); err != nil || !configured {
			http.Error(w, `{"error":"model `+model+` is not configured for billing; please contact the administrator to add pricing"}`, http.StatusPaymentRequired)
			return
		}
	}

	upstreamURL := h.proxyURL + r.URL.Path
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	upReq, err := http.NewRequestWithContext(ctx, r.Method, upstreamURL, bytes.NewReader(body))
	if err != nil {
		http.Error(w, `{"error":"failed to build request"}`, http.StatusInternalServerError)
		return
	}

	copyHeaders(upReq.Header, r.Header)
	upReq.Header.Set("Authorization", "Bearer "+info.ParentKeyCode)
	upReq.Header.Set("X-Api-Key", info.ParentKeyCode)

	isStream := isStreamingRequest(body)

	resp, err := h.httpClient.Do(upReq)
	if err != nil {
		http.Error(w, `{"error":"upstream request failed"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	if resp.StatusCode != http.StatusOK {
		io.Copy(w, resp.Body) //nolint:errcheck
		if model != "" {
			h.wg.Add(1)
			go func() {
				defer h.wg.Done()
				h.recordFail(info, model)
			}()
		}
		return
	}

	var usage tokenUsage
	if isStream {
		usage = h.streamResponse(w, resp.Body)
	} else {
		usage = h.bufferedResponse(w, resp.Body)
	}

	if model != "" {
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			h.recordUsage(info, kc, model, usage)
		}()
	}
}

// lookupKeyInfo returns the cached KeyInfo for a key_code.
// On a cache miss it falls back to MySQL, builds the KeyInfo, and warms the cache.
func (h *Handler) lookupKeyInfo(ctx context.Context, kc string) (*redis.KeyInfo, error) {
	info, err := h.redisClient.GetKeyInfo(ctx, kc)
	if err != nil {
		return nil, err
	}
	if info != nil {
		return info, nil
	}
	// Cache miss: load sub-key and parent from MySQL.
	subKey, err := h.statsStore.GetKeyByCode(ctx, kc)
	if err != nil {
		return nil, err
	}
	if subKey.ParentID == -1 {
		// Root key — return a stub so the caller can reject it.
		return &redis.KeyInfo{ID: subKey.ID, IsActive: subKey.IsActive}, nil
	}
	parentKey, err := h.statsStore.GetKeyByID(ctx, subKey.ParentID)
	if err != nil || !parentKey.IsActive {
		return nil, fmt.Errorf("parent key unavailable")
	}
	built := redis.KeyInfo{
		ID:            subKey.ID,
		IsActive:      subKey.IsActive,
		QuotaCNY:      subKey.QuotaCNY,
		ParentKeyCode: parentKey.KeyCode,
	}
	h.redisClient.SetKeyInfo(ctx, kc, built) //nolint:errcheck
	return &built, nil
}

func (h *Handler) streamResponse(w http.ResponseWriter, body io.Reader) tokenUsage {
	flusher, canFlush := w.(http.Flusher)
	var usage tokenUsage
	var leftover []byte
	buf := make([]byte, 4096)

	for {
		n, err := body.Read(buf)
		if n > 0 {
			chunk := make([]byte, len(leftover)+n)
			copy(chunk, leftover)
			copy(chunk[len(leftover):], buf[:n])
			leftover = nil

			lines := bytes.Split(chunk, []byte("\n"))
			for i, line := range lines {
				if i == len(lines)-1 && len(line) > 0 {
					leftover = line
					continue
				}
				w.Write(line)         //nolint:errcheck
				w.Write([]byte("\n")) //nolint:errcheck
				parseSSEUsage(line, &usage)
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			break
		}
	}
	if len(leftover) > 0 {
		w.Write(leftover) //nolint:errcheck
		parseSSEUsage(leftover, &usage)
	}
	return usage
}

func (h *Handler) bufferedResponse(w http.ResponseWriter, body io.Reader) tokenUsage {
	data, err := io.ReadAll(body)
	if err != nil {
		return tokenUsage{}
	}
	w.Write(data) //nolint:errcheck
	return parseBodyUsage(data)
}

func (h *Handler) recordUsage(info *redis.KeyInfo, kc string, model string, usage tokenUsage) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	contextLen := usage.InputTokens + usage.CacheWriteTokens + usage.CacheHitTokens
	price, err := h.findPrice(ctx, model, contextLen)
	if err != nil {
		log.Printf("no price for model %s: %v", model, err)
		return
	}

	cost := calcCost(usage, price)

	entry := stats.UsageEntry{
		Model:            model,
		InputTokens:      usage.InputTokens,
		OutputTokens:     usage.OutputTokens,
		CacheWriteTokens: usage.CacheWriteTokens,
		CacheHitTokens:   usage.CacheHitTokens,
		CostCNY:          cost,
		CallCount:        1,
	}

	if err := h.statsStore.Record(ctx, info.ID, entry); err != nil {
		log.Printf("record stats error: %v", err)
		return
	}
	if err := h.redisClient.IncrUsed(ctx, kc, cost); err != nil {
		log.Printf("incr used error: %v", err)
	}
}

func (h *Handler) recordFail(info *redis.KeyInfo, model string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	entry := stats.UsageEntry{Model: model, FailCount: 1}
	if err := h.statsStore.Record(ctx, info.ID, entry); err != nil {
		log.Printf("record fail stats error: %v", err)
	}
}

// modelHasPrices returns true if at least one price tier is configured for the model.
func (h *Handler) modelHasPrices(ctx context.Context, model string) (bool, error) {
	raw, err := h.redisClient.GetPriceCacheRaw(ctx, model)
	if err != nil {
		return false, err
	}
	if raw != nil {
		var prices []stats.ModelPrice
		if json.Unmarshal(raw, &prices) == nil {
			return len(prices) > 0, nil
		}
	}
	prices, err := h.statsStore.GetModelPrices(ctx, model)
	if err != nil {
		return false, err
	}
	return len(prices) > 0, nil
}

// findPrice checks the Redis price cache first, falls back to MySQL on a miss.
func (h *Handler) findPrice(ctx context.Context, model string, contextLen int64) (*stats.ModelPrice, error) {
	raw, err := h.redisClient.GetPriceCacheRaw(ctx, model)
	if err != nil {
		return nil, err
	}
	var prices []stats.ModelPrice
	if raw != nil {
		if err := json.Unmarshal(raw, &prices); err != nil {
			log.Printf("findPrice: corrupt cache for %s, falling back to MySQL: %v", model, err)
			prices = nil
		}
	}
	if prices == nil {
		prices, err = h.statsStore.GetModelPrices(ctx, model)
		if err != nil {
			return nil, err
		}
		if len(prices) > 0 {
			if data, merr := json.Marshal(prices); merr == nil {
				h.redisClient.SetPriceCacheRaw(ctx, model, data) //nolint:errcheck
			}
		}
	}
	for _, p := range prices {
		if contextLen >= p.ContextMin {
			if p.ContextMax == -1 || contextLen < p.ContextMax {
				pc := p
				return &pc, nil
			}
		}
	}
	return nil, fmt.Errorf("no price found for model %s context %d", model, contextLen)
}

func calcCost(u tokenUsage, p *stats.ModelPrice) string {
	million := decimal.NewFromInt(1_000_000)
	sum := decimal.NewFromInt(u.InputTokens).Mul(decimal.RequireFromString(p.InputCNY)).
		Add(decimal.NewFromInt(u.OutputTokens).Mul(decimal.RequireFromString(p.OutputCNY)))
	if p.CacheHitCNY != "0" {
		sum = sum.Add(decimal.NewFromInt(u.CacheHitTokens).Mul(decimal.RequireFromString(p.CacheHitCNY)))
	}
	if p.CacheWriteCNY != "0" {
		sum = sum.Add(decimal.NewFromInt(u.CacheWriteTokens).Mul(decimal.RequireFromString(p.CacheWriteCNY)))
	}
	return sum.Div(million).StringFixed(12)
}

func extractKey(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	if key := r.Header.Get("X-Api-Key"); key != "" {
		return key
	}
	return ""
}

// copyHeaders copies safe headers from src to dst, excluding hop-by-hop and auth headers.
func copyHeaders(dst, src http.Header) {
	skip := map[string]bool{
		"Authorization":       true,
		"X-Api-Key":           true,
		"Connection":          true,
		"Keep-Alive":          true,
		"Proxy-Authenticate":  true,
		"Proxy-Authorization": true,
		"Te":                  true,
		"Trailers":            true,
		"Transfer-Encoding":   true,
		"Upgrade":             true,
	}
	for k, vs := range src {
		canonical := http.CanonicalHeaderKey(k)
		if skip[canonical] {
			continue
		}
		for _, v := range vs {
			dst.Add(canonical, v)
		}
	}
}

func extractModel(body []byte) string {
	var req struct {
		Model string `json:"model"`
	}
	json.Unmarshal(body, &req) //nolint:errcheck
	return req.Model
}

func isStreamingRequest(body []byte) bool {
	var req struct {
		Stream bool `json:"stream"`
	}
	json.Unmarshal(body, &req) //nolint:errcheck
	return req.Stream
}

func parseSSEUsage(line []byte, u *tokenUsage) {
	trimmed := bytes.TrimRight(line, "\r")
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	} else {
		return
	}
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[DONE]")) || trimmed[0] != '{' {
		return
	}
	applyUsage(u, trimmed)
}

func parseBodyUsage(data []byte) tokenUsage {
	var u tokenUsage
	applyUsage(&u, data)
	return u
}

// applyUsage extracts token counts from an Anthropic or OpenAI JSON payload.
// Anthropic streaming: message_start has usage under message.usage; message_delta has it at top-level.
// OpenAI streaming/non-streaming: usage at top-level with prompt_tokens/completion_tokens.
func applyUsage(u *tokenUsage, data []byte) {
	if !gjson.ValidBytes(data) {
		return
	}
	root := gjson.ParseBytes(data)
	eventType := root.Get("type").String()

	// Top-level usage takes precedence; fall back to message.usage for upstreams
	// that nest it (e.g. direct Anthropic API message_start events).
	usageNode := root.Get("usage")
	if !usageNode.Exists() {
		usageNode = root.Get("message.usage")
	}
	if !usageNode.Exists() {
		return
	}

	switch eventType {
	case "message_start":
		u.InputTokens += usageNode.Get("input_tokens").Int()
		u.CacheWriteTokens += usageNode.Get("cache_creation_input_tokens").Int()
		u.CacheHitTokens += usageNode.Get("cache_read_input_tokens").Int()
	case "message_delta":
		u.OutputTokens += usageNode.Get("output_tokens").Int()
	default:
		// OpenAI style
		promptTokens := usageNode.Get("prompt_tokens").Int()
		completionTokens := usageNode.Get("completion_tokens").Int()
		if promptTokens > 0 || completionTokens > 0 {
			cached := usageNode.Get("prompt_tokens_details.cached_tokens").Int()
			u.CacheHitTokens += cached
			u.InputTokens += promptTokens - cached
			u.OutputTokens += completionTokens
			return
		}
		// Anthropic non-streaming
		inputTokens := usageNode.Get("input_tokens").Int()
		outputTokens := usageNode.Get("output_tokens").Int()
		if inputTokens > 0 || outputTokens > 0 {
			u.InputTokens += inputTokens
			u.OutputTokens += outputTokens
			u.CacheWriteTokens += usageNode.Get("cache_creation_input_tokens").Int()
			u.CacheHitTokens += usageNode.Get("cache_read_input_tokens").Int()
		}
	}
}
