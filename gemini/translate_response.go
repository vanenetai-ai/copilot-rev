package gemini

import (
	"encoding/json"
	"strings"

	"copilot-go/anthropic"
	"copilot-go/store"
)

func TranslateFromOpenAI(resp anthropic.ChatCompletionResponse) GenerateContentResponse {
	result := GenerateContentResponse{
		Candidates: make([]Candidate, 0, len(resp.Choices)),
	}
	if resp.Usage != nil {
		result.UsageMetadata = &UsageMetadata{
			PromptTokenCount:     resp.Usage.PromptTokens,
			CandidatesTokenCount: resp.Usage.CompletionTokens,
			TotalTokenCount:      resp.Usage.TotalTokens,
		}
	}
	if resp.Model != "" {
		result.ModelVersion = store.ToDisplayID(resp.Model)
	}

	for _, choice := range resp.Choices {
		candidate := Candidate{
			Index:        choice.Index,
			FinishReason: translateFinishReason(choice.FinishReason),
			Content: Content{
				Role:  "model",
				Parts: make([]Part, 0, 2),
			},
		}
		if choice.Message != nil {
			if choice.Message.Content != "" {
				candidate.Content.Parts = append(candidate.Content.Parts, Part{Text: choice.Message.Content})
			}
			for _, toolCall := range choice.Message.ToolCalls {
				candidate.Content.Parts = append(candidate.Content.Parts, Part{
					FunctionCall: &FunctionCall{
						Name: toolCall.Function.Name,
						Args: parseFunctionArguments(toolCall.Function.Arguments),
					},
				})
			}
		}
		result.Candidates = append(result.Candidates, candidate)
	}
	return result
}

func translateFinishReason(reason *string) string {
	if reason == nil {
		return ""
	}
	switch strings.ToLower(*reason) {
	case "stop", "tool_calls":
		return "STOP"
	case "length":
		return "MAX_TOKENS"
	case "content_filter":
		return "SAFETY"
	default:
		return strings.ToUpper(*reason)
	}
}

func parseFunctionArguments(arguments string) interface{} {
	if arguments == "" {
		return map[string]interface{}{}
	}
	var parsed interface{}
	if err := json.Unmarshal([]byte(arguments), &parsed); err == nil {
		return parsed
	}
	return map[string]interface{}{"raw": arguments}
}
