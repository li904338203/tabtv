package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

// 组工具（AGT-50）：group_create / group_execute。
//
// ⚠️ 两种组的提交时机按钱分档（AV-23），工具照搬：分镜图组**建组即提交 = 付费**（走 PaidCall），
// 视频组**先落 draft**（免费，把 videoPlan 预览回给模型与用户）—— 执行要单独调 group_execute，
// 那一步才扣费，且受规划确认的额度约束。
// ⚠️ 子项必须带 groupItemId：走 CreateGroup / ExecuteGroup 天然满足，别用 generate 自己逐镜出片。
// ⚠️ 组部分成功不摊平：额度按账本结算，失败项的钱由生成域退回后记进条目的 refunded（扫描循环）。

var groupTypes = map[string]bool{"storyboard_image": true, "video_generator": true, "asset": true}

// ---------------------------------------------------------------- group_create

// GroupCreate 是 group_create。
type GroupCreate struct{ d MultiDeps }

func (GroupCreate) Name() domain.ToolName { return domain.ToolGroupCreate }
func (GroupCreate) Paid() bool            { return true }
func (GroupCreate) Action() collab.Action { return collab.ActionGenerate }

func (GroupCreate) Description() string {
	return "在画布上建组节点。storyboard_image=分镜图组：建组即为每个提示词已就绪的镜头出分镜图（逐项扣费）；" +
		"video_generator=视频组：只建草稿（免费），返回每镜会用的输入编排（videoPlan），确认后再用 group_execute 执行；" +
		"asset=资产组（只聚合展示，免费）。"
}

func (GroupCreate) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","required":["scriptId","groupType"],"properties":{` +
		`"scriptId":{"type":"string"},` +
		`"groupType":{"type":"string","enum":["storyboard_image","video_generator","asset"]},` +
		`"name":{"type":"string"},` +
		`"shotIds":{"type":"array","items":{"type":"string"},"description":"不给=全部镜头"},` +
		`"modelCode":{"type":"string","description":"分镜图组必填（建组即提交）"},` +
		`"operation":{"type":"string","description":"视频组默认操作，如 ref2video / img2video / first_last_frame"},` +
		`"durationSec":{"type":"integer","minimum":0,"description":"视频组每镜时长（秒）；不给按镜头自己的时长"},` +
		`"durations":{"type":"object","additionalProperties":{"type":"integer"},"description":"逐镜时长：镜头 ID → 秒"},` +
		`"params":{"type":"object","description":"整组参数：aspect_ratio / quality / resolution / audio"},` +
		`"position":{"type":"object","properties":{"x":{"type":"number"},"y":{"type":"number"}}},` +
		`"wait":{"type":"boolean","default":true,"description":"分镜图组：提交后暂停等全部出完"}}}`)
}

type groupCreateArgs struct {
	ScriptID    flexID         `json:"scriptId"`
	GroupType   string         `json:"groupType"`
	Name        string         `json:"name"`
	ShotIDs     []flexID       `json:"shotIds"`
	ModelCode   string         `json:"modelCode"`
	Operation   string         `json:"operation"`
	DurationSec int            `json:"durationSec"`
	Durations   map[string]int `json:"durations"`
	Params      map[string]any `json:"params"`
	Position    *position      `json:"position"`
	Wait        *bool          `json:"wait"`
}

var groupTitles = map[string]string{"storyboard_image": "分镜图组", "video_generator": "视频生成器组", "asset": "资产组"}

