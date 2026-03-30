package handler

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"copilot-go/config"
	"copilot-go/instance"
	"copilot-go/store"

	"github.com/gin-gonic/gin"
)

// RegisterProxy sets up the proxy server routes.
func RegisterProxy(r *gin.Engine) {
	// Initialize rate limiter from environment.
	instance.InitRateLimiter()

	// Load per-account rate limit from pool config.
	if poolCfg, err := store.GetPoolConfig(); err == nil && poolCfg != nil {
		instance.SetPerAccountRPM(poolCfg.RateLimitRPM)
	}

	r.Use(proxyAuth())

	// OpenAI compatible endpoints
	r.POST("/chat/completions", proxyCompletions)
	r.POST("/v1/chat/completions", proxyCompletions)
	r.GET("/models", proxyModels)
	r.GET("/v1/models", proxyModels)
	r.POST("/embeddings", proxyEmbeddings)
	r.POST("/v1/embeddings", proxyEmbeddings)

	// Anthropic compatible endpoints
	r.POST("/v1/messages", proxyMessages)
	r.POST("/v1/messages/count_tokens", proxyCountTokens)

	// OpenAI Responses API endpoint
	r.POST("/v1/responses", proxyResponses)

	// Gemini native endpoints
	r.GET("/v1beta/models", proxyGeminiModels)
	r.POST("/v1beta/models/*action", proxyGeminiContent)
}

func proxyAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			// Also check x-api-key for Anthropic-style auth
			apiKey := c.GetHeader("x-api-key")
			if apiKey != "" {
				authHeader = "Bearer " + apiKey
			}
		}
	if authHeader == "" {
		apiKey := c.GetHeader("x-goog-api-key")
		if apiKey != "" {
			authHeader = "Bearer " + apiKey
		}
	}
	if authHeader == "" {
		if apiKey := c.Query("key"); apiKey != "" {
			authHeader = "Bearer " + apiKey
		}
	}

		if authHeader == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing authorization"})
			return
		}

		token := strings.TrimPrefix(authHeader, "Bearer ")

		// Check pool API key first
		poolCfg, _ := store.GetPoolConfig()
		if poolCfg != nil && poolCfg.Enabled && poolCfg.ApiKey == token {
			c.Set("isPool", true)
			c.Set("poolStrategy", poolCfg.Strategy)
			c.Next()
			return
		}

		// Check individual account API key
		account, err := store.GetAccountByApiKey(token)
		if err != nil || account == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid API key"})
			return
		}

		c.Set("accountID", account.ID)
		c.Set("isPool", false)
		c.Next()
	}
}

// resolvedAccount holds the resolved state and account ID.
type resolvedAccount struct {
	State     *config.State
	AccountID string
}

func resolveState(c *gin.Context, exclude map[string]bool) *resolvedAccount {
	isPool, _ := c.Get("isPool")
	if isPool == true {
		strategy := ""
		if s, ok := c.Get("poolStrategy"); ok {
			strategy = s.(string)
		}
		account, err := instance.SelectAccount(strategy, exclude)
		if err != nil || account == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no available accounts in pool"})
			return nil
		}
		state := instance.GetInstanceState(account.ID)
		if state == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "selected account instance not running"})
			return nil
		}
		return &resolvedAccount{State: state, AccountID: account.ID}
	}

	accountID, exists := c.Get("accountID")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no account context"})
		return nil
	}
	aid := accountID.(string)
	state := instance.GetInstanceState(aid)
	if state == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "account instance not running"})
		return nil
	}
	return &resolvedAccount{State: state, AccountID: aid}
}

// isRetryableStatus returns true for HTTP status codes that warrant a retry with a different account.
func isRetryableStatus(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests || (statusCode >= 500 && statusCode <= 599)
}

func checkRateLimit(accountID string) (bool, float64) {
	return instance.CheckRateLimit(accountID)
}

func writeRateLimitResponse(c *gin.Context, retryAfter float64) {
	c.Header("Retry-After", fmt.Sprintf("%.0f", retryAfter))
	c.JSON(http.StatusTooManyRequests, gin.H{
		"error": gin.H{
			"message": "rate limit exceeded",
			"type":    "rate_limit_error",
		},
	})
}

