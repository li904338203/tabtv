package app

import (
	"context"
	"errors"
	"testing"
)

type guardProbe struct {
	active    int
	err       error
	always    bool
	choice    CloseChoice
	asked     int
	minimized int
	setAlways bool
}

func (p *guardProbe) guard() *CloseGuard {
	return &CloseGuard{
		Active:            func(context.Context) (int, error) { return p.active, p.err },
		AlwaysMinimize:    func() bool { return p.always },
		SetAlwaysMinimize: func(_ context.Context, on bool) { p.setAlways = on },
		Ask:               func(context.Context, int) CloseChoice { p.asked++; return p.choice },
		Minimize:          func(context.Context) { p.minimized++ },
	}
}

func TestBeforeClose_没有任务直接退出不询问(t *testing.T) {
	p := &guardProbe{}
	if p.guard().BeforeClose(context.Background()) || p.asked != 0 {
		t.Fatalf("没有任务应直接关：%+v", p)
	}
	// 查不到（数据库没起来）也直接关：那时没有任务能跑，拦下只会让人关不掉窗口。
	p = &guardProbe{active: 3, err: errors.New("db down")}
	if p.guard().BeforeClose(context.Background()) || p.asked != 0 {
		t.Fatalf("查询失败应直接关：%+v", p)
	}
}

func TestBeforeClose_有任务时按选择处理(t *testing.T) {
	cases := []struct {
		choice    CloseChoice
		prevent   bool
		minimized int
		setAlways bool
	}{
		{CloseMinimize, true, 1, false},
		{CloseMinimizeAlways, true, 1, true},
		{CloseCancel, true, 0, false},
		{CloseQuit, false, 0, false},
	}
	for _, c := range cases {
		p := &guardProbe{active: 2, choice: c.choice}
		g := p.guard()
		if got := g.BeforeClose(context.Background()); got != c.prevent || p.minimized != c.minimized || p.setAlways != c.setAlways || p.asked != 1 {
			t.Errorf("选择 %d：prevent=%v %+v", c.choice, got, p)
		}
	}
}

func TestBeforeClose_不再询问与强制退出(t *testing.T) {
	p := &guardProbe{active: 1, always: true}
	if !p.guard().BeforeClose(context.Background()) || p.asked != 0 || p.minimized != 1 {
		t.Fatalf("「总是最小化」应不询问直接最小化：%+v", p)
	}
	// 选了「仍然退出」之后 Wails 可能再触发一次关闭（macOS 的 Cmd+Q 与窗口关闭各一次），不能再问第二遍。
	p = &guardProbe{active: 1, choice: CloseQuit}
	g := p.guard()
	g.BeforeClose(context.Background())
	if g.BeforeClose(context.Background()) || p.asked != 1 {
		t.Fatalf("已确认退出后不应再问：%+v", p)
	}
	// 系统关机 / SIGTERM：不询问。
	p = &guardProbe{active: 5, choice: CloseCancel}
	g = p.guard()
	g.ForceQuit()
	if g.BeforeClose(context.Background()) || p.asked != 0 {
		t.Fatalf("强制退出不应询问：%+v", p)
	}
}
