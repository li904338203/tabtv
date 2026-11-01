//go:build integration

// 资产链路的真库测试（P07-02）。
//
// ⚠️ 必须用真库：这里最要紧的一条「自动识别只在本脚本内归并」
// 由 `shot_asset_ref → shot.script_id` 的反查实现 ——
// 用 mock 仓储测的话，测的是"作者相信它只查了本脚本"。
package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"

	scriptapp "github.com/li904338203/tabtv/backend/internal/script/app"
	"github.com/li904338203/tabtv/backend/internal/script/repo"
)

// fakeAssetGen 记下每次受理（供断言"每项独立任务"），并照受理入口的幂等语义：**同一把键回同一次任务**（连同它当时的状态，Replayed=true）。
type fakeAssetGen struct {
	calls []scriptapp.AssetGenRequest
	err   error
	// failAt 第 N 次调用（从 1 起）返回的错误。
	failAt map[int]error
	byKey  map[string]scriptapp.AssetGenResult
}

func (f *fakeAssetGen) Generate(_ context.Context, in scriptapp.AssetGenRequest) (scriptapp.AssetGenResult, error) {
	if f.err != nil {
		return scriptapp.AssetGenResult{}, f.err
	}
	f.calls = append(f.calls, in)
	if err := f.failAt[len(f.calls)]; err != nil {
		return scriptapp.AssetGenResult{}, err
	}
	if f.byKey == nil {
		f.byKey = map[string]scriptapp.AssetGenResult{}
	}
	if prev, ok := f.byKey[in.IdempotencyKey]; ok {
		prev.Replayed = true
		return prev, nil
	}
	res := scriptapp.AssetGenResult{GenerationID: int64(9000 + len(f.calls)), Status: "queued"}
	f.byKey[in.IdempotencyKey] = res
	return res, nil
}

// finish 模拟 Worker 把某次任务跑到终态。
func (f *fakeAssetGen) finish(genID int64, status string) {
	for k, v := range f.byKey {
		if v.GenerationID == genID {
			v.Status = status
			f.byKey[k] = v
		}
	}
}

type fixedBalance int

func (b fixedBalance) Balance(context.Context, int64) (int, error) { return int(b), nil }

func TestReconcile_建资产并按实体顺序绑定引用(t *testing.T) {
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
	if len(out.Assets) != 1 || out.Assets[0].Name != "唐僧" {
		t.Fatalf("应建出 1 个资产: %+v", out.Assets)
	}
	// 描述里必须带上代码固定的构图规范。
	if !strings.Contains(out.Assets[0].Description, "构图规范") {
		t.Fatalf("描述应含版式规范: %s", out.Assets[0].Description)
	}
	// 引用绑到了第一镜（@唐僧 出现在那里）。
	refs, err := r.ListShotRefs(ctx, sc.Script.ID)
	if err != nil {
		t.Fatal(err)
	}
	shots, _ := r.ListShots(ctx, sc.Script.ID)
	if len(refs[shots[0].ID]) != 1 || refs[shots[0].ID][0].Sort != 1 {
		t.Fatalf("第一镜应有一条 sort=1 的引用: %+v", refs[shots[0].ID])
	}
	if len(refs[shots[1].ID]) != 0 {
		t.Fatalf("第二镜没有实体，不该有引用: %+v", refs[shots[1].ID])
	}
}

func TestReconcile_重复执行不产生重复资产(t *testing.T) {
	// 重新生成分镜会重跑 reconcile，这是常态。
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
	out, err := svc.ReconcileAssets(ctx, ws, sc.Script.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Assets) != 1 {
		t.Fatalf("重复 reconcile 不该产生重复资产: %+v", out.Assets)
	}
}

