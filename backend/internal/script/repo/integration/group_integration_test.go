//go:build integration

// 组任务的真库测试（P07-04）。
//
// ⚠️ 必须用真库：这里守的不变量全在库里 —— 建组与子项的事务原子性、
// 子项号唯一、组的乐观锁、子项的 CAS 回写、按 generation 反查子项。
// 用 mock 仓储的话这些都只是"测试作者相信它们成立"。
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	scriptapp "github.com/li904338203/tabtv/backend/internal/script/app"
	"github.com/li904338203/tabtv/backend/internal/script/domain"
	"github.com/li904338203/tabtv/backend/internal/script/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// fakeGroupGen 是可编程的组子项生成器，并照受理入口的幂等语义：
// **同一把键回同一次任务**（连同它当时的状态，Replayed=true）。
type fakeGroupGen struct {
	nextID   int64
	credits  int
	calls    []scriptapp.GroupGenRequest
	failWith error
	// insufficientAfter 之后的调用一律报"积分不足"。
	insufficientAfter int
	byKey             map[string]scriptapp.GroupGenResult
}

func (f *fakeGroupGen) Generate(_ context.Context, in scriptapp.GroupGenRequest) (
	scriptapp.GroupGenResult, error) {
	f.calls = append(f.calls, in)
	if f.failWith != nil {
		return scriptapp.GroupGenResult{}, f.failWith
	}
	if f.insufficientAfter > 0 && len(f.calls) > f.insufficientAfter {
		return scriptapp.GroupGenResult{}, httpx.Errf(errcode.InsufficientCredits, "积分不足")
	}
	if f.byKey == nil {
		f.byKey = map[string]scriptapp.GroupGenResult{}
	}
	if prev, ok := f.byKey[in.IdempotencyKey]; ok {
		prev.Replayed = true
		return prev, nil
	}
	f.nextID++
	res := scriptapp.GroupGenResult{GenerationID: 90000 + f.nextID, Credits: f.credits, Status: "queued"}
	f.byKey[in.IdempotencyKey] = res
	return res, nil
}

// finish 模拟 Worker 把某次任务跑到终态（受理入口此后命中它时回的就是这个状态）。
func (f *fakeGroupGen) finish(genID int64, status string) {
	for k, v := range f.byKey {
		if v.GenerationID == genID {
			v.Status = status
			f.byKey[k] = v
		}
	}
}

type fakeCanceller struct{ cancelled []int64 }

func (f *fakeCanceller) Cancel(_ context.Context, _ int64, genID int64, _ string) error {
	f.cancelled = append(f.cancelled, genID)
	return nil
}

// groupSetup 建一个 3 镜、提示词全就绪的脚本。
func groupSetup(t *testing.T) (*scriptapp.Service, *repo.Repo, *billingrepo.Repo,
	*fakeGroupGen, int64) {
	t.Helper()
	model := &fakeModel{reply: goodReply}
	svc, r, bill, pool := setup(t, model, 6)
	fund(t, bill, 1000)
	sc := newScript(t, svc, "从前有座山")
	if _, err := storyboard(svc, sc.Script.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReconcileAssets(context.Background(), ws, sc.Script.ID); err != nil {
		t.Fatal(err)
	}
	// 资产都出好了图：真实流程里第 3 步（提示词）要求资产全部就绪，
	// 且 CD-82 起引用的资产缺图的镜头不再规划分镜图。
	if _, err := pool.Exec(context.Background(), `
		update script_asset a set status = 'ready', media_object_id = x.mid
		from (select id, 880000 + row_number() over (order by id) as mid from script_asset) x
		where a.id = x.id and a.id in (
			select r.script_asset_id from shot_asset_ref r join shot s on s.id = r.shot_id
			where s.script_id = $1)`, sc.Script.ID); err != nil {
		t.Fatal(err)
	}
	// 模板模式把两套提示词都补齐（0 积分，不影响费用断言）。
	if _, err := svc.Compose(context.Background(), scriptapp.ComposeInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user, ScriptID: sc.Script.ID,
		Source: domain.SourcePromptTemplate,
	}); err != nil {
		t.Fatal(err)
	}
	gg := &fakeGroupGen{credits: 8}
	svc.SetGroupGenerator(gg)
	return svc, r, bill, gg, sc.Script.ID
}

