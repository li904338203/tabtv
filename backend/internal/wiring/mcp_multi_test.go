package wiring

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	agentdomain "github.com/li904338203/tabtv/backend/internal/agent/domain"
	agentrepo "github.com/li904338203/tabtv/backend/internal/agent/repo"
	mcpdomain "github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

type fakeSkillLister []agentrepo.Skill

func (f fakeSkillLister) Skills(context.Context) ([]agentrepo.Skill, error) { return f, nil }

func quietSkills(rows ...agentrepo.Skill) MCPSkills {
	return MCPSkills{Lister: fakeSkillLister(rows), Render: AgentSkillRenderer{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// 提示词内容是**渲染后的**技能正文：变量给全（SkillVars 的全部键都有值），没有 `{{` 残留；
// 渲染失败（用了不认识的变量）的技能不出现 —— 原样给出的话外部模型会照着 `{{var}}` 字面量执行。
func TestPromptFromSkill_RenderedNoPlaceholder(t *testing.T) {
	s := quietSkills(
		agentrepo.Skill{Code: "video_single", Name: "单段成片", IsEnabled: true,
			Body: "当前会话阶段：{{state}}。推理模型：{{reasoningModel}}。先读画布。"},
		agentrepo.Skill{Code: "broken", Name: "坏的", IsEnabled: true, Body: "{{notAVar}}"},
	)
	list, err := s.Prompts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Code != "video_single" {
		t.Fatalf("渲染失败的技能不应出现：%+v", list)
	}
	if strings.Contains(list[0].Body, "{{") || !strings.Contains(list[0].Body, "executing") {
		t.Fatalf("应已渲染：%q", list[0].Body)
	}
	// 变量与站内 Agent 同一份（SkillVars 的键一个不少）。
	for k := range agentapp.SkillVars(agentdomain.StatePlanning, "") {
		if _, ok := mcpSkillVars()[k]; !ok {
			t.Errorf("MCP 渲染缺变量 %s", k)
		}
	}
}

// 停用的技能不出现（列表本来只含启用的，这里再挡一道）。
func TestPromptFromSkill_DisabledHidden(t *testing.T) {
	s := quietSkills(agentrepo.Skill{Code: "a", IsEnabled: true, Body: "x"},
		agentrepo.Skill{Code: "b", IsEnabled: false, Body: "y"})
	list, err := s.Prompts(context.Background())
	if err != nil || len(list) != 1 || list[0].Code != "a" {
		t.Fatalf("%+v %v", list, err)
	}
}

// 营销专用技能不出现在 MCP 提示词里（MKD-22，MKT-T-32）：scenes=[] 与含 general 的出现，
// 只含 marketing 的不出现；prompts/get 走同一份列表，所以按名取也取不到。
func TestPromptFromSkill_营销专用技能不出现(t *testing.T) {
	s := quietSkills(
		agentrepo.Skill{Code: "plain", IsEnabled: true, Body: "x", Scenes: []byte(`[]`)},
		agentrepo.Skill{Code: "both", IsEnabled: true, Body: "x", Scenes: []byte(`["general","marketing"]`)},
		agentrepo.Skill{Code: "mk_only", IsEnabled: true, Body: "x", Scenes: []byte(`["marketing"]`)},
	)
	list, err := s.Prompts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, p := range list {
		got = append(got, p.Code)
	}
	if strings.Join(got, ",") != "plain,both" {
		t.Fatalf("MCP 只应列通用场景技能，得到 %v", got)
	}
}

// 「站内工具名 → MCP 工具名」对照要覆盖站内 Agent 的**全部**工具：漏一个，模型照着技能调那个工具就是
// 「工具不存在」，而它不知道该换成哪个。站内加工具而忘了补对照时这里当场红。
func TestMCPPromptToolMap_覆盖全部Agent工具(t *testing.T) {
	for _, spec := range agentdomain.Tools() {
		if _, ok := mcpdomain.AgentToolMapping[string(spec.Name)]; !ok {
			t.Errorf("对照表缺站内工具 %s（mcp/domain.AgentToolMapping）", spec.Name)
		}
	}
	if !strings.Contains(mcpdomain.AgentToolNotice(), "read_canvas → canvas_read") {
		t.Fatal(mcpdomain.AgentToolNotice())
	}
}

// Agent 适配器翻出来的余额不足要还原成原样的业务错误（MCP 回 42201 + 差额，不是给站内模型的话）。
func TestUnAgentErr_还原余额不足(t *testing.T) {
	biz := httpx.Errf(errcode.InsufficientCredits, "余额不足")
	err := unAgentErr(&agentapp.InsufficientCreditsError{Balance: -1, Cause: biz})
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.InsufficientCredits {
		t.Fatalf("%v", err)
	}
	other := errors.New("x")
	if unAgentErr(other) != other {
		t.Fatal("其余错误原样返回")
	}
}
