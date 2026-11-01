// Command api 是 HTTP/WebSocket 服务进程。
//
// 职责（08 §2）：HTTP、WebSocket、认证、业务 command/query、事务入口、
// 签名下载 URL、管理接口。
// 不负责：直接等待 Provider 长任务；在 handler 里扣积分或写 SQL。
// 本进程**不启动** Provider consumer —— 那是 worker 的事。
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	agentrepo "github.com/li904338203/tabtv/backend/internal/agent/repo"
	agenthttp "github.com/li904338203/tabtv/backend/internal/agent/transport"
	billingapp "github.com/li904338203/tabtv/backend/internal/billing/app"
	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	billinghttp "github.com/li904338203/tabtv/backend/internal/billing/transport"
	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	canvasrepo "github.com/li904338203/tabtv/backend/internal/canvas/repo"
	canvashttp "github.com/li904338203/tabtv/backend/internal/canvas/transport"
	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	catalogrepo "github.com/li904338203/tabtv/backend/internal/catalog/repo"
	cataloghttp "github.com/li904338203/tabtv/backend/internal/catalog/transport"
	collabapp "github.com/li904338203/tabtv/backend/internal/collaboration/app"
	collabdomain "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	collabrepo "github.com/li904338203/tabtv/backend/internal/collaboration/repo"
	collabhttp "github.com/li904338203/tabtv/backend/internal/collaboration/transport"
	contentapp "github.com/li904338203/tabtv/backend/internal/content/app"
	contentrepo "github.com/li904338203/tabtv/backend/internal/content/repo"
	contenthttp "github.com/li904338203/tabtv/backend/internal/content/transport"
	directorapp "github.com/li904338203/tabtv/backend/internal/director/app"
	directorhttp "github.com/li904338203/tabtv/backend/internal/director/transport"
	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	genrepo "github.com/li904338203/tabtv/backend/internal/generation/repo"
	genhttp "github.com/li904338203/tabtv/backend/internal/generation/transport"
	identityapp "github.com/li904338203/tabtv/backend/internal/identity/app"
	"github.com/li904338203/tabtv/backend/internal/identity/channel"
	identityrepo "github.com/li904338203/tabtv/backend/internal/identity/repo"
	"github.com/li904338203/tabtv/backend/internal/identity/token"
	identityhttp "github.com/li904338203/tabtv/backend/internal/identity/transport"
	"github.com/li904338203/tabtv/backend/internal/identity/verify"
	marketingapp "github.com/li904338203/tabtv/backend/internal/marketing/app"
	marketingrepo "github.com/li904338203/tabtv/backend/internal/marketing/repo"
	marketinghttp "github.com/li904338203/tabtv/backend/internal/marketing/transport"
	mcpapp "github.com/li904338203/tabtv/backend/internal/mcp/app"
	mcpoauth "github.com/li904338203/tabtv/backend/internal/mcp/oauth"
	mcprepo "github.com/li904338203/tabtv/backend/internal/mcp/repo"
	mcptools "github.com/li904338203/tabtv/backend/internal/mcp/tools"
	mcphttp "github.com/li904338203/tabtv/backend/internal/mcp/transport"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	mediarepo "github.com/li904338203/tabtv/backend/internal/media/repo"
	mediahttp "github.com/li904338203/tabtv/backend/internal/media/transport"
	obsapp "github.com/li904338203/tabtv/backend/internal/observability/app"
	obsrepo "github.com/li904338203/tabtv/backend/internal/observability/repo"
	obshttp "github.com/li904338203/tabtv/backend/internal/observability/transport"
	opsapp "github.com/li904338203/tabtv/backend/internal/ops/app"
	opsdomain "github.com/li904338203/tabtv/backend/internal/ops/domain"
	opsrepo "github.com/li904338203/tabtv/backend/internal/ops/repo"
	opshttp "github.com/li904338203/tabtv/backend/internal/ops/transport"
	paymentapp "github.com/li904338203/tabtv/backend/internal/payment/app"
	paymentrepo "github.com/li904338203/tabtv/backend/internal/payment/repo"
	paymenthttp "github.com/li904338203/tabtv/backend/internal/payment/transport"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/platform/mq"
	providerrepo "github.com/li904338203/tabtv/backend/internal/provider/repo"
	providertransport "github.com/li904338203/tabtv/backend/internal/provider/transport"
	scriptapp "github.com/li904338203/tabtv/backend/internal/script/app"
	scriptrepo "github.com/li904338203/tabtv/backend/internal/script/repo"
	scripthttp "github.com/li904338203/tabtv/backend/internal/script/transport"
	smartcutapp "github.com/li904338203/tabtv/backend/internal/smartcut/app"
	smartcuthttp "github.com/li904338203/tabtv/backend/internal/smartcut/transport"
	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	timelinerepo "github.com/li904338203/tabtv/backend/internal/timeline/repo"
	timelinehttp "github.com/li904338203/tabtv/backend/internal/timeline/transport"
	workflowapp "github.com/li904338203/tabtv/backend/internal/workflow/app"
	workflowhttp "github.com/li904338203/tabtv/backend/internal/workflow/transport"

	"github.com/li904338203/tabtv/backend/internal/platform/bootstrap"
	"github.com/li904338203/tabtv/backend/internal/platform/buildinfo"
	"github.com/li904338203/tabtv/backend/internal/platform/cache"
	"github.com/li904338203/tabtv/backend/internal/platform/config"
	"github.com/li904338203/tabtv/backend/internal/platform/eventbus"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	plog "github.com/li904338203/tabtv/backend/internal/platform/log"
	"github.com/li904338203/tabtv/backend/internal/platform/migrate"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
	"github.com/li904338203/tabtv/backend/internal/transport/ws"
	"github.com/li904338203/tabtv/backend/internal/wiring"
	"github.com/li904338203/tabtv/backend/migrations"
)

func main() {
	if err := run(); err != nil {
		slog.Error("api 启动失败", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		// 配置非法时直接退出，不带着危险开关起服务（ADR-0001 §8.1）。
		return err
	}

	logger := plog.New(cfg.Log.Level, cfg.Log.Format)
	slog.SetDefault(logger)
	logger.Info("api 启动中",
		"env", cfg.Env,
		"addr", cfg.HTTPAddr,
		"version", buildinfo.Version,
	)

	// API 需要全部四个依赖：DB 与 Cache 服务请求，MQ 用于受理后投递，
	// 对象存储用于签发下载地址。
	deps, err := bootstrap.Open(context.Background(), cfg, bootstrap.Want{
		DB: true, Cache: true, MQ: true, ObjStore: true,
	}, logger)
	if err != nil {
		return err
	}
	defer deps.Close(logger)

	// Schema 版本闸（P14-04）。⚠️ 必须在建领域服务**之前** ——
	// 漏跑迁移时进程会照常起来、/healthz 照样 200，而每个碰到新列的请求都 500。
	if err := checkSchema(deps, logger); err != nil {
		return err
	}

	h, err := buildDomains(cfg, deps, logger)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           router(deps, h),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// 优雅关闭：收到信号后停止接收新连接，给在途请求留出时间。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	// 整图运行的接力派发 + 重启恢复（NEW-04、NEW-05）。
	//
	// ⚠️ 两件事是同一个动作：Worker 那侧只推进节点状态，下一批由这里接手；
	// 进程重启后内存里什么都没有，唯一可靠的事实是库里的节点状态 ——
	// 所以"接着派发"与"恢复"跑的是同一段代码，不会出现
	// "平时靠内存队列、重启才走恢复"那种两套路径不一致的情况。
	if h.workflowTick != nil {
		go workflowTicker(ctx, h.workflowTick, logger)
	}
	if h.scriptRecover != nil {
		go scriptRecoveryTicker(ctx, h.scriptRecover, logger)
	}
	if h.agentRecover != nil {
		go optimizeRecoveryTicker(ctx, h.agentRecover, logger)
	}
	if h.directorRecover != nil {
		go directorRecoveryTicker(ctx, h.directorRecover, logger)
	}
	if h.smartcutRecover != nil {
		go smartcutRecoveryTicker(ctx, h.smartcutRecover, logger)
		go smartcutTranscribeTicker(ctx, h.smartcutRecover, logger)
	}
	// MCP 调用审计的写协程（MCPT-06）。⚠️ 不拿信号 ctx：收到退出信号时还有在途的工具调用，
	// 它们的审计要在 srv.Shutdown 之后由 Close 排空写完，而不是随信号一起丢掉。
	if h.mcpCalls != nil {
		go h.mcpCalls.Run(context.Background())
	}
	logger.Info("api 已就绪", "addr", cfg.HTTPAddr)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("收到退出信号，开始优雅关闭")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		err := srv.Shutdown(shutdownCtx)
		h.mcpCalls.Close() // nil 安全；在途请求都结束后再排空审计缓冲
		return err
	}
}

// workflowTicker 周期性地把还在跑的整图运行推一把。
//
// ⚠️ 间隔按**节点耗时**定，不按"越快越好"：一个节点动辄几十秒，
// 2 秒的派发延迟无关紧要，而把间隔压到 100ms 只会让库被空转的扫描刷屏。
// ⚠️ 多个 API 实例同时扫也安全：抢占是 CAS 的（ClaimNode），
// 同一个节点只有一个实例能抢到。
func workflowTicker(ctx context.Context, svc *workflowapp.Service, logger *slog.Logger) {
	const interval = 2 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := svc.Tick(ctx, 50); err != nil {
				logger.Warn("整图运行扫描失败", "error", err)
			}
		}
	}
}

