package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// generate 的营销分支（MKT-T-22、dev/01 §3.5、MKD-9）：营销会话里付费的 generate 必须对应已批准的计划项。
//
//   - planItemKey 必填；取本会话**最近一张已批准**规划卡里的同键条目，找不到 → plan_item_not_approved。
//   - 模型给的 modelCode / operationCode / inputs / 时长 / 张数与条目不一致 → plan_item_mismatch
//     （不静默以哪一边为准）；省略则用条目的。
//   - inputs 由服务端组装：条目的商品图（按条目顺序）+ 角色，都以 reference 发；视频生视频条目
//     只发源视频（MKT-T-28）；
//     params.marketing = {picks:[{kind,code,version}]} 由服务端写入，覆盖模型给的同名键；节点标题 = 条目 label。
//   - 之后与通用会话同一条路（canvasgen：建节点 → 报价 → 受理 → 挂节点）。
//
// ⚠️ 全部校验在建节点、报价、扣费之前：拒绝没有任何副作用。
// ⚠️ 确认卡上的图就是发给上游的图（MKT-AC-26 ②⑤）：一致性由这里保证，不靠模型听话。
// ⚠️ 拒绝与失败都带错误码（agent_step.error_code）：停后续按码判，不按文案（MKT-T-27）。

// isMarketing 判断本次调用是不是营销会话。RunContext 带了场景就用它；没带时（调用方漏传）读会话 ——
// 宁可多一次查询，也不能让营销会话因为一个空字段绕过计划项约束。
func (t Generate) isMarketing(ctx context.Context, rc app.RunContext) (bool, error) {
	if rc.Scene != "" {
		return rc.Scene == domain.SceneMarketing, nil
	}
	if t.Plans == nil {
		return false, nil
	}
	sp, err := t.Plans.SessionPlan(ctx, rc.SessionID)
	if err != nil {
		return false, fmt.Errorf("读取会话失败：%w", err)
	}
	return sp.Scene == domain.SceneMarketing, nil
}

// marketingGenArgs 是营销分支额外读的参数（与 GenerateArgs 同一段 raw）。
type marketingGenArgs struct {
	PlanItemKey string `json:"planItemKey"`
}

