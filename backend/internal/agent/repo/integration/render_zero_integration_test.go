//go:build integration

// 导出免费（docs/agent/smartcut SCK-3，SCT-41）：Agent 的 render 在额度已经用尽时、报价为 0 也照常提交，不挂起、不弹追加额度卡。
// 用真的额度外壳（app/budget.go）—— 「0 积分放行」的判断只有一处（domain.CheckBudget），不在 render 里特判。
package integration

import (
	"context"
	"encoding/json"
	"testing"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	agenttools "github.com/li904338203/tabtv/backend/internal/agent/tools"
)

// zeroRenderTimelines 是只够 render 用的时间线端口：预检通过、报价 0。
type zeroRenderTimelines struct{ started []agentapp.RenderRequest }

func (f *zeroRenderTimelines) Ensure(context.Context, agentapp.TimelineEnsureRequest) (agentapp.TimelineView, error) {
	return agentapp.TimelineView{}, nil
}

func (f *zeroRenderTimelines) Assemble(context.Context, agentapp.AssembleRequest) (agentapp.AssembleOutcome, error) {
	return agentapp.AssembleOutcome{}, nil
}

func (f *zeroRenderTimelines) PrecheckRender(context.Context, agentapp.RenderRequest) (agentapp.RenderPrecheck, error) {
	return agentapp.RenderPrecheck{DurationMs: 8000, Credits: 0}, nil
}

func (f *zeroRenderTimelines) StartRender(_ context.Context, in agentapp.RenderRequest) (agentapp.RenderView, error) {
	f.started = append(f.started, in)
	return agentapp.RenderView{ID: 9501, Status: "queued"}, nil
}

func TestRender_0积分不弹额度卡(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	s := paidSession(t, r, pool, next, 10)
	b := agentapp.NewBudget(r, next, nil)
	// 先把额度用尽。
	if _, susp, err := b.PaidCall(ctx, paidRC(s, next(), 1), 10, func(string) (agentapp.PaidResult, error) {
		return agentapp.PaidResult{GenerationID: next(), Credits: 10}, nil
	}); err != nil || susp != nil {
		t.Fatalf("用尽额度：%v %v", susp, err)
	}
	tl := &zeroRenderTimelines{}
	var render agenttools.Tool
	for _, tool := range agenttools.MultiTools(agenttools.MultiDeps{Timelines: tl, Budget: b}) {
		if tool.Name() == domain.ToolRender {
			render = tool
		}
	}
	res, err := render.Run(ctx, paidRC(s, next(), 1), json.RawMessage(`{"timelineId":"77","wait":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Suspend != nil && res.Suspend.Event == domain.EvOverBudget {
		t.Fatalf("0 积分不该挂起要额度：%+v", res.Suspend)
	}
	if len(tl.started) != 1 || tl.started[0].TimelineID != 77 {
		t.Fatalf("应照常提交导出：%+v", tl.started)
	}
	if n := occupied(t, pool, s.ID); n != 10 {
		t.Fatalf("0 积分不占额度：%d", n)
	}
	var cards int
	if err := pool.QueryRow(ctx, `select count(*)::int from agent_card where session_id = $1`, s.ID).Scan(&cards); err != nil ||
		cards != 0 {
		t.Fatalf("不弹追加额度卡：%d %v", cards, err)
	}
}
