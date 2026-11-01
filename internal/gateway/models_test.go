package gateway

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"gpt-load/internal/channel"
	"gpt-load/internal/execution"
	"gpt-load/internal/protocol"
	"gpt-load/internal/state"
)

func TestVisibleModelIDs(t *testing.T) {
	snapshot, err := state.Compile(state.CompileInput{
		ChannelRegistry: channel.NewRegistry(),
		Groups: []state.GroupConfig{
			{ConnectionType: "api_key", ID: 1, Name: "first", ChannelID: channel.OpenAI, Params: json.RawMessage(`{}`),
				Models: []state.ModelConfig{
					{ID: "zeta"}, {ID: "shared", Alias: "first-alias"}, {ID: "alpha"},
				},
				Enabled: true,
			},
			{ConnectionType: "api_key", ID: 2, Name: "second", ChannelID: channel.Anthropic, Params: json.RawMessage(`{}`),
				Models: []state.ModelConfig{
					{ID: "shared", Alias: "second-alias"}, {ID: "beta"},
				},
				Enabled: true,
			},
			{ConnectionType: "api_key", ID: 3, Name: "disabled", ChannelID: channel.Gemini, Params: json.RawMessage(`{}`),
				Models: []state.ModelConfig{{ID: "hidden"}}, Enabled: false,
			},
		},
	})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	tests := []struct {
		name      string
		snapshot  *state.ConfigSnapshot
		accessKey state.AccessKeyView
		value     protocol.Protocol
		want      []string
	}{
		{name: "no filters sorted and deduplicated", snapshot: snapshot, value: protocol.OpenAICompletions, want: []string{"alpha", "beta", "first-alias", "second-alias", "zeta"}},
		{name: "protocol allowed", snapshot: snapshot, accessKey: state.AccessKeyView{Filters: state.FilterSet{Protocols: map[protocol.Protocol]struct{}{protocol.OpenAICompletions: {}}}}, value: protocol.OpenAICompletions, want: []string{"alpha", "beta", "first-alias", "second-alias", "zeta"}},
		{name: "protocol denied", snapshot: snapshot, accessKey: state.AccessKeyView{Filters: state.FilterSet{Protocols: map[protocol.Protocol]struct{}{protocol.Gemini: {}}}}, value: protocol.OpenAICompletions, want: []string{}},
		{name: "model filter matches aliases", snapshot: snapshot, accessKey: state.AccessKeyView{Filters: state.FilterSet{Models: map[string]struct{}{"first-alias": {}, "zeta": {}, "shared": {}, "missing": {}}}}, value: protocol.OpenAICompletions, want: []string{"first-alias", "zeta"}},
		{name: "group filter keeps any matching target", snapshot: snapshot, accessKey: state.AccessKeyView{Filters: state.FilterSet{Groups: map[uint]struct{}{2: {}}}}, value: protocol.OpenAICompletions, want: []string{"beta", "second-alias"}},
		{name: "joint filters", snapshot: snapshot, accessKey: state.AccessKeyView{Filters: state.FilterSet{Protocols: map[protocol.Protocol]struct{}{protocol.Anthropic: {}}, Models: map[string]struct{}{"beta": {}, "second-alias": {}, "shared": {}}, Groups: map[uint]struct{}{2: {}}}}, value: protocol.Anthropic, want: []string{"beta", "second-alias"}},
		{name: "dangling group filter", snapshot: snapshot, accessKey: state.AccessKeyView{Filters: state.FilterSet{Groups: map[uint]struct{}{99: {}}}}, value: protocol.OpenAICompletions, want: []string{}},
		{name: "disabled group model absent", snapshot: snapshot, value: protocol.Gemini, want: []string{"alpha", "beta", "first-alias", "second-alias", "zeta"}},
		{name: "responses uses channel capabilities", snapshot: snapshot, value: protocol.OpenAIResponses, want: []string{"alpha", "beta", "first-alias", "second-alias", "zeta"}},
		{name: "nil snapshot", snapshot: nil, value: protocol.OpenAICompletions, want: []string{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := visibleModelIDs(test.snapshot, test.accessKey, test.value)
			if got == nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("visibleModelIDs() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestCodexLiveIsHiddenFromModelLists(t *testing.T) {
	t.Parallel()
	snapshot, err := state.Compile(state.CompileInput{ChannelRegistry: channel.NewRegistry(), Groups: []state.GroupConfig{{
		ID: 1, ChannelID: channel.Codex, ConnectionType: "subscription", Params: json.RawMessage(`{}`),
		Models: []state.ModelConfig{{ID: "gpt-5.5"}, {ID: "gpt-live-1-codex"}}, Enabled: true,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	voice := protocol.Protocol("codex-live")
	if got := visibleModelIDs(snapshot, state.AccessKeyView{}, voice); len(got) != 0 {
		t.Fatalf("voice model list = %v", got)
	}
	if got := visibleModelIDs(snapshot, state.AccessKeyView{}, protocol.OpenAICompletions); !reflect.DeepEqual(got, []string{"gpt-5.5"}) {
		t.Fatalf("ordinary model list = %v", got)
	}
	if got := collectCodexVisibleModelIDs(snapshot, state.AccessKeyView{}); !reflect.DeepEqual(got, []string{"gpt-5.5"}) {
		t.Fatalf("Codex coding model list = %v", got)
	}
	key := state.AccessKeyView{Filters: state.FilterSet{Protocols: map[protocol.Protocol]struct{}{protocol.OpenAIResponses: {}}}}
	if got := visibleModelIDs(snapshot, key, protocol.OpenAICompletions); !reflect.DeepEqual(got, []string{"gpt-5.5"}) {
		t.Fatalf("Responses-only key model list = %v", got)
	}
}

func TestVisibleOpenAIModelIDsUnionsChatAndResponses(t *testing.T) {
	t.Parallel()

	snapshot, err := state.Compile(state.CompileInput{ChannelRegistry: channel.NewRegistry(), Groups: []state.GroupConfig{
		{ConnectionType: "api_key", ID: 1, ChannelID: channel.OpenAI, Params: json.RawMessage(`{}`),
			Models: []state.ModelConfig{
				{ID: "chat-only"},
				{ID: "shared"},
			},
			Enabled: true,
		},
		{ConnectionType: "api_key", ID: 2, ChannelID: channel.Anthropic, Params: json.RawMessage(`{}`),
			Models: []state.ModelConfig{
				{ID: "responses-only"},
				{ID: "shared"},
			},
			Enabled: true,
		},
		{ConnectionType: "api_key", ID: 3, ChannelID: channel.Gemini, Params: json.RawMessage(`{}`),
			Models:  []state.ModelConfig{{ID: "both"}},
			Enabled: true,
		},
	}})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	tests := []struct {
		name      string
		accessKey state.AccessKeyView
		want      []string
	}{
		{
			name: "unrestricted union",
			want: []string{"both", "chat-only", "responses-only", "shared"},
		},
		{
			name: "responses protocol filter",
			accessKey: state.AccessKeyView{Filters: state.FilterSet{
				Protocols: map[protocol.Protocol]struct{}{
					protocol.OpenAIResponses: {},
				},
			}},
			want: []string{"both", "chat-only", "responses-only", "shared"},
		},
		{
			name: "chat protocol filter",
			accessKey: state.AccessKeyView{Filters: state.FilterSet{
				Protocols: map[protocol.Protocol]struct{}{
					protocol.OpenAICompletions: {},
				},
			}},
			want: []string{"both", "chat-only", "responses-only", "shared"},
		},
		{
			name: "Images protocol filter",
			accessKey: state.AccessKeyView{Filters: state.FilterSet{
				Protocols: map[protocol.Protocol]struct{}{
					protocol.OpenAIImages: {},
				},
			}},
			want: []string{"both", "chat-only", "shared"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := visibleModelIDs(
				snapshot,
				test.accessKey,
				protocol.OpenAICompletions,
			)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("visibleModelIDs() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestVisibleOpenAIModelIDsIncludesEmbeddingRoutes(t *testing.T) {
	t.Parallel()

	snapshot := &state.ConfigSnapshot{ExecutionCandidates: state.ExecutionCandidateIndex{
		protocol.OpenAICompletions: {
			execution.OperationChatCompletion: {
				"chat-only": {{GroupID: 1}},
				"shared":    {{GroupID: 1}},
			},
		},
		protocol.OpenAIEmbeddings: {
			execution.OperationEmbeddingsCreate: {
				"embedding-only": {{GroupID: 2}},
				"shared":         {{GroupID: 2}},
			},
		},
	}}

	tests := []struct {
		name      string
		accessKey state.AccessKeyView
		want      []string
	}{
		{
			name: "unrestricted union",
			want: []string{"chat-only", "embedding-only", "shared"},
		},
		{
			name: "embedding protocol filter",
			accessKey: state.AccessKeyView{Filters: state.FilterSet{Protocols: map[protocol.Protocol]struct{}{
				protocol.OpenAIEmbeddings: {},
			}}},
			want: []string{"embedding-only", "shared"},
		},
		{
			name: "embedding protocol and matching group filters",
			accessKey: state.AccessKeyView{Filters: state.FilterSet{
				Protocols: map[protocol.Protocol]struct{}{protocol.OpenAIEmbeddings: {}},
				Groups:    map[uint]struct{}{2: {}},
			}},
			want: []string{"embedding-only", "shared"},
		},
		{
			name: "embedding protocol and nonmatching group filters",
			accessKey: state.AccessKeyView{Filters: state.FilterSet{
				Protocols: map[protocol.Protocol]struct{}{protocol.OpenAIEmbeddings: {}},
				Groups:    map[uint]struct{}{1: {}},
			}},
			want: []string{},
		},
		{
			name: "chat protocol filter excludes embedding-only routes",
			accessKey: state.AccessKeyView{Filters: state.FilterSet{Protocols: map[protocol.Protocol]struct{}{
				protocol.OpenAICompletions: {},
			}}},
			want: []string{"chat-only", "shared"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := visibleModelIDs(snapshot, test.accessKey, protocol.OpenAICompletions)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("visibleModelIDs() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestMarshalModelList(t *testing.T) {
	tests := []struct {
		name     string
		value    protocol.Protocol
		ids      []string
		expected string
	}{
		{
			name: "OpenAI", value: protocol.OpenAICompletions, ids: []string{"alpha", "zeta"},
			expected: `{"object":"list","data":[{"id":"alpha","object":"model","created":1735689600,"owned_by":"gpt-load"},{"id":"zeta","object":"model","created":1735689600,"owned_by":"gpt-load"}]}`,
		},
		{
			name: "Anthropic", value: protocol.Anthropic, ids: []string{"alpha", "zeta"},
			expected: `{"data":[{"type":"model","id":"alpha","display_name":"alpha","created_at":"2025-01-01T00:00:00Z"},{"type":"model","id":"zeta","display_name":"zeta","created_at":"2025-01-01T00:00:00Z"}],"has_more":false,"first_id":"alpha","last_id":"zeta"}`,
		},
		{
			name: "Gemini", value: protocol.Gemini, ids: []string{"models/custom"},
			expected: `{"models":[{"name":"models/models/custom"}]}`,
		},
		{
			name: "OpenAI empty", value: protocol.OpenAICompletions, ids: nil,
			expected: `{"object":"list","data":[]}`,
		},
		{
			name: "Anthropic empty", value: protocol.Anthropic, ids: nil,
			expected: `{"data":[],"has_more":false,"first_id":"","last_id":""}`,
		},
		{
			name: "Gemini empty", value: protocol.Gemini, ids: nil,
			expected: `{"models":[]}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body, err := marshalModelList(test.value, test.ids)
			if err != nil {
				t.Fatalf("marshalModelList() error = %v", err)
			}
			assertJSONEqual(t, string(body), test.expected)
			if strings.Contains(string(body), `"code"`) || strings.Contains(string(body), `"message"`) ||
				strings.Contains(string(body), "nextPageToken") {
				t.Fatalf("model response contains forbidden envelope fields: %s", body)
			}
			for _, forbidden := range []string{"baseModelId", "version", "inputTokenLimit", "outputTokenLimit", "supportedGenerationMethods"} {
				if strings.Contains(string(body), forbidden) {
					t.Fatalf("model response contains forbidden official metadata field %q: %s", forbidden, body)
				}
			}
		})
	}
}

func TestBuildVisibleModelListBoundsFinalAnthropicJSONAcrossGroups(t *testing.T) {
	snapshot, err := state.Compile(state.CompileInput{
		ChannelRegistry: channel.NewRegistry(),
		Groups: []state.GroupConfig{
			{ConnectionType: "api_key", ID: 1, Name: "first", ChannelID: channel.Anthropic, Params: json.RawMessage(`{}`),
				Models: []state.ModelConfig{{ID: "zeta"}}, Enabled: true,
			},
			{ConnectionType: "api_key", ID: 2, Name: "second", ChannelID: channel.Anthropic, Params: json.RawMessage(`{}`),
				Models: []state.ModelConfig{{ID: "alpha"}}, Enabled: true,
			},
		},
	})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	want := `{"data":[{"type":"model","id":"alpha","display_name":"alpha","created_at":"2025-01-01T00:00:00Z"},{"type":"model","id":"zeta","display_name":"zeta","created_at":"2025-01-01T00:00:00Z"}],"has_more":false,"first_id":"alpha","last_id":"zeta"}`

	body, err := buildVisibleModelList(
		snapshot, state.AccessKeyView{}, protocol.Anthropic, int64(len(want)),
	)
	if err != nil || string(body) != want {
		t.Fatalf("exact limit body/error = %s / %v", body, err)
	}
	body, err = buildVisibleModelList(
		snapshot, state.AccessKeyView{}, protocol.Anthropic, int64(len(want)-1),
	)
	if !errors.Is(err, errModelListTooLarge) || body != nil {
		t.Fatalf("overflow body/error = %s / %v", body, err)
	}
}

func assertJSONEqual(t *testing.T, got, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal([]byte(got), &gotValue); err != nil {
		t.Fatalf("decode got JSON: %v; body=%s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("decode expected JSON: %v", err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON mismatch\ngot:  %s\nwant: %s", got, want)
	}
}

func TestVisibleOpenAIModelIDsIncludesUtilityRoutes(t *testing.T) {
	t.Parallel()

	snapshot := &state.ConfigSnapshot{ExecutionCandidates: state.ExecutionCandidateIndex{
		protocol.OpenAICompletions: {
			execution.OperationChatCompletion: {
				"chat-only": {{GroupID: 1}},
				"shared":    {{GroupID: 1}},
			},
		},
		protocol.Rerank: {
			execution.OperationRerank: {
				"rerank-only": {{GroupID: 2}},
				"shared":      {{GroupID: 2}},
			},
		},
		protocol.Decisions: {
			execution.OperationDecisionsCreate: {
				"decision-only": {{GroupID: 3}},
				"shared":        {{GroupID: 3}},
			},
		},
		protocol.AudioSpeech: {
			execution.OperationAudioSpeech: {
				"speech-only": {{GroupID: 4}},
			},
		},
	}}

	tests := []struct {
		name      string
		accessKey state.AccessKeyView
		want      []string
	}{
		{
			name: "unrestricted union",
			want: []string{"chat-only", "decision-only", "rerank-only", "shared", "speech-only"},
		},
		{
			name: "audio speech protocol filter",
			accessKey: state.AccessKeyView{Filters: state.FilterSet{Protocols: map[protocol.Protocol]struct{}{
				protocol.AudioSpeech: {},
			}}},
			want: []string{"speech-only"},
		},
		{
			name: "decisions protocol filter",
			accessKey: state.AccessKeyView{Filters: state.FilterSet{Protocols: map[protocol.Protocol]struct{}{
				protocol.Decisions: {},
			}}},
			want: []string{"decision-only", "shared"},
		},
		{
			name: "rerank protocol filter",
			accessKey: state.AccessKeyView{Filters: state.FilterSet{Protocols: map[protocol.Protocol]struct{}{
				protocol.Rerank: {},
			}}},
			want: []string{"rerank-only", "shared"},
		},
		{
			name: "rerank protocol and matching group filters",
			accessKey: state.AccessKeyView{Filters: state.FilterSet{
				Protocols: map[protocol.Protocol]struct{}{protocol.Rerank: {}},
				Groups:    map[uint]struct{}{2: {}},
			}},
			want: []string{"rerank-only", "shared"},
		},
		{
			name: "rerank protocol and nonmatching group filters",
			accessKey: state.AccessKeyView{Filters: state.FilterSet{
				Protocols: map[protocol.Protocol]struct{}{protocol.Rerank: {}},
				Groups:    map[uint]struct{}{1: {}},
			}},
			want: []string{},
		},
		{
			name: "chat protocol filter excludes rerank-only routes",
			accessKey: state.AccessKeyView{Filters: state.FilterSet{Protocols: map[protocol.Protocol]struct{}{
				protocol.OpenAICompletions: {},
			}}},
			want: []string{"chat-only", "shared"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := visibleModelIDs(snapshot, test.accessKey, protocol.OpenAICompletions)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("visibleModelIDs() = %#v, want %#v", got, test.want)
			}
		})
	}
}

// 私有目录与真实路由使用同一个门禁，别名不能绕过账号绑定。
// 普通全量 Key 看不到私有模型，专属 Key 不获得其他账号目录。
// 数据面目录包含普通 OpenAI 与 Codex 专用两种路径。
// 二者必须共享权限，不能只隐藏其中一个目录。
// 使用实际客户端目录支持的模型名，避免目录过滤掩盖权限错误。
// 同厂商两个账号的模型有意不同，便于确认隔离结果。
// 无权的显式组过滤与无筛选 Key 都需要得到空目录。
// 目录检查不会连接上游，也不依赖凭据健康。
func TestHostingVisibleModels(t *testing.T) {
	snapshot, err := state.Compile(state.CompileInput{
		ChannelRegistry: channel.NewRegistry(), HostingGroups: map[uint]uint{1: 7, 2: 8},
		Groups: []state.GroupConfig{
			{ID: 1, ChannelID: channel.Codex, ConnectionType: "subscription", Params: json.RawMessage(`{}`), Models: []state.ModelConfig{{ID: "gpt-5.5"}}, Enabled: true},
			{ID: 2, ChannelID: channel.Codex, ConnectionType: "subscription", Params: json.RawMessage(`{}`), Models: []state.ModelConfig{{ID: "gpt-5.4"}}, Enabled: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 覆盖空组筛选与显式伪造组筛选；仅改过滤器不构成所有权。
	// 编译真实渠道能力，目录只读取已编译路由而非测试手写目录。
	for _, key := range []state.AccessKeyView{{ID: 9}, {ID: 9, Filters: state.FilterSet{Groups: map[uint]struct{}{1: {}}}}} {
		if got := visibleModelIDs(snapshot, key, protocol.OpenAIResponses); len(got) != 0 {
			t.Fatalf("private models leaked: %v", got)
		}
	}
	// 同一厂商两个账号依然相互独立，模型别名不会合并权限。
	// 同时核对 Codex 客户端专用目录，避免普通目录通过但专用目录漏出。
	key := state.AccessKeyView{ID: 7, Filters: state.FilterSet{Groups: map[uint]struct{}{1: {}}}}
	if got := visibleModelIDs(snapshot, key, protocol.OpenAIResponses); !reflect.DeepEqual(got, []string{"gpt-5.5"}) {
		t.Fatalf("owner models: %v", got)
	}
	if got := collectCodexVisibleModelIDs(snapshot, key); !reflect.DeepEqual(got, []string{"gpt-5.5"}) {
		t.Fatalf("Codex owner models: %v", got)
	}
}
