//go:build integration

// 首页最近项目带 Agent 会话（AGT-45）：内容域经端口向画布域要，不自己读 agent_session。
package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	canvasrepo "github.com/li904338203/tabtv/backend/internal/canvas/repo"
	contentapp "github.com/li904338203/tabtv/backend/internal/content/app"
	"github.com/li904338203/tabtv/backend/internal/content/domain"
)

// canvasAgents 与 cmd/api 里的适配器同形：把画布仓储接成内容域的端口。
type canvasAgents struct{ r *canvasrepo.Repo }

func (c canvasAgents) ProjectAgents(ctx context.Context, ws int64, ids []int64) (
	map[int64]contentapp.ProjectAgent, error) {
	m, err := c.r.AgentSummaries(ctx, ws, ids)
	if err != nil {
		return nil, err
	}
	out := map[int64]contentapp.ProjectAgent{}
	for id, b := range m {
		out[id] = contentapp.ProjectAgent{SessionID: b.SessionID, Title: b.Title, State: b.State,
			LastActiveAt: b.LastActiveAt}
	}
	return out, nil
}

type brokenAgents struct{}

func (brokenAgents) ProjectAgents(context.Context, int64, []int64) (map[int64]contentapp.ProjectAgent, error) {
	return nil, errors.New("模拟失败")
}

func TestHome_最近项目带Agent会话且失败只降级(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	const ws = int64(7790)

	mk := func(name string) int64 {
		id := next()
		if _, err := pool.Exec(ctx, `
			insert into project (id, workspace_id, created_by_user_id, name, canvas_data, status)
			values ($1,$2,1,$3,'{}'::jsonb,'active')`, id, ws, name); err != nil {
			t.Fatal(err)
		}
		return id
	}
	plain := mk("普通")
	agentProj := mk("Agent")
	sid := next()
	if _, err := pool.Exec(ctx, `
		insert into agent_session (id, workspace_id, project_id, payer_account_id, created_by_user_id,
			title, state, last_active_at)
		values ($1,$2,$3,1,1,'做一条短片','awaiting_input',$4)`, sid, ws, agentProj, time.Now()); err != nil {
		t.Fatal(err)
	}

	svc := contentapp.New(r)
	svc.SetProjectAgents(canvasAgents{canvasrepo.New(pool)})
	h := svc.HomeFor(ctx, ws, domain.PositionHome, time.Now())
	if len(h.Recent) != 2 {
		t.Fatalf("应有 2 个最近项目，得到 %d", len(h.Recent))
	}
	if _, ok := h.RecentAgents[plain]; ok {
		t.Errorf("普通项目不应带会话")
	}
	if a := h.RecentAgents[agentProj]; a.SessionID != sid || a.Title != "做一条短片" || a.State != "awaiting_input" {
		t.Errorf("Agent 项目应带最近会话，得到 %+v", a)
	}

	// 端口失败：项目照常给，会话一律缺省（降级成普通卡片），首页不报错。
	svc.SetProjectAgents(brokenAgents{})
	h = svc.HomeFor(ctx, ws, domain.PositionHome, time.Now())
	if len(h.Recent) != 2 || len(h.RecentAgents) != 0 {
		t.Errorf("端口失败应只降级会话信息，得到 recent=%d agents=%v", len(h.Recent), h.RecentAgents)
	}
}
