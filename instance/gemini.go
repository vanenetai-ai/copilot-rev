package instance

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"copilot-go/anthropic"
	"copilot-go/config"
	"copilot-go/gemini"
	"copilot-go/store"

	"github.com/gin-gonic/gin"
)

func DoGeminiProxy(c *gin.Context, state *config.State, model string, bodyBytes []byte, stream bool) (*http.Response, error) {
	translated, err := gemini.TranslateToOpenAI(model, bodyBytes, stream)
	if err != nil {
		return nil, err
	}
	return DoCompletionsProxy(c, state, translated)
}

func BuildGeminiClientResponse(resp *http.Response, stream bool) (*CachedResponse, error) {
	defer func() { _ = resp.Body.Close() }()

	if stream {
		return buildGeminiStreamResponse(resp)
	}
	return buildGeminiNonStreamResponse(resp)
}

func GeminiModelsHandler(c *gin.Context, state *config.State) {
	state.RLock()
	models := state.Models
	state.RUnlock()

	result := gemini.ModelsListResponse{
		Models: make([]gemini.Model, 0),
	}
	if models == nil {
		c.JSON(http.StatusOK, result)
		return
	}

	for _, m := range models.Data {
		displayID := store.ToDisplayID(m.ID)
		result.Models = append(result.Models, gemini.Model{
			Name:        fmt.Sprintf("models/%s", displayID),
			Version:     m.Version,
			DisplayName: firstNonEmpty(m.Name, displayID),
			Description: firstNonEmpty(m.Vendor, m.OwnedBy),
			InputTokenLimit: func() int {
				if m.Capabilities == nil {
					return 0
				}
				if m.Capabilities.Limits.MaxPromptTokens > 0 {
					return m.Capabilities.Limits.MaxPromptTokens
				}
				return m.Capabilities.Limits.MaxContextWindow
			}(),
			OutputTokenLimit: func() int {
				if m.Capabilities == nil {
					return 0
				}
				return m.Capabilities.Limits.MaxOutputTokens
			}(),
			SupportedGenerationMethods: []string{
				"generateContent",
				"streamGenerateContent",
			},
		})
	}

	c.JSON(http.StatusOK, result)
}

func buildGeminiNonStreamResponse(resp *http.Response) (*CachedResponse, error) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return &CachedResponse{
			StatusCode:  resp.StatusCode,
			ContentType: "application/json",
			Body:        body,
			StoredAt:    time.Now(),
		}, nil
	}

	var openAIResp anthropic.ChatCompletionResponse
	if err := json.Unmarshal(body, &openAIResp); err != nil {
		return nil, fmt.Errorf("failed to parse upstream response")
	}

	geminiResp := gemini.TranslateFromOpenAI(openAIResp)
	clientBody, err := json.Marshal(geminiResp)
	if err != nil {
		return nil, err
	}

	return &CachedResponse{
		StatusCode:  http.StatusOK,
		ContentType: "application/json",
		Body:        clientBody,
		StoredAt:    time.Now(),
	}, nil
}

func buildGeminiStreamResponse(resp *http.Response) (*CachedResponse, error) {
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return &CachedResponse{
			StatusCode:  resp.StatusCode,
			ContentType: "application/json",
			Body:        body,
			StoredAt:    time.Now(),
		}, nil
	}

	var buffer bytes.Buffer
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 10*1024*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			return &CachedResponse{
				StatusCode:  http.StatusOK,
				ContentType: "text/event-stream",
				Body:        buffer.Bytes(),
				IsStream:    true,
				StoredAt:    time.Now(),
			}, nil
		}

		var chunk anthropic.ChatCompletionResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		translated := gemini.TranslateChunkFromOpenAI(chunk)
		if translated == nil {
			continue
		}
		if err := writeGeminiSSE(&buffer, translated); err != nil {
			return nil, err
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return &CachedResponse{
		StatusCode:  http.StatusOK,
		ContentType: "text/event-stream",
		Body:        buffer.Bytes(),
		IsStream:    true,
		StoredAt:    time.Now(),
	}, nil
}

func writeGeminiSSE(w io.Writer, data interface{}) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", string(jsonData))
	return err
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
