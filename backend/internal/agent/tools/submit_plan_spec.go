package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
)

// 规划视频的两条硬约束（design/01 修订块 R-1；会话 107419662995685376，2026-10-24 用户要求，营销与通用会话都适用）：
//
//  1. 画幅 / 清晰度 / 时长里用户选了「智能」的项，规划之前必须用 ask_user 跟他确认（按固定题目 id 认，
//     见 domain.SpecKeys）。那次 Agent 问了卖点和人群，规格一项没问，自己定了 9:16 · 15 秒 · 720p。
//  2. 一支视频一次生成，不拆段：计划里有两条及以上视频时，每一条都必须是完整的选定（或确认）时长。那次规划
//     写着「15 秒」，提交的却是 5 秒 + 10 秒两段（「当前视频模型」一节按目录里默认的「不支持多镜头」要求拆段，已改）。
//     只有一条视频时不核对时长（对话里「改成 10 秒」是正常的改动，而且还要过规划确认卡）。
//
// 通用会话唯一的例外是脚本线做长片（有成片渲染项、且不止一个视频条目）：视频条目是长片的分段，改为核对
// 成片时长，并用报价确认「一次生成确实放不下这么长」—— 放得下就退回，让它改成一个视频条目。
// 营销会话没有这个例外。素材裂变的条目不核对时长（它跟着源视频走）。
// ⚠️ MCP 建的会话没有输入框偏好，规格都是「智能」：同样要先问（MCP 客户端用 agent.submit_card 回答问答卡），
// 或者建会话时在 prefs.video 里带上 ratio / resolution / duration。

// checkSpecConfirmed 有视频条目、而规格里还有没确认的项时退回（不发卡、不报价）。
func checkSpecConfirmed(spec app.VideoSpecState, items []json.RawMessage) error {
	if len(spec.Missing) == 0 {
		return nil
	}
	hasVideo := false
	for _, raw := range items {
		var it struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &it) == nil && strings.TrimSpace(it.Type) == "video" {
			hasVideo = true
			break
		}
	}
	if !hasVideo {
		return nil
	}
	labels := make([]string, 0, len(spec.Missing))
	for _, k := range spec.Missing {
		labels = append(labels, domain.SpecLabel(k))
	}
	return &domain.ToolRejection{Code: domain.ToolErrSpecUnconfirmed,
		Message: "未提交规划（没有发卡、没有扣费）：" + strings.Join(labels, "、") +
			"还没有定（输入框偏好里是「智能」，或会话没有设置），规划视频之前要先跟用户确认。请用 ask_user（kind=questions）问清这几项，题目 id 分别用 " +
			strings.Join(spec.Missing, " / ") + "，单选，选项列所选视频模型支持的值、推荐值放第一个；" +
			"用户回答后按他选的值重新规划、再提交。",
		Details: map[string]any{"missing": spec.Missing}}
}

// specDuration 核对第 n 项视频的时长（用户选定或确认了时长时）：
//
//   - 没写时长：补成选定的时长；
//   - 计划里有**两条及以上**视频（multi）：每一条都必须是完整的选定时长（批量变体、几支独立的视频都是），
//     有一条短于它就是在拆段 → 退回；
//   - 只有一条视频：不核对。用户在对话里说「改成 10 秒」时那一条就是 10 秒，而它还要过规划确认卡 ——
//     这里拦下的话，正常的改时长也要再回答一张问答卡。
//
// 时长不知道、不是视频、或是素材裂变条目（跟着源视频走）时原样返回。
func specDuration(spec app.VideoSpecState, n int, raw json.RawMessage, it domain.PlanItem, multi bool) (domain.PlanItem, error) {
	d := spec.Duration
	if d <= 0 || it.Kind != "video" || it.FissionRoute != "" {
		return it, nil
	}
	var s struct {
		DurationSec *int `json:"durationSec"`
	}
	_ = json.Unmarshal(raw, &s)
	if s.DurationSec == nil || *s.DurationSec <= 0 {
		it.Quantity, it.Unit = d, "second"
		return it, nil
	}
	if multi && *s.DurationSec != d {
		return it, fmt.Errorf("第 %d 项：时长 %d 秒与用户选定的 %d 秒不一致。一支视频一次生成，不要拆成几段分别生成；"+
			"每个视频条目都是一条完整的 %d 秒视频（批量变体也是），多个镜头写进同一段提示词", n, *s.DurationSec, d, d)
	}
	return it, nil
}

// planShape 数计划里的视频条目、看有没有成片渲染项（按 type，报价之前就要用）。
func planShape(items []json.RawMessage) (videos int, render bool) {
	for _, raw := range items {
		var it struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &it) != nil {
			continue
		}
		switch strings.TrimSpace(it.Type) {
		case "render":
			render = true
		case "video":
			videos++
		}
	}
	return videos, render
}

// planIsLongFilm 计划里有成片渲染项、且不止一个视频条目：脚本线做长片（分镜 → 视频组 → 时间线 → 渲染）。
func planIsLongFilm(items []json.RawMessage) bool {
	videos, render := planShape(items)
	return render && videos > 1
}

// checkLongFilm 核对脚本线长片：成片（渲染）时长必须等于用户选定的时长；而且只有一次生成放不下这么长时才允许拆段。
//
// 「放不下」用报价判（与受理同一个口径，不另写一份时长表）：拿第一个视频条目的模型与生成方式按成片时长报一次价 ——
// 报得出价 = 一次就能生成 → 退回；因时长越界报不出（PlanFaultQuantity）= 放不下 → 放行；
// 其它原因报不出价时不据此拦（分段各自的报价会说明原因）。时长不知道时不核对。
func (t SubmitPlan) checkLongFilm(ctx context.Context, rc RunContext, items []domain.PlanItem) error {
	d := rc.Spec.Duration
	if d <= 0 {
		return nil
	}
	var first *domain.PlanItem
	segments := 0
	for i := range items {
		switch items[i].Kind {
		case "render":
			if q := items[i].Quantity; q != d {
				return fmt.Errorf("第 %d 项：成片时长 %d 秒与用户选定的 %d 秒不一致，按选定的时长规划", i+1, q, d)
			}
		case "video":
			segments++
			if first == nil {
				first = &items[i]
			}
		}
	}
	if first == nil {
		return nil
	}
	probe := domain.PlanItem{Kind: "video", ModelCode: first.ModelCode, OperationCode: first.OperationCode,
		Inputs: first.Inputs, Unit: "second", Quantity: d, Label: "整片一次生成"}
	p, err := t.d.Pricer.PriceItem(ctx, probe, rc.MemberLevelID)
	if err != nil || !p.Available {
		return nil
	}
	return fmt.Errorf("规划未提交（没有发卡、没有扣费）：成片 %d 秒用 %s 一次就能生成（单条报价 ✦%d），一支视频不要拆成 %d 段走脚本线。"+
		"改成一个 %d 秒的视频条目，多个镜头写进同一段提示词", d, first.ModelCode, p.Item.Credits, segments, d)
}
