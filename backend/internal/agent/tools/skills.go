package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

// load_skill / read_skill_ref（dev/01 §5.3、design/01 §10）。
//
// load_skill 把技能追加到 agent_session.skill_codes：Runner 每次调用模型前按它组装系统提示词
// （技能主说明）并收窄工具（AllowedTools 与技能白名单取交集 —— 只能收窄，不能放宽）。
// ⚠️ 正文先在这里渲染一次：渲染失败（缺变量 / 未闭合占位符）**报错且不加载**，不回落原文
// （原文里的 `{{var}}` 会被模型当字面量照着做，§5.7）。变量与 Runner 同一份（app.SkillVars）。

const maxSkillCodes = 8

// LoadSkill 是 load_skill 工具。
type LoadSkill struct{ d PlanningDeps }

func (LoadSkill) Name() domain.ToolName { return domain.ToolLoadSkill }
func (LoadSkill) Paid() bool            { return false }
func (LoadSkill) Action() collab.Action { return collab.ActionView }

func (LoadSkill) Description() string {
	return "加载一个技能：它的主说明会从下一次起放进你的系统提示词，并可能收窄你可用的工具。" +
		"返回技能的步骤；附属文档用 read_skill_ref 按名称读取。"
}

func (LoadSkill) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","required":["code"],"properties":{` +
		`"code":{"type":"string","description":"技能 code"}},"additionalProperties":false}`)
}

type loadSkillArgs struct {
	Code string `json:"code"`
}

// enabledSkill 取启用的技能；不存在与已停用给出不同的说法（模型据此决定要不要换一个）。
func enabledSkill(ctx context.Context, s Store, code string) (repo.Skill, error) {
	code = ptTrim(code)
	if code == "" {
		return repo.Skill{}, fmt.Errorf("缺少 code")
	}
	sk, err := s.GetSkillByCode(ctx, code)
	if errors.Is(err, repo.ErrNotFound) {
		return repo.Skill{}, fmt.Errorf("技能 %q 不存在", code)
	}
	if err != nil {
		return repo.Skill{}, fmt.Errorf("读取技能失败：%w", err)
	}
	if !sk.IsEnabled {
		return repo.Skill{}, fmt.Errorf("技能 %q 已停用", code)
	}
	return sk, nil
}

func renderSkill(r app.SkillRenderer, rc RunContext, body string) (string, error) {
	if r == nil {
		return body, nil
	}
	return r.RenderSkill(body, app.SkillVars(rc.State, rc.ModelCode))
}

func (t LoadSkill) Run(ctx context.Context, rc RunContext, args json.RawMessage) (ToolResult, error) {
	var a loadSkillArgs
	if err := ptDecode(args, &a, false); err != nil {
		return ToolResult{}, err
	}
	sk, err := enabledSkill(ctx, t.d.Store, a.Code)
	if err != nil {
		return ToolResult{}, err
	}
	if _, err := renderSkill(t.d.Skills, rc, sk.Body); err != nil {
		return ToolResult{}, fmt.Errorf("技能 %q 的主说明渲染失败，未加载：%w", sk.Code, err)
	}
	sess, err := t.d.Store.GetByID(ctx, rc.SessionID)
	if err != nil {
		return ToolResult{}, fmt.Errorf("读取会话失败：%w", err)
	}
	// 场景隔离（04 文档 P-22②）：只加载适用于本会话场景的技能（同 /agent/skills?scene= 的过滤）。
	if !domain.SkillInScene(sk.SceneList(), domain.SceneKey(sess.Scene)) {
		return ToolResult{}, fmt.Errorf("技能 %q 不适用于当前会话的场景，换一个适用的技能", sk.Code)
	}
	var codes []string
	if len(sess.SkillCodes) > 0 {
		if err := json.Unmarshal(sess.SkillCodes, &codes); err != nil {
			codes = nil
		}
	}
	already := false
	for _, c := range codes {
		if c == sk.Code {
			already = true
			break
		}
	}
	if !already {
		if len(codes) >= maxSkillCodes {
			return ToolResult{}, fmt.Errorf("一个会话最多加载 %d 个技能", maxSkillCodes)
		}
		codes = append(codes, sk.Code)
		if err := t.d.Store.UpdateSkills(ctx, rc.SessionID, ptJSON(codes)); err != nil {
			return ToolResult{}, fmt.Errorf("记录已加载技能失败：%w", err)
		}
	}
	var tools []string
	_ = json.Unmarshal(sk.Tools, &tools)
	out := map[string]any{"code": sk.Code, "name": sk.Name, "loaded": true, "alreadyLoaded": already,
		"note": "主说明已放入系统提示词（下一次调用起生效）"}
	if len(sk.Steps) > 0 && string(sk.Steps) != "[]" && string(sk.Steps) != "null" {
		out["steps"] = sk.Steps
	}
	if len(tools) > 0 {
		out["toolWhitelist"] = tools
	}
	return ToolResult{
		Content: fitJSON(out),
		Display: ptJSON(map[string]any{"code": sk.Code, "name": sk.Name, "description": sk.Description}),
	}, nil
}

