package proxy

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ==================== MiMo 翻译器 ====================
// 将 OpenAI Responses API 请求翻译为 MiMo Chat Completions API
// 移植自 mimo2codex/src/translate/reqToChat.ts 和 respToResponses.ts

// MimoChatRequest MiMo Chat Completions 请求格式
type MimoChatRequest struct {
	Model               string            `json:"model"`
	Messages            []MimoChatMessage `json:"messages"`
	Stream              bool              `json:"stream,omitempty"`
	StreamOptions       *MimoStreamOpts   `json:"stream_options,omitempty"`
	Tools               []MimoChatTool    `json:"tools,omitempty"`
	ToolChoice          any               `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool             `json:"parallel_tool_calls,omitempty"`
	Temperature         *float64          `json:"temperature,omitempty"`
	TopP                *float64          `json:"top_p,omitempty"`
	MaxCompletionTokens *int              `json:"max_completion_tokens,omitempty"`
	Thinking            *MimoThinking     `json:"thinking,omitempty"`
	ReasoningEffort     string            `json:"reasoning_effort,omitempty"`
}

type MimoStreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

type MimoThinking struct {
	Type string `json:"type"` // "enabled" | "disabled" | "auto"
}

type MimoChatMessage struct {
	Role             string           `json:"role"`
	Content          any              `json:"content"` // string 或 []MimoContentPart
	ToolCalls        []MimoToolCall   `json:"tool_calls,omitempty"`
	ToolCallID       string           `json:"tool_call_id,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
}

type MimoContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL    string `json:"url"`
		Detail string `json:"detail,omitempty"`
	} `json:"image_url,omitempty"`
}

type MimoToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type MimoChatTool struct {
	Type     string         `json:"type"`
	Function *MimoToolFunc  `json:"function,omitempty"`
}

type MimoToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// MimoWebSearchTool MiMo 原生 web_search 工具
type MimoWebSearchTool struct {
	Type string `json:"type"` // "web_search"
}

// ==================== Responses → Chat Completions 翻译 ====================

