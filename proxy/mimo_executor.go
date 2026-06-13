package proxy

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ==================== MiMo 上游执行器 ====================

// MimoUpstreamConfig MiMo 上游配置
type MimoUpstreamConfig struct {
	BaseURL        string // MiMo API 基础 URL
	APIKey         string // MiMo API Key
	IsTokenPlan    bool   // 是否为 Token Plan 账号
	ModelMapping   string // 账号级别模型映射 JSON
}

// GenerateMimoResponseID 生成 MiMo 响应 ID
func GenerateMimoResponseID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "resp_mimo_" + hex.EncodeToString(b)
}

// HandleMimoResponsesAPI 处理通过 MiMo 上游的 Responses API 请求
// 流程: Responses API body → Chat Completions body → MiMo upstream → Chat chunks → Responses SSE events
func HandleMimoResponsesAPI(c *gin.Context, responsesBody []byte, cfg *MimoUpstreamConfig) {
	isStream := gjson.GetBytes(responsesBody, "stream").Bool()

	// 翻译请求: Responses → Chat Completions
	chatBody, model, err := ResponsesToMimoChat(responsesBody, cfg.IsTokenPlan)
	if err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("failed to translate request: %v", err),
		})
		return
	}

	// 应用账号级别模型映射
	model = ApplyAccountModelMapping(model, cfg.ModelMapping)
	chatBody, _ = sjson.SetBytes(chatBody, "model", model)

	slog.Debug("mimo upstream request",
		"model", model,
		"stream", isStream,
		"isTokenPlan", cfg.IsTokenPlan,
	)

	// 确定 MiMo API 端点
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = ResolveMimoBaseURL("", cfg.APIKey)
	}
	apiURL := strings.TrimRight(baseURL, "/") + "/v1/chat/completions"

	// 生成响应 ID
	responseID := GenerateMimoResponseID()

	// 发起上游请求
	req, err := http.NewRequestWithContext(c.Request.Context(), "POST", apiURL, bytes.NewReader(chatBody))
	if err != nil {
		c.JSON(http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("failed to create request: %v", err),
		})
		return
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Accept", "text/event-stream")

	client := &http.Client{
		Timeout: 0, // 无超时，由 context 控制
	}

	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, map[string]string{
			"error": fmt.Sprintf("mimo upstream error: %v", err),
		})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		slog.Error("mimo upstream error", "status", resp.StatusCode, "body", string(bodyBytes))
		
		// 友好错误处理
		errMsg := string(bodyBytes)
		if resp.StatusCode == 400 && strings.Contains(errMsg, "webSearchEnabled is false") {
			errMsg = "MiMo Web Search 插件未激活。请在 https://platform.xiaomimimo.com/#/console/plugin 激活后，" +
				"设置环境变量 MIMO_FORCE_WEB_SEARCH=true，或移除请求中的 web_search 工具。" +
				"原始错误: " + errMsg
		}
		
		c.JSON(resp.StatusCode, map[string]string{
			"error": errMsg,
		})
		return
	}

	if isStream {
		handleMimoStreamResponse(c, resp.Body, responseID)
	} else {
		handleMimoNonStreamResponse(c, resp.Body, responseID)
	}
}

