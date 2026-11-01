package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/li904338203/tabtv/backend/internal/marketing/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// ConstraintTemplate 是场景约束段的模板 code（MKD-15）：拼在生成提示词末尾，**不进** Agent 上下文。
const ConstraintTemplate = "agent.scene.marketing.constraint"

// SetPrompts 接上模板渲染（取场景约束段）。为 nil 时约束段为空（只用于测试）。
func (s *Service) SetPrompts(p PromptRenderer) { s.prompts = p }

// AppendixResult 是生成侧营销段（dev/01 §4.1）。
type AppendixResult struct {
	// Text = Compose(钉住版本的原文, 约束段)：创意 → Hook → 风格 → 场景约束段。
	Text string
	// Picks 是 `kind:code@version`，只给受理日志用。
	Picks []string
}

// Appendix 按生成参数里的 `params.marketing.picks` 拼营销段（MKT-T-21、MKD-6）。
//
//   - 没有 `marketing` 段 → 空结果：**通用生成一个字都不拼**（MKR-10）。
//   - 有 `marketing` 段（哪怕一样都没选）→ 约束段**总是追加**：它是商品保真规则，不是选项的附属。
//   - 只按钉住的 (code, version) 取历史原文（Snapshot），**不看是否在投**：已下线 / 已过期的
//     选项照常拼（MKT-AC-08、11），否则一次运营操作会让进行中的会话突然少一段配方而任务是成功的。
//   - params 形状不对、钉住的版本不存在 → 40001（只可能是手改 params，不是用户能在界面上解决的事），
//     details 点名 kind / code / version。
func (s *Service) Appendix(ctx context.Context, params json.RawMessage) (AppendixResult, error) {
	picks, present, err := domain.ParsePicks(params)
	if err != nil {
		return AppendixResult{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	if !present {
		return AppendixResult{}, nil
	}
	snaps, err := s.Snapshot(ctx, picks)
	if err != nil {
		var biz *httpx.BizError
		if errors.As(err, &biz) && biz.Code == errcode.MarketingOptionUnavailable {
			// Snapshot 的「版本不存在」在受理侧是参数错误：钉住的 (code, version) 由服务端写入，
			// 取不到只能是 params 被手改过。
			return AppendixResult{}, notPinned(picks, biz)
		}
		return AppendixResult{}, err
	}
	var constraint string
	if s.prompts != nil {
		c, err := s.prompts.RenderPrompt(ctx, ConstraintTemplate, "", nil)
		if err != nil {
			// ⚠️ 不悄悄去掉约束段：少了它商品保真规则就没发出去，而任务是成功的。
			return AppendixResult{}, err
		}
		constraint = c
	}
	out := AppendixResult{Text: domain.Compose(snaps, constraint), Picks: make([]string, 0, len(snaps))}
	for _, sn := range snaps {
		out.Picks = append(out.Picks, string(sn.Kind)+":"+sn.Code+"@"+strconv.Itoa(sn.Version))
	}
	return out, nil
}

// notPinned 把 Snapshot 的 42215 not_found 翻成 40001，并补上钉住的版本号。
func notPinned(picks []domain.Pick, biz *httpx.BizError) error {
	kind, _ := biz.Details["kind"].(string)
	code, _ := biz.Details["code"].(string)
	version := 0
	for _, p := range picks {
		if string(p.Kind) == kind && p.Code == code {
			version = p.Version
			break
		}
	}
	return httpx.Errf(errcode.InvalidArgument,
		fmt.Sprintf("params.marketing 钉住的营销选项 %s v%d 不存在", code, version)).
		WithDetails(map[string]any{"kind": kind, "code": code, "version": version})
}