func TestReconcile_不跨脚本归并同名资产(t *testing.T) {
	// ⚠️ 这是老系统最典型的一次事故：按用户级同名匹配，新节点里的
	// `@唐僧` 直接命中数周前的记录，把别的节点的成品图**和 failed 状态
	// 一起**继承过来 —— 用户看到凭空出现的"生成失败"和一张不认识的占位图。
	svc, r, bill, _ := setup(t, &fakeModel{reply: goodReply}, 0)
	fund(t, bill, 100)
	ctx := context.Background()

	// 脚本 A：建出「唐僧」并把它标成已出图。
	a := newScript(t, svc, "第一个节点")
	if _, err := storyboard(svc, a.Script.ID); err != nil {
		t.Fatal(err)
	}
	outA, err := svc.ReconcileAssets(ctx, ws, a.Script.ID)
	if err != nil {
		t.Fatal(err)
	}
	ready := "ready"
	mediaID := int64(123456)
	if _, err := r.UpdateAsset(ctx, repo.UpdateAssetInput{
		ID: outA.Assets[0].ID, WorkspaceID: ws, Status: &ready, MediaObjectID: &mediaID,
	}); err != nil {
		t.Fatal(err)
	}

	// 脚本 B：另一个画布节点，同样出现「唐僧」。
	b, err := svc.Create(ctx, scriptapp.CreateInput{
		WorkspaceID: ws, ProjectID: project, CanvasNodeID: "node-script-2",
		SourceType: "script", RawInput: "第二个节点",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GenerateStoryboard(ctx, scriptapp.StoryboardInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user,
		ScriptID: b.Script.ID, ModelCode: "gpt-x",
	}); err != nil {
		t.Fatal(err)
	}
	outB, err := svc.ReconcileAssets(ctx, ws, b.Script.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(outB.Assets) != 1 {
		t.Fatalf("脚本 B 应有自己的资产: %+v", outB.Assets)
	}
	if outB.Assets[0].ID == outA.Assets[0].ID {
		t.Fatal("⚠️ 跨脚本命中了同名资产 —— 这正是要防的那个事故")
	}
	if outB.Assets[0].Status == "ready" || outB.Assets[0].MediaObjectID != 0 {
		t.Fatalf("脚本 B 的新资产不该继承 A 的成品图与状态: %+v", outB.Assets[0])
	}
}

func TestReconcile_已出图的资产不被覆盖(t *testing.T) {
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
	// 用户改了描述并出了图。
	custom := "用户精心改过的描述\n构图规范：随便"
	ready := "ready"
	mediaID := int64(555)
	if _, err := r.UpdateAsset(ctx, repo.UpdateAssetInput{
		ID: out.Assets[0].ID, WorkspaceID: ws, Description: &custom,
		Status: &ready, MediaObjectID: &mediaID,
	}); err != nil {
		t.Fatal(err)
	}
	// 重新 reconcile：⚠️ 不能把用户的工作删掉。
	if _, err := svc.ReconcileAssets(ctx, ws, sc.Script.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := r.GetAsset(ctx, ws, out.Assets[0].ID)
	if got.Description != custom {
		t.Fatalf("已出图的资产描述被覆盖了: %s", got.Description)
	}
}

func TestGenerateAssets_每项独立任务且余额不足停后续(t *testing.T) {
	// ⚠️ 逐项失败的话用户会收到十条一模一样的"积分不足"，每条还占一次任务记录。
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
	assets, _ := r.ListAssetsByScript(ctx, sc.Script.ID)
	if len(assets) != 4 {
		t.Fatalf("应有 4 个资产: %d", len(assets))
	}

	gen := &fakeAssetGen{}
	svc.SetAssetGenerator(gen)
	svc.SetBalanceReader(fixedBalance(25)) // 单价 10，只够两个

	items, err := svc.GenerateAssets(ctx, scriptapp.BatchAssetInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user,
		ScriptID: sc.Script.ID, ModelCode: "img-1", UnitCost: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(gen.calls) != 2 {
		t.Fatalf("25 分按 10 分一个只够 2 个，实际提交 %d", len(gen.calls))
	}
	// ⚠️ **每项独立任务**：一项失败不影响别的项，用户可以只重试失败的那几个。
	seen := map[string]bool{}
	for _, c := range gen.calls {
		if seen[c.IdempotencyKey] {
			t.Fatalf("幂等键撞了，两项会被当成同一次任务: %s", c.IdempotencyKey)
		}
		seen[c.IdempotencyKey] = true
		if c.AssetID == 0 {
			t.Fatal("受理时要带上 assetId，否则出好的图回写不到资产上")
		}
		if !strings.Contains(c.Prompt, "构图规范") {
			t.Fatalf("提示词应含版式规范: %s", c.Prompt)
		}
	}
	// 后面两项被停掉并说清原因。
	stopped := 0
	for _, it := range items {
		if it.Skipped && strings.Contains(it.Reason, "积分不足") {
			stopped++
		}
	}
	if stopped != 2 {
		t.Fatalf("应有 2 项因积分不足被停: %+v", items)
	}
}

func TestGenerateAssets_已就绪的跳过(t *testing.T) {
	svc, r, bill, _ := setup(t, &fakeModel{reply: goodReply}, 0)
	fund(t, bill, 100)
	sc := newScript(t, svc, "从前有座山")
	if _, err := storyboard(svc, sc.Script.ID); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	out, _ := svc.ReconcileAssets(ctx, ws, sc.Script.ID)
	ready := "ready"
	mediaID := int64(77)
	if _, err := r.UpdateAsset(ctx, repo.UpdateAssetInput{
		ID: out.Assets[0].ID, WorkspaceID: ws, Status: &ready, MediaObjectID: &mediaID,
	}); err != nil {
		t.Fatal(err)
	}
	gen := &fakeAssetGen{}
	svc.SetAssetGenerator(gen)
	svc.SetBalanceReader(fixedBalance(1000))

	items, err := svc.GenerateAssets(ctx, scriptapp.BatchAssetInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user,
		ScriptID: sc.Script.ID, ModelCode: "img-1", UnitCost: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(gen.calls) != 0 {
		t.Fatalf("已就绪的不该重做（那要再花一次钱）: %+v", gen.calls)
	}
	if !items[0].Skipped || !strings.Contains(items[0].Reason, "已有资产图") {
		t.Fatalf("应说明为什么跳过: %+v", items[0])
	}
}

func TestBindAsset_回写结果且不覆盖用户的改动(t *testing.T) {
	svc, r, bill, _ := setup(t, &fakeModel{reply: goodReply}, 0)
	fund(t, bill, 100)
	sc := newScript(t, svc, "从前有座山")
	if _, err := storyboard(svc, sc.Script.ID); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	out, _ := svc.ReconcileAssets(ctx, ws, sc.Script.ID)
	assetID := out.Assets[0].ID

	generating := "generating"
	if _, err := r.UpdateAsset(ctx, repo.UpdateAssetInput{
		ID: assetID, WorkspaceID: ws, Status: &generating,
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.BindAsset(ctx, assetID, 8888, "ready"); err != nil {
		t.Fatal(err)
	}
	got, _ := r.GetAsset(ctx, ws, assetID)
	if got.Status != "ready" || got.MediaObjectID != 8888 {
		t.Fatalf("回写没生效: %+v", got)
	}

	// ⚠️ 用户在任务跑的途中改成了上传：晚到的结果不该把他的动作覆盖回去
	// （与生成域"终态不被晚到结果覆盖"是同一条规则）。
	upload := "upload"
	other := int64(1111)
	if _, err := r.UpdateAsset(ctx, repo.UpdateAssetInput{
		ID: assetID, WorkspaceID: ws, Source: &upload, MediaObjectID: &other,
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.BindAsset(ctx, assetID, 9999, "ready"); err != nil {
		t.Fatal(err)
	}
	got, _ = r.GetAsset(ctx, ws, assetID)
	if got.MediaObjectID != 1111 {
		t.Fatalf("已不在 generating 的资产不该被晚到的结果覆盖: %+v", got)
	}
}

func TestAttachExisting_跨脚本手动复用(t *testing.T) {
	// ⚠️ 这是跨脚本复用的**唯一**入口：它是用户的显式动作。
	// 自动识别永远只在本脚本内归并 —— 两者混在一起就是那个"凭空出现的失败"。
	svc, r, bill, _ := setup(t, &fakeModel{reply: goodReply}, 0)
	fund(t, bill, 100)
	ctx := context.Background()
	a := newScript(t, svc, "第一个节点")
	if _, err := storyboard(svc, a.Script.ID); err != nil {
		t.Fatal(err)
	}
	outA, _ := svc.ReconcileAssets(ctx, ws, a.Script.ID)
	shared := outA.Assets[0]

	b, err := svc.Create(ctx, scriptapp.CreateInput{
		WorkspaceID: ws, ProjectID: project, CanvasNodeID: "node-script-3",
		SourceType: "manual", RawInput: "手写",
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err := svc.AddShot(ctx, scriptapp.AddShotInput{
		WorkspaceID: ws, ScriptID: b.Script.ID, DurationSec: 5, Description: "一个镜头",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AttachExisting(ctx, ws, b.Script.ID, d.Shots[0].ID, shared.ID); err != nil {
		t.Fatal(err)
	}
	refs, _ := r.ListShotRefs(ctx, b.Script.ID)
	if len(refs[d.Shots[0].ID]) != 1 || refs[d.Shots[0].ID][0].AssetID != shared.ID {
		t.Fatalf("手动挑选的资产应挂上: %+v", refs)
	}
	// 重复挂同一个：原样返回而不是报错。
	if _, err := svc.AttachExisting(ctx, ws, b.Script.ID, d.Shots[0].ID, shared.ID); err != nil {
		t.Fatalf("重复点击不该失败: %v", err)
	}
	refs, _ = r.ListShotRefs(ctx, b.Script.ID)
	if len(refs[d.Shots[0].ID]) != 1 {
		t.Fatalf("重复挂不该产生第二条引用: %+v", refs)
	}
}

func TestLibrary_工作区级资产库可按类型与关键字筛(t *testing.T) {
	svc, _, bill, _ := setup(t, &fakeModel{reply: goodReply}, 0)
	fund(t, bill, 100)
	ctx := context.Background()
	sc := newScript(t, svc, "从前有座山")
	if _, err := storyboard(svc, sc.Script.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReconcileAssets(ctx, ws, sc.Script.ID); err != nil {
		t.Fatal(err)
	}
	all, err := svc.Library(ctx, ws, "", "", 50)
	if err != nil || len(all) == 0 {
		t.Fatalf("资产库应有内容: %v %d", err, len(all))
	}
	byType, _ := svc.Library(ctx, ws, "character", "", 50)
	if len(byType) != 1 {
		t.Fatalf("按类型筛应只剩角色: %+v", byType)
	}
	none, _ := svc.Library(ctx, ws, "", "不存在的名字", 50)
	if len(none) != 0 {
		t.Fatalf("关键字不匹配应为空: %+v", none)
	}
	// 别的工作区看不到。
	other, _ := svc.Library(ctx, ws+999, "", "", 50)
	if len(other) != 0 {
		t.Fatalf("跨工作区不该看到: %+v", other)
	}
}

func TestPatchStep_资产没就绪不能进提示词步(t *testing.T) {
	// ⚠️ 没有参考图的提示词合成出来的词，下游拿去出图时没有任何锚定，
	// 等于白花一次钱。
	svc, r, bill, _ := setup(t, &fakeModel{reply: goodReply}, 0)
	fund(t, bill, 100)
	ctx := context.Background()
	sc := newScript(t, svc, "从前有座山")
	if _, err := storyboard(svc, sc.Script.ID); err != nil {
		t.Fatal(err)
	}
	out, _ := svc.ReconcileAssets(ctx, ws, sc.Script.ID)

	cur, _ := svc.Get(ctx, ws, sc.Script.ID)
	step := 3
	if _, err := svc.Patch(ctx, scriptapp.PatchInput{
		WorkspaceID: ws, ScriptID: sc.Script.ID,
		ExpectedVersion: cur.Script.Version, CurrentStep: &step,
	}); err == nil {
		t.Fatal("资产没就绪不该能进提示词步")
	}

	// 资产就绪之后放行。
	ready := "ready"
	mediaID := int64(42)
	if _, err := r.UpdateAsset(ctx, repo.UpdateAssetInput{
		ID: out.Assets[0].ID, WorkspaceID: ws, Status: &ready, MediaObjectID: &mediaID,
	}); err != nil {
		t.Fatal(err)
	}
	cur, _ = svc.Get(ctx, ws, sc.Script.ID)
	if _, err := svc.Patch(ctx, scriptapp.PatchInput{
		WorkspaceID: ws, ScriptID: sc.Script.ID,
		ExpectedVersion: cur.Script.Version, CurrentStep: &step,
	}); err != nil {
		t.Fatalf("资产就绪后应放行: %v", err)
	}
}

func TestSaveAsset_质量提示随响应回去而不拦请求(t *testing.T) {
	// ⚠️ 名字没语义、描述里写了别人的名字都是质量问题不是错误 ——
	// 拦下来用户就走不下去了，而他可能只是还没来得及改。
	svc, _, bill, _ := setup(t, &fakeModel{reply: goodReply}, 0)
	fund(t, bill, 100)
	ctx := context.Background()
	sc := newScript(t, svc, "从前有座山")
	if _, err := storyboard(svc, sc.Script.ID); err != nil {
		t.Fatal(err)
	}
	out, _ := svc.ReconcileAssets(ctx, ws, sc.Script.ID)

	bad := "角色1"
	got, lints, err := svc.SaveAsset(ctx, scriptapp.SaveAssetInput{
		WorkspaceID: ws, AssetID: out.Assets[0].ID, Name: &bad,
	})
	if err != nil {
		t.Fatalf("质量问题不该拦住保存: %v", err)
	}
	if got.Name != bad {
		t.Fatalf("保存应当生效: %+v", got)
	}
	var found bool
	for _, l := range lints {
		if l.Code == "name_not_semantic" {
			found = true
		}
	}
	if !found {
		t.Fatalf("应当回一条质量提示: %+v", lints)
	}
}

func TestSaveAsset_非法输入仍然拦住(t *testing.T) {
	svc, _, bill, _ := setup(t, &fakeModel{reply: goodReply}, 0)
	fund(t, bill, 100)
	ctx := context.Background()
	sc := newScript(t, svc, "从前有座山")
	if _, err := storyboard(svc, sc.Script.ID); err != nil {
		t.Fatal(err)
	}
	out, _ := svc.ReconcileAssets(ctx, ws, sc.Script.ID)

	empty := "  "
	if _, _, err := svc.SaveAsset(ctx, scriptapp.SaveAssetInput{
		WorkspaceID: ws, AssetID: out.Assets[0].ID, Name: &empty,
	}); err == nil {
		t.Fatal("空名是错误不是质量问题，该拦")
	}
	badType := "novel"
	if _, _, err := svc.SaveAsset(ctx, scriptapp.SaveAssetInput{
		WorkspaceID: ws, AssetID: out.Assets[0].ID, Type: &badType,
	}); err == nil {
		t.Fatal("非法类型该拦")
	}
}

// manyEntities 是一份有 4 个实体的分镜，用来测批量生成。
var manyEntities = fmt.Sprintf(`{"shots":[{"description":"群像","durationSec":5,
 "entities":[{"name":"甲","type":"character"},{"name":"乙","type":"character"},
             {"name":"丙","type":"prop"},{"name":"丁","type":"scene"}]}],
 "assets":[{"name":"甲","type":"character","appearance":"%s"},
           {"name":"乙","type":"character","appearance":"b"},
           {"name":"丙","type":"prop","appearance":"c"},
           {"name":"丁","type":"scene","appearance":"d"}]}`, "a")