func createGroup(t *testing.T, svc *scriptapp.Service, scriptID int64,
	typ domain.GroupType, node string) scriptapp.GroupView {
	t.Helper()
	v, err := svc.CreateGroup(context.Background(), scriptapp.CreateGroupInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user, ScriptID: scriptID,
		CanvasNodeID: node, GroupType: typ, Name: "测试组", ModelCode: "gpt-x",
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// ---------------------------------------------------------------- 建组

func TestCreateGroup_分镜图组建组即提交(t *testing.T) {
	svc, _, _, gg, scriptID := groupSetup(t)
	v := createGroup(t, svc, scriptID, domain.GroupStoryboardImage, "node-group-1")

	if len(v.Items) != 2 {
		t.Fatalf("应有 2 个子项（2 镜）: %d", len(v.Items))
	}
	if len(gg.calls) != 2 {
		t.Fatalf("建组即提交应提交 2 次，实际 %d", len(gg.calls))
	}
	for _, it := range v.Items {
		if it.Status != string(domain.ItemQueued) {
			t.Fatalf("子项应已排队: %+v", it)
		}
		if it.GenerationID == 0 {
			t.Fatal("子项必须挂上 generationId，否则回写找不到落点")
		}
		if it.CreditsCost != 8 {
			t.Fatalf("子项要各记各的费用（AV-22），实际 %d", it.CreditsCost)
		}
	}
	if v.Group.Status != string(domain.GroupRunning) {
		t.Fatalf("组应为 running: %s", v.Group.Status)
	}
	// ⚠️ 必须带 groupItemId：不带的话子项永远停在 queued。
	for _, c := range gg.calls {
		if c.GroupItemID == 0 {
			t.Fatal("受理时没带 groupItemId，子项将永远停在 queued")
		}
		// ⚠️ 操作码必须是目录里**真实存在**的 text2img / img2img，
		// 不能自造一个"storyboard_image" —— 目录里没有那一行，
		// 绑操作、配价、能力矩阵全落不上，联调时表现为一个 404。
		if c.OperationCode != domain.OpText2Img && c.OperationCode != domain.OpImg2Img {
			t.Fatalf("操作码应是目录里已有的 text2img/img2img，实际 %s", c.OperationCode)
		}
	}
}

func TestCreateGroup_视频组先落draft不扣费(t *testing.T) {
	// ⚠️ 一条视频几十积分，且输入编排直接决定成片 —— 建组就提交
	// 等于让用户为一批他还没看过的输入付钱。
	svc, _, bill, gg, scriptID := groupSetup(t)
	before, _ := bill.Balance(context.Background(), payer)

	v := createGroup(t, svc, scriptID, domain.GroupVideo, "node-group-video")
	if len(gg.calls) != 0 {
		t.Fatalf("视频组建组不该提交任何任务，实际提交 %d 次", len(gg.calls))
	}
	if v.Group.Status != string(domain.GroupDraft) {
		t.Fatalf("视频组应为 draft: %s", v.Group.Status)
	}
	if after, _ := bill.Balance(context.Background(), payer); after != before {
		t.Fatalf("建组不该扣费：%d → %d", before, after)
	}
	// 输入规划要在建组时就算好并存进快照 —— 预览与真跑必须是同一份数据。
	// ⚠️ 文生视频的提示词走 params.prompt（CD-81），没有外部输入时 `[]` 是合法的 —— 只有带图的操作才必须有输入。
	for _, it := range v.Items {
		op := gjsonString(t, it.ParamsSnapshot, "operation")
		if op == "" {
			t.Fatalf("快照里应有操作码: %s", it.ParamsSnapshot)
		}
		if op != domain.OpText2Video && (len(it.InputRefs) == 0 || string(it.InputRefs) == "[]") {
			t.Fatalf("%s 子项应带规划好的输入: %s", op, it.InputRefs)
		}
	}
}

func TestCreateGroup_同一节点重复建组返回已有的(t *testing.T) {
	// 同 AV-05：报 409 的话前端得再查一次，而中间那一刻用户看到的是失败提示。
	svc, _, _, _, scriptID := groupSetup(t)
	first := createGroup(t, svc, scriptID, domain.GroupVideo, "node-dup")
	second := createGroup(t, svc, scriptID, domain.GroupVideo, "node-dup")
	if first.Group.ID != second.Group.ID {
		t.Fatalf("应返回同一个组：%d vs %d", first.Group.ID, second.Group.ID)
	}
}

func TestCreateGroup_提示词没就绪的镜头不收进组(t *testing.T) {
	// ⚠️ 没有词的镜头提交上去就是一张与剧情无关的图，而钱照扣。
	svc, r, _, _, scriptID := groupSetup(t)
	shots, err := r.ListShots(context.Background(), scriptID)
	if err != nil {
		t.Fatal(err)
	}
	// 改掉第 1 镜的描述 → 它的提示词变 stale，不再是 ready。
	desc := "改过的画面"
	if _, err := svc.SaveShot(context.Background(), scriptapp.SaveShotInput{
		WorkspaceID: ws, ScriptID: scriptID, ShotID: shots[0].ID,
		ExpectedVersion: shots[0].Version, Description: &desc,
	}); err != nil {
		t.Fatal(err)
	}
	v := createGroup(t, svc, scriptID, domain.GroupVideo, "node-partial")
	if len(v.Items) != 1 {
		t.Fatalf("只应收进 1 个提示词就绪的镜头，实际 %d", len(v.Items))
	}
	if v.Items[0].ShotID == shots[0].ID {
		t.Fatal("stale 的那一镜不该被收进组")
	}
}

func TestCreateGroup_一个镜头都没就绪时报出是哪几镜(t *testing.T) {
	model := &fakeModel{reply: goodReply}
	svc, _, bill, _ := setup(t, model, 6)
	fund(t, bill, 1000)
	sc := newScript(t, svc, "从前有座山")
	if _, err := storyboard(svc, sc.Script.ID); err != nil {
		t.Fatal(err)
	}
	svc.SetGroupGenerator(&fakeGroupGen{credits: 8})
	// 一条提示词都没合成过。
	_, err := svc.CreateGroup(context.Background(), scriptapp.CreateGroupInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user, ScriptID: sc.Script.ID,
		CanvasNodeID: "node-none", GroupType: domain.GroupVideo, ModelCode: "gpt-x",
	})
	if err == nil {
		t.Fatal("没有可用镜头时应报错")
	}
	if !strings.Contains(err.Error(), "提示词") {
		t.Fatalf("错误要说清原因，实际 %v", err)
	}
}

// ---------------------------------------------------------------- 执行

func TestExecuteGroup_已成功的跳过失败的重跑(t *testing.T) {
	svc, r, _, gg, scriptID := groupSetup(t)
	v := createGroup(t, svc, scriptID, domain.GroupVideo, "node-exec")

	// 第一次执行：两项都提交。
	out := execGroup(t, svc, v.Group.ID, v.Group.Version, false)
	if len(gg.calls) != 2 {
		t.Fatalf("应提交 2 项，实际 %d", len(gg.calls))
	}

	// 让第 1 项成功、第 2 项失败。
	items, _ := r.ListItems(context.Background(), v.Group.ID)
	bind(t, svc, items[0].GenerationID, "success", "")
	bind(t, svc, items[1].GenerationID, "failed", "上游超时")
	gg.finish(items[0].GenerationID, "success")
	gg.finish(items[1].GenerationID, "failed")
	failedGen := items[1].GenerationID

	view, err := svc.Group(context.Background(), ws, v.Group.ID)
	if err != nil {
		t.Fatal(err)
	}
	// ⚠️ 一成一败必须是 partial_success，不能摊平成 success 或 failed。
	if view.Group.Status != string(domain.GroupPartialSuccess) {
		t.Fatalf("应为 partial_success: %s", view.Group.Status)
	}

	// 第二次执行：只重跑失败的那一项。
	gg.calls = nil
	out = execGroup(t, svc, view.Group.ID, view.Group.Version, false)
	if got := map[int64]bool{}; true {
		for _, c := range gg.calls {
			got[c.GroupItemID] = true
		}
		if len(got) != 1 || !got[items[1].ID] {
			t.Fatalf("只该重跑失败的那 1 项，实际提交了子项 %v", got)
		}
	}
	// ⚠️ 重跑必须是**一次新任务**：输入没变时同一把键指向那次失败，受理把它原样返回，
	// 子项被标成 queued 而没有任务在跑 —— 一直转圈（CD-77）。
	after, _ := r.ListItems(context.Background(), v.Group.ID)
	if after[1].GenerationID == failedGen || after[1].GenerationID == 0 {
		t.Fatalf("失败项的重跑应挂上新任务，仍是那次失败的 %d", failedGen)
	}
	if after[1].Status != "queued" {
		t.Fatalf("重跑后子项应在 queued，得到 %s", after[1].Status)
	}
	_ = out
}

func TestExecuteGroup_余额不足停掉后续项(t *testing.T) {
	// ⚠️ 逐项失败会给用户一串一模一样的"积分不足"，每条还占一次任务记录。
	// 前面已提交的照常跑 —— 这是允许部分成功的操作。
	svc, r, _, gg, scriptID := groupSetup(t)
	gg.insufficientAfter = 1
	v := createGroup(t, svc, scriptID, domain.GroupVideo, "node-broke")

	execGroup(t, svc, v.Group.ID, v.Group.Version, false)
	if len(gg.calls) != 2 {
		t.Fatalf("第 2 项撞到积分不足后应停下，实际调用 %d 次", len(gg.calls))
	}
	items, _ := r.ListItems(context.Background(), v.Group.ID)
	if items[0].Status != string(domain.ItemQueued) {
		t.Fatalf("先提交的那项应照常跑: %s", items[0].Status)
	}
	if items[1].Status != string(domain.ItemFailed) {
		t.Fatalf("撞到积分不足的那项应失败: %s", items[1].Status)
	}
}

func TestExecuteGroup_运行中不许再次执行(t *testing.T) {
	// 组还在跑时第二次点"执行"先被状态拦住 —— 这一道挡的是最常见的
	// 「按钮点两下」。真正的并发保护是下面那条乐观锁用例。
	svc, _, _, gg, scriptID := groupSetup(t)
	v := createGroup(t, svc, scriptID, domain.GroupVideo, "node-lock")

	execGroup(t, svc, v.Group.ID, v.Group.Version, false)
	submitted := len(gg.calls)

	cur, _ := svc.Group(context.Background(), ws, v.Group.ID)
	_, err := svc.ExecuteGroup(context.Background(), scriptapp.ExecuteGroupInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user, GroupID: v.Group.ID,
		ModelCode: "gpt-x", ExpectedVersion: cur.Group.Version,
	})
	if err == nil {
		t.Fatal("运行中不该允许再次执行")
	}
	if len(gg.calls) != submitted {
		t.Fatalf("被挡住的那次不该提交任何任务，实际多提交了 %d 次",
			len(gg.calls)-submitted)
	}
}

