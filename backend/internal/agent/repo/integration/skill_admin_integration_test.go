//go:build integration

// 管理端技能配置（AGT-60）的真库测试：乐观锁要在真并发下测（顺序调用永远先撞上别的检查，
// 把 version 条件整条删掉照样绿，CLAUDE.md §5.7）；停用不删行、用户端不可见、按 code 仍可追溯。
// 推理模型目录用假实现（目录域的 is_agent_capable 校验在 catalog 自己的测试里）。
package integration

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

type itSkillModels []agentapp.SkillModelInfo

func (m itSkillModels) SkillModels(context.Context) ([]agentapp.SkillModelInfo, error) { return m, nil }

func newSkillAdmin(t *testing.T) (*agentapp.SkillAdmin, *repo.Repo) {
	t.Helper()
	r, _, next := setup(t)
	a := agentapp.NewSkillAdmin(r, itSkillModels{
		{Code: "chat_agent", Domain: "text", Status: "enabled", AgentCapable: true},
		{Code: "chat_plain", Domain: "text", Status: "enabled"},
	}, wiring.AgentSkillRenderer{}, next)
	return a, r
}

func skillIn(code string) agentapp.SkillInput {
	return agentapp.SkillInput{Code: code, Name: "测试技能", Category: "通用",
		Body: "当前状态 {{state}}，推理模型 {{reasoningModel}}", Tags: []string{"测试"},
		Steps: []domain.SkillStep{{No: 1, Name: "了解需求"}, {No: 2, Name: "确认规划", Checkpoint: true}},
		Tools: []string{"read_canvas", "load_skill", "read_skill_ref"}}
}

func errCode(err error) errcode.Code {
	var be *httpx.BizError
	if errors.As(err, &be) {
		return be.Code
	}
	return 0
}

func TestSkillAdmin_版本冲突(t *testing.T) {
	a, r := newSkillAdmin(t)
	ctx := context.Background()
	s, err := a.Create(ctx, 1, skillIn("conflict_skill"))
	if err != nil {
		t.Fatal(err)
	}
	// 8 个管理员拿同一个版本同时保存：恰好一个成功，其余都是版本冲突，版本只进一位。
	const n = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		ok, conf int
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			in := skillIn("conflict_skill")
			in.Name = "改名" + string(rune('A'+i))
			_, err := a.Update(ctx, int64(i+1), s.ID, s.Version, in)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errCode(err) == errcode.VersionConflict:
				conf++
			default:
				t.Errorf("意外错误：%v", err)
			}
		}(i)
	}
	wg.Wait()
	if ok != 1 || conf != n-1 {
		t.Fatalf("成功 %d 次、冲突 %d 次，期望 1 / %d", ok, conf, n-1)
	}
	cur, err := r.GetSkillByID(ctx, s.ID)
	if err != nil || cur.Version != s.Version+1 {
		t.Fatalf("版本应只进一位：%v %d", err, cur.Version)
	}

	// 附属文档的写推进技能版本：拿旧版本写附属文档也冲突，且不留半个附属文档。
	if _, err := a.SaveRef(ctx, 1, s.ID, 0, s.Version, agentapp.SkillRefInput{Name: "指南", Body: "x"}); errCode(err) != errcode.VersionConflict {
		t.Fatalf("旧版本写附属文档应冲突：%v", err)
	}
	v, err := a.SaveRef(ctx, 1, s.ID, 0, cur.Version, agentapp.SkillRefInput{Name: "指南", Body: "状态 {{state}}"})
	if err != nil || v != cur.Version+1 {
		t.Fatalf("新建附属文档失败或版本没推进：%v %d", err, v)
	}
	refs, _ := r.ListSkillReferences(ctx, s.ID)
	if len(refs) != 1 {
		t.Fatalf("附属文档 %d 份，期望 1", len(refs))
	}
	// 同名附属文档：名称冲突，且技能版本不因失败的写而推进（同一事务回滚）。
	if _, err := a.SaveRef(ctx, 1, s.ID, 0, v, agentapp.SkillRefInput{Name: "指南", Body: "y"}); errCode(err) != errcode.DuplicateName {
		t.Fatalf("同名附属文档应报重名：%v", err)
	}
	if cur, _ := r.GetSkillByID(ctx, s.ID); cur.Version != v {
		t.Fatalf("失败的写推进了版本：%d → %d", v, cur.Version)
	}
	if _, err := a.DeleteRef(ctx, 1, s.ID, refs[0].ID, v); err != nil {
		t.Fatalf("删除附属文档失败：%v", err)
	}
}

func TestSkillAdmin_推理模型必须支持Agent(t *testing.T) {
	a, r := newSkillAdmin(t)
	ctx := context.Background()
	in := skillIn("reasoning_skill")
	in.Bindings.Reasoning = "chat_plain"
	_, err := a.Create(ctx, 1, in)
	if errCode(err) != errcode.InvalidArgument || !strings.Contains(err.Error(), "不支持 Agent") {
		t.Fatalf("非 Agent 推理模型应被拒：%v", err)
	}
	if _, err := r.GetSkillByCode(ctx, "reasoning_skill"); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("被拒的技能不应落库：%v", err)
	}
	in.Bindings.Reasoning = "chat_agent"
	if _, err := a.Create(ctx, 1, in); err != nil {
		t.Fatalf("Agent 推理模型应可绑定：%v", err)
	}
	// 渲染失败（未知变量）同样整笔拒绝。
	bad := skillIn("render_skill")
	bad.Body = "按 {{story}} 写"
	if _, err := a.Create(ctx, 1, bad); errCode(err) != errcode.InvalidArgument || !strings.Contains(err.Error(), "渲染失败") {
		t.Fatalf("渲染失败应被拒：%v", err)
	}
}

func TestSkillAdmin_停用后用户端不可见(t *testing.T) {
	a, r := newSkillAdmin(t)
	ctx := context.Background()
	s, err := a.Create(ctx, 1, skillIn("toggle_skill"))
	if err != nil {
		t.Fatal(err)
	}
	visible := func() bool {
		list, err := r.ListEnabledSkills(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range list {
			if x.Code == "toggle_skill" {
				return true
			}
		}
		return false
	}
	if visible() {
		t.Fatal("新建的技能（停用）不应出现在用户端")
	}
	if _, err := a.SetEnabled(ctx, 1, s.ID, true); err != nil {
		t.Fatal(err)
	}
	if !visible() {
		t.Fatal("启用后应出现在用户端")
	}
	if _, err := a.SetEnabled(ctx, 1, s.ID, false); err != nil {
		t.Fatal(err)
	}
	if visible() {
		t.Fatal("停用后仍出现在用户端")
	}
	// 停用不删行：按 code 仍取得到（已加载过它的会话要能说清「已停用」）。
	got, err := r.GetSkillByCode(ctx, "toggle_skill")
	if err != nil || got.IsEnabled {
		t.Fatalf("停用后按 code 应仍可取到且为停用：%v %+v", err, got.IsEnabled)
	}
	all, err := a.List(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("管理端列表应仍含停用的技能：%v %d", err, len(all))
	}
}
