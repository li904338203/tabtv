//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	"github.com/li904338203/tabtv/backend/internal/generation/repo"
)

// 作品自动登记：用户不再手动提交作品，成功的图片/视频/音频生成自动进作品库（落 pending）。

// failingWorks 模拟登记失败。
type failingWorks struct{ calls int }

func (f *failingWorks) RegisterWork(context.Context, int64, int64) (bool, error) {
	f.calls++
	return false, errors.New("作品库写不进去")
}

func completeWith(t *testing.T, key string, outputs []genapp.ProviderOutput,
	works func(*repo.Repo) genapp.WorkRegistrar) (int64, *repo.Repo, func(string, ...any) int) {
	t.Helper()
	svc, r, ledger, pool := setup(t, 10)
	ctx := context.Background()
	fund(t, ledger, 100)
	if _, err := pool.Exec(ctx, `
		insert into project (id, workspace_id, created_by_user_id, name)
		values ($1,$2,$3,'  夏日汽水  ')`, proj, ws, user); err != nil {
		t.Fatal(err)
	}
	res, err := accept(svc, key)
	if err != nil {
		t.Fatal(err)
	}
	g, _ := r.ClaimForRun(ctx, res.Generation.ID, 0)
	e := newExecutor(t, r, &fakeProvider{}, ledger)
	e.SetArchiver(&fakeArchiver{})
	e.SetIDGen(idGen(t))
	if works != nil {
		e.SetWorkRegistrar(works(r))
	}
	ok, err := e.Complete(ctx, g, outputs, json.RawMessage(`{"ok":true}`))
	if err != nil || !ok {
		t.Fatalf("应标成功: ok=%v err=%v", ok, err)
	}
	count := func(sql string, args ...any) int {
		var n int
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	return g.ID, r, count
}

// 成功一次登记一件 pending 作品；媒体取「第一个有媒体的产出」，标题取项目名（去空白）；
// 重复成功（重复消息）不重复登记，也不重置审核状态。
func TestComplete_自动登记与幂等(t *testing.T) {
	ctx := context.Background()
	works := &recordingWorks{}
	genID, r, count := completeWith(t, "auto-work-2", []genapp.ProviderOutput{
		{Type: "text", Text: "先出一段文字"},
		{Type: "image", MIME: "image/png"},
		{Type: "image", MIME: "image/png"},
	}, works.bind)
	if works.calls != 1 || !works.lastOK {
		t.Fatalf("成功一次应新登记一件: calls=%d ok=%v", works.calls, works.lastOK)
	}
	// 文本在 idx 0，第一张图在 idx 1 —— 媒体应是 idx 1 的那个。
	if n := count(`
		select count(*) from work w
		join generation_output o on o.generation_id = w.source_id and o.idx = 1
		where w.source_type='generation' and w.source_id=$1
		  and w.media_object_id = o.media_object_id
		  and w.review_status='pending' and not w.is_featured and not w.is_offline
		  and w.submitted_at is null and w.title='夏日汽水'`, genID); n != 1 {
		t.Fatalf("应登记一件 pending 作品（媒体取第一个媒体产出、标题取项目名），实际 %d", n)
	}
	// 重复成功（重复消息）不重复登记，也不重置审核状态。
	if _, err := r.Pool().Exec(ctx,
		`update work set review_status='rejected' where source_id=$1`, genID); err != nil {
		t.Fatal(err)
	}
	ok, err := r.RegisterWork(ctx, 990002, genID)
	if err != nil || ok {
		t.Fatalf("重复登记应为无操作: ok=%v err=%v", ok, err)
	}
	if n := count(`select count(*) from work where source_id=$1 and review_status='rejected'`,
		genID); n != 1 {
		t.Fatalf("重复登记不得新建也不得把已驳回洗回待审，实际 %d", n)
	}
}

// 纯文本生成不登记。
func TestComplete_文本生成不登记作品(t *testing.T) {
	works := &recordingWorks{}
	genID, r, count := completeWith(t, "auto-work-text", []genapp.ProviderOutput{
		{Type: "text", Text: "一段文字"},
	}, works.bind)
	if works.calls != 0 {
		t.Fatalf("没有媒体产出不该调用登记，实际 %d 次", works.calls)
	}
	// 仓储侧也兜住：即使被调用，没有媒体产出也不落行。
	if ok, err := r.RegisterWork(context.Background(), 990003, genID); err != nil || ok {
		t.Fatalf("文本生成不该登记: ok=%v err=%v", ok, err)
	}
	if n := count(`select count(*) from work where source_id=$1`, genID); n != 0 {
		t.Fatalf("文本生成不该有作品，实际 %d", n)
	}
}

// 登记失败只告警：任务照样成功，扣费不退。
func TestComplete_登记失败不影响生成成功(t *testing.T) {
	works := &failingWorks{}
	genID, _, count := completeWith(t, "auto-work-fail", []genapp.ProviderOutput{
		{Type: "image", MIME: "image/png"},
	}, func(*repo.Repo) genapp.WorkRegistrar { return works })
	if works.calls != 1 {
		t.Fatalf("应尝试登记一次，实际 %d", works.calls)
	}
	if n := count(`select count(*) from generation where id=$1 and status='success'`, genID); n != 1 {
		t.Fatal("登记失败不得影响生成成功")
	}
	if n := count(`select count(*) from credit_transaction where ref_id=$1 and type='refund'`,
		itoa(genID)); n != 0 {
		t.Fatalf("登记失败不得触发退款，实际 %d 条退款流水", n)
	}
}

// 取消/失败的任务不登记：仓储按 status='success' 过滤。
func TestRegisterWork_非成功任务不登记(t *testing.T) {
	svc, r, ledger, _ := setup(t, 10)
	ctx := context.Background()
	fund(t, ledger, 100)
	res, err := accept(svc, "auto-work-cancel")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Cancel(ctx, ws, res.Generation.ID, "不要了"); err != nil {
		t.Fatal(err)
	}
	if ok, err := r.RegisterWork(ctx, 990004, res.Generation.ID); err != nil || ok {
		t.Fatalf("已取消的任务不该登记: ok=%v err=%v", ok, err)
	}
}

// recordingWorks 转调真仓储并计数。
type recordingWorks struct {
	r      *repo.Repo
	calls  int
	lastOK bool
}

func (w *recordingWorks) bind(r *repo.Repo) genapp.WorkRegistrar {
	w.r = r
	return w
}

func (w *recordingWorks) RegisterWork(ctx context.Context, id, generationID int64) (bool, error) {
	w.calls++
	ok, err := w.r.RegisterWork(ctx, id, generationID)
	w.lastOK = ok
	return ok, err
}