func applySimulatedCacheHeaders(c *gin.Context, businessHit bool, clientHit bool) {
	c.Header("X-Business-Cache-Simulated", cacheDirectiveHeaderValue(businessHit))
	c.Header("X-Client-Cache-Simulated", cacheDirectiveHeaderValue(clientHit))
}

func cacheDirectiveHeaderValue(hit bool) string {
	if hit {
		return "HIT"
	}
	return "MISS"
}

type cacheDirective int

const (
	cacheDirectiveAuto cacheDirective = iota
	cacheDirectiveHit
	cacheDirectiveMiss
)

func parseCacheDirective(value string) cacheDirective {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "hit", "yes", "on":
		return cacheDirectiveHit
	case "0", "false", "miss", "no", "off":
		return cacheDirectiveMiss
	default:
		return cacheDirectiveAuto
	}
}

func proxyGeminiModels(c *gin.Context) {
	resolved := resolveState(c, nil)
	if resolved == nil {
		return
	}
	instance.GeminiModelsHandler(c, resolved.State)
}

func proxyGeminiContent(c *gin.Context) {
	isPool, _ := c.Get("isPool")
	maxAttempts := 1
	if isPool == true {
		maxAttempts = 3
	}

	model, stream, ok := parseGeminiAction(c.Param("action"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "unsupported Gemini endpoint"})
		return
	}
	operation := c.FullPath() + "|" + c.Param("action")

	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}

	exclude := make(map[string]bool)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		resolved := resolveState(c, exclude)
		if resolved == nil {
			return
		}

		businessCacheHit := resolveBusinessCacheHit(c, resolved.AccountID, bodyBytes)
		clientCacheHit := resolveClientCacheHit(c, resolved.AccountID, bodyBytes)
		if tryServeCachedResponseWithOperation(c, resolved.AccountID, operation, bodyBytes, businessCacheHit, clientCacheHit) {
			return
		}
		allowed, retryAfter := checkRateLimit(resolved.AccountID)
		if !allowed {
			instance.RecordRequest(resolved.AccountID, instance.RequestUsageEvent{
				Failed:           true,
				Is429:            true,
				BusinessCacheHit: businessCacheHit,
				ClientCacheHit:   clientCacheHit,
			})
			if attempt < maxAttempts-1 {
				exclude[resolved.AccountID] = true
				log.Printf("Local rate limit hit for account %s, retrying with different account", resolved.AccountID)
				continue
			}
			applySimulatedCacheHeaders(c, businessCacheHit, clientCacheHit)
			writeRateLimitResponse(c, retryAfter)
			return
		}

		resp, proxyErr := instance.DoGeminiProxy(c, resolved.State, model, bodyBytes, stream)
		if proxyErr != nil {
			if resp != nil {
				_ = resp.Body.Close()
			}
			instance.RecordRequest(resolved.AccountID, instance.RequestUsageEvent{
				Failed:           true,
				BusinessCacheHit: businessCacheHit,
				ClientCacheHit:   clientCacheHit,
			})
			if attempt < maxAttempts-1 {
				exclude[resolved.AccountID] = true
				log.Printf("Gemini proxy error for account %s, retrying: %v", resolved.AccountID, proxyErr)
				continue
			}
			applySimulatedCacheHeaders(c, businessCacheHit, clientCacheHit)
			c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("proxy request failed: %v", proxyErr)})
			return
		}

		if isRetryableStatus(resp.StatusCode) && attempt < maxAttempts-1 {
			is429 := resp.StatusCode == http.StatusTooManyRequests
			instance.RecordRequest(resolved.AccountID, instance.RequestUsageEvent{
				Failed:           true,
				Is429:            is429,
				BusinessCacheHit: businessCacheHit,
				ClientCacheHit:   clientCacheHit,
			})
			_ = resp.Body.Close()
			exclude[resolved.AccountID] = true
			log.Printf("Upstream returned %d for account %s, retrying with different account", resp.StatusCode, resolved.AccountID)
			continue
		}

		clientResponse, buildErr := instance.BuildGeminiClientResponse(resp, stream)
		if buildErr != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("failed to build response: %v", buildErr)})
			return
		}
		instance.RecordRequest(resolved.AccountID, instance.RequestUsageEvent{
			Failed:           isRetryableStatus(resp.StatusCode),
			Is429:            resp.StatusCode == http.StatusTooManyRequests,
			BusinessCacheHit: businessCacheHit,
			ClientCacheHit:   clientCacheHit,
		})
		storeCachedResponseWithOperation(resolved.AccountID, operation, bodyBytes, clientResponse)
		applySimulatedCacheHeaders(c, businessCacheHit, clientCacheHit)
		instance.WriteCachedResponse(c, clientResponse)
		return
	}
}