func (t Generate) runMarketing(ctx context.Context, rc app.RunContext, raw json.RawMessage) (app.ToolResult, error) {
	var mk marketingGenArgs
	_ = json.Unmarshal(raw, &mk) // 形状错误由下面的 GenerateArgs 解析报
	key := strings.TrimSpace(mk.PlanItemKey)
	reject := func(code, msg string, details map[string]any) (app.ToolResult, error) {
		return app.ToolResult{}, &app.StepError{PlanItemKey: key, ErrorCode: code,
			Err: &domain.ToolRejection{Code: code, Message: msg, Details: details}}
	}
	if t.Plans == nil {
		return app.ToolResult{}, &app.StepError{PlanItemKey: key, ErrorCode: errcode.Internal.Name(),
			Err: errors.New("营销生成未接入，暂时无法提交生成")}
	}
	sp, err := t.Plans.SessionPlan(ctx, rc.SessionID)
	if err != nil {
		return app.ToolResult{}, &app.StepError{PlanItemKey: key, ErrorCode: errcode.Internal.Name(),
			Err: fmt.Errorf("读取已批准的规划失败：%w", err)}
	}
	approved := approvedKeys(sp)
	// notes 是服务端替模型做的决定（推断的条目键、忽略的占位输入），受理成功后写进回给它的结果（withNotes）。
	var notes []string
	if key == "" {
		// 没带 planItemKey（高频，R-16）：已批准计划里唯一对得上的条目就用它，不再退回重来；
		// 对不上或不唯一才退回，并把可选条目列出来（而不是叫它「先调整规划」—— 它只是漏了一个参数）。
		var hint struct {
			NodeType      string `json:"nodeType"`
			ModelCode     string `json:"modelCode"`
			OperationCode string `json:"operationCode"`
		}
		_ = json.Unmarshal(raw, &hint)
		k, _ := domain.InferPlanItemKey(sp.Items, sp.Steps, strings.TrimSpace(hint.NodeType),
			strings.TrimSpace(hint.ModelCode), strings.TrimSpace(hint.OperationCode))
		if k == "" {
			return reject(domain.ToolErrPlanItemNotApproved, missingKeyMessage(sp), map[string]any{"approvedKeys": approved})
		}
		key = k
		notes = append(notes, fmt.Sprintf("没带 planItemKey：已按唯一对得上的已批准条目 %s 执行。之后调用 generate 请带上 planItemKey。", k))
	}
	item, ok := findPlanItem(sp, key)
	if !ok {
		return reject(domain.ToolErrPlanItemNotApproved,
			fmt.Sprintf("未提交（没有建节点、没有扣费）：条目 %q 不在已批准的计划里，请先调整规划（用 submit_plan 重新规划并等用户确认）。", key),
			map[string]any{"item": key, "approvedKeys": approved})
	}
	// 渲染 / 分镜 / 提示词合成这类条目不归 generate（04 文档）：指路，而不是建一个节点再报价失败。
	if fix := domain.WrongToolFor("", item.Kind); fix != "" {
		return reject(domain.ToolErrWrongTool,
			fmt.Sprintf("未提交（没有建节点、没有扣费）：条目 %q 是「%s」，不用 generate，%s。", key, item.Label, fix),
			map[string]any{"item": key, "type": item.Kind})
	}

	// 停后续（MKT-T-27、MKT-F-51）：本计划里已有条目因余额不足失败 → 后续变体不受理、不扣费。
	// 按步骤的 error_code 现算（不按文案、不打标记）；被余额挡住的那一项自己可以重试。
	if by, stop := domain.ShouldStop(sp.Steps, key); stop {
		return reject(domain.ToolErrPlanStopped,
			fmt.Sprintf("余额不足，后续变体已停止（没有建节点、没有扣费）：条目 %q 因余额不足未能提交。"+
				"请告诉用户充值；充值后先重试 %q，成功后再继续其余变体。不要逐个尝试后续变体。", by, by),
			map[string]any{"item": key, "stoppedBy": by})
	}

	var a GenerateArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return app.ToolResult{}, &app.StepError{PlanItemKey: key, ErrorCode: errcode.InvalidArgument.Name(),
			Err: fmt.Errorf("参数不合法：%v", err)}
	}

	// 服务端组装的输入（营销条目：商品图 + 角色；视频生视频条目只有源视频）。非 video / image 条目不带商品图，输入沿用模型的。
	media := item.Kind == "video" || item.Kind == "image"
	var want []marketingInput
	if media {
		if want, err = t.marketingInputs(ctx, rc, sp.Input, item); err != nil {
			return app.ToolResult{}, &app.StepError{PlanItemKey: key, ErrorCode: stepErrorCode(err),
				Err: fmt.Errorf("条目 %q 的输入组装失败：%w", key, err)}
		}
	}

	// 冲突：模型给了而与条目不一致的字段一次报完。
	duration, count := planQuantity(item)
	var conflicts []map[string]any
	diff := func(field string, got, expected any) {
		conflicts = append(conflicts, map[string]any{"field": field, "got": got, "expected": expected})
	}
	if m := strings.TrimSpace(a.ModelCode); m != "" && item.ModelCode != "" && m != item.ModelCode {
		diff("modelCode", m, item.ModelCode)
	}
	if op := strings.TrimSpace(a.OperationCode); op != "" && item.OperationCode != "" && op != item.OperationCode {
		diff("operationCode", op, item.OperationCode)
	}
	if a.DurationSec > 0 && duration > 0 && a.DurationSec != duration {
		diff("durationSec", a.DurationSec, duration)
	}
	if a.Count > 0 && count > 0 && a.Count != count {
		diff("count", a.Count, count)
	}
	if media && len(a.Inputs) > 0 && string(a.Inputs) != "null" {
		got, ok := normalizeDeclaredInputs(a.Inputs)
		var unknown []string
		if ok {
			got, unknown = resolveInputRefs(got, sp.Input.Products, characterIDs(want, item),
				item.FissionRoute == domain.FissionVideo2Video)
		}
		switch {
		case ok && len(got) > 0 && len(unknown) == len(got):
			// 全是认不出的占位符（模型看不到真实 ID，只能编，R-16）：它没有表达任何「换一张图」的意图，
			// 按批准的条目发图并说一句。确认卡上的图仍然就是发给上游的图。
			notes = append(notes, fmt.Sprintf("inputs 里的 %s 不是本会话素材的 ID，已忽略，按批准的条目发图。"+
				"营销会话里不用传 inputs。", quoteAll(unknown)))
		case !ok || len(unknown) > 0 || !sameMarketingInputs(got, want):
			diff("inputs", describeInputs(got, ok), describeInputs(want, true))
		}
	}
	if len(conflicts) > 0 {
		names := make([]string, 0, len(conflicts))
		for _, c := range conflicts {
			names = append(names, c["field"].(string))
		}
		return reject(domain.ToolErrPlanItemMismatch,
			fmt.Sprintf("未提交（没有建节点、没有扣费）：%s 与已批准的条目 %q 不一致。营销会话里模型、操作、商品图与时长以用户确认的计划为准："+
				"省略这些参数（服务端按计划项填），或先调整规划。", strings.Join(names, " / "), key),
			map[string]any{"item": key, "conflicts": conflicts})
	}

	// 以条目为准改写参数，再走与通用会话同一个解析（节点类型推导、画布参数编码）。
	fields := map[string]json.RawMessage{}
	_ = json.Unmarshal(raw, &fields)
	delete(fields, "planItemKey")
	setField(fields, "modelCode", firstNonEmpty(item.ModelCode, strings.TrimSpace(a.ModelCode)))
	setField(fields, "operationCode", firstNonEmpty(item.OperationCode, strings.TrimSpace(a.OperationCode)))
	// 新建节点没给类型时用条目的产出类型（目录接入时仍按操作的产出类型核对，不一致照常报错）。
	if strings.TrimSpace(a.NodeID) == "" && strings.TrimSpace(a.NodeType) == "" && isNodeKind(item.Kind) {
		setField(fields, "nodeType", item.Kind)
	}
	if title := variantTitle(item); title != "" {
		setField(fields, "title", title)
	}
	if duration > 0 {
		setField(fields, "durationSec", duration)
	}
	if count > 0 {
		setField(fields, "count", count)
	}
	if media {
		setField(fields, "inputs", declaredInputs(want))
	}
	fixed, _ := json.Marshal(fields)
	a, params, err := parseGenerateArgs(ctx, fixed, t.Outputs)
	if err != nil {
		return app.ToolResult{}, &app.StepError{PlanItemKey: key, ErrorCode: errcode.InvalidArgument.Name(), Err: err}
	}
	// 画幅 / 清晰度：模型没给时按用户的选择（偏好或确认过的回答）→ 所选创意的推荐补（再没有就是节点默认，见 generate_defaults.go）。
	if a.NodeID == "" {
		params = fillGenerateDefaults(params, a.NodeType, t.creativeRatio(ctx, item), rc.Spec)
	}
	// 选项由服务端写（覆盖模型给的同名键）：只有 (kind, code, version)，不含名称与原文（MKD-5）。
	// 营销的 video / image 条目总写这一段（哪怕没选任何选项）：受理侧据此追加场景约束段（MKT-T-21）。
	delete(params, "marketing")
	if media {
		params["marketing"] = marketingParam(item.Picks)
	}
	// 批量变体（MKT-T-27）：新建节点按变体序落进这张计划的画框（在原节点上重跑的不挪）。
	// 放在全部校验之后：被拒的调用不能留下一个空画框。
	var place *variantPlacement
	if strings.TrimSpace(a.NodeID) == "" {
		if place = t.placeVariant(ctx, rc, sp, key); place != nil {
			b, _ := json.Marshal(map[string]float64{"x": place.X, "y": place.Y})
			_ = json.Unmarshal(b, &a.Position)
		}
	}
	res, err := t.submit(ctx, rc, a, params, key)
	// 受理失败（如余额不足）时节点照样保留，所以成败都挂进画框。
	t.attachVariant(ctx, rc, place, a.NodeID)
	if err == nil && len(notes) > 0 {
		res.Content = withNotes(res.Content, notes)
	}
	return res, err
}

