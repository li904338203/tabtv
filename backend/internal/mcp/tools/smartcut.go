package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/li904338203/tabtv/backend/internal/canvasgen"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 智能剪辑（docs/agent/smartcut SCT-41，SC-F-160~165）：smartcut_create 建节点（可同时连上素材、连上即铺上时间线）、
// smartcut_get 只读详情、smartcut_plan 免费的 AI 剪辑（结果直接写入并存快照，来源 mcp）、smartcut_export 导出成片。
//
// ⚠️ 四个都放在**默认开启**的画布工具集（与导演台同处）：导出单开 smartcut_export，因为 timeline.render 所在的
// timeline 工具集默认关闭（SC-F-163）。smartcut_get 只读，只读凭据看得到；其余写画布 / 时间线，只读凭据看不到
// （domain.Visible 自动挡）。
// ⚠️ 导出免费（SCK-3）：报价为 0 时直接执行、不进确认；运营在后台改了价时照常走确认（以报价金额为准，不按工具名硬编码）。
// ⚠️ AI 剪辑同步调模型（几分钟）：脱离单次调用的截止（runDetached），做不完先回 running，结果用 smartcut_get 查。

// smartcutModeNames 剪辑方式（与网页同一组）。
var smartcutModeNames = map[string]string{"mix": "素材混剪", "board": "分镜成片", "talk": "口播精剪", "narr": "解说成片"}

// maxSmartcutInstructionRunes 与 smartcut/app.MaxInstructionChars 一致（那边也会拦，这里先给一句看得懂的话）。
const maxSmartcutInstructionRunes = 2000

// ---------------------------------------------------------------- 出参

type smartcutRenderOut struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	Progress      int    `json:"progress"`
	Stage         string `json:"stage,omitempty"`
	OutputMediaID string `json:"outputMediaId,omitempty" jsonschema:"成片的媒体 ID（成功后；用 media.get 或资源 seeaitv://media/{id} 取）"`
	Error         string `json:"error,omitempty"`
}

type smartcutSummaryOut struct {
	TimelineID   string             `json:"timelineId,omitempty"`
	Version      int                `json:"version"`
	IsArranged   bool               `json:"isArranged" jsonschema:"剪过（AI 或用户）：剪过之后新连进来的素材只进素材条，不再自动铺上时间线"`
	ClipCount    int                `json:"clipCount" jsonschema:"画面轨的段数"`
	TotalMs      int                `json:"totalMs"`
	EffectiveMs  int                `json:"effectiveMs" jsonschema:"成片时长（扣掉转场重叠）"`
	InputsStale  bool               `json:"inputsStale" jsonschema:"素材有变化：连进来的素材与上次剪辑依据的不同"`
	RenderStale  bool               `json:"renderStale" jsonschema:"剪辑已改、成片还是旧的"`
	LatestRender *smartcutRenderOut `json:"latestRender,omitempty" jsonschema:"最近一次导出"`
	ActiveCall   string             `json:"activeCall,omitempty" jsonschema:"进行中的 AI 调用（plan / edit / translate）"`
}

type smartcutInputUntrusted struct {
	Title string `json:"title,omitempty"`
}

type smartcutInputOut struct {
	No           string                 `json:"no" jsonschema:"素材编号（按连线顺序；多产物的节点是 1-1、1-2……）"`
	Kind         string                 `json:"kind"`
	Status       string                 `json:"status" jsonschema:"ready / pending / running / failed / invalid"`
	SourceNodeID string                 `json:"sourceNodeId"`
	DurationMs   int                    `json:"durationMs,omitempty"`
	Used         bool                   `json:"used" jsonschema:"已经用在时间线上"`
	Reason       string                 `json:"reason,omitempty"`
	Untrusted    smartcutInputUntrusted `json:"untrusted"`
}

type smartcutClipOut struct {
	Track        string `json:"track" jsonschema:"video / audio"`
	Role         string `json:"role,omitempty" jsonschema:"音频轨的用途：music / voice / sfx"`
	No           int    `json:"no" jsonschema:"这条轨上的第几段（从 1 起）"`
	MediaID      string `json:"mediaId"`
	SourceNodeID string `json:"sourceNodeId,omitempty"`
	Origin       string `json:"origin,omitempty" jsonschema:"ai / user / sync"`
	StartMs      int    `json:"startMs" jsonschema:"成片起点"`
	DurationMs   int    `json:"durationMs"`
	Hidden       bool   `json:"hidden,omitempty" jsonschema:"跟随画面的音效被裁到看不见了（不进成片）"`
}