func (t GroupCreate) Run(ctx context.Context, rc app.RunContext, raw json.RawMessage) (app.ToolResult, error) {
	if err := requireExecuting(rc, domain.ToolGroupCreate); err != nil {
		return app.ToolResult{}, err
	}
	if t.d.Canvas == nil || t.d.Scripts == nil || t.d.Budget == nil {
		return app.ToolResult{}, errors.New("脚本服务未接入")
	}
	var a groupCreateArgs
	if err := multiDecode(raw, &a); err != nil {
		return app.ToolResult{}, err
	}
	if a.ScriptID == 0 || !groupTypes[a.GroupType] {
		return app.ToolResult{}, errors.New("scriptId 必填，groupType 只能是 storyboard_image / video_generator / asset")
	}
	model := strings.TrimSpace(a.ModelCode)
	if a.GroupType == "storyboard_image" && model == "" {
		return app.ToolResult{}, errors.New("分镜图组建组即提交，modelCode 必填（图片模型）")
	}
	if a.DurationSec < 0 {
		return app.ToolResult{}, errors.New("durationSec 不能为负")
	}
	durations := map[int64]int{}
	for k, v := range a.Durations {
		id, err := strconv.ParseInt(k, 10, 64)
		if err != nil || id <= 0 || v < 0 {
			return app.ToolResult{}, errors.New("durations 的键必须是镜头 ID、值是非负秒数")
		}
		durations[id] = v
	}
	name := strings.TrimSpace(a.Name)
	if name == "" {
		name = groupTitles[a.GroupType]
	}
	// ① 组节点先落盘（组与画布节点一一对应；报价也要按这个节点 ID 判「是不是已经建过」）。
	nodeParams := map[string]any{"groupType": a.GroupType, "scriptId": idS(int64(a.ScriptID)),
		"projectId": idS(rc.ProjectID)}
	if model != "" {
		nodeParams["modelCode"] = model
	}
	nodeID, err := addAgentNode(ctx, t.d.Canvas, rc, "group", name, nodeParams, a.Position)
	if err != nil {
		return app.ToolResult{}, err
	}
	req := app.GroupCreateRequest{WorkspaceID: rc.WorkspaceID, PayerAccountID: rc.PayerAccountID, UserID: rc.UserID,
		ScriptID: int64(a.ScriptID), CanvasNodeID: nodeID, GroupType: a.GroupType, Name: name,
		ShotIDs: ids64(a.ShotIDs), ModelCode: model, Operation: strings.TrimSpace(a.Operation),
		DurationSec: a.DurationSec, Durations: durations, Params: a.Params}

	if a.GroupType != "storyboard_image" {
		// 视频组 / 资产组：建组不产生任务、不扣费，不走额度。
		g, err := t.d.Scripts.CreateGroup(ctx, req)
		if err != nil {
			return app.ToolResult{}, fmt.Errorf("组节点 %s 已建好，但建组失败：%v", nodeID, err)
		}
		syncNodeParams(ctx, t.d, rc, nodeID, "group", groupNodeParams(g, model))
		content := map[string]any{"groupId": idS(g.ID), "nodeId": nodeID, "groupType": g.GroupType,
			"status": g.Status, "items": groupItemsView(g)}
		if a.GroupType == "video_generator" {
			content["videoPlan"] = groupItemsView(g)
			content["next"] = "这是草稿，还没有扣费。向用户说明每镜的输入编排与时长；确认后用 group_execute 执行"
		}
		return app.ToolResult{Content: ptJSON(content), Display: ptJSON(map[string]any{"groupId": idS(g.ID),
			"nodeId": nodeID, "groupType": g.GroupType, "itemCount": len(g.Items),
			"videoPlan": groupItemsView(g)}), Outputs: 1}, nil
	}

	// ② 分镜图组：建组即提交 —— 先逐项报价（与受理同一组参数），再在额度内建组。
	q, err := t.d.Scripts.QuoteGroupCreate(ctx, req)
	if err != nil {
		return app.ToolResult{}, fmt.Errorf("组节点 %s 已建好，但报价失败：%v", nodeID, err)
	}
	var g app.GroupView
	res, susp, err := t.d.Budget.PaidCallTool(ctx, rc, string(domain.ToolGroupCreate), q.Total,
		func(string) (app.PaidResult, error) {
			// 每项的幂等键由脚本域按「组 + 子项 + 输入指纹」给。
			v, err := t.d.Scripts.CreateGroup(ctx, req)
			if err != nil {
				return app.PaidResult{}, err
			}
			g = v
			gens, credits := submittedItems(v, nil)
			return app.PaidResult{Credits: credits, RefType: "script_group", RefID: v.ID, RefIDs: gens}, nil
		})
	if susp != nil {
		return overBudgetResult(susp, q.Total, map[string]any{"nodeId": nodeID, "skipped": q.Skipped}), nil
	}
	if err != nil {
		return app.ToolResult{}, paidError(fmt.Sprintf("建分镜图组（节点 %s 已保留）", nodeID), q.Total, err)
	}
	if res.Replayed {
		if g, err = t.d.Scripts.Group(ctx, rc.WorkspaceID, res.RefID); err != nil {
			return app.ToolResult{}, err
		}
	}
	syncNodeParams(ctx, t.d, rc, nodeID, "group", groupNodeParams(g, model))
	return groupSubmitted(g, nodeID, res, q, wantWait(a.Wait), "等待分镜图全部出完"), nil
}

