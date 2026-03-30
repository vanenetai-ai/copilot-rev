package instance

import (
	"encoding/json"
	"net/http"

	"copilot-go/config"
	"copilot-go/store"

	"github.com/gin-gonic/gin"
)

// DoResponsesProxy forwards requests directly to GitHub Copilot /responses endpoint.
func DoResponsesProxy(state *config.State, bodyBytes []byte) (*http.Response, error) {
	// Convert model ID
	var payload map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &payload); err == nil {
		if model, ok := payload["model"].(string); ok {
			payload["model"] = store.ToCopilotID(model)
			bodyBytes, _ = json.Marshal(payload)
		}
	}

	extraHeaders := make(http.Header)
	extraHeaders.Set("X-Initiator", "user")

	return ProxyRequestWithBytes(state, "POST", "/responses", bodyBytes, extraHeaders, false)
}

// ForwardResponsesResponse forwards the upstream response directly to client.
func ForwardResponsesResponse(c *gin.Context, resp *http.Response) {
	clientResponse, err := BuildResponsesClientResponse(resp)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read response"})
		return
	}
	WriteCachedResponse(c, clientResponse)
}

func BuildResponsesClientResponse(resp *http.Response) (*CachedResponse, error) {
	return buildRawClientResponse(resp)
}
