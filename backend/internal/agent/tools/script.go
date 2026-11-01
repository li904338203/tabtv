package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/canvasgen"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

// 脚本线工具（AGT-50）：script_create（免费）→ script_storyboard → script_assets → script_compose。
//
// ⚠️ 只编排、不重写规则：分镜「结算晚于校验」、轮次随调用次数变、资产图带 scriptAssetId、
// 自动识别只在本脚本内归并（AV-20）都由脚本域保证；Agent 不得绕开它直接调模型，
// 也**不要**用 generate 给资产出图（那样 Worker 回写不了 script_asset，资产永远转圈）。

// ---------------------------------------------------------------- script_create

// ScriptCreate 是 script_create：先在画布上建脚本节点，再按节点建脚本（按节点幂等）。
type ScriptCreate struct{ d MultiDeps }

func (ScriptCreate) Name() domain.ToolName { return domain.ToolScriptCreate }
func (ScriptCreate) Paid() bool            { return false }
func (ScriptCreate) Action() collab.Action { return collab.ActionEdit }

func (ScriptCreate) Description() string {
	return "多段视频的第一步：在画布上新建一个脚本节点并建脚本（免费）。text 是剧情 / 角色设定 / 你写好的分镜大纲；" +
		"sourceType：script=按剧本生成分镜、character=按角色生成、manual=自己逐镜编写。返回 scriptId，之后用 script_storyboard 生成分镜。"
}

func (ScriptCreate) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","required":["text"],"properties":{` +
		`"text":{"type":"string","description":"剧情 / 角色设定原文"},` +
		`"sourceType":{"type":"string","enum":["script","character","manual"],"default":"script"},` +
		`"globalStyle":{"type":"string","description":"全片风格（可选）"},` +
		`"title":{"type":"string"},` +
		`"position":{"type":"object","properties":{"x":{"type":"number"},"y":{"type":"number"}}}}}`)
}

type scriptCreateArgs struct {
	Text        string    `json:"text"`
	SourceType  string    `json:"sourceType"`
	GlobalStyle string    `json:"globalStyle"`
	Title       string    `json:"title"`
	Position    *position `json:"position"`
}

const maxScriptText = 20000

func (t ScriptCreate) Run(ctx context.Context, rc app.RunContext, raw json.RawMessage) (app.ToolResult, error) {
	if err := requireExecuting(rc, domain.ToolScriptCreate); err != nil {
		return app.ToolResult{}, err
	}
	if t.d.Canvas == nil || t.d.Scripts == nil {
		return app.ToolResult{}, errors.New("脚本服务未接入")
	}
	var a scriptCreateArgs
	if err := multiDecode(raw, &a); err != nil {
		return app.ToolResult{}, err
	}
	a.Text = strings.TrimSpace(a.Text)
	if a.Text == "" {
		return app.ToolResult{}, errors.New("text 不能为空：写上剧情、角色设定或分镜大纲")
	}
	if ptRunes([]byte(a.Text)) > maxScriptText {
		return app.ToolResult{}, fmt.Errorf("text 超过 %d 字，请精简后再建脚本", maxScriptText)
	}
	if a.SourceType == "" {
		a.SourceType = "script"
	}
	if a.SourceType != "script" && a.SourceType != "character" && a.SourceType != "manual" {
		return app.ToolResult{}, errors.New("sourceType 只能是 script / character / manual")
	}
	title := strings.TrimSpace(a.Title)
	if title == "" {
		title = "脚本生成器"
	}
	// ① 节点先落盘（脚本与画布节点一一对应）→ ② 按节点建脚本（同一节点重复建 = 返回已有的那一个）
	// → ③ 把 scriptId 写回节点（前端按 params.scriptId 打开脚本）。整条在 canvasgen（MCPT-16，与 MCP 共用）。
	var pos *canvasgen.Position
	if a.Position != nil {
		pos = &canvasgen.Position{X: a.Position.X, Y: a.Position.Y}
	}
	o := objectOrch(t.d.Canvas)
	o.Logger = t.d.logger()
	var sc app.ScriptView
	nodeID, err := o.CreateObjectNode(ctx, multiCaller(rc), canvasgen.ObjectNode{NodeType: "script", Title: title,
		Params: map[string]any{"sourceType": a.SourceType, "sourceText": a.Text}, Position: pos}, "script",
		func(ctx context.Context, nodeID string) (map[string]any, error) {
			v, err := t.d.Scripts.CreateScript(ctx, app.ScriptCreateInput{
				WorkspaceID: rc.WorkspaceID, ProjectID: rc.ProjectID, CanvasNodeID: nodeID,
				SourceType: a.SourceType, RawInput: a.Text, GlobalStyle: strings.TrimSpace(a.GlobalStyle),
			})
			if err != nil {
				return nil, err
			}
			sc = v
			return scriptNodeParams(v), nil
		})
	var oe *canvasgen.ObjectError
	if errors.As(err, &oe) {
		return app.ToolResult{}, fmt.Errorf("脚本节点 %s 已建好，但建脚本失败：%v", oe.NodeID, oe.Err)
	}
	if err != nil {
		return app.ToolResult{}, ensureNodeError(err)
	}
	content := map[string]any{"scriptId": idS(sc.ID), "nodeId": nodeID, "shots": len(sc.Shots),
		"next": "用 script_storyboard 生成分镜"}
	return app.ToolResult{Content: ptJSON(content),
		Display: ptJSON(map[string]any{"scriptId": idS(sc.ID), "nodeId": nodeID}), Outputs: 1}, nil
}