// scriptRecoveryTicker 周期性地把停在 running 太久的同步模型调用判失败并退款（启动时先跑一次）。
//
// ⚠️ 判死阈值大于这类请求的上限（见 scriptapp.SyncLLMRequestTimeout），扫到的一定已经没有请求在等它了；
// 多个 API 实例同时扫也安全：退款幂等、判失败是 CAS。
func scriptRecoveryTicker(ctx context.Context, svc *scriptapp.Service, logger *slog.Logger) {
	run := func() {
		if n, err := svc.RecoverStaleCalls(ctx); err != nil {
			logger.Warn("同步模型调用恢复扫描失败", "error", err)
		} else if n > 0 {
			logger.Info("同步模型调用恢复扫描：已判失败并退款", "count", n)
		}
	}
	run()
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

// longRunningRoutes 是**在 API 进程里同步调用模型**的端点（分镜生成、提示词 AI 合成；06 §4.1，不经过 Worker），
// 以及**大文件上传**（3D 导演台的预演视频与泼溅文件）。
// ⚠️ 只匹配 POST；新增同类端点要加在这里，否则它会被全局 30 秒超时在模型写到一半时掐断。
var longRunningRoutes = []*regexp.Regexp{
	regexp.MustCompile(`^/api/v1/scripts/[^/]+/storyboard$`),
	regexp.MustCompile(`^/api/v1/scripts/[^/]+/prompts/compose$`),
	// 创造页一键优化提示词（AGT-43）：同步调推理模型。
	regexp.MustCompile(`^/api/v1/agent/prompt-optimize$`),
	// 3D 导演台的预演视频（≤ 50 MiB）与泼溅文件（≤ 100 MiB）上传（DRT-03）：100 MiB 在 10 Mbps 下要 80 秒，
	// 30 秒的上限会在请求体还没读完时就让存储调用拿到一个已经过期的 ctx。截图（≤ 10 MiB 的 PNG）不放宽。
	regexp.MustCompile(`^/api/v1/projects/[^/]+/director-scenes/[^/]+/(previews|assets)$`),
	// 3D 导演台 AI 搭景（DRT-19）：同步调看图模型（单次上限 3 分钟，由服务自己先触发、带上准确的错误）。
	regexp.MustCompile(`^/api/v1/projects/[^/]+/director-scenes/[^/]+/build$`),
	// 智能剪辑的 AI 剪辑、方案切换、追加指令、字幕翻译（SCT-27 / 29 / 36）：同步调文本模型，不合规时还要再调一次修复。
	regexp.MustCompile(`^/api/v1/projects/[^/]+/smartcuts/[^/]+/(plan|edit|translate)$`),
	regexp.MustCompile(`^/api/v1/projects/[^/]+/smartcuts/[^/]+/plan/[^/]+/apply$`),
}

// mcpRoute 是 MCP 端点（/mcp、/mcp/readonly、/mcp/x/{toolset}）：**所有方法**整体跳过全局超时，
// 也不进 longRunningRoutes（开发 01 §12 R20 ③）。
//
// ⚠️ 30 秒外壳会在流中途写 504：客户端拿不到结果而 handler 继续空跑；2025 会话的 GET SSE
// 与 2026 的 subscriptions/listen 是合法长连接，会被每 30 秒掐一次（MCPT-01 Q4 实测）。
// 单次 tools/call 的上限由 MCP 自己的接收中间件给（mcphttp.DefaultCallTimeout），
// 空闲会话由 SessionTimeout 回收。只匹配根上的 /mcp，`/api/v1/...` 下的 MCP 管理接口照常 30 秒。
var mcpRoute = regexp.MustCompile(`^/mcp(/|$)`)

// directorRecoveryTicker 是 AI 搭景的恢复扫描（docs/agent/director，DRT-19）：每 5 分钟、启动时先跑一次，
// 把「进行中」超过判死阈值的调用判失败（释放并发额度；免费所以没有退款），顺带分批删掉超过保留期的调用记录。
func directorRecoveryTicker(ctx context.Context, svc *directorapp.Service, logger *slog.Logger) {
	run := func() {
		if n, err := svc.RecoverStaleCalls(ctx); err != nil {
			logger.Warn("AI 搭景恢复扫描失败", "error", err)
		} else if n > 0 {
			logger.Info("AI 搭景恢复扫描：已判失败", "count", n)
		}
		if n, err := svc.CleanupCalls(ctx); err != nil {
			logger.Warn("AI 搭景调用记录清理失败", "error", err)
		} else if n > 0 {
			logger.Info("AI 搭景调用记录清理", "deleted", n)
		}
	}
	run()
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

// smartcutTranscribeTicker 是语音识别的续接扫描（docs/agent/smartcut SCT-33）：识别用音轨抽好之后替用户受理识别。
//
// 每 3 秒一次：用户点完「识别」就在等，音轨通常几秒内就抽好；扫描本身只查 queued 且没挂任务的识别行（走部分索引），
// 没有等着的时候几乎不花钱。多个 API 实例同时扫也安全（逐行领取 + 受理幂等）。
func smartcutTranscribeTicker(ctx context.Context, svc *smartcutapp.Service, logger *slog.Logger) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n, err := svc.ResumePendingTranscribes(ctx); err != nil {
				logger.Warn("语音识别续接扫描失败", "error", err)
			} else if n > 0 {
				logger.Info("语音识别续接扫描", "handled", n)
			}
		}
	}
}

