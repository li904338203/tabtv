package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// timeline（MCPT-16，MCP-F-35）：按分镜装配 → 渲染成片。默认关闭的 timeline 工具集。
//
// ⚠️ assemble **默认空跑**（dryRun 缺省为 true）：只给计划与缺口、不写库，连时间线都不建；
// 显式 dryRun=false 才装配（0 积分，但会改用户的时间线 → 写操作，要 edit）。只补缺口，不动已有片段。
// ⚠️ render：先预检（timeline 域的 PrecheckRender，免费），有问题直接返回问题清单（指出是第几段）、不进确认；
// 没问题按预检的报价走确认服务，受理（扣费）在 timeline 域自己的事务里。
// ⚠️ 渲染要 export 动作（把内容取出去），不是 generate。

type timelineIn struct {
	ProjectID   string `json:"projectId,omitempty" jsonschema:"assemble：项目 ID（只给 scriptId 时可省，由脚本推出）"`
	ScriptID    string `json:"scriptId,omitempty" jsonschema:"assemble：按该脚本的分镜装配（取它最新的时间线，没有时 dryRun 为 false 会新建一条）"`
	TimelineID  string `json:"timelineId,omitempty" jsonschema:"assemble / render / get：时间线 ID"`
	Name        string `json:"name,omitempty" jsonschema:"assemble：新建时间线的名称"`
	AspectRatio string `json:"aspectRatio,omitempty" jsonschema:"assemble：新建时的画幅，如 16:9 / 9:16"`
	Force       bool   `json:"force,omitempty" jsonschema:"assemble：重建自动装配出来的片段（手工加的不动）"`
	DryRun      *bool  `json:"dryRun,omitempty" jsonschema:"assemble：默认 true（只看计划、不写库）；显式 false 才装配"`
	RenderID    string `json:"renderId,omitempty" jsonschema:"get：查某一次渲染（不给：该时间线最近一次）"`
	// 确认（render）
	ConfirmQuoteID string `json:"confirmQuoteId,omitempty" jsonschema:"render 的确认凭证：上一次调用返回的 quoteId"`
	ConfirmCredits *int   `json:"confirmCredits,omitempty" jsonschema:"回显报价金额，必须等于该 quoteId 的金额；与 confirmQuoteId 一起给"`
}

type assembleGapOut struct {
	ShotNo int    `json:"shotNo"`
	Reason string `json:"reason"`
}

type timelineViewOut struct {
	ID          string        `json:"id"`
	ScriptID    string        `json:"scriptId,omitempty"`
	Version     int           `json:"version"`
	AspectRatio string        `json:"aspectRatio,omitempty"`
	ClipCount   int           `json:"clipCount"`
	TotalMs     int           `json:"totalMs"`
	EffectiveMs int           `json:"effectiveMs" jsonschema:"算上转场重叠后的实际时长"`
	Created     bool          `json:"created,omitempty"`
	Untrusted   nameUntrusted `json:"untrusted"`
}

type renderIssueOut struct {
	Code    string `json:"code"`
	Detail  string `json:"detail"`
	ClipIdx int    `json:"clipIdx,omitempty" jsonschema:"第几段（从 1 起；0：与片段无关）"`
}

type renderOut struct {
	ID            string `json:"id"`
	TimelineID    string `json:"timelineId,omitempty"`
	Status        string `json:"status"`
	Progress      int    `json:"progress"`
	Stage         string `json:"stage,omitempty"`
	Credits       int    `json:"credits"`
	OutputMediaID string `json:"outputMediaId,omitempty" jsonschema:"成片的媒体 ID（成功后；用 media.get 或资源 seeaitv://media/{id} 取）"`
	Error         string `json:"error,omitempty"`
}

type timelineOut struct {
	Status     string           `json:"status" jsonschema:"assemble：planned（空跑）/ assembled；render：queued / not_renderable（预检没通过，未扣费）/ needs_confirmation / declined；get：ok"`
	TimelineID string           `json:"timelineId,omitempty"`
	DryRun     *bool            `json:"dryRun,omitempty"`
	Summary    string           `json:"summary,omitempty" jsonschema:"assemble：装配结论（有缺口时缺口在第一句）"`
	Append     int              `json:"append,omitempty" jsonschema:"assemble：要（已）加进时间线的镜头数"`
	Skipped    []int            `json:"skippedShots,omitempty" jsonschema:"assemble：已在时间线上、这次不动的镜号"`
	Gaps       []assembleGapOut `json:"gaps,omitempty" jsonschema:"assemble：缺素材 / 失败的镜头（逐条镜号 + 原因）"`
	Timeline   *timelineViewOut `json:"timeline,omitempty"`
	Issues     []renderIssueOut `json:"issues,omitempty" jsonschema:"render：预检问题"`
	DurationMs int              `json:"durationMs,omitempty"`
	Render     *renderOut       `json:"render,omitempty"`
	RenderID   string           `json:"renderId,omitempty"`
	Credits    int              `json:"credits" jsonschema:"render：本次确认的积分（重放时为原确认的积分，本次未再扣）"`
	Replayed   bool             `json:"replayed"`
	// NeedsConfirmation render 的基线确认。
	NeedsConfirmation *needsConfirmationOut `json:"needsConfirmation,omitempty"`
	Message           string                `json:"message"`

	elicit *app.ElicitRequest
	links  []app.ResourceLink
}

