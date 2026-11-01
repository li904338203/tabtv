package domain

import (
	"errors"
	"testing"
)

// ⚠️ 这张表是**绊线**：四类任务的状态词表由各域自己维护，
// 而任务中心是唯一一个必须认得全部的地方。新增一个状态词时，
// 要么在 AllRawStates 里加一行并补 NormalizeState，要么这条测试会红。
//
// 不统一就混在一个列表里的后果**很安静**：用户点"失败"筛选，
// 四类里有一类的状态词对不上，那一类就整体消失 —— 而列表看起来很正常。
func TestNormalizeState_覆盖每个域的每个取值(t *testing.T) {
	want := map[string]TaskState{
		"queued":          StateQueued,
		"pending":         StateQueued, // ⚠️ 只有整图运行用 pending
		"running":         StateRunning,
		"success":         StateSuccess,
		"partial_success": StatePartial,
		"failed":          StateFailed,
		"cancelled":       StateCancelled,
	}
	for kind, raws := range AllRawStates() {
		for _, raw := range raws {
			exp, ok := want[raw]
			if !ok {
				t.Fatalf("%s 的状态 %q 没有对应的统一状态 —— 新增状态词时要同时补映射", kind, raw)
			}
			if got := NormalizeState(kind, raw); got != exp {
				t.Errorf("%s/%s → %s，期望 %s", kind, raw, got, exp)
			}
		}
	}
}

// ⚠️ 未知值一律 failed：丢弃的话那条任务会从列表里消失（用户看不到自己花过的钱），
// 当成 running 的话界面会永远转圈（他一直等一个不会来的结果）。
func TestNormalizeState_未知值落failed(t *testing.T) {
	for _, raw := range []string{"", "unknown", "SUCCESS", "done"} {
		if got := NormalizeState(KindGeneration, raw); got != StateFailed {
			t.Errorf("%q 应落 failed，得到 %s", raw, got)
		}
	}
}

// ⚠️ partial_success **不能摊平**（AV-24）：显示成 success 用户不会去翻那条失败的，
// 显示成 failed 他会把已经成功的也重跑一遍 —— 两种都在花他的钱。
func TestPartialSuccess_不摊平(t *testing.T) {
	got := NormalizeState(KindWorkflow, "partial_success")
	if got == StateSuccess || got == StateFailed {
		t.Fatalf("partial_success 被摊平成了 %s", got)
	}
	// 而且它**是终态** —— 漏掉的话前端会一直轮询一个永远不再变的任务。
	if !TerminalState(StatePartial) {
		t.Fatal("partial_success 应是终态")
	}
}

func TestTerminalState(t *testing.T) {
	for _, s := range []TaskState{StateSuccess, StatePartial, StateFailed, StateCancelled} {
		if !TerminalState(s) {
			t.Errorf("%s 应是终态", s)
		}
	}
	for _, s := range []TaskState{StateQueued, StateRunning} {
		if TerminalState(s) {
			t.Errorf("%s 不该是终态", s)
		}
	}
}

// ⚠️ RawStatesFor 与 NormalizeState 必须**互为逆运算**：
// 筛选要下推到 SQL（取回来再过滤在分页下是错的 —— 取 20 条再过滤剩 3 条，
// 用户翻一页只看到 3 条而他以为那就是全部）。
func TestRawStatesFor_与映射互逆(t *testing.T) {
	for kind, raws := range AllRawStates() {
		for _, raw := range raws {
			unified := NormalizeState(kind, raw)
			back := RawStatesFor(unified)
			found := false
			for _, b := range back {
				if b == raw {
					found = true
				}
			}
			if !found {
				t.Errorf("%s/%s → %s，但 RawStatesFor(%s)=%v 里没有它",
					kind, raw, unified, unified, back)
			}
		}
	}
}

func TestParseState(t *testing.T) {
	if s, ok := ParseState(""); !ok || s != "" {
		t.Fatal("空串应表示不筛选")
	}
	for _, v := range []string{"queued", "running", "success", "partial_success",
		"failed", "cancelled"} {
		if _, ok := ParseState(v); !ok {
			t.Errorf("%q 应合法", v)
		}
	}
	for _, v := range []string{"pending", "done", "SUCCESS"} {
		// ⚠️ `pending` 是**原始**状态不是统一状态 —— 前端筛选用的是统一词表。
		if _, ok := ParseState(v); ok {
			t.Errorf("%q 不该作为筛选值", v)
		}
	}
}

func TestTaskKind(t *testing.T) {
	for _, k := range []TaskKind{KindGeneration, KindWorkflow, KindRender, KindExport} {
		if !k.Valid() {
			t.Errorf("%s 应合法", k)
		}
	}
	// 写作/数字人的任务类型不该存在（FULL-01）。
	for _, k := range []TaskKind{"writing", "avatar", ""} {
		if TaskKind(k).Valid() {
			t.Errorf("%q 不该合法", k)
		}
	}
}

// ---------------------------------------------------------------- 审核