// submittedItems 是这一次提交出去的子项：生成任务 ID 不在 before 里的那些（before=nil 表示全部算）。
func submittedItems(g app.GroupView, before map[int64]bool) (gens []int64, credits int) {
	for _, it := range g.Items {
		if it.GenerationID == 0 || before[it.GenerationID] {
			continue
		}
		gens = append(gens, it.GenerationID)
		credits += it.Credits
	}
	return gens, credits
}

// groupSubmitted 是组提交之后回给模型的结果（含逐项状态与跳过原因；有提交的项时挂起等终态）。
func groupSubmitted(g app.GroupView, nodeID string, res app.PaidResult, q app.GroupQuote, wait bool,
	reason string) app.ToolResult {
	submitted := len(res.RefIDs)
	content := map[string]any{"groupId": idS(g.ID), "groupType": g.GroupType, "status": g.Status,
		"submitted": submitted, "quoted": q.Total, "items": groupItemsView(g)}
	if nodeID != "" {
		content["nodeId"] = nodeID
	}
	if len(q.Skipped) > 0 {
		content["skipped"] = q.Skipped
	}
	if res.Replayed {
		content["replayed"] = true
	}
	out := app.ToolResult{Content: ptJSON(content), Display: ptJSON(map[string]any{"groupId": idS(g.ID),
		"nodeId": nodeID, "submitted": submitted, "credits": q.Total}), Outputs: 1}
	if submitted == 0 {
		content["message"] = "没有任何一项提交成功（没有扣费），看 items 里的 error 决定怎么改"
		out.Content = ptJSON(content)
		return out
	}
	out.WaitRef = true
	content["waiting"] = wait
	out.Content = ptJSON(content)
	if wait {
		out.Suspend = waitSuspension(reason)
	}
	return out
}

// ---------------------------------------------------------------- group_execute

// GroupExecute 是 group_execute：执行组（或指定子项），逐项受理、逐项扣费。
type GroupExecute struct{ d MultiDeps }

func (GroupExecute) Name() domain.ToolName { return domain.ToolGroupExecute }
func (GroupExecute) Paid() bool            { return true }
func (GroupExecute) Action() collab.Action { return collab.ActionGenerate }

func (GroupExecute) Description() string {
	return "执行组（视频组草稿 / 失败项重试）：每项单独受理、单独扣费；已成功的跳过（force=true 才重跑）。" +
		"余额不足时停掉后续项。wait=true（默认）提交后暂停等全部结束；部分成功时会逐项说明失败原因。"
}

func (GroupExecute) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","required":["groupId","modelCode"],"properties":{` +
		`"groupId":{"type":"string"},"modelCode":{"type":"string"},` +
		`"operation":{"type":"string","description":"视频组操作码覆盖（不给=用每项规划好的）"},` +
		`"durationSec":{"type":"integer","minimum":0},` +
		`"itemIds":{"type":"array","items":{"type":"string"},"description":"只执行这几项；不给=整组"},` +
		`"force":{"type":"boolean"},"wait":{"type":"boolean","default":true}}}`)
}

