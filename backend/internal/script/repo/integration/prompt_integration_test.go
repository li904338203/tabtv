//go:build integration

// 提示词链路的真库测试（P07-03）。
//
// ⚠️ 必须用真库：这里守的几条不变量全在库里 —— 指纹列的读写、
// 提示词的乐观锁、失败时不覆盖已有文本（coalesce 而不是 nullif）、
// 以及扣费/退款的唯一约束。用 mock 仓储的话这些都测不到。
package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"

	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	scriptapp "github.com/li904338203/tabtv/backend/internal/script/app"
	"github.com/li904338203/tabtv/backend/internal/script/domain"
	"github.com/li904338203/tabtv/backend/internal/script/repo"
)

// prepared 建一个带 2 镜 + 1 资产的脚本，供提示词用例复用。
func prepared(t *testing.T) (*scriptapp.Service, *repo.Repo, *billingrepo.Repo, int64, []int64) {
	t.Helper()
	model := &fakeModel{reply: goodReply}
	svc, r, bill, _ := setup(t, model, 6)
	fund(t, bill, 1000)
	sc := newScript(t, svc, "从前有座山")
	res, err := storyboard(svc, sc.Script.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReconcileAssets(context.Background(), ws, sc.Script.ID); err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, 0, len(res.Detail.Shots))
	for _, sh := range res.Detail.Shots {
		ids = append(ids, sh.ID)
	}
	// 后续用例要自己控制模型的回复，这里把它还原成可编程状态。
	model.reply = "一段合成好的提示词"
	return svc, r, bill, sc.Script.ID, ids
}