func (o timelineOut) ElicitRequest() *app.ElicitRequest { return o.elicit }
func (o timelineOut) ResourceLinks() []app.ResourceLink { return o.links }

const timelineDescription = "把视频组的成片装配到时间线并渲染成片。" +
	"assemble：按分镜装配（0 积分）。**默认 dryRun=true**：只给计划与缺口（缺素材 / 失败的镜头逐条给镜号与原因），不写库、不建时间线；" +
	"确认后显式传 dryRun=false 才装配（只补缺口，不动已有片段；该脚本还没有时间线时新建一条）。" +
	"render：渲染成片（付费，按秒计费）：先免费预检，有问题时不扣费并逐条指出是第几段；没问题按预检的报价确认后受理。" +
	"get：时间线摘要与最近一次（或指定 renderId 的）渲染。" + paidDescTail

func timelineTool(d Deps) app.Tool {
	return app.Define(app.Tool{
		Name:        "timeline",
		Toolset:     domain.ToolsetTimeline,
		Description: timelineDescription,
		Methods: []app.Method{
			{Name: "assemble", Action: collab.ActionEdit, Write: true},
			{Name: "render", Action: collab.ActionExport, Paid: true, Write: true},
			{Name: "get", Action: collab.ActionView},
		},
	}, func(ctx context.Context, method string, in timelineIn) (timelineOut, string, error) {
		p, err := principal(ctx)
		if err != nil {
			return timelineOut{}, "", err
		}
		if d.Timelines == nil {
			return timelineOut{}, "", unwired("Timelines")
		}
		switch method {
		case "assemble":
			return timelineAssemble(ctx, d, p, in)
		case "render":
			return timelineRender(ctx, d, p, in)
		case "get":
			return timelineGet(ctx, d, p, in)
		}
		return timelineOut{}, "", httpx.Errf(errcode.InvalidArgument, "未知 method："+method)
	})
}

// AssembleDryRun 是 assemble 的空跑判定：**不传 dryRun 就是空跑**（只有显式 false 才写）。
func assembleDryRun(in timelineIn) bool { return in.DryRun == nil || *in.DryRun }

func timelineAssemble(ctx context.Context, d Deps, p app.Principal, in timelineIn) (timelineOut, string, error) {
	ref := app.TimelineRef{Name: strings.TrimSpace(in.Name), AspectRatio: strings.TrimSpace(in.AspectRatio)}
	var err error
	if in.TimelineID != "" {
		if ref.TimelineID, err = parseID("timelineId", in.TimelineID); err != nil {
			return timelineOut{}, "", err
		}
	}
	if in.ScriptID != "" {
		if ref.ScriptID, err = parseID("scriptId", in.ScriptID); err != nil {
			return timelineOut{}, "", err
		}
	}
	if in.ProjectID != "" {
		if ref.ProjectID, err = parseID("projectId", in.ProjectID); err != nil {
			return timelineOut{}, "", err
		}
	}
	if ref.TimelineID == 0 && ref.ScriptID == 0 {
		return timelineOut{}, "", httpx.Errf(errcode.InvalidArgument, "scriptId 与 timelineId 至少给一个")
	}
	if len([]rune(ref.Name)) > maxTitleRunes {
		return timelineOut{}, "", httpx.Errf(errcode.InvalidArgument, fmt.Sprintf("name 最多 %d 字", maxTitleRunes))
	}
	dry := assembleDryRun(in)
	res, err := d.Timelines.Assemble(ctx, p, ref, in.Force, dry)
	if err != nil {
		return timelineOut{}, "", err
	}
	out := timelineOut{Status: "assembled", DryRun: &dry, Summary: res.Summary, Append: res.Append, Skipped: res.Skipped}
	for _, g := range res.Gaps {
		out.Gaps = append(out.Gaps, assembleGapOut{ShotNo: g.ShotNo, Reason: g.Reason})
	}
	if res.Timeline != nil {
		out.TimelineID = idStr(res.Timeline.ID)
		out.Timeline = timelineViewOutOf(*res.Timeline)
	}
	gaps := ""
	if len(out.Gaps) > 0 {
		gaps = gapText(out.Gaps) + "。"
	}
	if dry {
		out.Status = "planned"
		out.Message = gaps + "这是空跑：没有写库" + map[bool]string{true: "", false: "，该脚本还没有时间线（dryRun=false 时会新建一条）"}[res.Timeline != nil] +
			"。确认接受后传 dryRun=false 装配；有缺口时可先把那几镜补出来（script.group_execute 重试失败项）。"
	} else {
		out.Message = gaps + "已装配（0 积分）。下一步用 timeline.render 渲染成片（付费）。"
	}
	return out, res.Summary, nil
}