func smartcutSummaryOutOf(s app.SmartcutSummary) smartcutSummaryOut {
	out := smartcutSummaryOut{TimelineID: idStr(s.TimelineID), Version: s.Version, IsArranged: s.IsArranged,
		ClipCount: s.ClipCount, TotalMs: s.TotalMs, EffectiveMs: s.EffectiveMs, InputsStale: s.InputsStale,
		RenderStale: s.RenderStale, ActiveCall: s.ActiveCallKind}
	if r := s.LatestRender; r != nil {
		out.LatestRender = &smartcutRenderOut{ID: idStr(r.ID), Status: r.Status, Progress: r.Progress, Stage: r.Stage,
			OutputMediaID: idStr(r.OutputMediaID), Error: domain.CleanText(r.Error, maxErrorRunes)}
	}
	return out
}

// ---------------------------------------------------------------- smartcut_create

type smartcutCreateIn struct {
	ProjectID      string         `json:"projectId" jsonschema:"项目 ID"`
	Title          string         `json:"title,omitempty" jsonschema:"节点标题（默认「智能剪辑」）"`
	Position       *canvasPointIn `json:"position,omitempty" jsonschema:"节点位置；不给则由服务端排在空位"`
	InputNodeIDs   []string       `json:"inputNodeIds,omitempty" jsonschema:"要连进来的素材节点 ID（视频 / 图片 / 音频 / 文本 / 脚本 / 视频组），按播放顺序"`
	IdempotencyKey string         `json:"idempotencyKey,omitempty" jsonschema:"幂等键：重试时带同一个值只会建出一个智能剪辑节点"`
}

type smartcutCreateOut struct {
	ProjectID        string             `json:"projectId"`
	NodeID           string             `json:"nodeId" jsonschema:"新建的智能剪辑节点"`
	TimelineID       string             `json:"timelineId" jsonschema:"节点的时间线（已写回节点的 params.timelineId）"`
	Connected        []string           `json:"connected" jsonschema:"这次连进来的素材节点"`
	AlreadyConnected []string           `json:"alreadyConnected" jsonschema:"本来就连着的（重试时）"`
	Appended         int                `json:"appended" jsonschema:"按连线即入轨铺上时间线的段数"`
	SyncError        string             `json:"syncError,omitempty" jsonschema:"素材已连上、但铺上时间线失败时的原因（用户打开剪辑台时会再铺一次）"`
	Summary          smartcutSummaryOut `json:"summary"`
	Message          string             `json:"message"`
	links            []app.ResourceLink
}

func (o smartcutCreateOut) ResourceLinks() []app.ResourceLink { return o.links }

const smartcutCreateDescription = "在项目画布上新建一个智能剪辑节点并建好它的时间线（不花积分）。可以同时给 inputNodeIds" +
	"（按播放顺序列出视频 / 图片 / 音频 / 文本 / 脚本 / 视频组节点），它们会按顺序连进来，并按「连线即入轨」铺上时间线。" +
	"返回 nodeId 与 timelineId；之后用 smartcut_plan 让 AI 按要求剪，用 smartcut_get 查看，用 smartcut_export 导出成片（免费）。" +
	"重试时带同一个 idempotencyKey，避免建出两个节点。"

// smartcutCreateMessage 建好之后的下一步提示：AI 剪辑关着（SmartcutAIDisabled）时不提 smartcut_plan。
func smartcutCreateDescriptionFor(d Deps) string {
	if d.SmartcutAIDisabled {
		return strings.Replace(smartcutCreateDescription, "之后用 smartcut_plan 让 AI 按要求剪，", "之后请用户在剪辑台里手动剪（AI 剪辑暂未开放），", 1)
	}
	return smartcutCreateDescription
}

func smartcutCreateMessage(d Deps) string {
	if d.SmartcutAIDisabled {
		return "智能剪辑已建好（0 积分）。AI 剪辑暂未开放：请用户在 seeAitv 里打开剪辑台手动剪；需要成片时用 smartcut_export（免费）。"
	}
	return "智能剪辑已建好（0 积分）。下一步用 smartcut_plan 让 AI 剪，或请用户在 seeAitv 里打开剪辑台；" +
		"需要成片时用 smartcut_export（免费）。"
}