// missingKeyMessage 是没带 planItemKey、又推断不出唯一条目时的退回文案：列出可选条目（键 + 名称 + 规格），
// 让模型补上键重试一次就好。⚠️ 不能说「先调整规划」—— 它只是漏了一个参数，照那句话去重新规划的话，
// 用户会再收到一张一模一样的确认卡。
func missingKeyMessage(sp app.SessionPlan) string {
	const head = "未提交（没有建节点、没有扣费）："
	if !sp.Approved {
		return head + "还没有已批准的规划。先用 submit_plan 提交规划并等用户确认，再调用 generate。"
	}
	_, pending := domain.InferPlanItemKey(sp.Items, sp.Steps, "", "", "")
	if len(pending) == 0 {
		return head + "已批准计划里能用 generate 的条目都已提交过，不需要再调用 generate；要再生成，先用 submit_plan 调整规划并等用户确认。"
	}
	lines := make([]string, 0, len(pending))
	for _, it := range pending {
		lines = append(lines, describePlanItem(it))
	}
	return head + "营销会话里 generate 要带 planItemKey（已批准计划里那一项的 key）。可选的条目：" +
		strings.Join(lines, "；") + "。带上对应的 planItemKey 重新调用即可：模型、生成方式、时长、商品图按条目填，不用再传。"
}