// smartcutRecoveryTicker 是 AI 剪辑的恢复扫描（docs/agent/smartcut SCT-27）：每 5 分钟、启动时先跑一次，
// 把「进行中」超过判死阈值的调用判失败（释放并发额度；免费所以没有退款），顺带分批删掉超过保留期的调用记录。
func smartcutRecoveryTicker(ctx context.Context, svc *smartcutapp.Service, logger *slog.Logger) {
	run := func() {
		if n, err := svc.RecoverStaleCalls(ctx); err != nil {
			logger.Warn("AI 剪辑恢复扫描失败", "error", err)
		} else if n > 0 {
			logger.Info("AI 剪辑恢复扫描：已判失败", "count", n)
		}
		if n, err := svc.CleanupCalls(ctx); err != nil {
			logger.Warn("AI 剪辑调用记录清理失败", "error", err)
		} else if n > 0 {
			logger.Info("AI 剪辑调用记录清理", "deleted", n)
		}
	}
	run()
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

// optimizeRecoveryTicker 把停在 running 太久的「一键优化提示词」调用判失败并退款（AGT-43，启动时先跑一次）。
// 判死阈值见 agentapp.OptimizeStaleAfter（大于同步模型请求上限）；多实例同时扫也安全。
func optimizeRecoveryTicker(ctx context.Context, svc *agentapp.Service, logger *slog.Logger) {
	run := func() {
		if n, err := svc.RecoverStaleOptimizes(ctx); err != nil {
			logger.Warn("提示词优化恢复扫描失败", "error", err)
		} else if n > 0 {
			logger.Info("提示词优化恢复扫描：已判失败并退款", "count", n)
		}
	}
	run()
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

// requestTimeout 给请求设上限：同步调模型的端点用 long，其余用 normal。
//
// ⚠️ 不能在路由组里再挂一个更长的 middleware.Timeout 来"放宽"：context 的截止时间只会取更早的那个，
// 外层 30 秒先到期。所以在最外层按路径选一次。
func requestTimeout(normal, long time.Duration) func(http.Handler) http.Handler {
	short, extended := middleware.Timeout(normal), middleware.Timeout(long)
	return func(next http.Handler) http.Handler {
		shortH, longH := short(next), extended(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if mcpRoute.MatchString(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			if r.Method == http.MethodPost {
				for _, re := range longRunningRoutes {
					if re.MatchString(r.URL.Path) {
						longH.ServeHTTP(w, r)
						return
					}
				}
			}
			shortH.ServeHTTP(w, r)
		})
	}
}

// router 目前只挂健康检查。
// 中间件顺序按 08 §4.1：request id → access log → CORS/body limit/timeout
// → JWT → rate limit → handler → error envelope。
// 认证、限流、错误信封在 P01-04/P02 补齐，这里先放前三段。
// buildDomains 装配身份域与协作域。
// handlers 收拢各域的 HTTP 处理器。
//
// ⚠️ 刻意用结构体而不是多返回值：每加一个领域，元组式签名都要同步改掉
// 函数里每一处 `return nil, nil, ..., err`，漏一个就是编译错误堆在无关的行上。
type handlers struct {
	identity      *identityhttp.Handler
	collab        *collabhttp.Handler
	catalog       *cataloghttp.Handler
	media         *mediahttp.Handler
	canvas        *canvashttp.Handler
	quickGen      *canvashttp.QuickHandler
	billing       *billinghttp.Handler
	billingAdmin  *billinghttp.AdminHandler
	payment       *paymenthttp.Handler
	paymentAdmin  *paymenthttp.AdminHandler
	gen           *genhttp.Handler
	script        *scripthttp.Handler
	workflow      *workflowhttp.Handler
	timeline      *timelinehttp.Handler
	timelineAdmin *timelinehttp.AdminHandler
	// smartcut 是智能剪辑节点的路由（docs/agent/smartcut SCT-07）。
	smartcut *smartcuthttp.Handler
	// smartcutRecover 是 AI 剪辑的恢复扫描与调用记录清理（SCT-27）。
	smartcutRecover *smartcutapp.Service
	content         *contenthttp.Handler
	contentAdmin    *contenthttp.AdminHandler
	ops             *opshttp.Handler
	obs             *obshttp.Handler
	// agent 是画布 Agent 的用户端路由（AGT-26）。
	agent *agenthttp.Handler
	// agentAdmin 是管理端技能配置（AGT-60）。
	agentAdmin *agenthttp.AdminHandler
	// marketingAdmin 是管理端营销选项与分类（MKT-T-03）。
	marketingAdmin *marketinghttp.AdminHandler
	// marketing 是用户端营销接口（MKT-T-04）：公开、不挂登录；总开关关闭时全部 404。
	marketing *marketinghttp.Handler
	// workflowTick 是整图运行的接力派发循环（在 API 进程里跑）。
	workflowTick *workflowapp.Service
	// scriptRecover 是同步模型调用的恢复扫描（停在 running 太久的判失败并退款，在 API 进程里跑）。
	scriptRecover *scriptapp.Service
	// agentRecover 是一键优化提示词的恢复扫描（AGT-43）；Agent 关闭时为 nil。
	agentRecover *agentapp.Service
	// director 是 3D 导演台的 AI 搭景接口（docs/agent/director，DRT-19）。
	director *directorhttp.Handler
	// directorRecover 是 AI 搭景的恢复扫描（进行中太久的判失败）与调用记录清理（保留 30 天）。
	directorRecover *directorapp.Service
	// mcp 是对外 MCP 服务（MCPT-02）；SEEAITV_MCP_ENABLED=false 时为 nil（不挂路由 → 404）。
	mcp *mcphttp.Handler
	// mcpKeys 是 /me/mcp/*（访问密钥与已连接的客户端，MCPT-03）；随 mcp 一起，关闭时为 nil。
	mcpKeys *mcphttp.KeysHandler
	// mcpCalls 是 MCP 调用审计的异步写入器（MCPT-06）；随 mcp 一起，关闭时为 nil。
	mcpCalls *mcpapp.CallLogger
	// mcpOAuth 是对外 MCP 的 OAuth 2.1 授权服务器（MCPT-12）；随 mcp 一起，关闭时为 nil。
	mcpOAuth *mcpoauth.Server
	// mcpWorkspace 是工作区管理员的「MCP 连接」与工作区开关（MCPT-14）；随 mcp 一起，关闭时为 nil。
	mcpWorkspace *mcphttp.WorkspaceHandler
	// mcpAdmin 是管理端「MCP 连接」（MCPT-14）：**总是挂**，环境变量关着时如实显示 envEnabled=false。
	mcpAdmin *mcphttp.AdminHandler
	// mcpSwitch 是平台运行时总开关（MCPT-14）；随 mcp 一起，关闭时为 nil。OAuth 端点也挂在它下面。
	mcpSwitch mcphttp.SwitchChecker
	// mcpApprovals 是页面审批 /mcp/approvals/*（MCPT-13）；随 mcp 一起，关闭时为 nil。
	mcpApprovals *mcphttp.ApprovalHandler
	ws           http.Handler
	// cleaner 由 API 持有但**不在 API 跑** —— 见 cmd/worker。
	// 这里只是为了让装配逻辑集中在一处。
	cleaner *canvasapp.Cleaner
}

func buildDomains(cfg config.Config, deps *bootstrap.Deps, logger *slog.Logger) (
	handlers, error) {
	jwtSecret := []byte(cfg.Auth.JWTSecret)
	if len(jwtSecret) < 32 {
		// 仅开发：生成一次性密钥。进程重启后旧 token 全部失效，这是预期行为。
		// production 下 config.Validate 已拒绝空密钥，不会走到这里。
		jwtSecret = make([]byte, 32)
		if _, err := rand.Read(jwtSecret); err != nil {
			return handlers{}, err
		}
		logger.Warn("未配置 SEEAITV_JWT_SECRET，已生成一次性开发密钥（重启后旧 token 失效）")
	}
	tokens, err := token.New(token.DefaultConfig(jwtSecret))
	if err != nil {
		return handlers{}, err
	}
	gen, err := id.New(0)
	if err != nil {
		return handlers{}, err
	}
	codes := verify.New(deps.Cache, deps.Cache, verify.DefaultConfig())
	// 真实 SMTP/短信凭据属 PARAM-03（受阻）。开发下先用日志渠道，
	// production 由 config 的 fail-fast 挡住（验证码进日志等于人人可登录）。
	senders := channel.NewRegistry(
		channel.LogSender{K: channel.Email, Logger: logger},
		channel.LogSender{K: channel.SMS, Logger: logger},
	)
	svc := identityapp.New(identityrepo.New(deps.DB.Pool), tokens, codes, senders, gen.Next)

	// hub 注入身份域：封禁与改密时要断开已建立的连接，
	// 只改数据库状态而不断连接，被封用户仍会持续收到事件推送（NEW-18）。
	hub := ws.NewHub()
	svc.SetRevoker(hub)

	// 密钥加密：用于认证配置里的 SMTP/短信凭据。
	// 开发下若未配置，回落到 JWT secret —— 仅为让本地可用，
	// production 必须显式配置独立的密钥材料。
	if cfg.Auth.SecretKey == "" {
		// 开发回落：用 JWT 密钥派生。production 由 config 强制要求独立配置。
		logger.Warn("未配置 SEEAITV_SECRET_KEY，回落到 JWT 密钥（仅开发）")
	}
	// ⚠️ 历史版本一起交进去（密钥轮换）、回落材料由调用方决定 —— 见 wiring.NewCipher。
	// API 传它**实际使用**的 JWT 密钥（不足 32 字节时是上面随机生成的一次性密钥），
	// 与 Worker 的回落值本来就不同（AGT-01 只收口构造，不改行为）。
	cipher, err := wiring.NewCipher(cfg, string(jwtSecret))
	if err != nil {
		return handlers{}, err
	}
	svc.SetSecretCipher(cipher)
	svc.SetAudit(audit.New(deps.DB.Pool, gen.Next, logger))

	wsAuth := identityhttp.NewWSAuth(tokens, svc)

	// 协作域同样注入 hub：成员移除后必须撤销其事件订阅（NEW-18）。
	collabSvc := collabapp.New(collabrepo.New(deps.DB.Pool), gen.Next)
	collabSvc.SetRevoker(hub)
	collabSvc.SetAudit(audit.New(deps.DB.Pool, gen.Next, logger))
	collabSvc.SetLogger(logger)
	// 在线状态只存 Redis（P12-02）。⚠️ Redis 不可用时 presence 整体降级为
	// "看不到别人"，但画布照常能打开、照常能保存 —— 它是提示不是权限。
	if deps.Cache != nil {
		collabSvc.SetPresence(presenceStore{deps.Cache})
	}
	collabHandler := collabhttp.NewHandler(collabSvc, identityhttp.UserIDFrom)

	auditLog := audit.New(deps.DB.Pool, gen.Next, logger)
	catalogSvc := catalogapp.New(catalogrepo.New(deps.DB.Pool), gen.Next)
	catalogSvc.SetAudit(auditLog)
	catalogSvc.SetSecretCipher(cipher)
	catalogSvc.SetLogger(logger)
	// 配置缓存键含 revision：写配置即 bump，下次读自然落到新键（CD-14）。
	if deps.Cache != nil {
		catalogSvc.SetCache(catalogapp.NewConfigCache(deps.Cache, deps.Cache.Key, 30*time.Minute))
	}
	// 启动时把代码注册的协议同步进表（修 GAP-09）：
	// 代码没实现的不会出现在管理端下拉里，实现了的也不会漏。
	if err := catalogSvc.SyncProtocols(context.Background()); err != nil {
		return handlers{}, err
	}
	subjectID := func(r *http.Request) int64 {
		c := identityhttp.ClaimsFrom(r.Context())
		if c == nil {
			return 0
		}
		id, _ := c.SubjectID()
		return id
	}
	catalogHandler := cataloghttp.NewHandler(catalogSvc, subjectID)
	// 连接测试：共享的 HTTP 传输层 + 解密后的密钥。
	// ⚠️ production 下 AllowPrivate 必须是 false —— 允许内网意味着
	// 任何能配 base_url 的人都能让服务端去访问云元数据端点（SSRF）。
	catalogHandler.SetProber(providerProber{
		svc: catalogSvc,
		client: providertransport.NewClient(providertransport.URLPolicy{
			AllowPrivate: cfg.Dev.AllowPrivateNetwork,
		}),
	})
	catalogHandler.SetUserIDResolver(subjectID)

	mediaSvc := mediaapp.New(mediarepo.New(deps.DB.Pool), deps.ObjStore, gen.Next)
	// 排进预览素材分析后叫醒 Worker 的扫描器（SCT-16）；没有 Redis 时扫描器靠 5 秒兜底。
	mediaSvc.SetAnalysisWaker(wiring.AnalysisWaker(deps.Cache))
	// 媒体删除保护：各域当场回答"还在用吗"（P14-03）。
	//
	// ⚠️ 两个进程都要注册。只注册 API 的话，API 拒绝删除而 Worker 的回收器
	// 照样把它收掉 —— 而两边各自看起来都没错。
	mediaSvc.SetLogger(logger)
	mediaSvc.AddReferenceChecker(mediaRefChecker{mediaapp.UsageScriptAsset,
		scriptrepo.New(deps.DB.Pool).MediaReferences})
	mediaSvc.AddReferenceChecker(mediaRefChecker{mediaapp.UsageTimelineClip,
		timelinerepo.New(deps.DB.Pool, gen.Next).MediaReferences})
	mediaSvc.AddReferenceChecker(mediaRefChecker{mediaapp.UsageGenerationOut,
		genrepo.New(deps.DB.Pool).MediaReferences})
	mediaSvc.AddReferenceChecker(mediaRefChecker{mediaapp.UsageProjectCover,
		canvasrepo.New(deps.DB.Pool).MediaReferences})
	// Agent 会话的附件与营销会话的商品图 / 角色图 / 源视频（MKT-T-23）。分两个 Kind 只为文案。
	mediaSvc.AddReferenceChecker(mediaRefChecker{mediaapp.UsageAgentSession,
		agentrepo.New(deps.DB.Pool).GeneralMediaReferences})
	mediaSvc.AddReferenceChecker(mediaRefChecker{mediaapp.UsageAgentMarketing,
		agentrepo.New(deps.DB.Pool).MarketingMediaReferences})
	// 营销商品库里**未删除**商品的图（MKT-T-33，MKT-D-53）。软删之后不再拦（MKD-31）。
	mediaSvc.AddReferenceChecker(mediaRefChecker{mediaapp.UsageMarketingProduct,
		marketingrepo.New(deps.DB.Pool).ProductMediaReferences})
	// 角色库里**未删除**角色的图片与参考音频（MKT-T-34）。
	mediaSvc.AddReferenceChecker(mediaRefChecker{mediaapp.UsageMarketingCharacter,
		marketingrepo.New(deps.DB.Pool).CharacterMediaReferences})
	// 3D 导演台（DRD-16）：场景正文引用的全景 / 泼溅，与截图、预演、泼溅记录。回收站里的项目也算引用。
	mediaSvc.AddReferenceChecker(mediaRefChecker{mediaapp.UsageDirectorScene,
		canvasrepo.New(deps.DB.Pool).DirectorMediaReferences})
	mediaHandler := mediahttp.NewHandler(mediaSvc, subjectID, subjectID)

	// 内容域（P13-01）：首页聚合、Banner 排期、公告、消息中心、精选作品。
	contentSvc := contentapp.New(contentrepo.New(deps.DB.Pool, gen.Next))
	contentSvc.SetAudit(auditLog)
	contentSvc.SetLogger(logger)
	// 首页最近项目的 Agent 会话由画布域回答（AGT-45）：「最近活跃的会话」那条规则
	// 只在画布仓储里有一份，全部项目页与首页共用。
	contentSvc.SetProjectAgents(projectAgents{canvasrepo.New(deps.DB.Pool)})
	contentHandler := contenthttp.NewHandler(contentSvc, identityhttp.UserIDFrom)
	// ⚠️ 公开取图由媒体域实现端口：内容域只负责判"这件作品公开不公开"，
	// 对象存储、缩略图、Range 那一套不该它知道（谁定义端口谁拥有契约）。
	contentHandler.SetMedia(mediaHandler)
	contentAdmin := contenthttp.NewAdminHandler(contentSvc, subjectID)

	// 任务运维域（P13-03）：死信处置、受控重试、人工补偿、项目运维。
	//
	// ⚠️ 它**自己不碰任务表** —— 重试与补偿都通过端口交给拥有那张表的域
	// 执行（下面注册）。在这里直接改 generation 的状态的话，就会出现第二条
	// 改任务状态的路径，而它必然与 Worker 那条不一致（退款、组子项回写、
	// 整图推进这三件事在那条路上一件都看不见）。
	opsSvc := opsapp.New(opsrepo.New(deps.DB.Pool, gen.Next))
	opsSvc.SetAudit(auditLog)
	opsSvc.SetLogger(logger)
	// ⚠️ 重投走与正常受理**同一个 publisher**：另开一条投递路径的话，
	// 两条路的 exchange / 持久化设置早晚会不一致，而不一致的表现是
	// "重投的消息 broker 重启后消失了"。
	opsSvc.SetRepublisher(mq.NewPublisher(deps.MQ))
	opsHandler := opshttp.NewHandler(opsSvc, subjectID)

	// 可观测域（P13-04）：API 请求日志、Provider 请求日志检索、统计口径。
	//
	// ⚠️ `api_request_log` 与 `stat_daily` 在 P01-03 就建好了，**到这里为止
	// 没有任何读写方** —— 与 `resource_share`（P12-01）、死信队列（P13-03）
	// 是同一类："建了没人用"。
	obsSvc := obsapp.New(obsrepo.New(deps.DB.Pool, gen.Next))
	obsSvc.SetLogger(logger)
	// ⚠️ Provider 日志由 Provider 域读（表归它），走端口不直接查对方的表 ——
	// 一旦它改了列，编译不会报错而日志页会在运行时整页打不开。
	obsSvc.SetProviderLogs(providerrepo.New(deps.DB.Pool, gen.Next))
	obsHandler := obshttp.NewHandler(obsSvc)

	// 生成、目录、画布、账本、脚本、整图的共享装配（AGT-01，T-3）：
	// 适配器与 Set* 链在 internal/wiring，Worker 将来用同一份，而不是再抄一份。
	core, err := wiring.BuildCore(wiring.CoreInput{
		Cfg: cfg, Deps: deps, NextID: gen.Next, Logger: logger,
		Audit: auditLog, Media: mediaSvc, Catalog: catalogSvc,
	})
	if err != nil {
		return handlers{}, err
	}
	canvasSvc, genSvc, scriptSvc, wfSvc := core.Canvas, core.Gen, core.Script, core.Workflow
	canvasHandler := canvashttp.NewHandler(canvasSvc, identityhttp.UserIDFrom)

	billingSvc := billingapp.New(billingrepo.New(deps.DB.Pool, gen.Next))
	// 商品改价要留痕：「这一单为什么是 99 而不是 199」半年后只有审计说得清。
	billingSvc.SetAudit(auditLog)

	// 支付域（P11-02）。
	//
	// ⚠️ 渠道适配器**按渠道码共存**（同 provider 的真假共存）：
	// 开发支付与真实渠道不是一个全局开关 —— 一刀切的话打开开发支付
	// 就等于所有渠道都变假，真实联调没法做。
	payRepo := paymentrepo.New(deps.DB.Pool, gen.Next)
	payRegistry := paymentapp.NewRegistry()
	payChannels := paymentapp.NewChannelAdmin(payRepo, cipher, payRegistry,
		&http.Client{Timeout: 20 * time.Second}, logger)
	payChannels.SetAudit(auditLog)
	// 启动时按库里的配置装配一遍。⚠️ 失败只告警不阻断启动：
	// 一家渠道的证书过期不该让整个 API 起不来。
	if err := payChannels.Reload(context.Background()); err != nil {
		logger.Warn("支付渠道装配失败", "error", err)
	}
	paySvc := paymentapp.New(payRegistry, payRepo, billingSvc, logger)
	// 退款与对账（P11-03）。⚠️ 两个端口都定义在**账本域**：
	// 它只需要「退这一笔」「查这一笔」，不需要认识回调与验签的形状。
	billingSvc.SetRefundChannel(refundChannel{payRegistry})
	billingSvc.SetReconcileQuery(reconcileQuery{payRegistry})
	billingSvc.SetLogger(logger)

	// 会员等级解析器（P11-01）。
	//
	// ⚠️ **一份，注入给所有要它的 handler**：各写一份的话，
	// 「报价说这个模型能用、受理说等级不够」这种自相矛盾迟早出现，
	// 而两边各自都"没错"。等级按**付费主体**取（团队工作区的成员
	// 继承工作区的等级），取不到时降级成 0 而不是报错 ——
	// 一次数据库抖动不该让所有生成都失败。
	memberLevelOf := func(req *http.Request) int64 {
		ws, ok := collabhttp.WorkspaceFrom(req.Context())
		if !ok {
			return 0
		}
		return billingSvc.MemberLevelID(req.Context(), ws.PayerAccountID)
	}
	catalogHandler.SetMemberLevelResolver(memberLevelOf)
	if cfg.CompensationValidFor > 0 {
		billingSvc.SetCompensationValidFor(cfg.CompensationValidFor)
	}

	// 订阅 Worker 广播的状态事件并投给 WebSocket hub。
	// ⚠️ pub/sub 是尽力而为的 —— 事件本来就不是事实来源，
	// 前端断线重连后要用 HTTP 对账（GO-26）。
	// （发布侧 genSvc.SetEvents 已在 wiring.BuildCore 里接上。）
	//
	// 通用事件总线（AGT-02）：任意进程发到 `events:bus` 的 {topic, payload}，按 topic 原样投给 hub。
	// ⚠️ 过渡期两条桥并存：旧 EventBridge 只为接住滚动发布期间还没升级的旧 Worker
	// 发到 `events:generation` 的事件，下一版本删除（见 AGT-02）。发布方不双发，所以不会重复投递。
	if deps.Cache != nil {
		go eventbus.NewBridge(deps.Cache.Client, deps.Cache.Key, hub, logger).Run(context.Background())
		// 画布操作提交后推 canvas.ops_applied 到 project:{id}（AGT-10）。
		// Agent 跑在 Worker 时由 Worker 侧的画布服务另行接上同一个发布器。
		canvasSvc.SetEvents(eventbus.NewPublisher(deps.Cache.Client, deps.Cache.Key))
		legacyBridge := genapp.NewEventBridge(deps.Cache.Client, deps.Cache.Key, hub, logger)
		go legacyBridge.Run(context.Background())
	}

	// 共享资源的出入口（P12-01）。
	//
	// ⚠️ 注册在这里而不是让协作域去 import 画布/脚本域：谁定义端口谁拥有契约，
	// 协作域只知道 ResourceProvider，具体是项目还是资产由组装根接上。
	// 反过来做的话，协作域会慢慢变成一个知道所有业务域内部结构的地方。
	collabSvc.RegisterProvider(canvasapp.NewShareProvider(canvasSvc))
	assetShare := scriptapp.NewAssetShare(scriptSvc, mediaSvc)
	collabSvc.RegisterProvider(assetShare)
	collabSvc.RegisterImporter(collabdomain.ResourceScriptAsset, assetShare)

	// 时间线（P10-01）。装配（素材 / 镜头 / 归属端口、渲染受理的报价扣费退款、ffmpeg 与字幕能力、
	// 水印开关）在 wiring.NewTimelineService：画布 Agent 在 Worker 里调同一个服务（AGT-50），
	// 两个进程各抄一份适配器的话「渲染多少钱」「加不加水印」早晚有两个答案。
	// 时间线写入事件（timeline.updated，docs/agent/smartcut SCD-19）：没有 Redis 时不接，前端按版本号补取。
	var timelineEvents timelineapp.EventPublisher
	if deps.Cache != nil {
		timelineEvents = eventbus.NewPublisher(deps.Cache.Client, deps.Cache.Key)
	}
	tlSvc := wiring.NewTimelineService(wiring.TimelineInput{
		Pool: deps.DB.Pool, NextID: gen.Next, Media: mediaSvc, Script: scriptSvc, Canvas: canvasSvc,
		Catalog: catalogSvc, CompensationValidFor: cfg.CompensationValidFor, Logger: logger,
		Events: timelineEvents, Smartcut: cfg.Smartcut,
	})

	// 运维补偿与受控重试的端口注册（P13-03）。
	//
	// ⚠️ **只注册生成与渲染两类**：整图运行由它的子任务推进（补偿单个节点
	// 才有意义），打包导出不扣费（补偿它只是改个状态，没有账务可言）。
	// 注册进来的两个适配器都只是转调各域自己的方法 —— 补偿要做的三件事
	// （判失败、退款、推进组/运行）都在那一侧，运维域一件都不重复实现。
	opsSvc.Register(opsdomain.KindGeneration, genCompensator{genSvc})
	opsSvc.Register(opsdomain.KindRender, renderCompensator{tlSvc})
	opsSvc.SetRetrier(opsRetrier{genSvc})

	// 提交作品前要校验来源归属与是否成功 —— 那是生成/时间线域才知道的事，
	// 所以注入放在这两个服务都装好之后（P13-02）。
	contentHandler.SetWorkSource(workSourceGuard{gen: genSvc, tl: tlSvc})

	// 智能剪辑（SCT-07 / 25 / 27）：AI 剪辑同步调文本模型（API 进程里，不经 Worker），阶段推送走同一条实时总线。
	var smartcutEvents smartcutapp.PlanEvents
	if deps.Cache != nil {
		smartcutEvents = eventbus.NewPublisher(deps.Cache.Client, deps.Cache.Key)
	}
	smartcutSvc := wiring.NewSmartcutService(wiring.SmartcutInput{
		Timeline: tlSvc, Canvas: canvasSvc, Media: mediaSvc, Generations: core.GenRepo, Log: logger,
		Plan: &wiring.SmartcutPlanInput{Pool: deps.DB.Pool, Timeline: tlSvc, Catalog: catalogSvc, Media: mediaSvc,
			Generations: core.GenRepo, Text: core.TextProvider, Events: smartcutEvents, Cfg: cfg.Smartcut,
			NextID: gen.Next, Logger: logger, RenderTimeline: timelinehttp.ViewJSON,
			// 分镜成片：素材条里的脚本 / 视频组按连线顺序装配（SCT-28）。
			Board: wiring.NewSmartcutBoard(scriptSvc, mediaSvc)},
		// 语音识别（SCT-33）：按模型收费的生成任务，受理与续接在 API 进程里。
		Speech: &wiring.SmartcutSpeechInput{Catalog: catalogSvc, Gen: genSvc, Media: mediaSvc, Timeline: tlSvc,
			Events: smartcutEvents},
		// 片段级生成（SCT-37~39）：受理在 API；取消与运维补偿路径的回写也在这里（成功 / 失败在 Worker）；
		// 剪辑台配置里的可选模型也从这里列。
		Clip: &wiring.SmartcutClipInput{Gen: genSvc, GenRepo: core.GenRepo, Media: mediaSvc, Timeline: tlSvc,
			Render: timelinehttp.ViewJSON, Catalog: catalogSvc},
	})
	// 取消与运维补偿发生在 API 进程：识别任务在这两条路上到终态时也要回写（Worker 那侧另装一份）。
	genSvc.SetSmartcutBinder(wiring.SmartcutBinder{Svc: smartcutSvc})

	// 创造页直接生成（AGT-42）。
	quickGenHandler := canvashttp.NewQuickHandler(canvasSvc, func(req *http.Request) int64 {
		id, _ := identityhttp.UserIDFrom(req.Context())
		return id
	})

	out := handlers{
		identity:     identityhttp.NewHandler(svc, tokens),
		collab:       collabHandler,
		catalog:      catalogHandler,
		media:        mediaHandler,
		canvas:       canvasHandler,
		quickGen:     quickGenHandler,
		billing:      billinghttp.NewHandler(billingSvc, subjectID),
		billingAdmin: billinghttp.NewAdminHandler(billingSvc, subjectID),
		payment:      paymenthttp.NewHandler(paySvc),
		paymentAdmin: paymenthttp.NewAdminHandler(payChannels, subjectID),
		gen:          genhttp.NewHandler(genSvc),
		script:       scripthttp.NewHandler(scriptSvc, identityhttp.UserIDFrom),
		workflow: workflowhttp.New(wfSvc, func(req *http.Request) int64 {
			id, _ := identityhttp.UserIDFrom(req.Context())
			return id
		}),
		workflowTick:    wfSvc,
		scriptRecover:   scriptSvc,
		director:        directorhttp.NewHandler(core.Director, identityhttp.UserIDFrom),
		directorRecover: core.Director,
		timelineAdmin:   timelinehttp.NewAdminHandler(tlSvc, subjectID),
		content:         contentHandler,
		contentAdmin:    contentAdmin,
		ops:             opsHandler,
		obs:             obsHandler,
		timeline: timelinehttp.NewHandler(tlSvc, func(req *http.Request) int64 {
			id, _ := identityhttp.UserIDFrom(req.Context())
			return id
		}),
		smartcut: smartcuthttp.NewHandler(smartcutSvc),
		// AI 剪辑的恢复扫描（进行中太久的判失败，释放并发额度）与调用记录清理（保留 30 天，SCT-27）。
		smartcutRecover: smartcutSvc,
		ws:              newWSHandler(hub, wsAuth, wsAuth, logger, cfg.WSAllowedOrigins),
	}
	// 会员等级（P11-01）：**同一个解析器**注入给每一个要它的入口 ——
	// 报价、受理、整图、渲染。少接一处的症状是那条路上的等级门槛
	// 形同虚设（报价说不能用、直接打 API 照样跑），而两边各自都"没错"。
	out.gen.SetMemberLevelResolver(memberLevelOf)
	// 创造页直接生成（AGT-42）也会受理生成，等级门槛必须同一个解析器。
	out.quickGen.SetMemberLevelResolver(memberLevelOf)
	// 画布 Agent（AGT-26）：API 只收消息、写库、写 outbox，一轮的运行在 Worker（T-2）。
	// 批准规划时要重新报价，会员等级用同一个解析器。
	var agentEvents agentapp.Events
	if deps.Cache != nil {
		agentEvents = wiring.AgentEvents{Bus: eventbus.NewPublisher(deps.Cache.Client, deps.Cache.Key)}
	}
	// 用户端营销服务（MKT-T-04）：营销接口与建营销会话（MKT-T-17）共用同一个实例 ——
	// 选项可见性 / 钉版本只有一处判定（MKD-13）。
	marketingSvc := wiring.NewMarketingService(wiring.MarketingServiceInput{
		Repo: marketingrepo.New(deps.DB.Pool), Catalog: catalogSvc,
		Settings: marketingapp.Settings{MaxProductImages: cfg.Marketing.MaxProductImages,
			MaxVariants: cfg.Marketing.MaxVariants, AgentEnabled: cfg.Agent.Enabled,
			MaxProducts: cfg.Marketing.MaxProducts, MaxCharacters: cfg.Marketing.MaxCharacters},
	})
	// 商品库 / 角色库（MKT-T-33/34）：库路由与建营销会话取快照（MKT-T-37）共用同一个实例。
	marketingLib := wiring.NewMarketingLibrary(wiring.MarketingLibraryInput{
		Repo: marketingrepo.New(deps.DB.Pool), Media: mediaSvc, NextID: gen.Next, Logger: logger,
		Settings: marketingapp.LibrarySettings{MaxProductImages: cfg.Marketing.MaxProductImages,
			MaxProducts: cfg.Marketing.MaxProducts, MaxCharacters: cfg.Marketing.MaxCharacters},
	})
	// 营销总开关关闭时不接入：scene=marketing 的建会话请求一律 400（MKT-T-17）。
	var agentScene agentapp.SceneResolver
	var agentLib *marketingapp.Library
	if cfg.Marketing.Enabled {
		agentScene, agentLib = wiring.AgentScene{Svc: marketingSvc}, marketingLib
	}
	agentSvc := wiring.NewAgentService(wiring.AgentAPIInput{
		Repo: agentrepo.New(deps.DB.Pool), NextID: gen.Next,
		Canvas: canvasSvc, Media: mediaSvc, Script: scriptSvc, Catalog: catalogSvc,
		Billing: billingSvc, Gen: genSvc, Events: agentEvents, Logger: logger,
		// 一键优化提示词（AGT-43）：与脚本分镜同一个文本 Provider、同一个账本。
		Chat: core.TextProvider, Ledger: core.Billing, Compensate: cfg.CompensationValidFor,
		Scene: agentScene,
		// 批量变体上限（MKT-T-27）：批准规划时兜底，与 /marketing/config 同一个配置值。
		MaxVariants: cfg.Marketing.MaxVariants,
		// 建营销会话时把商品 / 角色冻结成快照（MKT-T-37，MKD-25）。
		Library: agentLib,
	})
	if cfg.Agent.Enabled {
		out.agentRecover = agentSvc
	}
	out.agent = agenthttp.NewHandler(agentSvc, func(req *http.Request) int64 {
		id, _ := identityhttp.UserIDFrom(req.Context())
		return id
	}, cfg.Agent.Enabled)
	out.agent.SetMemberLevelResolver(memberLevelOf)
	// 技能封面公开取图（MKT-T-05、MKD-19）：启用判定在 agent，出字节交给媒体域。
	out.agent.SetMedia(mediaHandler)
	// 管理端技能配置（AGT-60）：渲染器与运行时同一个（AgentSkillRenderer），保存前试渲染的
	// 结论才与 load_skill / Runner 一致。不受 Agent 总开关影响（上线前要能先配好）。
	skillAdmin := agentapp.NewSkillAdmin(agentrepo.New(deps.DB.Pool), skillModels{catalogSvc},
		wiring.AgentSkillRenderer{}, gen.Next)
	skillAdmin.SetAudit(auditLog)
	// 口播文案与输入槽声明校验（MKT-T-05）：唯一实现在 marketing/domain，经 wiring 适配。
	skillAdmin.SetMarketingChecks(wiring.AgentSkillCopy{}, wiring.AgentSkillSlots{})
	out.agentAdmin = agenthttp.NewAdminHandler(skillAdmin, subjectID)
	// 管理端营销选项与分类（MKT-T-03）：推荐模型问目录、默认技能问 Agent、封面问媒体，
	// 端口适配在 wiring。不受营销总开关影响（上线前要能先配好，同技能管理）。
	// 用户端营销接口（MKT-T-04）：两个上限来自配置（MKD-16），与技能槽位校验同一个数；
	// Agent 关着时 /marketing/config 回 available=false（B-5）。
	out.marketing = marketinghttp.NewHandler(marketingSvc, cfg.Marketing.Enabled)
	out.marketing.SetMedia(mediaHandler)
	// 商品库（MKT-T-33）：要登录 + 工作区闸（GET → view，其余 → edit，MKD-32）；仍受营销总开关。
	out.marketing.SetLibrary(marketingLib, subjectID)
	// 生成角色（MKT-T-35）：会员等级用 `POST /generations` 同一个 memberLevelOf（付费主体同取自工作区闸）。
	out.marketing.SetCharacterGen(wiring.NewMarketingCharacterGen(genSvc, catalogSvc,
		wiring.MarketingWorkspaceMedia{Media: mediaSvc}), memberLevelOf)
	out.marketingAdmin = marketinghttp.NewAdminHandler(wiring.NewMarketingAdmin(wiring.MarketingAdminInput{
		Repo: marketingrepo.New(deps.DB.Pool), Catalog: catalogSvc, Agent: agentrepo.New(deps.DB.Pool),
		Media: mediaSvc, NextID: gen.Next, Audit: auditLog,
	}), subjectID)
	// 营销角色（平台预置）管理（MKT-T-36）：同一对权限位 agent:read / agent:write，媒体校验沿用选项封面那一个端口。
	presetAdmin := marketingapp.NewPresetAdmin(marketingrepo.New(deps.DB.Pool), wiring.MarketingMedia{Media: mediaSvc},
		gen.Next)
	presetAdmin.SetAudit(auditLog)
	out.marketingAdmin.SetPresets(presetAdmin)
	out.workflow.SetMemberLevelResolver(memberLevelOf)
	// AI 搭景按会员等级选模型（报价的会员门槛与生成同一个解析器）。
	out.director.SetMemberLevelResolver(memberLevelOf)
	// AI 剪辑选模型时看的会员等级与报价同一个解析器（SCT-27）。
	out.smartcut.SetMemberLevelResolver(memberLevelOf)
	out.smartcut.SetUserIDs(identityhttp.UserIDFrom)
	out.timeline.SetMemberLevelResolver(memberLevelOf)
	// 开发支付：**仅开发环境**。production 下 config 已 fail-fast，
	// 这里再挡一道 —— 两处都判是因为它一旦漏了就是"谁都能白拿会员"。
	out.billing.SetDevPay(cfg.Dev.FakePayment && cfg.Env != config.EnvProduction)
	// 真实渠道（P11-02）。⚠️ 没配渠道时 `/pay` 与 `/sync` 仍然挂着，
	// 但调用会得到"渠道未配置" —— 比 404 说得清（404 会让人以为端点没做）。
	out.billing.SetPayGateway(payGateway{paySvc, payRegistry})
	// 对外 MCP（MCPT-02）。注册表与快照生成器（tools/mcpschema）是同一个构造函数（MT-8）。
	// 凭据（MCPT-03）：访问密钥 → Principal，每个请求实时查账号状态、成员角色、付费主体、会员等级
	// （不缓存，MCP-F-11）。成员资格与账号状态的判定各自只有一份，经 wiring 的适配器调用。
	//
	// 三级连接管理与两级开关（MCPT-14）：管理端页面**总是挂**（环境变量关着时如实显示 envEnabled=false），
	// 其余随 MCP 总开关。开关每次请求读库、不缓存（撤销延迟 0，见 mcp/app.Switch）。
	mcpManage := mcpapp.NewManage(mcprepo.New(deps.DB.Pool), wiring.MCPWorkspaces{Svc: collabSvc}, auditLog,
		cfg.MCP.Enabled, logger)
	out.mcpAdmin = mcphttp.NewAdminHandler(mcpManage, subjectID)
	if cfg.MCP.Enabled {
		out.mcpWorkspace = mcphttp.NewWorkspaceHandler(mcpManage, identityhttp.UserIDFrom)
		out.mcpSwitch = mcpManage.Switch()
		mcpStore := mcprepo.New(deps.DB.Pool)
		mcpMembers := wiring.MCPMembers{Svc: collabSvc}
		mcpResolver := mcpapp.NewResolver(mcpStore, wiring.MCPUsers{Svc: svc}, mcpMembers, billingSvc, logger)
		out.mcpKeys = mcphttp.NewKeysHandler(mcpapp.NewKeys(mcpStore, mcpMembers, auditLog, gen.Next),
			identityhttp.UserIDFrom)
		// 两级限流与调用审计（MCPT-06）。限流只靠 Redis：没有 Redis 时不限流（保护不是正确性）；
		// 审计写自己的表，异步、独立 context、失败只告警，写协程在 run() 里随进程启停。
		var mcpLimiter mcphttp.RateLimiter
		var oauthLimiter mcpoauth.RateLimiter
		if deps.Cache != nil {
			l := mcpapp.NewLimiter(deps.Cache.Client, deps.Cache.Key,
				cfg.MCP.RateUserClient, cfg.MCP.RateWorkspace, logger)
			mcpLimiter, oauthLimiter = l, l
		}
		// OAuth 2.1 授权服务器（MCPT-12）：令牌落到同一张 mcp_grant，之后与访问密钥走同一个 Resolver。
		// CIMD 拉取走共享出网策略（production 禁内网、连接期校验、不跟随重定向）；req 句柄用
		// SEEAITV_SECRET_KEY 派生的子密钥签名（与 requestState 同一条回落）。
		out.mcpOAuth = mcpoauth.New(mcpoauth.Options{
			Store: mcpStore, Users: wiring.MCPUsers{Svc: svc}, Members: mcpMembers, Audit: auditLog,
			NextID: gen.Next,
			Fetcher: mcpoauth.TransportFetcher{Client: providertransport.NewClient(providertransport.URLPolicy{
				AllowPrivate: cfg.Dev.AllowPrivateNetwork,
			})},
			Limiter: oauthLimiter, DCRPerHour: mcpoauth.DefaultDCRPerHour,
			PublicURL: cfg.MCP.PublicURL, WebPublicURL: cfg.MCP.WebPublicURL,
			RequestKey: mcpoauth.DeriveRequestKey(mcpStateMaterial(cfg, jwtSecret)),
			UserIDs:    identityhttp.UserIDFrom, Logger: logger,
		})
		// 页面审批（MCPT-13）：只认 quote 所属授权的用户本人；不挂工作区中间件。
		out.mcpApprovals = mcphttp.NewApprovalHandler(mcpapp.NewApprovals(mcpStore,
			wiring.MCPApprovalWorkspaces{Svc: collabSvc}, wiring.MCPPayerBalances{Svc: billingSvc}, auditLog),
			identityhttp.UserIDFrom)
		out.mcpCalls = mcpapp.NewCallLogger(mcpStore, gen.Next, logger, 0)
		// 只读工具（MCPT-04）的端口：各域服务经 wiring 适配，角色与租户一律取 Principal。
		mcpDeps := wiring.NewMCPToolDeps(wiring.MCPReadInput{
			Identity: svc, Collab: collabSvc, Billing: billingSvc, Canvas: canvasSvc, GenRepo: core.GenRepo,
			Gen: genSvc, Catalog: catalogSvc, Content: contentSvc, Workflow: wfSvc, Timeline: tlSvc,
			Media: mediaSvc, MediaURLTTL: cfg.MCP.MediaURLTTL,
			// 报价（MCPT-05）：复用 estimate / Agent 规划的报价路径，签发 quoteId 落 mcp_quote。
			Store: mcpStore, NextID: gen.Next, QuoteTTL: cfg.MCP.QuoteTTL,
			// generate（MCPT-10）：画布生成编排与受理同 Agent（internal/canvasgen），花钱走确认服务。
			// elicitation 的 requestState 用 SEEAITV_SECRET_KEY 派生的子密钥签名（多实例一致、不新增环境变量）；
			// 开发下未配置时与密钥加密同一条回落（JWT 密钥）。
			Ledger: core.Billing, StateKey: mcpapp.DeriveStateKey(mcpStateMaterial(cfg, jwtSecret)),
			// 页面审批（MCPT-13）：凭据开了「花钱需在页面确认」时，审批链接指向用户端 /mcp/approve/[quoteId]。
			ApprovalBase: mcpApprovalBase(cfg),
			// 写工具（MCPT-11）：media.upload 的 URL 下载走共享出网策略（production 禁内网，config 已 fail-fast）；
			// tasks.wait 上限（SEEAITV_MCP_WAIT_MAX）。
			Download: providertransport.NewClient(providertransport.URLPolicy{
				AllowPrivate: cfg.Dev.AllowPrivateNetwork,
			}),
			WaitMax: cfg.MCP.WaitMax,
			// 多段工具集（MCPT-16）：脚本线 / 整图 / 时间线委托 Agent 的同一组适配器（报价口径只有一份）；
			// 提示词从站内技能派生（与 Agent 同一个渲染器）。
			Script: scriptSvc, ScriptRepo: scriptrepo.New(deps.DB.Pool), Skills: agentSvc,
			// 委托站内 Agent（MCPT-17）：同一个 agent 服务；站内 Agent 关闭时 agent 工具集不注册（列表与片段里都没有）。
			Agent: agentSvc, AgentEnabled: cfg.Agent.Enabled,
			// 3D 导演台（DRT-29）：director_create / director_build，搭景服务与网页路径同一份（BuildCore）。
			Director: core.Director,
			// 智能剪辑（SCT-41）：smartcut_create / get / plan / export，与网页同一份智能剪辑服务（带 AI 剪辑端口）。
			Smartcut: smartcutSvc,
			// 这一版只保留手动剪辑：smartcut_plan 按开关注册（缺省不注册）。
			SmartcutAIEnabled: cfg.Smartcut.AIEnabled,
		})
		mcpRegistry := mcptools.NewRegistry(mcpDeps)
		// 客户端配置片段（MCPT-11F）：公网地址 + 注册表里此刻真有的工具集，进程内渲染一次。
		out.mcpKeys.SetClientSnippets(cfg.MCP.PublicURL, mcpapp.ToolsetAvailability(mcpRegistry.List()))
		// 已连接客户端的列表与撤销走三级连接管理（撤销只有一个入口，MCPT-14）。
		out.mcpKeys.SetManage(mcpManage)
		out.mcp = mcphttp.NewHandler(mcphttp.Options{
			Switch:          out.mcpSwitch,
			Limiter:         mcpLimiter,
			Calls:           out.mcpCalls,
			Auth:            mcphttp.KeyAuth{Resolver: mcpResolver},
			Registry:        mcpRegistry,
			PublicURL:       cfg.MCP.PublicURL,
			ResultMaxTokens: cfg.MCP.ResultMaxTokens,
			Logger:          logger,
		})
	}
	return out, nil
}

// mcpApprovalBase 是页面审批链接的用户端地址（MCPT-13）：SEEAITV_WEB_PUBLIC_URL；开发下未配置时回落到
// 本机用户端（与 OAuth 同意页的开发回落同一个端口）。production 下 config 已强制要求配置成 https。
func mcpApprovalBase(cfg config.Config) string {
	if cfg.MCP.WebPublicURL != "" || cfg.Env == config.EnvProduction {
		return cfg.MCP.WebPublicURL
	}
	return "http://localhost:3000"
}

// mcpStateMaterial 是 MCP elicitation requestState 子密钥的主材料：SEEAITV_SECRET_KEY，开发下未配置时回落到
// API 实际使用的 JWT 密钥（与 wiring.NewCipher 同一条回落；production 由 config 强制要求独立配置）。
func mcpStateMaterial(cfg config.Config, jwtSecret []byte) string {
	if cfg.Auth.SecretKey != "" {
		return cfg.Auth.SecretKey
	}
	return string(jwtSecret)
}

// providerProber 把传输层的探活接成目录域的端口。
type providerProber struct {
	svc    *catalogapp.Service
	client *providertransport.Client
}

func (p providerProber) Probe(ctx context.Context, code string) (any, error) {
	cfg, err := p.svc.ProviderForProbe(ctx, code)
	if err != nil {
		return nil, err
	}
	// 认证头与真实调用同一条规则（volc_v4 要按这次 GET 签名，SCD-38）。
	creds := providertransport.Credentials{Type: cfg.AuthType, Key: cfg.APIKey, HeaderName: cfg.HeaderName,
		Region: cfg.SignRegion, Service: cfg.SignService}
	header, err := creds.Headers("GET", providertransport.ProbeURL(cfg.BaseURL, cfg.TestPath), nil, nil, time.Now())
	if err != nil {
		// 配置本身不完整（缺地域、密钥不是 AK:SK）：连不连得上都无从谈起，直接说缺什么。
		return providertransport.ProbeResult{OK: false, ErrorKind: providertransport.ErrorPolicy,
			Message: err.Error()}, nil
	}
	return p.client.Probe(ctx, cfg.BaseURL, cfg.TestPath, header,
		providertransport.Options{
			ConnectTimeout: time.Duration(cfg.ConnectTimeoutMS) * time.Millisecond,
			RequestTimeout: time.Duration(cfg.RequestTimeoutMS) * time.Millisecond,
		}), nil
}

func router(deps *bootstrap.Deps, h handlers) http.Handler {
	identity, collab, catalog, media := h.identity, h.collab, h.catalog, h.media
	wsHandler := h.ws
	r := chi.NewRouter()
	// 顺序按 08 §4.1：trace → 日志 → 恢复 → 超时 → 认证 → 限流 → handler。
	r.Use(httpx.TraceID)
	r.Use(middleware.RealIP)
	r.Use(httpx.Recover)
	// ⚠️ 请求日志必须挂在 `httpx.TraceID` **之后**：日志的第一价值就是按
	// traceId 把「API 请求 → 生成任务 → Provider 调用」串起来（AC-26）。
	// 挂在前面的话每条日志的 traceId 都是空的，而那时这张表基本没有用。
	//
	// ⚠️ 也要挂在 `Recover` 之后：panic 被兜住之后响应是 500，
	// 而那一条恰恰最该出现在日志里。
	if h.obs != nil {
		r.Use(h.obs.Logger(func(ctx context.Context) (int64, int64, int64) {
			var userID, adminID int64
			if c := identityhttp.ClaimsFrom(ctx); c != nil {
				if id, err := c.SubjectID(); err == nil {
					// ⚠️ 按 token 类型分栏：user token 与 admin token 是
					// **两个主体空间**，同一个数字在两边指的是不同的人 ——
					// 混在一列里的话按 ID 查日志会捞出别人的请求。
					if c.Kind == token.KindAdmin {
						adminID = id
					} else {
						userID = id
					}
				}
			}
			var wsID int64
			if ws, ok := collabhttp.WorkspaceFrom(ctx); ok {
				wsID = ws.WorkspaceID
			}
			return userID, adminID, wsID
		}))
	}
	// ⚠️ 同步调模型的端点（分镜、提示词 AI 合成）放宽到 scriptapp.SyncLLMRequestTimeout，其余 30 秒。
	r.Use(requestTimeout(30*time.Second, scriptapp.SyncLLMRequestTimeout))

	// liveness：只回答"进程还活着吗"，**不查依赖**。
	// 数据库挂了重启进程没用，反而会在依赖恢复前不断重启，把问题放大。
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})

	// readiness：逐个探活依赖，任一不通即返回 503，让负载均衡摘掉本实例。
	r.Get("/readyz", func(w http.ResponseWriter, req *http.Request) {
		res := deps.Ready(req.Context())
		status := http.StatusOK
		if res.Status != "ok" {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, res)
	})
	r.Get("/version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, buildinfo.Get())
	})

	r.Route("/api/v1", func(r chi.Router) {
		identity.Mount(r)
		identity.MountAdmin(r)
		// 支付回调（P11-02）。⚠️ **公开端点**：渠道不会带我们的 token，
		// 所以这里不挂任何鉴权中间件 —— 安全性完全靠**验签**。
		// 挂在最前面是为了让它明显地与鉴权路由分开，避免哪天被顺手裹进
		// RequireUser 里（裹进去的话所有回调都会 401，而渠道只会一直重试）。
		h.payment.Mount(r)
		collab.Mount(r, identity.RequireUser)
		// 受让方视角刻意不套工作区中间件：他不是源工作区的成员，
		// 授权的唯一来源是 resource_share（P12-01）。
		collab.MountShares(r, identity.RequireUser)
		catalog.MountUser(r, identity.RequireUser)
		catalog.MountAdmin(r, identity.RequireAdmin, identity.RequirePermission)
		catalog.MountModelAdmin(r, identity.RequireAdmin, identity.RequirePermission)
		catalog.MountPricing(r, identity.RequireUser, identity.RequireAdmin, identity.RequirePermission)
		catalog.MountMaterial(r, identity.RequireUser, identity.RequireAdmin, identity.RequirePermission)
		catalog.MountPrompt(r, identity.RequireAdmin, identity.RequirePermission)
		// 渲染水印配置与成片追溯（P10-04）。
		h.timelineAdmin.MountAdmin(r, identity.RequireAdmin, identity.RequirePermission)
		// 商品管理：会员等级 / 套餐 / 积分包（P11-01）。
		h.billingAdmin.MountAdmin(r, identity.RequireAdmin, identity.RequirePermission)
		// 订单退款与对账（P11-03）。⚠️ 走 `order:*` 不走 `member:*`：
		// 处理退款是客服/财务动作，改商品价格是运营动作。
		h.billingAdmin.MountOrderAdmin(r, identity.RequireAdmin, identity.RequirePermission)
		// 后台订单检索/详情、人工对账标记、人工调账（P11-04）。
		h.billingAdmin.MountOrderSearch(r, identity.RequireAdmin, identity.RequirePermission)
		// 支付渠道配置与回调留痕（P11-02）。
		h.paymentAdmin.MountAdmin(r, identity.RequireAdmin, identity.RequirePermission)
		// ⚠️ 业务域一律用 `wsGuard` 而不是裸的 `RequireWorkspace`（P12-03）：
		// 它 = 装工作区上下文 + **按 HTTP 方法给一个安全的默认动作要求**
		// （GET → view，其余 → edit）。到 P12-02 为止，角色只在 canvas/app 里
		// 被检查过，其余五个域一次都没查过 —— 一个只读成员可以上传素材、
		// **发起生成花掉团队的积分**、跑整张图、渲成片，而后台看不出任何异常。
		//
		// 把闸门收在这里而不是在每个 app 方法里补一行：后者漏掉任何一处
		// 都不会有编译错误，而漏掉的那一处就是一个隐身的越权口子。
		wsGuard := func(next http.Handler) http.Handler {
			return collab.RequireWorkspace(collabhttp.GuardByMethod(next))
		}
		// wsCtx 只装上下文、不带默认闸，供**例外路由**自带更宽松的闸。
		// ⚠️ 例外必须放进独立分组，不能用 `r.With` 去"放宽"：chi 的 With 是
		// **追加**在组中间件之后的，默认闸先跑就已经拒了（实测踩过）。
		wsCtx := collab.RequireWorkspace
		media.Mount(r, identity.RequireUser, identity.RequireAdmin, wsGuard)
		h.canvas.Mount(r, identity.RequireUser, wsGuard)
		// 3D 导演台 AI 搭景（DRT-19）：默认闸（GET → view，POST → edit），免费所以不标 generate。
		h.director.Mount(r, identity.RequireUser, wsGuard)
		// 创造页直接生成（AGT-42）：建项目 + 建节点 + 受理，标 generate。
		h.quickGen.Mount(r, identity.RequireUser, wsGuard)
		h.script.Mount(r, identity.RequireUser, wsGuard)
		h.workflow.Mount(r, identity.RequireUser, wsGuard, wsCtx)
		h.timeline.Mount(r, identity.RequireUser, wsGuard, wsCtx)
		// 智能剪辑节点（SCT-07）：默认闸（GET → view，POST → edit）。
		h.smartcut.Mount(r, identity.RequireUser, wsGuard)
		// 画布 Agent（AGT-26）：会话类路由走 wsGuard（GET → view，其余 → edit）；
		// 用户偏好只要登录；总开关关闭时全部 404 AGENT_DISABLED。
		h.agent.Mount(r, identity.RequireUser, wsGuard)
		// 管理端技能配置（AGT-60）：agent:read / agent:write。
		h.agentAdmin.MountAdmin(r, identity.RequireAdmin, identity.RequirePermission)
		// 管理端营销选项与分类（MKT-T-03）：agent:read / agent:write（MKD-20，不新造权限位）。
		h.marketingAdmin.MountAdmin(r, identity.RequireAdmin, identity.RequirePermission)
		// 用户端营销接口（MKT-T-04）：**不挂 requireUser**（未登录可浏览模板墙，MKT-F-03）。
		h.marketing.MountPublic(r)
		// 商品库（MKT-T-33）：完整路径挂在独立分组，不进上面那个公开的 /marketing 子路由。
		h.marketing.MountLibrary(r, identity.RequireUser, wsGuard)
		// 生成角色（MKT-T-35）：路由只装工作区上下文，dryRun 要 view、真生成在处理器里判 generate（CD-59）。
		h.marketing.MountCharacterGen(r, identity.RequireUser, wsCtx)
		h.billing.Mount(r, identity.RequireUser, wsGuard)
		// 会员、商品与订单（P11-01）。
		h.billing.MountMember(r, identity.RequireUser, wsGuard)
		// 工作区用量审计（P12-04、NEW-20）。要 manage —— 「团队的积分被谁花了」
		// 是管理动作，editor 能花钱不等于他该看得到同事花了多少。
		h.billing.MountUsage(r, identity.RequireUser, wsCtx)
		// 内容域（P13-01）。
		// ⚠️ 精选墙与公开取图**不挂 requireUser**：那是首页的门面，
		// 没登录的人看到一张空白首页正是他决定要不要注册的那一眼。
		h.content.MountPublic(r)
		h.content.MountUser(r, identity.RequireUser, wsCtx)
		// 任务中心与作品（P13-02）。
		h.content.MountTasks(r, identity.RequireUser, wsGuard, wsCtx)
		h.contentAdmin.MountAdmin(r, identity.RequireAdmin, identity.RequirePermission)
		// 作品审核（AD-09）。⚠️ 权限位走 `review:*` 而**不是** `content:*`：
		// 审核是"这东西能不能放出去"，内容运营是"首页放什么" ——
		// 合在一起的话，配 Banner 的人顺手就能通过审核。
		h.contentAdmin.MountReview(r, identity.RequireAdmin, identity.RequirePermission)
		// 任务运营台（P13-03）：与用户侧任务中心**共用同一个 union**，
		// 只是换了租户条件、多挑几列。权限位是 `ops:read`。
		h.contentAdmin.MountOpsTasks(r, identity.RequireAdmin, identity.RequirePermission)
		// 死信处置、受控重试、人工补偿、项目运维（P13-03；GAP-07、AD-08）。
		h.ops.Mount(r, identity.RequireAdmin, identity.RequirePermission)
		// 请求日志与统计（P13-04；AD-07、GAP-24）。
		h.obs.Mount(r, identity.RequireAdmin, identity.RequirePermission)
		h.gen.Mount(r, identity.RequireUser, wsGuard, func(req *http.Request) int64 {
			id, _ := identityhttp.UserIDFrom(req.Context())
			return id
		})
		// MCP 访问密钥与已连接的客户端（MCPT-03）：只要登录 —— 工作区写在密钥自己身上，
		// 不靠 X-Workspace-Id（建密钥时校验是否为该工作区 active 成员）。MCP 总开关关闭时不挂。
		if h.mcpKeys != nil {
			h.mcpKeys.Mount(r, identity.RequireUser)
		}
		// OAuth 同意页的两个接口（MCPT-12，供用户端同意页 MCPT-13）：只要登录，工作区在请求体里选。
		if h.mcpOAuth != nil {
			// 平台总开关关着时同意接口也 404（拒绝新授权，MCPT-14）。
			r.Group(func(r chi.Router) {
				r.Use(mcphttp.PlatformGate(h.mcpSwitch, nil))
				h.mcpOAuth.MountConsent(r, identity.RequireUser)
			})
		}
		// 工作区管理员的「MCP 连接」与工作区开关（MCPT-14）：要 manage（显式标注，不靠 GET 默认的 view）。
		if h.mcpWorkspace != nil {
			h.mcpWorkspace.Mount(r, identity.RequireUser, wsCtx)
		}
		// 管理端「MCP 连接」（MCPT-14）：读 mcp:read、写 mcp:write。
		h.mcpAdmin.MountAdmin(r, identity.RequireAdmin, identity.RequirePermission)
		// 页面审批（MCPT-13）：只要登录，归属以 quote 所属授权的 user_id 为准。
		if h.mcpApprovals != nil {
			h.mcpApprovals.Mount(r, identity.RequireUser)
		}
	})

	// 单条实时通道（CD-08）。token 走首帧不入 URL。
	r.Handle("/ws/events", wsHandler)

	// 对外 MCP（MCPT-02，M-6 同域）：挂在根上与 /ws/events 并列，不进 /api/v1（端点由 MCP 规范定义，
	// 不进 OpenAPI）。总开关关闭时 h.mcp 为 nil → 三个端点 404。超时见 mcpRoute。
	if h.mcp != nil {
		h.mcp.Mount(r)
	}
	// OAuth 元数据与协议端点（MCPT-12）：/.well-known/oauth-protected-resource[/…]、
	// /.well-known/oauth-authorization-server、/oauth/{authorize,token,revoke,register}。
	// 挂在根上、不进 OpenAPI（由规范定义），见 api/mcp/README.md。随 MCP 总开关。
	// 平台总开关关着时这些端点一律 404（拒绝新授权，MCPT-14）；撤销端点 /oauth/revoke 例外 ——
	// 平台暂停期间用户想收回授权，不该被拦。
	if h.mcpOAuth != nil {
		r.Group(func(r chi.Router) {
			r.Use(mcphttp.PlatformGate(h.mcpSwitch, func(req *http.Request) bool { return req.URL.Path == "/oauth/revoke" }))
			h.mcpOAuth.MountRoot(r)
		})
	}

	return r
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// payGateway 把支付域接成账本域的支付端口。
//
// ⚠️ 这一层只是**转接**：真正的编排在 `payment/app.Service` 里。
// 在这里加逻辑的话，回调路径与"拉起支付"路径会各有一份判断。
type payGateway struct {
	svc *paymentapp.Service
	reg *paymentapp.Registry
}