// fitJSON 序列化；超过上限时去掉 steps（它最长且可以再取）。
func fitJSON(m map[string]any) json.RawMessage {
	b := ptJSON(m)
	if ptRunes(b) <= MaxContentRunes {
		return b
	}
	delete(m, "steps")
	m["truncated"] = true
	return ptJSON(m)
}

// ReadSkillRef 是 read_skill_ref 工具。
type ReadSkillRef struct{ d PlanningDeps }

func (ReadSkillRef) Name() domain.ToolName { return domain.ToolReadSkillRef }
func (ReadSkillRef) Paid() bool            { return false }
func (ReadSkillRef) Action() collab.Action { return collab.ActionView }

func (ReadSkillRef) Description() string {
	return "读取技能的一份附属文档（流程指引、写法指南、示例）。"
}

func (ReadSkillRef) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","required":["code","name"],"properties":{` +
		`"code":{"type":"string","description":"技能 code"},"name":{"type":"string","description":"附属文档名"}` +
		`},"additionalProperties":false}`)
}

type readSkillRefArgs struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

func (t ReadSkillRef) Run(ctx context.Context, rc RunContext, args json.RawMessage) (ToolResult, error) {
	var a readSkillRefArgs
	if err := ptDecode(args, &a, false); err != nil {
		return ToolResult{}, err
	}
	sk, err := enabledSkill(ctx, t.d.Store, a.Code)
	if err != nil {
		return ToolResult{}, err
	}
	name := ptTrim(a.Name)
	if name == "" {
		return ToolResult{}, fmt.Errorf("缺少 name")
	}
	ref, err := t.d.Store.SkillReferenceByName(ctx, sk.ID, name)
	if errors.Is(err, repo.ErrNotFound) {
		return ToolResult{}, fmt.Errorf("技能 %q 没有名为 %q 的附属文档", sk.Code, name)
	}
	if err != nil {
		return ToolResult{}, fmt.Errorf("读取附属文档失败：%w", err)
	}
	body, err := renderSkill(t.d.Skills, rc, ref.Body)
	if err != nil {
		return ToolResult{}, fmt.Errorf("附属文档 %q 渲染失败：%w", name, err)
	}
	body, cut := ptCutRunes(body, maxDocReadRunes)
	out := map[string]any{"code": sk.Code, "name": ref.Name, "content": body}
	if cut {
		out["truncated"] = true
	}
	return ToolResult{
		Content: ptJSON(out),
		Display: ptJSON(map[string]any{"code": sk.Code, "skillName": sk.Name, "name": ref.Name}),
	}, nil
}

var (
	_ app.Tool = LoadSkill{}
	_ app.Tool = ReadSkillRef{}
)