// HandleMimoChatCompletionsAPI 处理通过 MiMo 上游的 Chat Completions API 请求
// 流程: Chat Completions body → MiMo 格式化 → MiMo upstream → 原样返回
func HandleMimoChatCompletionsAPI(c *gin.Context, chatBody []byte, cfg *MimoUpstreamConfig) {
	isStream := gjson.GetBytes(chatBody, "stream").Bool()

	// 翻译请求: 标准 Chat → MiMo Chat
	mimoBody, model, err := TranslateChatCompletionsToMimo(chatBody)
	if err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("failed to translate request: %v", err),
		})
		return
	}

	// 应用账号级别模型映射
	model = ApplyAccountModelMapping(model, cfg.ModelMapping)
	mimoBody, _ = sjson.SetBytes(mimoBody, "model", model)

	slog.Debug("mimo chat completions request",
		"model", model,
		"stream", isStream,
	)

	// 确定 MiMo API 端点
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = ResolveMimoBaseURL("", cfg.APIKey)
	}
	apiURL := strings.TrimRight(baseURL, "/") + "/v1/chat/completions"

	// 发起上游请求
	req, err := http.NewRequestWithContext(c.Request.Context(), "POST", apiURL, bytes.NewReader(mimoBody))
	if err != nil {
		c.JSON(http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("failed to create request: %v", err),
		})
		return
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	if isStream {
		req.Header.Set("Accept", "text/event-stream")
	}

	client := &http.Client{Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, map[string]string{
			"error": fmt.Sprintf("mimo upstream error: %v", err),
		})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		
		// 友好错误处理
		errMsg := string(bodyBytes)
		if resp.StatusCode == 400 && strings.Contains(errMsg, "webSearchEnabled is false") {
			errMsg = "MiMo Web Search 插件未激活。请在 https://platform.xiaomimimo.com/#/console/plugin 激活后重试。" +
				"原始错误: " + errMsg
		}
		
		c.JSON(resp.StatusCode, map[string]string{
			"error": errMsg,
		})
		return
	}

	if isStream {
		// 流式: 透传 MiMo 的 SSE 流
		handleMimoPassthroughStream(c, resp.Body)
	} else {
		// 非流式: 透传响应
		c.Header("Content-Type", "application/json")
		io.Copy(c.Writer, resp.Body)
	}
}

// ==================== 流式响应处理 ====================

// handleMimoStreamResponse 处理 MiMo 流式响应，翻译为 Responses SSE 格式
func handleMimoStreamResponse(c *gin.Context, body io.Reader, responseID string) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		// 回退到非流式处理
		handleMimoNonStreamResponse(c, body, responseID)
		return
	}

	// 发送 response.created 事件
	createdEvent := buildResponsesCreatedEvent(responseID)
	fmt.Fprintf(c.Writer, "%s\n\n", createdEvent)
	flusher.Flush()

	scanner := bufio.NewScanner(body)
	// 增大缓冲区以处理大型 reasoning_content
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	var currentData strings.Builder

	for scanner.Scan() {
		line := scanner.Text()

		// 检查 context 取消
		select {
		case <-c.Request.Context().Done():
			return
		default:
		}

		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				break
			}
			currentData.WriteString(data)
		} else if line == "" && currentData.Len() > 0 {
			// 一个完整的 SSE 事件
			chunk := []byte(currentData.String())
			currentData.Reset()

			events, err := ChatChunkToResponsesEvent(chunk, responseID)
			if err != nil {
				slog.Error("failed to translate chunk", "error", err)
				continue
			}

			for _, event := range events {
				if event != "" {
					fmt.Fprintf(c.Writer, "%s\n\n", event)
					flusher.Flush()
				}
			}
		}
	}

	// 处理最后的数据
	if currentData.Len() > 0 {
		chunk := []byte(currentData.String())
		events, _ := ChatChunkToResponsesEvent(chunk, responseID)
		for _, event := range events {
			if event != "" {
				fmt.Fprintf(c.Writer, "%s\n\n", event)
				flusher.Flush()
			}
		}
	}

	// 确保发送 completed 事件
	fmt.Fprintf(c.Writer, "%s\n\n", buildResponsesCompletedEvent(responseID))
	flusher.Flush()
}

// handleMimoNonStreamResponse 处理 MiMo 非流式响应，翻译为 Responses 格式
func handleMimoNonStreamResponse(c *gin.Context, body io.Reader, responseID string) {
	bodyBytes, err := io.ReadAll(body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("failed to read response: %v", err),
		})
		return
	}

	// 翻译为 Responses 格式
	events, err := ChatChunkToResponsesEvent(bodyBytes, responseID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("failed to translate response: %v", err),
		})
		return
	}

	// 对于非流式响应，提取 response.completed 事件中的 response 对象
	for _, event := range events {
		if strings.Contains(event, "response.completed") {
			parts := strings.SplitN(event, "data: ", 2)
			if len(parts) == 2 {
				c.Header("Content-Type", "application/json")
				c.String(http.StatusOK, parts[1])
				return
			}
		}
	}

	// 如果没有找到 completed 事件，返回原始翻译结果
	if len(events) > 0 {
		c.Header("Content-Type", "text/event-stream")
		for _, event := range events {
			fmt.Fprintf(c.Writer, "%s\n\n", event)
		}
		return
	}

	c.JSON(http.StatusInternalServerError, map[string]string{
		"error": "empty response from mimo upstream",
	})
}

