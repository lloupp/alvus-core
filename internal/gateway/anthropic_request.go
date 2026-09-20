package gateway

import (
	"encoding/json"
	"fmt"
	"strings"
)

func translateAnthropicRequest(in anthropicRequest) ([]byte, error) {
	messages := make([]openAIMessage, 0, len(in.Messages)+1)
	if system := extractTextContent(in.System); system != "" {
		messages = append(messages, openAIMessage{Role: "system", Content: system})
	}
	for _, m := range in.Messages {
		converted, err := translateAnthropicMessage(m)
		if err != nil {
			return nil, err
		}
		messages = append(messages, converted...)
	}
	out := map[string]any{
		"model":      in.Model,
		"messages":   messages,
		"max_tokens": in.MaxTokens,
		"stream":     in.Stream,
	}
	if in.Temperature != nil {
		out["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		out["top_p"] = *in.TopP
	}
	if len(in.StopSequences) > 0 {
		out["stop"] = in.StopSequences
	}
	if len(in.Tools) > 0 {
		tools := make([]map[string]any, 0, len(in.Tools))
		for _, t := range in.Tools {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  t.InputSchema,
				},
			})
		}
		out["tools"] = tools
	}
	if len(in.ToolChoice) > 0 {
		if tc := translateToolChoice(in.ToolChoice); tc != nil {
			out["tool_choice"] = tc
		}
	}
	return json.Marshal(out)
}

func translateToolChoice(raw json.RawMessage) any {
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &tc) != nil {
		return nil
	}
	switch tc.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		if tc.Name != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": tc.Name}}
		}
	}
	return nil
}

func translateAnthropicMessage(m anthMessage) ([]openAIMessage, error) {
	var plain string
	if json.Unmarshal(m.Content, &plain) == nil {
		return []openAIMessage{{Role: m.Role, Content: plain}}, nil
	}
	var blocks []map[string]any
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil, fmt.Errorf("invalid message content")
	}
	var textParts []string
	var toolCalls []openAIToolCall
	var out []openAIMessage
	for _, b := range blocks {
		typ, _ := b["type"].(string)
		switch typ {
		case "text":
			if v, ok := b["text"].(string); ok {
				textParts = append(textParts, v)
			}
		case "tool_use":
			var tc openAIToolCall
			tc.ID, _ = b["id"].(string)
			tc.Type = "function"
			tc.Function.Name, _ = b["name"].(string)
			args, _ := json.Marshal(b["input"])
			tc.Function.Arguments = string(args)
			toolCalls = append(toolCalls, tc)
		case "tool_result":
			id, _ := b["tool_use_id"].(string)
			content := stringifyContent(b["content"])
			out = append(out, openAIMessage{Role: "tool", ToolCallID: id, Content: content})
		}
	}
	if len(textParts) > 0 || len(toolCalls) > 0 {
		out = append([]openAIMessage{{Role: m.Role, Content: strings.Join(textParts, "\n"), ToolCalls: toolCalls}}, out...)
	}
	if len(out) == 0 {
		out = append(out, openAIMessage{Role: m.Role, Content: ""})
	}
	return out, nil
}

func extractTextContent(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []map[string]any
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b["type"] == "text" {
			if t, ok := b["text"].(string); ok {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func stringifyContent(v any) string {
	switch x := v.(type) {
	case string:
		return x
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}
