package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	agentrepo "github.com/li904338203/tabtv/backend/internal/agent/repo"
	agenttools "github.com/li904338203/tabtv/backend/internal/agent/tools"
	billingapp "github.com/li904338203/tabtv/backend/internal/billing/app"
	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	collabrepo "github.com/li904338203/tabtv/backend/internal/collaboration/repo"
	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
	genrepo "github.com/li904338203/tabtv/backend/internal/generation/repo"
	marketingapp "github.com/li904338203/tabtv/backend/internal/marketing/app"
	marketingrepo "github.com/li904338203/tabtv/backend/internal/marketing/repo"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	mediadomain "github.com/li904338203/tabtv/backend/internal/media/domain"
	"github.com/li904338203/tabtv/backend/internal/media/relay"
	opsapp "github.com/li904338203/tabtv/backend/internal/ops/app"
	opsrepo "github.com/li904338203/tabtv/backend/internal/ops/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/bootstrap"
	"github.com/li904338203/tabtv/backend/internal/platform/config"
	"github.com/li904338203/tabtv/backend/internal/platform/ffsem"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/memqueue"
	"github.com/li904338203/tabtv/backend/internal/platform/objstore"
	"github.com/li904338203/tabtv/backend/internal/platform/settings"
	providerapp "github.com/li904338203/tabtv/backend/internal/provider/app"
	providerrepo "github.com/li904338203/tabtv/backend/internal/provider/repo"
	"github.com/li904338203/tabtv/backend/internal/provider/transport"
	scriptrepo "github.com/li904338203/tabtv/backend/internal/script/repo"
	smartcutapp "github.com/li904338203/tabtv/backend/internal/smartcut/app"
	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	timelinerepo "github.com/li904338203/tabtv/backend/internal/timeline/repo"
	"github.com/li904338203/tabtv/backend/internal/timeline/typeset"
	"github.com/li904338203/tabtv/backend/internal/wiring"
	workflowrepo "github.com/li904338203/tabtv/backend/internal/workflow/repo"
)

// background 是原 Worker 进程的全部工作 + 原 API 进程里的扫描循环（TT-T-07）：
// 队列消费、执行器、渲染、Agent 轮次、恢复扫描与各种清理。
//
// ⚠️ 来源系统里一部分循环在 API（整图派发、同步模型调用恢复、AI 搭景 / AI 剪辑恢复、识别续接），
// 一部分在 Worker（执行、渲染、Agent、回收站与媒体回收、日志清理）。整图运行尤其是两边各做一半：
// Worker 推进节点状态、API 的扫描派发下一批 —— 合并时两段都保留，缺一段整次运行就会停住（TTR-4）。
type background struct {
	cfg          config.Config
	deps         *bootstrap.Deps
	gen          *id.Generator
	logger       *slog.Logger
	media        *mediaapp.Service
	catalog      *catalogapp.Service
	core         *wiring.Core
	timeline     *timelineapp.Service
	smartcut     *smartcutapp.Service
	agent        *agentapp.Service
	analysisWake *wiring.AnalysisWake
	billing      *billingrepo.Repo
	settings     *settings.Store

	broker *memqueue.Broker
	wg     sync.WaitGroup
}

// 队列名与 topic（与来源系统一致，死信记录里能对得上）。
const (
	queueGeneration = "generation.jobs"
	queueExport     = "export.jobs"
	queueRender     = "render.jobs"
)

// outboxReplayWindow：启动时把这段时间内已投递的 outbox 重新投一遍（见 replayOutbox）。
const outboxReplayWindow = 24 * time.Hour

func (b *background) go_(ctx context.Context, f func(context.Context)) {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		f(ctx)
	}()
}

