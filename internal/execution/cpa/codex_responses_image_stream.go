// 原生 Images 以字节块返回 SSE，外层客户端始终消费 Responses SSE。
// 转换器只保存一个未收齐的事件及本次响应的 ID，不缓冲整个图像流。
// 生命周期仍由执行适配器管理，取消、空闲超时和配额观测沿用同一条 CPA 链路。
// 事件必须按 Responses 语义交付，否则客户端无法配对图片与请求。
// 这里不做网络重试，也不从非完成类的图片事件推定已产生最终图片。
// 结果字段没有被文本化，客户端仍能通过标准的图片条目直接消费。
package cpa

import (
	"bytes"
	"encoding/json"
	"errors"

	"gpt-load/internal/execution"
)

// codexResponsesImageStream 是单次派发的流内状态，仅在执行循环中使用。
// 各次尝试分配独立响应 ID，轮换账号时不会复用前一次的事件序号。
type codexResponsesImageStream struct {
	// 复用 CPA 现有的 SSE 边界及 Responses 事件大小限制。
	assembler nativeResponsesSSEAssembler
	// 前导和终态共享同一组 ID；前导始终携带空 output。
	response codexResponsesImageResponse
	item     codexResponsesImageItem
	// sequence 对所有下游事件递增，每个原生事件可展开为多条。
	sequence int
	// terminal 表示成功或错误都已送到下游方言，尾数据不再解释。
	terminal bool
}

// push 将完整的原生 Images 事件转成一个或多个 Responses 事件。
// 在收到真正的图片或失败前不提交 Responses 响应，保留上游拒绝的处理空间。
// 上游读块可能跨 SSE 事件或 base64 中间位置，必须先做增量组帧。
// 解析错误返回给适配器，调用者保留已接收上游响应的事实。
func (stream *codexResponsesImageStream) push(chunk []byte) ([][]byte, error) {
	raw, err := stream.assembler.pushWire(chunk)
	if err != nil {
		return nil, err
	}
	var events [][]byte
	for _, frame := range raw {
		// 已组帧的数据属于这一尝试，未来到达的字节不会覆盖当前解析值。
		name, payload := codexImageSSEFields(frame)
		// 兼容上游的结束标记。标记本身不能证明生成成功。
		// 若此前没有完成事件，调用者在 EOF 报告上游未完成。
		if bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		converted, err := stream.convertEvent(name, payload)
		if err != nil {
			// 一次转换中的错误不发送已经构造但尚未校验完毕的前导。
			return nil, err
		}
		events = append(events, converted...)
		if stream.terminal {
			// 一个生成请求只有一个终态，后续片段不会重复发送完成事件。
			break
		}
	}
	return events, nil
}

// codexImageSSEFields 提取事件名及按 SSE 规则合并的 data 字段。
// 块已经按空行切分，这里仍需兼容 LF、CRLF 以及多条 data 行。
// 连续 data 行以换行连接，保留 JSON 内容中允许的格式空白。
// 事件名可在 SSE 行中声明，也可以只使用 JSON 的 type 字段。
func codexImageSSEFields(frame []byte) (string, []byte) {
	var name string
	var data [][]byte
	for len(frame) != 0 {
		end := bytes.IndexAny(frame, "\r\n")
		if end < 0 {
			end = len(frame)
		}
		field, value, _ := bytes.Cut(frame[:end], []byte(":"))
		// SSE 只忽略冒号后的一个前导空格，不能裁剪图片数据行的其余内容。
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "event":
			name = string(value)
		case "data":
			data = append(data, value)
		}
		// CRLF 的第二个字符会作为空行跳过，不参与字段合并。
		frame = frame[min(end+1, len(frame)):]
	}
	if len(data) == 1 {
		// 常见事件只有一条 data，不再复制大型 base64 字段。
		return name, data[0]
	}
	// 按 SSE 规范拼接数据行，空行和注释不属于 JSON 正文。
	return name, bytes.Join(data, []byte("\n"))
}

