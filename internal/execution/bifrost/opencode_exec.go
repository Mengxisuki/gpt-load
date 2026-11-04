// 本文件连接客户端的 custom 工具合同与目标的 Chat 函数工具合同。
// 转换只发生在已选择 OpenCode Chat 路由的请求中，原生 Responses 不经过这里。
// 支持范围由声明文法限定，不能根据模型名猜测任意自由文本工具的含义。
// 请求端将源码放进一个 JSON 字符串，响应端校验后再还原自由文本。
// 适配器不执行源码，客户端收到的工具仍由客户端自己的执行环境运行。
package bifrost

import (
	"encoding/json"
	"errors"
	"io"
	"strings"

	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
)

// openCodeExecGrammar 是现场 exec 的文法，不是一个通用 Lark 解释器。
// plain_source 已覆盖所有非空字符串，pragma 分支不缩小可接受语言。
// 因此 JSON Schema 的非空字符串与该文法等价，无需添加解析器依赖。
// 只去除定义两端空白；其他变动必须重新确认语义，不能自动放宽约束。
const openCodeExecGrammar = `start: pragma_source | plain_source
pragma_source: PRAGMA_LINE NEWLINE SOURCE
plain_source: SOURCE

PRAGMA_LINE: /[ \t]*\/\/ @exec:[^\r\n]*/
NEWLINE: /\r?\n/
SOURCE: /[\s\S]+/`

// 顶层声明返回时不额外增加 namespace；functions 容器则恢复原容器名。
// 这里只保存恢复需要的信息，原工具描述在转为函数时直接保留。
// 不保存历史源码或响应参数，避免把请求数据延长到下一次执行。
// 每个 attempt 独立保存默认命名空间，响应恢复不依赖全局状态或缓存。
// 当前只适配默认命名空间的 exec，其他自定义工具仍在发送前拒绝。
// 普通函数及其他命名空间下的同名函数不会被改成 custom。
type openCodeExecBridge struct{ namespace string }

