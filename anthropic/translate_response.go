package anthropic

import (
	"fmt"
	"time"
)

// TranslateToAnthropic converts an OpenAI chat completion response to Anthropic format.
func TranslateToAnthropic(resp ChatCompletionResponse) AnthropicResponse {
	var content []AnthropicContentBlock
	stopReason := "end_turn"

	for _, choice := range resp.Choices {
		msg := choice.Message
		if msg == nil {
			continue
		}

		if choice.FinishReason != nil {
			stopReason = MapOpenAIStopReasonToAnthropic(*choice.FinishReason)
		}

		if msg.Content != "" {
			content = append(content, AnthropicContentBlock{
				Type: "text",
				Text: msg.Content,
			})
		}

		for _, tc := range msg.ToolCalls {
			var input interface{}
			if tc.Function.Arguments != "" {
				// Parse arguments as JSON
				input = parseJSONSafe(tc.Function.Arguments)
			}
			content = append(content, AnthropicContentBlock{
				Type:  "tool_use",
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: input,
			})
		}
	}

	if len(content) == 0 {
		content = append(content, AnthropicContentBlock{
			Type: "text",
			Text: "",
		})
	}

	usage := AnthropicUsage{}
	if resp.Usage != nil {
		cachedTokens := 0
		if resp.Usage.PromptTokensDetails != nil {
			cachedTokens = resp.Usage.PromptTokensDetails.CachedTokens
		}
		usage.InputTokens = maxInt(resp.Usage.PromptTokens-cachedTokens, 0)
		usage.OutputTokens = resp.Usage.CompletionTokens
		if cachedTokens > 0 {
			usage.CacheReadInputTokens = cachedTokens
		}
	}

	id := resp.ID
	if id == "" {
		id = fmt.Sprintf("msg_%d", time.Now().UnixNano())
	}

	return AnthropicResponse{
		ID:         id,
		Type:       "message",
		Role:       "assistant",
		Content:    content,
		Model:      resp.Model,
		StopReason: stopReason,
		Usage:      usage,
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func parseJSONSafe(s string) interface{} {
	var result interface{}
	if err := jsonUnmarshal([]byte(s), &result); err != nil {
		return map[string]interface{}{}
	}
	return result
}