func balance(t *testing.T, bill *billingrepo.Repo) int {
	t.Helper()
	v, err := bill.Balance(context.Background(), payer)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func shotOf(t *testing.T, r *repo.Repo, scriptID, shotID int64) repo.Shot {
	t.Helper()
	sh, err := r.GetShot(context.Background(), scriptID, shotID)
	if err != nil {
		t.Fatal(err)
	}
	return sh
}

func prompts(t *testing.T, svc *scriptapp.Service, scriptID int64) map[string]scriptapp.PromptView {
	t.Helper()
	list, err := svc.Prompts(context.Background(), ws, scriptID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]scriptapp.PromptView{}
	for _, v := range list {
		out[key(v.ShotNo, v.Kind)] = v
	}
	return out
}

func key(shotNo int, kind domain.PromptKind) string {
	return fmt.Sprintf("%d:%s", shotNo, kind)
}

func compose(t *testing.T, svc *scriptapp.Service, scriptID int64,
	in scriptapp.ComposeInput) scriptapp.ComposeResult {
	t.Helper()
	in.WorkspaceID, in.PayerAccountID, in.UserID, in.ScriptID = ws, payer, user, scriptID
	res, err := svc.Compose(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// ---------------------------------------------------------------- 模板模式

func TestCompose_模板模式不收费(t *testing.T) {
	// ⚠️ 模板完全由代码拼，没有调用上游 —— 收费没有任何依据。
	// 老系统对两种模式一视同仁地扣分，用户点了"模板组装"还是掉积分。
	svc, _, bill, scriptID, _ := prepared(t)
	before := balance(t, bill)
	res := compose(t, svc, scriptID, scriptapp.ComposeInput{
		Source: domain.SourcePromptTemplate,
	})
	if res.Quote.Total != 0 {
		t.Fatalf("模板模式应报价 0，实际 %d", res.Quote.Total)
	}
	if res.Quote.TemplateCount != 4 { // 2 镜 × 2 种
		t.Fatalf("应有 4 条模板项，实际 %d", res.Quote.TemplateCount)
	}
	after := balance(t, bill)
	if before != after {
		t.Fatalf("模板模式不该扣费：%d → %d", before, after)
	}

	got := prompts(t, svc, scriptID)
	img := got[key(1, domain.PromptImage)]
	if img.Status != domain.PromptReady || img.Source != domain.SourcePromptTemplate {
		t.Fatalf("第 1 镜图片提示词状态不对: %+v", img)
	}
	// 参考图编号必须在词里，且排在审美词之前。
	if !strings.Contains(img.Text, "图1 = 唐僧") {
		t.Fatalf("模板词里没有参考图编号:\n%s", img.Text)
	}
	if strings.Index(img.Text, "图1 = 唐僧") > strings.Index(img.Text, "参考图使用规则") {
		t.Fatal("识别信息应排在使用规则之前")
	}
}

// ---------------------------------------------------------------- 报价

func TestCompose_dryRun只报价不写库(t *testing.T) {
	// ⚠️ 前端不能自己算价（GAP-21）：老系统前端写死「选中数 × 6」，
	// 而后端按定价目录算 —— 用户看到的价和实际扣的钱不是一回事。
	svc, _, bill, scriptID, _ := prepared(t)
	before := balance(t, bill)

	res := compose(t, svc, scriptID, scriptapp.ComposeInput{
		Source: domain.SourcePromptAI, ModelCode: "gpt-x", DryRun: true,
	})
	if res.Quote.AICount != 4 || res.Quote.UnitCost != 6 || res.Quote.Total != 24 {
		t.Fatalf("报价不对: %+v", res.Quote)
	}
	if after := balance(t, bill); after != before {
		t.Fatalf("dryRun 不该扣费：%d → %d", before, after)
	}
	for k, v := range prompts(t, svc, scriptID) {
		if v.Status != domain.PromptPending {
			t.Fatalf("dryRun 不该写库，%s 变成了 %s", k, v.Status)
		}
	}
}

func TestCompose_已就绪的跳过且不计入报价(t *testing.T) {
	// ⚠️ 重算要再花一次钱，而用户点"批量合成"的意思是
	// "把没有的和过期的补上"。
	svc, _, _, scriptID, _ := prepared(t)
	compose(t, svc, scriptID, scriptapp.ComposeInput{Source: domain.SourcePromptTemplate})

	res := compose(t, svc, scriptID, scriptapp.ComposeInput{
		Source: domain.SourcePromptAI, ModelCode: "gpt-x", DryRun: true,
	})
	if res.Quote.SkipCount != 4 || res.Quote.AICount != 0 || res.Quote.Total != 0 {
		t.Fatalf("已就绪的应全部跳过: %+v", res.Quote)
	}

	// force 时不跳过 —— 用户显式点了"重新生成"。
	forced := compose(t, svc, scriptID, scriptapp.ComposeInput{
		Source: domain.SourcePromptAI, ModelCode: "gpt-x", DryRun: true, Force: true,
	})
	if forced.Quote.SkipCount != 0 || forced.Quote.AICount != 4 {
		t.Fatalf("force 时不该跳过: %+v", forced.Quote)
	}
}

// ---------------------------------------------------------------- AI 模式结账

func TestCompose_AI模式逐条扣费(t *testing.T) {
	model := &fakeModel{reply: "改写后的提示词"}
	svc, _, bill, _ := setup(t, model, 6)
	fund(t, bill, 1000)
	sc := newScript(t, svc, "从前有座山")
	model.reply = goodReply
	if _, err := storyboard(svc, sc.Script.ID); err != nil {
		t.Fatal(err)
	}
	model.reply = "改写后的提示词"
	before, _ := bill.Balance(context.Background(), payer)

	res, err := svc.Compose(context.Background(), scriptapp.ComposeInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user, ScriptID: sc.Script.ID,
		Source: domain.SourcePromptAI, ModelCode: "gpt-x",
		Kinds: []domain.PromptKind{domain.PromptImage},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 2 {
		t.Fatalf("应有 2 条（2 镜 × 图片）: %d", len(res.Items))
	}
	after, _ := bill.Balance(context.Background(), payer)
	if before-after != 12 {
		t.Fatalf("应扣 2×6=12 分，实扣 %d", before-after)
	}
	got := prompts(t, svc, sc.Script.ID)
	if got[key(1, domain.PromptImage)].Text != "改写后的提示词" {
		t.Fatalf("模型产出没落库: %+v", got[key(1, domain.PromptImage)])
	}
	// 只合成了图片，视频那条应保持 pending 且没被扣费。
	if got[key(1, domain.PromptVideo)].Status != domain.PromptPending {
		t.Fatal("没选的种类不该被动过")
	}
}

func TestCompose_模型返回空词时退款且不落空词(t *testing.T) {
	// ⚠️ 空词写进去的话，用户点生成时才发现没词可用 —— 而钱已经扣了。
	// 与分镜同一套规则（CD-34）：先校验产出，通过才算数。
	model := &fakeModel{reply: goodReply}
	svc, _, bill, _ := setup(t, model, 6)
	fund(t, bill, 1000)
	sc := newScript(t, svc, "从前有座山")
	if _, err := storyboard(svc, sc.Script.ID); err != nil {
		t.Fatal(err)
	}
	model.reply = "   " // 空白＝没产出
	before, _ := bill.Balance(context.Background(), payer)

	res, err := svc.Compose(context.Background(), scriptapp.ComposeInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user, ScriptID: sc.Script.ID,
		Source: domain.SourcePromptAI, ModelCode: "gpt-x",
		Kinds: []domain.PromptKind{domain.PromptImage},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range res.Items {
		if it.Status != domain.PromptFailed {
			t.Fatalf("应全部失败: %+v", it)
		}
		if it.Reason == "" {
			t.Fatal("失败项必须带原因，否则用户不知道该重试哪一条")
		}
	}
	if after, _ := bill.Balance(context.Background(), payer); after != before {
		t.Fatalf("失败应全额退款：%d → %d", before, after)
	}
	for _, v := range prompts(t, svc, sc.Script.ID) {
		if v.Text != "" {
			t.Fatalf("不该落空词或脏词: %q", v.Text)
		}
	}
}

func TestCompose_失败不覆盖用户手写的词(t *testing.T) {
	// ⚠️ 这是 coalesce/nullif 的分界：失败只想把状态标成 failed，
	// 用 nullif 的话会把用户手改过的那版词一起抹掉 ——
	// 他什么都没做错，只是点了一次重新生成。
	model := &fakeModel{reply: goodReply}
	svc, _, bill, _ := setup(t, model, 6)
	fund(t, bill, 1000)
	sc := newScript(t, svc, "从前有座山")
	res, err := storyboard(svc, sc.Script.ID)
	if err != nil {
		t.Fatal(err)
	}
	shotID := res.Detail.Shots[0].ID

	if _, err := svc.SavePrompt(context.Background(), scriptapp.SavePromptInput{
		WorkspaceID: ws, ScriptID: sc.Script.ID, ShotID: shotID,
		Kind: domain.PromptImage, Text: "我自己写的词",
	}); err != nil {
		t.Fatal(err)
	}

	model.reply = ""
	if _, err := svc.Compose(context.Background(), scriptapp.ComposeInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user, ScriptID: sc.Script.ID,
		Source: domain.SourcePromptAI, ModelCode: "gpt-x", Force: true,
		ShotIDs: []int64{shotID}, Kinds: []domain.PromptKind{domain.PromptImage},
	}); err != nil {
		t.Fatal(err)
	}

	got := prompts(t, svc, sc.Script.ID)[key(1, domain.PromptImage)]
	if got.Text != "我自己写的词" {
		t.Fatalf("手写的词被失败的重新生成抹掉了: %q", got.Text)
	}
	if got.Status != domain.PromptFailed {
		t.Fatalf("状态应为 failed: %s", got.Status)
	}
}

// ---------------------------------------------------------------- stale

func TestPrompts_改镜头描述后变stale(t *testing.T) {
	svc, r, _, scriptID, shotIDs := prepared(t)
	compose(t, svc, scriptID, scriptapp.ComposeInput{Source: domain.SourcePromptTemplate})

	shot := shotOf(t, r, scriptID, shotIDs[0])
	newDesc := "完全不同的画面"
	if _, err := svc.SaveShot(context.Background(), scriptapp.SaveShotInput{
		WorkspaceID: ws, ScriptID: scriptID, ShotID: shotIDs[0],
		ExpectedVersion: shot.Version, Description: &newDesc,
	}); err != nil {
		t.Fatal(err)
	}

	got := prompts(t, svc, scriptID)
	if got[key(1, domain.PromptImage)].Status != domain.PromptStale {
		t.Fatalf("改过的镜头应 stale: %+v", got[key(1, domain.PromptImage)])
	}
	// ⚠️ stale 不是失败：那段词还在、还能用。
	if got[key(1, domain.PromptImage)].Text == "" {
		t.Fatal("stale 不该清空文本")
	}
	// 没改的那一镜不受影响。
	if got[key(2, domain.PromptImage)].Status != domain.PromptReady {
		t.Fatalf("没改的镜头不该受影响: %+v", got[key(2, domain.PromptImage)])
	}
}

func TestPrompts_改回原样后重新变ready(t *testing.T) {
	// ⚠️ 这条专门证明状态是**算出来的**而不是标出来的：
	// 只靠"改动时标 stale"的话，改回去以后它永远留在 stale，
	// 用户得再花一次钱重算一段一模一样的词。
	svc, r, _, scriptID, shotIDs := prepared(t)
	compose(t, svc, scriptID, scriptapp.ComposeInput{Source: domain.SourcePromptTemplate})

	shot := shotOf(t, r, scriptID, shotIDs[0])
	original := shot.Description
	changed := "临时改一下"
	saved, err := svc.SaveShot(context.Background(), scriptapp.SaveShotInput{
		WorkspaceID: ws, ScriptID: scriptID, ShotID: shotIDs[0],
		ExpectedVersion: shot.Version, Description: &changed,
	})
	if err != nil {
		t.Fatal(err)
	}
	if prompts(t, svc, scriptID)[key(1, domain.PromptImage)].Status != domain.PromptStale {
		t.Fatal("改完应先变 stale")
	}
	if _, err := svc.SaveShot(context.Background(), scriptapp.SaveShotInput{
		WorkspaceID: ws, ScriptID: scriptID, ShotID: shotIDs[0],
		ExpectedVersion: saved.Version, Description: &original,
	}); err != nil {
		t.Fatal(err)
	}
	if got := prompts(t, svc, scriptID)[key(1, domain.PromptImage)]; got.Status != domain.PromptReady {
		t.Fatalf("改回原样应重新 ready: %+v", got)
	}
}

func TestPrompts_改资产描述后引用它的镜头变stale(t *testing.T) {
	// ⚠️ 不把资产描述算进指纹的话，用户改完角色外观、提示词还显示 ready ——
	// 他拿着一段过时的词去出图，画面里那个角色还是旧的样子，
	// 而界面上一切正常。
	svc, _, _, scriptID, _ := prepared(t)
	compose(t, svc, scriptID, scriptapp.ComposeInput{Source: domain.SourcePromptTemplate})

	assets, _, err := svc.Assets(context.Background(), ws, scriptID)
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) == 0 {
		t.Fatal("前置条件：应有资产")
	}
	desc := "改过的外观描述"
	if _, _, err := svc.SaveAsset(context.Background(), scriptapp.SaveAssetInput{
		WorkspaceID: ws, AssetID: assets[0].ID, Description: &desc,
	}); err != nil {
		t.Fatal(err)
	}

	got := prompts(t, svc, scriptID)
	if got[key(1, domain.PromptImage)].Status != domain.PromptStale {
		t.Fatalf("引用该资产的镜头应 stale: %+v", got[key(1, domain.PromptImage)])
	}
	// 第 2 镜没引用资产，不该被牵连。
	if got[key(2, domain.PromptImage)].Status != domain.PromptReady {
		t.Fatalf("没引用该资产的镜头不该 stale: %+v", got[key(2, domain.PromptImage)])
	}
}

func TestPrompts_改全片风格后全部变stale(t *testing.T) {
	svc, _, _, scriptID, _ := prepared(t)
	compose(t, svc, scriptID, scriptapp.ComposeInput{Source: domain.SourcePromptTemplate})

	d, err := svc.Get(context.Background(), ws, scriptID)
	if err != nil {
		t.Fatal(err)
	}
	style := "赛博朋克"
	if _, err := svc.Patch(context.Background(), scriptapp.PatchInput{
		WorkspaceID: ws, ScriptID: scriptID, ExpectedVersion: d.Script.Version,
		GlobalStyle: &style,
	}); err != nil {
		t.Fatal(err)
	}
	for k, v := range prompts(t, svc, scriptID) {
		if v.Status != domain.PromptStale {
			t.Fatalf("改全片风格应让 %s 变 stale，实际 %s", k, v.Status)
		}
	}
}

// ---------------------------------------------------------------- 人工编辑

func TestSavePrompt_人工写的词也记指纹(t *testing.T) {
	// ⚠️ 不记指纹的话手写的词永远显示 ready —— 而它恰恰最可能
	// 和后来改过的画面对不上。
	svc, r, _, scriptID, shotIDs := prepared(t)
	if _, err := svc.SavePrompt(context.Background(), scriptapp.SavePromptInput{
		WorkspaceID: ws, ScriptID: scriptID, ShotID: shotIDs[0],
		Kind: domain.PromptVideo, Text: "手写的视频提示词",
	}); err != nil {
		t.Fatal(err)
	}
	got := prompts(t, svc, scriptID)[key(1, domain.PromptVideo)]
	if got.Source != domain.SourcePromptManual || got.Status != domain.PromptReady {
		t.Fatalf("人工保存后应是 manual/ready: %+v", got)
	}

	shot := shotOf(t, r, scriptID, shotIDs[0])
	desc := "改一下画面"
	if _, err := svc.SaveShot(context.Background(), scriptapp.SaveShotInput{
		WorkspaceID: ws, ScriptID: scriptID, ShotID: shotIDs[0],
		ExpectedVersion: shot.Version, Description: &desc,
	}); err != nil {
		t.Fatal(err)
	}
	if after := prompts(t, svc, scriptID)[key(1, domain.PromptVideo)]; after.Status != domain.PromptStale {
		t.Fatalf("手写的词也应能变 stale: %+v", after)
	}
}

func TestSavePrompt_版本不匹配返回冲突(t *testing.T) {
	svc, r, _, scriptID, shotIDs := prepared(t)
	shot := shotOf(t, r, scriptID, shotIDs[0])
	if _, err := svc.SavePrompt(context.Background(), scriptapp.SavePromptInput{
		WorkspaceID: ws, ScriptID: scriptID, ShotID: shotIDs[0],
		Kind: domain.PromptImage, Text: "第一版", ExpectedVersion: shot.Version,
	}); err != nil {
		t.Fatal(err)
	}
	// 拿过期的版本再写一次：必须被挡住，否则先写的人的改动被悄悄覆盖。
	_, err := svc.SavePrompt(context.Background(), scriptapp.SavePromptInput{
		WorkspaceID: ws, ScriptID: scriptID, ShotID: shotIDs[0],
		Kind: domain.PromptImage, Text: "第二版", ExpectedVersion: shot.Version,
	})
	if err == nil {
		t.Fatal("过期版本应返回冲突")
	}
	if got := prompts(t, svc, scriptID)[key(1, domain.PromptImage)]; got.Text != "第一版" {
		t.Fatalf("先写入的不该被覆盖: %q", got.Text)
	}
}

// ---------------------------------------------------------------- 就绪判定

func TestPromptsReady_未就绪时给出镜号(t *testing.T) {
	// 组任务要靠它拦住"还没合成就批量出图"，所以必须说清是哪几镜。
	svc, _, _, scriptID, shotIDs := prepared(t)
	ok, notReady, err := svc.PromptsReady(context.Background(), ws, scriptID, domain.PromptImage)
	if err != nil {
		t.Fatal(err)
	}
	if ok || len(notReady) != 2 {
		t.Fatalf("初始应两镜都未就绪: ok=%v %v", ok, notReady)
	}
	compose(t, svc, scriptID, scriptapp.ComposeInput{
		Source: domain.SourcePromptTemplate, ShotIDs: []int64{shotIDs[0]},
		Kinds: []domain.PromptKind{domain.PromptImage},
	})
	ok, notReady, _ = svc.PromptsReady(context.Background(), ws, scriptID, domain.PromptImage)
	if ok || len(notReady) != 1 || notReady[0] != 2 {
		t.Fatalf("应只剩第 2 镜未就绪: ok=%v %v", ok, notReady)
	}
}

// ---------------------------------------------------------------- 模板渲染

// fakePrompts 返回一份带占位符的模板，模拟运营在后台配置的内容。
type fakePrompts struct {
	tpl  string
	vars map[string]string
}

func (f *fakePrompts) RenderPrompt(_ context.Context, _, _ string,
	vars map[string]string) (string, error) {
	f.vars = vars
	out := f.tpl
	for k, v := range vars {
		out = strings.ReplaceAll(out, "{{"+k+"}}", v)
	}
	return out, nil
}

func TestCompose_系统提示词里的占位符必须被替换(t *testing.T) {
	// ⚠️ 取模板原文直接发出去的话，模型收到的是字面量 `{{shotDescription}}` ——
	// 而这不报错，模型会照着那段乱码生成，用户只看到"质量莫名其妙地差"。
	model := &fakeModel{reply: goodReply}
	svc, _, bill, _ := setup(t, model, 6)
	fund(t, bill, 1000)
	sc := newScript(t, svc, "从前有座山")
	if _, err := storyboard(svc, sc.Script.ID); err != nil {
		t.Fatal(err)
	}
	fp := &fakePrompts{tpl: "镜头是：{{shotDescription}}，风格 {{globalStyle}}"}
	svc.SetPrompts(fp)
	model.reply = "合成好的词"

	if _, err := svc.Compose(context.Background(), scriptapp.ComposeInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user, ScriptID: sc.Script.ID,
		Source: domain.SourcePromptAI, ModelCode: "gpt-x",
		Kinds: []domain.PromptKind{domain.PromptImage},
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(model.last.System, "{{") {
		t.Fatalf("系统提示词里还留着占位符:\n%s", model.last.System)
	}
	if !strings.Contains(model.last.System, "镜头是：") {
		t.Fatalf("模板没生效: %q", model.last.System)
	}
	// ⚠️ 可选变量也要给全：少给一个就整条渲染失败、悄悄回落到内置兜底。
	for _, k := range []string{"shotDescription", "globalStyle", "shotSize",
		"cameraMove", "lighting", "assetProfiles"} {
		if _, ok := fp.vars[k]; !ok {
			t.Errorf("变量 %s 没传给渲染", k)
		}
	}
}

func TestStoryboard_系统提示词里的占位符必须被替换(t *testing.T) {
	model := &fakeModel{reply: goodReply}
	svc, _, bill, _ := setup(t, model, 6)
	fund(t, bill, 1000)
	fp := &fakePrompts{tpl: "剧情：{{story}}，出 {{shotCount}} 个镜头"}
	svc.SetPrompts(fp)
	sc := newScript(t, svc, "从前有座山")

	if _, err := svc.GenerateStoryboard(context.Background(), scriptapp.StoryboardInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user,
		ScriptID: sc.Script.ID, ModelCode: "gpt-x", ShotCount: 3,
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(model.last.System, "{{") {
		t.Fatalf("系统提示词里还留着占位符:\n%s", model.last.System)
	}
	if !strings.Contains(model.last.System, "从前有座山") {
		t.Fatalf("剧情没渲染进去: %q", model.last.System)
	}
	// ⚠️ user 消息仍然是上下文的**保证载体**：模板渲染失败或没配模板时，
	// 回落的内置兜底里没有剧情 —— 那时 user 消息是模型唯一能看到的剧情。
	if !strings.Contains(model.last.User, "从前有座山") {
		t.Fatal("user 消息里必须有剧情原文")
	}
}
