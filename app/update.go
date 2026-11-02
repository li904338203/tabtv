package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/li904338203/tabtv/app/updater"
)

// 自动更新（TT-T-31）。检查与下载在宿主里做，不经后端路由：更新替换的是宿主程序本身。

// UpdateEventName 是「发现新版本」推给前端的事件名。
const UpdateEventName = "tabtv:update"

// UpdateInfo 是一次检查的结果（前端「关于」页与提示条用）。
type UpdateInfo struct {
	Current   string `json:"current"`
	Commit    string `json:"commit"`
	BuildTime string `json:"buildTime"`
	Latest    string `json:"latest"`
	Notes     string `json:"notes"`
	PageURL   string `json:"pageUrl"`
	Available bool   `json:"available"`
	// Mode：auto = 应用内下载安装；manual = 只能打开下载页。
	Mode string `json:"mode"`
	// Message 是给人看的说明（无法检查、已是最新、开发构建等）。
	Message string `json:"message"`
}

// UpdateConfig 由 main 注入。
type UpdateConfig struct {
	Current, Commit, BuildTime string
	Feed                       string // 空 = updater.DefaultFeed
	// Quit 走正常退出路径（TT-T-30：停后台、停库）。安装器 / 新版本在退出后接手。
	Quit func()
	// Emit 把发现新版本的消息推给前端。
	Emit func(UpdateInfo)
}

type updateState struct {
	mu      sync.Mutex
	cfg     UpdateConfig
	client  *updater.Client
	target  updater.Target
	latest  *updater.Release
	running bool
}

// ConfigureUpdate 注入更新配置。⚠️ 包级函数而不是 App 方法：App 的导出方法都会绑定给前端，函数类型无法序列化。
func ConfigureUpdate(a *App, cfg UpdateConfig) {
	a.update = &updateState{cfg: cfg, target: updater.Current(),
		client: &updater.Client{Feed: cfg.Feed, Current: cfg.Current}}
}

func (s *updateState) base() UpdateInfo {
	return UpdateInfo{Current: s.cfg.Current, Commit: s.cfg.Commit, BuildTime: s.cfg.BuildTime, Mode: string(s.target.Mode)}
}

func (s *updateState) check(ctx context.Context) UpdateInfo {
	info := s.base()
	// 开发构建不发任何请求：版本号不可比较，提示「升级」会覆盖掉开发者手上的程序。
	if _, ok := updater.ParseVersion(s.cfg.Current); !ok {
		info.Message = "开发构建，不检查更新"
		return info
	}
	r, err := s.client.Latest(ctx)
	switch {
	case errors.Is(err, updater.ErrUnavailable):
		info.Message = err.Error()
		return info
	case err != nil:
		info.Message = err.Error()
		return info
	}
	info.Latest, info.Notes, info.PageURL = r.Tag, r.Body, r.HTMLURL
	info.Available = updater.Newer(s.cfg.Current, r.Tag)
	if !info.Available {
		info.Message = "已是最新版本"
		return info
	}
	if s.target.Mode == updater.ModeAuto {
		if _, ok := r.Asset(updater.CurrentAssetName(r.Tag)); !ok {
			// 这个版本没有本平台的包：退回到「打开下载页」，而不是给一个点了必失败的安装按钮。
			info.Mode = string(updater.ModeManual)
		}
	}
	s.mu.Lock()
	s.latest = &r
	s.mu.Unlock()
	return info
}

// CheckUpdate 立即检查一次（「关于」页的按钮）。
func (a *App) CheckUpdate() UpdateInfo {
	if a.update == nil {
		return UpdateInfo{Message: "更新不可用"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return a.update.check(ctx)
}

// InstallUpdate 下载、校验并安装最近一次检查到的新版本，成功后退出应用（由新版本接手）。
// 返回空串表示已开始安装；否则是失败原因（当前版本照常运行）。
func (a *App) InstallUpdate() string {
	s := a.update
	if s == nil {
		return "更新不可用"
	}
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return "正在下载更新，请稍候"
	}
	r := s.latest
	s.running = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.running = false; s.mu.Unlock() }()
	if r == nil {
		return "请先检查更新"
	}
	if s.target.Mode != updater.ModeAuto {
		return "本机不支持应用内安装，请打开下载页手动更新"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	pkg, err := s.client.Download(ctx, *r, updater.CurrentAssetName(r.Tag), s.target.Dir)
	if err != nil {
		return err.Error()
	}
	if err := s.target.Apply(pkg); err != nil {
		return err.Error()
	}
	if s.cfg.Quit != nil {
		go s.cfg.Quit()
	}
	return ""
}

// RunUpdateLoop 启动 30 秒后检查一次，此后每 24 小时一次；autoCheck 为 false 时不发任何请求（TT-F-38）。
func RunUpdateLoop(ctx context.Context, a *App, autoCheck func() bool) {
	s := a.update
	if s == nil {
		return
	}
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if autoCheck() {
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			info := s.check(cctx)
			cancel()
			if info.Available && s.cfg.Emit != nil {
				s.cfg.Emit(info)
			}
		}
		// 失败也不加密重试：下个周期再查（匿名限流 60 次/小时/IP）。
		timer.Reset(24 * time.Hour)
	}
}