func smartcutCreateTool(d Deps) app.Tool {
	return app.Define(app.Tool{
		Name:        "smartcut_create",
		Toolset:     domain.ToolsetCanvas,
		Description: smartcutCreateDescriptionFor(d),
		// 不破坏：只新建节点、时间线与连进新节点的连线。
		Methods: []app.Method{{Action: collab.ActionEdit, Write: true}},
	}, func(ctx context.Context, _ string, in smartcutCreateIn) (smartcutCreateOut, string, error) {
		p, err := principal(ctx)
		if err != nil {
			return smartcutCreateOut{}, "", err
		}
		pid, err := parseID("projectId", in.ProjectID)
		if err != nil {
			return smartcutCreateOut{}, "", err
		}
		switch {
		case utf8.RuneCountInString(in.Title) > maxTitleRunes:
			return smartcutCreateOut{}, "", httpx.Errf(errcode.InvalidArgument, fmt.Sprintf("title 最多 %d 字", maxTitleRunes))
		case len(in.IdempotencyKey) > 200:
			return smartcutCreateOut{}, "", httpx.Errf(errcode.InvalidArgument, "idempotencyKey 最多 200 字符")
		case len(in.InputNodeIDs) > canvasgen.MaxSmartcutInputs:
			return smartcutCreateOut{}, "", httpx.Errf(errcode.InvalidArgument,
				fmt.Sprintf("inputNodeIds 最多 %d 个", canvasgen.MaxSmartcutInputs))
		}
		if d.Smartcuts == nil {
			return smartcutCreateOut{}, "", unwired("Smartcuts")
		}
		var pos *canvasgen.Position
		if in.Position != nil {
			pos = &canvasgen.Position{X: in.Position.X, Y: in.Position.Y}
		}
		inputs := make([]string, 0, len(in.InputNodeIDs))
		for _, id := range in.InputNodeIDs {
			inputs = append(inputs, strings.TrimSpace(id))
		}
		batch := domain.ObjectBatchID(p.GrantID, strings.TrimSpace(in.IdempotencyKey), randomSuffix())
		res, err := d.Smartcuts.Create(ctx, p, app.SmartcutCreateInput{ProjectID: pid, Title: strings.TrimSpace(in.Title),
			Position: pos, InputNodeIDs: inputs, BatchID: batch})
		if err != nil {
			return smartcutCreateOut{}, "", objectError(err, "时间线")
		}
		out := smartcutCreateOut{ProjectID: idStr(pid), NodeID: res.NodeID, TimelineID: idStr(res.TimelineID),
			Connected: nonNil(res.Connected), AlreadyConnected: nonNil(res.Existing), Appended: res.Appended,
			SyncError: res.SyncError, Summary: smartcutSummaryOutOf(res.Summary),
			Message: smartcutCreateMessage(d),
			links:   []app.ResourceLink{canvasLink(pid)}}
		return out, fmt.Sprintf("已建智能剪辑节点 %s（时间线 %s），连进 %d 个素材、铺上 %d 段", res.NodeID, out.TimelineID,
			len(out.Connected), res.Appended), nil
	})
}

// ---------------------------------------------------------------- smartcut_get

type smartcutGetIn struct {
	ProjectID string `json:"projectId" jsonschema:"项目 ID"`
	NodeID    string `json:"nodeId" jsonschema:"智能剪辑节点 ID（smartcut_create 返回的 nodeId，或 canvas_read 里类型为 smartcut 的节点）"`
}

type smartcutGetOut struct {
	ProjectID   string             `json:"projectId"`
	NodeID      string             `json:"nodeId"`
	HasTimeline bool               `json:"hasTimeline" jsonschema:"false：节点还没有自己的时间线（新建后还没打开过、或复制出来的），smartcut_plan 时会先补上"`
	Summary     smartcutSummaryOut `json:"summary"`
	Inputs      []smartcutInputOut `json:"inputs" jsonschema:"素材条：连进来的素材，按编号"`
	Omitted     int                `json:"inputsOmitted,omitempty" jsonschema:"素材条超出上限没列出的条数"`
	Clips       []smartcutClipOut  `json:"clips" jsonschema:"片段清单"`
	Message     string             `json:"message"`
	links       []app.ResourceLink
}

func (o smartcutGetOut) ResourceLinks() []app.ResourceLink { return o.links }

