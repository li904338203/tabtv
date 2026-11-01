//go:build integration

// 资产图批量生成：失败后的重试、画幅画质透传、积分不足停后续（CD-77）。
package integration

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"

	scriptapp "github.com/li904338203/tabtv/backend/internal/script/app"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 起因：幂等键只由「资产 + 描述指纹」组成。同一段描述上一次失败之后再点生成，受理入口把那次失败的任务
// 原样返回（幂等命中），资产却被标成 generating —— 再也没有任务在跑，列表里一直转圈，再点还是它。
func TestGenerateAssets_上一次失败后重试会真的再提交一次(t *testing.T) {
	svc, r, bill, _ := setup(t, &fakeModel{reply: goodReply}, 0)
	fund(t, bill, 100)
	sc := newScript(t, svc, "从前有座山")
	if _, err := storyboard(svc, sc.Script.ID); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	out, err := svc.ReconcileAssets(ctx, ws, sc.Script.ID)
	if err != nil {
		t.Fatal(err)
	}
	assetID := out.Assets[0].ID
	gen := &fakeAssetGen{}
	svc.SetAssetGenerator(gen)
	svc.SetBalanceReader(fixedBalance(1000))
	batch := func() []scriptapp.BatchAssetItem {
		t.Helper()
		items, err := svc.GenerateAssets(ctx, scriptapp.BatchAssetInput{
			WorkspaceID: ws, PayerAccountID: payer, UserID: user,
			ScriptID: sc.Script.ID, ModelCode: "img-1", AssetIDs: []int64{assetID},
		})
		if err != nil {
			t.Fatal(err)
		}
		return items
	}

	first := batch()[0]
	// 双击：还在跑的那一次被命中是对的（不新建任务）。
	if again := batch()[0]; again.GenerationID != first.GenerationID || len(gen.byKey) != 1 {
		t.Fatalf("还在跑时再点应收敛到同一次：%d vs %d，任务数 %d", again.GenerationID, first.GenerationID, len(gen.byKey))
	}

	// 那一次失败了（Worker 跑完、回写资产）。
	gen.finish(first.GenerationID, "failed")
	if err := svc.BindAsset(ctx, assetID, 0, "failed"); err != nil {
		t.Fatal(err)
	}

	retry := batch()[0]
	if retry.Skipped || retry.GenerationID == 0 || retry.GenerationID == first.GenerationID {
		t.Fatalf("失败后的重试必须是一次新任务（此前拿回的是那次失败的 %d），得到 %+v", first.GenerationID, retry)
	}
	last := gen.calls[len(gen.calls)-1]
	if !strings.HasSuffix(last.IdempotencyKey, ":r"+strconv.FormatInt(first.GenerationID, 10)) {
		t.Fatalf("新键应由那次失败的 ID 推出（双击才收敛得到同一次），得到 %s", last.IdempotencyKey)
	}
	if got, _ := r.GetAsset(ctx, ws, assetID); got.Status != "generating" {
		t.Fatalf("重试提交后资产应在 generating，得到 %s", got.Status)
	}

	// 重试那一次又失败了：再点一次仍然要拿到新任务。
	gen.finish(retry.GenerationID, "failed")
	if err := svc.BindAsset(ctx, assetID, 0, "failed"); err != nil {
		t.Fatal(err)
	}
	if third := batch()[0]; third.GenerationID == retry.GenerationID || third.GenerationID == first.GenerationID {
		t.Fatalf("连续失败后再重试仍应是新任务，得到 %+v", third)
	}
}

