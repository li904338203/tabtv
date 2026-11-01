//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
	"github.com/li904338203/tabtv/backend/internal/generation/repo"
)

// fakeArchiver 记录归档次数；mediaID 递增。
type fakeArchiver struct {
	mu     sync.Mutex
	n      int64
	fail   bool
	usages []int64
}

func (a *fakeArchiver) Archive(context.Context, genapp.ArchiveInput) (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fail {
		return 0, io.ErrUnexpectedEOF
	}
	a.n++
	return 700000 + a.n, nil
}

func (a *fakeArchiver) RegisterUsage(_ context.Context, mediaID int64, _ string, _ int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.usages = append(a.usages, mediaID)
	return nil
}

// 多产物必须全部归档：只显示第一张等于把另外几张白扣的积分丢了。
func TestComplete_多产物全部归档(t *testing.T) {
	svc, r, ledger, pool := setup(t, 10)
	ctx := context.Background()
	fund(t, ledger, 100)
	res, err := accept(svc, "multi-out")
	if err != nil {
		t.Fatal(err)
	}
	g, _ := r.ClaimForRun(ctx, res.Generation.ID, 0)

	arch := &fakeArchiver{}
	e := newExecutor(t, r, &fakeProvider{}, ledger)
	e.SetArchiver(arch)
	gen := idGen(t)
	e.SetIDGen(gen)

	// 造出媒体行，满足产物表的外键语义（逻辑外键，但查询要join得到）。
	for i := int64(1); i <= 3; i++ {
		if _, err := pool.Exec(ctx, `
			insert into media_object (id, workspace_id, bucket, object_key, content_type,
				size_bytes, purpose, created_by_user_id)
			values ($1,$2,'b',$3,'image/png',10,'generation_output',1)`,
			700000+i, ws, fmt.Sprintf("k%d", 700000+i)); err != nil {
			t.Fatal(err)
		}
	}

	ok, err := e.Complete(ctx, g, []genapp.ProviderOutput{
		{Type: "image", MIME: "image/png"},
		{Type: "image", MIME: "image/png"},
		{Type: "text", Text: "一段描述"},
	}, json.RawMessage(`{"ok":true}`))
	if err != nil || !ok {
		t.Fatalf("归档应成功: ok=%v err=%v", ok, err)
	}

	outs, err := r.ListOutputs(ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 3 {
		t.Fatalf("应有 3 个产物，实际 %d", len(outs))
	}
	if outs[2].OutputType != "text" || outs[2].TextContent != "一段描述" {
		t.Errorf("文本产物应直接存内容，实际 %+v", outs[2])
	}
	// 产物要登记引用，否则会被媒体回收队列收走。
	if len(arch.usages) != 2 {
		t.Errorf("应为 2 个媒体产物登记引用，实际 %d", len(arch.usages))
	}
	var status string
	_ = pool.QueryRow(ctx, `select status from generation where id=$1`, g.ID).Scan(&status)
	if status != "success" {
		t.Errorf("应标成功，实际 %s", status)
	}
}

// 归档失败**不标成功**：存储失败时显示成功是最糟的失败 ——
// 既不触发退款，也不会被重试捞起。
func TestComplete_归档失败不标成功(t *testing.T) {
	svc, r, ledger, pool := setup(t, 10)
	ctx := context.Background()
	fund(t, ledger, 100)
	res, _ := accept(svc, "arch-fail")
	g, _ := r.ClaimForRun(ctx, res.Generation.ID, 0)

	e := newExecutor(t, r, &fakeProvider{}, ledger)
	e.SetArchiver(&fakeArchiver{fail: true})
	e.SetIDGen(idGen(t))

	ok, err := e.Complete(ctx, g, []genapp.ProviderOutput{{Type: "image", MIME: "image/png"}}, nil)
	if err == nil {
		t.Fatal("归档失败应报错")
	}
	if ok {
		t.Fatal("归档失败不该标成功")
	}
	var status string
	_ = pool.QueryRow(ctx, `select status from generation where id=$1`, g.ID).Scan(&status)
	if status != "running" {
		t.Errorf("应留在 running 等重试，实际 %s", status)
	}
	var n int
	_ = pool.QueryRow(ctx, `select count(*) from generation_output where generation_id=$1`, g.ID).Scan(&n)
	if n != 0 {
		t.Errorf("不该留下产物记录，实际 %d", n)
	}
}

// 取消与成功的竞争：谁先拿到状态条件谁赢。
func TestCancel_与成功竞争(t *testing.T) {
	svc, r, ledger, pool := setup(t, 10)
	ctx := context.Background()
	fund(t, ledger, 100)
	res, _ := accept(svc, "race-cancel")
	id := res.Generation.ID
	if _, err := pool.Exec(ctx, `update generation set status='running' where id=$1`, id); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	var cancelOK, finishOK bool
	wg.Add(2)
	go func() {
		defer wg.Done()
		cancelOK, _ = r.Cancel(ctx, ws, id, "用户取消")
	}()
	go func() {
		defer wg.Done()
		finishOK, _ = r.Finish(ctx, repo.FinishInput{ID: id, Status: "success", Progress: 100})
	}()
	wg.Wait()

	// ⚠️ 只能有一个赢：两个都成功意味着状态被覆盖过一次。
	if cancelOK == finishOK {
		t.Fatalf("取消与完成只应有一个生效，实际 cancel=%v finish=%v", cancelOK, finishOK)
	}
	var status string
	_ = pool.QueryRow(ctx, `select status from generation where id=$1`, id).Scan(&status)
	if status != "cancelled" && status != "success" {
		t.Fatalf("状态应为二者之一，实际 %s", status)
	}
}

// 已完成的任务取消不了：那时钱已经花在上游了。
func TestCancel_已完成的任务不能取消(t *testing.T) {
	svc, r, ledger, pool := setup(t, 10)
	ctx := context.Background()
	fund(t, ledger, 100)
	res, _ := accept(svc, "cancel-done")
	id := res.Generation.ID
	if _, err := pool.Exec(ctx, `update generation set status='success' where id=$1`, id); err != nil {
		t.Fatal(err)
	}
	ok, err := r.Cancel(ctx, ws, id, "太晚了")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("已完成的任务不该能取消")
	}
}