// describePlanItem 写成「item1「营销短片」（seedance_2_5 · ref2video · 15 秒）」。
func describePlanItem(it domain.PlanItem) string {
	var parts []string
	for _, s := range []string{it.ModelCode, it.OperationCode} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	if d, c := planQuantity(it); d > 0 {
		parts = append(parts, strconv.Itoa(d)+" 秒")
	} else if c > 0 {
		parts = append(parts, strconv.Itoa(c)+" 个")
	}
	s := it.Key + "「" + it.Label + "」"
	if len(parts) > 0 {
		s += "（" + strings.Join(parts, " · ") + "）"
	}
	return s
}

// withNotes 把服务端替模型做的决定写进回给它的结果：不说的话它不知道发生了什么，下一次还会照旧那样调用。
func withNotes(content json.RawMessage, notes []string) json.RawMessage {
	m := map[string]any{}
	if err := json.Unmarshal(content, &m); err != nil {
		return content
	}
	m["notes"] = notes
	b, err := json.Marshal(m)
	if err != nil {
		return content
	}
	return b
}

// approvedKeys 是已批准计划里的全部条目键（回给模型，让它知道能用哪些）。
func approvedKeys(sp app.SessionPlan) []string {
	out := []string{}
	if !sp.Approved {
		return out
	}
	for _, it := range sp.Items {
		if it.Key != "" {
			out = append(out, it.Key)
		}
	}
	return out
}

func findPlanItem(sp app.SessionPlan, key string) (domain.PlanItem, bool) {
	if !sp.Approved {
		return domain.PlanItem{}, false
	}
	for _, it := range sp.Items {
		if it.Key == key {
			return it, true
		}
	}
	return domain.PlanItem{}, false
}

// planQuantity 把条目的数量换成受理参数（与报价同一个映射：second → 时长，其余 → 张数 / 次数）。
func planQuantity(it domain.PlanItem) (durationSec, count int) {
	q := it.Quantity
	if q <= 0 {
		return 0, 0
	}
	if it.Unit == "second" {
		return q, 0
	}
	return 0, q
}

// marketingInput 是一条服务端组装的输入（都以 reference 发：商品图、角色图、源视频都是本工作区的媒体）。
type marketingInput struct {
	Kind string // reference / generation / text …（模型给的原样记，比较用）
	ID   string
}