func (g payGateway) CreatePayment(ctx context.Context, channel string,
	ref paymentapp.OrderRef) (paymentapp.PayParams, error) {
	return g.svc.CreatePayment(ctx, channel, ref)
}

func (g payGateway) SyncOrder(ctx context.Context, channel string,
	order paymentapp.OrderInfo, outTradeNo string) (bool, error) {
	return g.svc.SyncOrder(ctx, channel, order, outTradeNo)
}

func (g payGateway) Channels() []string { return g.reg.Codes() }

// refundChannel 把支付域接成账本域的退款端口。
type refundChannel struct{ reg *paymentapp.Registry }

func (a refundChannel) Refund(ctx context.Context, channel string,
	in paymentapp.RefundRequest) (paymentapp.RefundResult, error) {
	ch, err := a.reg.Get(channel)
	if err != nil {
		return paymentapp.RefundResult{}, err
	}
	return ch.Refund(ctx, in)
}

// reconcileQuery 把支付域接成账本域的对账查询端口。
//
// ⚠️ 这里把 `CallbackEvent` **压成两个标量**（付没付、多少钱）：
// 对账只关心这两件事，而让账本域认识整个回调事件的形状
// 会把支付协议的细节泄漏进账务代码。
type reconcileQuery struct{ reg *paymentapp.Registry }