// ResponsesToMimoChat 将 OpenAI Responses API 请求翻译为 MiMo Chat Completions 请求
func ResponsesToMimoChat(responsesBody []byte, isTokenPlan bool) ([]byte, string, error) {
	if !gjson.ValidBytes(responsesBody) {
		return nil, "", fmt.Errorf("invalid JSON body")
	}

	model := NormalizeMimoModelID(strings.TrimSpace(gjson.GetBytes(responsesBody, "model").String()))
	if model == "" {
		model = MimoDefaultModel
	}

	chat := MimoChatRequest{
		Model:  model,
		Stream: gjson.GetBytes(responsesBody, "stream").Bool(),
	}

	// 流式选项
	if chat.Stream {
		chat.StreamOptions = &MimoStreamOpts{IncludeUsage: true}
	}

	// 翻译 instructions → system message
	if instructions := gjson.GetBytes(responsesBody, "instructions").String(); instructions != "" {
		chat.Messages = append(chat.Messages, MimoChatMessage{
			Role:    "system",
			Content: instructions,
		})
	}

	// 翻译 input → messages
	input := gjson.GetBytes(responsesBody, "input")
	if input.Exists() {
		messages := translateResponsesInputToMessages(input, model)
		chat.Messages = append(chat.Messages, messages...)
	}

	// 翻译 tools
	tools := gjson.GetBytes(responsesBody, "tools")
	if tools.Exists() {
		chatTools, hasWebSearch := translateResponsesTools(tools, isTokenPlan)
		if len(chatTools) > 0 {
			chat.Tools = chatTools
		}

		// Token Plan 账号 + 有 web_search + 搜索已启用 = 自动搜索注入
		if hasWebSearch && isTokenPlan && IsSearchEnabled() {
			// 提取搜索查询
			query := ExtractSearchQuery(chat.Messages)
			if query != "" {
				slog.Info("auto search triggered", "query", query, "provider", GetSearchProvider())

				// 执行搜索
				searchResults, err := PerformSearch(query, 5)
				if err != nil {
					slog.Warn("auto search failed", "error", err)
				} else if searchResults != nil && len(searchResults.Results) > 0 {
					// 注入搜索结果到消息
					formattedResults := FormatSearchResultsForPrompt(searchResults)
					chat.Messages = InjectSearchResults(chat.Messages, formattedResults)
					slog.Info("search results injected", "count", len(searchResults.Results))
				}

				// 移除 web_search 工具（避免 MiMo 400 错误）
				chat.Tools = RemoveWebSearchTool(chat.Tools)
			}
		}
	}

	// 翻译 tool_choice
	if toolChoice := gjson.GetBytes(responsesBody, "tool_choice"); toolChoice.Exists() {
		chat.ToolChoice = translateToolChoice(toolChoice)
	}

	// 翻译 parallel_tool_calls
	if pt := gjson.GetBytes(responsesBody, "parallel_tool_calls"); pt.Exists() {
		val := pt.Bool()
		chat.ParallelToolCalls = &val
	}

	// 翻译 temperature
	if temp := gjson.GetBytes(responsesBody, "temperature"); temp.Exists() && temp.Type != gjson.Null {
		val := temp.Float()
		chat.Temperature = &val
	}

	// 翻译 max_output_tokens → max_completion_tokens
	if maxTokens := gjson.GetBytes(responsesBody, "max_output_tokens"); maxTokens.Exists() && maxTokens.Type != gjson.Null {
		val := int(maxTokens.Int())
		chat.MaxCompletionTokens = &val
	}

	// 翻译 reasoning.effort → reasoning_effort
	if effort := gjson.GetBytes(responsesBody, "reasoning.effort"); effort.Exists() {
		chat.ReasoningEffort = normalizeReasoningEffort(effort.String())
	}

	// MiMo 特殊处理: 注入 thinking 默认值
	normalizeMimoThinking(&chat, model)

	// MiMo 特殊处理: thinking 模式下移除 temperature
	if MimoThinkingFixesTemperature[model] && chat.Thinking != nil && chat.Thinking.Type == "enabled" {
		chat.Temperature = nil
	}

	// MiMo 特殊处理: tool_choice 非 "auto" 时移除
	if chat.ToolChoice != nil {
		if tc, ok := chat.ToolChoice.(string); ok && tc != "auto" {
			chat.ToolChoice = nil
		}
	}

	// 清理 reasoning_effort: thinking disabled 时不需要
	if chat.Thinking != nil && chat.Thinking.Type == "disabled" && chat.ReasoningEffort == "none" {
		chat.ReasoningEffort = ""
	}

	result, err := json.Marshal(chat)
	return result, model, err
}

// translateResponsesInputToMessages 翻译 Responses input 到 Chat messages
func translateResponsesInputToMessages(input gjson.Result, model string) []MimoChatMessage {
	var messages []MimoChatMessage

	if input.Type == gjson.String {
		// 简单字符串 input → user message
		messages = append(messages, MimoChatMessage{
			Role:    "user",
			Content: input.String(),
		})
		return messages
	}

	if !input.IsArray() {
		return messages
	}

	supportsImages := MimoModelSupportsImages(model)

	input.ForEach(func(_, item gjson.Result) bool {
		msg := translateResponsesInputItem(item, supportsImages)
		if msg != nil {
			messages = append(messages, *msg)
		}
		return true
	})

	return messages
}

// translateResponsesInputItem 翻译单个 Responses input item
func translateResponsesInputItem(item gjson.Result, supportsImages bool) *MimoChatMessage {
	itemType := item.Get("type").String()

	switch itemType {
	case "message":
		return translateResponsesMessage(item, supportsImages)
	case "function_call":
		// function_call items 在 Chat Completions 中是 assistant message 的一部分
		// 这里返回一个带 tool_calls 的 assistant message
		return &MimoChatMessage{
			Role: "assistant",
			ToolCalls: []MimoToolCall{
				{
					ID:   item.Get("call_id").String(),
					Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{
						Name:      item.Get("name").String(),
						Arguments: item.Get("arguments").String(),
					},
				},
			},
		}
	case "function_call_output":
		return &MimoChatMessage{
			Role:       "tool",
			ToolCallID: item.Get("call_id").String(),
			Content:    extractOutputText(item.Get("output")),
		}
	case "reasoning":
		// reasoning item → assistant message with reasoning_content
		text := extractReasoningText(item)
		if text != "" {
			return &MimoChatMessage{
				Role:             "assistant",
				ReasoningContent: text,
			}
		}
	}

	return nil
}

