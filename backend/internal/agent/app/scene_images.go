package app

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
)

// 推理模型看商品图（MKT-T-19，dev/01 MKD-18、§5.4）。
//
//   - 营销会话**首条用户消息**附商品图：按商品槽顺序取前 k 张，k = 推理模型在 agent_chat 上 image 角色的
//     上限（查不到按 0，退化为纯文字序号）；「## 营销输入」里「能看到 #1~#k」与实际附上的张数一致。
//   - view_media 按序号补看：Runner 在**下一次**模型调用时，紧跟那一批工具结果之后追加一条只进本次请求的
//     user 消息（Images），不落 agent_message。
//
// ⚠️ 图一律是服务端按 SEEAITV_MARKETING_REASONING_IMAGE_WIDTH 缩好的 **base64 内联**（data:image/...），
// 不发签名外链（MKT-AC-05：推理请求会进 provider_request_log 与上游日志，外链在有效期内拿到日志的人就能取图）。
// ⚠️ 任何一步取不到都只降级（少附图、只给序号），**不中断本轮**（MKR-7）。

// SceneImages 是给推理模型看商品图要问的事（实现在 wiring：目录域的能力矩阵 + 媒体域的缩略图）。
type SceneImages interface {
	// ReasoningImageLimit 推理模型在 agent_chat 上 image 角色的上限（与报价同一份能力矩阵）。
	// 查不到 / 没声明 / 不收图 → 0。
	ReasoningImageLimit(ctx context.Context, modelCode string) (int, error)
	// InlineImage 按**工作区**取一张图的缩略图并内联成 data: 地址。不是位图、取不到、过大都报错。
	InlineImage(ctx context.Context, workspaceID, mediaID int64) (string, error)
}

// viewImagePrefix 标在 view_media 临时消息前：协议里只有 user 消息能带图，用前缀说明这不是用户的发言。
const viewImagePrefix = SystemEventPrefix + "以下是你用 view_media 请求查看的图："

// pendingView 是一次 view_media 留给下一次模型调用的附图。
type pendingView struct {
	afterCallID string
	images      []string
	labels      []string
}

// sceneImageState 是一轮里缓存的看图状态：上限与首条消息的图每轮只取一次（一轮里会调好几次模型）。
type sceneImageState struct {
	loaded bool
	limit  int
	// first 首条用户消息附的图（#1 起连续；取不到某一张就停在那里，保证「#1~#k」连续成立）。
	first []string
	// pending 下一次模型调用要追加的临时附图。
	pending []pendingView
}

// loadSceneImages 取推理模型的看图上限与首条消息的图（营销会话、每轮一次）。通用会话什么都不做。
func (r *Runner) loadSceneImages(ctx context.Context, run *turnRun) {
	st := &run.images
	if st.loaded {
		return
	}
	st.loaded = true
	if run.sess.Scene != domain.SceneMarketing || r.d.Images == nil {
		return
	}
	limit, err := r.d.Images.ReasoningImageLimit(ctx, run.model)
	if err != nil {
		r.d.Logger.Warn("查询推理模型看图上限失败，退化为文字序号", "sessionId", run.sess.ID,
			"model", run.model, "error", err)
		return
	}
	if limit <= 0 {
		return
	}
	st.limit = limit
	var si SceneInput
	if len(run.sess.SceneInput) == 0 || json.Unmarshal(run.sess.SceneInput, &si) != nil {
		return
	}
	for i, idStr := range si.Products {
		if i >= limit {
			break
		}
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err == nil && id > 0 {
			var img string
			if img, err = r.d.Images.InlineImage(ctx, run.sess.WorkspaceID, id); err == nil {
				st.first = append(st.first, img)
				continue
			}
		}
		// 停在第一张取不到的：「能看到 #1~#k」必须连续成立，跳过中间一张会让模型把 #3 当成 #2。
		r.d.Logger.Warn("商品图取不到，首条消息只附前几张", "sessionId", run.sess.ID, "index", i+1,
			"mediaId", idStr, "error", err)
		break
	}
}