// ⚠️ 乐观锁要在**真并发**下测，不能用"先调一次再调一次"。
//
// 顺序调用永远先撞上状态检查（第一次执行完组就是 running），
// 于是那种测试**一次都走不到 CAS** —— 把 UpdateGroupStatus 的版本条件
// 整条删掉，它照样绿。两个请求同时在飞才是它真正要挡的场景：
// 两边都读到 version=N、都通过状态检查，然后靠 CAS 决出胜负。
// 没有它就是两批任务、两份钱（FLOW §5.2）。
func TestUpdateGroupStatus_并发下只有一方能改成功(t *testing.T) {
	svc, r, _, _, scriptID := groupSetup(t)
	v := createGroup(t, svc, scriptID, domain.GroupVideo, "node-cas")
	version := v.Group.Version

	const racers = 8
	var wg sync.WaitGroup
	var won, conflicted int64
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.UpdateGroupStatus(context.Background(), v.Group.ID,
				string(domain.GroupRunning), version)
			switch {
			case err == nil:
				atomic.AddInt64(&won, 1)
			case errors.Is(err, repo.ErrGroupConflict):
				atomic.AddInt64(&conflicted, 1)
			default:
				t.Errorf("意外错误: %v", err)
			}
		}()
	}
	wg.Wait()

	if won != 1 {
		t.Fatalf("%d 个并发请求应当只有 1 个成功，实际 %d 个", racers, won)
	}
	if conflicted != racers-1 {
		t.Fatalf("其余 %d 个都应返回冲突，实际 %d 个", racers-1, conflicted)
	}
	after, err := r.GetGroup(context.Background(), ws, v.Group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != version+1 {
		t.Fatalf("版本应只进位一次：%d → %d", version, after.Version)
	}
}