// prepareOpenCodeExecBridge 在动态工具提升后、工具选择校验前转换声明和历史。
// 调用者已拒绝加密与托管历史，此处只处理有明确等价表达的 exec。
// 不执行任何代码，也不把历史调用的输入拼成提示词或工具描述。
func prepareOpenCodeExecBridge(request *schemas.BifrostResponsesRequest) (*schemas.BifrostResponsesRequest, *openCodeExecBridge, error) {
	// 此函数接收本次 attempt 从原始正文新解码出的 DTO，可以就地替换字段。
	// SDK 解包函数可能浅拷贝，因此不能把这个 DTO 当作可跨请求复用的对象。
	var bridge *openCodeExecBridge
	execNames := 0
	// 只读遍历原声明，先确认所有 custom 都可支持，再生成新的函数声明。
	// functions 是 SDK 的默认命名空间，其他 namespace 保留原有函数处理。
	// 队列携带声明所在的命名空间；工具自身的 name 不足以确定归属。
	// 嵌套容器的路径会累积，防止外部容器中的 functions 冒充默认空间。
	type declared struct {
		tool      schemas.ResponsesTool
		namespace string
	}
	var queue []declared
	if request.Params != nil {
		for _, tool := range request.Params.Tools {
			queue = append(queue, declared{tool: tool})
		}
	}
	// 必须遍历全部声明：未知 custom 即使本轮未被选中也不能静默丢失。
	// 先完成检查，再执行转换，避免部分成功的声明列表进入 SDK。
	for len(queue) > 0 {
		entry := queue[0]
		queue = queue[1:]
		tool := entry.tool
		// 空或普通函数容器的基础形状沿用已有入口校验，此处只追踪 custom。
		// 不在这里改名，后续统一交给 SDK 的默认空间解包与别名处理。
		if tool.Type == schemas.ResponsesToolTypeNamespace && tool.ResponsesToolNamespace != nil {
			name := ""
			if tool.Name != nil {
				name = *tool.Name
			}
			if entry.namespace != "" {
				name = entry.namespace + "." + name
			}
			for _, child := range tool.Tools {
				queue = append(queue, declared{child, name})
			}
			continue
		}
		// 顶层声明没有 functions 容器时，SDK 不检查默认名字冲突。
		// 同一裸 exec 不能既是函数又是 custom，否则响应恢复无法判断类型。
		if tool.Name != nil && *tool.Name == "exec" && (entry.namespace == "" || entry.namespace == "functions") {
			execNames++
		}
		if tool.Type != schemas.ResponsesToolTypeCustom {
			continue
		}
		// 精确识别文法，而不是只根据工具名信任客户端声明。
		// 有多个默认 exec 时不能判断返回归属，因此不接受重复声明。
		if bridge != nil || tool.Name == nil || *tool.Name != "exec" || (entry.namespace != "" && entry.namespace != "functions") || !hasOpenCodeExecGrammar(tool) {
			return nil, nil, errors.New("Chat conversion supports only the declared exec non-empty-source grammar")
		}
		// 保存客户端声明的空间而非 Chat 的裸函数名，以便恢复输出形状。
		// 格式对象只参与识别，不透传为目标模型不认识的扩展参数。
		bridge = &openCodeExecBridge{namespace: entry.namespace}
	}
	if bridge == nil {
		// 没有声明时不能猜测历史 custom 的契约，也不能把未知工具结果强行改名。
		for _, item := range request.Input {
			if item.Type != nil && (*item.Type == schemas.ResponsesMessageTypeCustomToolCall || *item.Type == schemas.ResponsesMessageTypeCustomToolCallOutput) {
				return nil, nil, errors.New("custom history requires a supported exec declaration")
			}
		}
		return request, nil, nil
	}
	// 即使未使用强制 tool_choice，也必须拒绝声明冲突。
	// 模型生成裸函数名后已没有足够信息区分原声明类型。
	if execNames != 1 {
		return nil, nil, errors.New("exec declaration has conflicting tool names")
	}
	// 复用 SDK 的默认命名空间拆包及重名校验，避免自建第二套别名规则。
	// 返回的是请求副本，原始 AttemptSpec.Body 始终不变。
	normalized, err := providerUtils.UnwrapDefaultNamespaceTools(request)
	if err != nil {
		return nil, nil, errors.New("exec declaration has conflicting tool names")
	}
	// 展平其他命名空间由 Compatible 发送入口完成，这里只转换默认 custom。
	// 该顺序保留外部命名空间同名函数的身份，响应端不会将其误识别为 exec。
	request = normalized
	for i, tool := range request.Params.Tools {
		if tool.Type != schemas.ResponsesToolTypeCustom {
			continue
		}
		// 上游只需要 input 参数；保留原工具说明，并明确输入是完整原始源码。
		// JSON Schema 是生成约束，响应端仍会独立校验，不能依赖模型一定遵守。
		// required 与 additionalProperties 同时限制包装形状，不给模型增加可选参数。
		// minLength 对应文法的加号量词；纯空白仍是合法的非空源码输入。
		properties := schemas.NewOrderedMap()
		properties.Set("input", map[string]any{"type": "string", "minLength": 1, "description": "The complete raw source text, including any optional // @exec: pragma. Do not wrap it in Markdown."})
		// 清掉 custom 联合类型分支，避免序列化时混出两套工具格式。
		// 不开启 strict：转换不能额外要求所有 Chat 目标支持结构化工具约束。
		tool.Type = schemas.ResponsesToolTypeFunction
		tool.ResponsesToolCustom = nil
		tool.ResponsesToolFunction = &schemas.ResponsesToolFunction{Parameters: &schemas.ToolFunctionParameters{Type: "object", Properties: properties, Required: []string{"input"}, AdditionalProperties: &schemas.AdditionalPropertiesStruct{AdditionalPropertiesBool: schemas.Ptr(false)}}}
		request.Params.Tools[i] = tool
	}
	// 转换历史早于工具白名单检查，使声明、历史和选择采用同一种函数类型。
	// 若历史不可移植，调用者会返回 not_sent，不会先消费上游再报告错误。
	if err := bridge.convertHistory(request); err != nil {
		return nil, nil, err
	}
	// 强制工具选择也必须同步改为 function，不能在上游丢成 auto。
	// SDK 的 allowed_tools 过滤在此之后执行，沿用它的白名单约束。
	if request.Params.ToolChoice != nil && request.Params.ToolChoice.ResponsesToolChoiceStruct != nil {
		choice := request.Params.ToolChoice.ResponsesToolChoiceStruct
		if choice.Type == schemas.ResponsesToolChoiceTypeCustom {
			if choice.Name == nil || *choice.Name != "exec" {
				return nil, nil, errors.New("custom tool choice must select exec")
			}
			choice.Type = schemas.ResponsesToolChoiceTypeFunction
		}
		// 普通函数选择保持原样，只改已经确认语义的 custom exec。
		// allowed_tools 的 mode 与成员数量保留，实际裁剪仍由现有逻辑负责。
		for i, allowed := range choice.Tools {
			if allowed.Type != "custom" {
				continue
			}
			if allowed.Name == nil || *allowed.Name != "exec" {
				return nil, nil, errors.New("custom allowlist must select exec")
			}
			allowed.Type = "function"
			choice.Tools[i] = allowed
		}
	}
	return request, bridge, nil
}