// translateResponsesMessage 翻译 Responses message item
func translateResponsesMessage(item gjson.Result, supportsImages bool) *MimoChatMessage {
	role := item.Get("role").String()
	if role == "developer" {
		role = "system"
	}

	content := item.Get("content")
	if !content.Exists() {
		return &MimoChatMessage{Role: role, Content: ""}
	}

	// 处理 content 为字符串的情况
	if content.Type == gjson.String {
		return &MimoChatMessage{Role: role, Content: content.String()}
	}

	// 处理 content 为数组的情况
	if content.IsArray() {
		parts := translateContentParts(content, supportsImages)
		if len(parts) == 0 {
			return &MimoChatMessage{Role: role, Content: ""}
		}
		// 如果只有一個 text part，简化为字符串
		if len(parts) == 1 && parts[0].Type == "text" {
			return &MimoChatMessage{Role: role, Content: parts[0].Text}
		}
		return &MimoChatMessage{Role: role, Content: parts}
	}

	return &MimoChatMessage{Role: role, Content: ""}
}

// translateContentParts 翻译内容部分
func translateContentParts(content gjson.Result, supportsImages bool) []MimoContentPart {
	var parts []MimoContentPart

	content.ForEach(func(_, part gjson.Result) bool {
		partType := part.Get("type").String()
		switch partType {
		case "input_text", "output_text":
			text := part.Get("text").String()
			if text != "" {
				parts = append(parts, MimoContentPart{Type: "text", Text: text})
			}
		case "input_image":
			if supportsImages {
				parts = append(parts, MimoContentPart{
					Type: "image_url",
					ImageURL: &struct {
						URL    string `json:"url"`
						Detail string `json:"detail,omitempty"`
					}{
						URL:    part.Get("image_url").String(),
						Detail: part.Get("detail").String(),
					},
				})
			}
			// 不支持图片时静默丢弃
		}
		return true
	})

	return parts
}

// extractOutputText 提取输出文本
func extractOutputText(output gjson.Result) string {
	if output.Type == gjson.String {
		return output.String()
	}
	if output.IsArray() {
		var texts []string
		output.ForEach(func(_, part gjson.Result) bool {
			if part.Get("type").String() == "output_text" || part.Get("type").String() == "input_text" {
				if text := part.Get("text").String(); text != "" {
					texts = append(texts, text)
				}
			}
			return true
		})
		return strings.Join(texts, "\n")
	}
	return output.String()
}

// extractReasoningText 提取 reasoning 文本
func extractReasoningText(item gjson.Result) string {
	// 优先使用 encrypted_content
	if ec := item.Get("encrypted_content"); ec.Exists() && ec.String() != "" {
		return ec.String()
	}
	// 回退到 summary
	summary := item.Get("summary")
	if summary.IsArray() {
		var texts []string
		summary.ForEach(func(_, s gjson.Result) bool {
			if s.Get("type").String() == "summary_text" {
				texts = append(texts, s.Get("text").String())
			}
			return true
		})
		return strings.Join(texts, "")
	}
	return ""
}

