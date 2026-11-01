//go:build integration

// 图片预设的真库测试（P09-01）。
//
// ⚠️ 必须用真库：预设是**配置**，它的值来自 seed 与后台，不是代码常量。
// 用 mock 只能证明"拼接函数没写错"，证不了"库里那份配方真的拼得出词"。
package integration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	"github.com/li904338203/tabtv/backend/internal/catalog/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

func TestPresetPrompt_从库里取配方并拼词(t *testing.T) {
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		insert into image_preset (id, code, name, preset_group, spec, input_mode,
			prompt, params, dimensions, sort, is_enabled)
		values ($1,'relight','重新打光','tuning','retouch','image_required',
			'保持人物与构图不变，只改变光照。','{}'::jsonb,
			$2::jsonb, 10, true)`,
		gen.Next(), `[{"code":"mood","name":"氛围","default":"natural","options":[
			{"value":"natural","label":"自然","prompt":"色温自然。"},
			{"value":"warm","label":"暖调","prompt":"整体偏暖。"}]}]`); err != nil {
		t.Fatal(err)
	}
	svc := catalogapp.New(repo.New(pool), gen.Next)

	// ⚠️ 用户没打开过调节面板时按 default 出词 —— 跳过的话同一个预设
	// 会"时有时无地缺几句"。
	got, err := svc.PresetPrompt(ctx, catalogapp.PresetAsk{
		Params:   json.RawMessage(`{"preset":"relight"}`),
		HasImage: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "只改变光照") || !strings.Contains(got, "色温自然") {
		t.Fatalf("正文与默认维度都该出词:\n%s", got)
	}

	// 选过的维度用选中的。
	got, err = svc.PresetPrompt(ctx, catalogapp.PresetAsk{
		Params:   json.RawMessage(`{"preset":"relight","presetOptions":{"mood":"warm"}}`),
		HasImage: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "整体偏暖") || strings.Contains(got, "色温自然") {
		t.Fatalf("应用选中的取值:\n%s", got)
	}
}

// ⚠️ 不校验的话用户对一个空节点点"打光"，钱扣了、上游收到一句没有图的
// 提示词，出来一张跟他的照片毫无关系的图。
func TestPresetPrompt_输入不满足时在扣费前就拒(t *testing.T) {
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		insert into image_preset (id, code, name, preset_group, spec, input_mode,
			prompt, params, sort, is_enabled)
		values ($1,'upscale','高清放大','tuning','retouch','image_required',
			'提升清晰度。','{}'::jsonb, 10, true)`, gen.Next()); err != nil {
		t.Fatal(err)
	}
	svc := catalogapp.New(repo.New(pool), gen.Next)

	_, err := svc.PresetPrompt(ctx, catalogapp.PresetAsk{
		Params: json.RawMessage(`{"preset":"upscale"}`), HasImage: false,
	})
	if err == nil {
		t.Fatal("没有图时应被拒绝")
	}
	if !strings.Contains(err.Error(), "高清放大") {
		t.Fatalf("错误要说清是哪个工具: %v", err)
	}
}

// ⚠️ 历史节点里存着早就下线的 code 是常态（下线用 is_enabled=false 不删行）。
// 为此把一次生成拒掉的话，用户会发现"以前能跑的节点今天点不动了"。
func TestPresetPrompt_下线的预设按无预设处理(t *testing.T) {
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		insert into image_preset (id, code, name, preset_group, spec, input_mode,
			prompt, params, sort, is_enabled)
		values ($1,'gone','已下线','tuning','retouch','image_required',
			'不该出现的词','{}'::jsonb, 10, false)`, gen.Next()); err != nil {
		t.Fatal(err)
	}
	svc := catalogapp.New(repo.New(pool), gen.Next)

	got, err := svc.PresetPrompt(ctx, catalogapp.PresetAsk{
		Params: json.RawMessage(`{"preset":"gone"}`), HasImage: true,
	})
	if err != nil {
		t.Fatalf("下线的预设不该让生成失败: %v", err)
	}
	if got != "" {
		t.Fatalf("下线的预设不该出词: %q", got)
	}
}

func TestPresetPrompt_没选预设时安静返回空(t *testing.T) {
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	svc := catalogapp.New(repo.New(pool), gen.Next)
	got, err := svc.PresetPrompt(context.Background(), catalogapp.PresetAsk{
		Params: json.RawMessage(`{"prompt":"一只猫"}`),
	})
	if err != nil || got != "" {
		t.Fatalf("绝大多数生成不走预设: %q %v", got, err)
	}
}

// ⚠️ prompt **不下发给前端**：前端拼出来的词会落进 canvasData，
// 用户改一个字玩法就失效，历史节点也永远停在旧词。
func TestImagePresets_配置接口不下发提示词(t *testing.T) {
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		insert into image_preset (id, code, name, preset_group, spec, input_mode,
			prompt, params, sort, is_enabled)
		values ($1,'x','某工具','tuning','retouch','either',
			'这段词绝不能出现在响应里','{}'::jsonb, 10, true)`, gen.Next()); err != nil {
		t.Fatal(err)
	}
	rows, err := repo.New(pool).ListImagePresets(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Prompt == "" {
		t.Fatalf("仓储层应当取得到 prompt（服务端拼接要用）: %+v", rows)
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "绝不能出现") {
		t.Fatalf("序列化给前端时不该带上 prompt:\n%s", raw)
	}
}
