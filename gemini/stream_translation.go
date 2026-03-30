package gemini

import "copilot-go/anthropic"

func TranslateChunkFromOpenAI(chunk anthropic.ChatCompletionResponse) *GenerateContentResponse {
	if len(chunk.Choices) == 0 {
		return nil
	}

	candidates := make([]Candidate, 0, len(chunk.Choices))
	for _, choice := range chunk.Choices {
		if choice.Delta == nil {
			continue
		}
		candidate := Candidate{
			Index:        choice.Index,
			FinishReason: translateFinishReason(choice.FinishReason),
			Content: Content{
				Role:  "model",
				Parts: make([]Part, 0, 2),
			},
		}
		if choice.Delta.Content != "" {
			candidate.Content.Parts = append(candidate.Content.Parts, Part{Text: choice.Delta.Content})
		}
		for _, toolCall := range choice.Delta.ToolCalls {
			candidate.Content.Parts = append(candidate.Content.Parts, Part{
				FunctionCall: &FunctionCall{
					Name: toolCall.Function.Name,
					Args: parseFunctionArguments(toolCall.Function.Arguments),
				},
			})
		}
		if len(candidate.Content.Parts) == 0 && candidate.FinishReason == "" {
			continue
		}
		candidates = append(candidates, candidate)
	}

	if len(candidates) == 0 {
		return nil
	}
	response := &GenerateContentResponse{
		Candidates:   candidates,
		ModelVersion: chunk.Model,
	}
	if chunk.Usage != nil {
		response.UsageMetadata = &UsageMetadata{
			PromptTokenCount:     chunk.Usage.PromptTokens,
			CandidatesTokenCount: chunk.Usage.CompletionTokens,
			TotalTokenCount:      chunk.Usage.TotalTokens,
		}
	}
	return response
}
