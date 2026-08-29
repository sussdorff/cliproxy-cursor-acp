package plugin

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/sussdorff/cliproxy-cursor-acp/internal/cursor"
)

const (
	protocolChat      = "openai"
	protocolResponses = "openai-response"
)

type callerRequest struct {
	prompt         string
	conversationID string
	stateless      bool
	protocol       string
	tools          cursor.ToolNames
	toolResult     *callerToolResult
}

type callerToolResult struct {
	callID string
	result cursor.ToolResult
}

func decodeCallerRequest(executor pluginapi.ExecutorRequest) (callerRequest, error) {
	body := executor.OriginalRequest
	if len(body) == 0 {
		body = executor.Payload
	}
	protocol := strings.TrimSpace(executor.SourceFormat)
	if protocol == "" {
		protocol = strings.TrimSpace(executor.Format)
	}
	if protocol == "" {
		var probe map[string]json.RawMessage
		if json.Unmarshal(body, &probe) == nil {
			if _, ok := probe["input"]; ok {
				protocol = protocolResponses
			} else {
				protocol = protocolChat
			}
		}
	}
	var decoded callerRequest
	var err error
	switch protocol {
	case protocolChat:
		decoded, err = decodeChatRequest(body)
	case protocolResponses:
		decoded, err = decodeResponsesRequest(body)
	default:
		return callerRequest{}, cursorFailure("unsupported_protocol", "only OpenAI Chat Completions and Responses are supported")
	}
	if err != nil {
		return callerRequest{}, err
	}
	decoded.protocol = protocol
	for _, key := range []string{"derived_session_id", "execution_session_id"} {
		if candidate, ok := executor.Metadata[key].(string); ok && strings.TrimSpace(candidate) != "" {
			decoded.conversationID = strings.TrimSpace(candidate)
			decoded.stateless = false
			break
		}
	}
	if decoded.conversationID == "" && decoded.toolResult == nil {
		identifier := make([]byte, 16)
		if _, err := rand.Read(identifier); err != nil {
			return callerRequest{}, cursor.ValidationFailure("conversation_id_unavailable", "could not create request identity")
		}
		decoded.conversationID = "stateless-" + hex.EncodeToString(identifier)
		decoded.stateless = true
	}
	if decoded.toolResult == nil && strings.TrimSpace(decoded.prompt) == "" {
		return callerRequest{}, cursorFailure("invalid_request", "OpenAI request requires text content")
	}
	return decoded, nil
}

func decodeChatRequest(body []byte) (callerRequest, error) {
	var request struct {
		SessionID      string            `json:"session_id"`
		ConversationID string            `json:"conversation_id"`
		PromptCacheKey string            `json:"prompt_cache_key"`
		Prompt         string            `json:"prompt"`
		Messages       []json.RawMessage `json:"messages"`
		Tools          []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return callerRequest{}, cursorFailure("invalid_request", "request must be JSON")
	}
	tools, err := decodeTools(request.Tools, true)
	if err != nil {
		return callerRequest{}, err
	}
	result := callerRequest{prompt: request.Prompt, conversationID: firstNonEmpty(request.SessionID, request.ConversationID, request.PromptCacheKey), tools: tools}
	for index, raw := range request.Messages {
		var message struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
		}
		if err := json.Unmarshal(raw, &message); err != nil {
			return callerRequest{}, cursorFailure("invalid_request", "OpenAI messages must be objects")
		}
		if message.Role == "tool" {
			if index != len(request.Messages)-1 {
				continue
			}
			output, err := decodeStringContent(message.Content)
			if err != nil || strings.TrimSpace(message.ToolCallID) == "" {
				return callerRequest{}, cursorFailure("malformed_tool_result", "tool results require an opaque tool_call_id and text content")
			}
			result.toolResult = &callerToolResult{callID: message.ToolCallID, result: parseCallerToolResult(output)}
			continue
		}
		if string(message.Content) == "null" || len(message.Content) == 0 {
			continue
		}
		var content any
		if err := json.Unmarshal(message.Content, &content); err != nil {
			return callerRequest{}, cursorFailure("invalid_request", "OpenAI message content must be text")
		}
		text, ok := contentText(content)
		if !ok {
			return callerRequest{}, cursorFailure("invalid_request", "OpenAI message content must be text")
		}
		result.prompt += "[" + message.Role + "] " + text + "\n"
	}
	return result, nil
}