const smartcutGetDescription = "查看智能剪辑节点的时间线（只读）：总时长、片段清单（哪条轨第几段、来源节点、成片起点与时长）、" +
	"素材条（连进来的素材是否就绪、是否已用上）、素材有没有变化、最近一次导出的状态。导出成片用 smartcut_export。"

func smartcutGetTool(d Deps) app.Tool {
	return app.Define(app.Tool{
		Name:        "smartcut_get",
		Toolset:     domain.ToolsetCanvas,
		Description: smartcutGetDescription,
		Methods:     []app.Method{{Action: collab.ActionView}},
	}, func(ctx context.Context, _ string, in smartcutGetIn) (smartcutGetOut, string, error) {
		p, err := principal(ctx)
		if err != nil {
			return smartcutGetOut{}, "", err
		}
		pid, err := parseID("projectId", in.ProjectID)
		if err != nil {
			return smartcutGetOut{}, "", err
		}
		nodeID := strings.TrimSpace(in.NodeID)
		if nodeID == "" {
			return smartcutGetOut{}, "", httpx.Errf(errcode.InvalidArgument, "nodeId 必填：要查看的智能剪辑节点")
		}
		if d.Smartcuts == nil {
			return smartcutGetOut{}, "", unwired("Smartcuts")
		}
		det, err := d.Smartcuts.Get(ctx, p, pid, nodeID)
		if err != nil {
			return smartcutGetOut{}, "", err
		}
		out := smartcutGetOut{ProjectID: idStr(pid), NodeID: nodeID, HasTimeline: det.HasTimeline,
			Summary: smartcutSummaryOutOf(det.SmartcutSummary), Omitted: det.Omitted,
			Inputs: make([]smartcutInputOut, 0, len(det.Inputs)), Clips: make([]smartcutClipOut, 0, len(det.Clips)),
			links: []app.ResourceLink{canvasLink(pid)}}
		if !det.HasTimeline {
			out.Message = "这个智能剪辑还没有自己的时间线：用户打开剪辑台时会自动补上。"
			if !d.SmartcutAIDisabled {
				out.Message = "这个智能剪辑还没有自己的时间线：调用 smartcut_plan 时会先补上，用户打开剪辑台时也会自动补上。"
			}
			return out, "智能剪辑 " + nodeID + " 还没有自己的时间线", nil
		}
		for _, it := range det.Inputs {
			out.Inputs = append(out.Inputs, smartcutInputOut{No: it.No, Kind: it.Kind, Status: it.Status,
				SourceNodeID: it.SourceNodeID, DurationMs: it.DurationMs, Used: it.Used,
				Reason:    domain.CleanText(it.Reason, maxErrorRunes),
				Untrusted: smartcutInputUntrusted{Title: domain.CleanText(it.Title, maxTitleRunes)}})
		}
		for _, c := range det.Clips {
			out.Clips = append(out.Clips, smartcutClipOut{Track: c.TrackKind, Role: c.TrackRole, No: c.No,
				MediaID: idStr(c.MediaID), SourceNodeID: c.SourceNodeID, Origin: c.Origin, StartMs: c.StartMs,
				DurationMs: c.DurationMs, Hidden: c.Hidden})
		}
		out.Message = "智能剪辑的时间线摘要、素材条与片段清单。"
		if r := det.LatestRender; r != nil && r.OutputMediaID != 0 {
			out.links = append(out.links, mediaLink(r.OutputMediaID, "成片"))
		}
		return out, domain.WithUntrustedNotice(fmt.Sprintf("智能剪辑 %s：%d 段，%d 毫秒", nodeID, det.ClipCount,
			det.EffectiveMs)), nil
	})
}

// ---------------------------------------------------------------- smartcut_plan

type smartcutPlanIn struct {
	ProjectID   string `json:"projectId" jsonschema:"项目 ID"`
	NodeID      string `json:"nodeId" jsonschema:"智能剪辑节点 ID"`
	Mode        string `json:"mode" jsonschema:"剪辑方式：mix 素材混剪 / board 分镜成片（连着脚本或视频组）/ talk 口播精剪（要有人声）/ narr 解说成片"`
	Instruction string `json:"instruction,omitempty" jsonschema:"剪辑要求（最多 2000 字），例如：剪一条 15 秒的新品卡点视频"`
	TargetSec   int    `json:"targetSec,omitempty" jsonschema:"目标时长（秒）；不给 = 按素材与要求自动"`
	AspectRatio string `json:"aspectRatio,omitempty" jsonschema:"画幅：16:9 / 9:16 / 1:1；不给 = 保持时间线当前的画幅"`
	Variants    int    `json:"variants,omitempty" jsonschema:"方案数 1~3（默认 1；多套时写入第一套成功的，其余可在剪辑台里切换）"`
}

