// 本文件适配 Codex 的单轮文本生图。入站和出站客户端均使用 Responses，
// 只有选中的 Codex 尝试改走现有 CPA Images 执行器。
// 通用调度仍使用客户端操作，以保留模型授权和请求日志。
// 上游协议证据来自生图尝试，供错误分类和重放判断使用。
// 本转换不负责解释文本模型的 image_generation 工具调用。
// 普通输出与 SSE 完成事件共享条目定义，让客户端使用同一个提取路径。
package cpa

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"gpt-load/internal/execution"
)

// SDK 根据客户端格式和路径选择具体上游端点，不能只替换其中一项。
const codexImagesGenerationPath = "/v1/images/generations"

// 使用固定错误避免把提示词、历史 ID 或未知字段名带进日志和客户端错误。
// 输入错误属于本次请求，不应占用凭据、降低账号权重或轮换账号。
var errCodexImageConversionInput = errors.New("Codex Images conversion requires a single text prompt")

// codexResponsesImagePayload 按选中的上游模型构造生成请求。
// 拒绝未声明的字段，避免将参考图、工具、续接或采样选项悄悄丢掉。
// 流式模式以执行入口为准，避免下游和上游采用不同的响应格式。
// 入站 model 仍校验为字符串，但授权后的映射模型才是 Images 实际目标。
// 这里也拒绝显式存储，网关已经按渠道原有规则处理可降级的存储偏好。
func codexResponsesImagePayload(body []byte, model string, stream bool) ([]byte, error) {
	var source struct {
		Model  string          `json:"model"`
		Input  json.RawMessage `json:"input"`
		Stream bool            `json:"stream"`
		Store  bool            `json:"store"`
		// 网关已有文本推理档位观测。生图执行器不接受该字段，也不向上游传递。
		Reasoning map[string]json.RawMessage `json:"reasoning"`
		// 允许 SDK 序列化空工具数组，实际声明任何工具都拒绝转换。
		// 工具的执行与这些直接图片参数属于不同协议语义。
		Tools []json.RawMessage `json:"tools"`
		// 这些图片选项可直接发给 Images，由生图上游校验可用值。
		// 不复制供应商参数枚举，防止 SDK 支持新增规格时网关先行拒绝。
		Size          string `json:"size,omitempty"`
		Quality       string `json:"quality,omitempty"`
		OutputFormat  string `json:"output_format,omitempty"`
		PartialImages *int   `json:"partial_images,omitempty"`
	}
	if !decodeCodexImageInput(body, &source) || source.Store || len(source.Tools) != 0 {
		return nil, errCodexImageConversionInput
	}
	prompt := codexResponsesImagePrompt(source.Input)
	// 空段与空白可以自然拼接，但整体没有绘制指令时不消耗订阅额度。
	if strings.TrimSpace(prompt) == "" {
		return nil, errCodexImageConversionInput
	}
	// 用新的 JSON 对象只写入生成参数。生成接口默认返回一张 base64 图片。
	// 不覆盖网关解析的目标模型，公开别名仍由下游响应改写处理。
	// partial_images 的显式零值需要保留，表示只期待最终图片。
	return json.Marshal(struct {
		Model         string `json:"model"`
		Prompt        string `json:"prompt"`
		Stream        bool   `json:"stream"`
		Size          string `json:"size,omitempty"`
		Quality       string `json:"quality,omitempty"`
		OutputFormat  string `json:"output_format,omitempty"`
		PartialImages *int   `json:"partial_images,omitempty"`
	}{model, prompt, stream, source.Size, source.Quality, source.OutputFormat, source.PartialImages})
}

// codexResponsesImagePrompt 接受字符串或一条用户消息。
// 多段文本按原顺序以换行拼接，任何非文本段都会拒绝整条消息。
// 普通字符串没有角色或历史，直接保留它对布局和文字的要求。
func codexResponsesImagePrompt(input json.RawMessage) string {
	var text string
	if json.Unmarshal(input, &text) == nil {
		return text
	}
	// 可选 type 表示简写消息；带历史条目 ID、状态或其他字段的消息不能转换。
	// 只接受 user 角色，防止把系统指令或助手历史改写成用户生图请求。
	var messages []struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if !decodeCodexImageInput(input, &messages) || len(messages) != 1 || messages[0].Role != "user" ||
		(messages[0].Type != "" && messages[0].Type != "message") {
		return ""
	}
	if json.Unmarshal(messages[0].Content, &text) == nil {
		return text
	}
	// 保留段内空格及换行，避免修改提示词里的样式或排版指令。
	// 只允许客户端输入文本，output_text 即使有内容也表示不受支持的历史。
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if !decodeCodexImageInput(messages[0].Content, &parts) {
		return ""
	}
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if part.Type != "input_text" {
			return ""
		}
		texts = append(texts, part.Text)
	}
	return strings.Join(texts, "\n")
}

// body 已由执行契约校验为单个 JSON 值，嵌套输入来自同一份已解析的 JSON。
// 此处额外校验字段集合，不替换网关的通用请求校验。
// 顶层和嵌套使用同一套严格解码，避免部分字段因层级不同而被静默忽略。
func decodeCodexImageInput(body []byte, target any) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target) == nil
}