func parseGeminiAction(action string) (string, bool, bool) {
	trimmed := strings.TrimPrefix(action, "/")
	parts := strings.SplitN(trimmed, ":", 2)
	if len(parts) != 2 || parts[0] == "" {
		return "", false, false
	}
	switch parts[1] {
	case "generateContent":
		return parts[0], false, true
	case "streamGenerateContent":
		return parts[0], true, true
	default:
		return "", false, false
	}
}

func tryServeCachedResponse(c *gin.Context, accountID string, bodyBytes []byte, businessHit bool, clientHit bool) bool {
	return tryServeCachedResponseWithOperation(c, accountID, c.FullPath(), bodyBytes, businessHit, clientHit)
}

func tryServeCachedResponseWithOperation(c *gin.Context, accountID string, operation string, bodyBytes []byte, businessHit bool, clientHit bool) bool {
	if !businessHit && !clientHit {
		return false
	}
	cached, ok := instance.GetCachedResponse(accountID, operation, bodyBytes)
	if !ok {
		return false
	}
	applySimulatedCacheHeaders(c, businessHit, clientHit)
	instance.RecordRequest(accountID, instance.RequestUsageEvent{
		BusinessCacheHit: businessHit,
		ClientCacheHit:   clientHit,
	})
	instance.WriteCachedResponse(c, cached)
	return true
}

func storeCachedResponse(accountID string, c *gin.Context, bodyBytes []byte, response *instance.CachedResponse) {
	storeCachedResponseWithOperation(accountID, c.FullPath(), bodyBytes, response)
}

func storeCachedResponseWithOperation(accountID string, operation string, bodyBytes []byte, response *instance.CachedResponse) {
	if response == nil || response.StatusCode != http.StatusOK {
		return
	}
	instance.StoreCachedResponse(accountID, operation, bodyBytes, response)
}

func resolveBusinessCacheHit(c *gin.Context, accountID string, bodyBytes []byte) bool {
	switch parseCacheDirective(c.GetHeader("X-Business-Cache-Hit")) {
	case cacheDirectiveHit:
		return true
	case cacheDirectiveMiss:
		return false
	default:
		return instance.DetectBusinessCacheHit(accountID, c.FullPath(), bodyBytes)
	}
}

func resolveClientCacheHit(c *gin.Context, accountID string, bodyBytes []byte) bool {
	switch parseCacheDirective(c.GetHeader("X-Client-Cache-Hit")) {
	case cacheDirectiveHit:
		return true
	case cacheDirectiveMiss:
		return false
	default:
		return instance.DetectClientCacheHit(accountID, c.FullPath(), bodyBytes)
	}
}

