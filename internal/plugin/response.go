package plugin

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sussdorff/cliproxy-cursor-acp/internal/cursor"
)

func marshalCallerResponse(protocol, conversationID, model string, event cursor.ToolTurnEvent) ([]byte, error) {
	switch protocol {
	case protocolChat:
		return json.Marshal(chatResponse(conversationID, model, event))
	case protocolResponses:
		return json.Marshal(responsesResponse(conversationID, model, event))
	default:
		return nil, fmt.Errorf("unsupported response protocol %q", protocol)
	}
}

func chatResponse(conversationID, model string, event cursor.ToolTurnEvent) map[string]any {
	message := map[string]any{"role": "assistant"}
	finish := "stop"
	usage := map[string]int64{}
	if event.ToolCall != nil {
		message["content"] = nil
		message["tool_calls"] = []any{map[string]any{"id": event.ToolCall.ID, "type": "function", "function": map[string]any{"name": event.ToolCall.Name, "arguments": toolArguments(event.ToolCall.Request)}}}
		finish = "tool_calls"
	} else if event.Result != nil {
		message["content"] = event.Result.Text
		finish = chatFinishReason(event.Result.StopReason)
		usage = map[string]int64{"prompt_tokens": event.Result.InputTokens, "completion_tokens": event.Result.OutputTokens, "total_tokens": event.Result.InputTokens + event.Result.OutputTokens}
	}
	return map[string]any{"id": "chatcmpl-" + conversationID, "object": "chat.completion", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": usage}
}

func responsesResponse(conversationID, model string, event cursor.ToolTurnEvent) map[string]any {
	output := make([]any, 0, 1)
	usage := map[string]int64{}
	if event.ToolCall != nil {
		output = append(output, map[string]any{"id": event.ToolCall.ID, "type": "function_call", "status": "completed", "call_id": event.ToolCall.ID, "name": event.ToolCall.Name, "arguments": toolArguments(event.ToolCall.Request)})
	} else if event.Result != nil {
		output = append(output, map[string]any{"id": "msg-" + conversationID, "type": "message", "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": event.Result.Text, "annotations": []any{}}}})
		usage = map[string]int64{"input_tokens": event.Result.InputTokens, "output_tokens": event.Result.OutputTokens, "total_tokens": event.Result.InputTokens + event.Result.OutputTokens}
	}
	response := map[string]any{"id": "resp-" + conversationID, "object": "response", "created_at": time.Now().Unix(), "status": "completed", "model": model, "output": output, "usage": usage}
	if event.Result != nil {
		switch event.Result.StopReason {
		case "max_tokens", "max_turn_requests":
			response["status"] = "incomplete"
			response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
		case "refusal":
			response["status"] = "failed"
			response["error"] = map[string]any{"code": "content_filter", "message": "Cursor refused the turn"}
		case "cancelled":
			response["status"] = "cancelled"
		}
	}
	return response
}

func chatFinishReason(stopReason string) string {
	switch stopReason {
	case "max_tokens", "max_turn_requests":
		return "length"
	case "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}

func toolArguments(request cursor.ToolRequest) string {
	arguments := make(map[string]any)
	switch request.Kind {
	case cursor.ToolRead:
		arguments["filePath"] = request.Path
		if request.Line > 0 {
			arguments["offset"] = request.Line
		}
		if request.Limit > 0 {
			arguments["limit"] = request.Limit
		}
	case cursor.ToolWrite:
		arguments["filePath"] = request.Path
		arguments["content"] = request.Content
	case cursor.ToolShell:
		command := shellQuote(request.Command)
		for _, argument := range request.Args {
			command += " " + shellQuote(argument)
		}
		arguments["command"] = command
		if request.WorkingDir != "" {
			arguments["workdir"] = request.WorkingDir
		}
	}
	payload, _ := json.Marshal(arguments)
	return string(payload)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func marshalCallerStream(protocol, conversationID, model string, event cursor.ToolTurnEvent) ([][]byte, error) {
	if protocol == protocolChat {
		return marshalChatStream(conversationID, model, event)
	}
	if protocol == protocolResponses {
		return marshalResponsesStream(conversationID, model, event)
	}
	return nil, fmt.Errorf("unsupported response protocol %q", protocol)
}

func marshalChatStream(conversationID, model string, event cursor.ToolTurnEvent) ([][]byte, error) {
	id := "chatcmpl-" + conversationID
	created := time.Now().Unix()
	var delta map[string]any
	finish := "stop"
	if event.ToolCall != nil {
		delta = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": event.ToolCall.ID, "type": "function", "function": map[string]any{"name": event.ToolCall.Name, "arguments": toolArguments(event.ToolCall.Request)}}}}
		finish = "tool_calls"
	} else {
		delta = map[string]any{"role": "assistant", "content": event.Result.Text}
		finish = chatFinishReason(event.Result.StopReason)
	}
	first, _ := json.Marshal(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}})
	last, _ := json.Marshal(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}})
	frames := [][]byte{[]byte("data: " + string(first) + "\n\n"), []byte("data: " + string(last) + "\n\n")}
	if event.Result != nil {
		usage, _ := json.Marshal(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model, "choices": []any{}, "usage": map[string]int64{"prompt_tokens": event.Result.InputTokens, "completion_tokens": event.Result.OutputTokens, "total_tokens": event.Result.InputTokens + event.Result.OutputTokens}})
		frames = append(frames, []byte("data: "+string(usage)+"\n\n"))
	}
	return append(frames, []byte("data: [DONE]\n\n")), nil
}

func marshalResponsesStream(conversationID, model string, turnEvent cursor.ToolTurnEvent) ([][]byte, error) {
	response := responsesResponse(conversationID, model, turnEvent)
	var item any
	if output, ok := response["output"].([]any); ok && len(output) > 0 {
		item = output[0]
	}
	sequence := 0
	frames := make([][]byte, 0, 3)
	if turnEvent.ToolCall != nil {
		frames = append(frames, responseSSE("response.output_item.done", map[string]any{"type": "response.output_item.done", "sequence_number": sequence, "output_index": 0, "item": item}))
		sequence++
	} else if turnEvent.Result != nil {
		frames = append(frames, responseSSE("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "sequence_number": sequence, "item_id": "msg-" + conversationID, "output_index": 0, "content_index": 0, "delta": turnEvent.Result.Text}))
		sequence++
	}
	frames = append(frames, responseSSE("response.completed", map[string]any{"type": "response.completed", "sequence_number": sequence, "response": response}))
	return frames, nil
}

func responseSSE(event string, payload any) []byte {
	encoded, _ := json.Marshal(payload)
	return []byte("event: " + event + "\ndata: " + string(encoded) + "\n\n")
}
