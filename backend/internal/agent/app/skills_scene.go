package app

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 用户端技能列表的场景过滤与技能封面（MKT-T-05；MKT-D-10、13、14，MKD-19）。

// SkillModelRef 是技能绑定的一个模型：code + 显示名（取不到名时退回 code）。
type SkillModelRef struct {
	Code string
	Name string
}

// SkillListItem 是用户端技能列表的一项：技能行 + 已解析出显示名的绑定模型。
// 绑定为空串的槽位不出现（nil）。
type SkillListItem struct {
	repo.Skill
	Video     *SkillModelRef
	Image     *SkillModelRef
	Reasoning *SkillModelRef
}

// ParseSkillScene 解析查询参数 scene：空 = general；其余只认 general / marketing。
func ParseSkillScene(v string) (string, error) {
	switch v {
	case "":
		return domain.SkillSceneGeneral, nil
	case domain.SkillSceneGeneral, domain.SkillSceneMarketing:
		return v, nil
	}
	return "", httpx.Errf(errcode.InvalidArgument, "scene 只能是 general 或 marketing")
}

// SkillsInScene 列出 scene 场景下启用的技能（MKT-AC-15）。
//
// ⚠️ 过滤只按 domain.SkillInScene 判（唯一实现），不在 SQL 里另写一份 jsonb 条件：
// 技能总共几十条，两份判定迟早不一致，而不一致的表现是某个技能只在一个页面上消失。
// ⚠️ 模型显示名取不到（目录查询失败、模型已删）时**退回 code** 而不是整页报错：
// 一个标签的名字不该让技能弹层打不开。
func (s *Service) SkillsInScene(ctx context.Context, scene string) ([]SkillListItem, error) {
	rows, err := s.repo.ListEnabledSkills(ctx)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	names := map[string]string{}
	if s.p.SkillModels != nil {
		if ms, err := s.p.SkillModels.SkillModels(ctx); err == nil {
			for _, m := range ms {
				names[m.Code] = m.Name
			}
		} else {
			s.log.Warn("技能列表取模型显示名失败，退回 code", "err", err)
		}
	}
	ref := func(code string) *SkillModelRef {
		if code == "" {
			return nil
		}
		n := names[code]
		if n == "" {
			n = code
		}
		return &SkillModelRef{Code: code, Name: n}
	}
	out := make([]SkillListItem, 0, len(rows))
	for _, sk := range rows {
		if !domain.SkillInScene(sk.SceneList(), scene) {
			continue
		}
		var b domain.SkillBindings
		_ = json.Unmarshal(sk.ModelBindings, &b)
		// 输入槽声明按新形状下发（MKD-26）：库里可能还是旧形状，前端只认新形状。
		if s.p.SlotDecls != nil && hasSlots(sk.InputSlots) {
			sk.InputSlots = s.p.SlotDecls.NormalizeSlotDecl(sk.InputSlots)
		}
		out = append(out, SkillListItem{Skill: sk, Video: ref(b.Video), Image: ref(b.Image),
			Reasoning: ref(b.Reasoning)})
	}
	return out, nil
}

// SkillCoverMedia 返回**已启用**技能的封面媒体 ID（公开取图用，MKD-19）。
//
// ⚠️ 停用、不存在、没配封面一律同一个 404：区分开等于告诉访客「曾经有过这个技能」。
// 判定在这里一次做完，调用方拿到 ID 后直接 ServePublic（那一步不做归属校验）。
func (s *Service) SkillCoverMedia(ctx context.Context, code string) (int64, error) {
	sk, err := s.repo.GetSkillByCode(ctx, code)
	if errors.Is(err, repo.ErrNotFound) || (err == nil && (!sk.IsEnabled || sk.CoverMediaID == 0)) {
		return 0, httpx.Errf(errcode.NotFound, "技能封面不存在")
	}
	if err != nil {
		return 0, httpx.Wrap(errcode.Internal, err)
	}
	return sk.CoverMediaID, nil
}
