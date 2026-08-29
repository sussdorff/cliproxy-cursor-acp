package plugin

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/sussdorff/cliproxy-cursor-acp/internal/cursor"
)

// TestRegressionCCA7CDChatStreamUsesHostNativeJSONChunks guards the native host
// contract: CLIProxyAPI owns OpenAI Chat Completions SSE framing and its [DONE] event.
func TestRegressionCCA7CDChatStreamUsesHostNativeJSONChunks(t *testing.T) {
	frames, err := marshalChatStream("turn-1", "cursor/auto", cursor.ToolTurnEvent{
		Result: &cursor.Result{Text: "hello", InputTokens: 3, OutputTokens: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 3 {
		t.Fatalf("frame count = %d, want 3", len(frames))
	}
	for index, frame := range frames {
		if bytes.HasPrefix(frame, []byte("data:")) {
			t.Fatalf("frame %d contains plugin-owned SSE data prefix: %q", index, frame)
		}
		if bytes.Contains(frame, []byte("[DONE]")) {
			t.Fatalf("frame %d contains plugin-owned completion marker: %q", index, frame)
		}
		if !json.Valid(frame) {
			t.Fatalf("frame %d is not an independent JSON object: %q", index, frame)
		}
	}
}

func TestResponsesStreamRetainsEventDataSSEFraming(t *testing.T) {
	frames, err := marshalResponsesStream("turn-1", "cursor/auto", cursor.ToolTurnEvent{
		Result: &cursor.Result{Text: "hello"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 {
		t.Fatalf("frame count = %d, want 2", len(frames))
	}
	if !bytes.HasPrefix(frames[0], []byte("event: response.output_text.delta\ndata: ")) {
		t.Fatalf("delta frame lost Responses SSE framing: %q", frames[0])
	}
	if !bytes.HasPrefix(frames[1], []byte("event: response.completed\ndata: ")) {
		t.Fatalf("terminal frame lost Responses SSE framing: %q", frames[1])
	}
}