// marketingInputs 组装条目的输入：商品图（条目顺序）→ 角色（会话顺序）。
//
// 素材裂变（MKT-T-28）：video2video 条目**只发源视频**（该操作恰好收 1 个视频）；重新生成的条目与
// 普通条目**不发源视频**。源视频取条目上冻结的 ID，并核对仍是会话的源视频（会话的 scene_input
// 建好后不变，对不上只可能是卡片被改过 —— 宁可失败也不发一条用户没确认过的视频）。
func (t Generate) marketingInputs(ctx context.Context, rc app.RunContext, si app.SceneInput,
	it domain.PlanItem) ([]marketingInput, error) {
	if it.FissionRoute == domain.FissionVideo2Video {
		v := si.SourceVideo
		if v == nil || v.Kind != app.RefMedia || v.ID == "" || v.ID != it.SourceVideoID {
			return nil, fmt.Errorf("源视频与已批准的条目不一致（条目 %q，会话 %v）", it.SourceVideoID, v)
		}
		return []marketingInput{{Kind: "reference", ID: it.SourceVideoID}}, nil
	}
	out := make([]marketingInput, 0, len(it.ProductMediaIDs)+len(si.Characters))
	for _, id := range it.ProductMediaIDs {
		out = append(out, marketingInput{Kind: "reference", ID: id})
	}
	for _, ref := range si.Characters {
		mid, err := t.Plans.CharacterMedia(ctx, rc.WorkspaceID, ref)
		if err != nil {
			return nil, err
		}
		out = append(out, marketingInput{Kind: "reference", ID: strconv.FormatInt(mid, 10)})
	}
	return out, nil
}

// declaredInputs 是交给受理的输入数组（order 从 1 起，即发给上游的参考图顺序）。
func declaredInputs(in []marketingInput) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for i, x := range in {
		out = append(out, map[string]any{"kind": x.Kind, "referenceId": x.ID, "order": i + 1})
	}
	return out
}

// normalizeDeclaredInputs 把模型给的 inputs 按 order 排好，抽出 (kind, 引用 ID)。形状不对返回 ok=false。
func normalizeDeclaredInputs(raw json.RawMessage) ([]marketingInput, bool) {
	var arr []struct {
		Kind         string          `json:"kind"`
		Order        int             `json:"order"`
		ReferenceID  json.RawMessage `json:"referenceId"`
		GenerationID json.RawMessage `json:"generationId"`
		Text         string          `json:"text"`
		MaterialCode string          `json:"materialCode"`
	}
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, false
	}
	sort.SliceStable(arr, func(i, j int) bool { return arr[i].Order < arr[j].Order })
	out := make([]marketingInput, 0, len(arr))
	for _, x := range arr {
		id := ""
		switch x.Kind {
		case "reference":
			id = rawID(x.ReferenceID)
		case "generation":
			id = rawID(x.GenerationID)
		case "material":
			id = x.MaterialCode
		default:
			id = x.Text
		}
		out = append(out, marketingInput{Kind: x.Kind, ID: id})
	}
	return out, true
}

// resolveInputRefs 把模型写的参考图引用换成真实 mediaId，并挑出认不出的占位符（R-16）。
//
// 模型看不到商品图 / 角色的真实 ID（mediaId 不进上下文，见 app.refsForModel），只能照序号去写，而写法五花八门：
// 会话 107440483873062912 写 "1"，会话 107492880150429696 写 "product_1" —— 逐个兼容写法是治标。所以按语义分三类：
//   - 序号：商品图 "1" / "#1" / "product_1" / "商品图1" / "image-2" / "第1张"……，角色 "character_1" / "角色1"……
//     → 换成会话里第 N 张商品图（规划选图用的是同一份序号，ApplyProductSelection）/ 第 N 个角色的图；
//   - 纯数字：真实 ID（或越界的序号），原样严格比对 —— 比不上说明模型真想换图，照常退回；
//   - 其余：认不出的占位符（"main_image"……），返回给调用方：全是占位符时按条目发图，混着真实引用时退回。
//
// 素材裂变（video2video）条目的输入是源视频，序号写法对它没有意义，一律不换算。
func resolveInputRefs(in []marketingInput, products, characters []string, fission bool) ([]marketingInput, []string) {
	out := make([]marketingInput, len(in))
	var unknown []string
	for i, x := range in {
		out[i] = x
		if x.Kind != "reference" {
			continue
		}
		if !fission {
			if n, ok := refOrdinal(x.ID, productRefPrefixes, false); ok && n <= len(products) {
				out[i].ID = products[n-1]
				continue
			}
			if n, ok := refOrdinal(x.ID, characterRefPrefixes, true); ok && n <= len(characters) {
				out[i].ID = characters[n-1]
				continue
			}
		}
		if isDigits(x.ID) {
			continue
		}
		unknown = append(unknown, x.ID)
	}
	return out, unknown
}