type smartcutVariantOut struct {
	Index        int      `json:"index"`
	Status       string   `json:"status"`
	Summary      string   `json:"summary,omitempty"`
	TotalMs      int      `json:"totalMs"`
	Warnings     []string `json:"warnings,omitempty"`
	UsedFallback bool     `json:"usedFallback,omitempty" jsonschema:"模型的方案不合规，退回了按规则排的方案"`
	Error        string   `json:"error,omitempty"`
}

type smartcutPlanOut struct {
	Status         string               `json:"status" jsonschema:"completed：已写入；running：仍在剪（已在后台继续），用 smartcut_get 查看"`
	ProjectID      string               `json:"projectId"`
	NodeID         string               `json:"nodeId"`
	TimelineID     string               `json:"timelineId,omitempty"`
	CallID         string               `json:"callId,omitempty"`
	AppliedVariant int                  `json:"appliedVariant" jsonschema:"写入的是第几套（从 0 起；-1 = 没有写入）"`
	Variants       []smartcutVariantOut `json:"variants"`
	SnapshotID     string               `json:"snapshotId,omitempty" jsonschema:"写入前存的快照（用户能在剪辑台里撤销这轮 AI）"`
	Repair         string               `json:"repair,omitempty" jsonschema:"这次顺带给节点补上 / 分出了自己的时间线时的说明"`
	Summary        smartcutSummaryOut   `json:"summary"`
	Message        string               `json:"message"`
	links          []app.ResourceLink
}

func (o smartcutPlanOut) ResourceLinks() []app.ResourceLink { return o.links }

const smartcutPlanDescription = "让 AI 按要求剪辑智能剪辑节点里的素材（不花积分，但按用户限流：同时只能跑一个、每小时有次数上限，" +
	"与网页共用）。结果直接写入时间线，写入前存快照（用户能在剪辑台里撤销这轮 AI）；会覆盖当前剪辑。" +
	"与用户的编辑撞上时按同一套方案重做一次，仍冲突就报错、不覆盖用户的改动。节点还没有时间线时先补建。" +
	"被限流时返回 RATE_LIMITED / CONCURRENCY_LIMITED 与 retry_after_seconds，到时间之前不要重试；" +
	"素材不合适时返回 SMARTCUT_INPUT_UNSUITABLE 与原因、可选的出路。"