// firstUserKept 判断会话的首条用户消息是否在本次上下文里（没被截断），返回它的消息 ID。
//
// ⚠️ 与 BuildContext 用同一个截断函数：附图张数写进系统提示词（「能看到 #1~#k」），而图挂在消息上 ——
// 首条消息被截掉时两边都必须是 0，否则提示词说「能看到」而请求里一张图都没有。
func firstUserKept(msgs []repo.Message, hasOlder bool, budget int) (int64, bool) {
	if hasOlder {
		return 0, false // 更早的消息没取出来：首条用户消息不在手上
	}
	first := int64(0)
	for _, m := range msgs {
		if m.Role == repo.RoleUser {
			first = m.ID
			break
		}
	}
	if first == 0 {
		return 0, false
	}
	kept, _ := truncateMessages(msgs, budget)
	for _, m := range kept {
		if m.ID == first {
			return first, true
		}
	}
	return 0, false
}

// injectViewImages 把 view_media 的临时附图插进本次请求：每条紧跟**那一批**工具结果之后。
//
// ⚠️ 不能插在那条工具结果与同批其它工具结果之间：OpenAI 要求助手发起的每个 tool_call 的结果紧跟在它后面，
// 中间夹一条 user 消息整轮 400。Anthropic 侧连续的 user 内容会被协议层合并成一条（tool_result 块在前）。
// 找不到对应工具结果（理论上不会：它是刚写的最新消息）时追加在末尾。
func injectViewImages(msgs []ChatMessage, pend []pendingView) []ChatMessage {
	if len(pend) == 0 {
		return msgs
	}
	// 同一插入点的合并成一条。
	type group struct {
		images []string
		labels []string
	}
	at := map[int]*group{}
	var order []int
	for _, p := range pend {
		pos := len(msgs)
		for i, m := range msgs {
			if m.Role == "tool" && m.ToolCallID == p.afterCallID {
				j := i + 1
				for j < len(msgs) && msgs[j].Role == "tool" {
					j++
				}
				pos = j
				break
			}
		}
		g, ok := at[pos]
		if !ok {
			g = &group{}
			at[pos] = g
			order = append(order, pos)
		}
		g.images = append(g.images, p.images...)
		g.labels = append(g.labels, p.labels...)
	}
	out := make([]ChatMessage, 0, len(msgs)+len(order))
	for i := 0; i <= len(msgs); i++ {
		if g, ok := at[i]; ok {
			text := viewImagePrefix
			for k, l := range g.labels {
				if k > 0 {
					text += "、"
				}
				text += l
			}
			out = append(out, ChatMessage{Role: "user", Content: text, Images: g.images})
		}
		if i < len(msgs) {
			out = append(out, msgs[i])
		}
	}
	return out
}

// takeViewImages 收下一次 view_media 的附图。超过本次请求还能附的张数（推理模型上限）时拒收并返回说明。
func (r *Runner) takeViewImages(run *turnRun, callID string, res ToolResult) (ok bool, reason string) {
	st := &run.images
	if st.limit <= 0 {
		return false, "当前推理模型不支持看图，请根据商品名称、卖点与用户描述继续"
	}
	n := 0
	for _, p := range st.pending {
		n += len(p.images)
	}
	if n+len(res.AttachImages) > st.limit {
		return false, "一次最多查看 " + strconv.Itoa(st.limit) + " 张，请先基于已看到的图继续，需要时下一步再看"
	}
	var labels []string
	var v struct {
		Label string `json:"label"`
	}
	if json.Unmarshal(res.Content, &v) == nil && v.Label != "" {
		labels = []string{v.Label}
	}
	st.pending = append(st.pending, pendingView{afterCallID: callID, images: res.AttachImages, labels: labels})
	return true, ""
}
