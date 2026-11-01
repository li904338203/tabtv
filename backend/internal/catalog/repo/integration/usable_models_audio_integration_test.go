//go:build integration

// 用户端 `/models` 下发 hasNativeAudio（MKT-T-01）。
//
// ⚠️ 必须用真库：值来自 SQL 里的 `(m.domain = 'video' and m.has_native_audio)`，
// 漏 select、漏 group by 都只在真跑时才报。
package integration

import (
	"context"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/catalog/repo"
)

func TestListUsableModels_下发hasNativeAudio(t *testing.T) {
	r, pool, next := setupCatalog(t)
	ctx := context.Background()

	seedModel(t, pool, next, modelSeed{code: "video-audio", providerStatus: "enabled",
		modelStatus: "enabled", domain: "video", op: "text2video", opEnabled: true})
	seedModel(t, pool, next, modelSeed{code: "video-mute", providerStatus: "enabled",
		modelStatus: "enabled", domain: "video", op: "text2video", opEnabled: true})
	seedModel(t, pool, next, modelSeed{code: "image-stale", providerStatus: "enabled",
		modelStatus: "enabled", domain: "image", op: "text2img", opEnabled: true})
	// image-stale 模拟「改域前残留的出声标记」：领域校验会挡，但列上没有 CHECK，
	// 所以 SQL 侧按 domain 再收一道 —— 非视频模型必须恒为 false。
	if _, err := pool.Exec(ctx,
		`update model set has_native_audio = true where code in ('video-audio','image-stale')`); err != nil {
		t.Fatal(err)
	}

	all, err := r.ListUsableModels(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]repo.UsableModel{}
	for _, m := range all {
		got[m.Code] = m
	}
	if len(got) != 3 {
		t.Fatalf("应列出 3 个模型，得到 %+v", got)
	}
	if !got["video-audio"].HasNativeAudio {
		t.Fatalf("出声的视频模型应为 true：%+v", got["video-audio"])
	}
	if got["video-mute"].HasNativeAudio {
		t.Fatalf("不出声的视频模型应为 false：%+v", got["video-mute"])
	}
	if got["image-stale"].HasNativeAudio {
		t.Fatalf("非视频模型应恒为 false：%+v", got["image-stale"])
	}

	// 按 resultType=video 筛选时同样带出（营销 / Agent 的实际调用形态）。
	videos, err := r.ListUsableModels(ctx, "video", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(videos) != 2 {
		t.Fatalf("resultType=video 应得 2 个，得到 %+v", videos)
	}
	for _, m := range videos {
		if m.HasNativeAudio != (m.Code == "video-audio") {
			t.Fatalf("hasNativeAudio 与库里不一致：%+v", m)
		}
	}
}