// 生成结果是一次无状态推理。这里生成的 ID 只用于关联本次 Responses
// JSON 或事件，不登记上游归属，也不伪装成可用于续接的 Codex 资源。
// Responses 必须返回模型与输出数组，哪怕上游图片响应没有这些元数据。
// tools 为空表明这是网关适配的直接生成，客户端没有发起额外工具流程。
// Usage 保留原始 JSON，让图片和文本 token 明细能独立于适配器演进。
type codexResponsesImageResponse struct {
	ID                string                    `json:"id"`
	Object            string                    `json:"object"`
	Model             string                    `json:"model"`
	CreatedAt         int64                     `json:"created_at"`
	Status            string                    `json:"status"`
	Output            []codexResponsesImageItem `json:"output"`
	Usage             json.RawMessage           `json:"usage,omitempty"`
	Store             bool                      `json:"store"`
	ParallelToolCalls bool                      `json:"parallel_tool_calls"`
	ToolChoice        string                    `json:"tool_choice"`
	Tools             []json.RawMessage         `json:"tools"`
}

// 最终图片作为独立条目返回，便于客户端提取及按同一条目 ID 计费去重。
// 流式前导只带 ID 和进行中状态，结果字段留给完成事件。
type codexResponsesImageItem struct {
	ID            string `json:"id"`
	Type          string `json:"type"`
	Status        string `json:"status"`
	Result        string `json:"result,omitempty"`
	OutputFormat  string `json:"output_format,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

// newCodexResponsesImageResponse 为普通和流式路径分配同一结构的 ID 和初始状态。
// 只有收到完整图片才会填充 output 和 completed 状态。
// response 和 item 使用不同 ID 前缀，同一流的事件依赖它们稳定关联。
// 不用请求 ID 或凭据派生这些值，避免把内部追踪信息带到客户端。
// 初始时刻为本地响应创建时刻，适用于不提供时间戳的原生图片事件。
// 该时刻在同一请求的所有流式事件中保持稳定。
func newCodexResponsesImageResponse(model string) (codexResponsesImageResponse, codexResponsesImageItem) {
	id := strings.ReplaceAll(uuid.NewString(), "-", "")
	return codexResponsesImageResponse{
		ID: "resp_" + id, Object: "response", Model: model, CreatedAt: time.Now().Unix(),
		Status: "in_progress", Output: []codexResponsesImageItem{},
		ToolChoice: "none", Tools: []json.RawMessage{},
	}, codexResponsesImageItem{ID: "ig_" + id, Type: "image_generation_call", Status: "in_progress"}
}

// 完成状态只由一个确实收到的生成结果触发。沿用原始用量对象，
// 保留文本及图片 token 明细，缺失用量时保持缺失。
// 两种传输共用同一终态结构，避免流式计费与普通请求所见结果不同。
func (response *codexResponsesImageResponse) complete(item codexResponsesImageItem, usage json.RawMessage) {
	response.Status = "completed"
	response.Output = []codexResponsesImageItem{item}
	response.Usage = usage
}

// convertCodexResponsesImageResponse 将普通 Images 响应转为 Responses JSON。
// 转换仅请求一张图，空结果、多个结果或仅返回 URL 时均显式失败。
// 普通响应没有额外网络步骤，所有需下发的图片数据由当前尝试提供。
// 拒绝同时出现 error 和图像，避免把供应商报告的失败写成成功。
// 成功响应无上游模型身份字段时，客户端看到选中的模型或它的公开别名。
func convertCodexResponsesImageResponse(payload []byte, model string) ([]byte, error) {
	// 转换前就检查已知的 Responses 资源上限，避免为明显过大的结果另建副本。
	if int64(len(payload)) > execution.DefaultUnaryResponseBodyLimitBytes {
		return nil, errors.New("Codex Images response exceeds the Responses body limit")
	}
	var source struct {
		Created      int64           `json:"created"`
		OutputFormat string          `json:"output_format"`
		Error        json.RawMessage `json:"error"`
		Data         []struct {
			Base64        string `json:"b64_json"`
			RevisedPrompt string `json:"revised_prompt"`
		} `json:"data"`
		Usage json.RawMessage `json:"usage"`
	}
	// 响应只投影协议明确需要的字段，避免泄露上游附带的私有元数据。
	// 先确认 data 基数，再访问首图，空结果直接交给执行边界报错。
	if json.Unmarshal(payload, &source) != nil || codexImageHasError(source.Error) ||
		len(source.Data) != 1 || !codexImageHasBase64(source.Data[0].Base64) {
		return nil, errors.New("Codex Images response does not contain one base64 image")
	}
	response, item := newCodexResponsesImageResponse(model)
	// 原生普通响应提供创建时刻。上游省略该字段时沿用本地响应创建时刻。
	// 不把不合法的负数时间戳写给下游。
	if source.Created > 0 {
		response.CreatedAt = source.Created
	}
	item.Status, item.Result = "completed", source.Data[0].Base64
	// 使用上游报告的实际格式和修订提示，不从请求参数推断结果格式。
	item.OutputFormat, item.RevisedPrompt = source.OutputFormat, source.Data[0].RevisedPrompt
	response.complete(item, source.Usage)
	return json.Marshal(response)
}

// 仅校验图片结果的 base64 表示，不解码像素，也不为大型图片分配完整解码缓冲。
// 流式部分图片与普通响应共用此验证，失败时不能构造成功输出条目。
// 空字符串及无法解码的占位内容不构成真实的图片输出。
func codexImageHasBase64(value string) bool {
	n, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding, strings.NewReader(value)))
	return err == nil && n > 0
}

// Images 允许显式 error:null；有内容的错误对象不能与成功图片一起转发。
// 缺省值和显式 null 都意味着供应商没有报告这个错误字段。
func codexImageHasError(raw json.RawMessage) bool {
	value := bytes.TrimSpace(raw)
	return len(value) != 0 && !bytes.Equal(value, []byte("null"))
}
