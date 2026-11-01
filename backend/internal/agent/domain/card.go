package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// CardKind 是卡片类型（与 agent_card.kind 的 CHECK 约束逐字一致）。
type CardKind string

const (
	CardChoice        CardKind = "choice"
	CardQuestions     CardKind = "questions"
	CardPlanConfirm   CardKind = "plan_confirm"
	CardBudget        CardKind = "budget"
	CardDeleteConfirm CardKind = "delete_confirm"
)

// CardStatus 是 agent_card.status 的取值。
type CardStatus string

const (
	CardPending   CardStatus = "pending"
	CardSubmitted CardStatus = "submitted"
	CardExpired   CardStatus = "expired"
	CardVoided    CardStatus = "voided"
)

// Decision 是确认类卡片的决定。
type Decision string

const (
	DecisionApprove Decision = "approve"
	DecisionAdjust  Decision = "adjust"
	DecisionReject  Decision = "reject"
)

const (
	// MaxFreeTextRunes：「我有别的想法」与问答卡文本题的上限（按 Unicode 码点）。
	MaxFreeTextRunes = 500
	// MaxCommentRunes：「需要调整」修改意见的上限。
	MaxCommentRunes = 2000
)

// ErrInvalidSubmission 卡片提交不合法。具体原因在包装的错误文本里。
var ErrInvalidSubmission = errors.New("卡片提交不合法")

// Card 是校验所需的卡片快照（来自 agent_card 的 kind + payload）。
type Card struct {
	Kind    CardKind
	Payload json.RawMessage
}

// Option 是选择卡 / 问答卡的一个选项。
type Option struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
}

// ChoicePayload 是选择卡（ask_user 单选 / 多选）。
type ChoicePayload struct {
	Title   string   `json:"title"`
	Multi   bool     `json:"multi"`
	Options []Option `json:"options"`
	// AllowOther：是否有「我有别的想法」。
	AllowOther bool `json:"allowOther"`
	// ResumeState：发卡前的状态，提交后回到这里。
	ResumeState State `json:"resumeState"`
}

// QuestionType 是问答卡单题的类型。
type QuestionType string

const (
	QuestionSingle QuestionType = "single"
	QuestionMulti  QuestionType = "multi"
	QuestionText   QuestionType = "text"
)

// Question 是问答卡的一题。
type Question struct {
	ID         string       `json:"id"`
	Text       string       `json:"text"`
	Type       QuestionType `json:"type"`
	Options    []Option     `json:"options,omitempty"`
	AllowOther bool         `json:"allowOther,omitempty"`
}

// QuestionsPayload 是分页问答卡。
type QuestionsPayload struct {
	Title       string     `json:"title"`
	Questions   []Question `json:"questions"`
	ResumeState State      `json:"resumeState"`
}

