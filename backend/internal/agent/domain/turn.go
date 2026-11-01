package domain

// 轮次：计费单位（T-8，决策 D-1）。一轮 = 一次用户动作（发消息、提交卡片、批准、追加额度）
// 触发的连续运行，直到下一次挂起。refType=agent_turn，refID=turnID。

// TurnTrigger 是轮次的触发来源。
type TurnTrigger string

const (
	// TriggerUser 用户动作触发：计费。
	TriggerUser TurnTrigger = "user"
	// TriggerSystem 系统唤醒（所等任务终态后续跑）：**不另收费**，成本归入触发它的那一轮。
	TriggerSystem TurnTrigger = "system"
)

// Billable 报告该来源的轮次是否计费。
func (t TurnTrigger) Billable() bool { return t == TriggerUser }

// TurnStatus 是 agent_turn.status 的取值。
type TurnStatus string

const (
	TurnPending   TurnStatus = "pending"
	TurnRunning   TurnStatus = "running"
	TurnSuccess   TurnStatus = "success"
	TurnFailed    TurnStatus = "failed"
	TurnCancelled TurnStatus = "cancelled"
)

// IsTerminal 报告轮次状态是否终态。
func (s TurnStatus) IsTerminal() bool {
	return s == TurnSuccess || s == TurnFailed || s == TurnCancelled
}

// TurnSettle 是一轮结束时的结账决定（照 script/domain.SettleDecision 的形状）。
type TurnSettle struct {
	// Refund 为真表示**如果扣过费**就要全额退回。是否真的调用退款看 CallRefund。
	Refund bool
	Status TurnStatus
	Reason string
}

// CallRefund 报告是否真的要调用账本退款。
//
// ⚠️ 0 积分的轮（系统唤醒、免费模型）不能调 Refund：账本找不到原消费流水会返回
// ErrNotFound，于是一次正常的空回复被记成「退款失败」（dev/01 §4.2）。
func (s TurnSettle) CallRefund(charged int) bool { return s.Refund && charged > 0 }

// DecideTurnSettle 按「本轮有效输出数 + 运行错误」给出结账决定。
//
// 「有效输出」= 非空的助手文本，或至少一个 status=success 的工具步骤。
// 只算「对用户有用的东西」：一次失败的工具调用、一段空白回复都不算。
//
//   - 无有效输出且出错 → 全额退款 + failed；
//   - 无有效输出且没出错（模型空回复）→ 同样全额退款 + failed：让用户为一句空话付费更糟；
//   - 有有效输出 → success，**即使后半程出错也不退**：已经有产出了，退款等于白送。
//
// 扣费在运行前（预扣即消费，同脚本线），所以这里只决定退不退，不决定扣不扣。
func DecideTurnSettle(validOutputs int, err error) TurnSettle {
	if validOutputs > 0 {
		s := TurnSettle{Status: TurnSuccess}
		if err != nil {
			s.Reason = "本轮已有产出，后续出错不退款：" + err.Error()
		}
		return s
	}
	if err != nil {
		return TurnSettle{Refund: true, Status: TurnFailed, Reason: "本轮没有有效输出：" + err.Error()}
	}
	return TurnSettle{Refund: true, Status: TurnFailed, Reason: "模型空回复，本轮没有有效输出"}
}

// TopicTurnRequested 是「请 Worker 跑一轮」的 outbox topic。
//
// 放在 domain 而不是 app：repo 写 outbox、app 的消费者按它路由，两边都能引用同一个常量，
// 又不必让 repo 反向依赖 app。
const TopicTurnRequested = "agent.turn_requested"

// TurnRequestedPayload 是 TopicTurnRequested 的消息体。ID 一律字符串（同 generation outbox）。
type TurnRequestedPayload struct {
	SessionID     string `json:"sessionId"`
	TurnID        string `json:"turnId"`
	SchemaVersion int    `json:"schemaVersion"`
}
