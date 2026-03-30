package gemini

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"copilot-go/anthropic"
	"copilot-go/store"
)

func TranslateToOpenAI(model string, bodyBytes []byte, stream bool) ([]byte, error) {
	var payload GenerateContentRequest
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		return nil, fmt.Errorf("invalid request: %v", err)
	}

	openAI := anthropic.ChatCompletionsPayload{
		Model:    store.ToCopilotID(model),
		Messages: make([]anthropic.OpenAIMessage, 0, len(payload.Contents)+1),
		Stream:   stream,
		N:        1,
	}

	if payload.SystemInstruction != nil {
		systemContent, err := convertPartsToMessageContent(payload.SystemInstruction.Parts)
		if err != nil {
			return nil, err
		}
		openAI.Messages = append(openAI.Messages, anthropic.OpenAIMessage{
			Role:    "system",
			Content: systemContent,
		})
	}

	for _, content := range payload.Contents {
		messages, err := convertContentToMessages(content)
		if err != nil {
			return nil, err
		}
		openAI.Messages = append(openAI.Messages, messages...)
	}

	if payload.GenerationConfig != nil {
		openAI.Temperature = payload.GenerationConfig.Temperature
		openAI.TopP = payload.GenerationConfig.TopP
		openAI.MaxTokens = payload.GenerationConfig.MaxOutputTokens
		if len(payload.GenerationConfig.StopSequences) == 1 {
			openAI.Stop = payload.GenerationConfig.StopSequences[0]
		} else if len(payload.GenerationConfig.StopSequences) > 1 {
			openAI.Stop = payload.GenerationConfig.StopSequences
		}
	}

	openAI.Tools = translateTools(payload.Tools)
	openAI.ToolChoice = translateToolChoice(payload.ToolConfig)

	translated, err := json.Marshal(openAI)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal translated request: %v", err)
	}
	return translated, nil
}

func convertContentToMessages(content Content) ([]anthropic.OpenAIMessage, error) {
	role := normalizeRole(content.Role)
	if role == "tool" {
		return convertFunctionResponses(content.Parts)
	}

	textAndMediaParts := make([]Part, 0, len(content.Parts))
	toolCalls := make([]anthropic.ToolCall, 0)
	for idx, part := range content.Parts {
		if part.FunctionCall != nil {
			arguments, err := json.Marshal(part.FunctionCall.Args)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal function call arguments: %v", err)
			}
			index := idx
			toolCalls = append(toolCalls, anthropic.ToolCall{
				ID:    fmt.Sprintf("gemini-call-%d", idx),
				Type:  "function",
				Index: &index,
				Function: anthropic.FunctionCall{
					Name:      part.FunctionCall.Name,
					Arguments: string(arguments),
				},
			})
			continue
		}
		if part.FunctionResponse != nil {
			role = "tool"
			continue
		}
		textAndMediaParts = append(textAndMediaParts, part)
	}

	messages := make([]anthropic.OpenAIMessage, 0, 1)
	if len(textAndMediaParts) > 0 || (role != "assistant" && len(toolCalls) == 0) {
		messageContent, err := convertPartsToMessageContent(textAndMediaParts)
		if err != nil {
			return nil, err
		}
		messages = append(messages, anthropic.OpenAIMessage{
			Role:    role,
			Content: messageContent,
		})
	}
	if len(toolCalls) > 0 {
		messages = append(messages, anthropic.OpenAIMessage{
			Role:      "assistant",
			ToolCalls: toolCalls,
		})
	}
	return messages, nil
}

func convertFunctionResponses(parts []Part) ([]anthropic.OpenAIMessage, error) {
	messages := make([]anthropic.OpenAIMessage, 0, len(parts))
	for _, part := range parts {
		if part.FunctionResponse == nil {
			continue
		}
		responseBody, err := json.Marshal(part.FunctionResponse.Response)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal function response: %v", err)
		}
		messages = append(messages, anthropic.OpenAIMessage{
			Role:       "tool",
			ToolCallID: part.FunctionResponse.Name,
			Content:    string(responseBody),
		})
	}
	return messages, nil
}

func convertPartsToMessageContent(parts []Part) (interface{}, error) {
	if len(parts) == 0 {
		return "", nil
	}
	if len(parts) == 1 && parts[0].Text != "" && parts[0].InlineData == nil {
		return parts[0].Text, nil
	}

	content := make([]anthropic.OpenAIContentPart, 0, len(parts))
	for _, part := range parts {
		switch {
		case part.Text != "":
			content = append(content, anthropic.OpenAIContentPart{
				Type: "text",
				Text: part.Text,
			})
		case part.InlineData != nil:
			url, err := inlineDataToDataURL(*part.InlineData)
			if err != nil {
				return nil, err
			}
			content = append(content, anthropic.OpenAIContentPart{
				Type: "image_url",
				ImageURL: &anthropic.OpenAIImageURL{
					URL: url,
				},
			})
		}
	}
	if len(content) == 0 {
		return "", nil
	}
	return content, nil
}

func inlineDataToDataURL(inline InlineData) (string, error) {
	if inline.Data == "" {
		return "", fmt.Errorf("inline_data.data is required")
	}
	mimeType := inline.MimeType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	if _, err := base64.StdEncoding.DecodeString(inline.Data); err != nil {
		return "", fmt.Errorf("inline_data.data must be base64 encoded")
	}
	return fmt.Sprintf("data:%s;base64,%s", mimeType, inline.Data), nil
}

func translateTools(tools []Tool) []anthropic.OpenAITool {
	result := make([]anthropic.OpenAITool, 0)
	for _, tool := range tools {
		for _, decl := range tool.FunctionDeclarations {
			result = append(result, anthropic.OpenAITool{
				Type: "function",
				Function: anthropic.OpenAIFunction{
					Name:        decl.Name,
					Description: decl.Description,
					Parameters:  decl.Parameters,
				},
			})
		}
	}
	return result
}

func translateToolChoice(toolConfig *ToolConfig) interface{} {
	if toolConfig == nil || toolConfig.FunctionCallingConfig == nil {
		return nil
	}
	switch strings.ToUpper(toolConfig.FunctionCallingConfig.Mode) {
	case "NONE":
		return "none"
	case "ANY":
		if len(toolConfig.FunctionCallingConfig.AllowedFunctionNames) == 1 {
			return map[string]interface{}{
				"type": "function",
				"function": map[string]string{
					"name": toolConfig.FunctionCallingConfig.AllowedFunctionNames[0],
				},
			}
		}
		return "required"
	default:
		return nil
	}
}

func normalizeRole(role string) string {
	switch strings.ToLower(role) {
	case "model", "assistant":
		return "assistant"
	case "function", "tool":
		return "tool"
	default:
		return "user"
	}
}