func smartcutPlanTool(d Deps) app.Tool {
	return app.Define(app.Tool{
		Name:        "smartcut_plan",
		Toolset:     domain.ToolsetCanvas,
		Description: smartcutPlanDescription,
		// 调外部模型但不花钱（SCK-2）；覆盖时间线前存快照，可撤销 —— 不标 Destructive。
		Methods: []app.Method{{Action: collab.ActionEdit, Write: true, OpenWorld: true}},
	}, func(ctx context.Context, _ string, in smartcutPlanIn) (smartcutPlanOut, string, error) {
		p, err := principal(ctx)
		if err != nil {
			return smartcutPlanOut{}, "", err
		}
		pid, err := parseID("projectId", in.ProjectID)
		if err != nil {
			return smartcutPlanOut{}, "", err
		}
		nodeID, mode := strings.TrimSpace(in.NodeID), strings.TrimSpace(in.Mode)
		instruction := strings.TrimSpace(in.Instruction)
		switch {
		case nodeID == "":
			return smartcutPlanOut{}, "", httpx.Errf(errcode.InvalidArgument, "nodeId 必填：要剪辑的智能剪辑节点")
		case smartcutModeNames[mode] == "":
			return smartcutPlanOut{}, "", httpx.Errf(errcode.InvalidArgument,
				"mode 只能是 mix（素材混剪）/ board（分镜成片）/ talk（口播精剪）/ narr（解说成片）").
				WithDetails(map[string]any{"path": "mode"})
		case utf8.RuneCountInString(instruction) > maxSmartcutInstructionRunes:
			return smartcutPlanOut{}, "", httpx.Errf(errcode.InvalidArgument,
				fmt.Sprintf("instruction 最多 %d 字", maxSmartcutInstructionRunes))
		}
		if d.Smartcuts == nil {
			return smartcutPlanOut{}, "", unwired("Smartcuts")
		}
		req := app.SmartcutPlanInput{ProjectID: pid, NodeID: nodeID, Mode: mode, Instruction: instruction,
			TargetSec: in.TargetSec, AspectRatio: strings.TrimSpace(in.AspectRatio), Variants: in.Variants,
			BatchID: domain.ObjectBatchID(p.GrantID, "", randomSuffix())}
		// 同步模型调用脱离单次调用的截止（paid.go runDetached）：做完了直接回结果，做不完先回 running。
		finish := func(_ app.SmartcutPlanResult, err error) {
			if err != nil {
				slog.Default().Warn("MCP 的 AI 剪辑在后台结束时失败", "grant", p.GrantID, "node", nodeID, "err", err)
			}
		}
		res, done, err := runDetached(ctx, func(bg context.Context) (app.SmartcutPlanResult, error) {
			return d.Smartcuts.Plan(bg, p, req)
		}, finish)
		if !done {
			return smartcutPlanOut{Status: "running", ProjectID: idStr(pid), NodeID: nodeID, AppliedVariant: -1,
					Variants: []smartcutVariantOut{}, Message: "AI 剪辑仍在进行（超过了单次调用时限，已在后台继续）。" +
						"不要重新发起；稍后用 smartcut_get 查看（activeCall 为空时片段清单就是新的结果）。",
					links: []app.ResourceLink{canvasLink(pid)}},
				"AI 剪辑仍在进行，稍后用 smartcut_get 查看", nil
		}
		if err != nil {
			return smartcutPlanOut{}, "", err
		}
		out := smartcutPlanOut{Status: "completed", ProjectID: idStr(pid), NodeID: nodeID,
			TimelineID: idStr(res.TimelineID), CallID: idStr(res.CallID), AppliedVariant: res.AppliedVariant,
			Variants: make([]smartcutVariantOut, 0, len(res.Variants)), SnapshotID: idStr(res.SnapshotID),
			Repair: res.Repair, Summary: smartcutSummaryOutOf(res.Summary),
			Message: "已剪好并写入时间线（0 积分）。把结果与警告转告用户；需要成片时用 smartcut_export（免费）。" +
				"不满意可以换个要求再调用一次，或请用户在剪辑台里「撤销这轮 AI」。",
			links: []app.ResourceLink{canvasLink(pid)}}
		for _, v := range res.Variants {
			out.Variants = append(out.Variants, smartcutVariantOut{Index: v.Index, Status: v.Status,
				Summary: domain.CleanText(v.Summary, maxParamsRunes), TotalMs: v.TotalMs, Warnings: v.Warnings,
				UsedFallback: v.UsedFallback, Error: domain.CleanText(v.Error, maxErrorRunes)})
		}
		return out, fmt.Sprintf("已剪好：%d 段，%d 毫秒（%s）", res.Summary.ClipCount, res.Summary.EffectiveMs,
			smartcutModeNames[mode]), nil
	})
}

// ---------------------------------------------------------------- smartcut_export

type smartcutExportIn struct {
	ProjectID      string `json:"projectId" jsonschema:"项目 ID"`
	NodeID         string `json:"nodeId" jsonschema:"智能剪辑节点 ID"`
	Resolution     string `json:"resolution,omitempty" jsonschema:"分辨率：720p / 1080p（不给 = 按时间线自身的尺寸）"`
	FPS            int    `json:"fps,omitempty" jsonschema:"帧率：24 / 25 / 30（不给 = 按时间线自身的帧率）"`
	IdempotencyKey string `json:"idempotencyKey,omitempty" jsonschema:"幂等键：重试时带同一个值只会建出一个导出任务"`
	// 确认（运营改过价时才会用到；导出默认免费，不需要确认）
	ConfirmQuoteID string `json:"confirmQuoteId,omitempty" jsonschema:"需要确认时的凭证：上一次调用返回的 quoteId"`
	ConfirmCredits *int   `json:"confirmCredits,omitempty" jsonschema:"回显报价金额，必须等于该 quoteId 的金额；与 confirmQuoteId 一起给"`
}