// Start 启动全部后台工作。ctx 取消后各循环退出；Wait 等它们收尾。
func (b *background) Start(ctx context.Context) error {
	cfg, deps, log, pool := b.cfg, b.deps, b.logger, b.deps.DB.Pool
	nextID := b.gen.Next
	ffSem := ffsem.New(cfg.Smartcut.FFmpegMaxConcurrent)

	opsSvc := opsapp.New(opsrepo.New(pool, nextID))
	opsSvc.SetLogger(log)
	b.broker = memqueue.New(log, func(ctx context.Context, queue, topic, reason string, body []byte) error {
		return opsSvc.RecordDead(ctx, queue, topic, reason, body)
	})

	// ---- 回收站、媒体回收、导出包清理、封面帧、预览素材分析
	canvasSvc := b.core.Canvas
	canvasSvc.SetDirectorMedia(wiring.DirectorMediaStore(b.media))
	cleaner := canvasapp.NewCleaner(canvasSvc, log, alwaysLock{})
	cleaner.SetTimelineSweeper(timelinerepo.New(pool, nextID))
	b.go_(ctx, cleaner.Run)
	b.go_(ctx, mediaapp.NewCollector(b.media, log).Run)
	b.go_(ctx, mediaapp.NewExportCleaner(b.media, log).Run)
	if mediadomain.FFmpegAvailable() {
		b.go_(ctx, mediaapp.NewPosterScanner(b.media, log, alwaysLock{}).Run)
		analysis := mediaapp.NewAnalysisScanner(b.media, log, ffSem)
		analysis.SetInterval(cfg.Smartcut.AnalysisScanInterval)
		analysis.SetLease(cfg.Smartcut.AnalysisLease)
		analysis.SetWake(b.analysisWake.Wakeups())
		b.go_(ctx, analysis.Run)
	} else {
		log.Warn("找不到 ffmpeg：视频没有封面帧，剪辑台没有预览版与波形，渲染会失败（设置 → 环境自检）")
	}

	// ---- 执行器（生成任务）
	genRepo := genrepo.New(pool)
	providers := b.buildProvider(ctx, ffSem)
	executor := genapp.NewExecutor(genRepo, providers.routing, log)
	executor.SetRefunder(refunderAdapter{b.billing, cfg.CompensationValidFor})
	executor.SetIDGen(nextID)
	executor.SetArchiver(archiverAdapter{b.media})
	executor.SetWorkRegistrar(genRepo)
	executor.SetAssetBinder(b.core.Script)
	executor.SetGroupBinder(b.core.Script)
	executor.SetSmartcutBinder(wiring.SmartcutBinder{Svc: b.smartcut})
	executor.SetRunAdvancer(b.core.Workflow)
	executor.SetEvents(genapp.NewBusEventPublisher(deps.Bus))
	if cfg.GenerationMaxRunningPerPayer > 0 {
		executor.MaxRunningPerPayer = cfg.GenerationMaxRunningPerPayer
	}
	// 并发上限改了即生效（设置 → 常规）。执行器每次领任务时读这个字段，单个 int 的写入无需加锁。
	if b.settings != nil {
		b.settings.Watch(settings.GenerationMaxRunning, func(v any) {
			if n, ok := v.(int); ok && n > 0 {
				executor.MaxRunningPerPayer = n
			}
		})
	}
	b.broker.Register(queueGeneration, gendomain.OutboxTopic, max(cfg.Queue.Prefetch, 4),
		func(c context.Context, body []byte) memqueue.Action {
			var msg gendomain.OutboxPayload
			if err := json.Unmarshal(body, &msg); err != nil {
				log.Error("消息格式非法，已转入死信", "error", err)
				return memqueue.Dead
			}
			gid, _ := strconv.ParseInt(msg.GenerationID, 10, 64)
			outcome, err := executor.Execute(c, gid)
			if err != nil {
				log.Error("执行失败", "generationId", gid, "outcome", outcome, "error", err)
			}
			switch outcome {
			case genapp.OutcomeDuplicate:
				return memqueue.Discard
			case genapp.OutcomeRequeue, genapp.OutcomeRetry:
				return memqueue.Requeue
			case genapp.OutcomeDead:
				log.Warn("任务转入死信待核对", "generationId", gid)
				return memqueue.Dead
			}
			return memqueue.Done
		})

	// ---- 批量导出
	b.broker.Register(queueExport, mediaapp.ExportTopic, 2, func(c context.Context, body []byte) memqueue.Action {
		var msg mediaapp.ExportPayload
		if err := json.Unmarshal(body, &msg); err != nil {
			return memqueue.Dead
		}
		eid, _ := strconv.ParseInt(msg.ExportID, 10, 64)
		outcome, err := b.media.RunExport(c, eid, log)
		if err != nil {
			log.Error("导出执行失败", "exportId", eid, "error", err)
		}
		switch outcome {
		case mediaapp.ExportDuplicate:
			return memqueue.Discard
		case mediaapp.ExportRetry:
			return memqueue.Requeue
		}
		return memqueue.Done
	})

	// ---- 成片渲染（独立队列；CPU 密集，并发默认 1）
	renderer := timelineapp.NewRenderer(timelinerepo.New(pool, nextID), log)
	renderer.SetFetcher(renderFetcher{b.media})
	renderer.SetArchiver(renderArchiver{b.media})
	renderer.SetRefunder(renderRefunder{b.billing, cfg.CompensationValidFor})
	renderer.SetNextID(nextID)
	renderer.SetFFmpegSem(ffSem)
	renderer.SetEvents(deps.Bus)
	renderer.SetTypesetter(typeset.New(typeset.Options{}))
	renderer.MaxConcurrent = 1
	if b.settings != nil {
		renderer.MaxConcurrent = b.settings.Int(settings.RenderMaxConcurrent)
		b.settings.Watch(settings.RenderMaxConcurrent, func(v any) {
			if n, ok := v.(int); ok && n > 0 {
				renderer.MaxConcurrent = n
			}
		})
	}
	b.broker.Register(queueRender, timelineapp.RenderTopic, 1, func(c context.Context, body []byte) memqueue.Action {
		var msg timelineapp.RenderPayload
		if err := json.Unmarshal(body, &msg); err != nil {
			return memqueue.Dead
		}
		rid, _ := strconv.ParseInt(msg.RenderID, 10, 64)
		outcome, err := renderer.Run(c, rid)
		if err != nil {
			log.Error("渲染执行失败", "renderId", rid, "error", err)
		}
		switch outcome {
		case timelineapp.RenderDuplicate:
			return memqueue.Discard
		case timelineapp.RenderRequeue:
			return memqueue.Requeue
		}
		return memqueue.Done
	})
	b.go_(ctx, every(time.Minute, false, func(c context.Context) { renderer.RecoverStale(c) }))

	// ---- 画布 Agent 轮次
	if cfg.Agent.Enabled {
		if err := b.startAgent(ctx, providers, genRepo); err != nil {
			return err
		}
	}

	// ---- 启动：先重放 outbox（进程异常退出时队列里的消息会丢，消费者都是幂等的），再起投递器与消费者
	if n, err := replayOutbox(ctx, pool, outboxReplayWindow); err != nil {
		log.Warn("重放 outbox 失败（恢复扫描会兜底）", "error", err)
	} else if n > 0 {
		log.Info("已重放启动前未确认完成的消息", "count", n)
	}
	b.broker.Start(ctx)
	b.go_(ctx, genapp.NewRelay(genRepo, b.broker, log).Run)

	// ---- 恢复扫描与周期作业（原 Worker）
	b.go_(ctx, every(time.Minute, true, func(c context.Context) {
		if n := executor.RecoverStale(c, 10*time.Minute, 200); n > 0 {
			log.Info("恢复扫描处理完成", "handled", n)
		}
		if n := executor.RecoverRefunds(c, time.Minute, 100); n > 0 {
			log.Info("补退款完成", "refunded", n)
		}
	}))
	provLogs := providerrepo.New(pool, nextID)
	b.go_(ctx, every(time.Hour, true, func(c context.Context) {
		retention := cfg.LogRetention
		if b.settings != nil {
			retention = time.Duration(b.settings.Int(settings.RequestLogRetention)) * 24 * time.Hour
		}
		if n, err := provLogs.CleanupBefore(c, time.Now().Add(-retention), 50000); err != nil {
			log.Warn("清理上游调用日志失败", "error", err)
		} else if n > 0 {
			log.Info("清理过期上游调用日志", "rows", n)
		}
		if n, err := canvasSvc.CleanupOpLog(c, canvasapp.DefaultOpLogRetention, 50000, time.Now()); err != nil {
			log.Warn("清理画布操作日志失败", "error", err)
		} else if n > 0 {
			log.Info("清理过期画布操作日志", "rows", n)
		}
	}))

	// ---- 原 API 进程的扫描循环
	b.go_(ctx, every(2*time.Second, false, func(c context.Context) {
		if _, err := b.core.Workflow.Tick(c, 50); err != nil {
			log.Warn("整图运行扫描失败", "error", err)
		}
	}))
	b.go_(ctx, every(5*time.Minute, true, func(c context.Context) {
		if n, err := b.core.Script.RecoverStaleCalls(c); err != nil {
			log.Warn("同步模型调用恢复扫描失败", "error", err)
		} else if n > 0 {
			log.Info("同步模型调用恢复扫描：已判失败", "count", n)
		}
		if b.agent != nil && cfg.Agent.Enabled {
			if _, err := b.agent.RecoverStaleOptimizes(c); err != nil {
				log.Warn("提示词优化恢复扫描失败", "error", err)
			}
		}
		if _, err := b.core.Director.RecoverStaleCalls(c); err != nil {
			log.Warn("AI 搭景恢复扫描失败", "error", err)
		}
		_, _ = b.core.Director.CleanupCalls(c)
		if _, err := b.smartcut.RecoverStaleCalls(c); err != nil {
			log.Warn("AI 剪辑恢复扫描失败", "error", err)
		}
		_, _ = b.smartcut.CleanupCalls(c)
	}))
	b.go_(ctx, every(3*time.Second, false, func(c context.Context) {
		if _, err := b.smartcut.ResumePendingTranscribes(c); err != nil {
			log.Warn("语音识别续接扫描失败", "error", err)
		}
	}))
	return nil
}