func TestExecuteGroup_镜头改过的子项拒绝用旧快照跑(t *testing.T) {
	// ⚠️ FLOW-05：跑出来的画面是用户已经改掉的版本，钱花了还得重来。
	svc, r, _, gg, scriptID := groupSetup(t)
	v := createGroup(t, svc, scriptID, domain.GroupVideo, "node-stale")

	items, _ := r.ListItems(context.Background(), v.Group.ID)
	shot, err := r.GetShot(context.Background(), scriptID, items[0].ShotID)
	if err != nil {
		t.Fatal(err)
	}
	desc := "建组之后又改了一次"
	if _, err := svc.SaveShot(context.Background(), scriptapp.SaveShotInput{
		WorkspaceID: ws, ScriptID: scriptID, ShotID: shot.ID,
		ExpectedVersion: shot.Version, Description: &desc,
	}); err != nil {
		t.Fatal(err)
	}

	cur, _ := svc.Group(context.Background(), ws, v.Group.ID)
	execGroup(t, svc, v.Group.ID, cur.Group.Version, false)

	after, _ := r.ListItems(context.Background(), v.Group.ID)
	var changed repo.GroupItem
	for _, it := range after {
		if it.ShotID == shot.ID {
			changed = it
		}
	}
	if changed.Status != string(domain.ItemFailed) {
		t.Fatalf("改过镜头的子项不该被提交，实际 %s", changed.Status)
	}
	if !strings.Contains(changed.ErrorMessage, "镜头已修改") {
		t.Fatalf("错误要说清是镜头变了: %q", changed.ErrorMessage)
	}
	for _, c := range gg.calls {
		if c.ShotID == shot.ID {
			t.Fatal("改过的镜头不该被提交上去")
		}
	}
}

