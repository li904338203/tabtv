package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock 是自动推进的时钟：After(d) 立刻把时间往前拨 d 并触发 —— 50 秒的等待在测试里瞬间跑完，
// 而轮询与心跳的节奏完全按「时钟上的时间」走。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.now = c.now.Add(d)
	t := c.now
	c.mu.Unlock()
	ch := make(chan time.Time, 1)
	ch <- t
	return ch
}

type beat struct {
	progress, total float64
	msg             string
}

func withBeats(ctx context.Context) (context.Context, *[]beat) {
	var beats []beat
	return WithProgress(ctx, func(_ context.Context, p, t float64, m string) {
		beats = append(beats, beat{p, t, m})
	}), &beats
}

func hbText(state string, waited time.Duration) string { return state + "@" + waited.String() }

func TestWait_每3秒心跳且不超过上限(t *testing.T) {
	clk := &fakeClock{now: time.Unix(1000, 0)}
	ctx, beats := withBeats(context.Background())
	polls := 0
	out, err := Wait(ctx, WaitOptions{Max: 50 * time.Second, Clock: clk}, func(context.Context) (string, bool, error) {
		polls++
		return "working", false, nil
	}, hbText)
	if err != nil {
		t.Fatal(err)
	}
	if out.Terminal || out.State != "working" || out.Waited != 50*time.Second {
		t.Fatalf("到点应返回当前状态且恰好等满 50 秒：%+v", out)
	}
	// 每 1 秒一查：0、1、…、50 共 51 次。
	if polls != 51 || out.Polls != 51 {
		t.Fatalf("应每秒查一次（51 次），实际 %d", polls)
	}
	// 每 3 秒一次心跳：3、6、…、48 共 16 次；progress 单调递增、total 为上限秒数。
	if len(*beats) != 16 {
		t.Fatalf("应有 16 次心跳，实际 %d：%+v", len(*beats), *beats)
	}
	for i, b := range *beats {
		if b.progress != float64(3*(i+1)) || b.total != 50 || !strings.HasPrefix(b.msg, "working@") {
			t.Fatalf("第 %d 次心跳不对：%+v", i+1, b)
		}
	}
}

func TestWait_没有progressToken不发心跳(t *testing.T) {
	clk := &fakeClock{}
	out, err := Wait(context.Background(), WaitOptions{Max: 10 * time.Second, Clock: clk},
		func(context.Context) (string, bool, error) { return "working", false, nil }, hbText)
	if err != nil || out.Terminal {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestWait_终态立即返回(t *testing.T) {
	clk := &fakeClock{}
	ctx, beats := withBeats(context.Background())
	n := 0
	out, err := Wait(ctx, WaitOptions{Max: 50 * time.Second, Clock: clk}, func(context.Context) (string, bool, error) {
		n++
		if n == 5 {
			return "completed", true, nil
		}
		return "working", false, nil
	}, hbText)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Terminal || out.State != "completed" || out.Waited != 4*time.Second || n != 5 {
		t.Fatalf("第 5 次查到终态应立即返回（等了 4 秒）：%+v polls=%d", out, n)
	}
	if len(*beats) != 1 {
		t.Fatalf("4 秒内应只有 1 次心跳，实际 %d", len(*beats))
	}
}

func TestWait_到点返回当前状态不报失败(t *testing.T) {
	clk := &fakeClock{}
	out, err := Wait(context.Background(), WaitOptions{Max: 2500 * time.Millisecond, Clock: clk},
		func(context.Context) (string, bool, error) { return "working", false, nil }, hbText)
	if err != nil {
		t.Fatalf("到点不是错误：%v", err)
	}
	// 最后一步按剩余时长收窄：不会多等到 3 秒。
	if out.Terminal || out.State != "working" || out.Waited != 2500*time.Millisecond {
		t.Fatalf("%+v", out)
	}
	// 上限为 0（截止近在眼前）：只查一次就返回。
	out, err = Wait(context.Background(), WaitOptions{Max: 0, Clock: clk},
		func(context.Context) (string, bool, error) { return "working", false, nil }, hbText)
	if err != nil || out.Polls != 1 {
		t.Fatalf("Max=0 应只查一次：%+v %v", out, err)
	}
}

func TestWait_客户端断开即停止(t *testing.T) {
	clk := &fakeClock{}
	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	_, err := Wait(ctx, WaitOptions{Max: 50 * time.Second, Clock: clk}, func(context.Context) (string, bool, error) {
		n++
		if n == 3 {
			cancel()
		}
		return "working", false, nil
	}, hbText)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ctx 取消应返回 Canceled，实际 %v", err)
	}
	if n != 3 {
		t.Fatalf("断开后不应再查库，实际查了 %d 次", n)
	}
}

func TestWait_查询出错原样返回(t *testing.T) {
	boom := errors.New("not found")
	_, err := Wait(context.Background(), WaitOptions{Max: 50 * time.Second, Clock: &fakeClock{}},
		func(context.Context) (string, bool, error) { return "", false, boom }, hbText)
	if !errors.Is(err, boom) {
		t.Fatalf("%v", err)
	}
}

func TestEffectiveWaitMax_不超过上限与调用截止(t *testing.T) {
	now := time.Unix(1000, 0)
	bg := context.Background()
	cases := []struct {
		name           string
		deadline       time.Duration // 0 = 无截止
		requested, lim time.Duration
		want           time.Duration
	}{
		{"默认取上限", 0, 0, 50 * time.Second, 50 * time.Second},
		{"请求更短", 0, 10 * time.Second, 50 * time.Second, 10 * time.Second},
		{"请求更长被夹到上限", 0, 120 * time.Second, 50 * time.Second, 50 * time.Second},
		{"60 秒截止留 5 秒余量", 60 * time.Second, 0, 58 * time.Second, 55 * time.Second},
		{"截止已近", 3 * time.Second, 0, 50 * time.Second, 0},
		{"上限未配", 0, 0, 0, DefaultWaitMax},
	}
	for _, c := range cases {
		ctx := bg
		if c.deadline > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(bg, now.Add(c.deadline))
			defer cancel()
		}
		if got := EffectiveWaitMax(ctx, now, c.requested, c.lim); got != c.want {
			t.Errorf("%s：want %s got %s", c.name, c.want, got)
		}
	}
}
