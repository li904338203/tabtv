package domain

import (
	"errors"
	"strings"
)

// 作品审核（P13-02；AD-09）。
//
// ⚠️ **四个动作是四件事，不是一个字段的四个值**：
//
//	approved / rejected → 改 review_status（这是"有没有违规"）
//	featured/unfeatured → 改 is_featured（这是"值不值得展示"）
//	offline             → 改 is_offline（这是"现在撤下来"）
//
// 把它们挤进一个 `status` 列的话，"下架一件已加精的作品"就变成了
// 丢掉加精信息 —— 而重新上架时运营得凭记忆决定要不要再加精一次。

// ReviewAction 是一次审核动作，与 `ck_review_action` 一致。
type ReviewAction string

const (
	ActionApprove   ReviewAction = "approved"
	ActionReject    ReviewAction = "rejected"
	ActionOffline   ReviewAction = "offline"
	ActionFeature   ReviewAction = "featured"
	ActionUnfeature ReviewAction = "unfeatured"
)

func (a ReviewAction) Valid() bool {
	switch a {
	case ActionApprove, ActionReject, ActionOffline, ActionFeature, ActionUnfeature:
		return true
	}
	return false
}

var (
	ErrBadAction         = errors.New("审核动作非法")
	ErrReasonNeeded      = errors.New("驳回与下架必须填写原因")
	ErrReasonTooLong     = errors.New("原因最多 255 字")
	ErrFeatureUnapproved = errors.New("只有审核通过的作品才能加精")
	ErrAlreadyOffline    = errors.New("作品已下架")
)

// WorkState 是判定一次审核动作是否可执行所需的最小状态。
type WorkState struct {
	ReviewStatus string
	IsFeatured   bool
	IsOffline    bool
}

// ValidateReview 判定一次审核动作。
//
// ⚠️ **驳回与下架必须填原因**，通过与加精不必：
// 前两者是"我拦下了你的东西"，用户有权知道为什么（而且那条原因会**发给他**）；
// 后两者是放行，没有需要解释的东西。强制给通过也填原因的话，
// 运营会开始填"ok"——那时这个字段就彻底没用了。
//
// ⚠️ **加精要求已审核通过**：加精是公开发布决定，而公开可见性是
// `approved ∧ featured ∧ ¬offline`（CD-61）。允许给未通过的作品加精，
// 运营会以为自己发布了，而它根本不在公开墙上 —— 那种"点了没反应"
// 最难排查，因为每一步都"成功"了。
func ValidateReview(cur WorkState, action ReviewAction, reason string) error {
	if !action.Valid() {
		return ErrBadAction
	}
	reason = strings.TrimSpace(reason)
	if len([]rune(reason)) > 255 {
		return ErrReasonTooLong
	}
	switch action {
	case ActionReject, ActionOffline:
		if reason == "" {
			return ErrReasonNeeded
		}
	case ActionFeature:
		if cur.ReviewStatus != "approved" {
			return ErrFeatureUnapproved
		}
	}
	if action == ActionOffline && cur.IsOffline {
		// ⚠️ 重复下架报错而不是静默成功：静默成功会让"下架了但还看得到"
		// 这类问题无从分辨（到底是没生效，还是本来就已经下架了）。
		return ErrAlreadyOffline
	}
	return nil
}

// ApplyReview 算出一次动作之后的状态。**纯函数**，不碰库。
//
// ⚠️ 下架**不改 review_status**：下架与驳回是两件事 ——
// 驳回意味着作者要改，下架可能只是运营临时撤下（比如活动结束）。
// 混成一件的话，重新上架时那件作品会显示成"被驳回过"。
func ApplyReview(cur WorkState, action ReviewAction) WorkState {
	next := cur
	switch action {
	case ActionApprove:
		next.ReviewStatus = "approved"
		// ⚠️ 通过之后**不自动加精**：审核只回答"有没有违规"，
		// 加精是另一次显式决定（CD-61）。自动加精等于所有通过审核的作品
		// 都上了公开墙。
	case ActionReject:
		next.ReviewStatus = "rejected"
		// ⚠️ 驳回要**顺带取消加精**：一件已加精的作品被驳回后若仍留着
		// `is_featured`，运营一旦把它改回 approved 它就立刻重新公开了 ——
		// 而那不是他的本意（他只是想重新审）。
		next.IsFeatured = false
	case ActionOffline:
		next.IsOffline = true
	case ActionFeature:
		next.IsFeatured = true
		// ⚠️ 加精要**顺带撤销下架**：否则运营点了加精、作品仍然不可见，
		// 而界面上两个开关都是"开"的状态。
		next.IsOffline = false
	case ActionUnfeature:
		next.IsFeatured = false
	}
	return next
}

// NotifyOnReview 判定这次动作要不要通知作者，以及通知的类型。
//
// ⚠️ **加精/取消加精不通知**：那是运营的展示决定，与作者无关 ——
// 给他发"你的作品被取消加精了"只会让他以为自己做错了什么。
// 通过、驳回、下架都要通知：前者是好消息，后两者他必须知道原因。
func NotifyOnReview(action ReviewAction) (string, bool) {
	switch action {
	case ActionApprove:
		return "work_approved", true
	case ActionReject:
		return "work_rejected", true
	case ActionOffline:
		return "work_offline", true
	}
	return "", false
}

// ReviewRefID 是通知的幂等引用。
//
// ⚠️ **必须带动作**（`217:approved`），不能只用 workID：
// 先驳回后通过要发两条通知，只用 workID 的话第二条会被唯一索引挡掉 ——
// 作者永远收不到"已通过"，而他还以为自己的作品被拒了。
func ReviewRefID(workID string, action ReviewAction) string {
	return workID + ":" + string(action)
}
