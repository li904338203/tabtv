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
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

// view_media（MKT-T-19，dev/01 MKD-18、§5.4）：只读、免费，按序号查看一张商品图。
//
// ⚠️ 入参只有「槽位 + 序号」，**没有媒体 ID**：图只能取自本会话 scene_input 里用户放进商品槽的那几张，
// 不能借它按 ID 读任意媒体（同工作区的别的素材也不行）；取图本身还按工作区校验归属（媒体域 SQL 条件）。
// ⚠️ 图不在工具结果里（协议层只有 user 消息能带图）：结果只说「已附上」，图由 Runner 在**下一次**模型调用时
// 以一条临时 user 消息附上，不落 agent_message。图是服务端缩好的 base64 内联，不发外链（MKT-AC-05）。

// SessionReader 读会话（*repo.Repo 直接满足）。
type SessionReader interface {
	GetByID(ctx context.Context, id int64) (repo.Session, error)
}

// ViewMedia 是 view_media 工具。
type ViewMedia struct {
	Sessions SessionReader
	Images   app.SceneImages
}

func (ViewMedia) Name() domain.ToolName { return domain.ToolViewMedia }
func (ViewMedia) Paid() bool            { return false }
func (ViewMedia) Action() collab.Action { return collab.ActionView }

func (ViewMedia) Description() string {
	return "按序号查看一张用户上传的商品图（「## 营销输入」里的「商品图 #n」）：slot 固定为 product，index 从 1 起。" +
		"图会在你下一次思考时附上；首条消息里已经能看到的图不用再看。免费。"
}

func (ViewMedia) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"slot":{"type":"string","enum":["product"],"description":"槽位，目前只有 product（商品图）"},` +
		`"index":{"type":"integer","minimum":1,"description":"商品图序号，#1 为主图"}` +
		`},"required":["slot","index"],"additionalProperties":false}`)
}

type viewMediaArgs struct {
	Slot  string `json:"slot"`
	Index int    `json:"index"`
}

// viewMediaResult 是回给模型的内容。Label 由 Runner 写进临时附图消息（「商品图 #7」）。
type viewMediaResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Label   string `json:"label"`
	Index   int    `json:"index"`
}

func (t ViewMedia) Run(ctx context.Context, rc RunContext, args json.RawMessage) (ToolResult, error) {
	if t.Sessions == nil || t.Images == nil {
		return ToolResult{}, errors.New("看图暂不可用，请根据商品名称、卖点与用户描述继续")
	}
	var a viewMediaArgs
	if err := ptDecode(args, &a, false); err != nil {
		return ToolResult{}, err
	}
	if s := strings.TrimSpace(a.Slot); s != "" && s != "product" {
		return ToolResult{}, fmt.Errorf("slot 只能是 product")
	}
	sess, err := t.Sessions.GetByID(ctx, rc.SessionID)
	if err != nil {
		return ToolResult{}, fmt.Errorf("读取会话失败：%w", err)
	}
	if sess.Scene != domain.SceneMarketing || sess.WorkspaceID != rc.WorkspaceID {
		return ToolResult{}, errors.New("当前会话没有商品图")
	}
	si, err := app.ParseSceneInput(sess.SceneInput)
	if err != nil {
		return ToolResult{}, err
	}
	total := len(si.Products)
	if total == 0 {
		return ToolResult{}, errors.New("用户没有上传商品图")
	}
	if a.Index < 1 || a.Index > total {
		return ToolResult{}, fmt.Errorf("index 越界：商品图共 %d 张，序号范围 1~%d", total, total)
	}
	id, err := strconv.ParseInt(si.Products[a.Index-1], 10, 64)
	if err != nil || id <= 0 {
		return ToolResult{}, fmt.Errorf("商品图 #%d 暂时取不到", a.Index)
	}
	img, err := t.Images.InlineImage(ctx, rc.WorkspaceID, id)
	if err != nil {
		// 不回内部错误原文：媒体域的错误里可能有对象键之类的存储细节。
		return ToolResult{}, fmt.Errorf("商品图 #%d 暂时取不到，请先基于已看到的信息继续", a.Index)
	}
	label := fmt.Sprintf("商品图 #%d", a.Index)
	return ToolResult{
		Content: ptJSON(viewMediaResult{OK: true, Message: "已附上" + label + "，见下一条消息",
			Label: label, Index: a.Index}),
		Display:      ptJSON(map[string]any{"slot": "product", "index": a.Index}),
		AttachImages: []string{img},
	}, nil
}