func gapText(gaps []assembleGapOut) string {
	parts := make([]string, 0, len(gaps))
	for _, g := range gaps {
		parts = append(parts, fmt.Sprintf("第 %d 镜：%s", g.ShotNo, g.Reason))
	}
	return fmt.Sprintf("有 %d 个镜头缺素材（%s）", len(gaps), strings.Join(parts, "；"))
}

func timelineRender(ctx context.Context, d Deps, p app.Principal, in timelineIn) (timelineOut, string, error) {
	tid, err := parseID("timelineId", in.TimelineID)
	if err != nil {
		return timelineOut{}, "", err
	}
	var first *app.RenderPrecheck
	if in.ConfirmQuoteID == "" && in.ConfirmCredits == nil && app.ElicitationFrom(ctx).Answer == nil {
		pre, err := d.Timelines.PrecheckRender(ctx, p, tid)
		if err != nil {
			return timelineOut{}, "", err
		}
		if len(pre.Issues) > 0 {
			return timelineOut{Status: "not_renderable", TimelineID: idStr(tid), Issues: issuesOut(pre.Issues),
					DurationMs: pre.DurationMs, Message: "预检没通过，没有扣费、没有报价。按 issues 逐条修正后再渲染。"},
				fmt.Sprintf("预检没通过：%d 个问题", len(pre.Issues)), nil
		}
		first = &pre
	}
	var pre app.RenderPrecheck
	fp := paidFingerprint("timeline.render", map[string]any{"timelineId": idStr(tid)})
	g, err := gatePaid(ctx, d, p, paidAsk{Tool: "timeline.render", Fingerprint: fp,
		ConfirmQuoteID: in.ConfirmQuoteID, ConfirmCredits: in.ConfirmCredits,
		Price: func(ctx context.Context) (app.Priced, error) {
			if first != nil {
				pre, first = *first, nil
			} else {
				x, err := d.Timelines.PrecheckRender(ctx, p, tid)
				if err != nil {
					return app.Priced{}, err
				}
				pre = x
			}
			if len(pre.Issues) > 0 {
				return app.Priced{}, httpx.Errf(errcode.InvalidArgument, "渲染预检没通过（"+pre.Issues[0].Detail+
					"），没有扣费：用 timeline.render 不带确认重新调用查看全部问题").
					WithDetails(map[string]any{"reason": "precheck_failed", "issues": issuesOut(pre.Issues)})
			}
			return app.Priced{Credits: pre.Credits, Detail: map[string]any{"timelineId": idStr(tid),
				"durationMs": pre.DurationMs}, Summary: fmt.Sprintf("渲染时间线 %d 的成片（约 %d 秒）", tid,
				(pre.DurationMs+999)/1000)}, nil
		}})
	if err != nil {
		return timelineOut{}, "", err
	}
	base := timelineOut{TimelineID: idStr(tid)}
	switch g.Kind {
	case gateConfirm:
		if g.Pending.Elicit != nil {
			return timelineOut{elicit: g.Pending.Elicit}, "", nil
		}
		nc, msg, summary := pendingOut(g.Pending, "timeline.render")
		base.Status, base.NeedsConfirmation, base.Message, base.DurationMs = "needs_confirmation", nc, msg, pre.DurationMs
		return base, summary, nil
	case gateDeclined:
		base.Status, base.Message = "declined", declinedMessage
		return base, "用户已拒绝，未扣费", nil
	case gateInProgress:
		base.Status, base.Credits, base.Replayed, base.Message = "running", g.Verified.Credits, true, inProgressMessage
		return base, "这个确认已受理，仍在处理中", nil
	case gateReplay:
		if err := replayedFailure(g.Result); err != nil {
			return timelineOut{}, "", err
		}
		var ref struct {
			RenderID string `json:"renderId"`
		}
		_ = json.Unmarshal(g.Result, &ref)
		rid, err := parseID("renderId", ref.RenderID)
		if err != nil {
			return timelineOut{}, "", httpx.Wrap(errcode.Internal, err)
		}
		r, err := d.Timelines.Render(ctx, p, rid)
		if err != nil {
			return timelineOut{}, "", err
		}
		return timelineOut{Status: r.Status, TimelineID: idStr(tid), RenderID: idStr(r.ID), Render: renderOutOf(r),
			Credits: g.Verified.Credits, Replayed: true, Message: paidReplayMessage + "用 tasks（kind=render）查看进度。",
			links: renderLinks(r)}, "这个确认已受理过，本次没有扣费", nil
	}
	key := g.Verified.IdemKey
	if key == "" {
		key = domain.ObjectBatchID(p.GrantID, "", randomSuffix())
	}
	r, err := d.Timelines.StartRender(ctx, p, tid, key)
	if err != nil {
		recordPaid(ctx, d, p, g, failureOf(err))
		return timelineOut{}, "", err
	}
	recordPaid(ctx, d, p, g, map[string]string{"renderId": idStr(r.ID)})
	status := r.Status
	if status == "" {
		status = "queued"
	}
	out := timelineOut{Status: status, TimelineID: idStr(tid), RenderID: idStr(r.ID), Render: renderOutOf(r),
		Credits: g.Verified.Credits, DurationMs: pre.DurationMs,
		Message: "渲染已受理（后台任务，独立队列）。宿主超时不代表失败，不要重新提交；用 tasks（kind=render）或 timeline.get 查看进度。"}
	return out, fmt.Sprintf("已提交渲染 %s（%d 积分）", out.RenderID, g.Verified.Credits), nil
}

