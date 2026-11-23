package embedded

import (
	"sort"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func TestMergeModelCatalogUsesCPAStaticDefinitionsForEverySubscriptionChannel(t *testing.T) {
	t.Parallel()

	for _, provider := range []string{ProviderCodex, ProviderClaude, ProviderAntigravity, ProviderGrok} {
		t.Run(provider, func(t *testing.T) {
			static := registry.GetStaticModelDefinitionsByChannel(provider)
			if len(static) == 0 {
				t.Fatalf("CPA static catalog for %q is empty", provider)
			}
			catalogID := ""
			for _, model := range static {
				if model != nil && strings.TrimSpace(model.ID) != "" {
					candidate := strings.TrimSpace(model.ID)
					if provider == ProviderAntigravity {
						if _, excluded := antigravityExcludedModelIDs[candidate]; excluded {
							continue
						}
					}
					catalogID = candidate
					break
				}
			}
			if catalogID == "" {
				t.Fatalf("CPA static catalog for %q has no usable model ID", provider)
			}

			got := MergeModelCatalog(provider, []string{" upstream-only ", catalogID, catalogID})
			if !sort.StringsAreSorted(got) || !containsModelID(got, "upstream-only") || !containsModelID(got, catalogID) {
				t.Fatalf("merged model IDs = %#v", got)
			}
			if len(got) != len(uniqueModelIDs(got)) {
				t.Fatalf("merged model IDs contain duplicates: %#v", got)
			}
		})
	}
}

func TestMergeModelCatalogKeepsAntigravityExcludedModelsOut(t *testing.T) {
	t.Parallel()

	got := MergeModelCatalog("antigravity", []string{"gemini-2.5-pro"})
	for id := range antigravityExcludedModelIDs {
		if containsModelID(got, id) {
			t.Fatalf("merged Antigravity catalog retained excluded model %q: %#v", id, got)
		}
	}
}

// 验证订阅模型发现会补齐 Images 2.6，即使账号返回的模型列表只含文本模型。
// 这是管理端选择分组模型的来源，覆盖执行器可用但界面无法登记的回归。
func TestMergeModelCatalogIncludesCodexImage26Models(t *testing.T) {
	t.Parallel()
	// 只给文本模型作为输入，确保发现结果来自 CPA 的内置 Images 模型。
	got := MergeModelCatalog(ProviderCodex, []string{"gpt-6.2-sol"})
	// 同时核对基础名和两种变体，确保发现结果与原生执行支持的模型一致。
	// 逐个报告遗漏，便于发现 SDK 升级只丢失其中一个变体的情况。
	for _, id := range []string{"gpt-image-2.6", "gpt-image-2.6-flare", "gpt-image-2.6-sunburst"} {
		if !containsModelID(got, id) {
			t.Errorf("Codex 订阅模型列表缺少 %q", id)
		}
	}
}

func containsModelID(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func uniqueModelIDs(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