// ---------------------------------------------------------------- script_storyboard

// ScriptStoryboard 是 script_storyboard：同步生成分镜（计费在 script_llm_call，解析失败由脚本域全额退款）。
type ScriptStoryboard struct{ d MultiDeps }

func (ScriptStoryboard) Name() domain.ToolName { return domain.ToolScriptStoryboard }
func (ScriptStoryboard) Paid() bool            { return true }
func (ScriptStoryboard) Action() collab.Action { return collab.ActionGenerate }

func (ScriptStoryboard) Description() string {
	return "为脚本生成分镜（会扣积分；同步执行，可能要一两分钟）。返回镜头列表（镜号、时长、摘要）。" +
		"再次调用 = 重新生成并覆盖镜头表（会再扣一次费），只在用户要求时这么做。"
}

func (ScriptStoryboard) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","required":["scriptId","modelCode"],"properties":{` +
		`"scriptId":{"type":"string"},"modelCode":{"type":"string","description":"文本模型"},` +
		`"shotCount":{"type":"integer","minimum":0,"maximum":60,"description":"期望镜头数，0=由模型决定"},` +
		`"extraContext":{"type":"string","description":"补充要求（每段时长上限、镜头节奏等）"}}}`)
}

type storyboardArgs struct {
	ScriptID     flexID `json:"scriptId"`
	ModelCode    string `json:"modelCode"`
	ShotCount    int    `json:"shotCount"`
	ExtraContext string `json:"extraContext"`
}

func (t ScriptStoryboard) Run(ctx context.Context, rc app.RunContext, raw json.RawMessage) (app.ToolResult, error) {
	if err := requireExecuting(rc, domain.ToolScriptStoryboard); err != nil {
		return app.ToolResult{}, err
	}
	if t.d.Scripts == nil || t.d.Budget == nil {
		return app.ToolResult{}, errors.New("脚本服务未接入")
	}
	var a storyboardArgs
	if err := multiDecode(raw, &a); err != nil {
		return app.ToolResult{}, err
	}
	if a.ScriptID == 0 || strings.TrimSpace(a.ModelCode) == "" {
		return app.ToolResult{}, errors.New("scriptId 与 modelCode 必填")
	}
	if a.ShotCount < 0 || a.ShotCount > 60 {
		return app.ToolResult{}, errors.New("shotCount 只能是 0~60")
	}
	req := app.StoryboardRequest{WorkspaceID: rc.WorkspaceID, PayerAccountID: rc.PayerAccountID,
		UserID: rc.UserID, ScriptID: int64(a.ScriptID), MemberLevelID: rc.MemberLevelID,
		ModelCode: strings.TrimSpace(a.ModelCode), ShotCount: a.ShotCount, ExtraContext: strings.TrimSpace(a.ExtraContext)}
	quote, err := t.d.Scripts.QuoteStoryboard(ctx, req)
	if err != nil {
		return app.ToolResult{}, fmt.Errorf("分镜报价失败：%v", err)
	}
	var out app.StoryboardOutcome
	res, susp, err := t.d.Budget.PaidCallTool(ctx, rc, string(domain.ToolScriptStoryboard), quote,
		func(string) (app.PaidResult, error) {
			// 分镜的幂等由脚本域的轮次（含失败）保证；外壳的幂等键用不上。
			r, err := t.d.Scripts.Storyboard(ctx, req)
			if err != nil {
				return app.PaidResult{}, err
			}
			out = r
			return app.PaidResult{Credits: r.Credits, RefType: "script_llm_call", RefID: r.CallID}, nil
		})
	if susp != nil {
		return overBudgetResult(susp, quote, map[string]any{"scriptId": idS(req.ScriptID)}), nil
	}
	if err != nil {
		return app.ToolResult{}, paidError("分镜生成", quote, err)
	}
	if res.Replayed {
		if out.Script, err = t.d.Scripts.Script(ctx, rc.WorkspaceID, req.ScriptID); err != nil {
			return app.ToolResult{}, err
		}
	}
	syncNodeParams(ctx, t.d, rc, out.Script.CanvasNodeID, "script", scriptNodeParams(out.Script))
	content := map[string]any{"scriptId": idS(req.ScriptID), "credits": res.Credits,
		"shotCount": len(out.Script.Shots), "shots": out.Script.Shots,
		"next": "用 script_assets 对账资产并出资产设定图"}
	return app.ToolResult{Content: ptJSON(content), Display: ptJSON(map[string]any{
		"scriptId": idS(req.ScriptID), "credits": res.Credits, "shotCount": len(out.Script.Shots)}), Outputs: 1}, nil
}

