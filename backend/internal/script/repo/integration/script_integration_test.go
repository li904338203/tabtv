//go:build integration

// 脚本链路的真库测试。
//
// ⚠️ 必须用真库：这里要守的几条不变量都由**库的约束**决定 ——
// 脚本与画布节点 1:1（唯一索引）、镜号连续且唯一（唯一索引）、
// 幂等键去重（唯一索引）、乐观锁（带版本的 where）。
// 用 mock 仓储的话，这几条会变成"测试作者相信它们成立"。
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
	scriptapp "github.com/li904338203/tabtv/backend/internal/script/app"
	"github.com/li904338203/tabtv/backend/internal/script/domain"
	"github.com/li904338203/tabtv/backend/internal/script/repo"
)

const (
	ws      = int64(7001)
	payer   = int64(7002)
	user    = int64(7003)
	project = int64(7004)
)

// fakeModel 是可编程的文本模型：想让它回什么就回什么。
type fakeModel struct {
	reply string
	err   error
	calls int
	// last 是最后一次收到的请求 —— 用来断言**真正发出去的**是什么，
	// 而不是我们以为发出去的（占位符没渲染这类错误只有在这里看得见）。
	last scriptapp.TextRequest
}

func (f *fakeModel) Complete(_ context.Context, req scriptapp.TextRequest) (string, error) {
	f.calls++
	f.last = req
	return f.reply, f.err
}

type fakeQuoter struct{ credits int }

func (f fakeQuoter) QuoteText(context.Context, scriptapp.TextQuoteAsk) (
	scriptapp.TextQuote, error) {
	return scriptapp.TextQuote{Credits: f.credits, ModelID: 1}, nil
}

// ledger 用真账本：退款是否真的发生、是否只发生一次，都要看真表。
type ledger struct{ repo *billingrepo.Repo }

func (l ledger) Consume(ctx context.Context, payerID, userID int64, amount int,
	refType, refID, reason string) error {
	_, err := l.repo.Consume(ctx, billingrepo.ConsumeInput{
		PayerID: payerID, Amount: amount, RefType: refType, RefID: refID,
		UserID: userID, Reason: reason,
	})
	return err
}

func (l ledger) Refund(ctx context.Context, payerID int64, refType, refID, reason string) error {
	_, err := l.repo.Refund(ctx, billingrepo.RefundInput{
		PayerID: payerID, RefType: refType, RefID: refID, Reason: reason,
	})
	if errors.Is(err, billingrepo.ErrDuplicate) {
		return nil
	}
	return err
}

func setup(t *testing.T, model *fakeModel, credits int) (
	*scriptapp.Service, *repo.Repo, *billingrepo.Repo, *pgxpool.Pool) {
	t.Helper()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	r := repo.New(pool)
	bill := billingrepo.New(pool, gen.Next)

	svc := scriptapp.New(r, gen.Next)
	svc.SetTextModel(model)
	svc.SetQuoter(fakeQuoter{credits: credits})
	svc.SetLedger(ledger{bill})
	return svc, r, bill, pool
}

func fund(t *testing.T, bill *billingrepo.Repo, amount int) {
	t.Helper()
	if _, err := bill.Grant(context.Background(), billingrepo.GrantInput{
		PayerID: payer, Source: "purchase", SourceRefID: "test", Amount: amount,
	}); err != nil {
		t.Fatal(err)
	}
}

