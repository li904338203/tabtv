//go:build integration

// 智能剪辑的五个操作不是画布节点的操作（docs/agent/smartcut SCT-08）：只为它们绑的模型
// 不出现在用户端 /models（画布节点的模型下拉）里。
package integration

import (
	"context"
	"slices"
	"testing"
)

func TestListUsableModels_不列智能剪辑专用模型(t *testing.T) {
	r, pool, next := setupCatalog(t)
	ctx := context.Background()
	only := map[string]string{
		"smartcut_plan": "text", "speech2text": "text", "video_desubtitle": "video",
		"text2music": "audio", "video2audio": "audio",
	}
	for op, domain := range only {
		seedModel(t, pool, next, modelSeed{code: "only-" + op, providerStatus: "enabled", modelStatus: "enabled",
			domain: domain, op: op, opEnabled: true})
	}
	// 另外绑了画布操作的模型照常出现（同一个看图模型既做 AI 剪辑又做文本生成是常态）。
	both := seedModel(t, pool, next, modelSeed{code: "plan-and-chat", providerStatus: "enabled", modelStatus: "enabled",
		domain: "text", op: "smartcut_plan", opEnabled: true})
	bindOperation(t, pool, next, both, "text2text")

	for _, filter := range []string{"", "text", "video", "audio"} {
		list, err := r.ListUsableModels(ctx, filter, false)
		if err != nil {
			t.Fatal(err)
		}
		codes := []string{}
		for _, m := range list {
			codes = append(codes, m.Code)
		}
		for op := range only {
			if slices.Contains(codes, "only-"+op) {
				t.Errorf("筛选 %q：只绑了 %s 的模型不该出现在 /models 里：%v", filter, op, codes)
			}
		}
		if filter == "" && !slices.Contains(codes, both) {
			t.Errorf("另外绑了画布操作的模型照常出现：%v", codes)
		}
	}
}
