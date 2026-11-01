//go:build integration

package integration

// 智能剪辑名下的任务（docs/agent/smartcut SCT-37，SCD-33、SC-AC-58）：不挂画布节点、不登记作品、不发站内通知、
// 四条终态路径都回写；节点最近一次任务不受它影响。

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
	"github.com/li904338203/tabtv/backend/internal/generation/repo"
)

// scBinder 记下每一次回写的任务与状态。
type scBinder struct {
	mu    sync.Mutex
	calls []string
}

func (b *scBinder) BindSmartcut(_ context.Context, g repo.Generation, status string, _ []repo.Output, _ string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, fmt.Sprintf("%d:%s", g.ID, status))
	return nil
}

func (b *scBinder) list() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.calls...)
}

type scNotifier struct {
	mu sync.Mutex
	n  int
}

func (c *scNotifier) Notify(context.Context, int64, string, string, string, string, string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return nil
}

func acceptSmartcut(svc *genapp.Service, key string) (genapp.AcceptResult, error) {
	return svc.Accept(context.Background(), genapp.AcceptInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user, ProjectID: proj,
		OperationCode: "text2img", ModelCode: "dev_image", IdempotencyKey: key, Count: 1,
		Params: json.RawMessage(`{"prompt":"重拍"}`), Origin: gendomain.OriginSmartcut,
		SmartcutTarget: json.RawMessage(`{"timelineId":"77","kind":"replace","tool":"regen","clipId":"88","version":3}`),
	})
}

// SC-AC-58：智能剪辑名下的任务 node_id 为空、来源冻结进快照、回写目标落库；常规任务的快照里没有来源字段。
func TestInsert_smartcut任务node_id为空(t *testing.T) {
	svc, _, ledger, pool := setup(t, 10)
	fund(t, ledger, 100)
	res, err := acceptSmartcut(svc, "sc-node-1")
	if err != nil {
		t.Fatal(err)
	}
	var node *string
	var origin, target string
	if err := pool.QueryRow(context.Background(), `select node_id, coalesce(params_snapshot->'_snapshot'->>'origin',''),
		smartcut_target->>'kind' from generation where id = $1`, res.Generation.ID).Scan(&node, &origin, &target); err != nil {
		t.Fatal(err)
	}
	if node != nil || origin != "smartcut" || target != "replace" {
		t.Fatalf("node_id 为空、来源 smartcut、回写目标落库：%v %q %q", node, origin, target)
	}
	plain, err := accept(svc, "sc-node-plain")
	if err != nil {
		t.Fatal(err)
	}
	var hasOrigin bool
	if err := pool.QueryRow(context.Background(), `select params_snapshot->'_snapshot' ? 'origin' from generation where id = $1`,
		plain.Generation.ID).Scan(&hasOrigin); err != nil || hasOrigin {
		t.Fatalf("常规任务的快照逐字不变（没有 origin）：%v %v", hasOrigin, err)
	}
}

// SC-AC-58：成功的智能剪辑任务不登记作品（常规任务照常登记）。
func TestRegisterWork_smartcut来源不登记(t *testing.T) {
	svc, r, ledger, pool := setup(t, 10)
	ctx := context.Background()
	fund(t, ledger, 100)
	for _, c := range []struct {
		key  string
		sc   bool
		want bool
	}{{"work-sc", true, false}, {"work-plain", false, true}} {
		var res genapp.AcceptResult
		var err error
		if c.sc {
			res, err = acceptSmartcut(svc, c.key)
		} else {
			res, err = accept(svc, c.key)
		}
		if err != nil {
			t.Fatal(err)
		}
		media := res.Generation.ID + 1000
		if _, err := pool.Exec(ctx, `insert into media_object (id, workspace_id, bucket, object_key, content_type, size_bytes,
			purpose, created_by_user_id) values ($1,$2,'b',$3,'image/png',10,'generation_output',$4)`,
			media, ws, fmt.Sprintf("k/%d", media), user); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `insert into generation_output (id, generation_id, idx, media_object_id, output_type)
			values ($1,$2,0,$3,'image')`, media+1, res.Generation.ID, media); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `update generation set status = 'success' where id = $1`, res.Generation.ID); err != nil {
			t.Fatal(err)
		}
		got, err := r.RegisterWork(ctx, res.Generation.ID+2000, res.Generation.ID)
		if err != nil || got != c.want {
			t.Fatalf("%s：登记 %v，期望 %v（%v）", c.key, got, c.want, err)
		}
	}
}