// handleMimoPassthroughStream 透传 MiMo SSE 流（用于 Chat Completions API）
func handleMimoPassthroughStream(c *gin.Context, body io.Reader) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		io.Copy(c.Writer, body)
		return
	}

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		fmt.Fprintf(c.Writer, "%s\n", line)
		if line == "" {
			flusher.Flush()
		}
	}
}

// ==================== 辅助函数 ====================

func buildResponsesCreatedEvent(responseID string) string {
	resp := map[string]any{
		"id":         responseID,
		"object":     "response",
		"status":     "in_progress",
		"model":      "mimo",
		"output":     []any{},
	}
	data, _ := json.Marshal(resp)
	return "event: response.created\ndata: " + string(data)
}

// ApplyAccountModelMapping 应用账号级别模型映射
// mapping 格式: {"gpt-4o": "mimo-v2.5-pro", "o3-mini": "mimo-v2-flash"}
func ApplyAccountModelMapping(model string, mappingJSON string) string {
	if mappingJSON == "" || mappingJSON == "{}" {
		return model
	}

	var mapping map[string]string
	if err := json.Unmarshal([]byte(mappingJSON), &mapping); err != nil {
		return model
	}

	if mapped, ok := mapping[model]; ok {
		return mapped
	}

	return model
}

// ==================== Codex 工具输出检测 ====================

// checkCodexComplete 检查 Codex 是否完成了任务
// 通过检测特定的函数调用来判断
func checkCodexComplete(chunkData []byte) bool {
	if !gjson.ValidBytes(chunkData) {
		return false
	}

	choices := gjson.GetBytes(chunkData, "choices")
	if !choices.IsArray() {
		return false
	}

	complete := false
	choices.ForEach(func(_, choice gjson.Result) bool {
		if choice.Get("finish_reason").String() != "" {
			complete = true
			return false // 停止遍历
		}
		return true
	})

	return complete
}

// HandleMimoUpstream 处理 MiMo 上游请求的统一分发
// 根据请求路径判断是 Responses API 还是 Chat Completions API
func HandleMimoUpstream(c *gin.Context, reqInfo *RequestContext, cfg *MimoUpstreamConfig) {
	path := c.Request.URL.Path

	if strings.Contains(path, "/responses") {
		HandleMimoResponsesAPI(c, reqInfo.Body, cfg)
	} else if strings.Contains(path, "/chat/completions") {
		HandleMimoChatCompletionsAPI(c, reqInfo.Body, cfg)
	} else {
		// 默认走 Responses API
		HandleMimoResponsesAPI(c, reqInfo.Body, cfg)
	}
}

// RequestContext 请求上下文信息
type RequestContext struct {
	Body     []byte
	Stream   bool
	Model    string
	IsCodex  bool
}

// DetermineUpstreamType 判断请求应该路由到哪个上游
// 返回 "mimo", "codex", "openai"
func DetermineUpstreamType(model string, accountUpstreamType string) string {
	// 优先使用账号配置的上游类型
	if accountUpstreamType != "" {
		return strings.ToLower(strings.TrimSpace(accountUpstreamType))
	}

	// 根据模型名称判断
	if IsMimoModel(model) {
		return UpstreamTypeMimo
	}

	// 默认走 Codex
	return "codex"
}

// HandleUpstreamRequest 统一的上游请求处理入口
func HandleUpstreamRequest(c *gin.Context, reqBody []byte, upstreamType string, cfg *MimoUpstreamConfig) {
	switch upstreamType {
	case UpstreamTypeMimo:
		HandleMimoUpstream(c, &RequestContext{Body: reqBody}, cfg)
	default:
		// 默认走原始 Codex 处理
		c.JSON(http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("unsupported upstream type: %s", upstreamType),
		})
	}
}