// Wait 等全部后台循环退出（ctx 取消之后）。
func (b *background) Wait(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

type providers struct {
	routing genapp.Provider
	http    *providerapp.HTTPProvider
}

// buildProvider 组装执行器的上游适配器（原 Worker 的 buildProvider）。目录复用装配里那一份。
func (b *background) buildProvider(ctx context.Context, ffSem *ffsem.Sem) providers {
	cfg, log := b.cfg, b.logger
	policy := transport.URLPolicy{AllowPrivate: cfg.Dev.AllowPrivateNetwork}
	h := providerapp.NewHTTPProvider(transport.NewClient(policy), b.catalog, policy, log)
	h.SetRequestLogger(providerrepo.New(b.deps.DB.Pool, b.gen.Next))
	h.SetFFmpegSem(ffSem)
	store := b.deps.ObjStore
	h.SetMediaResolver(mediaRefAdapter{b.media, store})
	var relayScratch providerapp.ScratchStore
	if rc := cfg.RelayStorage; rc.Endpoint != "" {
		if rs, err := objstore.New(context.Background(), rc); err != nil {
			log.Error("建立公网中转存储客户端失败，只收地址的上游将拿不到参考媒体", "error", err)
		} else {
			h.SetMediaRelay(relay.New(b.media, rs, cfg.RelayPrefix, log))
			relayScratch = prefixedScratch{rs, cfg.RelayPrefix}
			// 成功时也记一行：排查「参考图为什么发不出去」时第一个要确认的就是中转开没开。
			log.Info("公网中转存储已启用", "endpoint", rc.Endpoint, "bucket", rc.Bucket, "prefix", cfg.RelayPrefix)
		}
	}
	h.SetScratch(store, relayScratch)
	b.go_(ctx, providerapp.NewScratchSweeper(store, log).Run)

	var dev genapp.Provider
	if cfg.Dev.FakeProvider {
		dev = genapp.DevProvider{}
		log.Warn("开发假适配器已启用，dev_fake 协议的模型不会调用真实上游")
	}
	return providers{routing: providerapp.NewRouting(h, dev), http: h}
}

// startAgent 装 Agent 的工具注册表、轮次消费者与扫描循环（原 Worker 的 Agent 段）。
func (b *background) startAgent(ctx context.Context, p providers, genRepo *genrepo.Repo) error {
	cfg, log, pool, nextID := b.cfg, b.logger, b.deps.DB.Pool, b.gen.Next
	bus := b.deps.Bus
	core := b.core
	toolReg, err := agenttools.NewRegistry()
	if err != nil {
		return err
	}
	marketing := wiring.NewMarketingServiceFor(cfg, pool, b.catalog)
	paidIn := wiring.AgentPaidToolsInput{
		AgentRepo: agentrepo.New(pool), Gen: core.Gen, Catalog: b.catalog,
		Canvas: core.Canvas, Billing: b.billing, Bus: bus, NextID: nextID, Logger: log,
		Script: core.Script, Marketing: marketing,
	}
	register := func(ts []agentapp.Tool) error {
		for _, t := range ts {
			if err := toolReg.Register(t); err != nil {
				return err
			}
		}
		return nil
	}
	if err := register(wiring.NewAgentPaidTools(paidIn)); err != nil {
		return err
	}
	if err := register(wiring.NewAgentPlanningTools(wiring.AgentPlanningToolsInput{
		AgentRepo: agentrepo.New(pool), Canvas: core.Canvas, GenRepo: core.GenRepo,
		Media: b.media, Script: core.Script, Catalog: b.catalog,
		Billing: billingapp.New(b.billing), Bus: bus, NextID: nextID, Logger: log,
		Marketing: marketing, MaxVariants: cfg.Marketing.MaxVariants,
	})); err != nil {
		return err
	}
	if err := register(wiring.NewAgentMultiTools(wiring.AgentMultiToolsInput{
		Paid: paidIn, Script: core.Script, ScriptRepo: scriptrepo.New(pool),
		Workflow: core.Workflow, Timeline: b.timeline, GenRepo: core.GenRepo,
		Director: core.Director, Smartcut: b.smartcut, SmartcutAIEnabled: cfg.Smartcut.AIEnabled,
	})); err != nil {
		return err
	}
	scene := wiring.NewAgentSceneContext(wiring.AgentSceneInput{
		Marketing: wiring.NewMarketingService(wiring.MarketingServiceInput{
			Repo: marketingrepo.New(pool), Catalog: b.catalog,
			Settings: marketingapp.Settings{MaxProductImages: cfg.Marketing.MaxProductImages,
				MaxVariants: cfg.Marketing.MaxVariants, AgentEnabled: cfg.Agent.Enabled},
		}),
		Prompts: b.catalog, Script: core.Script, Variants: cfg.Marketing.MaxVariants, Logger: log,
	})
	if err := register(wiring.NewAgentMarketingTools(scene)); err != nil {
		return err
	}
	images := wiring.AgentSceneImages{Catalog: b.catalog, Media: b.media, Width: cfg.Marketing.ReasoningImageWidth}
	if err := toolReg.Register(wiring.NewAgentViewMediaTool(agentrepo.New(pool), images)); err != nil {
		return err
	}
	runner := wiring.NewAgentRunner(wiring.AgentRunnerInput{
		AgentRepo: agentrepo.New(pool), Chat: p.http, Catalog: b.catalog,
		Billing: b.billing, BillingSvc: billingapp.New(b.billing), Collab: collabrepo.New(pool),
		Canvas: core.Canvas, Bus: bus, Tools: toolReg, Compensate: cfg.CompensationValidFor,
		NextID: nextID, Logger: log,
		MaxCalls: cfg.Agent.TurnMaxCalls, ContextChars: cfg.Agent.ContextChars, Scene: scene,
		Images: images, MaxOutputTokens: cfg.Agent.MaxOutputTokens,
	})
	// 每个消费协程各自一个租约持有者（runner.Fork），与来源系统「多个消费者」一致。
	runners := make(chan *agentapp.Runner, cfg.Agent.RunnerConcurrency)
	for i := 0; i < max(cfg.Agent.RunnerConcurrency, 1); i++ {
		runners <- runner.Fork()
	}
	b.broker.Register(agentapp.TurnQueue, agentapp.TurnTopic, max(cfg.Agent.RunnerConcurrency, 1),
		func(c context.Context, body []byte) memqueue.Action {
			rn := <-runners
			defer func() { runners <- rn }()
			outcome, err := rn.HandleMessage(c, body)
			if errors.Is(err, agentapp.ErrBadMessage) {
				return memqueue.Dead
			}
			if err != nil {
				log.Warn("agent 轮次结束时带错误", "outcome", outcome.String(), "error", err)
			}
			switch outcome {
			case agentapp.OutcomeDuplicate, agentapp.OutcomeBusy, agentapp.OutcomeLeaseLost:
				return memqueue.Discard
			case agentapp.OutcomeRequeue:
				return memqueue.Requeue
			}
			return memqueue.Done
		})
	scanner := wiring.NewAgentScanner(wiring.AgentScannerInput{
		AgentRepo: agentrepo.New(pool), GenRepo: genRepo, Billing: b.billing, Bus: bus,
		Compensate: cfg.CompensationValidFor, NextID: nextID, Logger: log,
		MaxCalls: cfg.Agent.TurnMaxCalls, CardExpire: cfg.Agent.CardExpire,
		ScriptRepo: scriptrepo.New(pool), WorkflowRepo: workflowrepo.New(pool),
		TimelineRepo: timelinerepo.New(pool, nextID),
	})
	b.go_(ctx, every(agentapp.ScanInterval, true, func(c context.Context) {
		if _, err := scanner.Tick(c, 0); err != nil && c.Err() == nil {
			log.Warn("agent 扫描部分失败（下次扫描重试）", "error", err)
		}
	}))
	return nil
}

// replayOutbox 把最近一段时间内已投递、但对应任务可能还没做完的 outbox 消息标回未投递。
//
// 进程内队列不持久：上次运行时已投进队列、还没处理完就退出的消息丢了。消费者全部幂等
// （已完成的任务得到 Duplicate 直接丢弃），所以宁可多投，不能少投。窗口之外的由恢复扫描兜底。
func replayOutbox(ctx context.Context, pool *pgxpool.Pool, window time.Duration) (int64, error) {
	tag, err := pool.Exec(ctx, `
		update outbox_message set published_at = null
		where published_at is not null and published_at > now() - make_interval(secs => $1)`,
		window.Seconds())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// every 周期执行 f；immediate 为 true 时启动即先跑一次。
func every(interval time.Duration, immediate bool, f func(context.Context)) func(context.Context) {
	return func(ctx context.Context) {
		if immediate {
			f(ctx)
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				f(ctx)
			}
		}
	}
}

// alwaysLock：单进程不需要跨实例互斥，锁永远拿得到（来源系统的锁本来就只为省重复劳动，不承担正确性）。
type alwaysLock struct{}

func (alwaysLock) TryLock(context.Context, string, time.Duration) (bool, error) { return true, nil }
