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
		keys.GET("/:id/stats", h.keyStats)
	}

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
	keys, err := h.stats.ListRootKeys(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, keys)
}

func (h *Handler) createKey(c *gin.Context) {
	var body struct {
		Name     string  `json:"name" binding:"required"`
		KeyCode  string  `json:"key_code"`
		ParentID int64   `json:"parent_id"` // -1 = root key (omit or set to -1)
		QuotaCNY *string `json:"quota_cny"` // yuan string, nil/"" = unlimited
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	keyCode := body.KeyCode
	if keyCode == "" {
		keyCode = "sk-" + randomHex(24)
	}

	parentID := body.ParentID
	if parentID == 0 {
		parentID = -1 // treat omitted/zero as root key
	}

	// Validate parent exists and is itself a root key.
	if parentID != -1 {
		parent, err := h.stats.GetKeyByID(c.Request.Context(), parentID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "parent key not found"})
			} else {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			}
			return
		}
		if parent.ParentID != -1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "parent key must be a root key"})
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

	key := &stats.APIKey{
		Name:     body.Name,
		KeyCode:  keyCode,
		ParentID: parentID,
		QuotaCNY: quotaCNY,
		IsActive: true,
	}
	if err := h.stats.CreateKey(c.Request.Context(), key); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Warm the key:code cache. Parent key_code is needed; look it up if this is a sub-key.
	if key.ParentID != -1 {
		parent, err := h.stats.GetKeyByID(c.Request.Context(), key.ParentID)
		if err == nil {
			h.redis.SetKeyInfo(c.Request.Context(), key.KeyCode, redis.KeyInfo{
				ID:            key.ID,
				IsActive:      key.IsActive,
				QuotaCNY:      key.QuotaCNY,
				ParentKeyCode: parent.KeyCode,
			}) //nolint:errcheck
		}
	}

	c.JSON(http.StatusCreated, key)
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
		Name     *string `json:"name"`
		QuotaCNY *string `json:"quota_cny"`
		IsActive *bool   `json:"is_active"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

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

	if err := h.stats.UpdateKey(c.Request.Context(), &updated); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Invalidate cache so proxy picks up the new is_active / quota_cny.
	h.redis.DeleteKeyInfo(c.Request.Context(), updated.KeyCode) //nolint:errcheck
	c.JSON(http.StatusOK, &updated)
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

	// Collect all key_codes to remove from Redis before deleting from MySQL.
	keyCodes := []string{key.KeyCode}
	if key.ParentID == -1 {
		children, err := h.stats.ListChildKeys(ctx, key.ID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		for _, ch := range children {
			keyCodes = append(keyCodes, ch.KeyCode)
		}
	}

	// Delete from MySQL (cascades to children via `OR parent_id = ?`).
	if err := h.stats.DeleteKey(ctx, key.ID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Clean up Redis caches.
	for _, kc := range keyCodes {
		h.redis.DeleteKeyInfo(ctx, kc) //nolint:errcheck
		h.redis.DeleteUsed(ctx, kc)    //nolint:errcheck
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
	// Attach live used_cny from Redis counters.
	result := make([]stats.APIKey, len(keys))
	for i, k := range keys {
		used, _ := h.redis.GetUsed(ctx, k.KeyCode)
		k.UsedCNY = used
		result[i] = k
	}
	c.JSON(http.StatusOK, result)
}

const dateFmt = "2006-01-02"

func (h *Handler) keyStats(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	start := c.DefaultQuery("start", time.Now().AddDate(0, 0, -7).Format(dateFmt))
	end := c.DefaultQuery("end", time.Now().Format(dateFmt))

	if _, err := time.Parse(dateFmt, start); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid start date, expected YYYY-MM-DD"})
		return
	}
	if _, err := time.Parse(dateFmt, end); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid end date, expected YYYY-MM-DD"})
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
}

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

	export := configExport{
		Version:     1,
		ExportedAt:  time.Now().UTC().Format(time.RFC3339),
		ModelPrices: prices,
		APIKeys:     allKeys,
		DailyStats:  dailyStats,
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
	if body.Version != 1 {
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
	oldKeyCodes := make([]string, 0, len(oldRoots))
	for _, r := range oldRoots {
		oldKeyCodes = append(oldKeyCodes, r.KeyCode)
		if children, err := h.stats.ListChildKeys(ctx, r.ID); err == nil {
			for _, ch := range children {
				oldKeyCodes = append(oldKeyCodes, ch.KeyCode)
			}
		}
	}
	oldModels, _ := h.stats.GetAllModelPrices(ctx)

	if err := h.stats.ImportAll(ctx, body.APIKeys, body.DailyStats, body.ModelPrices); err != nil {
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
