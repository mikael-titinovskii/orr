package app

import (
	"encoding/json"
	"strings"
)

// The guardrail checks must see the same conversation in every wire format orr
// proxies. OpenAI Chat Completions carries tool results as role "tool";
// Anthropic /v1/messages carries them as tool_result blocks inside a user
// message; the Responses API carries them as function_call_output items. Each is
// normalised into the OpenAI shape here, which is what /v1/checks accepts.

type rawContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	Output    json.RawMessage `json:"output"`
	CallID    string          `json:"call_id"`
	Arguments string          `json:"arguments"`
}

type rawToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// guardrailConversation returns the model, the normalised messages, the last
// user text, and the trailing tool run. The trailing run is the only part a
// tool-result check needs: earlier results were already checked on the turn
// that carried them, and re-checking the whole history would grow with the
// conversation.
// undecodable reports that some part of the request could not be normalised, so
// the gate is looking at less than the upstream will consume. Callers fail
// closed on it rather than checking the remainder as if it were the whole
// conversation.
func guardrailConversation(body []byte) (string, []guardrailMessage, *guardrailMessage, []guardrailMessage, bool) {
	var payload struct {
		Model    string            `json:"model"`
		Messages []json.RawMessage `json:"messages"`
		Input    json.RawMessage   `json:"input"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", nil, nil, nil, true
	}
	var messages []guardrailMessage
	undecodable := false
	// Both fields are checked, never one in place of the other: /v1/responses
	// accepts `input`, and a body that also carries `messages` must not hide the
	// text the upstream actually consumes behind a benign array.
	if len(payload.Messages) > 0 {
		normalised, ok := normaliseMessages(payload.Messages)
		messages = append(messages, normalised...)
		undecodable = undecodable || !ok
	}
	if len(payload.Input) > 0 {
		normalised, ok := normaliseResponsesInput(payload.Input)
		messages = append(messages, normalised...)
		undecodable = undecodable || !ok
	}
	var lastUser *guardrailMessage
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "user" {
			continue
		}
		if len(messages[i].Content) == 0 || string(messages[i].Content) == "null" || string(messages[i].Content) == `""` {
			continue
		}
		last := messages[i]
		lastUser = &last
		break
	}
	return payload.Model, messages, lastUser, trailingToolRun(messages), undecodable
}

func normaliseMessages(raw []json.RawMessage) ([]guardrailMessage, bool) {
	messages := make([]guardrailMessage, 0, len(raw))
	decoded := true
	for _, item := range raw {
		var message struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCalls  []rawToolCall   `json:"tool_calls"`
			ToolCallID string          `json:"tool_call_id"`
		}
		if err := json.Unmarshal(item, &message); err != nil {
			decoded = false
			continue
		}
		// OpenAI shape, possibly with content blocks (Anthropic or OpenAI parts).
		blocks, blocksDecoded := decodeContentBlocks(message.Content)
		if !blocksDecoded {
			decoded = false
			continue
		}
		if len(blocks) == 0 {
			msg := guardrailMessage{Role: message.Role, Content: message.Content, ToolCallID: message.ToolCallID}
			if calls := encodeToolCalls(message.ToolCalls); len(calls) > 0 {
				msg.ToolCalls = calls
			}
			messages = append(messages, msg)
			continue
		}
		var text strings.Builder
		// The top-level tool_calls survive an array content payload: dropping them
		// would leave a tool result with no recognised call run, and the
		// tool-result check would never run for it.
		calls := append([]rawToolCall{}, message.ToolCalls...)
		for _, block := range blocks {
			switch block.Type {
			case "text", "input_text", "output_text":
				text.WriteString(block.Text)
			case "tool_use", "function_call":
				call := rawToolCall{ID: block.ID, Type: "function"}
				call.Function.Name = block.Name
				if len(block.Input) > 0 && string(block.Input) != "null" {
					call.Function.Arguments = string(block.Input)
				} else if block.Arguments != "" {
					call.Function.Arguments = block.Arguments
				}
				calls = append(calls, call)
			case "tool_result", "function_call_output":
				content := block.Content
				if len(content) == 0 {
					content = block.Output
				}
				if len(content) == 0 {
					content = []byte(`""`)
				}
				messages = append(messages, guardrailMessage{
					Role:       "tool",
					Content:    content,
					ToolCallID: firstNonEmpty(block.ToolUseID, block.CallID),
				})
			}
		}
		// Text and tool calls belong to one assistant/user message; tool results
		// were emitted separately above so the tool-result validator can link
		// them to the calls.
		if text.Len() > 0 || len(calls) > 0 {
			msg := guardrailMessage{Role: message.Role}
			if text.Len() > 0 {
				encoded, _ := json.Marshal(text.String())
				msg.Content = encoded
			}
			if encoded := encodeToolCalls(calls); len(encoded) > 0 {
				msg.ToolCalls = encoded
			}
			messages = append(messages, msg)
		}
	}
	return messages, decoded
}

// normaliseResponsesInput handles the Responses API input field. A plain string
// is a user turn; item arrays carry message parts and function call outputs.
func normaliseResponsesInput(raw json.RawMessage) ([]guardrailMessage, bool) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		encoded, _ := json.Marshal(text)
		return []guardrailMessage{{Role: "user", Content: encoded}}, true
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, false
	}
	decoded := true
	var messages []guardrailMessage
	for _, item := range items {
		var entry struct {
			Type      string          `json:"type"`
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			CallID    string          `json:"call_id"`
			Name      string          `json:"name"`
			Output    json.RawMessage `json:"output"`
			Input     json.RawMessage `json:"input"`
			Arguments string          `json:"arguments"`
		}
		if err := json.Unmarshal(item, &entry); err != nil {
			decoded = false
			continue
		}
		switch entry.Type {
		case "function_call_output":
			content := entry.Output
			if len(content) == 0 {
				content = []byte(`""`)
			}
			messages = append(messages, guardrailMessage{Role: "tool", Content: content, ToolCallID: entry.CallID})
		case "function_call":
			call := rawToolCall{ID: entry.CallID, Type: "function"}
			call.Function.Name = entry.Name
			call.Function.Arguments = entry.Arguments
			msg := guardrailMessage{Role: "assistant"}
			if encoded := encodeToolCalls([]rawToolCall{call}); len(encoded) > 0 {
				msg.ToolCalls = encoded
			}
			messages = append(messages, msg)
		default:
			content := entry.Content
			if len(content) == 0 && entry.Role == "" {
				continue
			}
			blocks, blocksDecoded := decodeContentBlocks(content)
			if !blocksDecoded {
				decoded = false
				continue
			}
			var text strings.Builder
			for _, block := range blocks {
				if block.Type == "input_text" || block.Type == "output_text" || block.Type == "text" {
					text.WriteString(block.Text)
				}
			}
			if text.Len() > 0 {
				encoded, _ := json.Marshal(text.String())
				messages = append(messages, guardrailMessage{Role: firstNonEmpty(entry.Role, "user"), Content: encoded})
			} else if len(content) > 0 {
				messages = append(messages, guardrailMessage{Role: firstNonEmpty(entry.Role, "user"), Content: content})
			}
		}
	}
	return messages, decoded
}

// decodeContentBlocks returns the blocks of an array content payload, and
// reports false when it looks like an array but cannot be decoded. Callers fail
// closed on that: a block it cannot read may be the tool_result the gate needs.
func decodeContentBlocks(content json.RawMessage) ([]rawContentBlock, bool) {
	trimmed := strings.TrimSpace(string(content))
	if !strings.HasPrefix(trimmed, "[") {
		return nil, true
	}
	var blocks []rawContentBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil, false
	}
	return blocks, true
}

func encodeToolCalls(calls []rawToolCall) json.RawMessage {
	if len(calls) == 0 {
		return nil
	}
	normalised := make([]rawToolCall, 0, len(calls))
	for _, call := range calls {
		call.Function.Name = canonicalToolName(call.Function.Name)
		call.Function.Arguments = canonicalToolArguments(call.Function.Name, call.Function.Arguments)
		normalised = append(normalised, call)
	}
	encoded, err := json.Marshal(normalised)
	if err != nil {
		return nil
	}
	return encoded
}

// canonicalToolName maps the tool names the harnesses actually send onto the
// three classes the policy's per_tool rails are written for. Claude Code and
// Kimi Code send Bash/Read/Write/Edit, OpenCode sends bash/read/write/edit/patch;
// without this the rails only ever see a name they do not know, and a per_tool
// rail is applied to the tools it names alone.
func canonicalToolName(name string) string {
	switch strings.ToLower(name) {
	case "bash", "sh", "shell", "terminal", "cmd", "powershell", "run_command", "run_shell_command", "execute_command":
		return "run_command"
	case "read", "read_file", "readfile", "view", "cat":
		return "read_file"
	case "write", "write_file", "writefile", "edit", "multiedit", "multi_edit",
		"apply_patch", "patch", "str_replace", "str_replace_editor",
		"notebookedit", "notebook_edit", "create_file":
		return "write_file"
	}
	return name
}

// toolArgumentAliases is the argument name each harness uses for the value the
// rails read: command for a shell call, path for a file call.
var toolArgumentAliases = map[string][]string{
	"run_command": {"cmd", "script", "shell_command"},
	"read_file":   {"file_path", "filePath", "filepath", "filename", "file"},
	"write_file":  {"file_path", "filePath", "filepath", "filename", "file"},
}

// canonicalToolArguments renames an argument to the name the policy reads, so a
// Claude `file_path` and an OpenCode `filePath` are both judged as `path`. An
// argument that is not a JSON object is passed through unchanged.
func canonicalToolArguments(tool, arguments string) string {
	aliases := toolArgumentAliases[tool]
	if len(aliases) == 0 || arguments == "" {
		return arguments
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(arguments), &value); err != nil {
		return arguments
	}
	target := "path"
	if tool == "run_command" {
		target = "command"
	}
	for _, alias := range aliases {
		source, ok := value[alias]
		if !ok {
			continue
		}
		if _, present := value[target]; !present {
			value[target] = source
		}
		delete(value, alias)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return arguments
	}
	return string(encoded)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// trailingToolRun keeps the last assistant message that carries tool calls and
// everything after it, but only when that run contains a tool message. It is
// the unit the tool-result validator needs for linkage and the unit that bounds
// the check's size.
func trailingToolRun(messages []guardrailMessage) []guardrailMessage {
	start := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" && len(messages[i].ToolCalls) > 0 {
			start = i
			break
		}
	}
	if start == -1 {
		return nil
	}
	run := append([]guardrailMessage{}, messages[start:]...)
	for _, message := range run {
		if message.Role == "tool" {
			return run
		}
	}
	return nil
}
