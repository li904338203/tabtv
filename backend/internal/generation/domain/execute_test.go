package domain

import (
	"errors"
	"testing"
	"time"
)

// 终态不可再变：晚到的回调、重复消息、恢复扫描都会试图再写一次。
func TestTransition_终态不可覆盖(t *testing.T) {
	for _, from := range []string{StatusSuccess, StatusFailed, StatusCancelled} {
		for _, to := range []string{StatusQueued, StatusRunning, StatusSuccess, StatusFailed} {
			err := Transition(from, to)
			if from == to {
				if !errors.Is(err, ErrSameState) {
					t.Errorf("%s → %s 应判为同态重复", from, to)
				}
				continue
			}
			if !errors.Is(err, ErrTerminal) {
				t.Errorf("%s → %s 应被拒（终态不可变），实际 %v", from, to, err)
			}
		}
	}
}

func TestTransition_合法路径(t *testing.T) {
	ok := [][2]string{
		{StatusQueued, StatusRunning},
		{StatusQueued, StatusFailed}, // 受理后还没跑就被判超时/取消
		{StatusQueued, StatusCancelled},
		{StatusRunning, StatusSuccess},
		{StatusRunning, StatusFailed},
		{StatusRunning, StatusCancelled},
	}
	for _, c := range ok {
		if err := Transition(c[0], c[1]); err != nil {
			t.Errorf("%s → %s 应合法，实际 %v", c[0], c[1], err)
		}
	}
	// running 不能倒回 queued：重试是**新建一行**，不是把原记录改回去（GO-17）。
	if err := Transition(StatusRunning, StatusQueued); !errors.Is(err, ErrTransition) {
		t.Errorf("running → queued 应被拒，实际 %v", err)
	}
}

// 同态重复不是错误，是重复消息的正常表现；调用方据此静默 ack。
func TestTransition_同态重复可识别(t *testing.T) {
	if err := Transition(StatusRunning, StatusRunning); !errors.Is(err, ErrSameState) {
		t.Fatalf("应可识别为同态重复，实际 %v", err)
	}
}

// 分错一类在两个方向上都很糟：永久错误反复打上游，临时错误让用户白白失败。
func TestClassifyHTTP(t *testing.T) {
	transient := []int{408, 429, 500, 502, 503, 504}
	permanent := []int{400, 401, 403, 404, 409, 415, 422}
	for _, s := range transient {
		if got := ClassifyHTTP(s); got != RetryTransient {
			t.Errorf("HTTP %d 应可重试，实际 %s", s, got)
		}
	}
	for _, s := range permanent {
		if got := ClassifyHTTP(s); got != RetryPermanent {
			t.Errorf("HTTP %d 不该重试，实际 %s", s, got)
		}
	}
}

// 没有退避的重试在上游限流时是火上浇油。
func TestBackoffFor_指数退避且有上限(t *testing.T) {
	prev := time.Duration(0)
	for i := 1; i <= 10; i++ {
		d := BackoffFor(i)
		if d < prev {
			t.Errorf("第 %d 次退避 %v 小于上一次 %v", i, d, prev)
		}
		if d > 5*time.Minute {
			t.Errorf("第 %d 次退避 %v 超过上限", i, d)
		}
		prev = d
	}
	if BackoffFor(1) != 10*time.Second {
		t.Errorf("首次退避应为 10s，实际 %v", BackoffFor(1))
	}
	// 非法输入按首次处理，不能返回 0（0 等于没有退避）。
	if BackoffFor(0) != 10*time.Second || BackoffFor(-5) != 10*time.Second {
		t.Error("非法 attempt 应按首次退避处理")
	}
}

// 重发可能在上游创建第二个任务，用户被计费两次而我们只认得一个。
func TestResolveUncertain(t *testing.T) {
	retry, code := ResolveUncertain(true)
	if !retry || code != "" {
		t.Error("协议支持按幂等键反查时，应去查一次而不是判失败")
	}
	retry, code = ResolveUncertain(false)
	if retry {
		t.Fatal("不支持反查时**绝不能**重发 —— 那可能造成上游二次计费")
	}
	if code != ErrorCodeSubmitUncertain {
		t.Errorf("应标专门的错误码让运维捞得出来，实际 %q", code)
	}
}

// 用存下来的 deadline，不按当前配置重算。
func TestExpiredBy(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Second)
	future := now.Add(time.Second)

	if !ExpiredBy(&past, now) {
		t.Error("过了死线应判超时")
	}
	if ExpiredBy(&future, now) {
		t.Error("没到死线不该判超时")
	}
	if !ExpiredBy(&now, now) {
		t.Error("恰好到点应判超时")
	}
	// 排队中还没领取的任务可能没有死线，这不该被当成"已超时"。
	if ExpiredBy(nil, now) {
		t.Error("没有死线的任务不该被判超时")
	}
}