// translateResponsesTools 翻译 Responses tools 到 Chat tools
func translateResponsesTools(tools gjson.Result, isTokenPlan bool) ([]MimoChatTool, bool) {
	var chatTools []MimoChatTool
	hasWebSearch := false

	tools.ForEach(func(_, tool gjson.Result) bool {
		toolType := tool.Get("type").String()

		switch toolType {
		case "function":
			name := tool.Get("name").String()
			if name == "" {
				return true // 跳过无名工具
			}
			chatTool := MimoChatTool{
				Type: "function",
				Function: &MimoToolFunc{
					Name:        name,
					Description: tool.Get("description").String(),
				},
			}
			if params := tool.Get("parameters"); params.Exists() && params.Raw != "" {
				chatTool.Function.Parameters = json.RawMessage(params.Raw)
			}
			chatTools = append(chatTools, chatTool)

		case "local_shell":
			// Codex 的 local_shell → MiMo 的 shell function
			chatTools = append(chatTools, MimoChatTool{
				Type: "function",
				Function: &MimoToolFunc{
					Name:        "shell",
					Description: "Execute a shell command on the local machine. Returns stdout, stderr and exit code.",
					Parameters: json.RawMessage(`{
						"type": "object",
						"properties": {
							"command": {
								"type": "array",
								"items": {"type": "string"},
								"description": "Argv array, e.g. [\"ls\", \"-la\"]"
							},
							"workdir": {
								"type": "string",
								"description": "Working directory (optional)"
							},
							"timeout_ms": {
								"type": "number",
								"description": "Timeout in milliseconds (optional)"
							}
						},
						"required": ["command"]
					}`),
				},
			})

		case "web_search", "web_search_preview":
			// Token Plan 账号默认没有激活 Web Search 插件
			// 如果发送 web_search，MiMo 会返回 400: "webSearchEnabled is false"
			// 用户需要在 https://platform.xiaomimimo.com/#/console/plugin 激活
			// 这里默认移除以避免 400 错误，但可通过环境变量强制启用
			forceWebSearch := os.Getenv("MIMO_FORCE_WEB_SEARCH") == "true"
			if !isTokenPlan || forceWebSearch {
				hasWebSearch = true
				chatTools = append(chatTools, MimoChatTool{Type: "web_search"})
			} else {
				slog.Debug("web_search skipped for token-plan account (set MIMO_FORCE_WEB_SEARCH=true to override)")
			}

		case "code_interpreter", "file_search", "image_generation",
			"computer_use_preview", "computer_use", "tool_search":
			// 这些工具 MiMo 不支持，静默丢弃

		case "namespace":
			// 递归处理 namespace 内的工具
			if nested := tool.Get("tools"); nested.Exists() {
				nestedTools, _ := translateResponsesTools(nested, isTokenPlan)
				chatTools = append(chatTools, nestedTools...)
			}

		case "mcp":
			// MCP 工具 MiMo 不支持，静默丢弃

		case "custom":
			// custom tool → function tool
			name := tool.Get("name").String()
			if name != "" {
				chatTools = append(chatTools, MimoChatTool{
					Type: "function",
					Function: &MimoToolFunc{
						Name:        name,
						Description: tool.Get("description").String(),
						Parameters:  json.RawMessage(`{"type":"object","properties":{"input":{"type":"string"}},"additionalProperties":true}`),
					},
				})
			}
		}
		return true
	})

	// 去重
	chatTools = deduplicateChatTools(chatTools)

	return chatTools, hasWebSearch
}

// deduplicateChatTools 去重工具列表
func deduplicateChatTools(tools []MimoChatTool) []MimoChatTool {
	seen := make(map[string]bool)
	var result []MimoChatTool
	for _, t := range tools {
		key := t.Type
		if t.Function != nil {
			key = "fn:" + t.Function.Name
		}
		if !seen[key] {
			seen[key] = true
			result = append(result, t)
		}
	}
	return result
}

// translateToolChoice 翻译 tool_choice
func translateToolChoice(tc gjson.Result) any {
	if tc.Type == gjson.String {
		return tc.String()
	}
	if tc.Get("type").String() == "function" {
		name := tc.Get("function.name").String()
		if name == "" {
			name = tc.Get("name").String()
		}
		if name != "" {
			return map[string]any{
				"type":     "function",
				"function": map[string]string{"name": name},
			}
		}
	}
	return nil
}

