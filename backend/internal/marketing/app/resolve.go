package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/li904338203/tabtv/backend/internal/marketing/domain"
	"github.com/li904338203/tabtv/backend/internal/marketing/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 建会话 / 规划 / 生成时把用户的选择翻成「钉住的版本原文」（MKT-T-17、20、21 用）。
//
// 两个入口的语义**相反**，不能合成一个加布尔参数：
//   - Resolve：用户**此刻**提交的选择 —— 要求存在 ∧ 在投 ∧ kind 对得上 ∧ 适用产出，
//     取当前版本钉住。已下线的不许新选（MKT-AC-11 后半句）。
//   - Snapshot：会话里**已经钉住**的 (code, version) —— 只按版本历史取，**不看是否在投**。
//     会话开始后运营下线了这个创意，这个会话里的后续生成仍按原版本执行（MKT-AC-11 前半句）；
//     看是否在投的话，一次运营操作会让一批进行中的会话突然少一段配方，而任务是成功的。

// 42215 的 details.reason。
const (
	ReasonDisabled       = "disabled"
	ReasonNotStarted     = "not_started"
	ReasonEnded          = "ended"
	ReasonOutputMismatch = "output_mismatch"
	ReasonNotFound       = "not_found"
)

// PickRef 是用户提交的一次选择：只有 kind + code，版本由服务端在受理那一刻取当前版钉住
// （客户端报的版本不可信，也没有意义：他看到的列表可能是几分钟前的）。
type PickRef struct {
	Kind domain.Kind `json:"kind"`
	Code string      `json:"code"`
}

// Resolve 校验并钉住用户此刻的选择，返回各项当前版本的原文（含片段，**只给服务端用**）。
//
// outputType 是本次产出（video / image），空串 = 不判适用产出。
// 每个维度最多一项（MKT-F-14）；同一维度出现两次是客户端的错，回 40001。
// 失败回 42215，details 点名维度、code 与原因；按 userFixable 分语气（MKT-UX-07）：
// 产出不符是用户换个技能 / 产出就能解决的，其余（下线、不在时段、不存在）是运营那边的事。
func (s *Service) Resolve(ctx context.Context, picks []PickRef, outputType string, now time.Time) (
	[]domain.Snapshot, error) {
	seen := map[domain.Kind]bool{}
	for i, p := range picks {
		if !p.Kind.Valid() {
			return nil, httpx.Errf(errcode.InvalidArgument, fmt.Sprintf("picks[%d].kind 只能是 creative / hook / style", i))
		}
		if seen[p.Kind] {
			return nil, httpx.Errf(errcode.InvalidArgument, fmt.Sprintf("%s只能选一个", p.Kind.Label()))
		}
		seen[p.Kind] = true
	}
	out := make([]domain.Snapshot, 0, len(picks))
	for _, p := range picks {
		o, err := s.resolveOne(ctx, p, outputType, now)
		if err != nil {
			return nil, err
		}
		out = append(out, domain.Snapshot{
			Pick: domain.Pick{Kind: o.Kind, Code: o.Code, Version: o.Version},
			Name: o.Name, Description: o.Description, Prompt: o.Prompt, Tags: o.Tags,
			IsAudioRequired: o.IsAudioRequired, OutputTypes: o.OutputTypes, InputRequirements: o.InputRequirements,
		})
	}
	return out, nil
}