func decodeResponsesRequest(body []byte) (callerRequest, error) {
	var request struct {
		SessionID      string            `json:"session_id"`
		ConversationID string            `json:"conversation_id"`
		PromptCacheKey string            `json:"prompt_cache_key"`
		Instructions   string            `json:"instructions"`
		Input          []json.RawMessage `json:"input"`
		Tools          []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return callerRequest{}, cursorFailure("invalid_request", "request must be JSON")
	}
	tools, err := decodeTools(request.Tools, false)
	if err != nil {
		return callerRequest{}, err
	}
	result := callerRequest{conversationID: firstNonEmpty(request.SessionID, request.ConversationID, request.PromptCacheKey), tools: tools}
	if request.Instructions != "" {
		result.prompt = "[system] " + request.Instructions + "\n"
	}
	for index, raw := range request.Input {
		var item struct {
			Type    string          `json:"type"`
			Role    string          `json:"role"`
			CallID  string          `json:"call_id"`
			Output  json.RawMessage `json:"output"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return callerRequest{}, cursorFailure("invalid_request", "Responses input items must be objects")
		}
		if item.Type == "function_call_output" {
			if index != len(request.Input)-1 {
				continue
			}
			output, err := decodeResponseToolOutput(item.Output)
			if err != nil || strings.TrimSpace(item.CallID) == "" {
				return callerRequest{}, cursorFailure("malformed_tool_result", "function_call_output requires an opaque call_id and text output")
			}
			result.toolResult = &callerToolResult{callID: item.CallID, result: parseCallerToolResult(output)}
			continue
		}
		if item.Role == "" || len(item.Content) == 0 {
			continue
		}
		text, err := responseContentText(item.Content)
		if err != nil {
			return callerRequest{}, err
		}
		result.prompt += "[" + item.Role + "] " + text + "\n"
	}
	return result, nil
}

func decodeTools(rawTools []json.RawMessage, chat bool) (cursor.ToolNames, error) {
	tools := make(cursor.ToolNames)
	seen := make(map[string]bool)
	for _, raw := range rawTools {
		var outer struct {
			Type       string          `json:"type"`
			Name       string          `json:"name"`
			Parameters json.RawMessage `json:"parameters"`
			Function   *struct {
				Name       string          `json:"name"`
				Parameters json.RawMessage `json:"parameters"`
			} `json:"function"`
		}
		if err := json.Unmarshal(raw, &outer); err != nil || outer.Type != "function" {
			continue
		}
		name, schema := outer.Name, outer.Parameters
		if chat {
			if outer.Function == nil {
				continue
			}
			name, schema = outer.Function.Name, outer.Function.Parameters
		}
		kind := cursor.ToolKind(name)
		if kind != cursor.ToolRead && kind != cursor.ToolWrite && kind != cursor.ToolShell {
			continue
		}
		if seen[name] {
			return nil, cursorFailure("ambiguous_tool_mapping", "supported OpenCode tool definitions must be unique")
		}
		seen[name] = true
		if err := validateOpenCodeSchema(kind, schema); err != nil {
			return nil, err
		}
		tools[kind] = name
	}
	return tools, nil
}

func validateOpenCodeSchema(kind cursor.ToolKind, raw json.RawMessage) error {
	var schema struct {
		Type       string   `json:"type"`
		Required   []string `json:"required"`
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	if json.Unmarshal(raw, &schema) != nil || schema.Type != "object" {
		return cursorFailure("unsupported_tool_schema", "supported OpenCode tools require their documented object schemas")
	}
	required := map[cursor.ToolKind]map[string]string{
		cursor.ToolRead:  {"filePath": "string"},
		cursor.ToolWrite: {"filePath": "string", "content": "string"},
		cursor.ToolShell: {"command": "string"},
	}[kind]
	requiredSet := make(map[string]bool, len(schema.Required))
	for _, field := range schema.Required {
		requiredSet[field] = true
	}
	emitted := map[cursor.ToolKind]map[string]bool{
		cursor.ToolRead:  {"filePath": true, "offset": true, "limit": true},
		cursor.ToolWrite: {"filePath": true, "content": true},
		cursor.ToolShell: {"command": true, "workdir": true},
	}[kind]
	for field := range requiredSet {
		if !emitted[field] {
			return cursorFailure("unsupported_tool_schema", fmt.Sprintf("OpenCode %s requires unsupported field %s", kind, field))
		}
	}
	for field, fieldType := range required {
		if schema.Properties[field].Type != fieldType || !requiredSet[field] {
			return cursorFailure("unsupported_tool_schema", fmt.Sprintf("OpenCode %s must require %s as %s", kind, field, fieldType))
		}
	}
	optional := map[cursor.ToolKind]map[string][]string{
		cursor.ToolRead:  {"offset": {"integer", "number"}, "limit": {"integer", "number"}},
		cursor.ToolWrite: {},
		cursor.ToolShell: {"timeout": {"integer", "number"}, "workdir": {"string"}},
	}[kind]
	for field := range emitted {
		if _, requiredField := required[field]; requiredField {
			continue
		}
		if _, present := schema.Properties[field]; !present {
			return cursorFailure("unsupported_tool_schema", fmt.Sprintf("OpenCode %s must declare %s", kind, field))
		}
	}
	for field, types := range optional {
		property, present := schema.Properties[field]
		if !present {
			continue
		}
		valid := false
		for _, fieldType := range types {
			valid = valid || property.Type == fieldType
		}
		if !valid {
			return cursorFailure("unsupported_tool_schema", fmt.Sprintf("OpenCode %s field %s has an unsupported type", kind, field))
		}
	}
	return nil
}

func responseContentText(raw json.RawMessage) (string, error) {
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", cursorFailure("invalid_request", "Responses message content must be an array")
	}
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		if part.Type != "input_text" && part.Type != "output_text" && part.Type != "text" {
			return "", cursorFailure("invalid_request", "Responses message content must be text")
		}
		values = append(values, part.Text)
	}
	return strings.Join(values, "\n"), nil
}

func decodeStringContent(raw json.RawMessage) (string, error) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	return value, nil
}

func decodeResponseToolOutput(raw json.RawMessage) (string, error) {
	if value, err := decodeStringContent(raw); err == nil {
		return value, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", err
	}
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		if part.Type != "input_text" {
			return "", fmt.Errorf("non-text tool output")
		}
		values = append(values, part.Text)
	}
	return strings.Join(values, "\n"), nil
}

func parseCallerToolResult(output string) cursor.ToolResult {
	result := cursor.ToolResult{Output: output}
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(output), &envelope) == nil {
		failure, hasFailure := envelope["error"]
		_, hasContent := envelope["content"]
		explicitErrorEnvelope := len(envelope) == 1 || (len(envelope) == 2 && hasContent)
		if hasFailure && explicitErrorEnvelope && string(failure) != "null" && string(failure) != `""` {
			result.IsError = true
		}
	}
	return result
}
