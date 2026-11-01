// Package domain 是生成受理的纯规则层：幂等键、任务快照、死线。
package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

var (
	ErrIdempotencyKey = errors.New("幂等键非法")
	ErrSnapshot       = errors.New("任务快照非法")
)

// MaxIdempotencyKeyLen 与 `generation.idempotency_key` 的列宽一致。
const MaxIdempotencyKeyLen = 128

// ValidateIdempotencyKey 校验幂等键。
//
// ⚠️ **必填**且由客户端提供（GO-12）。服务端不能"没有就生成一个"——
// 那样每次重试都是新键，重试就变成了重复扣费，幂等等于没有。
func ValidateIdempotencyKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: 必须提供 Idempotency-Key", ErrIdempotencyKey)
	}
	if n := utf8.RuneCountInString(key); n > MaxIdempotencyKeyLen {
		return fmt.Errorf("%w: 长度 %d 超过上限 %d", ErrIdempotencyKey, n, MaxIdempotencyKeyLen)
	}
	for _, r := range key {
		// 限制字符集：幂等键会进日志、进 URL、进对账脚本，
		// 允许任意字节会让这些地方各自需要一套转义。
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == ':' || r == '.'
		if !ok {
			return fmt.Errorf("%w: 只允许字母、数字与 - _ : .", ErrIdempotencyKey)
		}
	}
	return nil
}

// Status 是任务状态。⚠️ 只有五态（CD-06）：reviewing 已移出。
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSuccess   = "success"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// Snapshot 是受理时冻结的任务快照。
//
// ⚠️ **一旦写入不再变更**：后台此后改模型、预设、素材、参数定义，
// 都不得改变这次任务的输入解释（06 §3.1）。因此快照里存的是
// **值**而不是引用 —— 存 modelCode 然后运行时再查一遍，就不是快照了。
type Snapshot struct {
	ModelCode     string `json:"modelCode"`
	ModelID       int64  `json:"modelId,string"`
	OperationCode string `json:"operationCode"`
	ProviderCode  string `json:"providerCode"`
	APIProtocol   string `json:"apiProtocol"`
	ResultType    string `json:"resultType"`
	// PriceUnit / PriceCredits 是**受理那一刻的价格**。
	// 不存的话，事后调价会让历史任务的费用无法解释。
	PriceUnit    string `json:"priceUnit"`
	PriceCredits int    `json:"priceCredits"`
	CreditsCost  int    `json:"creditsCost"`
	EstSeconds   int    `json:"estSeconds"`
	// PromptSuffix 是素材贡献的提示词，服务端拼好后冻结 ——
	// 素材下线或改词都不该影响已受理的任务。
	PromptSuffix string `json:"promptSuffix,omitempty"`
	// Origin 是任务的来源标识（见 OriginMarketingCharacter）；空 = 画布 / 脚本 / Agent 等常规来源。
	// ⚠️ omitempty：常规任务的快照必须与改动前逐字相等。
	Origin string `json:"origin,omitempty"`
}

// OriginMarketingCharacter 是营销角色库「生成角色」的来源标识（MKT-T-35、dev/05 MKD-28）。
//
// 这类任务**不属于任何项目**（`project_id = 0`），产物是候选图、要用户挑，所以：
//   - 受理时输入按工作区解析，不查项目（见 AllowsNoProject）；
//   - 成功后**不自动登记待审作品**（MKR-17：候选草图涌进审核队列，标题全是「未命名作品」）。
//
// 标识冻结在快照里（重试照抄），不靠 `project_id = 0` 反推：哪天出现第二种无项目任务，
// 「要不要登记作品」应由它自己的来源决定，而不是被这一条顺带排除。
const OriginMarketingCharacter = "marketing_character"

// AllowsNoProject 判断某个来源能不能以 `project_id = 0` 受理。
//
// ⚠️ 只有显式声明的来源才行：其余调用方传 0 是缺陷（漏传项目），必须照旧被拒（404），
// 不能悄悄变成一条「无项目任务」——那样它在画布上永远找不到落点。
func AllowsNoProject(origin string) bool { return origin == OriginMarketingCharacter }

// OriginSmartcut 是智能剪辑名下的任务（语音识别、片段级生成，docs/agent/smartcut SCD-32、SCD-33）。
//
// 这类任务属于项目、但不挂画布节点（`node_id` 为空，不顶掉节点的「最近任务」），完成后由 SmartcutBinder
// 回写到时间线或分析缓存；**不发站内通知**（剪辑台自己显示进度，一次识别十段素材不该收到十条消息），
// 状态事件照发（前端靠它刷新）。
const OriginSmartcut = "smartcut"

// ValidOrigin 判断来源标识是否认识（空 = 常规来源）。
func ValidOrigin(origin string) bool {
	return origin == "" || origin == OriginMarketingCharacter || origin == OriginSmartcut
}

// Notifies 判断这个来源的任务到终态时发不发站内通知。
func Notifies(origin string) bool { return origin != OriginSmartcut }

// BuildSnapshot 组装快照并校验必填项。
func BuildSnapshot(s Snapshot) (json.RawMessage, error) {
	switch {
	case s.ModelCode == "":
		return nil, fmt.Errorf("%w: 缺少模型", ErrSnapshot)
	case s.OperationCode == "":
		return nil, fmt.Errorf("%w: 缺少操作", ErrSnapshot)
	case s.ResultType == "":
		return nil, fmt.Errorf("%w: 缺少结果类型", ErrSnapshot)
	case s.CreditsCost < 0:
		return nil, fmt.Errorf("%w: 费用不能为负", ErrSnapshot)
	}
	return json.Marshal(s)
}

// MinDeadlineSeconds 死线下限。
//
// ⚠️ 死线是 `max(30, est×3)`：est 很小的模型（比如几秒的文本润色）
// 也要留出排队、网络与重试的余量，否则正常任务会被误判超时。
const MinDeadlineSeconds = 30

// DeadlineAt 计算任务死线并返回绝对时间。
//
// ⚠️ **必须在受理时算好并存下来**（GO-24）：恢复扫描不得按当前配置重算旧任务 ——
// 管理员调大 est 之后，一批本该判超时的僵尸任务会突然"又活了"，
// 调小则会把正在正常跑的任务一刀切掉。
func DeadlineAt(now time.Time, estSeconds int) time.Time {
	sec := estSeconds * 3
	if sec < MinDeadlineSeconds {
		sec = MinDeadlineSeconds
	}
	return now.Add(time.Duration(sec) * time.Second)
}

// OutboxTopic 是受理后要投递的消息主题。
const OutboxTopic = "generation.queued"

// OutboxPayload 是投递给 Worker 的消息体。
//
// ⚠️ 只带 ID 与必要的路由信息，**不带快照内容**：
// 消息可能在队列里待很久，带着快照等于把同一份数据存两处，
// 而 Worker 本来就要回库读最新状态（先查状态再执行，保证幂等）。
type OutboxPayload struct {
	GenerationID   string `json:"generationId"`
	WorkspaceID    string `json:"workspaceId"`
	PayerAccountID string `json:"payerAccountId"`
	OperationCode  string `json:"operationCode"`
	ModelCode      string `json:"modelCode"`
	SchemaVersion  int    `json:"schemaVersion"`
}