// PlanItem 是规划确认卡的一个计划项。金额由服务端报价，不信模型。
type PlanItem struct {
	Kind      string `json:"kind"` // video | image | audio | render …
	ModelCode string `json:"modelCode,omitempty"`
	// OperationCode 是报价用的操作码（text2video / img2video / text2img …）。
	// 为空时按 Kind 取默认操作（见 wiring 的规划报价适配器）。
	// ⚠️ 批准时要按**同一组参数**重新报价（AGT-26），所以报价所需的一切都必须留在卡片上。
	OperationCode string `json:"operationCode,omitempty"`
	// Inputs 是按输入角色的计数（image / video / audio …），供能力矩阵判定；纯文生为空。
	Inputs   map[string]int `json:"inputs,omitempty"`
	Label    string         `json:"label"`
	Quantity int            `json:"quantity"`
	Unit     string         `json:"unit,omitempty"` // second | image | time
	Credits  int            `json:"credits"`
	// BlockedCode / BlockedReason：这一项此刻报不出价（模型下线、会员等级不够…），Credits=0。
	// UserFixable 说明是不是用户自己能解决（前端按它定语气，不匹配文案）。
	// ⚠️ 报价成功时三者必须清空（PlanQuoter 重报价会原样复制计划项）。
	BlockedCode   string `json:"blockedCode,omitempty"`
	BlockedReason string `json:"blockedReason,omitempty"`
	UserFixable   bool   `json:"userFixable,omitempty"`

	// ---- 以下只在营销会话的计划项上出现（MKT-T-20、MKD-9），全部 omitempty：
	// 通用会话的卡片 JSON 逐字不变（TestPlanItem_通用卡片逐字不变）。

	// Key 是条目键：执行时 generate 凭它取已批准的条目（MKT-T-22），确认卡换图凭它定位。
	Key string `json:"key,omitempty"`
	// Variant / VariantLabel：批量变体的序号与说明（上限校验在 MKT-T-27）。
	Variant      int    `json:"variant,omitempty"`
	VariantLabel string `json:"variantLabel,omitempty"`
	// ProductIndexes 选用的商品图序号（从 1 起，与「商品图 #n」一致），顺序即发给上游的参考图顺序；
	// ProductMediaIDs 是同序的媒体 ID（取自会话 scene_input.products，不另存别处的图）。
	ProductIndexes  []int    `json:"productIndexes,omitempty"`
	ProductMediaIDs []string `json:"productMediaIds,omitempty"`
	// ProductTotal 会话商品图总数；ImageLimit 本项最多能用几张商品图（模型 × 操作的 image 上限
	// 减去角色图占用；0 = 该操作不收图，所以用指针保留 0）。卡片据此显示「共 X 张，本项用 Y 张」。
	ProductTotal int  `json:"productTotal,omitempty"`
	ImageLimit   *int `json:"imageLimit,omitempty"`
	// Picks 本项用到的选项（钉住的版本 + 该版本名称，不含片段原文，MKD-5）。
	Picks []PlanPick `json:"picks,omitempty"`

	// ---- 素材裂变（MKT-T-28、MKT-F-53）：以会话源视频为源的视频条目。
	// SourceVideoID 是源视频的 mediaId（取自 scene_input.sourceVideo，发卡时冻结）；
	// FissionRoute = video2video（源视频作为输入发给上游）/ regenerate（模型不能以视频为输入，
	// 按商品图与选项重新生成，不发源视频）；FissionReason 是走 regenerate 的原因；
	// FissionNote 是卡片上的说明（走哪条、花多少，服务端按报价写，见 FissionNote）。
	SourceVideoID string `json:"sourceVideoId,omitempty"`
	FissionRoute  string `json:"fissionRoute,omitempty"`
	FissionReason string `json:"fissionReason,omitempty"`
	FissionNote   string `json:"fissionNote,omitempty"`
}

// PlanPick 是计划项用到的一个营销选项：(kind, code, version) 永远指向同一段原文。
type PlanPick struct {
	Kind    string `json:"kind"`
	Code    string `json:"code"`
	Version int    `json:"version"`
	Name    string `json:"name,omitempty"`
}

// IsMarketing 这一项是不是营销条目（带了条目键）。
func (it PlanItem) IsMarketing() bool { return it.Key != "" }

// Available 这一项能不能报价（没有阻塞原因）。
func (it PlanItem) Available() bool { return it.BlockedCode == "" }

// PlanConfirmPayload 是规划确认卡（submit_plan）。
type PlanConfirmPayload struct {
	Title          string     `json:"title"`
	DocPath        string     `json:"docPath,omitempty"`
	PlanDocVersion int        `json:"planDocVersion,omitempty"`
	Items          []PlanItem `json:"items"`
	Total          int        `json:"total"`
	Balance        int        `json:"balance"`
	// Shortfall = max(0, Total − Balance)；CanApprove = 每项都可用 ∧ 余额够（design/01 §6.2）。
	Shortfall  int  `json:"shortfall"`
	CanApprove bool `json:"canApprove"`
	// Notices 是服务端追加的如实提示（如口播类选项的「不保证口型同步」，MKD-12）；不靠 Agent 写。
	Notices []string `json:"notices,omitempty"`
}

// PlanTotals 由逐项报价算合计、差额与能否批准。只算可用项的金额；
// 有任何一项不可用或余额不足都不能批准。溢出时返回 ok=false。
func PlanTotals(items []PlanItem, balance int) (total, shortfall int, canApprove, ok bool) {
	canApprove = len(items) > 0
	for _, it := range items {
		if !it.Available() {
			canApprove = false
			continue
		}
		if it.Credits < 0 || total > maxPlanCredits-it.Credits {
			return 0, 0, false, false
		}
		total += it.Credits
	}
	if balance < total {
		shortfall = total - balance
		canApprove = false
	}
	return total, shortfall, canApprove, true
}