// ---------------------------------------------------------------- script_assets

// ScriptAssets 是 script_assets：先对账资产（免费），再为未就绪的资产批量出设定图（逐项扣费）。
type ScriptAssets struct{ d MultiDeps }

func (ScriptAssets) Name() domain.ToolName { return domain.ToolScriptAssets }
func (ScriptAssets) Paid() bool            { return true }
func (ScriptAssets) Action() collab.Action { return collab.ActionGenerate }

func (ScriptAssets) Description() string {
	return "按分镜对账角色 / 场景 / 道具资产，并为还没有设定图的资产批量出图（每张单独扣费）。" +
		"资产只在本脚本内归并；要复用别的脚本里的角色，用 list_assets 列出后请用户在卡片上选择。" +
		"wait=true（默认）提交后暂停，全部出完再继续。"
}

func (ScriptAssets) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","required":["scriptId","modelCode"],"properties":{` +
		`"scriptId":{"type":"string"},"modelCode":{"type":"string","description":"图片模型"},` +
		`"assetIds":{"type":"array","items":{"type":"string"},"description":"只出这几个；不给=全部未就绪的"},` +
		`"params":{"type":"object","properties":{"aspect_ratio":{"type":"string"},"quality":{"type":"string"}}},` +
		`"wait":{"type":"boolean","default":true}}}`)
}

type assetsArgs struct {
	ScriptID  flexID         `json:"scriptId"`
	ModelCode string         `json:"modelCode"`
	AssetIDs  []flexID       `json:"assetIds"`
	Params    map[string]any `json:"params"`
	Wait      *bool          `json:"wait"`
}