// 取消退款一次性。
func TestCancel_退款只发生一次(t *testing.T) {
	svc, r, ledger, _ := setup(t, 10)
	ctx := context.Background()
	fund(t, ledger, 100)
	res, _ := accept(svc, "cancel-refund")
	id := res.Generation.ID
	svc.SetRefunder(refunderAdapter{ledger})

	if err := svc.Cancel(ctx, ws, id, "不要了"); err != nil {
		t.Fatal(err)
	}
	if bal, _ := ledger.Balance(ctx, payer); bal != 100 {
		t.Fatalf("取消应退款，实际余额 %d", bal)
	}
	// 再取消一次：状态已是终态，应被拒且不重复退款。
	if err := svc.Cancel(ctx, ws, id, "再点一次"); err == nil {
		t.Error("重复取消应被拒")
	}
	if bal, _ := ledger.Balance(ctx, payer); bal != 100 {
		t.Fatalf("不该重复退款，实际余额 %d", bal)
	}
	_ = r
}

// 通知幂等：Worker 重试同一任务不会发第二条。
func TestNotification_幂等(t *testing.T) {
	_, r, _, pool := setup(t, 10)
	ctx := context.Background()
	gen := idGen(t)
	for i := 0; i < 3; i++ {
		if err := r.InsertNotification(ctx, gen(), 42, "generation_success",
			"生成完成", "", "generation", "999", ""); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	_ = pool.QueryRow(ctx, `select count(*) from notification where user_id=42`).Scan(&n)
	if n != 1 {
		t.Fatalf("应只有 1 条通知，实际 %d", n)
	}
}

// 产物读取带工作区归属：别的工作区取不到。
func TestGetOutput_工作区隔离(t *testing.T) {
	svc, r, ledger, pool := setup(t, 10)
	ctx := context.Background()
	fund(t, ledger, 100)
	res, _ := accept(svc, "out-scope")
	gen := idGen(t)
	if _, err := pool.Exec(ctx, `
		insert into generation_output (id, generation_id, idx, output_type, text_content)
		values ($1,$2,0,'text','hi')`, gen(), res.Generation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetOutput(ctx, ws, res.Generation.ID, 0); err != nil {
		t.Fatalf("本工作区应能取到: %v", err)
	}
	if _, err := r.GetOutput(ctx, ws+1, res.Generation.ID, 0); err == nil {
		t.Fatal("别的工作区不该取到产物")
	}
}

var _ = strings.TrimSpace

// recordingBinder 记下资产回写。
type recordingBinder struct {
	assetID int64
	mediaID int64
	status  string
	calls   int
}

func (b *recordingBinder) BindAsset(_ context.Context, assetID, mediaID int64, status string) error {
	b.calls++
	b.assetID, b.mediaID, b.status = assetID, mediaID, status
	return nil
}

// ⚠️ 不回写的话资产会**永远停在 generating**：图已经归档了，
// 但资产列表里那一项一直转圈，用户既看不到图也不敢重试。
func TestComplete_资产图成功后回写状态(t *testing.T) {
	svc, r, ledger, pool := setup(t, 10)
	ctx := context.Background()
	fund(t, ledger, 100)

	res, err := svc.Accept(ctx, genapp.AcceptInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user, ProjectID: proj,
		NodeID: "n_asset", OperationCode: "asset_gen", ModelCode: "dev_image",
		IdempotencyKey: "asset-1", Count: 1,
		Params: json.RawMessage(`{"prompt":"x"}`), ScriptAssetID: 424242,
	})
	if err != nil {
		t.Fatal(err)
	}
	// ⚠️ assetId 必须真的落库：不落的话 Worker 无从知道该回写哪个资产。
	var stored int64
	if err := pool.QueryRow(ctx,
		`select coalesce(script_asset_id,0) from generation where id=$1`,
		res.Generation.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 424242 {
		t.Fatalf("script_asset_id 没落库: %d", stored)
	}

	binder := &recordingBinder{}
	e := newExecutor(t, r, &fakeProvider{}, ledger)
	e.SetArchiver(&fakeArchiver{})
	e.SetAssetBinder(binder)
	g, err := r.ClaimForRun(ctx, res.Generation.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := e.Complete(ctx, g, []genapp.ProviderOutput{
		{Type: "image", MIME: "image/png", Size: 3, Body: strings.NewReader("png")},
	}, nil)
	if err != nil || !ok {
		t.Fatalf("Complete 应成功: %v %v", ok, err)
	}
	if binder.calls != 1 || binder.assetID != 424242 || binder.status != "ready" {
		t.Fatalf("应回写 ready: %+v", binder)
	}
	if binder.mediaID == 0 {
		t.Fatal("要带上归档后的 mediaId，否则资产还是没有图")
	}
}

func TestFail_资产图失败也回写(t *testing.T) {
	// 停在 generating 的话用户既看不到图也不敢重试 —— 失败同样要说清楚。
	svc, r, ledger, _ := setup(t, 10)
	ctx := context.Background()
	fund(t, ledger, 100)
	res, err := svc.Accept(ctx, genapp.AcceptInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user, ProjectID: proj,
		NodeID: "n_asset2", OperationCode: "asset_gen", ModelCode: "dev_image",
		IdempotencyKey: "asset-2", Count: 1,
		Params: json.RawMessage(`{"prompt":"x"}`), ScriptAssetID: 515151,
	})
	if err != nil {
		t.Fatal(err)
	}
	binder := &recordingBinder{}
	e := newExecutor(t, r, &fakeProvider{outcome: gendomain.SubmitRejected, httpCode: 400}, ledger)
	e.SetAssetBinder(binder)
	if _, err := e.Execute(ctx, res.Generation.ID); err != nil {
		t.Fatal(err)
	}
	if binder.calls != 1 || binder.status != "failed" {
		t.Fatalf("失败也要回写: %+v", binder)
	}
}

// 普通任务（没有 assetId）不该触发回写。
func TestComplete_普通任务不调资产回写(t *testing.T) {
	svc, r, ledger, _ := setup(t, 10)
	ctx := context.Background()
	fund(t, ledger, 100)
	res, err := accept(svc, "plain-1")
	if err != nil {
		t.Fatal(err)
	}
	binder := &recordingBinder{}
	e := newExecutor(t, r, &fakeProvider{}, ledger)
	e.SetArchiver(&fakeArchiver{})
	e.SetAssetBinder(binder)
	g, _ := r.ClaimForRun(ctx, res.Generation.ID, 0)
	if _, err := e.Complete(ctx, g, []genapp.ProviderOutput{
		{Type: "text", Text: "ok"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if binder.calls != 0 {
		t.Fatalf("普通任务不该触发资产回写: %+v", binder)
	}
}