// maxPlanCredits 与批准时 sumCredits 的上限一致（int32）。
const maxPlanCredits = 1<<31 - 1

// BudgetPayload 是追加额度卡。
type BudgetPayload struct {
	Reason    string `json:"reason"`
	Requested int    `json:"requested"`
	Approved  int    `json:"approved"`
	Used      int    `json:"used"`
	Refunded  int    `json:"refunded"`
}

// DeleteConfirmPayload 是删除确认卡（Agent 不能直接删用户节点）。
type DeleteConfirmPayload struct {
	Reason      string   `json:"reason"`
	NodeIDs     []string `json:"nodeIds"`
	ResumeState State    `json:"resumeState"`
}

// ChoiceSubmission 是选择卡的提交。Other=nil 表示没选「我有别的想法」。
type ChoiceSubmission struct {
	Selected []string `json:"selected"`
	Other    *string  `json:"other,omitempty"`
}

// Answer 是问答卡单题的回答。
type Answer struct {
	QuestionID string   `json:"questionId"`
	Selected   []string `json:"selected,omitempty"`
	Other      *string  `json:"other,omitempty"`
	Text       string   `json:"text,omitempty"`
}

// QuestionsSubmission 是问答卡的提交。
type QuestionsSubmission struct {
	Answers []Answer `json:"answers"`
}

// DecisionSubmission 是确认类卡片（规划确认 / 追加额度 / 删除确认）的提交。
type DecisionSubmission struct {
	Decision Decision `json:"decision"`
	Comment  string   `json:"comment,omitempty"`
	// ItemOverrides 确认前换图（只有规划确认卡的 approve 收；MKT-T-20）。服务端按覆盖重算
	// 商品图与输入计数后重新报价，合计变了按现有规则 409 + 新报价。
	ItemOverrides []ItemOverride `json:"itemOverrides,omitempty"`
}

// ItemOverride 是对一个营销条目的换图：按条目键定位，给出新的商品图序号（从 1 起，有序）。
type ItemOverride struct {
	Key            string `json:"key"`
	ProductIndexes []int  `json:"productIndexes"`
}

// allowedDecisions：每类确认卡能做的决定。
// ⚠️ adjust 只属于规划确认卡 —— 追加额度卡「调整」没有可回去的规划。
var allowedDecisions = map[CardKind][]Decision{
	CardPlanConfirm:   {DecisionApprove, DecisionAdjust, DecisionReject},
	CardBudget:        {DecisionApprove, DecisionReject},
	CardDeleteConfirm: {DecisionApprove, DecisionReject},
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w：%s", ErrInvalidSubmission, fmt.Sprintf(format, a...))
}

// decodeStrict 严格解码提交体：未知字段直接拒。
//
// ⚠️ encoding/json 默认忽略未知字段，键名写错时（`decison`）会被当成「什么都没选」，
// 而报出来的错是「必须选择一项」—— 指向用户而不是指向调用方（§5.10 的教训）。
func decodeStrict(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return invalid("提交内容格式错误：%v", err)
	}
	if dec.More() {
		return invalid("提交内容格式错误：多余的内容")
	}
	return nil
}