// convertEvent 只识别生成所需的部分图片、成功和失败。未知进度事件不会冒充输出。
// 响应 ID 不从 Images 中推断。Responses 客户端看到的是网关映射的这一轮结果。
// 供应商错误既可能是独立事件，也可能附在有图片字段的事件里。
func (stream *codexResponsesImageStream) convertEvent(name string, payload []byte) ([][]byte, error) {
	var source struct {
		Type   string          `json:"type"`
		Base64 string          `json:"b64_json"`
		Index  *int            `json:"partial_image_index"`
		Format string          `json:"output_format"`
		Usage  json.RawMessage `json:"usage"`
		Error  json.RawMessage `json:"error"`
	}
	// 同时出现两个事件类型来源时，冲突意味着无法判定真实状态。
	if json.Unmarshal(payload, &source) != nil || (name != "" && source.Type != "" && name != source.Type) {
		return nil, errors.New("invalid Codex Images SSE event")
	}
	if name == "" {
		// CPA 原生路径允许供应商省略 event 行，仅在 data 内声明事件类型。
		name = source.Type
	}
	var events [][]byte
	// 事件内的错误优先于事件名和图片字段。使用固定消息避免将供应商错误体直接发出。
	// 失败前没有图片时，这条错误是第一帧，网关可按现有规则返回 HTTP 错误。
	// 已有部分图片时同一错误作为 SSE 终止事件交付，客户端可展示失败。
	if name == "error" || name == "image_generation.failed" || codexImageHasError(source.Error) {
		stream.terminal = true
		err := stream.emit(&events, "error", map[string]any{
			"code": "upstream_image_generation_failed", "message": "Codex Images upstream generation failed.", "param": nil,
		})
		return events, err
	}
	if name != "image_generation.partial_image" && name != "image_generation.completed" {
		// 原生心跳和进行中通知不产生客户端图片，也无需分配新的输出项。
		return nil, nil
	}
	// 结果必须能用 Images 的 base64 字段表示。仅返回 URL 时不做二次下载。
	if !codexImageHasBase64(source.Base64) {
		return nil, errors.New("Codex Images SSE event does not contain a base64 image")
	}
	// 只在首个图像事件前发送前导，响应和条目 ID 在整个流中保持一致。
	// 新建条目用于关联随后的部分图片以及最终的 item.done。
	// 上游可能直接给出最终图片，此时也需要先补齐 Responses 的前导。
	if stream.sequence == 0 {
		if err := stream.emit(&events, "response.created", map[string]any{"response": stream.response}); err != nil {
			return nil, err
		}
		if err := stream.emit(&events, "response.output_item.added", map[string]any{"output_index": 0, "item": stream.item}); err != nil {
			return nil, err
		}
	}
	if name == "image_generation.partial_image" {
		// 原生 Images 和 Responses 对部分图片使用不同的 base64 字段名。
		// 保留真实部分图片索引，不从 Responses 序号推算，否则重绘顺序会错。
		if source.Index == nil || *source.Index < 0 {
			return nil, errors.New("invalid Codex Images partial image index")
		}
		err := stream.emit(&events, "response.image_generation_call.partial_image", map[string]any{
			"item_id": stream.item.ID, "output_index": 0, "partial_image_index": *source.Index,
			"partial_image_b64": source.Base64, "output_format": source.Format,
		})
		// 临时图片随事件转发，流状态不保留上一张部分图的 base64。
		return events, err
	}
	// 成功图片在 item.done 和最终 response.output 各出现一次，符合 Responses 的两种消费方式。
	// 二者复用同一个 item ID。计费方可以按条目身份去重，只计一张。
	// 不把部分图片放进最终 output，否则消费者可能误认为有多张完成图。
	stream.item.Status, stream.item.Result, stream.item.OutputFormat = "completed", source.Base64, source.Format
	stream.response.complete(stream.item, source.Usage)
	if err := stream.emit(&events, "response.image_generation_call.completed", map[string]any{
		"item_id": stream.item.ID, "output_index": 0,
	}); err != nil {
		return nil, err
	}
	if err := stream.emit(&events, "response.output_item.done", map[string]any{"output_index": 0, "item": stream.item}); err != nil {
		return nil, err
	}
	// Responses 的 completed 事件结束对话状态，图片的 completed 事件只结束图片调用。
	stream.terminal = true
	// 用量随完成事件下发，复用网关既有的 Responses 用量采集及计费输入。
	// 图片服务没有返回用量时，不能从预计价格或图片大小反推 token 数。
	err := stream.emit(&events, "response.completed", map[string]any{"response": stream.response})
	return events, err
}

// emit 分配协议序号并构造带类型的完整 SSE 事件。检查封装后的大小，
// 避免原生图片恰好符合上游限制但导致下游半途写出超限事件。
// 使用 JSON 的 type 即可让 Responses 客户端区分事件，遵循现有 CPA 的 data 帧形式。
// 只有消息成功纳入发送列表才递增序号，不会因编码失败留下可见空洞。
// 每帧独立通过网关的流式观察逻辑，因此限制按单帧而非累计字节计算。
func (stream *codexResponsesImageStream) emit(events *[][]byte, kind string, fields map[string]any) error {
	fields["type"] = kind
	fields["sequence_number"] = stream.sequence
	body, err := json.Marshal(fields)
	if err != nil {
		// 序列化失败由执行器报告为上游表示异常，不额外伪造完成事件。
		return err
	}
	const prefix = "data: "
	// 两个换行计入下游帧长，包含空白行的完整事件必须在同一限制内。
	if len(body)+len(prefix)+2 > execution.DefaultSSEEventLimitBytes {
		return errors.New("converted Responses image event exceeds limit")
	}
	*events = append(*events, append(append([]byte(prefix), body...), '\n', '\n'))
	stream.sequence++
	return nil
}