type smartcutExportOut struct {
	Status            string                `json:"status" jsonschema:"queued：已受理；not_renderable：预检没通过（未受理）；needs_confirmation：需用户确认费用；declined：用户拒绝；重放时为任务当前状态"`
	ProjectID         string                `json:"projectId"`
	NodeID            string                `json:"nodeId"`
	TimelineID        string                `json:"timelineId,omitempty"`
	RenderID          string                `json:"renderId,omitempty"`
	Render            *renderOut            `json:"render,omitempty"`
	Issues            []renderIssueOut      `json:"issues,omitempty" jsonschema:"预检问题（逐条指出是第几段）"`
	DurationMs        int                   `json:"durationMs,omitempty"`
	Credits           int                   `json:"credits" jsonschema:"本次确认的积分（导出默认免费，为 0）"`
	Replayed          bool                  `json:"replayed"`
	NeedsConfirmation *needsConfirmationOut `json:"needsConfirmation,omitempty"`
	Message           string                `json:"message"`

	elicit *app.ElicitRequest
	links  []app.ResourceLink
}

func (o smartcutExportOut) ElicitRequest() *app.ElicitRequest { return o.elicit }
func (o smartcutExportOut) ResourceLinks() []app.ResourceLink { return o.links }

const smartcutExportDescription = "把智能剪辑节点的时间线导出成成片（与网页的「导出」同一条路径）。先免费预检，有问题时不导出并逐条指出是第几段；" +
	"导出默认免费（0 积分不需要确认），按用户限流（同时只能导出一个、每小时有次数上限）。" +
	"提交后用 smartcut_get 或 tasks（kind=render）查看进度；宿主超时不代表失败，不要重复提交（重试时带同一个 idempotencyKey）。"

func smartcutExportTool(d Deps) app.Tool {
	return app.Define(app.Tool{
		Name:        "smartcut_export",
		Toolset:     domain.ToolsetCanvas,
		Description: smartcutExportDescription,
		// 把内容取出去：export 动作（同 timeline.render）；以报价金额判是否要确认（0 积分直接执行）。
		Methods: []app.Method{{Action: collab.ActionExport, Paid: true, Write: true}},
	}, func(ctx context.Context, _ string, in smartcutExportIn) (smartcutExportOut, string, error) {
		p, err := principal(ctx)
		if err != nil {
			return smartcutExportOut{}, "", err
		}
		pid, err := parseID("projectId", in.ProjectID)
		if err != nil {
			return smartcutExportOut{}, "", err
		}
		nodeID := strings.TrimSpace(in.NodeID)
		switch {
		case nodeID == "":
			return smartcutExportOut{}, "", httpx.Errf(errcode.InvalidArgument, "nodeId 必填：要导出的智能剪辑节点")
		case len(in.IdempotencyKey) > 200:
			return smartcutExportOut{}, "", httpx.Errf(errcode.InvalidArgument, "idempotencyKey 最多 200 字符")
		}
		if d.Smartcuts == nil {
			return smartcutExportOut{}, "", unwired("Smartcuts")
		}
		return smartcutExport(ctx, d, p, pid, nodeID, in)
	})
}

