package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"apiproxy/internal/redis"
	"apiproxy/internal/stats"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

const maxOCRImageSize = 5 * 1024 * 1024 // 5 MB

type Handler struct {
	redis      *redis.Client
	stats      *stats.Store
	ocrModel   string
	ocrAPIKey  string
	proxyURL   string
	httpClient *http.Client
}

func NewHandler(r *redis.Client, s *stats.Store, ocrModel, ocrAPIKey, proxyURL string) *Handler {
	return &Handler{
		redis:     r,
		stats:     s,
		ocrModel:  ocrModel,
		ocrAPIKey: ocrAPIKey,
		proxyURL:  proxyURL,
		httpClient: &http.Client{
			Timeout: 2 * time.Minute,
		},
	}
}

func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	keys := rg.Group("/keys")
	{
		keys.GET("", h.listKeys)
		keys.POST("", h.createKey)
		keys.GET("/:id", h.getKey)
		keys.PUT("/:id", h.updateKey)
		keys.DELETE("/:id", h.deleteKey)
		keys.GET("/:id/children", h.listChildren)
		keys.PUT("/:id/children", h.setRootChildren)
		keys.GET("/:id/stats", h.keyStats)
		keys.POST("/:id/clear-exhausted", h.clearExhausted)
	}

	// Flat child-key list and global stats (kept off /keys to avoid a static-vs-param
	// route conflict with /keys/:id).
	rg.GET("/child-keys", h.listAllChildren)
	rg.GET("/stats/all", h.allStats)

	prices := rg.Group("/prices")
	{
		prices.GET("", h.listPrices)
		prices.PUT("/:model", h.setPrices)
		prices.DELETE("/:model", h.deletePrices)
		prices.POST("/ocr", h.ocrPrice)
	}

	cfg := rg.Group("/config")
	{
		cfg.GET("/export", h.exportConfig)
		cfg.POST("/import", h.importConfig)
	}
}

func parseID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return 0, false
	}
	return id, true
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("randomHex: entropy unavailable: %v", err))
	}
	return hex.EncodeToString(b)
}

// --- Keys ---

func (h *Handler) listKeys(c *gin.Context) {
	ctx := c.Request.Context()
	keys, err := h.stats.ListRootKeys(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Attach the current-month exhaustion flag for each root.
	for i := range keys {
		exhausted, _ := h.redis.IsRootExhausted(ctx, keys[i].ID)
		keys[i].Exhausted = exhausted
	}
	c.JSON(http.StatusOK, keys)
}

func (h *Handler) createKey(c *gin.Context) {
	var body struct {
		Name     string  `json:"name" binding:"required"`
		KeyCode  string  `json:"key_code"`
		ParentID int64   `json:"parent_id"` // legacy: single root; -1 = root key
		RootIDs  []int64 `json:"root_ids"`  // ordered bound roots (priority = index)
		QuotaCNY *string `json:"quota_cny"` // yuan string, nil/"" = unlimited
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx := c.Request.Context()

	keyCode := body.KeyCode
	if keyCode == "" {
		keyCode = "sk-" + randomHex(24)
	}

	// Resolve the ordered root list. Fall back to legacy parent_id if root_ids omitted.
	rootIDs := body.RootIDs
	if len(rootIDs) == 0 && body.ParentID > 0 {
		rootIDs = []int64{body.ParentID}
	}
	rootIDs = dedupInt64(rootIDs) // avoid a PK clash in key_parents

	// Validate every bound root exists and is itself a root key.
	for _, rid := range rootIDs {
		root, err := h.stats.GetKeyByID(ctx, rid)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("root key %d not found", rid)})
			} else {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			}
			return
		}
		if root.ParentID != -1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("key %d must be a root key", rid)})
			return
		}
	}

	// Validate and normalise quota: nil / "" / "0" → "-1" (unlimited).
	quotaCNY := "-1"
	if body.QuotaCNY != nil && *body.QuotaCNY != "" && *body.QuotaCNY != "0" {
		if _, err := decimal.NewFromString(*body.QuotaCNY); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid quota_cny: " + err.Error()})
			return
		}
		quotaCNY = *body.QuotaCNY
	}

	// parent_id: -1 for root keys, else the primary (first) bound root.
	parentID := int64(-1)
	if len(rootIDs) > 0 {
		parentID = rootIDs[0]
	}

	key := &stats.APIKey{
		Name:     body.Name,
		KeyCode:  keyCode,
		ParentID: parentID,
		QuotaCNY: quotaCNY,
		IsActive: true,
	}
	if err := h.stats.CreateKey(ctx, key); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Persist bindings and warm the key:code cache for sub-keys.
	if len(rootIDs) > 0 {
		if err := h.stats.SetChildParents(ctx, key.ID, rootIDs); err != nil {
			// Roll back the just-created key so a binding failure doesn't leave
			// an orphaned, unusable child row behind.
			h.stats.DeleteKey(ctx, key.ID) //nolint:errcheck
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		h.warmKeyInfo(ctx, key)
		key.RootIDs = rootIDs
	}

	c.JSON(http.StatusCreated, key)
}