func (a reconcileQuery) QueryOrder(ctx context.Context, channel, outTradeNo string) (
	bool, int64, error) {
	ch, err := a.reg.Get(channel)
	if err != nil {
		return false, 0, err
	}
	ev, err := ch.QueryOrder(ctx, outTradeNo)
	if err != nil {
		return false, 0, err
	}
	return ev.Succeeded, ev.AmountCents, nil
}

// presenceStore 把 Redis 客户端适配成协作域的在线状态端口。
type presenceStore struct{ c *cache.Client }

func (p presenceStore) Touch(ctx context.Context, key, field, value string,
	ttl time.Duration) error {
	return p.c.PresenceTouch(ctx, key, field, value, ttl)
}

func (p presenceStore) Entries(ctx context.Context, key string) (map[string]string, error) {
	return p.c.PresenceEntries(ctx, key)
}

func (p presenceStore) Drop(ctx context.Context, key, field string) error {
	return p.c.PresenceDrop(ctx, key, field)
}

// workSourceGuard 实现内容域的来源校验端口（P13-02）。
//
// ⚠️ 这一步**不能省，也不能让内容域自己查**：它不知道 generation / render_job
// 那两张表的语义（什么算成功、产出在哪一项）。少了它，任何人都能拿一个
// 别人的 generationId 提交成自己的作品 —— 而作品墙上会出现他没做过的东西。
type workSourceGuard struct {
	gen *genapp.Service
	tl  *timelineapp.Service
}

