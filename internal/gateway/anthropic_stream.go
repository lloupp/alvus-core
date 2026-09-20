package gateway

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func translateOpenAIStream(w http.ResponseWriter, resp *http.Response, requestedModel string) {
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	writeSSE(w, flusher, "message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_alvus_stream", "type": "message", "role": "assistant", "model": requestedModel, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}})

	type toolState struct {
		blockIndex int
		started    bool
		id         string
		name       string
	}
	tools := map[int]*toolState{}
	nextBlockIndex := 0
	textBlockIndex := -1
	finishReason := ""

	scanner := bufio.NewScanner(resp.Body)
	buf := make([]byte, 64<<10)
	scanner.Buffer(buf, 4<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil || len(chunk.Choices) == 0 {
			continue
		}
		c := chunk.Choices[0]
		if c.Delta.Content != "" {
			if textBlockIndex < 0 {
				textBlockIndex = nextBlockIndex
				nextBlockIndex++
				writeSSE(w, flusher, "content_block_start", map[string]any{"type": "content_block_start", "index": textBlockIndex, "content_block": map[string]any{"type": "text", "text": ""}})
			}
			writeSSE(w, flusher, "content_block_delta", map[string]any{"type": "content_block_delta", "index": textBlockIndex, "delta": map[string]any{"type": "text_delta", "text": c.Delta.Content}})
		}
		for _, delta := range c.Delta.ToolCalls {
			ts := tools[delta.Index]
			if ts == nil {
				ts = &toolState{blockIndex: nextBlockIndex}
				nextBlockIndex++
				tools[delta.Index] = ts
			}
			if delta.ID != "" {
				ts.id = delta.ID
			}
			if delta.Function.Name != "" {
				ts.name = delta.Function.Name
			}
			if !ts.started && (ts.id != "" || ts.name != "") {
				writeSSE(w, flusher, "content_block_start", map[string]any{"type": "content_block_start", "index": ts.blockIndex, "content_block": map[string]any{"type": "tool_use", "id": firstNonEmpty(ts.id, fmt.Sprintf("call_%d", delta.Index)), "name": ts.name, "input": map[string]any{}}})
				ts.started = true
			}
			if delta.Function.Arguments != "" {
				if !ts.started {
					writeSSE(w, flusher, "content_block_start", map[string]any{"type": "content_block_start", "index": ts.blockIndex, "content_block": map[string]any{"type": "tool_use", "id": fmt.Sprintf("call_%d", delta.Index), "name": ts.name, "input": map[string]any{}}})
					ts.started = true
				}
				writeSSE(w, flusher, "content_block_delta", map[string]any{"type": "content_block_delta", "index": ts.blockIndex, "delta": map[string]any{"type": "input_json_delta", "partial_json": delta.Function.Arguments}})
			}
		}
		if c.FinishReason != nil {
			finishReason = *c.FinishReason
		}
	}
	if textBlockIndex >= 0 {
		writeSSE(w, flusher, "content_block_stop", map[string]any{"type": "content_block_stop", "index": textBlockIndex})
	}
	for _, ts := range tools {
		if ts.started {
			writeSSE(w, flusher, "content_block_stop", map[string]any{"type": "content_block_stop", "index": ts.blockIndex})
		}
	}
	stopReason := "end_turn"
	switch finishReason {
	case "length":
		stopReason = "max_tokens"
	case "tool_calls":
		stopReason = "tool_use"
	}
	writeSSE(w, flusher, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 0}})
	writeSSE(w, flusher, "message_stop", map[string]any{"type": "message_stop"})
}

func writeSSE(w io.Writer, flusher http.Flusher, event string, v any) {
	b, _ := json.Marshal(v)
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	if flusher != nil {
		flusher.Flush()
	}
}