// dedupInt64 returns the input with duplicates removed, preserving first-seen order.
func dedupInt64(in []int64) []int64 {
	seen := make(map[int64]struct{}, len(in))
	out := make([]int64, 0, len(in))
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// warmKeyInfo (re)builds the proxy cache entry for a sub-key from its current bindings.
func (h *Handler) warmKeyInfo(ctx context.Context, key *stats.APIKey) {
	refs, err := h.stats.GetChildParentRefs(ctx, key.ID)
	if err != nil {
		return
	}
	parents := make([]redis.ParentRef, 0, len(refs))
	for _, ref := range refs {
		parents = append(parents, redis.ParentRef{RootID: ref.RootID, KeyCode: ref.KeyCode})
	}
	h.redis.SetKeyInfo(ctx, key.KeyCode, redis.KeyInfo{
		V:        redis.KeyInfoSchema,
		ID:       key.ID,
		IsActive: key.IsActive,
		QuotaCNY: key.QuotaCNY,
		Parents:  parents,
	}) //nolint:errcheck
}

func (h *Handler) getKey(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	key, err := h.stats.GetKeyByID(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Attach live used_cny from Redis counter.
	used, _ := h.redis.GetUsed(c.Request.Context(), key.KeyCode)
	key.UsedCNY = used
	if key.ParentID != -1 {
		key.RootIDs, _ = h.stats.GetChildParents(c.Request.Context(), key.ID)
	} else {
		key.Exhausted, _ = h.redis.IsRootExhausted(c.Request.Context(), key.ID)
	}
	c.JSON(http.StatusOK, key)
}

func (h *Handler) updateKey(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	key, err := h.stats.GetKeyByID(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var body struct {
		Name     *string  `json:"name"`
		QuotaCNY *string  `json:"quota_cny"`
		IsActive *bool    `json:"is_active"`
		RootIDs  []int64  `json:"root_ids"` // if non-nil, replaces the child's bound roots (ordered)
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx := c.Request.Context()

	updated := *key
	if body.Name != nil {
		updated.Name = *body.Name
	}
	if body.QuotaCNY != nil {
		q := *body.QuotaCNY
		if q == "" || q == "0" {
			q = "-1"
		} else if q != "-1" {
			if _, err := decimal.NewFromString(q); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid quota_cny: " + err.Error()})
				return
			}
		}
		updated.QuotaCNY = q
	}
	if body.IsActive != nil {
		updated.IsActive = *body.IsActive
	}

	if err := h.stats.UpdateKey(ctx, &updated); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Replace bindings if provided (only meaningful for child keys).
	if body.RootIDs != nil && updated.ParentID != -1 {
		rootIDs := dedupInt64(body.RootIDs)
		for _, rid := range rootIDs {
			root, err := h.stats.GetKeyByID(ctx, rid)
			if err != nil || root.ParentID != -1 {
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("key %d must be an existing root key", rid)})
				return
			}
		}
		if err := h.stats.SetChildParents(ctx, updated.ID, rootIDs); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		updated.RootIDs = rootIDs
	} else if updated.ParentID != -1 {
		updated.RootIDs, _ = h.stats.GetChildParents(ctx, updated.ID)
	}

	// Invalidate cache so proxy picks up new is_active / quota_cny / bindings.
	h.redis.DeleteKeyInfo(ctx, updated.KeyCode) //nolint:errcheck
	// If a ROOT key changed (e.g. toggled active/inactive), every child bound to
	// it has a stale cached failover list — invalidate them so the proxy rebuilds.
	if updated.ParentID == -1 {
		h.invalidateChildrenOfRoot(ctx, updated.ID)
	}
	c.JSON(http.StatusOK, &updated)
}

// invalidateChildrenOfRoot drops the proxy cache entry of every child key bound
// to the given root so its failover list is rebuilt on the next request.
func (h *Handler) invalidateChildrenOfRoot(ctx context.Context, rootID int64) {
	childIDs, err := h.stats.ListChildIDsByRoot(ctx, rootID)
	if err != nil {
		return
	}
	for _, cid := range childIDs {
		if ch, err := h.stats.GetKeyByID(ctx, cid); err == nil {
			h.redis.DeleteKeyInfo(ctx, ch.KeyCode) //nolint:errcheck
		}
	}
}

func (h *Handler) deleteKey(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	key, err := h.stats.GetKeyByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Collect key_codes whose proxy cache must be invalidated. Deleting a root
	// unbinds it from its children (no cascade delete), so those children need
	// their cached failover list rebuilt on next request.
	invalidate := []string{key.KeyCode}
	if key.ParentID == -1 {
		childIDs, err := h.stats.ListChildIDsByRoot(ctx, key.ID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		for _, cid := range childIDs {
			if ch, err := h.stats.GetKeyByID(ctx, cid); err == nil {
				invalidate = append(invalidate, ch.KeyCode)
			}
		}
	}

	// Delete the key row + its bindings (see stats.DeleteKey — no child cascade).
	if err := h.stats.DeleteKey(ctx, key.ID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Clean up Redis caches.
	for _, kc := range invalidate {
		h.redis.DeleteKeyInfo(ctx, kc) //nolint:errcheck
	}
	h.redis.DeleteUsed(ctx, key.KeyCode) //nolint:errcheck
	if key.ParentID == -1 {
		h.redis.ClearRootExhausted(ctx, key.ID) //nolint:errcheck
	}

	c.Status(http.StatusNoContent)
}

func (h *Handler) listChildren(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	keys, err := h.stats.ListChildKeys(ctx, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, h.enrichChildren(ctx, keys))
}

// listAllChildren returns every child key (flat), each with its live used_cny and
// ordered bound root ids.
func (h *Handler) listAllChildren(c *gin.Context) {
	ctx := c.Request.Context()
	keys, err := h.stats.ListAllChildKeys(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, h.enrichChildren(ctx, keys))
}

// enrichChildren attaches live used_cny and ordered root_ids to each child key.
func (h *Handler) enrichChildren(ctx context.Context, keys []stats.APIKey) []stats.APIKey {
	result := make([]stats.APIKey, len(keys))
	for i, k := range keys {
		used, _ := h.redis.GetUsed(ctx, k.KeyCode)
		k.UsedCNY = used
		k.RootIDs, _ = h.stats.GetChildParents(ctx, k.ID)
		result[i] = k
	}
	return result
}

// allStats aggregates daily stats across every child key for the date range.
func (h *Handler) allStats(c *gin.Context) {
	ctx := c.Request.Context()
	start, end, ok := parseStatsRange(c)
	if !ok {
		return
	}
	children, err := h.stats.ListAllChildKeys(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ids := make([]int64, len(children))
	for i, ch := range children {
		ids[i] = ch.ID
	}
	rows, err := h.stats.QueryByParent(ctx, ids, start, end)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, rows)
}

// setRootChildren batch-binds a root key to a set of child keys (checkbox UI on
// the root side). Children checked get this root appended to their failover list;
// unchecked ones get it removed.
func (h *Handler) setRootChildren(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	root, err := h.stats.GetKeyByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if root.ParentID != -1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "not a root key"})
		return
	}

	var body struct {
		ChildIDs []int64 `json:"child_ids"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	childIDs := dedupInt64(body.ChildIDs)
	for _, cid := range childIDs {
		ch, err := h.stats.GetKeyByID(ctx, cid)
		if err != nil || ch.ParentID == -1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("key %d must be an existing child key", cid)})
			return
		}
	}

	affected, err := h.stats.SetRootChildren(ctx, id, childIDs)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Rebuild the proxy cache for every child whose bindings changed.
	for _, cid := range affected {
		if ch, err := h.stats.GetKeyByID(ctx, cid); err == nil {
			h.redis.DeleteKeyInfo(ctx, ch.KeyCode) //nolint:errcheck
		}
	}
	c.Status(http.StatusNoContent)
}

// clearExhausted clears a root key's current-month exhaustion flag.
func (h *Handler) clearExhausted(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	if err := h.redis.ClearRootExhausted(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

const dateFmt = "2006-01-02"

// parseStatsRange reads and validates the start/end query params (defaults: −7d..today).
func parseStatsRange(c *gin.Context) (start, end string, ok bool) {
	start = c.DefaultQuery("start", time.Now().AddDate(0, 0, -7).Format(dateFmt))
	end = c.DefaultQuery("end", time.Now().Format(dateFmt))
	if _, err := time.Parse(dateFmt, start); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid start date, expected YYYY-MM-DD"})
		return "", "", false
	}
	if _, err := time.Parse(dateFmt, end); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid end date, expected YYYY-MM-DD"})
		return "", "", false
	}
	return start, end, true
}

func (h *Handler) keyStats(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	start, end, ok := parseStatsRange(c)
	if !ok {
		return
	}

	ctx := c.Request.Context()
	key, err := h.stats.GetKeyByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if key.ParentID == -1 {
		children, err := h.stats.ListChildKeys(ctx, key.ID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		ids := make([]int64, len(children))
		for i, ch := range children {
			ids[i] = ch.ID
		}
		result, err := h.stats.QueryByParent(ctx, ids, start, end)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, result)
		return
	}

	result, err := h.stats.Query(ctx, key.ID, start, end)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}

// --- OCR ---

type ocrPriceTier struct {
	ContextMin    int64   `json:"context_min"`
	ContextMax    *int64  `json:"context_max"`
	InputCNY      float64 `json:"input_cny"`
	OutputCNY     float64 `json:"output_cny"`
	CacheHitCNY   float64 `json:"cache_hit_cny"`
	CacheWriteCNY float64 `json:"cache_write_cny"`
}

type ocrResult struct {
	Model string         `json:"model"`
	Tiers []ocrPriceTier `json:"tiers"`
}

func (h *Handler) ocrPrice(c *gin.Context) {
	file, header, err := c.Request.FormFile("image")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "image file required"})
		return
	}
	defer file.Close()

	if header.Size > maxOCRImageSize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "image too large, max 5 MB"})
		return
	}

	imgBytes, err := io.ReadAll(file)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read image"})
		return
	}

	mediaType := header.Header.Get("Content-Type")
	if mediaType == "" {
		mediaType = "image/png"
	}
	allowed := map[string]bool{"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true}
	if !allowed[mediaType] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported image type: " + mediaType})
		return
	}

	result, err := h.callVLM(c.Request.Context(), imgBytes, mediaType)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("VLM error: %v", err)})
		return
	}

	// Convert float yuan/百万token → decimal string yuan/百万token.
	prices := make([]stats.ModelPrice, len(result.Tiers))
	for i, t := range result.Tiers {
		contextMax := int64(-1)
		if t.ContextMax != nil {
			contextMax = *t.ContextMax
		}
		prices[i] = stats.ModelPrice{
			Model:         result.Model,
			ContextMin:    t.ContextMin,
			ContextMax:    contextMax,
			InputCNY:      decimal.NewFromFloat(t.InputCNY).String(),
			OutputCNY:     decimal.NewFromFloat(t.OutputCNY).String(),
			CacheHitCNY:   decimal.NewFromFloat(t.CacheHitCNY).String(),
			CacheWriteCNY: decimal.NewFromFloat(t.CacheWriteCNY).String(),
		}
	}

	c.JSON(http.StatusOK, gin.H{"model": result.Model, "prices": prices})
}

func (h *Handler) callVLM(ctx context.Context, imgBytes []byte, mediaType string) (*ocrResult, error) {
	imgB64 := base64.StdEncoding.EncodeToString(imgBytes)

	prompt := `Extract the pricing information from this model pricing screenshot.
Return a JSON object with this exact structure (no markdown, no explanation):
{
  "model": "<model name as shown>",
  "tiers": [
    {
      "context_min": 0,
      "context_max": null,
      "input_cny": <yuan per million tokens, number>,
      "output_cny": <yuan per million tokens, number>,
      "cache_hit_cny": <yuan per million tokens or 0 if not shown>,
      "cache_write_cny": <yuan per million tokens or 0 if not shown>
    }
  ]
}
If there are multiple context tiers, include one entry per tier.
context_max should be null if it is unlimited (+∞). All prices are in yuan (元) per million tokens.`

	reqBody := map[string]any{
		"model":      h.ocrModel,
		"max_tokens": 1024,
		"messages": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{
						"type": "image",
						"source": map[string]any{
							"type":       "base64",
							"media_type": mediaType,
							"data":       imgB64,
						},
					},
					{"type": "text", "text": prompt},
				},
			},
		},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		h.proxyURL+"/v1/messages", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	if h.ocrAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+h.ocrAPIKey)
		req.Header.Set("X-Api-Key", h.ocrAPIKey)
	}

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, string(respBytes))
	}

	// Parse Anthropic response envelope.
	var envelope struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(respBytes, &envelope); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if len(envelope.Content) == 0 {
		return nil, fmt.Errorf("empty response from VLM")
	}

	text := envelope.Content[0].Text
	var result ocrResult
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		// Log raw response server-side; don't expose VLM output to the client.
		fmt.Printf("parse VLM JSON error: %v — raw: %s\n", err, text)
		return nil, fmt.Errorf("parse VLM JSON: invalid response format")
	}
	return &result, nil
}

// --- Config export / import ---

type configExport struct {
	Version     int                           `json:"version"`
	ExportedAt  string                        `json:"exported_at"`
	ModelPrices map[string][]stats.ModelPrice `json:"model_prices"`
	APIKeys     []stats.APIKey                `json:"api_keys"`
	DailyStats  []stats.DailyStat             `json:"daily_stats"`
	KeyParents  []stats.KeyParent             `json:"key_parents"` // added in v2 (many-to-many bindings)
}

const configVersion = 2

func (h *Handler) exportConfig(c *gin.Context) {
	ctx := c.Request.Context()

	prices, err := h.stats.GetAllModelPrices(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	roots, err := h.stats.ListRootKeys(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	allKeys := make([]stats.APIKey, 0, len(roots))
	for _, root := range roots {
		allKeys = append(allKeys, root)
		children, err := h.stats.ListChildKeys(ctx, root.ID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		allKeys = append(allKeys, children...)
	}

	dailyStats, err := h.stats.ListAllStats(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	keyParents, err := h.stats.ListAllKeyParents(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	export := configExport{
		Version:     configVersion,
		ExportedAt:  time.Now().UTC().Format(time.RFC3339),
		ModelPrices: prices,
		APIKeys:     allKeys,
		DailyStats:  dailyStats,
		KeyParents:  keyParents,
	}

	c.Header("Content-Disposition", `attachment; filename="apiproxy-config.json"`)
	c.JSON(http.StatusOK, export)
}

const maxImportBodySize = 50 << 20 // 50 MB

func (h *Handler) importConfig(c *gin.Context) {
	// Guard against oversized payloads to prevent OOM.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxImportBodySize)

	var body configExport
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// Accept v1 (single parent_id) and v2 (many-to-many key_parents). For v1,
	// ImportAll backfills bindings from each key's parent_id.
	if body.Version != 1 && body.Version != 2 {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("unsupported config version %d", body.Version)})
		return
	}

	// Basic structural validation before touching the database.
	if err := validateImport(body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx := c.Request.Context()

	// Collect existing key codes BEFORE overwriting so we can fully flush Redis.
	oldRoots, _ := h.stats.ListRootKeys(ctx)
	oldChildren, _ := h.stats.ListAllChildKeys(ctx)
	oldKeyCodes := make([]string, 0, len(oldRoots)+len(oldChildren))
	for _, r := range oldRoots {
		oldKeyCodes = append(oldKeyCodes, r.KeyCode)
	}
	for _, ch := range oldChildren {
		oldKeyCodes = append(oldKeyCodes, ch.KeyCode)
	}
	oldModels, _ := h.stats.GetAllModelPrices(ctx)

	if err := h.stats.ImportAll(ctx, body.APIKeys, body.DailyStats, body.ModelPrices, body.KeyParents); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Flush Redis for all previously existing and newly imported keys.
	allKeyCodes := make(map[string]struct{}, len(oldKeyCodes)+len(body.APIKeys))
	for _, kc := range oldKeyCodes {
		allKeyCodes[kc] = struct{}{}
	}
	for _, k := range body.APIKeys {
		allKeyCodes[k.KeyCode] = struct{}{}
	}
	for kc := range allKeyCodes {
		h.redis.DeleteKeyInfo(ctx, kc) //nolint:errcheck
		h.redis.DeleteUsed(ctx, kc)   //nolint:errcheck
	}
	// Clear stale exhaustion flags for previously existing roots.
	for _, r := range oldRoots {
		h.redis.ClearRootExhausted(ctx, r.ID) //nolint:errcheck
	}

	// Restore used_cny counters in Redis from the imported daily_stats.
	idToKeyCode := make(map[int64]string, len(body.APIKeys))
	for _, k := range body.APIKeys {
		idToKeyCode[k.ID] = k.KeyCode
	}
	usedByKey := make(map[string]decimal.Decimal, len(body.APIKeys))
	for _, d := range body.DailyStats {
		kc, ok := idToKeyCode[d.KeyID]
		if !ok {
			continue
		}
		cost, err := decimal.NewFromString(d.CostCNY)
		if err != nil {
			continue
		}
		usedByKey[kc] = usedByKey[kc].Add(cost)
	}
	for kc, total := range usedByKey {
		f, _ := total.Float64()
		h.redis.SetUsed(ctx, kc, f) //nolint:errcheck
	}

	allModels := make(map[string]struct{}, len(oldModels)+len(body.ModelPrices))
	for m := range oldModels {
		allModels[m] = struct{}{}
	}
	for m := range body.ModelPrices {
		allModels[m] = struct{}{}
	}
	for m := range allModels {
		h.redis.DeletePriceCache(ctx, m) //nolint:errcheck
	}

	c.JSON(http.StatusOK, gin.H{
		"keys":   len(body.APIKeys),
		"stats":  len(body.DailyStats),
		"models": len(body.ModelPrices),
	})
}

// validateImport performs basic structural checks before the import touches the DB.
func validateImport(body configExport) error {
	keyIDs := make(map[int64]struct{}, len(body.APIKeys))
	keyCodes := make(map[string]struct{}, len(body.APIKeys))
	for _, k := range body.APIKeys {
		if k.ID <= 0 {
			return fmt.Errorf("api_key has invalid id %d", k.ID)
		}
		if k.KeyCode == "" {
			return fmt.Errorf("api_key id=%d has empty key_code", k.ID)
		}
		if _, dup := keyCodes[k.KeyCode]; dup {
			return fmt.Errorf("duplicate key_code %q in import", k.KeyCode)
		}
		keyCodes[k.KeyCode] = struct{}{}
		keyIDs[k.ID] = struct{}{}
	}
	for i, d := range body.DailyStats {
		if _, ok := keyIDs[d.KeyID]; !ok {
			return fmt.Errorf("daily_stat[%d] references unknown key_id %d", i, d.KeyID)
		}
		cost, err := decimal.NewFromString(d.CostCNY)
		if err != nil {
			return fmt.Errorf("daily_stat[%d] has invalid cost_cny %q", i, d.CostCNY)
		}
		if cost.IsNegative() {
			return fmt.Errorf("daily_stat[%d] has negative cost_cny", i)
		}
		if d.InputTokens < 0 || d.OutputTokens < 0 || d.CacheWriteTokens < 0 || d.CacheHitTokens < 0 {
			return fmt.Errorf("daily_stat[%d] has negative token field", i)
		}
	}
	for i, kp := range body.KeyParents {
		if _, ok := keyIDs[kp.ChildID]; !ok {
			return fmt.Errorf("key_parents[%d] references unknown child_id %d", i, kp.ChildID)
		}
		if _, ok := keyIDs[kp.RootID]; !ok {
			return fmt.Errorf("key_parents[%d] references unknown root_id %d", i, kp.RootID)
		}
	}
	return nil
}

// --- Prices ---

func (h *Handler) listPrices(c *gin.Context) {
	prices, err := h.stats.GetAllModelPrices(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, prices)
}

func (h *Handler) setPrices(c *gin.Context) {
	model := c.Param("model")
	var prices []stats.ModelPrice
	if err := c.ShouldBindJSON(&prices); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(prices) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "prices must not be empty; use DELETE to remove a model"})
		return
	}
	if err := h.stats.SetModelPrices(c.Request.Context(), model, prices); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.redis.DeletePriceCache(c.Request.Context(), model) //nolint:errcheck
	c.JSON(http.StatusOK, prices)
}

func (h *Handler) deletePrices(c *gin.Context) {
	model := c.Param("model")
	if err := h.stats.DeleteModelPrices(c.Request.Context(), model); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.redis.DeletePriceCache(c.Request.Context(), model) //nolint:errcheck
	c.Status(http.StatusNoContent)
}