// 节点最近一次任务只看挂在节点上的：智能剪辑名下的任务（node_id 为空）不会顶掉任何节点的状态。
func TestLatestByNode_不受smartcut任务影响(t *testing.T) {
	svc, r, ledger, _ := setup(t, 10)
	fund(t, ledger, 100)
	plain, err := accept(svc, "latest-plain")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acceptSmartcut(svc, "latest-sc"); err != nil {
		t.Fatal(err)
	}
	list, err := r.LatestByNode(context.Background(), ws, proj)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].GenerationID != plain.Generation.ID {
		t.Fatalf("只有挂在节点上的那一次：%+v", list)
	}
}

// SCD-33：四条终态路径（成功、失败、取消、运维补偿）都回写智能剪辑；智能剪辑名下的任务不发站内通知，常规任务照发。
func TestExecutor_四条终态路径都回调SmartcutBinder(t *testing.T) {
	svc, r, ledger, pool := setup(t, 10)
	ctx := context.Background()
	fund(t, ledger, 200)
	binder := &scBinder{}
	svc.SetSmartcutBinder(binder)
	notes := &scNotifier{}

	run := func(key string, outcome gendomain.SubmitOutcome) int64 {
		res, err := acceptSmartcut(svc, key)
		if err != nil {
			t.Fatal(err)
		}
		e := newExecutor(t, r, &fakeProvider{outcome: outcome, httpCode: 400}, ledger)
		e.SetSmartcutBinder(binder)
		e.SetNotifier(notes)
		if _, err := e.Execute(ctx, res.Generation.ID); err != nil {
			t.Fatal(err)
		}
		return res.Generation.ID
	}
	ok := run("four-success", gendomain.SubmitCompleted)
	failed := run("four-failed", gendomain.SubmitRejected)
	cancelled, err := acceptSmartcut(svc, "four-cancel")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Cancel(ctx, ws, cancelled.Generation.ID, ""); err != nil {
		t.Fatal(err)
	}
	stuck, err := acceptSmartcut(svc, "four-compensate")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update generation set status = 'running', deadline_at = now() - interval '1 hour' where id = $1`,
		stuck.Generation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CompensateFail(ctx, stuck.Generation.ID, "运维判定超时"); err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprint(binder.list())
	want := fmt.Sprint([]string{fmt.Sprintf("%d:success", ok), fmt.Sprintf("%d:failed", failed),
		fmt.Sprintf("%d:cancelled", cancelled.Generation.ID), fmt.Sprintf("%d:failed", stuck.Generation.ID)})
	if got != want {
		t.Fatalf("四条路径都回写：\n got %s\nwant %s", got, want)
	}
	if notes.n != 0 {
		t.Fatalf("智能剪辑名下的任务不发站内通知：%d", notes.n)
	}
	plain, err := accept(svc, "four-plain")
	if err != nil {
		t.Fatal(err)
	}
	e := newExecutor(t, r, &fakeProvider{outcome: gendomain.SubmitCompleted}, ledger)
	e.SetNotifier(notes)
	if _, err := e.Execute(ctx, plain.Generation.ID); err != nil {
		t.Fatal(err)
	}
	if notes.n != 1 {
		t.Fatalf("常规任务照发通知：%d", notes.n)
	}
}