func (g workSourceGuard) Resolve(r *http.Request, workspaceID int64,
	sourceType string, sourceID int64) (string, int64, error) {
	ctx := r.Context()
	switch sourceType {
	case "generation":
		// ⚠️ `Get` 带 workspaceID —— 归属校验就落在这一句上。
		item, err := g.gen.Get(ctx, workspaceID, sourceID)
		if err != nil {
			return "", 0, err
		}
		if item.Status != "success" {
			// 失败/排队中的任务不能提交：作品墙上不该出现一个还没产出的东西，
			// 而运营会对着一个空白的审核项不知道该怎么办。
			return "", 0, httpx.Errf(errcode.InvalidStateTransition,
				"只有已成功的任务才能提交为作品")
		}
		outs, err := g.gen.Outputs(ctx, workspaceID, sourceID)
		if err != nil {
			return "", 0, err
		}
		// ⚠️ 取**第一个有媒体的**产出而不是硬取 outputs[0]：一次生成可能
		// 先出一条文本再出图（GO-40 的多产物），取 [0] 会拿到一段文字，
		// 而作品墙上那一格就永远是空的。
		for _, o := range outs {
			if o.MediaObjectID != 0 {
				return "", o.MediaObjectID, nil
			}
		}
		return "", 0, httpx.Errf(errcode.InvalidStateTransition, "这次生成没有可展示的媒体产出")
	case "render":
		job, err := g.tl.GetRender(ctx, workspaceID, sourceID)
		if err != nil {
			return "", 0, err
		}
		if job.Status != "success" || job.OutputMediaID == 0 {
			return "", 0, httpx.Errf(errcode.InvalidStateTransition,
				"只有已成功的渲染才能提交为作品")
		}
		return "", job.OutputMediaID, nil
	}
	return "", 0, httpx.Errf(errcode.InvalidArgument, "来源类型非法")
}

