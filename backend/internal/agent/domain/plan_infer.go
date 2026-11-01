package domain

// InferPlanItemKey 在营销会话的 generate 没带 planItemKey 时，找已批准计划里唯一对得上的条目
// （marketing dev/01 R-16，修订 MKT-T-22 的「planItemKey 必填」）。
//
// 起因：高频出现 —— 批准之后模型第一次调 generate 常常照通用会话的写法漏掉 planItemKey（批准消息只有一句
// 「确认规划，授权最多花费 ✦N」，执行时它看不到条目键），整条计划只有一个能执行的条目也被退回重来，白白多一轮。
//
// 候选必须同时满足：
//   - generate 能执行（render / storyboard 这类不归 generate 的除外，见 WrongToolFor）；
//   - 还没在跑、也没成功过（最近一次步骤不是 running / waiting / success）—— 否则一次漏了键的调用会把
//     已经出片的条目再生成一遍；失败 / 取消过的仍是候选（重试是正当的）；
//   - 与模型给了的 nodeType / modelCode / operationCode 相符（没给的不参与筛选）。
//
// 恰好一个才返回它的键；零个或多个返回空键，由调用方退回并把可选条目列出来。
// 推断只补「是哪一项」：之后照常按条目核对模型、操作、时长、商品图，不一致仍然退回。
func InferPlanItemKey(items []PlanItem, steps []PlanStep, nodeType, modelCode, operationCode string) (string, []PlanItem) {
	latest := latestByKey(steps)
	var out []PlanItem
	for _, it := range items {
		if it.Key == "" || WrongToolFor("", it.Kind) != "" {
			continue
		}
		switch latest[it.Key].Status {
		case "running", "waiting", "success":
			continue
		}
		if nodeType != "" && it.Kind != nodeType ||
			modelCode != "" && it.ModelCode != "" && it.ModelCode != modelCode ||
			operationCode != "" && it.OperationCode != "" && it.OperationCode != operationCode {
			continue
		}
		out = append(out, it)
	}
	if len(out) == 1 {
		return out[0].Key, out
	}
	return "", out
}
