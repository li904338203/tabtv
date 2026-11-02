//go:build integration

// 型号级参数覆盖的写接口（TT-T-18）：新建 / 覆盖更新 / 校验拒绝 / 删除，且写进去的读得出来、每次改动推进模型配置版本。
package integration

import (
	"context"
	"encoding/json"
	"testing"

	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
)

func TestParamSupportAdmin_写读删(t *testing.T) {
	r, pool, next := setupCatalog(t)
	svc := catalogapp.New(r, next)
	ctx := context.Background()
	seedModel(t, pool, next, modelSeed{code: "vid", providerStatus: "enabled",
		modelStatus: "enabled", domain: "video", op: "text2video", opEnabled: true})
	if _, err := pool.Exec(ctx, `insert into param_def (id, code, name, control_type, config, is_enabled)
		values ($1, 'duration', '时长', 'segmented', '{}', true)`, next()); err != nil {
		t.Fatal(err)
	}
	rev := func() int64 {
		t.Helper()
		v, err := r.Revision(ctx, catalogapp.ScopeModel)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}

	r0 := rev()
	in := catalogapp.ParamSupportInput{ParamCode: "duration", AllowedValues: json.RawMessage(`["4","15"]`), DefaultValue: "4"}
	if err := svc.SaveParamSupport(ctx, 1, "vid", in); err != nil {
		t.Fatal(err)
	}
	if rev() <= r0 {
		t.Fatal("写覆盖应推进模型配置版本")
	}
	got, err := svc.ModelParams(ctx, "vid")
	if err != nil || got["duration"]["default"] != "4" {
		t.Fatalf("写进去的应能读出来：%+v %v", got, err)
	}

	// 同一参数再写一次 = 更新，不是第二行。
	in.AllowedValues, in.DefaultValue = json.RawMessage(`["5","10"]`), "10"
	if err := svc.SaveParamSupport(ctx, 1, "vid", in); err != nil {
		t.Fatal(err)
	}
	items, defs, err := svc.ParamSupports(ctx, "vid")
	if err != nil || len(items) != 1 || items[0].DefaultValue != "10" || len(defs) == 0 {
		t.Fatalf("应只有一行且已更新：%+v %+v %v", items, defs, err)
	}

	// 默认值不在可选项里：拒绝（读取侧会悄悄丢掉它，等于「保存成功了默认值却不对」）。
	bad := catalogapp.ParamSupportInput{ParamCode: "duration", AllowedValues: json.RawMessage(`["5"]`), DefaultValue: "8"}
	if err := svc.SaveParamSupport(ctx, 1, "vid", bad); presetErrCode(err) != errcode.InvalidArgument {
		t.Fatalf("默认值不在可选项里应拒绝：%v", err)
	}
	// 模型或参数不存在。
	if err := svc.SaveParamSupport(ctx, 1, "nope", in); presetErrCode(err) != errcode.NotFound {
		t.Fatalf("模型不存在应 NotFound：%v", err)
	}
	if err := svc.SaveParamSupport(ctx, 1, "vid", catalogapp.ParamSupportInput{ParamCode: "ghost", DefaultValue: "x"}); presetErrCode(err) != errcode.NotFound {
		t.Fatalf("参数不存在应 NotFound：%v", err)
	}

	if err := svc.DeleteParamSupport(ctx, 1, "vid", "duration"); err != nil {
		t.Fatal(err)
	}
	if items, _, _ := svc.ParamSupports(ctx, "vid"); len(items) != 0 {
		t.Fatalf("删除后应没有覆盖：%+v", items)
	}
	if err := svc.DeleteParamSupport(ctx, 1, "vid", "duration"); presetErrCode(err) != errcode.NotFound {
		t.Fatalf("再删一次应 NotFound：%v", err)
	}
}