// hasOpenCodeExecGrammar 验证可以无损转成非空字符串的已知声明。
// type、syntax、definition 三项必须同时匹配，缺失值不采用猜测的默认值。
// 只识别语法等价性已确认的这个版本，不判断任意 Lark 文法是否等价。
// 若客户端将来改变定义，应增加独立协议样例后再决定支持范围。
// 允许定义两端换行，是为了兼容 JSON 文本常见的多行字符串写法。
// 不规范化内部空白，因为正则表达式中的空格可能改变可接受语言。
func hasOpenCodeExecGrammar(tool schemas.ResponsesTool) bool {
	if tool.ResponsesToolCustom == nil || tool.Format == nil {
		return false
	}
	format := tool.Format
	return format.Type == "grammar" && format.Syntax != nil && *format.Syntax == "lark" &&
		format.Definition != nil && strings.TrimSpace(*format.Definition) == openCodeExecGrammar
}

// convertHistory 把完整 custom 回合转为 Chat 可表达的函数调用及结果。
// 调用关系按输入顺序验证，结果必须跟在调用之后且只能提交一次。
// 这不是远端历史查询：未携带的调用无法补全，直接返回明确错误。
// 只修改本次已解码的请求，不触碰冻结的原始正文或其他 attempt。
func (bridge *openCodeExecBridge) convertHistory(request *schemas.BifrostResponsesRequest) error {
	// 维护本次历史内的调用关联，拒绝孤立或重复的 custom 结果。
	// 一次调用尚未返回结果是合法的，但已有结果不能指向别的函数。
	calls := make(map[string]bool)
	for i, item := range request.Input {
		if item.Type == nil || (*item.Type != schemas.ResponsesMessageTypeCustomToolCall && *item.Type != schemas.ResponsesMessageTypeCustomToolCallOutput) {
			continue
		}
		if item.ResponsesToolMessage == nil || item.CallID == nil || *item.CallID == "" {
			return errors.New("exec history requires call_id")
		}
		// 单独复制嵌入消息，防止 SDK 内其他视图仍引用被改写的联合类型。
		// 调用 ID、状态及输出内容不参与名字重映射。
		message := *item.ResponsesToolMessage
		item.ResponsesToolMessage = &message
		if *item.Type == schemas.ResponsesMessageTypeCustomToolCall {
			if !bridge.isExec(&message) || message.ResponsesCustomToolCall == nil || message.ResponsesCustomToolCall.Input == "" {
				return errors.New("exec history requires non-empty source input")
			}
			// false 表示已返回结果，不是不存在；重复调用仍须拒绝。
			// 这样可以区分等待结果与完成状态，而无需持久化历史状态机。
			if _, exists := calls[*item.CallID]; exists {
				return errors.New("duplicate exec history call_id")
			}
			calls[*item.CallID] = true
			// 由标准 JSON 编码器保留换行、引号和 Unicode，禁止手拼参数字符串。
			// 下一轮 Chat 工具历史必须与首轮交给客户端的原始文本完全对应。
			args, marshalErr := json.Marshal(map[string]string{"input": message.ResponsesCustomToolCall.Input})
			if marshalErr != nil {
				return marshalErr
			}
			item.Type = schemas.Ptr(schemas.ResponsesMessageTypeFunctionCall)
			message.Arguments = schemas.Ptr(string(args))
			message.ResponsesCustomToolCall = nil
		} else {
			if !calls[*item.CallID] || message.Output == nil {
				return errors.New("exec result requires its preceding call and output")
			}
			// 输出可以是字符串或内容数组；本层只改变类型，复用既有结果转换。
			// 缺失 output 与空文本不同，后者是工具执行的合法返回值。
			calls[*item.CallID] = false
			item.Type = schemas.Ptr(schemas.ResponsesMessageTypeFunctionCallOutput)
		}
		request.Input[i] = item
	}
	return nil
}

