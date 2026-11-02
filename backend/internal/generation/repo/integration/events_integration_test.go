//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
)

// captureEvents 记下推出去的状态事件（按契约形状解开）。
type captureEvents struct {
	mu  sync.Mutex
	got []genapp.GenerationUpdate
	ws  []int64
}

func (c *captureEvents) PublishState(_ context.Context, workspaceID int64, payload json.RawMessage) error {
	var u genapp.GenerationUpdate
	if err := json.Unmarshal(payload, &u); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, u)
	c.ws = append(c.ws, workspaceID)
	return nil
}

func (c *captureEvents) last(t *testing.T, generationID int64) genapp.GenerationUpdate {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.got) - 1; i >= 0; i-- {
		if c.got[i].GenerationID == itoa(generationID) {
			if c.ws[i] != ws {
				t.Fatalf("事件应按任务所在工作区投递（前端订的是 workspace:%d），得到 %d", ws, c.ws[i])
			}
			return c.got[i]
		}
	}
	t.Fatalf("任务 %d 进入终态后没有推事件 —— 前端轮询耗尽之后就再也等不到它", generationID)
	return genapp.GenerationUpdate{}
}

func (c *captureEvents) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.got)
}

// ⚠️ 四条进入终态的路径都要推：此前只有 Worker 的成功 / 失败推了，
// 用户取消与运维补偿发生在 API 进程里、一条都不推；而失败事件里也没有原因。
func TestEvents_四条终态路径都推事件且带原因(t *testing.T) {
	svc, r, ledger, pool := setup(t, 10)
	ctx := context.Background()
	fund(t, ledger, 100)
	events := &captureEvents{}
	svc.SetEvents(events)

	// ① Worker 成功：带产物地址
	ok1, err := accept(svc, "evt-success")
	if err != nil {
		t.Fatal(err)
	}
	g, _ := r.ClaimForRun(ctx, ok1.Generation.ID, 0)
	if _, err := pool.Exec(ctx, `
		insert into media_object (id, workspace_id, bucket, object_key, content_type,
			size_bytes, purpose, created_by_user_id)
		values (700001,$1,'b','k700001','image/png',10,'generation_output',1)`, ws); err != nil {
		t.Fatal(err)
	}
	e := newExecutor(t, r, &fakeProvider{}, ledger)
	e.SetArchiver(&fakeArchiver{})
	e.SetIDGen(idGen(t))
	e.SetEvents(events)
	if done, err := e.Complete(ctx, g, []genapp.ProviderOutput{{Type: "image", MIME: "image/png"}}, nil); err != nil || !done {
		t.Fatalf("应成功：%v %v", done, err)
	}
	if u := events.last(t, g.ID); u.Status != gendomain.StatusSuccess || u.Error != nil ||
		len(u.Outputs) != 1 || u.Outputs[0].URL != gendomain.OutputMediaURL(700001) {
		t.Fatalf("成功事件应带产物地址、不带 error：%+v", u)
	}

	// ② Worker 失败：带原因
	bad, err := accept(svc, "evt-failed")
	if err != nil {
		t.Fatal(err)
	}
	fe := newExecutor(t, r, &fakeProvider{outcome: gendomain.SubmitRejected, httpCode: 400}, ledger)
	fe.SetEvents(events)
	if _, err := fe.Execute(ctx, bad.Generation.ID); err != nil {
		t.Fatal(err)
	}
	if u := events.last(t, bad.Generation.ID); u.Status != gendomain.StatusFailed || u.Error == nil ||
		u.Error.Reason != "provider_rejected" || u.Error.Message == "" || u.Error.Message == "fake" {
		// 原因码要对、文案要有；上游原话（这里是 "fake"）不进用户可见字段（usererr，B-17）。
		t.Fatalf("失败事件应带原因（此前没有，节点上只有一个红叉）：%+v %+v", u, u.Error)
	}

	// ③ 用户取消（API 进程）：推 cancelled；再取消一次被拒，不重复推
	c, err := accept(svc, "evt-cancel")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Cancel(ctx, ws, c.Generation.ID, "不要了"); err != nil {
		t.Fatal(err)
	}
	if u := events.last(t, c.Generation.ID); u.Status != gendomain.StatusCancelled || u.Error == nil ||
		u.Error.Reason != gendomain.ErrorCodeCancelled || u.Error.Message != "不要了" {
		t.Fatalf("取消应推 cancelled 事件并带原因：%+v", u)
	}
	before := events.count()
	if err := svc.Cancel(ctx, ws, c.Generation.ID, "再点一次"); err == nil {
		t.Fatal("重复取消应被拒")
	}
	if events.count() != before {
		t.Fatal("被拒的取消不该再推一条事件（会让前端把一个已落定的任务再处理一遍）")
	}

	// ④ 运维补偿（API 进程）：推 failed + ops_compensated
	stuck, err := accept(svc, "evt-compensate")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update generation set status='running' where id=$1`, stuck.Generation.ID); err != nil {
		t.Fatal(err)
	}
	if changed, err := svc.CompensateFail(ctx, stuck.Generation.ID, "卡住了"); err != nil || !changed {
		t.Fatalf("补偿应生效：%v %v", changed, err)
	}
	if u := events.last(t, stuck.Generation.ID); u.Status != gendomain.StatusFailed || u.Error == nil ||
		u.Error.Reason != gendomain.ErrorCodeOpsCompensated {
		t.Fatalf("运维补偿应推 failed + ops_compensated：%+v", u)
	}
}
