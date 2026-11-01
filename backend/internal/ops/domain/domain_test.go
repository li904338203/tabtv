package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCanReplay_只有待处理的能重投(t *testing.T) {
	if err := CanReplay(DeadPending); err != nil {
		t.Fatalf("待处理应可重投：%v", err)
	}
	// ⚠️ 已重投的再投一次就是让同一条消息执行两遍 —— 而死信里有一类是
	// 「提交结果不确定，可能已在上游计费」，重投两次就是计两次费。
	for _, s := range []DeadStatus{DeadReplayed, DeadDiscarded} {
		if !errors.Is(CanReplay(s), ErrDeadHandled) {
			t.Fatalf("%s 不该能重投", s)
		}
	}
}

func TestCanDiscard_理由必填(t *testing.T) {
	if !errors.Is(CanDiscard(DeadPending, ""), ErrNoteRequired) {
		t.Fatal("丢弃不填理由应被拒")
	}
	if err := CanDiscard(DeadPending, "上游已人工补单"); err != nil {
		t.Fatalf("填了理由应放行：%v", err)
	}
	if !errors.Is(CanDiscard(DeadDiscarded, "再丢一次"), ErrDeadHandled) {
		t.Fatal("已丢弃的不该能再丢")
	}
}

func TestCheckCompensate_终态拒绝(t *testing.T) {
	now := time.Now()
	err := CheckCompensate(CompensateInput{
		Kind: KindGeneration, Finished: true, Reason: "上游挂了",
	}, now)
	if !errors.Is(err, ErrAlreadyFinished) {
		t.Fatalf("终态应拒绝，得到 %v", err)
	}
}

func TestCheckCompensate_原因必填(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	err := CheckCompensate(CompensateInput{
		Kind: KindGeneration, DeadlineAt: &past,
	}, time.Now())
	if !errors.Is(err, ErrReasonRequired) {
		t.Fatalf("原因必填，得到 %v", err)
	}
}

func TestCheckCompensate_未超时要force且说得出还差多久(t *testing.T) {
	now := time.Now()
	future := now.Add(30 * time.Minute)
	in := CompensateInput{Kind: KindGeneration, DeadlineAt: &future, Reason: "上游挂了"}

	err := CheckCompensate(in, now)
	if err == nil {
		t.Fatal("未超时且没有 force 时应拒绝")
	}
	// ⚠️ 要说清**还差多久** —— 一句"尚未超时"会让运维反复去点，
	// 而他并不知道该等多久。
	if !strings.Contains(err.Error(), "30 分钟") {
		t.Fatalf("应说清还差多久，得到 %q", err.Error())
	}

	in.Force = true
	if err := CheckCompensate(in, now); err != nil {
		t.Fatalf("带 force 应放行：%v", err)
	}
}

func TestCheckCompensate_已超时直接放行(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Minute)
	if err := CheckCompensate(CompensateInput{
		Kind: KindRender, DeadlineAt: &past, Reason: "ffmpeg 卡死",
	}, now); err != nil {
		t.Fatalf("已超时应放行：%v", err)
	}
}

func TestCheckCompensate_没有死线也要force(t *testing.T) {
	// 排队中还没派发的任务没有死线 —— 这不是"超时了"，
	// 判成超时会让刚提交的任务被误补偿。
	err := CheckCompensate(CompensateInput{
		Kind: KindGeneration, Reason: "队列积压",
	}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "force") {
		t.Fatalf("无死线时应要求 force，得到 %v", err)
	}
}

func TestCheckCompensate_只支持两类(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	// ⚠️ 整图运行由子任务推进（补偿单个节点才有意义）、
	// 打包导出不扣费（补偿它只是改个状态，没有账务可言）。
	for _, k := range []TaskKind{"workflow", "export", ""} {
		if !errors.Is(CheckCompensate(CompensateInput{
			Kind: k, DeadlineAt: &past, Reason: "x",
		}, time.Now()), ErrBadKind) {
			t.Fatalf("%q 不该支持补偿", k)
		}
	}
}

func TestSniffAggregate_认得出也认不出都不报错(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantType string
		wantID   int64
	}{
		{"正常信封", `{"aggregateType":"generation","aggregateId":"123"}`, "generation", 123},
		// ⚠️ 解不开是**正常结果**：`AckDead` 的第一种情形就是"消息解不开"，
		// 而那一条恰恰最需要人看 —— 报错的话它连库都进不去。
		{"根本不是 JSON", "这不是 JSON", "", 0},
		{"空消息", "", "", 0},
		// ⚠️ 认不出 ID 时**类型仍然保留**：知道"这是一条生成任务的消息、
		// 只是认不出是哪一条"也是有用的。
		{"ID 不是数字", `{"aggregateType":"render","aggregateId":"abc"}`, "render", 0},
		{"ID 缺失", `{"aggregateType":"render"}`, "render", 0},
		{"ID 溢出", `{"aggregateType":"render","aggregateId":"99999999999999999999999"}`, "render", 0},
		{"信封里没有这两个字段", `{"foo":1}`, "", 0},
	}
	for _, c := range cases {
		typ, id := SniffAggregate([]byte(c.body))
		if typ != c.wantType || id != c.wantID {
			t.Fatalf("%s: 得到 (%q,%d)，期望 (%q,%d)", c.name, typ, id, c.wantType, c.wantID)
		}
	}
}
