package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

func translateOpenAIResponse(raw []byte, requestedModel string) (map[string]any, error) {
	var in struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   any              `json:"content"`
				ToolCalls []openAIToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &in); err != nil || len(in.Choices) == 0 {
		return nil, fmt.Errorf("invalid OpenAI response")
	}
	choice := in.Choices[0]
	content := make([]map[string]any, 0, 1+len(choice.Message.ToolCalls))
	if text := stringifyNullableText(choice.Message.Content); text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, tc := range choice.Message.ToolCalls {
		var input any = map[string]any{}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
		content = append(content, map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input})
	}
	stopReason := "end_turn"
	if choice.FinishReason == "tool_calls" {
		stopReason = "tool_use"
	} else if choice.FinishReason == "length" {
		stopReason = "max_tokens"
	}
	model := requestedModel
	if in.Model != "" {
		model = in.Model
	}
	return map[string]any{
		"id":            firstNonEmpty(in.ID, "msg_alvus"),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  in.Usage.PromptTokens,
			"output_tokens": in.Usage.CompletionTokens,
		},
	}, nil
}

func writeAnthropicError(w http.ResponseWriter, status int, typ, message string) {
	if message == "" {
		message = http.StatusText(status)
	}
	writeJSON(w, status, map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": message}})
}

func stringifyNullableText(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(bytes.TrimSpace(b))
}

func firstNonEmpty(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}