// normalizeMimoThinking 注入 MiMo thinking 默认值
func normalizeMimoThinking(chat *MimoChatRequest, model string) {
	if chat.Thinking != nil {
		return // 已经设置了
	}
	// 默认禁用 thinking 的模型不注入
	if MimoThinkingDefaultDisabled[model] {
		return
	}
	// 其他模型默认启用 thinking
	chat.Thinking = &MimoThinking{Type: "enabled"}
}

// ==================== Chat Completions → Responses 翻译 ====================

// ChatChunkToResponsesEvent 将 MiMo Chat Completions 流式 chunk 翻译为 Responses SSE 事件
// 返回多个 SSE 事件行
func ChatChunkToResponsesEvent(chunk []byte, responseID string) ([]string, error) {
	if !gjson.ValidBytes(chunk) {
		return nil, nil
	}

	var events []string

	chunkType := gjson.GetBytes(chunk, "object").String()
	if chunkType == "chat.completion.chunk" {
		events = translateChatStreamChunk(chunk, responseID)
	} else if chunkType == "chat.completion" {
		// 非流式响应
		events = translateChatCompletion(chunk, responseID)
	}

	return events, nil
}

// translateChatStreamChunk 翻译流式 chunk
func translateChatStreamChunk(chunk []byte, responseID string) []string {
	var events []string

	choices := gjson.GetBytes(chunk, "choices")
	if !choices.IsArray() {
		// 可能是 usage-only chunk
		if usage := gjson.GetBytes(chunk, "usage"); usage.Exists() {
			events = append(events, buildResponsesUsageEvent(usage, responseID))
		}
		return events
	}

	choices.ForEach(func(_, choice gjson.Result) bool {
		delta := choice.Get("delta")
		finishReason := choice.Get("finish_reason").String()

		// reasoning_content → reasoning event
		if rc := delta.Get("reasoning_content"); rc.Exists() && rc.String() != "" {
			events = append(events, buildResponsesReasoningDelta(rc.String(), responseID))
		}

		// content → output_text delta
		if content := delta.Get("content"); content.Exists() && content.String() != "" {
			events = append(events, buildResponsesTextDelta(content.String(), responseID))
		}

		// tool_calls → function_call events
		if toolCalls := delta.Get("tool_calls"); toolCalls.IsArray() {
			toolCalls.ForEach(func(_, tc gjson.Result) bool {
				events = append(events, buildResponsesFunctionCallDelta(tc, responseID))
				return true
			})
		}

		// finish_reason → completed event
		if finishReason == "stop" || finishReason == "tool_calls" {
			events = append(events, buildResponsesCompletedEvent(responseID))
		}

		return true
	})

	// usage event (通常在最后一个 chunk)
	if usage := gjson.GetBytes(chunk, "usage"); usage.Exists() {
		events = append(events, buildResponsesUsageEvent(usage, responseID))
	}

	return events
}

