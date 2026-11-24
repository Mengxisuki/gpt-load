package cpa

import (
	"bytes"
	"fmt"
)

const maxSubscriptionSSEEventBytes = 10 << 20

// nativeResponsesSSEAssembler restores complete SSE events from the line-level
// chunks emitted by the embedded Codex and Grok executors.
type nativeResponsesSSEAssembler struct {
	pending []byte
}

func (assembler *nativeResponsesSSEAssembler) push(chunk []byte) ([][]byte, error) {
	assembler.pending = append(assembler.pending, chunk...)
	if len(chunk) == 0 || chunk[len(chunk)-1] != '\n' && chunk[len(chunk)-1] != '\r' {
		assembler.pending = append(assembler.pending, '\n')
	}
	return assembler.scan()
}

// 原生 Images 以任意字节块到达，块末可能处在 JSON 字符串或 SSE 行中。
// 与文本 SDK 的逐行块共用边界扫描，保留原始字节且不插入隐式换行。
func (assembler *nativeResponsesSSEAssembler) pushWire(chunk []byte) ([][]byte, error) {
	assembler.pending = append(assembler.pending, chunk...)
	return assembler.scan()
}

// 只交付完整的数据事件，注释心跳及空行不进入上层协议转换。
func (assembler *nativeResponsesSSEAssembler) scan() ([][]byte, error) {
	var events [][]byte
	for {
		eventEnd, found := firstCompleteSubscriptionSSEEvent(assembler.pending)
		if !found {
			if len(assembler.pending) > maxSubscriptionSSEEventBytes {
				return nil, fmt.Errorf("subscription SSE event exceeds limit")
			}
			return events, nil
		}
		if eventEnd > maxSubscriptionSSEEventBytes {
			return nil, fmt.Errorf("subscription SSE event exceeds limit")
		}
		event := bytes.Clone(assembler.pending[:eventEnd])
		assembler.pending = assembler.pending[eventEnd:]
		if subscriptionSSEEventHasData(event) {
			events = append(events, event)
		}
	}
}

func (assembler *nativeResponsesSSEAssembler) finish() ([][]byte, error) {
	if assembler == nil || len(bytes.TrimSpace(assembler.pending)) == 0 {
		return nil, nil
	}
	events, err := assembler.push(nil)
	if err != nil {
		return nil, err
	}
	if len(events) == 0 || len(bytes.TrimSpace(assembler.pending)) != 0 {
		return nil, fmt.Errorf("subscription SSE stream ended with an incomplete event")
	}
	return events, nil
}

// 以行边界识别事件结束。Images 的 data 行可能含数 MiB base64，
// 用字节搜索跳过行内容，避免逐字节检查已缓冲的大片图片数据。
func firstCompleteSubscriptionSSEEvent(data []byte) (int, bool) {
	lineStart := 0
	for index := 0; index < len(data); {
		offset := bytes.IndexAny(data[index:], "\r\n")
		if offset < 0 {
			return 0, false
		}
		index += offset
		terminatorStart := index
		index++
		if data[terminatorStart] == '\r' && index < len(data) && data[index] == '\n' {
			index++
		}
		if terminatorStart == lineStart {
			return index, true
		}
		lineStart = index
	}
	return 0, false
}

func subscriptionSSEEventHasData(event []byte) bool {
	for offset := 0; offset < len(event); {
		lineEnd := bytes.IndexAny(event[offset:], "\r\n")
		line := event[offset:]
		if lineEnd >= 0 {
			lineEnd += offset
			line = event[offset:lineEnd]
		}
		if bytes.Equal(line, []byte("data")) || bytes.HasPrefix(line, []byte("data:")) {
			return true
		}
		if lineEnd < 0 {
			return false
		}
		offset = lineEnd + 1
		if event[lineEnd] == '\r' && offset < len(event) && event[offset] == '\n' {
			offset++
		}
	}
	return false
}
