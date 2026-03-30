package instance

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

type CachedResponse struct {
	StatusCode  int
	ContentType string
	Body        []byte
	IsStream    bool
	StoredAt    time.Time
}

var (
	responseCache   = make(map[string]*CachedResponse)
	responseCacheMu sync.RWMutex
	responseCacheTTLSeconds atomic.Int64
)

func init() {
	responseCacheTTLSeconds.Store(int64((5 * time.Minute) / time.Second))
}

func GetCachedResponse(accountID, operation string, bodyBytes []byte) (*CachedResponse, bool) {
	key := buildResponseCacheKey(accountID, operation, bodyBytes)
	now := time.Now()
	ttl := getResponseCacheTTL()

	responseCacheMu.RLock()
	cached, ok := responseCache[key]
	responseCacheMu.RUnlock()
	if !ok {
		return nil, false
	}
	if now.Sub(cached.StoredAt) > ttl {
		responseCacheMu.Lock()
		delete(responseCache, key)
		responseCacheMu.Unlock()
		return nil, false
	}
	return cloneCachedResponse(cached), true
}

func StoreCachedResponse(accountID, operation string, bodyBytes []byte, response *CachedResponse) {
	if response == nil || response.StatusCode != http.StatusOK || len(response.Body) == 0 {
		return
	}
	key := buildResponseCacheKey(accountID, operation, bodyBytes)

	responseCacheMu.Lock()
	defer responseCacheMu.Unlock()
	pruneExpiredResponsesLocked(time.Now())
	responseCache[key] = cloneCachedResponse(response)
}

func WriteCachedResponse(c *gin.Context, response *CachedResponse) {
	if response == nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "cached response unavailable"})
		return
	}
	contentType := response.ContentType
	if contentType == "" {
		if response.IsStream {
			contentType = "text/event-stream"
		} else {
			contentType = "application/json"
		}
	}
	if response.IsStream {
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")
	}
	c.Data(response.StatusCode, contentType, response.Body)
}

func buildResponseCacheKey(accountID, operation string, bodyBytes []byte) string {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(accountID))
	_, _ = hasher.Write([]byte{0})
	_, _ = hasher.Write([]byte(operation))
	_, _ = hasher.Write([]byte{0})
	_, _ = hasher.Write(bodyBytes)
	return hex.EncodeToString(hasher.Sum(nil))
}

func cloneCachedResponse(response *CachedResponse) *CachedResponse {
	return &CachedResponse{
		StatusCode:  response.StatusCode,
		ContentType: response.ContentType,
		Body:        append([]byte(nil), response.Body...),
		IsStream:    response.IsStream,
		StoredAt:    response.StoredAt,
	}
}

func pruneExpiredResponsesLocked(now time.Time) {
	ttl := getResponseCacheTTL()
	for key, cached := range responseCache {
		if now.Sub(cached.StoredAt) > ttl {
			delete(responseCache, key)
		}
	}
}

func SetResponseCacheTTLSeconds(ttlSeconds int) {
	if ttlSeconds <= 0 {
		ttlSeconds = 300
	}
	responseCacheTTLSeconds.Store(int64(ttlSeconds))
	responseCacheMu.Lock()
	pruneExpiredResponsesLocked(time.Now())
	responseCacheMu.Unlock()
}

func GetResponseCacheTTLSeconds() int {
	return int(responseCacheTTLSeconds.Load())
}

func getResponseCacheTTL() time.Duration {
	ttlSeconds := responseCacheTTLSeconds.Load()
	if ttlSeconds <= 0 {
		ttlSeconds = 300
	}
	return time.Duration(ttlSeconds) * time.Second
}