type groupExecuteArgs struct {
	GroupID     flexID   `json:"groupId"`
	ModelCode   string   `json:"modelCode"`
	Operation   string   `json:"operation"`
	DurationSec int      `json:"durationSec"`
	ItemIDs     []flexID `json:"itemIds"`
	Force       bool     `json:"force"`
	Wait        *bool    `json:"wait"`
}

func (t GroupExecute) Run(ctx context.Context, rc app.RunContext, raw json.RawMessage) (app.ToolResult, error) {
	if err := requireExecuting(rc, domain.ToolGroupExecute); err != nil {
		return app.ToolResult{}, err
	}
	if t.d.Scripts == nil || t.d.Budget == nil {
		return app.ToolResult{}, errors.New("脚本服务未接入")
	}
	var a groupExecuteArgs
	if err := multiDecode(raw, &a); err != nil {
		return app.ToolResult{}, err
	}
	if a.GroupID == 0 || strings.TrimSpace(a.ModelCode) == "" {
		return app.ToolResult{}, errors.New("groupId 与 modelCode 必填")
	}
	if a.DurationSec < 0 {
		return app.ToolResult{}, errors.New("durationSec 不能为负")
	}
	req := app.GroupExecuteRequest{WorkspaceID: rc.WorkspaceID, PayerAccountID: rc.PayerAccountID, UserID: rc.UserID,
		GroupID: int64(a.GroupID), ModelCode: strings.TrimSpace(a.ModelCode), Operation: strings.TrimSpace(a.Operation),
		DurationSec: a.DurationSec, Force: a.Force, ItemIDs: ids64(a.ItemIDs)}
	q, err := t.d.Scripts.QuoteGroupExecute(ctx, req)
	if err != nil {
		return app.ToolResult{}, fmt.Errorf("组执行报价失败：%v", err)
	}
	if len(q.Items) == 0 {
		return app.ToolResult{Content: ptJSON(map[string]any{"groupId": idS(req.GroupID), "submitted": 0,
			"skipped": q.Skipped, "message": "没有需要执行的子项（没有扣费）"}), Outputs: 1}, nil
	}
	var g app.GroupView
	res, susp, err := t.d.Budget.PaidCallTool(ctx, rc, string(domain.ToolGroupExecute), q.Total,
		func(string) (app.PaidResult, error) {
			before, err := t.d.Scripts.Group(ctx, rc.WorkspaceID, req.GroupID)
			if err != nil {
				return app.PaidResult{}, err
			}
			seen := map[int64]bool{}
			for _, it := range before.Items {
				if it.GenerationID != 0 {
					seen[it.GenerationID] = true
				}
			}
			v, err := t.d.Scripts.ExecuteGroup(ctx, req)
			if err != nil {
				return app.PaidResult{}, err
			}
			g = v
			gens, credits := submittedItems(v, seen)
			return app.PaidResult{Credits: credits, RefType: "script_group", RefID: v.ID, RefIDs: gens}, nil
		})
	if susp != nil {
		return overBudgetResult(susp, q.Total, map[string]any{"groupId": idS(req.GroupID)}), nil
	}
	if err != nil {
		return app.ToolResult{}, paidError("组执行", q.Total, err)
	}
	if res.Replayed || g.ID == 0 {
		if g, err = t.d.Scripts.Group(ctx, rc.WorkspaceID, req.GroupID); err != nil {
			return app.ToolResult{}, err
		}
	}
	// 组节点若是本会话建的，把子项状态写回去（用户的组节点由前端自己轮询刷新）。
	syncNodeParams(ctx, t.d, rc, g.CanvasNodeID, "group", groupNodeParams(g, req.ModelCode))
	return groupSubmitted(g, "", res, q, wantWait(a.Wait), "等待组内任务全部结束"), nil
}

var (
	_ app.Tool = GroupCreate{}
	_ app.Tool = GroupExecute{}
)