// SDK 展平其他命名空间时会生成别名，调用本判断前须先恢复该别名。
// 否则普通函数的名字可能碰巧与 exec 相同，导致误解包它的参数。
// 默认 functions 与省略 namespace 等价；其他命名空间的普通 exec 保持原样。
// 这个判断同时用于历史和响应，保证名字恢复与请求改写的边界一致。
func (b *openCodeExecBridge) isExec(item *schemas.ResponsesToolMessage) bool {
	return item != nil && item.Name != nil && *item.Name == "exec" && (item.Namespace == nil || *item.Namespace == "" || *item.Namespace == "functions")
}

// decodeExecInput 只接受一个 input 字符串；重复字段、额外字段及尾随 JSON 均拒绝。
// 严格解包防止把格式损坏的模型输出交给本地执行器；错误不包含生成的源码。
// 非空字符串就是受支持 grammar 的完整约束，包括空白和 Unicode 文本。
func decodeExecInput(arguments string) (string, error) {
	// 使用 token 顺序校验，避免 map 解码覆盖重复 input 字段。
	// 错误只包含固定诊断，不拼接可能含用户数据的 arguments。
	decoder := json.NewDecoder(strings.NewReader(arguments))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return "", errors.New("invalid exec input envelope")
	}
	token, err = decoder.Token()
	if err != nil || token != "input" {
		return "", errors.New("exec arguments must contain only input")
	}
	// Decode 到字符串会拒绝对象、数组和数字；空值及 null 被非空约束拒绝。
	// 不 TrimSpace 源码，语法允许的缩进与末尾换行必须原样保留。
	var input string
	if err = decoder.Decode(&input); err != nil || input == "" {
		return "", errors.New("exec input must be a non-empty string")
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim('}') {
		return "", errors.New("exec arguments must contain only input")
	}
	// 结束对象之后只能有 JSON 空白，第二个值或损坏尾部都不是有效包装。
	// 不能忽略尾部并将部分合法内容当作完整调用交给客户端。
	if _, err = decoder.Token(); err != io.EOF {
		return "", errors.New("trailing exec arguments")
	}
	return input, nil
}

// restoreItem 创建新工具消息，避免改写 SDK 留给后续流事件的 JSON 参数状态。
// added 阶段只恢复身份；done 和最终快照必须先通过完整参数校验。
func (b *openCodeExecBridge) restoreItem(item schemas.ResponsesMessage, added bool) (schemas.ResponsesMessage, error) {
	if item.Type == nil || *item.Type != schemas.ResponsesMessageTypeFunctionCall || !b.isExec(item.ResponsesToolMessage) {
		return item, nil
	}
	// added 事件只是工具身份通知，此时参数可能尚未到达。
	// 源码只能在完整结束事件中解包，避免发布半个转义序列或无效 JSON。
	source := ""
	if !added {
		if item.Arguments == nil {
			return item, errors.New("exec call has no arguments")
		}
		var err error
		source, err = decodeExecInput(*item.Arguments)
		if err != nil {
			return item, err
		}
	}
	// 清除 arguments 后再设置 custom 的 input，避免对外暴露两套互斥字段。
	// 保留 SDK 生成的 item ID 与上游 call_id，客户端据此关联下一轮结果。
	message := *item.ResponsesToolMessage
	message.Arguments = nil
	message.ResponsesCustomToolCall = &schemas.ResponsesCustomToolCall{Input: source}
	if b.namespace != "" {
		message.Namespace = schemas.Ptr(b.namespace)
	}
	item.ResponsesToolMessage = &message
	item.Type = schemas.Ptr(schemas.ResponsesMessageTypeCustomToolCall)
	return item, nil
}

// 普通 Compatible 请求没有 bridge，空接收器必须成为无操作。
// 这样共享响应入口无需为原生函数工具维护另一条复制的处理路径。
// 调用方在失败时负责附带上游用量，本层不推测或修改 Token 数量。
// 普通返回和流式终态共用同一恢复函数，避免两条链路的工具类型漂移。
// 只替换 Output 切片；用量、模型、终止状态和其他输出完整保留。
func (b *openCodeExecBridge) restoreResponse(response *schemas.BifrostResponsesResponse) error {
	if b == nil || response == nil {
		return nil
	}
	// 先构造完整新切片，任一工具失败时不发布部分恢复的终态响应。
	// 普通文本、普通函数和第三方命名空间工具仍保留各自原类型。
	output := make([]schemas.ResponsesMessage, len(response.Output))
	for i, item := range response.Output {
		restored, err := b.restoreItem(item, false)
		if err != nil {
			return err
		}
		output[i] = restored
	}
	response.Output = output
	return nil
}