func (t ScriptAssets) Run(ctx context.Context, rc app.RunContext, raw json.RawMessage) (app.ToolResult, error) {
	if err := requireExecuting(rc, domain.ToolScriptAssets); err != nil {
		return app.ToolResult{}, err
	}
	if t.d.Scripts == nil || t.d.Budget == nil {
		return app.ToolResult{}, errors.New("脚本服务未接入")
	}
	var a assetsArgs
	if err := multiDecode(raw, &a); err != nil {
		return app.ToolResult{}, err
	}
	if a.ScriptID == 0 || strings.TrimSpace(a.ModelCode) == "" {
		return app.ToolResult{}, errors.New("scriptId 与 modelCode 必填")
	}
	req := app.AssetsRequest{WorkspaceID: rc.WorkspaceID, PayerAccountID: rc.PayerAccountID, UserID: rc.UserID,
		ScriptID: int64(a.ScriptID), MemberLevelID: rc.MemberLevelID, ModelCode: strings.TrimSpace(a.ModelCode),
		AssetIDs: ids64(a.AssetIDs), Params: a.Params}
	q, err := t.d.Scripts.QuoteAssets(ctx, req)
	if err != nil {
		return app.ToolResult{}, fmt.Errorf("资产对账 / 报价失败：%v", err)
	}
	if len(q.Pending) == 0 {
		return app.ToolResult{Content: ptJSON(map[string]any{"pending": 0, "unmatched": q.Unmatched,
			"message": "资产都已有设定图，不需要出图。下一步用 script_compose 合成提示词"}), Outputs: 1}, nil
	}
	var out app.AssetsOutcome
	res, susp, err := t.d.Budget.PaidCallTool(ctx, rc, string(domain.ToolScriptAssets), q.Total,
		func(string) (app.PaidResult, error) {
			// 每项的幂等键由脚本域按「资产 ID + 描述指纹」给（双击收敛、失败换键）。
			r, err := t.d.Scripts.GenerateAssets(ctx, req)
			if err != nil {
				return app.PaidResult{}, err
			}
			out = r
			if len(r.GenerationIDs) == 0 {
				// 一张都没提交出去：没有扣费，按失败释放额度并把每一项的原因回给模型。
				return app.PaidResult{}, fmt.Errorf("没有任何资产提交成功：%s", assetReasons(r.Items))
			}
			return app.PaidResult{Credits: -1, RefType: "asset_batch", RefID: req.ScriptID,
				RefIDs: r.GenerationIDs}, nil
		})
	if susp != nil {
		return overBudgetResult(susp, q.Total, map[string]any{"scriptId": idS(req.ScriptID),
			"pending": len(q.Pending)}), nil
	}
	if err != nil {
		return app.ToolResult{}, paidError("资产出图", q.Total, err)
	}
	items := make([]map[string]any, 0, len(out.Items))
	for _, it := range out.Items {
		m := map[string]any{"assetId": idS(it.AssetID), "name": it.Name}
		if it.GenerationID != 0 && !it.Skipped {
			m["generationId"] = idS(it.GenerationID)
		}
		if it.Skipped {
			m["skipped"], m["reason"] = true, it.Reason
		}
		items = append(items, m)
	}
	wait := wantWait(a.Wait)
	submitted := len(res.RefIDs)
	content := map[string]any{"scriptId": idS(req.ScriptID), "submitted": submitted, "quoted": q.Total,
		"items": items, "unmatched": q.Unmatched, "waiting": wait}
	if res.Replayed {
		content["replayed"] = true
	}
	r := app.ToolResult{Content: ptJSON(content), Display: ptJSON(map[string]any{"scriptId": idS(req.ScriptID),
		"submitted": submitted, "credits": q.Total}), Outputs: 1, WaitRef: true}
	if wait {
		r.Suspend = waitSuspension("等待资产设定图全部出完")
	}
	return r, nil
}

func assetReasons(items []app.AssetItemOutcome) string {
	var parts []string
	for _, it := range items {
		if it.Reason != "" {
			parts = append(parts, it.Name+"："+it.Reason)
		}
		if len(parts) >= 8 {
			break
		}
	}
	if len(parts) == 0 {
		return "未知原因"
	}
	return strings.Join(parts, "；")
}

// ---------------------------------------------------------------- script_compose

// ScriptCompose 是 script_compose：合成分镜图 / 视频提示词。ai 模式按条扣费，template 模式 0 积分（不进额度）。
type ScriptCompose struct{ d MultiDeps }

func (ScriptCompose) Name() domain.ToolName { return domain.ToolScriptCompose }
func (ScriptCompose) Paid() bool            { return true }
func (ScriptCompose) Action() collab.Action { return collab.ActionGenerate }

func (ScriptCompose) Description() string {
	return "为镜头合成分镜图 / 视频提示词。source=ai 由文本模型写（每条扣费），template 由代码按模板拼（免费）。" +
		"已就绪且上游没变的会跳过；force=true 才重算（会再扣费）。"
}

func (ScriptCompose) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","required":["scriptId"],"properties":{` +
		`"scriptId":{"type":"string"},"source":{"type":"string","enum":["ai","template"],"default":"ai"},` +
		`"modelCode":{"type":"string","description":"source=ai 时必填"},` +
		`"shotIds":{"type":"array","items":{"type":"string"},"description":"不给=全部镜头"},` +
		`"kinds":{"type":"array","items":{"type":"string","enum":["image","video"]},"description":"不给=两种都合成"},` +
		`"force":{"type":"boolean"}}}`)
}