// ValidateSubmission 校验一次卡片提交是否合法。
//
//   - 选择卡：单选恰好一项、多选至少一项（「我有别的想法」算一项）；
//     「我有别的想法」文本非空且 ≤ 500 字；选项 ID 必须存在且不重复；
//   - 问答卡：每题都答，单选恰好一项、多选至少一项、文本题非空且 ≤ 500 字；
//   - 确认类卡：decision 在该卡允许的集合里；adjust 必须带修改意见。
func ValidateSubmission(card Card, payload json.RawMessage) error {
	switch card.Kind {
	case CardChoice:
		var p ChoicePayload
		if err := json.Unmarshal(card.Payload, &p); err != nil {
			return fmt.Errorf("卡片内容损坏：%w", err)
		}
		var s ChoiceSubmission
		if err := decodeStrict(payload, &s); err != nil {
			return err
		}
		return validatePick("", p.Options, p.Multi, p.AllowOther, s.Selected, s.Other)
	case CardQuestions:
		var p QuestionsPayload
		if err := json.Unmarshal(card.Payload, &p); err != nil {
			return fmt.Errorf("卡片内容损坏：%w", err)
		}
		var s QuestionsSubmission
		if err := decodeStrict(payload, &s); err != nil {
			return err
		}
		return validateAnswers(p.Questions, s.Answers)
	case CardPlanConfirm, CardBudget, CardDeleteConfirm:
		s, err := ParseDecision(card.Kind, payload)
		if err != nil || len(s.ItemOverrides) == 0 {
			return err
		}
		var p PlanConfirmPayload
		if err := json.Unmarshal(card.Payload, &p); err != nil {
			return fmt.Errorf("卡片内容损坏：%w", err)
		}
		return validateOverrides(p.Items, s.ItemOverrides)
	}
	return invalid("未知卡片类型 %q", card.Kind)
}

// validateOverrides 校验换图：条目键存在且是营销条目、同一条目不重复覆盖、序号在 1..商品图总数
// 且不重复、张数在 1..本项上限。
//
// ⚠️ 超上限是拒绝不是截断（MKT-F-48 ④）：截掉哪几张用户不知道，批准的就和他选的不一样。
func validateOverrides(items []PlanItem, ovs []ItemOverride) error {
	byKey := make(map[string]PlanItem, len(items))
	for _, it := range items {
		if it.Key != "" {
			byKey[it.Key] = it
		}
	}
	seen := make(map[string]bool, len(ovs))
	for _, o := range ovs {
		it, ok := byKey[o.Key]
		if !ok {
			return invalid("换图的条目 %q 不在这张卡片上", o.Key)
		}
		if seen[o.Key] {
			return invalid("条目 %q 重复换图", o.Key)
		}
		seen[o.Key] = true
		if it.ProductTotal <= 0 || it.ImageLimit == nil {
			return invalid("条目 %q 不使用商品图，不能换图", o.Key)
		}
		if len(o.ProductIndexes) == 0 {
			return invalid("条目 %q 至少选择一张商品图", o.Key)
		}
		if _, err := SelectProductIndexes(it.ProductTotal, o.ProductIndexes, *it.ImageLimit); err != nil {
			return invalid("条目 %q：%v", o.Key, err)
		}
	}
	return nil
}

// ParseDecision 解析并校验确认类卡片的提交。
func ParseDecision(kind CardKind, payload json.RawMessage) (DecisionSubmission, error) {
	allowedSet, ok := allowedDecisions[kind]
	if !ok {
		return DecisionSubmission{}, invalid("卡片类型 %q 不是确认类卡片", kind)
	}
	var s DecisionSubmission
	if err := decodeStrict(payload, &s); err != nil {
		return DecisionSubmission{}, err
	}
	found := false
	for _, d := range allowedSet {
		if d == s.Decision {
			found = true
			break
		}
	}
	if !found {
		return DecisionSubmission{}, invalid("该卡片不接受决定 %q", s.Decision)
	}
	// 换图只属于「批准规划」：带着它点「需要调整 / 不做了」是调用方的错，静默忽略的话
	// 前端以为换上了而其实什么都没发生。
	if len(s.ItemOverrides) > 0 && (kind != CardPlanConfirm || s.Decision != DecisionApprove) {
		return DecisionSubmission{}, invalid("itemOverrides 只能随规划确认卡的 approve 提交")
	}
	comment := strings.TrimSpace(s.Comment)
	if s.Decision == DecisionAdjust && comment == "" {
		return DecisionSubmission{}, invalid("选择「需要调整」时必须写修改意见")
	}
	if utf8.RuneCountInString(comment) > MaxCommentRunes {
		return DecisionSubmission{}, invalid("修改意见不能超过 %d 字", MaxCommentRunes)
	}
	s.Comment = comment
	return s, nil
}