// calls 只存本次流的工具身份，不保留源码；完整参数由 SDK 原有流状态聚合。
// 在终态到达前保留身份，可同时处理 arguments.done 与 output_item.done。
// 连接取消与上游异常仍由外层流生命周期控制，恢复器不自行重试请求。
// 格式错误交给外层延迟到读完用量后报告，不在这里制造成功终态。
// 流状态属于单次请求；普通函数不进入该表，允许并行混合工具调用。
// 自定义参数在 arguments.done 时完整校验后一次发送，文本流仍实时通过。
// 不做 JSON 片段猜测解码，避免转义、代理对或错误参数被提前交给客户端。
type openCodeExecStream struct {
	bridge   *openCodeExecBridge
	calls    map[string]bool
	sequence int
}

// restore 把函数事件转换成 custom 事件，可抑制包装参数的 delta 或展开为两帧。
// 新事件统一重排序号，不复用 SDK 已被跳过的序号，也不改它的输出索引。
func (s *openCodeExecStream) restore(event *schemas.BifrostResponsesStreamResponse) ([]*schemas.BifrostResponsesStreamResponse, error) {
	// SDK 会在后续终态继续使用工具状态；只改面向客户端的事件副本。
	// 流缓存中的 arguments 必须保持 JSON，否则终态会遭遇二次解包错误。
	copyEvent := *event
	event = &copyEvent
	if event.Item != nil {
		added := event.Type == schemas.ResponsesStreamResponseTypeOutputItemAdded
		if added && event.Item.Type != nil && *event.Item.Type == schemas.ResponsesMessageTypeFunctionCall && s.bridge.isExec(event.Item.ResponsesToolMessage) {
			if event.Item.ID == nil || *event.Item.ID == "" {
				return nil, errors.New("exec stream item has no id")
			}
			// 以条目 ID 识别后续参数事件，不用函数名区分并行的多次 exec。
			// 输出索引仍沿用 SDK，不能因抑制参数事件改变工具在响应中的位置。
			s.calls[*event.Item.ID] = true
		}
		item, err := s.bridge.restoreItem(*event.Item, added)
		if err != nil {
			return nil, err
		}
		event.Item = &item
	}
	events := []*schemas.BifrostResponsesStreamResponse{event}
	if event.ItemID != nil && s.calls[*event.ItemID] {
		switch event.Type {
		// 参数的分片边界由上游决定，与 JSON 字符串或 Unicode 边界无关。
		// 只暂停 custom 参数，普通文字与普通函数的 delta 不受影响。
		case schemas.ResponsesStreamResponseTypeFunctionCallArgumentsDelta:
			return nil, nil
		case schemas.ResponsesStreamResponseTypeFunctionCallArgumentsDone:
			if event.Arguments == nil {
				return nil, errors.New("exec stream has no complete arguments")
			}
			source, err := decodeExecInput(*event.Arguments)
			if err != nil {
				return nil, err
			}
			// 校验通过后补发一帧完整原文，再发 done，使客户端仍可按事件累计输入。
			// 两帧保留 item_id 与 output_index，但移除内部 JSON 参数字段。
			delta := *event
			delta.Type = schemas.ResponsesStreamResponseTypeCustomToolCallInputDelta
			delta.Arguments = nil
			delta.Delta = schemas.Ptr(source)
			event.Type = schemas.ResponsesStreamResponseTypeCustomToolCallInputDone
			event.Arguments = nil
			event.Input = schemas.Ptr(source)
			events = []*schemas.BifrostResponsesStreamResponse{&delta, event}
		}
	}
	// completed/incomplete 中的 output 快照也须恢复，不能只改增量事件。
	// 即使客户端忽略 delta、只读取终态，工具合同也应一致。
	if event.Response != nil {
		response := *event.Response
		event.Response = &response
		if err := s.bridge.restoreResponse(event.Response); err != nil {
			return nil, err
		}
	}
	// 原 SDK 序号包含已经抑制的函数 delta；在最终事件出口重新编号。
	// 序号属于单次请求，不能因多个并行工具各自重新从零开始。
	for _, item := range events {
		item.SequenceNumber = s.sequence
		s.sequence++
	}
	return events, nil
}