func proxyCompletions(c *gin.Context) {
	isPool, _ := c.Get("isPool")
	maxAttempts := 1
	if isPool == true {
		maxAttempts = 3
	}

	// Read body once for potential retries.
	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}

	exclude := make(map[string]bool)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		resolved := resolveState(c, exclude)
		if resolved == nil {
			return // resolveState already wrote the error response
		}

		businessCacheHit := resolveBusinessCacheHit(c, resolved.AccountID, bodyBytes)
		clientCacheHit := resolveClientCacheHit(c, resolved.AccountID, bodyBytes)
		if tryServeCachedResponse(c, resolved.AccountID, bodyBytes, businessCacheHit, clientCacheHit) {
			return
		}
		allowed, retryAfter := checkRateLimit(resolved.AccountID)
		if !allowed {
			instance.RecordRequest(resolved.AccountID, instance.RequestUsageEvent{
				Failed:           true,
				Is429:            true,
				BusinessCacheHit: businessCacheHit,
				ClientCacheHit:   clientCacheHit,
			})
			if attempt < maxAttempts-1 {
				exclude[resolved.AccountID] = true
				log.Printf("Local rate limit hit for account %s, retrying with different account", resolved.AccountID)
				continue
			}
			applySimulatedCacheHeaders(c, businessCacheHit, clientCacheHit)
			writeRateLimitResponse(c, retryAfter)
			return
		}

		resp, proxyErr := instance.DoCompletionsProxy(c, resolved.State, bodyBytes)
		if proxyErr != nil {
			if resp != nil {
				_ = resp.Body.Close()
			}
			instance.RecordRequest(resolved.AccountID, instance.RequestUsageEvent{
				Failed:           true,
				BusinessCacheHit: businessCacheHit,
				ClientCacheHit:   clientCacheHit,
			})
			if attempt < maxAttempts-1 {
				exclude[resolved.AccountID] = true
				log.Printf("Completions proxy error for account %s, retrying: %v", resolved.AccountID, proxyErr)
				continue
			}
			applySimulatedCacheHeaders(c, businessCacheHit, clientCacheHit)
			c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("proxy request failed: %v", proxyErr)})
			return
		}

		// Check if retryable.
		if isRetryableStatus(resp.StatusCode) && attempt < maxAttempts-1 {
			is429 := resp.StatusCode == http.StatusTooManyRequests
			instance.RecordRequest(resolved.AccountID, instance.RequestUsageEvent{
				Failed:           true,
				Is429:            is429,
				BusinessCacheHit: businessCacheHit,
				ClientCacheHit:   clientCacheHit,
			})
			_ = resp.Body.Close()
			exclude[resolved.AccountID] = true
			log.Printf("Upstream returned %d for account %s, retrying with different account", resp.StatusCode, resolved.AccountID)
			continue
		}

		clientResponse, buildErr := instance.BuildCompletionsClientResponse(resp)
		if buildErr != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("failed to build response: %v", buildErr)})
			return
		}
		instance.RecordRequest(resolved.AccountID, instance.RequestUsageEvent{
			Failed:           isRetryableStatus(resp.StatusCode),
			Is429:            resp.StatusCode == http.StatusTooManyRequests,
			BusinessCacheHit: businessCacheHit,
			ClientCacheHit:   clientCacheHit,
		})
		storeCachedResponse(resolved.AccountID, c, bodyBytes, clientResponse)
		applySimulatedCacheHeaders(c, businessCacheHit, clientCacheHit)
		instance.WriteCachedResponse(c, clientResponse)
		return
	}
}

func proxyModels(c *gin.Context) {
	resolved := resolveState(c, nil)
	if resolved == nil {
		return
	}
	instance.ModelsHandler(c, resolved.State)
}

func proxyEmbeddings(c *gin.Context) {
	isPool, _ := c.Get("isPool")
	maxAttempts := 1
	if isPool == true {
		maxAttempts = 3
	}

	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}

	exclude := make(map[string]bool)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		resolved := resolveState(c, exclude)
		if resolved == nil {
			return
		}

		businessCacheHit := resolveBusinessCacheHit(c, resolved.AccountID, bodyBytes)
		clientCacheHit := resolveClientCacheHit(c, resolved.AccountID, bodyBytes)
		if tryServeCachedResponse(c, resolved.AccountID, bodyBytes, businessCacheHit, clientCacheHit) {
			return
		}
		allowed, retryAfter := checkRateLimit(resolved.AccountID)
		if !allowed {
			instance.RecordRequest(resolved.AccountID, instance.RequestUsageEvent{
				Failed:           true,
				Is429:            true,
				BusinessCacheHit: businessCacheHit,
				ClientCacheHit:   clientCacheHit,
			})
			if attempt < maxAttempts-1 {
				exclude[resolved.AccountID] = true
				log.Printf("Local rate limit hit for account %s, retrying with different account", resolved.AccountID)
				continue
			}
			applySimulatedCacheHeaders(c, businessCacheHit, clientCacheHit)
			writeRateLimitResponse(c, retryAfter)
			return
		}

		resp, proxyErr := instance.DoEmbeddingsProxy(resolved.State, bodyBytes)
		if proxyErr != nil {
			if resp != nil {
				_ = resp.Body.Close()
			}
			instance.RecordRequest(resolved.AccountID, instance.RequestUsageEvent{
				Failed:           true,
				BusinessCacheHit: businessCacheHit,
				ClientCacheHit:   clientCacheHit,
			})
			if attempt < maxAttempts-1 {
				exclude[resolved.AccountID] = true
				log.Printf("Embeddings proxy error for account %s, retrying: %v", resolved.AccountID, proxyErr)
				continue
			}
			applySimulatedCacheHeaders(c, businessCacheHit, clientCacheHit)
			c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("proxy request failed: %v", proxyErr)})
			return
		}

		if isRetryableStatus(resp.StatusCode) && attempt < maxAttempts-1 {
			is429 := resp.StatusCode == http.StatusTooManyRequests
			instance.RecordRequest(resolved.AccountID, instance.RequestUsageEvent{
				Failed:           true,
				Is429:            is429,
				BusinessCacheHit: businessCacheHit,
				ClientCacheHit:   clientCacheHit,
			})
			_ = resp.Body.Close()
			exclude[resolved.AccountID] = true
			log.Printf("Upstream returned %d for account %s, retrying with different account", resp.StatusCode, resolved.AccountID)
			continue
		}

		clientResponse, buildErr := instance.BuildEmbeddingsClientResponse(resp)
		if buildErr != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("failed to build response: %v", buildErr)})
			return
		}
		instance.RecordRequest(resolved.AccountID, instance.RequestUsageEvent{
			Failed:           isRetryableStatus(resp.StatusCode),
			Is429:            resp.StatusCode == http.StatusTooManyRequests,
			BusinessCacheHit: businessCacheHit,
			ClientCacheHit:   clientCacheHit,
		})
		storeCachedResponse(resolved.AccountID, c, bodyBytes, clientResponse)
		applySimulatedCacheHeaders(c, businessCacheHit, clientCacheHit)
		instance.WriteCachedResponse(c, clientResponse)
		return
	}
}

