package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
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
	redisClient       *redis.Client
	statsStore        *stats.Store
	proxyURL          string
	httpClient        *http.Client
	exhaustedStatuses map[int]bool
	exhaustedPatterns []string       // lowercase substrings
	billingLoc        *time.Location // timezone for the monthly exhaustion reset
	observeModels     map[string]bool
	observeAll        bool
	wg                sync.WaitGroup
}

func NewHandler(r *redis.Client, s *stats.Store, proxyURL string, exhaustedStatuses []int, exhaustedPatterns []string, billingLoc *time.Location, observeModels []string) *Handler {
	statusSet := make(map[int]bool, len(exhaustedStatuses))
	for _, code := range exhaustedStatuses {
		statusSet[code] = true
	}
	if billingLoc == nil {
		billingLoc = time.Local
	}
	observeSet := make(map[string]bool, len(observeModels))
	observeAll := false
	for _, m := range observeModels {
		if m == "*" {
			observeAll = true
		}
		observeSet[m] = true
	}
	return &Handler{
		redisClient:       r,
		statsStore:        s,
		proxyURL:          proxyURL,
		exhaustedStatuses: statusSet,
		exhaustedPatterns: exhaustedPatterns,
		billingLoc:        billingLoc,
		observeModels:     observeSet,
		observeAll:        observeAll,
		httpClient: &http.Client{
			Timeout: 10 * time.Minute,
		},
	}
}

// isObserved reports whether a model is in observe mode (allowed through without
// pricing, response structure logged).
func (h *Handler) isObserved(model string) bool {
	return h.observeAll || h.observeModels[strings.ToLower(model)]
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
	if len(info.Parents) == 0 {
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
	if model == "" {
		// Image edits etc. upload multipart/form-data — the model is a form field,
		// not JSON. Extract it so observe/billing engage for those too.
		model = extractMultipartModel(r.Header.Get("Content-Type"), body)
	}
	if model != "" {
		if configured, err := h.modelHasPrices(ctx, model); err != nil || !configured {
			// Observe mode: let the model through unpriced so its response can be
			// studied; otherwise enforce the billing gate.
			if h.isObserved(model) {
				log.Printf("observe: model %q has no pricing, passing through (no billing)", model)
			} else {
				http.Error(w, `{"error":"model `+model+` is not configured for billing; please contact the administrator to add pricing"}`, http.StatusPaymentRequired)
				return
			}
		}
	}

	upstreamURL := h.proxyURL + r.URL.Path
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}
	isStream := isStreamingRequest(body)

	// Failover loop: try bound root keys in priority order, skipping any marked
	// exhausted this month, until one yields a non-exhaustion response.
	anyTransportError := false
	for _, parent := range info.Parents {
		if exhausted, _ := h.redisClient.IsRootExhausted(ctx, parent.RootID); exhausted {
			continue
		}

		upReq, err := http.NewRequestWithContext(ctx, r.Method, upstreamURL, bytes.NewReader(body))
		if err != nil {
			http.Error(w, `{"error":"failed to build request"}`, http.StatusInternalServerError)
			return
		}
		copyHeaders(upReq.Header, r.Header)
		upReq.Header.Set("Authorization", "Bearer "+parent.KeyCode)
		upReq.Header.Set("X-Api-Key", parent.KeyCode)

		resp, err := h.httpClient.Do(upReq)
		if err != nil {
			// Transport error — not an exhaustion signal; try the next root.
			log.Printf("upstream request failed (root %d): %v", parent.RootID, err)
			anyTransportError = true
			continue
		}

		// Only candidate error statuses are buffered & inspected; success (and
		// other statuses) stream through untouched.
		if h.exhaustedStatuses[resp.StatusCode] {
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRequestBodySize))
			resp.Body.Close()
			if h.isExhaustedBody(errBody) {
				log.Printf("root %d exhausted: status=%d body=%s", parent.RootID, resp.StatusCode, snippet(errBody))
				if err := h.redisClient.MarkRootExhausted(ctx, parent.RootID, durationUntilNextMonth(time.Now(), h.billingLoc)); err != nil {
					log.Printf("mark root %d exhausted error: %v", parent.RootID, err)
				}
				continue // failover to next root
			}
			// Normal error with a candidate status — forward it as the final response.
			log.Printf("upstream error (root %d): status=%d body=%s", parent.RootID, resp.StatusCode, snippet(errBody))
			writeUpstreamHeaders(w, resp.Header)
			w.WriteHeader(resp.StatusCode)
			w.Write(errBody) //nolint:errcheck
			h.recordFailAsync(info, model)
			return
		}

		// Final response (success or non-candidate error) — forward as today.
		defer resp.Body.Close()
		writeUpstreamHeaders(w, resp.Header)
		w.WriteHeader(resp.StatusCode)
		if resp.StatusCode != http.StatusOK {
			io.Copy(w, resp.Body) //nolint:errcheck
			h.recordFailAsync(info, model)
			return
		}
		observe := h.isObserved(model)
		var usage tokenUsage
		if isStream {
			usage = h.streamResponse(w, resp.Body, observe, model)
		} else {
			usage = h.bufferedResponse(w, resp.Body, observe, model)
		}
		if model != "" {
			h.wg.Add(1)
			go func() {
				defer h.wg.Done()
				h.recordUsage(info, kc, model, usage)
			}()
		}
		return
	}

	// Loop ended without a forwarded response. Distinguish the causes so the
	// client (and logs) aren't misled: a transport outage is not "exhaustion".
	if anyTransportError {
		http.Error(w, `{"error":"upstream request failed"}`, http.StatusBadGateway)
	} else {
		// Every bound root was either already flagged exhausted or returned an
		// exhaustion response this call.
		http.Error(w, `{"error":"all upstream keys exhausted"}`, http.StatusTooManyRequests)
	}
}