// 起因：资产弹窗里选的画幅 / 画质从没发出去 —— 受理时参数里只有 prompt，选了等于没选。
// ⚠️ 也不能放开任意键：带上 preset 会让服务端再往提示词后面拼一段配方，把代码固定的版式改掉。
func TestGenerateAssets_画幅画质透传且只认白名单(t *testing.T) {
	svc, _, bill, _ := setup(t, &fakeModel{reply: goodReply}, 0)
	fund(t, bill, 100)
	sc := newScript(t, svc, "从前有座山")
	if _, err := storyboard(svc, sc.Script.ID); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := svc.ReconcileAssets(ctx, ws, sc.Script.ID); err != nil {
		t.Fatal(err)
	}
	gen := &fakeAssetGen{}
	svc.SetAssetGenerator(gen)
	svc.SetBalanceReader(fixedBalance(1000))
	if _, err := svc.GenerateAssets(ctx, scriptapp.BatchAssetInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user, ScriptID: sc.Script.ID, ModelCode: "img-1",
		Params: map[string]any{"aspect_ratio": "2:1", "quality": "high", "preset": "storyboard", "prompt": "别的词"},
	}); err != nil {
		t.Fatal(err)
	}
	if len(gen.calls) != 1 {
		t.Fatalf("应提交 1 个资产，实际 %d", len(gen.calls))
	}
	want := map[string]any{"aspect_ratio": "2:1", "quality": "high"}
	if got := gen.calls[0].Params; !reflect.DeepEqual(got, want) {
		t.Fatalf("参数应只剩画幅与画质，得到 %v", got)
	}
	if !strings.Contains(gen.calls[0].Prompt, "构图规范") {
		t.Fatalf("提示词仍由代码拼（含版式规范），得到 %s", gen.calls[0].Prompt)
	}
}

// 单价没报进来时（UnitCost=0）预判挡不住余额不足，要靠受理返回的错误码停掉后续项：
// 否则后面每一项都得到同一句「积分不足」，每条还占一次受理。
func TestGenerateAssets_受理报积分不足时停掉后续项(t *testing.T) {
	svc, r, bill, _ := setup(t, &fakeModel{reply: manyEntities}, 0)
	fund(t, bill, 100)
	sc := newScript(t, svc, "从前有座山")
	if _, err := storyboard(svc, sc.Script.ID); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := svc.ReconcileAssets(ctx, ws, sc.Script.ID); err != nil {
		t.Fatal(err)
	}
	if assets, _ := r.ListAssetsByScript(ctx, sc.Script.ID); len(assets) != 4 {
		t.Fatalf("前置：应有 4 个资产，实际 %d", len(assets))
	}
	gen := &fakeAssetGen{failAt: map[int]error{2: httpx.Errf(errcode.InsufficientCredits, "积分不足")}}
	svc.SetAssetGenerator(gen)
	svc.SetBalanceReader(fixedBalance(1000))
	items, err := svc.GenerateAssets(ctx, scriptapp.BatchAssetInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user, ScriptID: sc.Script.ID, ModelCode: "img-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(gen.calls) != 2 {
		t.Fatalf("第 2 项报积分不足后应停止提交，实际受理了 %d 次", len(gen.calls))
	}
	if items[0].Skipped || !items[1].Skipped {
		t.Fatalf("第 1 项应提交、第 2 项应带原因跳过：%+v", items[:2])
	}
	for _, it := range items[2:] {
		if !it.Skipped || !strings.Contains(it.Reason, "积分不足") {
			t.Fatalf("后续项应因积分不足被停：%+v", it)
		}
	}
}

// CD-79：对账写出的资产描述照老系统分段（名称 / 描述 / … / 构图规范 / 视觉风格），视觉风格取自脚本的全片风格。
func TestReconcile_资产描述照老系统分段并带上全片风格(t *testing.T) {
	svc, r, bill, _ := setup(t, &fakeModel{reply: goodReply}, 0)
	fund(t, bill, 100)
	sc := newScript(t, svc, "从前有座山")
	if _, err := storyboard(svc, sc.Script.ID); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	out, err := svc.ReconcileAssets(ctx, ws, sc.Script.ID)
	if err != nil || len(out.Assets) == 0 {
		t.Fatalf("对账失败：%v %d", err, len(out.Assets))
	}
	got, _ := r.GetAsset(ctx, ws, out.Assets[0].ID)
	for _, want := range []string{"名称：唐僧", "\n\n描述：僧袍", "\n\n构图规范：", "\n\n视觉风格：水墨"} {
		if !strings.Contains(got.Description, want) {
			t.Fatalf("资产描述缺「%q」：\n%s", want, got.Description)
		}
	}
	if !strings.HasSuffix(got.Description, "视觉风格：水墨") {
		t.Fatalf("视觉风格应在最后（排在版式之后）：\n%s", got.Description)
	}
}
