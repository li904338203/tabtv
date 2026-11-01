package tools

import (
	"context"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
)

// 提示词（MCPT-16，MCP-F-37）：从站内技能（agent_skill）派生，与站内 Agent 共用同一份技能内容。
//
// 名字 = 技能 code（如 story_topic、video_single）；正文 = **渲染后的**技能正文（Skills 端口负责变量给全、
// 渲染失败的不出现）+ 代码生成的「站内工具名 → MCP 工具名」对照（domain.AgentToolNotice）。
// ⚠️ 每次现查：运营在技能后台停用一个技能，下一次 prompts/list 就没有它。
type skillPrompts struct{ d Deps }

func (s skillPrompts) List(ctx context.Context) ([]app.PromptInfo, error) {
	if s.d.Skills == nil {
		return []app.PromptInfo{}, nil
	}
	list, err := s.d.Skills.Prompts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]app.PromptInfo, 0, len(list))
	for _, sk := range list {
		out = append(out, app.PromptInfo{Name: sk.Code, Title: sk.Name, Description: sk.Description})
	}
	return out, nil
}

func (s skillPrompts) Get(ctx context.Context, name string) (app.PromptBody, error) {
	if s.d.Skills == nil {
		return app.PromptBody{}, app.ErrPromptNotFound
	}
	list, err := s.d.Skills.Prompts(ctx)
	if err != nil {
		return app.PromptBody{}, err
	}
	name = strings.TrimSpace(name)
	for _, sk := range list {
		if sk.Code == name {
			return app.PromptBody{Description: sk.Description, Text: sk.Body + domain.AgentToolNotice()}, nil
		}
	}
	return app.PromptBody{}, app.ErrPromptNotFound
}