func validatePick(prefix string, opts []Option, multi, allowOther bool,
	selected []string, other *string) error {
	known := make(map[string]bool, len(opts))
	for _, o := range opts {
		known[o.ID] = true
	}
	seen := make(map[string]bool, len(selected))
	for _, id := range selected {
		if !known[id] {
			return invalid("%s选项 %q 不存在", prefix, id)
		}
		if seen[id] {
			return invalid("%s选项 %q 重复", prefix, id)
		}
		seen[id] = true
	}
	count := len(selected)
	if other != nil {
		if !allowOther {
			return invalid("%s不支持「我有别的想法」", prefix)
		}
		if err := checkFreeText(prefix+"「我有别的想法」", *other); err != nil {
			return err
		}
		count++
	}
	if multi {
		if count < 1 {
			return invalid("%s至少选择一项", prefix)
		}
		return nil
	}
	if count != 1 {
		return invalid("%s必须恰好选择一项", prefix)
	}
	return nil
}

func checkFreeText(label, text string) error {
	t := strings.TrimSpace(text)
	if t == "" {
		return invalid("%s不能为空", label)
	}
	if utf8.RuneCountInString(t) > MaxFreeTextRunes {
		return invalid("%s不能超过 %d 字", label, MaxFreeTextRunes)
	}
	return nil
}

func validateAnswers(qs []Question, answers []Answer) error {
	byID := make(map[string]Answer, len(answers))
	for _, a := range answers {
		if _, dup := byID[a.QuestionID]; dup {
			return invalid("问题 %q 重复作答", a.QuestionID)
		}
		byID[a.QuestionID] = a
	}
	known := make(map[string]bool, len(qs))
	for i, q := range qs {
		known[q.ID] = true
		prefix := fmt.Sprintf("第 %d 题：", i+1)
		a, ok := byID[q.ID]
		if !ok {
			return invalid("%s还没有回答", prefix)
		}
		switch q.Type {
		case QuestionText:
			if len(a.Selected) > 0 || a.Other != nil {
				return invalid("%s文本题不接受选项", prefix)
			}
			if err := checkFreeText(prefix+"回答", a.Text); err != nil {
				return err
			}
		case QuestionSingle, QuestionMulti:
			if a.Text != "" {
				return invalid("%s选择题请用选项或「我有别的想法」作答", prefix)
			}
			if err := validatePick(prefix, q.Options, q.Type == QuestionMulti,
				q.AllowOther, a.Selected, a.Other); err != nil {
				return err
			}
		default:
			return fmt.Errorf("卡片内容损坏：%s未知题型 %q", prefix, q.Type)
		}
	}
	for id := range byID {
		if !known[id] {
			return invalid("问题 %q 不存在", id)
		}
	}
	return nil
}

// ResumeStateOf 取选择卡 / 问答卡 / 删除确认卡记录的发卡前状态。
func ResumeStateOf(card Card) (State, error) {
	var p struct {
		ResumeState State `json:"resumeState"`
	}
	if err := json.Unmarshal(card.Payload, &p); err != nil {
		return "", fmt.Errorf("卡片内容损坏：%w", err)
	}
	if _, err := InputResumeEvent(p.ResumeState); err != nil {
		return "", err
	}
	return p.ResumeState, nil
}

// SubmissionEvent 把一次**已校验**的卡片提交换成状态迁移事件。
//
//   - 选择 / 问答 / 删除确认卡 → 回到发卡前状态；
//   - 规划确认卡：approve → 执行、adjust → 规划（旧规划作废）、reject → idle；
//   - 追加额度卡：approve → 执行、reject → idle（拒绝后不再扣费，AG-E2E-04）。
func SubmissionEvent(card Card, payload json.RawMessage) (Event, error) {
	switch card.Kind {
	case CardChoice, CardQuestions:
		resume, err := ResumeStateOf(card)
		if err != nil {
			return "", err
		}
		return InputResumeEvent(resume)
	case CardDeleteConfirm:
		if _, err := ParseDecision(card.Kind, payload); err != nil {
			return "", err
		}
		resume, err := ResumeStateOf(card)
		if err != nil {
			return "", err
		}
		return InputResumeEvent(resume)
	case CardPlanConfirm, CardBudget:
		s, err := ParseDecision(card.Kind, payload)
		if err != nil {
			return "", err
		}
		switch s.Decision {
		case DecisionApprove:
			return EvApprove, nil
		case DecisionAdjust:
			return EvAdjust, nil
		default:
			return EvReject, nil
		}
	}
	return "", invalid("未知卡片类型 %q", card.Kind)
}