func timelineGet(ctx context.Context, d Deps, p app.Principal, in timelineIn) (timelineOut, string, error) {
	tid, err := parseID("timelineId", in.TimelineID)
	if err != nil {
		return timelineOut{}, "", err
	}
	tv, latest, err := d.Timelines.Get(ctx, p, tid)
	if err != nil {
		return timelineOut{}, "", err
	}
	if in.RenderID != "" {
		rid, err := parseID("renderId", in.RenderID)
		if err != nil {
			return timelineOut{}, "", err
		}
		r, err := d.Timelines.Render(ctx, p, rid)
		if err != nil {
			return timelineOut{}, "", err
		}
		if r.TimelineID != 0 && r.TimelineID != tid {
			return timelineOut{}, "", httpx.Errf(errcode.InvalidArgument, "这次渲染不属于该时间线")
		}
		latest = &r
	}
	out := timelineOut{Status: "ok", TimelineID: idStr(tv.ID), Timeline: timelineViewOutOf(tv),
		Message: "时间线摘要与渲染状态。"}
	if latest != nil {
		out.RenderID = idStr(latest.ID)
		out.Render = renderOutOf(*latest)
		out.links = renderLinks(*latest)
	}
	return out, domain.WithUntrustedNotice("时间线 " + out.TimelineID), nil
}

func timelineViewOutOf(v app.TimelineView) *timelineViewOut {
	return &timelineViewOut{ID: idStr(v.ID), ScriptID: idStr(v.ScriptID), Version: v.Version, AspectRatio: v.AspectRatio,
		ClipCount: v.ClipCount, TotalMs: v.TotalMs, EffectiveMs: v.EffectiveMs, Created: v.Created,
		Untrusted: nameUntrusted{Name: domain.CleanText(v.Name, maxTitleRunes)}}
}

func renderOutOf(r app.RenderView) *renderOut {
	return &renderOut{ID: idStr(r.ID), TimelineID: idStr(r.TimelineID), Status: r.Status, Progress: r.Progress,
		Stage: r.Stage, Credits: r.Credits, OutputMediaID: idStr(r.OutputMediaID), Error: domain.CleanText(r.Error, maxErrorRunes)}
}

func renderLinks(r app.RenderView) []app.ResourceLink {
	if r.OutputMediaID == 0 {
		return nil
	}
	return []app.ResourceLink{mediaLink(r.OutputMediaID, "成片")}
}

func issuesOut(in []app.RenderIssue) []renderIssueOut {
	out := make([]renderIssueOut, 0, len(in))
	for _, is := range in {
		out = append(out, renderIssueOut{Code: is.Code, Detail: is.Detail, ClipIdx: is.ClipIdx})
	}
	return out
}