func proxyMessages(c *gin.Context) {
	isPool, _ := c.Get("isPool")
	maxAttempts := 1
	if isPool == true {
		maxAttempts = 3
	}

	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}

	exclude := make(map[string]bool)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		resolved := resolveState(c, exclude)
		if resolved == nil {
			return
		}

		businessCacheHit := resolveBusinessCacheHit(c, resolved.AccountID, bodyBytes)
		clientCacheHit := resolveClientCacheHit(c, resolved.AccountID, bodyBytes)
		if tryServeCachedResponse(c, resolved.AccountID, bodyBytes, businessCacheHit, clientCacheHit) {
			return
		}
		allowed, retryAfter := checkRateLimit(resolved.AccountID)
		if !allowed {
			instance.RecordRequest(resolved.AccountID, instance.RequestUsageEvent{
				Failed:           true,
				Is429:            true,
				BusinessCacheHit: businessCacheHit,
				ClientCacheHit:   clientCacheHit,
			})
			if attempt < maxAttempts-1 {
				exclude[resolved.AccountID] = true
				log.Printf("Local rate limit hit for account %s, retrying with different account", resolved.AccountID)
				continue
			}
			applySimulatedCacheHeaders(c, businessCacheHit, clientCacheHit)
			writeRateLimitResponse(c, retryAfter)
			return
		}

		resp, proxyErr := instance.DoMessagesProxy(c, resolved.State, bodyBytes)
		if proxyErr != nil {
			if resp != nil {
				_ = resp.Body.Close()
			}
			instance.RecordRequest(resolved.AccountID, instance.RequestUsageEvent{
				Failed:           true,
				BusinessCacheHit: businessCacheHit,
				ClientCacheHit:   clientCacheHit,
			})
			if attempt < maxAttempts-1 {
				exclude[resolved.AccountID] = true
				log.Printf("Messages proxy error for account %s, retrying: %v", resolved.AccountID, proxyErr)
				continue
			}
			applySimulatedCacheHeaders(c, businessCacheHit, clientCacheHit)
			c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("proxy request failed: %v", proxyErr)})
			return
		}

		if isRetryableStatus(resp.StatusCode) && attempt < maxAttempts-1 {
			is429 := resp.StatusCode == http.StatusTooManyRequests
			instance.RecordRequest(resolved.AccountID, instance.RequestUsageEvent{
				Failed:           true,
				Is429:            is429,
				BusinessCacheHit: businessCacheHit,
				ClientCacheHit:   clientCacheHit,
			})
			_ = resp.Body.Close()
			exclude[resolved.AccountID] = true
			log.Printf("Upstream returned %d for account %s, retrying with different account", resp.StatusCode, resolved.AccountID)
			continue
		}

		clientResponse, buildErr := instance.BuildMessagesClientResponse(resp, bodyBytes)
		if buildErr != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("failed to build response: %v", buildErr)})
			return
		}
		instance.RecordRequest(resolved.AccountID, instance.RequestUsageEvent{
			Failed:           isRetryableStatus(resp.StatusCode),
			Is429:            resp.StatusCode == http.StatusTooManyRequests,
			BusinessCacheHit: businessCacheHit,
			ClientCacheHit:   clientCacheHit,
		})
		storeCachedResponse(resolved.AccountID, c, bodyBytes, clientResponse)
		applySimulatedCacheHeaders(c, businessCacheHit, clientCacheHit)
		instance.WriteCachedResponse(c, clientResponse)
		return
	}
}

