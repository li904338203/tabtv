package app

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
)

// sceneInputUnavailable 是营销输入取不到时写进「## 营销输入」的一句话：段落照常出现（模型知道这是
// 营销会话、知道该问什么），但不编造任何商品信息。
const sceneInputUnavailable = "（营销输入暂时无法读取；以用户消息为准，需要时用 ask_user 向用户确认商品与卖点。）"

// sceneSections 给出营销会话系统提示词的③场景段正文与④营销输入正文（MKT-T-18，dev/01 §3.3）。
// 通用会话两段都为空。
//
//   - ③ 渲染模板 agent.scene.marketing，变量**给全**（MarketingSceneVars）；渲染失败**回落代码兜底并告警**
//     —— 库里那一行坏了不能让整段营销要求凭空消失，而这一回落不报错，只有这条 Warn 看得见。
//   - ④ 由营销域按固定格式拼（SlotSummary）；取不到时写一句中性说明并告警，不中断本轮。
//
// visibleImages 是本次附给推理模型看的商品图张数（首条消息附图由 MKT-T-19 接入，之前恒为 0）。
func sceneSections(ctx context.Context, sc SceneContext, log *slog.Logger, sess repo.Session,
	state domain.State, model string, visibleImages int) (note, input string) {
	if domain.SceneHeading(sess.Scene) == "" {
		return "", ""
	}
	if log == nil {
		log = slog.Default()
	}
	if sc == nil {
		log.Warn("营销会话缺少场景上下文端口，场景段与营销输入留空", "sessionId", sess.ID)
		return "", ""
	}
	var si SceneInput
	siOK := true
	if len(sess.SceneInput) == 0 || json.Unmarshal(sess.SceneInput, &si) != nil {
		siOK = false
		log.Warn("营销会话的 scene_input 解析失败", "sessionId", sess.ID)
	}

	vars := MarketingSceneVars(state, model, sc.MaxVariants(), len(si.Products))
	note, err := sc.RenderScenePrompt(ctx, MarketingSceneTemplate, model, vars)
	if err != nil {
		log.Warn("营销场景段模板渲染失败，回落代码兜底", "sessionId", sess.ID,
			"template", MarketingSceneTemplate, "model", model, "error", err)
		if note, err = sc.FallbackScenePrompt(MarketingSceneTemplate, vars); err != nil {
			log.Error("营销场景段代码兜底也渲染失败，场景段留空", "sessionId", sess.ID, "error", err)
			note = ""
		}
	}

	if !siOK {
		return note, sceneInputUnavailable
	}
	input, err = sc.MarketingSummary(ctx, sess.WorkspaceID, si, visibleImages)
	if err != nil {
		log.Warn("营销输入摘要生成失败", "sessionId", sess.ID, "error", err)
		return note, sceneInputUnavailable
	}
	return note, input
}