// translateChatCompletion 翻译非流式响应
func translateChatCompletion(completion []byte, responseID string) []string {
	var events []string

	// 构建完整的 Responses 对象
	resp := map[string]any{
		"id":         responseID,
		"object":     "response",
		"created_at": gjson.GetBytes(completion, "created").Int(),
		"status":     "completed",
		"model":      gjson.GetBytes(completion, "model").String(),
		"output":     []any{},
		"usage":      nil,
	}

	// 翻译 choices
	choices := gjson.GetBytes(completion, "choices")
	if choices.IsArray() && choices.Get("0").Exists() {
		choice := choices.Get("0")
		message := choice.Get("message")

		var outputItems []any

		// reasoning_content → reasoning item
		if rc := message.Get("reasoning_content"); rc.Exists() && rc.String() != "" {
			outputItems = append(outputItems, map[string]any{
				"type":   "reasoning",
				"id":     "rs_" + responseID,
				"summary": []map[string]any{
					{"type": "summary_text", "text": rc.String()},
				},
				"encrypted_content": rc.String(),
				"status":            "completed",
			})
		}

		// content → message item
		if content := message.Get("content"); content.Exists() && content.String() != "" {
			outputItems = append(outputItems, map[string]any{
				"type":   "message",
				"id":     "msg_" + responseID,
				"role":   "assistant",
				"content": []map[string]any{
					{"type": "output_text", "text": content.String()},
				},
				"status": "completed",
			})
		}

		// tool_calls → function_call items
		if toolCalls := message.Get("tool_calls"); toolCalls.IsArray() {
			toolCalls.ForEach(func(_, tc gjson.Result) bool {
				outputItems = append(outputItems, map[string]any{
					"type":     "function_call",
					"id":       "fc_" + tc.Get("id").String(),
					"call_id":  tc.Get("id").String(),
					"name":     tc.Get("function.name").String(),
					"arguments": tc.Get("function.arguments").String(),
					"status":   "completed",
				})
				return true
			})
		}

		resp["output"] = outputItems
	}

	// usage
	if usage := gjson.GetBytes(completion, "usage"); usage.Exists() {
		resp["usage"] = map[string]any{
			"input_tokens":  usage.Get("prompt_tokens").Int(),
			"output_tokens": usage.Get("completion_tokens").Int(),
			"total_tokens":  usage.Get("total_tokens").Int(),
		}
	}

	data, _ := json.Marshal(resp)
	events = append(events, "event: response.completed\ndata: "+string(data))

	return events
}

// ==================== SSE 事件构建函数 ====================

func buildResponsesTextDelta(text, responseID string) string {
	event := map[string]any{
		"type":         "response.output_text.delta",
		"item_id":      "msg_" + responseID,
		"output_index": 0,
		"content_index": 0,
		"delta":        text,
	}
	data, _ := json.Marshal(event)
	return "event: response.output_text.delta\ndata: " + string(data)
}

func buildResponsesReasoningDelta(text, responseID string) string {
	event := map[string]any{
		"type":            "response.reasoning_summary_text.delta",
		"item_id":         "rs_" + responseID,
		"output_index":    0,
		"summary_index":   0,
		"delta":           text,
	}
	data, _ := json.Marshal(event)
	return "event: response.reasoning_summary_text.delta\ndata: " + string(data)
}

func buildResponsesFunctionCallDelta(tc gjson.Result, responseID string) string {
	name := tc.Get("function.name").String()
	args := tc.Get("function.arguments").String()
	callID := tc.Get("id").String()

	var events []string

	if name != "" {
		// function_call arguments.start
		startEvent := map[string]any{
			"type":        "response.function_call_arguments.done",
			"item_id":     "fc_" + callID,
			"output_index": 0,
			"name":        name,
			"call_id":     callID,
			"arguments":   args,
		}
		data, _ := json.Marshal(startEvent)
		events = append(events, "event: response.function_call_arguments.done\ndata: "+string(data))
	}

	if len(events) > 0 {
		return events[0]
	}
	return ""
}

func buildResponsesUsageEvent(usage gjson.Result, responseID string) string {
	event := map[string]any{
		"type": "response.usage",
		"usage": map[string]any{
			"input_tokens":  usage.Get("prompt_tokens").Int(),
			"output_tokens": usage.Get("completion_tokens").Int(),
			"total_tokens":  usage.Get("total_tokens").Int(),
		},
	}
	data, _ := json.Marshal(event)
	return "event: response.usage\ndata: " + string(data)
}

func buildResponsesCompletedEvent(responseID string) string {
	event := map[string]any{
		"type":    "response.completed",
		"response": map[string]any{
			"id":     responseID,
			"status": "completed",
		},
	}
	data, _ := json.Marshal(event)
	return "event: response.completed\ndata: " + string(data)
}

// ==================== Chat Completions API 请求处理 ====================