func smartcutExport(ctx context.Context, d Deps, p app.Principal, pid int64, nodeID string,
	in smartcutExportIn) (smartcutExportOut, string, error) {
	spec := app.SmartcutExportSpec{Resolution: strings.TrimSpace(in.Resolution), FPS: in.FPS}
	base := smartcutExportOut{ProjectID: idStr(pid), NodeID: nodeID, links: []app.ResourceLink{canvasLink(pid)}}
	var tid int64
	var pre app.RenderPrecheck
	precheck := func(ctx context.Context) error {
		id, x, err := d.Smartcuts.PrecheckExport(ctx, p, pid, nodeID, spec)
		if err != nil {
			return err
		}
		tid, pre = id, x
		return nil
	}
	if in.ConfirmQuoteID == "" && in.ConfirmCredits == nil && app.ElicitationFrom(ctx).Answer == nil {
		if err := precheck(ctx); err != nil {
			return smartcutExportOut{}, "", err
		}
		if len(pre.Issues) > 0 {
			base.Status, base.TimelineID, base.Issues, base.DurationMs = "not_renderable", idStr(tid), issuesOut(pre.Issues),
				pre.DurationMs
			base.Message = "预检没通过，没有导出。按 issues 逐条修正后再导出。"
			return base, fmt.Sprintf("预检没通过：%d 个问题", len(pre.Issues)), nil
		}
	}
	fp := paidFingerprint("smartcut_export", map[string]any{"projectId": idStr(pid), "nodeId": nodeID,
		"resolution": spec.Resolution, "fps": spec.FPS})
	checked := tid != 0
	g, err := gatePaid(ctx, d, p, paidAsk{Tool: "smartcut_export", Fingerprint: fp,
		ConfirmQuoteID: in.ConfirmQuoteID, ConfirmCredits: in.ConfirmCredits,
		Price: func(ctx context.Context) (app.Priced, error) {
			if !checked {
				if err := precheck(ctx); err != nil {
					return app.Priced{}, err
				}
			}
			checked = false
			if len(pre.Issues) > 0 {
				return app.Priced{}, httpx.Errf(errcode.InvalidArgument, "导出预检没通过（"+pre.Issues[0].Detail+
					"），没有导出：不带确认重新调用 smartcut_export 查看全部问题").
					WithDetails(map[string]any{"reason": "precheck_failed", "issues": issuesOut(pre.Issues)})
			}
			return app.Priced{Credits: pre.Credits, Detail: map[string]any{"nodeId": nodeID, "timelineId": idStr(tid),
				"durationMs": pre.DurationMs}, Summary: fmt.Sprintf("导出智能剪辑 %s 的成片（约 %d 秒）", nodeID,
				(pre.DurationMs+999)/1000)}, nil
		}})
	if err != nil {
		return smartcutExportOut{}, "", err
	}
	base.TimelineID = idStr(tid)
	switch g.Kind {
	case gateConfirm:
		if g.Pending.Elicit != nil {
			return smartcutExportOut{elicit: g.Pending.Elicit}, "", nil
		}
		nc, msg, summary := pendingOut(g.Pending, "smartcut_export")
		base.Status, base.NeedsConfirmation, base.Message, base.DurationMs = "needs_confirmation", nc, msg, pre.DurationMs
		return base, summary, nil
	case gateDeclined:
		base.Status, base.Message = "declined", declinedMessage
		return base, "用户已拒绝，未导出", nil
	case gateInProgress:
		base.Status, base.Credits, base.Replayed = "running", g.Verified.Credits, true
		base.Message = "这个确认已经受理，仍在处理中。不要重新提交；稍后用 smartcut_get 查看导出状态。"
		return base, "这个确认已受理，仍在处理中", nil
	case gateReplay:
		if err := replayedFailure(g.Result); err != nil {
			return smartcutExportOut{}, "", err
		}
		var ref struct {
			RenderID string `json:"renderId"`
		}
		_ = json.Unmarshal(g.Result, &ref)
		base.Status, base.RenderID, base.Credits, base.Replayed = "queued", ref.RenderID, g.Verified.Credits, true
		if rid, err := parseID("renderId", ref.RenderID); err == nil && d.Timelines != nil {
			if r, err := d.Timelines.Render(ctx, p, rid); err == nil {
				base.Status, base.Render = r.Status, renderOutOf(r)
				base.links = append(base.links, renderLinks(r)...)
			}
		}
		base.Message = paidReplayMessage + "用 smartcut_get 查看导出状态。"
		return base, "这个确认已受理过，本次没有重复导出", nil
	}
	key := g.Verified.IdemKey
	if key == "" {
		key = domain.ObjectBatchID(p.GrantID, strings.TrimSpace(in.IdempotencyKey), randomSuffix())
	}
	r, err := d.Smartcuts.StartExport(ctx, p, pid, nodeID, spec, key)
	if err != nil {
		recordPaid(ctx, d, p, g, failureOf(err))
		return smartcutExportOut{}, "", err
	}
	recordPaid(ctx, d, p, g, map[string]string{"renderId": idStr(r.ID)})
	status := r.Status
	if status == "" {
		status = "queued"
	}
	base.Status, base.TimelineID, base.RenderID, base.Render = status, idStr(r.TimelineID), idStr(r.ID), renderOutOf(r)
	base.Credits, base.DurationMs = g.Verified.Credits, pre.DurationMs
	base.Message = "导出已受理（后台任务，独立队列）。宿主超时不代表失败，不要重新提交；用 smartcut_get 或 tasks（kind=render）查看进度。"
	return base, fmt.Sprintf("已提交导出 %s（%d 积分）", base.RenderID, g.Verified.Credits), nil
}
