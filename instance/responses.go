package instance

import (
	"encoding/json"
	"net/http"
	"sync/atomic"

	"copilot-go/config"
	"copilot-go/store"

	"github.com/gin-gonic/gin"
)

var responsesAPIWebSearchEnabled atomic.Bool
var responsesFunctionApplyPatchEnabled atomic.Bool

func init() {
	responsesAPIWebSearchEnabled.Store(true)
	responsesFunctionApplyPatchEnabled.Store(true)
}

// DoResponsesProxy forwards requests directly to GitHub Copilot /responses endpoint.
func DoResponsesProxy(state *config.State, bodyBytes []byte) (*http.Response, error) {
	// Convert model ID
	var payload map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &payload); err == nil {
		if model, ok := payload["model"].(string); ok {
			payload["model"] = store.ToCopilotID(model)
		}
		if responsesFunctionApplyPatchEnabled.Load() {
			applyResponsesFunctionApplyPatch(payload)
		}
		if !responsesAPIWebSearchEnabled.Load() {
			removeResponsesWebSearchTool(payload)
		}
		bodyBytes, _ = json.Marshal(payload)
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

func SetResponsesAPIWebSearchEnabled(enabled bool) {
	responsesAPIWebSearchEnabled.Store(enabled)
}

func SetResponsesFunctionApplyPatchEnabled(enabled bool) {
	responsesFunctionApplyPatchEnabled.Store(enabled)
}

func removeResponsesWebSearchTool(payload map[string]interface{}) {
	rawTools, ok := payload["tools"].([]interface{})
	if !ok || len(rawTools) == 0 {
		return
	}
	filtered := make([]interface{}, 0, len(rawTools))
	for _, rawTool := range rawTools {
		tool, ok := rawTool.(map[string]interface{})
		if !ok {
			filtered = append(filtered, rawTool)
			continue
		}
		if toolType, _ := tool["type"].(string); toolType == "web_search" {
			continue
		}
		filtered = append(filtered, rawTool)
	}
	payload["tools"] = filtered
}

func applyResponsesFunctionApplyPatch(payload map[string]interface{}) {
	rawTools, ok := payload["tools"].([]interface{})
	if !ok || len(rawTools) == 0 {
		return
	}
	for index, rawTool := range rawTools {
		tool, ok := rawTool.(map[string]interface{})
		if !ok {
			continue
		}
		toolType, _ := tool["type"].(string)
		toolName, _ := tool["name"].(string)
		if toolType != "custom" || toolName != "apply_patch" {
			continue
		}
		rawTools[index] = map[string]interface{}{
			"type":        "function",
			"name":        "apply_patch",
			"description": "Use the apply_patch tool to edit files",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"input": map[string]interface{}{
						"type":        "string",
						"description": "The entire contents of the apply_patch command",
					},
				},
				"required": []string{"input"},
			},
			"strict": false,
		}
	}
	payload["tools"] = rawTools
}