func proxyCountTokens(c *gin.Context) {
	resolved := resolveState(c, nil)
	if resolved == nil {
		return
	}
	instance.CountTokensHandler(c, resolved.State)
}

func proxyResponses(c *gin.Context) {
	isPool, _ := c.Get("isPool")
	maxAttempts := 1
	if isPool == true {
		maxAttempts = 3
	}

	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}

	exclude := make(map[string]bool)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		resolved := resolveState(c, exclude)
		if resolved == nil {
			return
		}

		businessCacheHit := resolveBusinessCacheHit(c, resolved.AccountID, bodyBytes)
		clientCacheHit := resolveClientCacheHit(c, resolved.AccountID, bodyBytes)
		if tryServeCachedResponse(c, resolved.AccountID, bodyBytes, businessCacheHit, clientCacheHit) {
			return
		}
		allowed, retryAfter := checkRateLimit(resolved.AccountID)
		if !allowed {
			instance.RecordRequest(resolved.AccountID, instance.RequestUsageEvent{
				Failed:           true,
				Is429:            true,
				BusinessCacheHit: businessCacheHit,
				ClientCacheHit:   clientCacheHit,
			})
			if attempt < maxAttempts-1 {
				exclude[resolved.AccountID] = true
				log.Printf("Local rate limit hit for account %s, retrying with different account", resolved.AccountID)
				continue
			}
			applySimulatedCacheHeaders(c, businessCacheHit, clientCacheHit)
			writeRateLimitResponse(c, retryAfter)
			return
		}

		resp, proxyErr := instance.DoResponsesProxy(resolved.State, bodyBytes)
		if proxyErr != nil {
			if resp != nil {
				_ = resp.Body.Close()
			}
			instance.RecordRequest(resolved.AccountID, instance.RequestUsageEvent{
				Failed:           true,
				BusinessCacheHit: businessCacheHit,
				ClientCacheHit:   clientCacheHit,
			})
			if attempt < maxAttempts-1 {
				exclude[resolved.AccountID] = true
				log.Printf("Responses proxy error for account %s, retrying: %v", resolved.AccountID, proxyErr)
				continue
			}
			applySimulatedCacheHeaders(c, businessCacheHit, clientCacheHit)
			c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("proxy request failed: %v", proxyErr)})
			return
		}

		if isRetryableStatus(resp.StatusCode) && attempt < maxAttempts-1 {
			is429 := resp.StatusCode == http.StatusTooManyRequests
			instance.RecordRequest(resolved.AccountID, instance.RequestUsageEvent{
				Failed:           true,
				Is429:            is429,
				BusinessCacheHit: businessCacheHit,
				ClientCacheHit:   clientCacheHit,
			})
			_ = resp.Body.Close()
			exclude[resolved.AccountID] = true
			log.Printf("Upstream returned %d for account %s, retrying with different account", resp.StatusCode, resolved.AccountID)
			continue
		}

		clientResponse, buildErr := instance.BuildResponsesClientResponse(resp)
		if buildErr != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("failed to build response: %v", buildErr)})
			return
		}
		instance.RecordRequest(resolved.AccountID, instance.RequestUsageEvent{
			Failed:           isRetryableStatus(resp.StatusCode),
			Is429:            resp.StatusCode == http.StatusTooManyRequests,
			BusinessCacheHit: businessCacheHit,
			ClientCacheHit:   clientCacheHit,
		})
		storeCachedResponse(resolved.AccountID, c, bodyBytes, clientResponse)
		applySimulatedCacheHeaders(c, businessCacheHit, clientCacheHit)
		instance.WriteCachedResponse(c, clientResponse)
		return
	}
}
