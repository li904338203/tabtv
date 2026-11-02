//go:build integration

// 设置页的图片预设写接口（TT-T-20）：新建 / 修改标「已修改」/ 停用不删行 / 恢复内置版本，且每次改动推进配置版本。
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	"github.com/li904338203/tabtv/backend/internal/catalog/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

type fakeBuiltinPresets map[string]repo.AdminImagePreset

func (f fakeBuiltinPresets) BuiltinPreset(code string) (repo.AdminImagePreset, string, bool) {
	p, ok := f[code]
	return p, "hash-" + code, ok
}

func presetErrCode(err error) errcode.Code {
	var be *httpx.BizError
	if errors.As(err, &be) {
		return be.Code
	}
	return 0
}

func TestPresetAdmin_新建修改停用恢复(t *testing.T) {
	r, pool, next := setupCatalog(t)
	svc := catalogapp.New(r, next)
	ctx := context.Background()
	rev := func() int64 {
		t.Helper()
		v, err := r.Revision(ctx, catalogapp.ScopeCanvas)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	builtin := repo.AdminImagePreset{Code: "sheet_x", Name: "内置设定图", PresetGroup: "sheet", Spec: "sheet",
		InputMode: "either", Prompt: "内置提示词", Params: json.RawMessage(`{"presetCode":"sheet_x"}`), Sort: 3}
	// 模拟 seed 装好的内置行。
	if _, err := pool.Exec(ctx, `insert into image_preset (id, code, name, preset_group, spec, input_mode, prompt,
		params, sort, is_enabled, builtin_hash) values ($1,'sheet_x','内置设定图','sheet','sheet','either','内置提示词',
		'{"presetCode":"sheet_x"}',3,true,'hash-sheet_x')`, next()); err != nil {
		t.Fatal(err)
	}

	// 新建：编码重复报 DuplicateName；字段非法报 InvalidArgument。
	own := repo.AdminImagePreset{Code: "my_tone", Name: "我的质感", PresetGroup: "tuning", Spec: "retouch",
		InputMode: "image_required", Prompt: "调成胶片质感", Params: json.RawMessage(`{"presetCode":"my_tone"}`)}
	r0 := rev()
	if err := svc.SavePreset(ctx, 1, own, true); err != nil {
		t.Fatal(err)
	}
	if rev() <= r0 {
		t.Fatal("新建预设没有推进配置版本")
	}
	if err := svc.SavePreset(ctx, 1, own, true); presetErrCode(err) != errcode.DuplicateName {
		t.Fatalf("重复编码应报 DuplicateName，得到 %v", err)
	}
	bad := own
	bad.Code, bad.InputMode = "bad_one", "whatever"
	if err := svc.SavePreset(ctx, 1, bad, true); presetErrCode(err) != errcode.InvalidArgument {
		t.Fatalf("非法输入要求应报 InvalidArgument，得到 %v", err)
	}

	badOp := own
	badOp.Code, badOp.Operation = "bad_op", "no_such_op"
	if err := svc.SavePreset(ctx, 1, badOp, true); presetErrCode(err) != errcode.InvalidArgument {
		t.Fatalf("不存在的操作码应报 InvalidArgument，得到 %v", err)
	}

	// 修改内置预设：标「已修改」。
	edited := builtin
	edited.Prompt = "用户改过的提示词"
	if err := svc.SavePreset(ctx, 1, edited, false); err != nil {
		t.Fatal(err)
	}
	got, err := svc.AdminGetPreset(ctx, "sheet_x")
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsBuiltin || !got.IsUserModified || got.Prompt != "用户改过的提示词" {
		t.Fatalf("修改后应为「内置 / 已修改」：%+v", got)
	}
	missing := own
	missing.Code = "nope_nope"
	if err := svc.SavePreset(ctx, 1, missing, false); presetErrCode(err) != errcode.NotFound {
		t.Fatalf("改不存在的预设应 404，得到 %v", err)
	}

	// 停用不删行，列表里仍在（含已停用）。
	r1 := rev()
	if err := svc.SetPresetEnabled(ctx, 1, "sheet_x", false); err != nil {
		t.Fatal(err)
	}
	if rev() <= r1 {
		t.Fatal("停用没有推进配置版本")
	}
	list, err := svc.AdminListPresets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range list {
		if p.Code == "sheet_x" {
			found = true
			if p.IsEnabled {
				t.Fatal("停用没生效")
			}
		}
	}
	if !found {
		t.Fatal("停用的预设从列表里消失了 —— 停用不能删行")
	}

	// 恢复内置版本：未注入来源 / 非内置 → 404；内置 → 覆盖并清「已修改」，启用状态不动。
	if err := svc.RestorePreset(ctx, 1, "sheet_x"); presetErrCode(err) != errcode.NotFound {
		t.Fatalf("未注入内置来源应 404，得到 %v", err)
	}
	svc.SetBuiltinPresets(fakeBuiltinPresets{"sheet_x": builtin})
	if err := svc.RestorePreset(ctx, 1, "my_tone"); presetErrCode(err) != errcode.NotFound {
		t.Fatalf("非内置预设应 404，得到 %v", err)
	}
	r2 := rev()
	if err := svc.RestorePreset(ctx, 1, "sheet_x"); err != nil {
		t.Fatal(err)
	}
	if rev() <= r2 {
		t.Fatal("恢复内置版本没有推进配置版本")
	}
	got, _ = svc.AdminGetPreset(ctx, "sheet_x")
	if got.Prompt != "内置提示词" || got.IsUserModified || got.IsEnabled {
		t.Fatalf("恢复结果不对（应回到内置内容、清已修改、启用状态不动）：%+v", got)
	}
	var hash string
	_ = pool.QueryRow(ctx, `select builtin_hash from image_preset where code = 'sheet_x'`).Scan(&hash)
	if hash != "hash-sheet_x" {
		t.Fatalf("恢复后应写入内置哈希，得到 %q", hash)
	}
}