// 序号写法的前缀（小写比较；长的排前面，"商品图片" 要先于 "商品图"）。
var (
	productRefPrefixes   = []string{"products", "product", "商品图片", "商品图", "商品", "图片", "图", "images", "image", "img", "picture", "pic", "第", "no.", "#"}
	characterRefPrefixes = []string{"characters", "character", "char", "roles", "role", "角色", "人物"}
)

// refOrdinal 剥掉前缀（可以叠，如 "商品图#1"、"第1张"）后剩 1~3 位数字即为序号（从 1 起）。
// needPrefix 要求至少剥掉一个前缀：角色的序号必须写明是角色，否则一个越界的 "4" 会被当成第 4 个角色。
func refOrdinal(id string, prefixes []string, needPrefix bool) (int, bool) {
	s := strings.ToLower(strings.TrimSpace(id))
	stripped := false
	for {
		hit := false
		for _, p := range prefixes {
			if strings.HasPrefix(s, p) {
				s = strings.TrimLeft(s[len(p):], " _-#:：")
				stripped, hit = true, true
				break
			}
		}
		if !hit {
			break
		}
	}
	s = strings.TrimSpace(strings.TrimSuffix(s, "张"))
	if needPrefix && !stripped || s == "" || len(s) > 3 || !isDigits(s) {
		return 0, false
	}
	n, _ := strconv.Atoi(s)
	return n, n >= 1
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// characterIDs 是条目输入里角色那一段的 mediaId（marketingInputs 先排商品图、再排会话角色）。
func characterIDs(want []marketingInput, it domain.PlanItem) []string {
	if it.FissionRoute == domain.FissionVideo2Video || len(it.ProductMediaIDs) > len(want) {
		return nil
	}
	out := make([]string, 0, len(want)-len(it.ProductMediaIDs))
	for _, x := range want[len(it.ProductMediaIDs):] {
		out = append(out, x.ID)
	}
	return out
}

func quoteAll(ids []string) string {
	q := make([]string, len(ids))
	for i, s := range ids {
		q[i] = strconv.Quote(s)
	}
	return strings.Join(q, "、")
}

// rawID 收字符串与数字两种写法（"123" / 123）。
func rawID(r json.RawMessage) string {
	var s string
	if json.Unmarshal(r, &s) == nil {
		return strings.TrimSpace(s)
	}
	var n json.Number
	if json.Unmarshal(r, &n) == nil {
		return n.String()
	}
	return ""
}

func sameMarketingInputs(a, b []marketingInput) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// describeInputs 是冲突说明里的输入列表（"reference:123"）。
func describeInputs(in []marketingInput, ok bool) []string {
	if !ok {
		return []string{"（不是合法的输入数组）"}
	}
	out := make([]string, 0, len(in))
	for _, x := range in {
		out = append(out, x.Kind+":"+x.ID)
	}
	return out
}

// marketingParam 是 params.marketing：picks 只有 (kind, code, version)。
func marketingParam(picks []domain.PlanPick) map[string]any {
	list := make([]any, 0, len(picks))
	for _, p := range picks {
		list = append(list, map[string]any{"kind": p.Kind, "code": p.Code, "version": p.Version})
	}
	return map[string]any{"picks": list}
}

func setField(m map[string]json.RawMessage, k string, v any) {
	b, err := json.Marshal(v)
	if err == nil {
		m[k] = b
	}
}

func isNodeKind(k string) bool {
	return k == "image" || k == "video" || k == "audio" || k == "text"
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// stepErrorCode 是失败步骤的错误码：余额不足 → INSUFFICIENT_CREDITS；业务错误 → 错误码名；
// 其余 → INTERNAL。⚠️ 停后续按它判（MKT-T-27），不按文案。
func stepErrorCode(err error) string {
	var ins *app.InsufficientCreditsError
	if errors.As(err, &ins) {
		return errcode.InsufficientCredits.Name()
	}
	var rej *domain.ToolRejection
	if errors.As(err, &rej) {
		return rej.Code
	}
	var be *httpx.BizError
	if errors.As(err, &be) {
		if n := be.Code.Name(); n != "" {
			return n
		}
	}
	return errcode.Internal.Name()
}