func newScript(t *testing.T, svc *scriptapp.Service, raw string) scriptapp.Detail {
	t.Helper()
	d, err := svc.Create(context.Background(), scriptapp.CreateInput{
		WorkspaceID: ws, ProjectID: project, CanvasNodeID: "node-script-1",
		SourceType: "script", RawInput: raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func storyboard(svc *scriptapp.Service, scriptID int64) (scriptapp.StoryboardResult, error) {
	return svc.GenerateStoryboard(context.Background(), scriptapp.StoryboardInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user,
		ScriptID: scriptID, ModelCode: "gpt-x",
	})
}

const goodReply = `{"globalStyle":"水墨","shots":[
 {"description":"@唐僧 走在山路上","durationSec":8,
  "entities":[{"name":"唐僧","type":"character"}],
  "startState":"远景","endState":"近景"},
 {"description":"山门前","durationSec":4}],
 "assets":[{"name":"唐僧","type":"character","appearance":"僧袍"}]}`

// ---------------------------------------------------------------- 创建

func TestCreate_同一节点重复创建返回已有脚本(t *testing.T) {
	// ⚠️ AV-05 的「按节点恢复已有脚本」：用户重新打开节点时前端会再调一次创建。
	// 报 409 的话前端得再发一次查询，而中间那一刻用户看到的是一个失败提示 ——
	// 而他什么都没做错。
	svc, _, _, _ := setup(t, &fakeModel{}, 0)
	first := newScript(t, svc, "从前有座山")
	second := newScript(t, svc, "完全不同的内容")
	if first.Script.ID != second.Script.ID {
		t.Fatalf("同一节点应返回同一个脚本：%d vs %d", first.Script.ID, second.Script.ID)
	}
	// 已有脚本的内容不该被第二次创建冲掉。
	if second.Script.RawInput != "从前有座山" {
		t.Fatalf("已有内容被覆盖了: %q", second.Script.RawInput)
	}
}

// ---------------------------------------------------------------- 分镜与结账

func TestStoryboard_成功时扣费且落库(t *testing.T) {
	svc, r, bill, _ := setup(t, &fakeModel{reply: goodReply}, 6)
	fund(t, bill, 100)
	sc := newScript(t, svc, "从前有座山")

	res, err := storyboard(svc, sc.Script.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Detail.Shots) != 2 {
		t.Fatalf("应落 2 个镜头: %d", len(res.Detail.Shots))
	}
	if res.Detail.Shots[0].ShotNo != 1 || res.Detail.Shots[1].ShotNo != 2 {
		t.Fatal("镜号应连续")
	}
	if res.Detail.Shots[0].StartState == "" {
		t.Fatal("运动边界（起止状态）应落库")
	}
	if bal, _ := bill.Balance(context.Background(), payer); bal != 94 {
		t.Fatalf("应扣 6 分，余额 %d", bal)
	}
	// 档案写进 script_instance.asset_profiles。
	var profiles []domain.AssetProfile
	if err := json.Unmarshal(res.Detail.Script.AssetProfiles, &profiles); err != nil {
		t.Fatalf("档案不是数组: %s", res.Detail.Script.AssetProfiles)
	}
	if len(profiles) != 1 || profiles[0].Name != "唐僧" {
		t.Fatalf("档案不对: %+v", profiles)
	}
	// 调用记录可对账。
	calls, err := r.ListCalls(context.Background(), sc.Script.ID, 10)
	if err != nil || len(calls) != 1 {
		t.Fatalf("应有 1 条调用记录: %v %d", err, len(calls))
	}
	if calls[0].Status != "success" || calls[0].CreditsCost != 6 {
		t.Fatalf("调用记录不对: %+v", calls[0])
	}
}

func TestStoryboard_解析失败必须退款且不落镜头(t *testing.T) {
	// ⚠️ 这是老系统最贵的一个 bug：顺序是「调模型 → 结算 → 解析」，
	// 于是解析失败时已经扣了 60 积分、库里落 0 个镜头 ——
	// 用户看到一个空脚本和一笔说不清的消费。
	svc, r, bill, _ := setup(t, &fakeModel{reply: "模型今天不想干活。"}, 6)
	fund(t, bill, 100)
	sc := newScript(t, svc, "从前有座山")

	if _, err := storyboard(svc, sc.Script.ID); err == nil {
		t.Fatal("解析失败应当报错")
	}
	if bal, _ := bill.Balance(context.Background(), payer); bal != 100 {
		t.Fatalf("解析失败必须全额退款，余额 %d", bal)
	}
	shots, _ := r.ListShots(context.Background(), sc.Script.ID)
	if len(shots) != 0 {
		t.Fatalf("解析失败不该落任何镜头: %d", len(shots))
	}
	calls, _ := r.ListCalls(context.Background(), sc.Script.ID, 10)
	if len(calls) != 1 || calls[0].Status != "failed" {
		t.Fatalf("应留一条 failed 记录供排查: %+v", calls)
	}
	if calls[0].ErrorMessage == "" {
		t.Fatal("失败原因要落库，否则排查时只有一个 failed")
	}
}

func TestStoryboard_调用失败也退款(t *testing.T) {
	svc, _, bill, _ := setup(t, &fakeModel{err: fmt.Errorf("上游 502")}, 6)
	fund(t, bill, 100)
	sc := newScript(t, svc, "从前有座山")

	if _, err := storyboard(svc, sc.Script.ID); err == nil {
		t.Fatal("调用失败应当报错")
	}
	if bal, _ := bill.Balance(context.Background(), payer); bal != 100 {
		t.Fatalf("调用失败必须退款，余额 %d", bal)
	}
}

func TestStoryboard_失败后还能重新生成(t *testing.T) {
	// ⚠️ 轮次按**调用次数**算（含失败）。只按产出物算的话，一次失败什么都没改变，
	// 重试会算出同一个幂等键、命中那条 failed 记录 —— 永远 40902，
	// **用户再也点不动「重新生成」**。
	model := &fakeModel{reply: "坏输出"}
	svc, r, bill, _ := setup(t, model, 6)
	fund(t, bill, 100)
	sc := newScript(t, svc, "从前有座山")

	if _, err := storyboard(svc, sc.Script.ID); err == nil {
		t.Fatal("第一次应失败")
	}
	// 第二次：模型这回好好干活了。
	model.reply = goodReply
	res, err := storyboard(svc, sc.Script.ID)
	if err != nil {
		t.Fatalf("失败后必须还能重新生成: %v", err)
	}
	if len(res.Detail.Shots) != 2 {
		t.Fatalf("第二次应成功落镜头: %d", len(res.Detail.Shots))
	}
	if model.calls != 2 {
		t.Fatalf("模型应被调用 2 次: %d", model.calls)
	}
	// 两次都留了记录：一条 failed 一条 success，费用逐条可对账。
	calls, _ := r.ListCalls(context.Background(), sc.Script.ID, 10)
	if len(calls) != 2 {
		t.Fatalf("应有 2 条调用记录: %d", len(calls))
	}
	// 只扣了成功那一次的钱。
	if bal, _ := bill.Balance(context.Background(), payer); bal != 94 {
		t.Fatalf("只该扣成功那次的 6 分，余额 %d", bal)
	}
}

func TestStoryboard_重新生成会替换整批镜头(t *testing.T) {
	model := &fakeModel{reply: goodReply}
	svc, r, bill, _ := setup(t, model, 0)
	fund(t, bill, 100)
	sc := newScript(t, svc, "从前有座山")

	if _, err := storyboard(svc, sc.Script.ID); err != nil {
		t.Fatal(err)
	}
	// ⚠️ 整批替换要在一个事务里「先删再插」：镜号带唯一索引，
	// 新旧两批会重叠，逐条 upsert 必然撞索引。
	model.reply = `{"shots":[{"description":"只剩一个镜头","durationSec":5}]}`
	res, err := storyboard(svc, sc.Script.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Detail.Shots) != 1 {
		t.Fatalf("应替换成 1 个镜头: %d", len(res.Detail.Shots))
	}
	shots, _ := r.ListShots(context.Background(), sc.Script.ID)
	if len(shots) != 1 || shots[0].ShotNo != 1 {
		t.Fatalf("旧镜头没清干净: %+v", shots)
	}
}

// ---------------------------------------------------------------- 镜头表

func TestShots_增删改移与镜号连续(t *testing.T) {
	svc, r, bill, _ := setup(t, &fakeModel{reply: goodReply}, 0)
	fund(t, bill, 100)
	sc := newScript(t, svc, "从前有座山")
	if _, err := storyboard(svc, sc.Script.ID); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// 插一个镜头到第一个之后。
	shots, _ := r.ListShots(ctx, sc.Script.ID)
	d, err := svc.AddShot(ctx, scriptapp.AddShotInput{
		WorkspaceID: ws, ScriptID: sc.Script.ID, AfterShotID: shots[0].ID,
		DurationSec: 3, Description: "插入的",
	})
	if err != nil {
		t.Fatalf("AddShot: %v (cause=%v)", err, errors.Unwrap(err))
	}
	if len(d.Shots) != 3 || d.Shots[1].Description != "插入的" {
		t.Fatalf("插入位置不对: %+v", d.Shots)
	}
	assertContiguous(t, d)

	// 上移插入的那个。
	d, err = svc.MoveShot(ctx, ws, sc.Script.ID, d.Shots[1].ID, "up")
	if err != nil {
		t.Fatal(err)
	}
	if d.Shots[0].Description != "插入的" {
		t.Fatalf("上移没生效: %+v", d.Shots)
	}
	assertContiguous(t, d)

	// 复制第一个。
	d, err = svc.DuplicateShot(ctx, ws, sc.Script.ID, d.Shots[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Shots) != 4 || d.Shots[1].Description != "插入的" {
		t.Fatalf("复制位置不对: %+v", d.Shots)
	}
	// ⚠️ 副本的提示词状态必须是 pending：原样继承会让它一上来就是 ready，
	// 而它其实从没被合成过 —— 用户拿它去出图，出的是上一个镜头的画面。
	if d.Shots[1].ImagePromptStatus != "pending" {
		t.Fatalf("副本不该继承提示词状态: %s", d.Shots[1].ImagePromptStatus)
	}
	assertContiguous(t, d)

	// 删一个。
	d, err = svc.DeleteShot(ctx, ws, sc.Script.ID, d.Shots[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Shots) != 3 {
		t.Fatalf("删除没生效: %d", len(d.Shots))
	}
	assertContiguous(t, d)
}

func TestShots_重排要点全所有镜头(t *testing.T) {
	// ⚠️ 少传 ID 的话，没被点名的镜头会留在负数镜号区 ——
	// 它们在列表里排到最前面，而且谁也看不出为什么。宁可整体回滚。
	svc, r, bill, _ := setup(t, &fakeModel{reply: goodReply}, 0)
	fund(t, bill, 100)
	sc := newScript(t, svc, "从前有座山")
	if _, err := storyboard(svc, sc.Script.ID); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	shots, _ := r.ListShots(ctx, sc.Script.ID)

	if _, err := svc.ReorderShots(ctx, ws, sc.Script.ID, []int64{shots[0].ID}); err == nil {
		t.Fatal("少传镜头应被拒")
	}
	// 回滚干净：镜号没有负数。
	after, _ := r.ListShots(ctx, sc.Script.ID)
	for _, s := range after {
		if s.ShotNo <= 0 {
			t.Fatalf("回滚不干净，出现负数镜号: %+v", s)
		}
	}

	// 倒序重排。
	d, err := svc.ReorderShots(ctx, ws, sc.Script.ID,
		[]int64{shots[1].ID, shots[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if d.Shots[0].ID != shots[1].ID {
		t.Fatalf("重排没生效: %+v", d.Shots)
	}
	assertContiguous(t, d)
}

func TestSaveShot_版本冲突不覆盖(t *testing.T) {
	// AV-10：镜头保存有版本约束，冲突不能覆盖另一版本。
	svc, r, bill, _ := setup(t, &fakeModel{reply: goodReply}, 0)
	fund(t, bill, 100)
	sc := newScript(t, svc, "从前有座山")
	if _, err := storyboard(svc, sc.Script.ID); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	shots, _ := r.ListShots(ctx, sc.Script.ID)
	desc := "甲改的"
	if _, err := svc.SaveShot(ctx, scriptapp.SaveShotInput{
		WorkspaceID: ws, ScriptID: sc.Script.ID, ShotID: shots[0].ID,
		ExpectedVersion: shots[0].Version, Description: &desc,
	}); err != nil {
		t.Fatal(err)
	}
	// 乙拿着旧版本号再存：必须被拒。
	other := "乙改的"
	if _, err := svc.SaveShot(ctx, scriptapp.SaveShotInput{
		WorkspaceID: ws, ScriptID: sc.Script.ID, ShotID: shots[0].ID,
		ExpectedVersion: shots[0].Version, Description: &other,
	}); err == nil {
		t.Fatal("旧版本号应当被拒")
	}
	got, _ := r.GetShot(ctx, sc.Script.ID, shots[0].ID)
	if got.Description != "甲改的" {
		t.Fatalf("先写入的内容被覆盖了: %q", got.Description)
	}
}

func TestSaveShot_时长越界被拒(t *testing.T) {
	svc, r, bill, _ := setup(t, &fakeModel{reply: goodReply}, 0)
	fund(t, bill, 100)
	sc := newScript(t, svc, "从前有座山")
	if _, err := storyboard(svc, sc.Script.ID); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	shots, _ := r.ListShots(ctx, sc.Script.ID)
	bad := 999
	if _, err := svc.SaveShot(ctx, scriptapp.SaveShotInput{
		WorkspaceID: ws, ScriptID: sc.Script.ID, ShotID: shots[0].ID,
		ExpectedVersion: shots[0].Version, DurationSec: &bad,
	}); err == nil {
		t.Fatal("999 秒应被拒")
	}
}

// ---------------------------------------------------------------- 脚本版本与步骤

func TestPatch_版本冲突与步骤校验(t *testing.T) {
	svc, _, bill, _ := setup(t, &fakeModel{reply: goodReply}, 0)
	fund(t, bill, 100)
	sc := newScript(t, svc, "从前有座山")
	ctx := context.Background()

	// ⚠️ 没有镜头就想进第 2 步：服务端**独立校验**（AV-18），
	// 前端挡一道是体验，直接打 API 绕过前端是最容易发生的事。
	step := 2
	if _, err := svc.Patch(ctx, scriptapp.PatchInput{
		WorkspaceID: ws, ScriptID: sc.Script.ID,
		ExpectedVersion: sc.Script.Version, CurrentStep: &step,
	}); err == nil {
		t.Fatal("没有镜头不该能进资产步")
	}

	raw := "改过的剧情"
	updated, err := svc.Patch(ctx, scriptapp.PatchInput{
		WorkspaceID: ws, ScriptID: sc.Script.ID,
		ExpectedVersion: sc.Script.Version, RawInput: &raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Script.Version != sc.Script.Version+1 {
		t.Fatalf("版本应递增: %d", updated.Script.Version)
	}
	// 拿旧版本号再改：被拒。
	if _, err := svc.Patch(ctx, scriptapp.PatchInput{
		WorkspaceID: ws, ScriptID: sc.Script.ID,
		ExpectedVersion: sc.Script.Version, RawInput: &raw,
	}); err == nil {
		t.Fatal("旧版本号应当被拒")
	}
}

func TestGet_跨工作区取不到(t *testing.T) {
	// 归属校验与并发控制在同一条 where 里 —— 漏掉任一条都不会有编译错误，
	// 只会有一个安全洞。
	svc, _, _, _ := setup(t, &fakeModel{}, 0)
	sc := newScript(t, svc, "从前有座山")
	if _, err := svc.Get(context.Background(), ws+999, sc.Script.ID); err == nil {
		t.Fatal("别的工作区不该取得到")
	}
}

func assertContiguous(t *testing.T, d scriptapp.Detail) {
	t.Helper()
	for i, s := range d.Shots {
		if s.ShotNo != i+1 {
			t.Fatalf("镜号必须从 1 起连续: %+v", d.Shots)
		}
	}
}

// ⚠️ 这条钉住一个**时有时无**的唯一索引冲突：
// PostgreSQL 的唯一索引不延迟到语句结束才检查（经典的 `update t set i=i+1`
// 在 PG 上就会报重复）。新插入的镜头带着负数占位号，而临时号如果也用
// `-row_number()` 生成，两者会在同一条 UPDATE 里撞上 ——
// 谁先被更新取决于执行计划，于是集成测试过了、联调时炸了。
func TestShots_反复插入不会撞临时镜号(t *testing.T) {
	svc, r, bill, pool := setup(t, &fakeModel{reply: goodReply}, 0)
	fund(t, bill, 100)
	sc := newScript(t, svc, "从前有座山")
	if _, err := storyboard(svc, sc.Script.ID); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// 连续插入/复制多次：任何一次撞号都会在这里暴露。
	for i := 0; i < 6; i++ {
		shots, _ := r.ListShots(ctx, sc.Script.ID)
		if _, err := svc.DuplicateShot(ctx, ws, sc.Script.ID, shots[0].ID); err != nil {
			t.Fatalf("第 %d 次复制失败: %v", i+1, err)
		}
		d, err := svc.AddShot(ctx, scriptapp.AddShotInput{
			WorkspaceID: ws, ScriptID: sc.Script.ID, DurationSec: 4,
		})
		if err != nil {
			t.Fatalf("第 %d 次插入失败: %v", i+1, err)
		}
		assertContiguous(t, d)
	}
	// ⚠️ 任何时候都不该有镜头留在临时号上：留着的话它排到列表最前面，
	// 而且下一次插入会撞上同一个占位号，这个脚本从此再也插不进镜头。
	var negatives int
	if err := pool.QueryRow(ctx,
		`select count(*) from shot where script_id=$1 and shot_no <= 0`,
		sc.Script.ID).Scan(&negatives); err != nil {
		t.Fatal(err)
	}
	if negatives != 0 {
		t.Fatalf("有 %d 个镜头留在临时号上", negatives)
	}
}