// ---------------------------------------------------------------- 单项

func TestRetryItem_只重跑这一项(t *testing.T) {
	// ⚠️ AV-22：整组重跑会把已经成功的也重做一遍 —— 那是在花用户的钱
	// 做他没要的事。
	svc, r, _, gg, scriptID := groupSetup(t)
	v := createGroup(t, svc, scriptID, domain.GroupStoryboardImage, "node-retry")
	items, _ := r.ListItems(context.Background(), v.Group.ID)
	bind(t, svc, items[0].GenerationID, "success", "")
	bind(t, svc, items[1].GenerationID, "failed", "上游 500")

	cur, _ := svc.Group(context.Background(), ws, v.Group.ID)
	gg.calls = nil
	if _, err := svc.RetryItem(context.Background(), scriptapp.ExecuteGroupInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user, GroupID: v.Group.ID,
		ModelCode: "gpt-x", ExpectedVersion: cur.Group.Version,
	}, items[1].ID); err != nil {
		t.Fatal(err)
	}
	if len(gg.calls) != 1 {
		t.Fatalf("只该提交 1 项，实际 %d", len(gg.calls))
	}
	if gg.calls[0].GroupItemID != items[1].ID {
		t.Fatal("重试的应该是失败的那一项")
	}
	after, _ := r.ListItems(context.Background(), v.Group.ID)
	if after[0].Status != string(domain.ItemSuccess) {
		t.Fatalf("已成功的那项不该被动过: %s", after[0].Status)
	}
}