// recordFailAsync records a failed call against the child key in the background.
func (h *Handler) recordFailAsync(info *redis.KeyInfo, model string) {
	if model == "" {
		return
	}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		h.recordFail(info, model)
	}()
}

// isExhaustedBody reports whether an upstream error body matches any configured
// "quota exhausted" pattern (case-insensitive substring).
func (h *Handler) isExhaustedBody(body []byte) bool {
	if len(h.exhaustedPatterns) == 0 {
		return false
	}
	lower := strings.ToLower(string(body))
	for _, p := range h.exhaustedPatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// writeUpstreamHeaders copies all upstream response headers to the client.
func writeUpstreamHeaders(w http.ResponseWriter, src http.Header) {
	dst := w.Header()
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// durationUntilNextMonth returns the time until the start of the next natural
// month in the given (billing) timezone, used as the TTL for a root's
// exhaustion flag so it clears when the upstream quota resets.
func durationUntilNextMonth(now time.Time, loc *time.Location) time.Duration {
	n := now.In(loc)
	y, m, _ := n.Date()
	next := time.Date(y, m+1, 1, 0, 0, 0, 0, loc)
	return next.Sub(n)
}

// snippet truncates a body for logging.
func snippet(b []byte) string {
	const max = 500
	if len(b) > max {
		return string(b[:max]) + "…"
	}
	return string(b)
}

// lookupKeyInfo returns the cached KeyInfo for a key_code.
// On a cache miss it falls back to MySQL, builds the KeyInfo, and warms the cache.
func (h *Handler) lookupKeyInfo(ctx context.Context, kc string) (*redis.KeyInfo, error) {
	info, err := h.redisClient.GetKeyInfo(ctx, kc)
	if err != nil {
		return nil, err
	}
	// A current-schema entry is authoritative — including an empty-Parents
	// (negative) entry, so a disabled/orphaned key doesn't re-query MySQL on
	// every request. Entries written by an older binary have V == 0 and are
	// re-derived (upgrade compatibility).
	if info != nil && info.V >= redis.KeyInfoSchema {
		return info, nil
	}
	// Cache miss: load sub-key from MySQL.
	subKey, err := h.statsStore.GetKeyByCode(ctx, kc)
	if err != nil {
		return nil, err
	}
	if subKey.ParentID == -1 {
		// Root key — return a stub (no parents) so the caller can reject it.
		return &redis.KeyInfo{V: redis.KeyInfoSchema, ID: subKey.ID, IsActive: subKey.IsActive}, nil
	}
	refs, err := h.statsStore.GetChildParentRefs(ctx, subKey.ID)
	if err != nil {
		return nil, err
	}
	parents := make([]redis.ParentRef, 0, len(refs))
	for _, ref := range refs {
		parents = append(parents, redis.ParentRef{RootID: ref.RootID, KeyCode: ref.KeyCode})
	}
	built := redis.KeyInfo{
		V:        redis.KeyInfoSchema,
		ID:       subKey.ID,
		IsActive: subKey.IsActive,
		QuotaCNY: subKey.QuotaCNY,
		Parents:  parents,
	}
	h.redisClient.SetKeyInfo(ctx, kc, built) //nolint:errcheck
	return &built, nil
}

func (h *Handler) streamResponse(w http.ResponseWriter, body io.Reader, observe bool, model string) tokenUsage {
	flusher, canFlush := w.(http.Flusher)
	var usage tokenUsage
	var scan []byte // accumulates bytes for line-based usage parsing ONLY
	buf := make([]byte, 4096)

	for {
		n, err := body.Read(buf)
		if n > 0 {
			// Forward the raw bytes VERBATIM. Never reconstruct the stream by
			// splitting on "\n" and re-appending separators — that injects stray
			// blank lines at read boundaries, which in SSE are event delimiters,
			// corrupting the client's parsing (e.g. tool_use blocks leaking as
			// text). A proxy must pass the body through byte-for-byte.
			w.Write(buf[:n]) //nolint:errcheck
			if canFlush {
				flusher.Flush()
			}
			// Parse usage from a private copy without touching the forwarded bytes.
			scan = append(scan, buf[:n]...)
			for {
				idx := bytes.IndexByte(scan, '\n')
				if idx < 0 {
					break
				}
				line := scan[:idx]
				parseSSEUsage(line, &usage)
				if observe && bytes.Contains(line, []byte(`"usage"`)) {
					log.Printf("observe %s stream usage line: %s", model, snippet(bytes.TrimSpace(line)))
				}
				scan = scan[idx+1:]
			}
		}
		if err != nil {
			break
		}
	}
	if len(scan) > 0 {
		parseSSEUsage(scan, &usage)
	}
	return usage
}

func (h *Handler) bufferedResponse(w http.ResponseWriter, body io.Reader, observe bool, model string) tokenUsage {
	data, err := io.ReadAll(body)
	if err != nil {
		return tokenUsage{}
	}
	w.Write(data) //nolint:errcheck
	// Forward the original bytes untouched above; parse/inspect a decompressed
	// copy so gzipped responses (common for large image payloads) still yield
	// correct token usage.
	parsed := maybeGunzip(data)
	if observe {
		logObservedResponse(model, parsed)
	}
	return parseBodyUsage(parsed)
}

// maybeGunzip returns data decompressed if it is gzip-framed, else data unchanged.
func maybeGunzip(data []byte) []byte {
	if len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
		return data
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return data
	}
	defer zr.Close()
	dec, err := io.ReadAll(io.LimitReader(zr, maxRequestBodySize))
	if err != nil {
		return data
	}
	return dec
}

// logObservedResponse logs the shape of an observed model's JSON response — the
// raw usage object, the top-level field names, and the data[] length (image
// count) — without dumping response payloads such as base64 image data. The
// caller passes already-decompressed data.
func logObservedResponse(model string, data []byte) {
	if !gjson.ValidBytes(data) {
		n := len(data)
		if n > 32 {
			n = 32
		}
		log.Printf("observe %s: non-JSON response (%d bytes) first-bytes-hex=%x", model, len(data), data[:n])
		return
	}
	root := gjson.ParseBytes(data)
	var keys []string
	root.ForEach(func(k, _ gjson.Result) bool {
		keys = append(keys, k.String())
		return true
	})
	usage := root.Get("usage").Raw
	if usage == "" {
		usage = "(none)"
	}
	dataCount := "n/a"
	if d := root.Get("data"); d.IsArray() {
		dataCount = strconv.Itoa(len(d.Array()))
	}
	log.Printf("observe %s: top_keys=%v data_count=%s usage=%s", model, keys, dataCount, usage)
}

func (h *Handler) recordUsage(info *redis.KeyInfo, kc string, model string, usage tokenUsage) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	contextLen := usage.InputTokens + usage.CacheWriteTokens + usage.CacheHitTokens
	price, err := h.findPrice(ctx, model, contextLen)
	if err != nil {
		// Observe mode: no price configured, but still persist the usage with
		// zero cost so the model's calls/tokens show up in stats for study.
		if h.isObserved(model) {
			entry := stats.UsageEntry{
				Model:            model,
				InputTokens:      usage.InputTokens,
				OutputTokens:     usage.OutputTokens,
				CacheWriteTokens: usage.CacheWriteTokens,
				CacheHitTokens:   usage.CacheHitTokens,
				CostCNY:          "0",
				CallCount:        1,
			}
			if rerr := h.statsStore.Record(ctx, info.ID, entry); rerr != nil {
				log.Printf("observe record stats error (%s): %v", model, rerr)
			}
			return
		}
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
	entry := stats.UsageEntry{Model: model, FailCount: 1, CostCNY: "0"}
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

// extractMultipartModel pulls the "model" form field out of a multipart/form-data
// body (used by image edits etc.) without reading uploaded file parts into memory.
func extractMultipartModel(contentType string, body []byte) string {
	if !strings.HasPrefix(contentType, "multipart/") {
		return ""
	}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	boundary := params["boundary"]
	if boundary == "" {
		return ""
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		p, err := mr.NextPart()
		if err != nil {
			return ""
		}
		if p.FormName() == "model" && p.FileName() == "" {
			val, _ := io.ReadAll(io.LimitReader(p, 256))
			p.Close() //nolint:errcheck
			return strings.TrimSpace(string(val))
		}
		p.Close() //nolint:errcheck
	}
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

	// Top-level usage takes precedence; fall back to message.usage (Anthropic
	// message_start events) then response.usage (OpenAI Responses API streaming,
	// where the final response.completed event nests usage under "response").
	usageNode := root.Get("usage")
	if !usageNode.Exists() {
		usageNode = root.Get("message.usage")
	}
	if !usageNode.Exists() {
		usageNode = root.Get("response.usage")
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
		// This provider puts final totals in message_delta (message_start has placeholder values).
		// Use max so message_delta overrides a wrong message_start value without double-counting.
		if v := usageNode.Get("input_tokens").Int(); v > u.InputTokens {
			u.InputTokens = v
		}
		if v := usageNode.Get("cache_creation_input_tokens").Int(); v > u.CacheWriteTokens {
			u.CacheWriteTokens = v
		}
		if v := usageNode.Get("cache_read_input_tokens").Int(); v > u.CacheHitTokens {
			u.CacheHitTokens = v
		}
	default:
		// OpenAI style
		promptTokens := usageNode.Get("prompt_tokens").Int()
		completionTokens := usageNode.Get("completion_tokens").Int()
		if promptTokens > 0 || completionTokens > 0 {
			// Cache hit: try all known field locations across providers.
			cacheHit := firstNonZeroInt64(
				usageNode.Get("cache_read_tokens").Int(),                        // this provider top-level
				usageNode.Get("prompt_tokens_details.cached_tokens").Int(),      // Alibaba Cloud / standard OpenAI
			)
			// Cache write: field name varies — try all known variants.
			cacheWrite := firstNonZeroInt64(
				usageNode.Get("cache_creation_tokens").Int(),                                            // this provider top-level
				usageNode.Get("prompt_tokens_details.cache_creation_tokens").Int(),                      // this provider nested
				usageNode.Get("prompt_tokens_details.cache_creation_input_tokens").Int(),                // Alibaba Cloud docs
				usageNode.Get("prompt_tokens_details.cache_creation.cache_creation_input_tokens").Int(), // standard OpenAI
			)
			u.CacheHitTokens += cacheHit
			u.CacheWriteTokens += cacheWrite
			u.InputTokens += promptTokens - cacheHit - cacheWrite
			u.OutputTokens += completionTokens
			return
		}
		// input_tokens / output_tokens style (Anthropic non-streaming, or the
		// OpenAI Responses API — including its streaming response.completed event).
		inputTokens := usageNode.Get("input_tokens").Int()
		outputTokens := usageNode.Get("output_tokens").Int()
		if inputTokens > 0 || outputTokens > 0 {
			if d := usageNode.Get("input_tokens_details"); d.Exists() {
				// Responses API (and image models): input_tokens is the TOTAL and
				// already includes cached tokens — split the cached part out so it
				// bills at the cache-hit rate rather than the input rate.
				cached := d.Get("cached_tokens").Int()
				u.CacheHitTokens += cached
				u.InputTokens += inputTokens - cached
			} else {
				// Anthropic non-streaming: input_tokens excludes cache; cache is
				// reported in its own fields.
				u.InputTokens += inputTokens
				u.CacheWriteTokens += usageNode.Get("cache_creation_input_tokens").Int()
				u.CacheHitTokens += usageNode.Get("cache_read_input_tokens").Int()
			}
			u.OutputTokens += outputTokens
		}
	}
}

func firstNonZeroInt64(vals ...int64) int64 {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}