type composeArgs struct {
	ScriptID  flexID   `json:"scriptId"`
	Source    string   `json:"source"`
	ModelCode string   `json:"modelCode"`
	ShotIDs   []flexID `json:"shotIds"`
	Kinds     []string `json:"kinds"`
	Force     bool     `json:"force"`
}

func (t ScriptCompose) Run(ctx context.Context, rc app.RunContext, raw json.RawMessage) (app.ToolResult, error) {
	if err := requireExecuting(rc, domain.ToolScriptCompose); err != nil {
		return app.ToolResult{}, err
	}
	if t.d.Scripts == nil || t.d.Budget == nil {
		return app.ToolResult{}, errors.New("脚本服务未接入")
	}
	var a composeArgs
	if err := multiDecode(raw, &a); err != nil {
		return app.ToolResult{}, err
	}
	if a.Source == "" {
		a.Source = "ai"
	}
	if a.Source != "ai" && a.Source != "template" {
		return app.ToolResult{}, errors.New("source 只能是 ai / template")
	}
	if a.ScriptID == 0 || (a.Source == "ai" && strings.TrimSpace(a.ModelCode) == "") {
		return app.ToolResult{}, errors.New("scriptId 必填；source=ai 时 modelCode 必填")
	}
	req := app.ComposeRequest{WorkspaceID: rc.WorkspaceID, PayerAccountID: rc.PayerAccountID, UserID: rc.UserID,
		ScriptID: int64(a.ScriptID), ShotIDs: ids64(a.ShotIDs), Kinds: a.Kinds, Source: a.Source,
		ModelCode: strings.TrimSpace(a.ModelCode), Force: a.Force}
	// 先 DryRun 拿报价（脚本域自己的报价，不是我们估的）。
	q, err := t.d.Scripts.QuoteCompose(ctx, req)
	if err != nil {
		return app.ToolResult{}, fmt.Errorf("提示词合成报价失败：%v", err)
	}
	var out app.ComposeOutcome
	credits := 0
	if q.AICount == 0 || q.Total == 0 {
		// template 模式（或全部跳过）0 积分：不调模型、不扣费，所以**不进额度**。
		if out, err = t.d.Scripts.Compose(ctx, req); err != nil {
			return app.ToolResult{}, fmt.Errorf("提示词合成失败：%v", err)
		}
	} else {
		res, susp, err := t.d.Budget.PaidCallTool(ctx, rc, string(domain.ToolScriptCompose), q.Total,
			func(string) (app.PaidResult, error) {
				r, err := t.d.Scripts.Compose(ctx, req)
				if err != nil {
					return app.PaidResult{}, err
				}
				out = r
				// 逐条在账本上（script_llm / callID）；失败的那几条由脚本域退款，扫描循环按账本结算部分退回。
				return app.PaidResult{Credits: -1, RefType: "script_compose", RefID: req.ScriptID,
					RefIDs: r.CallIDs}, nil
			})
		if susp != nil {
			return overBudgetResult(susp, q.Total, map[string]any{"scriptId": idS(req.ScriptID)}), nil
		}
		if err != nil {
			return app.ToolResult{}, paidError("提示词合成", q.Total, err)
		}
		credits = q.Total
		if res.Replayed {
			out.Quote = q
		}
	}
	var done, failed, skipped int
	var problems []string
	for _, it := range out.Items {
		switch {
		case it.Status == "failed":
			failed++
			problems = append(problems, fmt.Sprintf("第 %d 镜 %s：%s", it.ShotNo, it.Kind, it.Reason))
		case it.Skipped:
			skipped++
		default:
			done++
		}
	}
	if sc, err := t.d.Scripts.Script(ctx, rc.WorkspaceID, req.ScriptID); err == nil {
		syncNodeParams(ctx, t.d, rc, sc.CanvasNodeID, "script", scriptNodeParams(sc))
	}
	content := map[string]any{"scriptId": idS(req.ScriptID), "source": a.Source, "composed": done,
		"failed": failed, "skipped": skipped, "quoted": credits}
	if len(problems) > 0 {
		content["failures"] = problems
	}
	return app.ToolResult{Content: ptJSON(content), Display: ptJSON(map[string]any{"scriptId": idS(req.ScriptID),
		"composed": done, "failed": failed, "credits": credits}), Outputs: 1}, nil
}

var (
	_ app.Tool = ScriptCreate{}
	_ app.Tool = ScriptStoryboard{}
	_ app.Tool = ScriptAssets{}
	_ app.Tool = ScriptCompose{}
)
