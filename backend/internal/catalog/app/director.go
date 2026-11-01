package app

import (
	"context"
	"errors"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/catalog/domain"
	"github.com/li904338203/tabtv/backend/internal/catalog/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// ---------------------------------------------------------------- 3D 导演台（docs/agent/director）

// sceneBuildMaxImages 是 AI 搭景参考图的上限（DR-PARAM-14，设计建议值）：还要与模型的 image 上限取小。
const sceneBuildMaxImages = 4

// SceneBuildChoice 是 AI 搭景这一次要用的模型。
type SceneBuildChoice struct {
	ModelCode string
	// MaxImages 参考图上限：模型在 scene_build 上 image 角色的上限与 DR-PARAM-14 取小；不收图为 0。
	MaxImages int
}

// SceneBuildUnavailable 说明 AI 搭景现在为什么不可用。
type SceneBuildUnavailable struct {
	Reason string
	// UserFixable 是用户自己能解决的（会员等级不够），其余要运营处理。
	UserFixable bool
}

// SceneBuildChoice 取 AI 搭景此刻可用的第一个模型（DRD-22）：绑定了 scene_build 的模型按排序逐个报价，
// 取第一个**报得出 0 积分**的（同 Agent 的默认推理模型的选法）。
//
// ⚠️ 免费靠配置成立，不靠代码特例：照常走能力合取式（模型、Provider、绑定、有价、能力矩阵、会员等级）。
// 报价 > 0 时**显式拒绝**：忽略价格的话「配了价格却不收」就是「选了等于没选」；真要收费是改决定与代码。
// ⚠️ build 与 build-config 都调这一个函数（DR-F-108），不另写一份可用性判断。
func (s *Service) SceneBuildChoice(ctx context.Context, memberLevelID int64, imageCount int) (
	SceneBuildChoice, *SceneBuildUnavailable, error) {
	return s.freeTextChoice(ctx, repo.OpSceneBuild, "AI 搭景", memberLevelID, imageCount, sceneBuildMaxImages)
}

// freeTextChoice 是「免费的同步看图文本调用」选模型的同一套规则（AI 搭景、AI 剪辑共用）：
// 绑定了 op 的模型按排序逐个报价，取第一个报得出 0 积分的；图片上限取模型在 op 上 image 角色的上限与 maxImages 的小者。
// label 只用在不可用原因的文案里。
func (s *Service) freeTextChoice(ctx context.Context, op, label string, memberLevelID int64, imageCount, maxImages int) (
	SceneBuildChoice, *SceneBuildUnavailable, error) {
	models, err := s.repo.ListModelsForOperation(ctx, op)
	if err != nil {
		return SceneBuildChoice{}, nil, httpx.Wrap(errcode.Internal, err)
	}
	if len(models) == 0 {
		return SceneBuildChoice{}, &SceneBuildUnavailable{
			Reason: label + "暂不可用：后台还没有给" + label + "（" + op + "）绑定可用的模型"}, nil
	}
	counts := domain.RoleCount{domain.RoleText: 1}
	if imageCount > 0 {
		counts[domain.RoleImage] = imageCount
	}
	var first *SceneBuildUnavailable
	priced := false
	for _, code := range models {
		q, err := s.QuoteWithFacts(ctx, QuoteRequest{ModelCode: code, OperationCode: op, Count: 1,
			Text: label, MemberLevelID: memberLevelID, RoleCounts: counts})
		if err != nil {
			if first == nil {
				first = quoteUnavailableFor(label, err)
			}
			continue
		}
		if q.Credits > 0 {
			priced = true
			continue
		}
		max, found, err := s.InputLimit(ctx, code, op, domain.RoleImage)
		if err != nil {
			return SceneBuildChoice{}, nil, err
		}
		if !found {
			max = 0
		}
		if max > maxImages {
			max = maxImages
		}
		return SceneBuildChoice{ModelCode: code, MaxImages: max}, nil, nil
	}
	if priced {
		return SceneBuildChoice{}, &SceneBuildUnavailable{
			Reason: label + "按约定免费，当前配置了价格，请联系运营改为 0"}, nil
	}
	return SceneBuildChoice{}, first, nil
}

// quoteUnavailableFor 把报价错误转成不可用原因：会员等级不够是用户自己能解决的，其余要运营处理。
func quoteUnavailableFor(label string, err error) *SceneBuildUnavailable {
	var be *httpx.BizError
	if errors.As(err, &be) {
		return &SceneBuildUnavailable{Reason: label + "暂不可用：" + be.Error(),
			UserFixable: be.Code == errcode.MemberLevelRequired}
	}
	return &SceneBuildUnavailable{Reason: label + "暂不可用：" + err.Error()}
}

// 导演台约束段的模板（DRT-07，DRD-18），按产物种类取。
var directorGuardCodes = []struct{ kind, code string }{
	{"capture", "director.reference_guard.capture"},
	{"preview", "director.reference_guard.preview"},
}

// DirectorGuardPrompt 渲染 3D 导演台的下游约束段（实现 generation/app.DirectorGuardSource）：
// 输入里有截图就加截图那段、有预演视频就加视频那段，两者都有时先图后视频，各占一行。
//
// 模板（DB 行）优先，渲染失败回落代码兜底并告警（同营销场景段的做法）；兜底也取不到时返回错误，
// 受理随之失败 —— ⚠️ 不静默少一段：少了它模型照着灰色素模画，而任务是「成功」的。
func (s *Service) DirectorGuardPrompt(ctx context.Context, kinds []string) (string, error) {
	want := map[string]bool{}
	for _, k := range kinds {
		want[k] = true
	}
	var parts []string
	for _, g := range directorGuardCodes {
		if !want[g.kind] {
			continue
		}
		text, err := s.RenderPrompt(ctx, g.code, "", nil)
		if err != nil {
			if s.log != nil {
				s.log.WarnContext(ctx, "导演台约束段模板渲染失败，改用代码兜底", "code", g.code, "error", err)
			}
			if text, err = s.RenderFallbackPrompt(g.code, nil); err != nil {
				return "", err
			}
		}
		if t := strings.TrimSpace(text); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n"), nil
}