func TestValidateReview(t *testing.T) {
	approved := WorkState{ReviewStatus: "approved"}
	pending := WorkState{ReviewStatus: "pending"}

	// ⚠️ 驳回与下架必须填原因（那条原因会**发给作者**）；
	// 通过与加精不必 —— 强制填的话运营会开始填"ok"，那时这个字段就没用了。
	if err := ValidateReview(pending, ActionReject, ""); !errors.Is(err, ErrReasonNeeded) {
		t.Fatalf("驳回应要求原因，得到 %v", err)
	}
	if err := ValidateReview(approved, ActionOffline, ""); !errors.Is(err, ErrReasonNeeded) {
		t.Fatalf("下架应要求原因，得到 %v", err)
	}
	if err := ValidateReview(pending, ActionApprove, ""); err != nil {
		t.Fatalf("通过不该要求原因：%v", err)
	}
	if err := ValidateReview(approved, ActionFeature, ""); err != nil {
		t.Fatalf("加精不该要求原因：%v", err)
	}

	// ⚠️ 未通过的作品不能加精：允许的话运营会以为自己发布了，
	// 而它根本不在公开墙上（公开要三条件）—— 那种"点了没反应"最难排查。
	if err := ValidateReview(pending, ActionFeature, ""); !errors.Is(err, ErrFeatureUnapproved) {
		t.Fatalf("未通过的不该能加精，得到 %v", err)
	}

	// ⚠️ 重复下架报错而不是静默成功：静默成功会让"下架了但还看得到"无从分辨。
	off := WorkState{ReviewStatus: "approved", IsOffline: true}
	if err := ValidateReview(off, ActionOffline, "违规"); !errors.Is(err, ErrAlreadyOffline) {
		t.Fatalf("重复下架应被拒，得到 %v", err)
	}

	if err := ValidateReview(pending, "banned", "x"); !errors.Is(err, ErrBadAction) {
		t.Fatalf("未知动作应被拒，得到 %v", err)
	}
	long := make([]rune, 256)
	for i := range long {
		long[i] = '字'
	}
	if err := ValidateReview(pending, ActionReject, string(long)); !errors.Is(err, ErrReasonTooLong) {
		t.Fatalf("超长原因应被拒，得到 %v", err)
	}
}

func TestApplyReview(t *testing.T) {
	// ⚠️ 通过之后**不自动加精**：自动加精等于所有通过审核的作品都上了公开墙。
	got := ApplyReview(WorkState{ReviewStatus: "pending"}, ActionApprove)
	if got.ReviewStatus != "approved" || got.IsFeatured {
		t.Fatalf("通过不该自动加精，得到 %+v", got)
	}

	// ⚠️ 驳回要顺带取消加精：留着 is_featured 的话，运营一旦把状态改回
	// approved 它就立刻重新公开了 —— 而那不是他的本意。
	got = ApplyReview(WorkState{ReviewStatus: "approved", IsFeatured: true}, ActionReject)
	if got.ReviewStatus != "rejected" || got.IsFeatured {
		t.Fatalf("驳回应顺带取消加精，得到 %+v", got)
	}

	// ⚠️ 下架**不改 review_status**：下架与驳回是两件事 ——
	// 混成一件的话，重新上架时那件作品会显示成"被驳回过"。
	got = ApplyReview(WorkState{ReviewStatus: "approved", IsFeatured: true}, ActionOffline)
	if got.ReviewStatus != "approved" || !got.IsFeatured || !got.IsOffline {
		t.Fatalf("下架只该改 is_offline，得到 %+v", got)
	}

	// ⚠️ 加精要顺带撤销下架：否则运营点了加精、作品仍然不可见，
	// 而界面上两个开关都是"开"的状态。
	got = ApplyReview(WorkState{ReviewStatus: "approved", IsOffline: true}, ActionFeature)
	if !got.IsFeatured || got.IsOffline {
		t.Fatalf("加精应顺带撤销下架，得到 %+v", got)
	}

	got = ApplyReview(WorkState{ReviewStatus: "approved", IsFeatured: true}, ActionUnfeature)
	if got.IsFeatured {
		t.Fatalf("取消加精应生效，得到 %+v", got)
	}
}

// ⚠️ 加精/取消加精**不通知作者**：那是运营的展示决定，与他无关 ——
// 发"你的作品被取消加精了"只会让他以为自己做错了什么。
func TestNotifyOnReview(t *testing.T) {
	for _, a := range []ReviewAction{ActionApprove, ActionReject, ActionOffline} {
		if _, ok := NotifyOnReview(a); !ok {
			t.Errorf("%s 应通知作者", a)
		}
	}
	for _, a := range []ReviewAction{ActionFeature, ActionUnfeature} {
		if _, ok := NotifyOnReview(a); ok {
			t.Errorf("%s 不该通知作者", a)
		}
	}
}

// ⚠️ 通知的幂等引用**必须带动作**：先驳回后通过要发两条，
// 只用 workID 的话第二条会被唯一索引挡掉 —— 作者永远收不到"已通过"。
func TestReviewRefID(t *testing.T) {
	a := ReviewRefID("217", ActionReject)
	b := ReviewRefID("217", ActionApprove)
	if a == b {
		t.Fatalf("不同动作应产生不同的幂等引用：%q", a)
	}
	if a != "217:rejected" {
		t.Fatalf("格式应是 workId:action，得到 %q", a)
	}
}