// ---------------------------------------------------------------- 运维端口适配

// genCompensator 把生成域接到运维域的 `TaskOps` 端口上。
//
// ⚠️ 这两个方法都只是**转调**：补偿要做的三件事（判失败、按原批次退款、
// 推进组子项与整图运行）全在生成域那一侧。在这里"顺手"实现一遍的话，
// 就会出现第二条改任务状态的路径，而它一定漏掉其中某一件。
type genCompensator struct{ svc *genapp.Service }

func (a genCompensator) Describe(ctx context.Context, id int64) (opsapp.TaskFacts, error) {
	f, err := a.svc.DescribeForOps(ctx, id)
	if err != nil {
		return opsapp.TaskFacts{}, err
	}
	return opsapp.TaskFacts{Finished: f.Finished, DeadlineAt: f.DeadlineAt, Credits: f.Credits}, nil
}

func (a genCompensator) Compensate(ctx context.Context, id int64, reason string) (bool, error) {
	return a.svc.CompensateFail(ctx, id, reason)
}

type renderCompensator struct{ svc *timelineapp.Service }

func (a renderCompensator) Describe(ctx context.Context, id int64) (opsapp.TaskFacts, error) {
	f, err := a.svc.DescribeRenderForOps(ctx, id)
	if err != nil {
		return opsapp.TaskFacts{}, err
	}
	return opsapp.TaskFacts{Finished: f.Finished, DeadlineAt: f.DeadlineAt, Credits: f.Credits}, nil
}