// TranslateChatCompletionsToMimo 将标准 Chat Completions 请求翻译为 MiMo 格式
func TranslateChatCompletionsToMimo(chatBody []byte) ([]byte, string, error) {
	if !gjson.ValidBytes(chatBody) {
		return nil, "", fmt.Errorf("invalid JSON body")
	}

	model := NormalizeMimoModelID(strings.TrimSpace(gjson.GetBytes(chatBody, "model").String()))
	if model == "" {
		model = MimoDefaultModel
	}

	// 直接使用原始 body，只做必要的修改
	body := chatBody

	// 设置正确的模型
	var err error
	body, err = sjson.SetBytes(body, "model", model)
	if err != nil {
		return nil, "", err
	}

	// 注入 thinking 默认值
	if !gjson.GetBytes(body, "thinking").Exists() && !MimoThinkingDefaultDisabled[model] {
		body, _ = sjson.SetBytes(body, "thinking", map[string]string{"type": "enabled"})
	}

	// thinking 模式下移除 temperature
	if MimoThinkingFixesTemperature[model] {
		thinkingType := gjson.GetBytes(body, "thinking.type").String()
		if thinkingType == "enabled" {
			body, _ = sjson.DeleteBytes(body, "temperature")
		}
	}

	// stream_options 注入
	if gjson.GetBytes(body, "stream").Bool() && !gjson.GetBytes(body, "stream_options").Exists() {
		body, _ = sjson.SetBytes(body, "stream_options", map[string]bool{"include_usage": true})
	}

	// 检查是否有 web_search 工具
	hasWebSearch := false
	tools := gjson.GetBytes(body, "tools")
	if tools.IsArray() {
		tools.ForEach(func(_, tool gjson.Result) bool {
			if tool.Get("type").String() == "web_search" {
				hasWebSearch = true
				return false
			}
			return true
		})
	}

	// Token Plan 账号 + 搜索已启用 = 自动搜索注入
	if hasWebSearch && IsSearchEnabled() {
		// 提取搜索查询（从最后一条 user 消息）
		query := ""
		messages := gjson.GetBytes(body, "messages")
		if messages.IsArray() {
			// 从后往前找 user 消息
			for i := len(messages.Array()) - 1; i >= 0; i-- {
				msg := messages.Array()[i]
				if msg.Get("role").String() == "user" {
					query = msg.Get("content").String()
					break
				}
			}
		}

		if query != "" {
			slog.Info("auto search triggered for chat completions", "query", query)

			// 执行搜索
			searchResults, err := PerformSearch(query, 5)
			if err != nil {
				slog.Warn("auto search failed", "error", err)
			} else if searchResults != nil && len(searchResults.Results) > 0 {
				// 注入搜索结果到 system 消息
				formattedResults := FormatSearchResultsForPrompt(searchResults)
				body = injectSearchResultsToChatBody(body, formattedResults)
				slog.Info("search results injected", "count", len(searchResults.Results))
			}

			// 移除 web_search 工具
			body = removeWebSearchFromBody(body)
		}
	}

	return body, model, nil
}

// injectSearchResultsToChatBody 将搜索结果注入到 Chat Completions body 的 system 消息中
func injectSearchResultsToChatBody(body []byte, searchResults string) []byte {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return body
	}

	// 查找并更新 system 消息
	found := false
	messages.ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("role").String() == "system" {
			found = true
			return false
		}
		return true
	})

	if found {
		// 追加到现有 system 消息
		body, _ = sjson.SetBytes(body, "messages.0.content",
			gjson.GetBytes(body, "messages.0.content").String()+"\n\n"+searchResults)
	} else {
		// 插入新的 system 消息
		systemMsg := map[string]string{
			"role":    "system",
			"content": searchResults,
		}
		// 在 messages 数组开头插入
		newMessages := []interface{}{systemMsg}
		for _, msg := range messages.Array() {
			newMessages = append(newMessages, msg.Value())
		}
		body, _ = sjson.SetBytes(body, "messages", newMessages)
	}

	return body
}

// removeWebSearchFromBody 从 Chat Completions body 中移除 web_search 工具
func removeWebSearchFromBody(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body
	}

	var newTools []interface{}
	tools.ForEach(func(_, tool gjson.Result) bool {
		if tool.Get("type").String() != "web_search" {
			newTools = append(newTools, tool.Value())
		}
		return true
	})

	body, _ = sjson.SetBytes(body, "tools", newTools)
	return body
}