func (s *Service) resolveOne(ctx context.Context, p PickRef, outputType string, now time.Time) (domain.Option, error) {
	if !domain.ValidCode(p.Code) {
		return domain.Option{}, unavailable(p.Kind, p.Code, "", ReasonNotFound)
	}
	row, err := s.store.GetOptionByCode(ctx, p.Code)
	if errors.Is(err, repo.ErrNotFound) {
		return domain.Option{}, unavailable(p.Kind, p.Code, "", ReasonNotFound)
	}
	if err != nil {
		return domain.Option{}, httpx.Wrap(errcode.Internal, err)
	}
	o := row.Option
	// kind 对不上按「不存在」报：code 全局唯一，拿一个 Hook 的 code 当创意提交，
	// 在「创意」这个维度里它确实不存在。
	if o.Kind != p.Kind {
		return domain.Option{}, unavailable(p.Kind, p.Code, "", ReasonNotFound)
	}
	// ⚠️ 判定走 domain.Lifecycle（Visible 的展开），与列表、详情、封面是同一个函数。
	switch domain.Lifecycle(o, now) {
	case domain.LifecycleDisabled:
		return domain.Option{}, unavailable(p.Kind, p.Code, o.Name, ReasonDisabled)
	case domain.LifecycleNotStarted:
		return domain.Option{}, unavailable(p.Kind, p.Code, o.Name, ReasonNotStarted)
	case domain.LifecycleEnded:
		return domain.Option{}, unavailable(p.Kind, p.Code, o.Name, ReasonEnded)
	}
	if outputType != "" && len(o.OutputTypes) > 0 && !containsStr(o.OutputTypes, outputType) {
		return domain.Option{}, unavailable(p.Kind, p.Code, o.Name, ReasonOutputMismatch)
	}
	return o, nil
}

// Snapshot 按钉住的 (kind, code, version) 取历史原文，**不看是否在投**。
//
// 版本行只增不改（MKD-5），按理永远取得到；取不到（或 kind 对不上）说明钉住的数据坏了，
// 回 42215 not_found —— 不悄悄跳过：跳过的话这次生成少了一段配方而任务是成功的。
func (s *Service) Snapshot(ctx context.Context, pins []domain.Pick) ([]domain.Snapshot, error) {
	out := make([]domain.Snapshot, 0, len(pins))
	for _, p := range pins {
		v, err := s.store.GetVersionByCode(ctx, p.Code, p.Version)
		if errors.Is(err, repo.ErrNotFound) {
			return nil, unavailable(p.Kind, p.Code, "", ReasonNotFound)
		}
		if err != nil {
			return nil, httpx.Wrap(errcode.Internal, err)
		}
		var snap domain.Snapshot
		if err := json.Unmarshal(v.Snapshot, &snap); err != nil {
			return nil, httpx.Wrap(errcode.Internal, fmt.Errorf("解析营销选项 %s v%d 的版本原文失败：%w", p.Code, p.Version, err))
		}
		if snap.Kind != p.Kind {
			return nil, unavailable(p.Kind, p.Code, "", ReasonNotFound)
		}
		// 以钉住的身份为准（历史行里的 version 与查询条件必然一致，code 也是）。
		snap.Pick = domain.Pick{Kind: p.Kind, Code: p.Code, Version: v.Version}
		out = append(out, snap)
	}
	return out, nil
}

// unavailable 构造 42215。message 是给人看的一句话；前端按 details 分类，**不匹配文案**。
func unavailable(kind domain.Kind, code, name, reason string) error {
	label := name
	if label == "" {
		label = code
	}
	var msg string
	switch reason {
	case ReasonDisabled:
		msg = fmt.Sprintf("%s「%s」已下线，请换一个", kind.Label(), label)
	case ReasonNotStarted:
		msg = fmt.Sprintf("%s「%s」还没到开始时间", kind.Label(), label)
	case ReasonEnded:
		msg = fmt.Sprintf("%s「%s」的活动已结束，请换一个", kind.Label(), label)
	case ReasonOutputMismatch:
		msg = fmt.Sprintf("%s「%s」不适用当前产出，请换一个技能或选项", kind.Label(), label)
	default:
		msg = fmt.Sprintf("%s「%s」不存在", kind.Label(), label)
	}
	return httpx.Errf(errcode.MarketingOptionUnavailable, msg).WithDetails(map[string]any{
		"kind": string(kind), "code": code, "reason": reason,
		"userFixable": reason == ReasonOutputMismatch,
	})
}