func (a renderCompensator) Compensate(ctx context.Context, id int64, reason string) (bool, error) {
	return a.svc.CompensateRender(ctx, id, reason)
}

// opsRetrier 把生成域的重试接给运维域。
//
// ⚠️ 走的是**用户那条一模一样的 `Retry`**：重新校验、重新报价、重新扣费，
// 费用记原任务的付费主体。后台没有"免费重试"这条路 —— 平台替用户吃掉上游
// 成本是一个商业决定，不能由一个运维按钮隐式做出。
type opsRetrier struct{ svc *genapp.Service }

func (a opsRetrier) RetryForOps(ctx context.Context, id int64, key string) (int64, int, error) {
	res, err := a.svc.RetryForOps(ctx, id, key)
	if err != nil {
		return 0, 0, err
	}
	return res.Generation.ID, res.Generation.CreditsCost, nil
}

// mediaRefChecker 把各域 repo 的 `MediaReferences` 适配成 `media/app.ReferenceChecker`。
//
// ⚠️ 这是**计算式**的删除保护（P14-03）：删除/回收时当场去问各域"还在用吗"，
// 而不是指望每个写入点都记得往 `media_usage` 登记一条。
// 登记式漏掉任何一处**都不会有编译错误** —— 实测八个 UsageType 只有两个
// 真的有人登记，于是"没人用"对几乎每个素材都成立。
type mediaRefChecker struct {
	kind string
	fn   func(context.Context, int64) ([]int64, error)
}

func (c mediaRefChecker) Kind() string { return c.kind }
func (c mediaRefChecker) References(ctx context.Context, mediaID int64) ([]int64, error) {
	return c.fn(ctx, mediaID)
}

// skillModels 把模型目录接成技能配置的 SkillModels 端口（AGT-60：绑定模型的校验与下拉选项）。
type skillModels struct{ catalog *catalogapp.Service }

func (m skillModels) SkillModels(ctx context.Context) ([]agentapp.SkillModelInfo, error) {
	list, err := m.catalog.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]agentapp.SkillModelInfo, 0, len(list))
	for _, x := range list {
		out = append(out, agentapp.SkillModelInfo{Code: x.Code, Name: x.Name, Domain: x.Domain,
			Status: x.Status, AgentCapable: x.IsAgentCapable})
	}
	return out, nil
}

// projectAgents 把画布仓储的 AgentSummaries 接成内容域的 ProjectAgents 端口（AGT-45）。
type projectAgents struct{ repo *canvasrepo.Repo }

func (p projectAgents) ProjectAgents(ctx context.Context, workspaceID int64,
	projectIDs []int64) (map[int64]contentapp.ProjectAgent, error) {
	m, err := p.repo.AgentSummaries(ctx, workspaceID, projectIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]contentapp.ProjectAgent, len(m))
	for id, b := range m {
		out[id] = contentapp.ProjectAgent{SessionID: b.SessionID, Title: b.Title,
			State: b.State, LastActiveAt: b.LastActiveAt}
	}
	return out, nil
}

// checkSchema 比对二进制与数据库的迁移版本。
//
// ⚠️ **两个方向处置不同**（见 migrate.CheckSchema）：
// 库落后 = 漏跑迁移 = 必然坏 → 拒绝启动；
// 库超前 = 回退窗口里的正常状态 → 放行但告警（在这里拦死的话，回退就做不了）。
func checkSchema(deps *bootstrap.Deps, logger *slog.Logger) error {
	if deps.DB == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	state, fatal, msg, err := migrate.CheckSchema(ctx, deps.DB.Pool, migrations.FS, migrations.Dir)
	if err != nil {
		// ⚠️ 读不到版本**不拦**：新库第一次起来时 schema_migration 还不存在，
		// 而那时该报的错是"迁移没跑"，由下游的具体查询给出更准的信息。
		logger.Warn("无法核对 schema 版本", "error", err)
		return nil
	}
	if fatal {
		logger.Error("schema 版本不匹配，拒绝启动",
			"applied", state.Applied, "expected", state.Expected, "detail", msg)
		return fmt.Errorf("schema 版本不匹配：库第 %d 版，二进制需要第 %d 版，请先执行 `migrate up`",
			state.Applied, state.Expected)
	}
	if msg != "" {
		logger.Warn("schema 版本比二进制新",
			"applied", state.Applied, "expected", state.Expected, "detail", msg)
		return nil
	}
	logger.Info("schema 版本已核对", "version", state.Applied)
	return nil
}

// newWSHandler 建实时通道（`/ws/events`）处理器。
//
// ⚠️ 来源白名单要显式给：`Handler.AllowedOrigins` 为空 = 只接受同源，而此前从没有任何地方给它赋过值 ——
// 开发时页面（:3000）与 API（:8180）不同源，握手一律被拒（`Origin ... is not authorized`），
// 实时通道从来没连上过，前端每 1.5 秒重连一次而界面只显示「离线」（CD-73）。
// 生产里页面与 API 同域（反向代理），留空即可。
func newWSHandler(hub *ws.Hub, auth ws.Authenticator, topics ws.TopicAuthorizer,
	logger *slog.Logger, origins []string) *ws.Handler {
	h := ws.NewHandler(hub, auth, topics, logger)
	h.AllowedOrigins = origins
	if len(origins) > 0 {
		logger.Info("实时通道允许跨源", "origins", origins)
	}
	return h
}