func TestSaveGroupItem_只有未跑的项能改(t *testing.T) {
	// ⚠️ 已经在跑或已成功的项改了参数，库里存的就不再是那次任务真正用过的
	// 输入 —— 而排查"为什么出来的图不对"时，快照是唯一的证据。
	svc, r, _, _, scriptID := groupSetup(t)
	v := createGroup(t, svc, scriptID, domain.GroupVideo, "node-edit")
	items, _ := r.ListItems(context.Background(), v.Group.ID)

	newParams := json.RawMessage(`{"operation":"img2video","durationSec":10}`)
	got, err := svc.SaveGroupItem(context.Background(), scriptapp.SaveGroupItemInput{
		WorkspaceID: ws, GroupID: v.Group.ID, ItemID: items[0].ID, Params: newParams,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gjsonString(t, got.ParamsSnapshot, "operation") != "img2video" {
		t.Fatalf("参数没保存: %s", got.ParamsSnapshot)
	}

	// 提交之后就不许改了。
	execGroup(t, svc, v.Group.ID, v.Group.Version, false)
	_, err = svc.SaveGroupItem(context.Background(), scriptapp.SaveGroupItemInput{
		WorkspaceID: ws, GroupID: v.Group.ID, ItemID: items[0].ID, Params: newParams,
	})
	if err == nil {
		t.Fatal("已排队的子项不该还能改参数")
	}
}

func TestCancelGroup_逐项取消并重算聚合状态(t *testing.T) {
	svc, r, _, _, scriptID := groupSetup(t)
	fc := &fakeCanceller{}
	svc.SetCanceller(fc)
	v := createGroup(t, svc, scriptID, domain.GroupStoryboardImage, "node-cancel")

	out, err := svc.CancelGroup(context.Background(), ws, v.Group.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.cancelled) != 2 {
		t.Fatalf("应逐项取消 2 个 generation，实际 %d", len(fc.cancelled))
	}
	if out.Group.Status != string(domain.GroupCancelled) {
		t.Fatalf("全部取消后组应为 cancelled: %s", out.Group.Status)
	}
	items, _ := r.ListItems(context.Background(), v.Group.ID)
	for _, it := range items {
		if it.Status != string(domain.ItemCancelled) {
			t.Fatalf("子项应为 cancelled: %s", it.Status)
		}
	}
}

// ⚠️ 这条守的是一个**死胡同**：镜头改过之后子项变 stale，
// 而 stale 的项提交时会被 shotVersion 比对拦下。如果刷新不同时对齐
// 版本号，这一项就**永远跑不了了** —— 状态看着回到了 draft，
// 一点执行又失败，而界面上看不出为什么。
func TestSaveGroupItem_刷新让过期的项能重新跑(t *testing.T) {
	svc, r, _, gg, scriptID := groupSetup(t)
	v := createGroup(t, svc, scriptID, domain.GroupVideo, "node-refresh")
	items, _ := r.ListItems(context.Background(), v.Group.ID)
	target := items[0]

	shot, err := r.GetShot(context.Background(), scriptID, target.ShotID)
	if err != nil {
		t.Fatal(err)
	}
	desc := "建组之后改了一次"
	if _, err := svc.SaveShot(context.Background(), scriptapp.SaveShotInput{
		WorkspaceID: ws, ScriptID: scriptID, ShotID: shot.ID,
		ExpectedVersion: shot.Version, Description: &desc,
	}); err != nil {
		t.Fatal(err)
	}
	after, _ := r.GetItem(context.Background(), v.Group.ID, target.ID)
	if after.Status != string(domain.ItemStale) {
		t.Fatalf("改完镜头子项应变 stale: %s", after.Status)
	}

	// 刷新：状态回 draft，且版本号对齐到当前。
	refreshed, err := svc.SaveGroupItem(context.Background(), scriptapp.SaveGroupItemInput{
		WorkspaceID: ws, GroupID: v.Group.ID, ItemID: target.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Status != string(domain.ItemDraft) {
		t.Fatalf("刷新后应回到 draft: %s", refreshed.Status)
	}
	if refreshed.ShotVersion == target.ShotVersion {
		t.Fatal("刷新必须对齐 shotVersion，否则这一项永远提交不了")
	}

	// 刷新之后必须真的能跑起来。
	gg.calls = nil
	cur, _ := svc.Group(context.Background(), ws, v.Group.ID)
	execGroup(t, svc, v.Group.ID, cur.Group.Version, false)
	final, _ := r.GetItem(context.Background(), v.Group.ID, target.ID)
	if final.Status != string(domain.ItemQueued) {
		t.Fatalf("刷新后应能提交，实际 %s（%s）", final.Status, final.ErrorMessage)
	}
}

// ---------------------------------------------------------------- 回写

func TestBindGroupItem_晚到的结果不覆盖终态(t *testing.T) {
	// ⚠️ 晚到的结果不能把已经取消的项改成成功 —— 用户看到自己取消的
	// 任务"成功了"，而钱也退过了。
	svc, r, _, _, scriptID := groupSetup(t)
	svc.SetCanceller(&fakeCanceller{})
	v := createGroup(t, svc, scriptID, domain.GroupStoryboardImage, "node-late")
	items, _ := r.ListItems(context.Background(), v.Group.ID)

	if _, err := svc.CancelGroup(context.Background(), ws, v.Group.ID, nil); err != nil {
		t.Fatal(err)
	}
	// Worker 那边慢了一步，现在才回写成功。
	bind(t, svc, items[0].GenerationID, "success", "")

	after, _ := r.ListItems(context.Background(), v.Group.ID)
	if after[0].Status != string(domain.ItemCancelled) {
		t.Fatalf("已取消的项不该被晚到的成功覆盖: %s", after[0].Status)
	}
}

func TestBindGroupItem_不是组任务时什么都不做(t *testing.T) {
	// 普通生成任务回写不该报错，也不该误伤任何组。
	svc, _, _, _, _ := groupSetup(t)
	if err := svc.BindGroupItem(context.Background(), 123456789, "success", ""); err != nil {
		t.Fatalf("非组任务的回写应安静返回: %v", err)
	}
}

// ---------------------------------------------------------------- 解组

func TestUngroup_保留引用且不删行(t *testing.T) {
	// ⚠️ 服务端不直接改 canvas_data：那会和前端的自动保存对撞，
	// 用户刚画的连线会静默消失。组行保留是为了前端存盘前崩了还能再取一次。
	svc, r, _, _, scriptID := groupSetup(t)
	v := createGroup(t, svc, scriptID, domain.GroupStoryboardImage, "node-ungroup")
	items, _ := r.ListItems(context.Background(), v.Group.ID)
	bind(t, svc, items[0].GenerationID, "success", "")
	bind(t, svc, items[1].GenerationID, "success", "")

	cur, _ := svc.Group(context.Background(), ws, v.Group.ID)
	out, err := svc.Ungroup(context.Background(), ws, v.Group.ID, cur.Group.Version)
	if err != nil {
		t.Fatal(err)
	}
	if out.Group.Status != string(domain.GroupUngrouped) {
		t.Fatalf("组应标为 ungrouped: %s", out.Group.Status)
	}
	if len(out.Nodes) != 2 {
		t.Fatalf("应返回 2 个节点描述，实际 %d", len(out.Nodes))
	}
	for _, n := range out.Nodes {
		if n.GenerationID == 0 {
			t.Fatal("节点要保留其产出引用（AV-26）")
		}
		if !strings.Contains(n.NodeID, "node-ungroup") {
			t.Fatalf("节点 ID 应沿用提交时那个，否则已有 generation 指不回来: %s", n.NodeID)
		}
	}
	// 行保留：再取一次仍然拿得到。
	again, err := svc.Group(context.Background(), ws, v.Group.ID)
	if err != nil {
		t.Fatalf("解组后组行应保留: %v", err)
	}
	if len(again.Items) != 2 {
		t.Fatal("子项行应保留")
	}
}

func TestUngroup_运行中不许解组(t *testing.T) {
	svc, _, _, _, scriptID := groupSetup(t)
	v := createGroup(t, svc, scriptID, domain.GroupStoryboardImage, "node-ungroup-run")
	cur, _ := svc.Group(context.Background(), ws, v.Group.ID)
	_, err := svc.Ungroup(context.Background(), ws, v.Group.ID, cur.Group.Version)
	if err == nil {
		t.Fatal("还在跑时解组会让回写找不到落点，必须拒绝")
	}
}

// ---------------------------------------------------------------- 工具

func execGroup(t *testing.T, svc *scriptapp.Service, groupID int64, version int,
	force bool) scriptapp.GroupView {
	t.Helper()
	v, err := svc.ExecuteGroup(context.Background(), scriptapp.ExecuteGroupInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user, GroupID: groupID,
		ModelCode: "gpt-x", ExpectedVersion: version, Force: force,
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func bind(t *testing.T, svc *scriptapp.Service, genID int64, status, msg string) {
	t.Helper()
	if err := svc.BindGroupItem(context.Background(), genID, status, msg); err != nil {
		t.Fatal(err)
	}
}

func gjsonString(t *testing.T, raw json.RawMessage, key string) string {
	t.Helper()
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("不是合法 JSON: %s", raw)
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return fmt.Sprint(m[key])
}

// ---------------------------------------------------------------- 导出

func TestExport_包含未完成与失败的部分(t *testing.T) {
	// ⚠️ 只导成功的那些看起来更"干净"，但用户下载脚本正是为了留档和排查 ——
	// 把失败项藏起来，他就无从知道哪一镜没出来、为什么。
	svc, r, _, _, scriptID := groupSetup(t)
	v := createGroup(t, svc, scriptID, domain.GroupStoryboardImage, "node-export")
	items, _ := r.ListItems(context.Background(), v.Group.ID)
	bind(t, svc, items[0].GenerationID, "success", "")
	bind(t, svc, items[1].GenerationID, "failed", "上游 500")

	out, err := svc.Export(context.Background(), ws, scriptID, "2026-09-21T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if out.SchemaVersion != scriptapp.ExportSchemaVersion {
		t.Fatalf("导出格式版本不对: %d", out.SchemaVersion)
	}
	if len(out.Shots) == 0 || len(out.Assets) == 0 {
		t.Fatalf("镜头与资产都应导出：%d 镜 %d 资产", len(out.Shots), len(out.Assets))
	}
	if len(out.Groups) != 1 || len(out.Groups[0].Items) != 2 {
		t.Fatalf("组与子项都应导出: %+v", out.Groups)
	}
	var failed int
	for _, it := range out.Groups[0].Items {
		if it.Status == "failed" {
			failed++
			if it.ErrorMessage == "" {
				t.Fatal("失败项必须带原因，否则留档等于没留")
			}
		}
	}
	if failed != 1 {
		t.Fatalf("失败项应被导出，实际 %d", failed)
	}
}

func TestExport_引用顺序与提示词有效状态(t *testing.T) {
	svc, r, _, _, scriptID := groupSetup(t)

	out, err := svc.Export(context.Background(), ws, scriptID, "now")
	if err != nil {
		t.Fatal(err)
	}
	// ⚠️ 引用顺序即参考图的锚定顺序，丢了就没法复现。
	var withRefs int
	for _, sh := range out.Shots {
		if len(sh.AssetRefs) > 0 {
			withRefs++
		}
	}
	if withRefs == 0 {
		t.Fatal("引用顺序应被导出")
	}

	// 改一个镜头让它的提示词过期，导出必须反映**算出来的**状态并给出警告。
	shots, _ := r.ListShots(context.Background(), scriptID)
	desc := "导出前改一次"
	if _, err := svc.SaveShot(context.Background(), scriptapp.SaveShotInput{
		WorkspaceID: ws, ScriptID: scriptID, ShotID: shots[0].ID,
		ExpectedVersion: shots[0].Version, Description: &desc,
	}); err != nil {
		t.Fatal(err)
	}
	out2, err := svc.Export(context.Background(), ws, scriptID, "now")
	if err != nil {
		t.Fatal(err)
	}
	var stale int
	for _, sh := range out2.Shots {
		if sh.ImagePrompt.Status == string(domain.PromptStale) {
			stale++
		}
	}
	if stale == 0 {
		t.Fatal("导出的提示词状态应是算出来的有效状态，不是库里那一列")
	}
	// ⚠️ 把"这份导出不完整"写进文件本身：只在界面上提示的话，
	// 文件传给别人、或者半年后自己再打开，就没人知道当时缺了什么。
	if len(out2.Warnings) == 0 {
		t.Fatal("有过期提示词时导出必须带 warnings")
	}
}

func TestExport_资产只给媒体ID不给URL(t *testing.T) {
	// ⚠️ 签名地址几小时就失效，写进导出文件等于给用户一堆
	// 第二天就打不开的链接。
	svc, _, _, _, scriptID := groupSetup(t)
	out, err := svc.Export(context.Background(), ws, scriptID, "now")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"http://", "https://", "signedUrl", "\"url\""} {
		if strings.Contains(string(raw), bad) {
			t.Fatalf("导出里不该出现 %q：地址会失效，只给 ID", bad)
		}
	}
}

// CD-82：引用的资产缺图的镜头不规划分镜图，也不提交（不扣费）。
//
// ⚠️ 起因：取资产图时悄悄跳过没图的，而提示词里「参考图对应关系：图N = 名称」是按全部引用编号的 ——
// 少一张图，它后面的编号全错一位，模型把 A 的特征画到 B 身上而任务是成功的。
// 跳过的原因要说出来：此前全被跳过时一律说「提示词还没就绪」，把缺图也说成了它。
func TestCreateGroup_引用的资产缺图的镜头不规划分镜图(t *testing.T) {
	svc, r, _, gg, scriptID := groupSetup(t)
	ctx := context.Background()
	shots, err := r.ListShots(ctx, scriptID)
	if err != nil {
		t.Fatal(err)
	}
	assets, err := r.ListAssetsByScript(ctx, scriptID)
	if err != nil || len(assets) == 0 {
		t.Fatalf("前置：应有资产，%v %d", err, len(assets))
	}
	// 唐僧（第 1 镜引用）的图没了：比如在资产页重新生成、还没出来
	generating := "generating"
	for _, a := range assets {
		if _, err := r.UpdateAsset(ctx, repo.UpdateAssetInput{ID: a.ID, WorkspaceID: ws, Status: &generating}); err != nil {
			t.Fatal(err)
		}
	}

	// 只建第 1 镜：整组建不起来，原因点名是哪一镜、缺哪几个资产
	_, err = svc.CreateGroup(ctx, scriptapp.CreateGroupInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user, ScriptID: scriptID,
		CanvasNodeID: "node-missing-1", GroupType: domain.GroupStoryboardImage, Name: "分镜图", ModelCode: "gpt-x",
		ShotIDs: []int64{shots[0].ID},
	})
	if err == nil || !strings.Contains(err.Error(), "第 1 镜引用的资产还没有设定图：唐僧") {
		t.Fatalf("应说清第 1 镜缺唐僧的设定图，得到 %v", err)
	}
	if len(gg.calls) != 0 {
		t.Fatalf("缺图的镜头不该提交（会扣费），实际提交 %d 次", len(gg.calls))
	}

	// 全片建组：第 1 镜不收，第 2 镜（不引用资产）照常
	v := createGroup(t, svc, scriptID, domain.GroupStoryboardImage, "node-missing-all")
	if len(v.Items) != 1 || v.Items[0].ShotID != shots[1].ID {
		t.Fatalf("只应收第 2 镜，得到 %+v", v.Items)
	}
}
