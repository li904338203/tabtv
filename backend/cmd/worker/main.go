// Command worker 是异步任务执行进程。
//
// 职责（08 §2）：消费 generation / script LLM / DAG / timeline-render，
// 调用 Provider，归档产物，推进状态。
// 不负责：处理浏览器会话；读取未经归属验证的 URL；改变后台配置。
// 本进程**不暴露用户写接口** —— 只开一个供编排系统探活的健康端口。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	agentrepo "github.com/li904338203/tabtv/backend/internal/agent/repo"
	agenttools "github.com/li904338203/tabtv/backend/internal/agent/tools"
	billingapp "github.com/li904338203/tabtv/backend/internal/billing/app"
	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	canvasrepo "github.com/li904338203/tabtv/backend/internal/canvas/repo"
	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	catalogrepo "github.com/li904338203/tabtv/backend/internal/catalog/repo"
	collabapp "github.com/li904338203/tabtv/backend/internal/collaboration/app"
	collabrepo "github.com/li904338203/tabtv/backend/internal/collaboration/repo"
	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
	genrepo "github.com/li904338203/tabtv/backend/internal/generation/repo"
	marketingapp "github.com/li904338203/tabtv/backend/internal/marketing/app"
	marketingrepo "github.com/li904338203/tabtv/backend/internal/marketing/repo"
	mcpdomain "github.com/li904338203/tabtv/backend/internal/mcp/domain"
	mcprepo "github.com/li904338203/tabtv/backend/internal/mcp/repo"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	mediadomain "github.com/li904338203/tabtv/backend/internal/media/domain"
	"github.com/li904338203/tabtv/backend/internal/media/relay"
	mediarepo "github.com/li904338203/tabtv/backend/internal/media/repo"
	obsapp "github.com/li904338203/tabtv/backend/internal/observability/app"
	obsrepo "github.com/li904338203/tabtv/backend/internal/observability/repo"
	opsapp "github.com/li904338203/tabtv/backend/internal/ops/app"
	opsrepo "github.com/li904338203/tabtv/backend/internal/ops/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/bootstrap"
	"github.com/li904338203/tabtv/backend/internal/platform/buildinfo"
	"github.com/li904338203/tabtv/backend/internal/platform/cache"
	"github.com/li904338203/tabtv/backend/internal/platform/config"
	"github.com/li904338203/tabtv/backend/internal/platform/eventbus"
	"github.com/li904338203/tabtv/backend/internal/platform/ffsem"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	plog "github.com/li904338203/tabtv/backend/internal/platform/log"
	"github.com/li904338203/tabtv/backend/internal/platform/migrate"
	"github.com/li904338203/tabtv/backend/internal/platform/mq"
	"github.com/li904338203/tabtv/backend/internal/platform/objstore"
	providerapp "github.com/li904338203/tabtv/backend/internal/provider/app"
	providerrepo "github.com/li904338203/tabtv/backend/internal/provider/repo"
	"github.com/li904338203/tabtv/backend/internal/provider/transport"
	scriptapp "github.com/li904338203/tabtv/backend/internal/script/app"
	scriptrepo "github.com/li904338203/tabtv/backend/internal/script/repo"
	smartcutapp "github.com/li904338203/tabtv/backend/internal/smartcut/app"
	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	timelinedomain "github.com/li904338203/tabtv/backend/internal/timeline/domain"
	timelinerepo "github.com/li904338203/tabtv/backend/internal/timeline/repo"
	"github.com/li904338203/tabtv/backend/internal/timeline/typeset"
	"github.com/li904338203/tabtv/backend/internal/wiring"
	workflowapp "github.com/li904338203/tabtv/backend/internal/workflow/app"
	workflowrepo "github.com/li904338203/tabtv/backend/internal/workflow/repo"
	"github.com/li904338203/tabtv/backend/migrations"
)

func main() {
	if err := run(); err != nil {
		slog.Error("worker 启动失败", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := plog.New(cfg.Log.Level, cfg.Log.Format)
	slog.SetDefault(logger)
	logger.Info("worker 启动中",
		"env", cfg.Env,
		"queuePrefix", cfg.Queue.ExchangePrefix,
		"prefetch", cfg.Queue.Prefetch,
		"version", buildinfo.Version,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Worker 同样需要四个依赖：读不可变快照、领任务、归档产物。
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

	// 健康端口与业务无关，仅供编排系统探活。
	health := &http.Server{
		Addr:              envOr("SEEAITV_WORKER_HEALTH_ADDR", ":8190"),
		Handler:           healthRouter(deps),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// 回收站清理（P04-01）。放在 Worker 不放 API：这是后台作业，
	// 一次删几万行的锁竞争不该打到用户的保存请求上。
	gen, err := id.New(0)
	if err != nil {
		return err
	}
	canvasSvc := canvasapp.New(canvasrepo.New(deps.DB.Pool), gen.Next)
	if cfg.TrashRetention > 0 {
		canvasSvc.SetRetention(cfg.TrashRetention)
	}
	canvasSvc.SetLogger(logger)
	// 回收站清理连同项目的时间线一起删（SCT-07，SCB-11）；孤儿时间线随同一个清理作业回收（SC-F-06）。
	canvasSvc.SetProjectTimelines(timelinerepo.New(deps.DB.Pool, gen.Next))

	// 媒体回收（P04-04）：无引用且超保留期的对象连同库行一起收掉。
	// 与回收站清理分开跑：这个要调对象存储，失败模式与重试策略都不一样。
	mediaSvc := mediaapp.New(mediarepo.New(deps.DB.Pool), deps.ObjStore, gen.Next)
	// 回收器永久删除前要问同一批来源（P14-03）—— 见 cmd/api 里同名的那段。
	mediaSvc.SetLogger(logger)
	// Worker 里也可能排预览素材分析（识别前抽音轨，SCT-33），排完一样要叫醒扫描器。
	mediaSvc.SetAnalysisWaker(wiring.AnalysisWaker(deps.Cache))
	// ffmpeg 并发信号量（SCD-30）：渲染与预览素材扫描器共用，管的是「这台机器同时跑几个 ffmpeg」。
	ffSem := ffsem.New(cfg.Smartcut.FFmpegMaxConcurrent)
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
	// 回收站清理之后要把导演台的文件交给媒体域软删（DRD-30）。
	// ⚠️ 必须在上面的检查器全部注册之后再启动清理：释放走 mediaSvc.Delete，少一个检查器，
	// 复制出来的项目还在用的截图就会被一起删掉。
	canvasSvc.SetDirectorMedia(wiring.DirectorMediaStore(mediaSvc))
	cleaner := canvasapp.NewCleaner(canvasSvc, logger, redisLocker{deps.Cache})
	cleaner.SetTimelineSweeper(timelinerepo.New(deps.DB.Pool, gen.Next))
	go cleaner.Run(ctx)
	collector := mediaapp.NewCollector(mediaSvc, logger)
	go collector.Run(ctx)

	// 邀请过期清理（P12-01）。
	//
	// ⚠️ 它不只是"整理数据"：uk_invite_pending 的条件是 status='pending'，
	// 过期的行若永远停在 pending，同一个人就再也邀请不了第二次 ——
	// 而用户看到的错误会是「名称已存在」，指向一个完全无关的方向。
	collabSvc := collabapp.New(collabrepo.New(deps.DB.Pool), gen.Next)
	go collabapp.NewInvitationCleaner(collabSvc, logger, redisLocker{deps.Cache}).Run(ctx)

	// 过期导出包清理（P09-03）。
	// ⚠️ 它只影响存储成本，不影响正确性：对外状态由 expires_at 算出来，
	// 这个作业停了也不会有人下到一个 404。
	go mediaapp.NewExportCleaner(mediaSvc, logger).Run(ctx)

	// 视频封面帧（CD-83）：给生成与成片的视频截一张代表画面（默认最后一帧，黑场往前找），
	// 项目封面靠它把视频算进来。⚠️ 不在出片主路径上截 —— 截帧失败不能让已归档的视频变成失败。
	if mediadomain.FFmpegAvailable() {
		go mediaapp.NewPosterScanner(mediaSvc, logger, redisLocker{deps.Cache}).Run(ctx)
		// 剪辑台的预览素材（SCT-16，SCD-30）：预览版、拼图、波形、识别用音轨、节拍。只处理排进来的行，不扫全库。
		analysis := mediaapp.NewAnalysisScanner(mediaSvc, logger, ffSem)
		analysis.SetInterval(cfg.Smartcut.AnalysisScanInterval)
		analysis.SetLease(cfg.Smartcut.AnalysisLease)
		analysis.SetWake(wiring.AnalysisWakeups(ctx, deps.Cache, logger))
		go analysis.Run(ctx)
		logger.Info("预览素材扫描器已启动", "ffmpegMaxConcurrent", ffSem.Cap(),
			"interval", cfg.Smartcut.AnalysisScanInterval, "lease", cfg.Smartcut.AnalysisLease)
	} else {
		logger.Warn("本机没有 ffmpeg，视频不会有封面帧 —— 只出过视频的项目卡片会一直是占位图；" +
			"剪辑台也不会有预览版、拼图与波形")
	}

	// 积分过期扫描（P05-01）。到期批次要标记并**记流水** ——
	// 只改状态不记流水的话，用户的余额凭空变少而账本里找不到任何解释。
	billingRepo := billingrepo.New(deps.DB.Pool, gen.Next)
	go runExpiryScan(ctx, billingRepo, logger, redisLocker{deps.Cache})

	// outbox 投递器（P05-02）：把已提交的受理消息发到 MQ。
	// ⚠️ 它保证 at-least-once —— "标记已发"与"MQ 收到"之间永远有窗口，
	// 因此消费端必须幂等（先查状态再执行），那是 Worker 的责任。
	if deps.MQ != nil {
		relay := genapp.NewRelay(genrepo.New(deps.DB.Pool), mq.NewPublisher(deps.MQ), logger)
		go relay.Run(ctx)
		logger.Info("outbox 投递器已启动")
	}

	// 生成任务消费者（P05-03）。
	// ⚠️ 手动 ack、消费幂等（靠 ClaimForRun 的 CAS）、prefetch=1。
	genRepo := genrepo.New(deps.DB.Pool)
	// ⚠️ 目录与 HTTP Provider 各只建一份：Agent 的推理调用与报价复用它们（两份目录 = 两份配置缓存）。
	providers := buildProvider(ctx, deps, cfg, gen.Next, logger, ffSem)
	executor := genapp.NewExecutor(genRepo, providers.routing, logger)
	executor.SetRefunder(refunderAdapter{billingRepo, cfg.CompensationValidFor})
	executor.SetIDGen(gen.Next)
	// 产物归档：写对象存储 + 登记 media_object 与活动引用。
	// ⚠️ 归档在事务外（远程调用），落库在事务内 —— 顺序是「先归档再标成功」。
	if deps.ObjStore != nil {
		mediaSvc2 := mediaapp.New(mediarepo.New(deps.DB.Pool), deps.ObjStore, gen.Next)
		executor.SetArchiver(archiverAdapter{mediaSvc2})
	}
	executor.SetNotifier(notifierAdapter{genRepo, gen.Next})
	// 成功的图片/视频/音频生成自动登记为待审作品（用户不再手动提交）。
	// 生成域自己的仓储实现端口，Worker 不依赖内容域。
	executor.SetWorkRegistrar(genRepo)
	// 资产图出好之后回写 script_asset。⚠️ 不接的话资产会永远停在 generating：
	// 图已经归档了，但列表里那一项一直转圈，用户既看不到图也不敢重试。
	// 同一个脚本域服务同时承担资产回写与组子项回写 —— 两个端口、一份实现。
	scriptBinder := scriptapp.New(scriptrepo.New(deps.DB.Pool), gen.Next)
	scriptBinder.SetLogger(logger)
	executor.SetAssetBinder(scriptBinder)
	executor.SetGroupBinder(scriptBinder)
	// 智能剪辑名下的任务（语音识别，SCT-33）到终态时回写源素材的识别缓存、推 smartcut.updated。
	// ⚠️ 与组子项回写同一个理由两边都装：成功与失败在这里，取消与运维补偿在 API 进程（cmd/api 另装一份）。
	var smartcutEvents smartcutapp.PlanEvents
	if deps.Cache != nil {
		smartcutEvents = eventbus.NewPublisher(deps.Cache.Client, deps.Cache.Key)
	}
	// 识别完成后写字幕（SCT-36）也在这里发生：时间线服务要能校验素材、发 timeline.updated（与 API 同一个出口，
	// SCD-19），所以用装配好的那一份，不能是光秃秃的 timelineapp.New。
	binderTimelineInput := wiring.TimelineInput{Pool: deps.DB.Pool, NextID: gen.Next, Media: mediaSvc, Canvas: canvasSvc,
		Logger: logger, Smartcut: cfg.Smartcut}
	if smartcutEvents != nil {
		binderTimelineInput.Events = smartcutEvents
	}
	binderTimeline := wiring.NewTimelineService(binderTimelineInput)
	smartcutBinder := wiring.NewSmartcutService(wiring.SmartcutInput{
		Timeline: binderTimeline, Canvas: canvasSvc, Media: mediaSvc,
		Log:    logger,
		Speech: &wiring.SmartcutSpeechInput{Media: mediaSvc, Events: smartcutEvents, Timeline: binderTimeline},
		// 片段级生成的回填（SCT-37）：只用到回写那几个端口（受理在 API）。
		Clip: &wiring.SmartcutClipInput{GenRepo: genrepo.New(deps.DB.Pool), Media: mediaSvc, Timeline: binderTimeline},
	})
	executor.SetSmartcutBinder(wiring.SmartcutBinder{Svc: smartcutBinder})

	// ⚠️ 整图运行的推进**必须在 Worker 这侧接**：节点跑完之后要有人把
	// 它的终态写回去、把下游标 blocked、重算运行状态。不接的话
	// 整次运行永远停在 running。
	//
	// ⚠️ 但**派发不在这里**：派发要走 API 的统一受理入口（算价、校验能力、
	// 扣费、写 outbox）。这里只推进状态，下一批由 API 的扫描循环接手。
	wfSvc := workflowapp.New()
	wfSvc.SetRuns(workflowrepo.New(deps.DB.Pool))
	wfSvc.SetNextID(gen.Next)
	wfSvc.SetLogger(logger)
	executor.SetRunAdvancer(wfSvc)
	// 状态事件走 Redis 广播：WebSocket 连接在 API 进程，
	// 而连接分布在多个实例上，Worker 不知道某个用户连在哪一台。
	if deps.Cache != nil {
		executor.SetEvents(genapp.NewRedisEventPublisher(deps.Cache.Client, deps.Cache.Key))
	}
	if cfg.GenerationMaxRunningPerPayer > 0 {
		executor.MaxRunningPerPayer = cfg.GenerationMaxRunningPerPayer
	}

	if deps.MQ != nil {
		consumer := mq.NewConsumer(deps.MQ, "generation.jobs", gendomain.OutboxTopic)
		consumer.SetLogger(logger)
		go func() {
			err := consumer.Consume(ctx, cfg.Queue.Prefetch, func(c context.Context, body []byte) mq.AckAction {
				var msg gendomain.OutboxPayload
				if err := json.Unmarshal(body, &msg); err != nil {
					// ⚠️ 解不开的消息重投多少次都解不开，但**不能就这么丢**：
					// 连是哪个任务都不知道，丢了就是静默的消息丢失。进死信队列留档。
					logger.Error("消息格式非法，已转入死信队列", "error", err)
					return mq.AckDead
				}
				id, _ := strconv.ParseInt(msg.GenerationID, 10, 64)
				outcome, err := executor.Execute(c, id)
				if err != nil {
					logger.Error("执行失败", "generationId", id, "outcome", outcome, "error", err)
				}
				switch outcome {
				case genapp.OutcomeDuplicate:
					// at-least-once 投递的正常结局，不是故障 —— 不进死信。
					return mq.AckDiscard
				case genapp.OutcomeRequeue, genapp.OutcomeRetry:
					return mq.AckRequeue
				case genapp.OutcomeDead:
					// 提交结果不确定：可能已在上游计费，必须有人去对账。
					logger.Warn("任务转入死信队列待人工核对", "generationId", id)
					return mq.AckDead
				}
				return mq.AckDone
			})
			if err != nil {
				// ⚠️ 现在只有 ctx 取消才会返回；返回 err 说明重连循环本身坏了。
				logger.Error("生成消费者停止", "error", err)
			}
		}()
		logger.Info("生成任务消费者已启动", "prefetch", cfg.Queue.Prefetch)
	}

	// 批量导出消费者（P09-03）。
	//
	// ⚠️ **独立队列**不与生成任务混在一条上：打包是纯 IO、几秒到几分钟，
	// 而生成任务动辄几分钟且 prefetch=1 —— 混在一起的话一个排队的视频
	// 会把后面所有人的"全部下载"堵死，反过来一批大导出也会挤占生成的位置。
	if deps.MQ != nil {
		exportConsumer := mq.NewConsumer(deps.MQ, "export.jobs", mediaapp.ExportTopic)
		exportConsumer.SetLogger(logger)
		go func() {
			err := exportConsumer.Consume(ctx, cfg.Queue.Prefetch,
				func(c context.Context, body []byte) mq.AckAction {
					var msg mediaapp.ExportPayload
					if err := json.Unmarshal(body, &msg); err != nil {
						logger.Error("导出消息格式非法，已转入死信队列", "error", err)
						return mq.AckDead
					}
					id, _ := strconv.ParseInt(msg.ExportID, 10, 64)
					outcome, err := mediaSvc.RunExport(c, id, logger)
					if err != nil {
						logger.Error("导出执行失败", "exportId", id, "error", err)
					}
					switch outcome {
					case mediaapp.ExportDuplicate:
						// at-least-once 投递的正常结局，不进死信。
						return mq.AckDiscard
					case mediaapp.ExportRetry:
						return mq.AckRequeue
					}
					return mq.AckDone
				})
			if err != nil {
				logger.Error("导出消费者停止", "error", err)
			}
		}()
		logger.Info("导出任务消费者已启动")
	}

	// 渲染消费者（P10-03）。
	//
	// ⚠️ **独立队列**：渲染是 CPU 密集且长耗时的，与生成任务（等上游 IO）
	// 完全不是一类。混在一条队列上的话，一次渲染会把 prefetch 占住，
	// 后面所有生成任务跟着不动；而渲染吃满 CPU 还会把查询接口一起拖慢。
	if deps.MQ != nil && deps.ObjStore != nil {
		renderer := timelineapp.NewRenderer(timelinerepo.New(deps.DB.Pool, gen.Next), logger)
		renderMedia := mediaapp.New(mediarepo.New(deps.DB.Pool), deps.ObjStore, gen.Next)
		renderer.SetFetcher(renderFetcher{renderMedia})
		renderer.SetArchiver(renderArchiver{renderMedia})
		renderer.SetRefunder(renderRefunder{billingRepo, cfg.CompensationValidFor})
		renderer.SetNextID(gen.Next)
		renderer.SetFFmpegSem(ffSem)
		// 导出进度事件（SCT-15，SCB-6）：投到 project:{id}，节流 1 次 / 秒。没有 Redis 时只是没有实时信号。
		if deps.Cache != nil {
			renderer.SetEvents(eventbus.NewPublisher(deps.Cache.Client, deps.Cache.Key))
		}
		// 文字与字幕在服务端排版（SCT-14）：字体懒加载，缺了只有带文字的导出失败（并说清原因）。
		renderer.SetTypesetter(typeset.New(typeset.Options{}))
		if n := envInt("SEEAITV_RENDER_MAX_CONCURRENT", 1); n > 0 {
			renderer.MaxConcurrent = n
		}
		if !timelineapp.FFmpegAvailable() {
			// ⚠️ 没有 ffmpeg 时**启动就说**：不说的话所有渲染都会失败，
			// 而"为什么所有渲染都失败"是个很难猜的问题。
			logger.Warn("本机没有 ffmpeg，渲染任务一律会失败")
		}
		// 渲染完成/失败也要通知发起人（P13-02 的挂账）。
		// ⚠️ 复用与生成域同一个适配器：两边写的是**同一张表**、靠**同一个**
		// 唯一索引（`uk_notification_ref`）幂等。各写一份的话两边对"什么算
		// 重复"的理解早晚会分叉，而分叉的表现是用户收到两条同样的通知。
		renderer.SetNotifier(notifierAdapter{genRepo, gen.Next})
		renderConsumer := mq.NewConsumer(deps.MQ, "render.jobs", timelineapp.RenderTopic)
		renderConsumer.SetLogger(logger)
		go func() {
			err := renderConsumer.Consume(ctx, 1, func(c context.Context, body []byte) mq.AckAction {
				var msg timelineapp.RenderPayload
				if err := json.Unmarshal(body, &msg); err != nil {
					logger.Error("渲染消息格式非法，已转入死信队列", "error", err)
					return mq.AckDead
				}
				id, _ := strconv.ParseInt(msg.RenderID, 10, 64)
				outcome, err := renderer.Run(c, id)
				if err != nil {
					logger.Error("渲染执行失败", "renderId", id, "error", err)
				}
				switch outcome {
				case timelineapp.RenderDuplicate:
					return mq.AckDiscard
				case timelineapp.RenderRequeue:
					return mq.AckRequeue
				}
				return mq.AckDone
			})
			if err != nil {
				logger.Error("渲染消费者停止", "error", err)
			}
		}()
		// 超时渲染的恢复：ffmpeg 可能卡死（畸形输入、磁盘满），
		// 而卡着的任务一直占着并发额度 —— 后面排队的全都不动。
		go func() {
			t := time.NewTicker(time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					renderer.RecoverStale(ctx)
				}
			}
		}()
		logger.Info("渲染消费者已启动", "maxConcurrent", renderer.MaxConcurrent,
			"ffmpeg", timelineapp.FFmpegAvailable())
	}

	// 画布 Agent 轮次消费者（AGT-22）。
	//
	// ⚠️ **独立队列**：一轮可能持续数分钟（等模型流式输出、跑十几次工具），
	// 混进生成队列会把 prefetch=1 的生成任务全堵住。
	// ⚠️ 并发靠**多个消费者**而不是加大 prefetch：消费者的 handler 是同步的，
	// 一个消费者 prefetch=4 只是把 4 条消息攥在手里、仍然一条一条跑 ——
	// 第 2~4 条会被一轮几分钟的对话挡住，而它们本该交给空闲的 Worker。
	// ⚠️ 推理模型调用与报价复用上面那一份目录 / HTTP Provider。
	agentQueues := 0
	if deps.MQ != nil && cfg.Agent.Enabled {
		var bus wiring.BusPublisher
		if deps.Cache != nil {
			pub := eventbus.NewPublisher(deps.Cache.Client, deps.Cache.Key)
			bus = pub
			// Agent 在 Worker 里写画布时也要推 canvas.ops_applied（前端按操作日志重放）。
			canvasSvc.SetEvents(pub)
		}
		// 工具注册表：具体工具由 AGT-23 / AGT-24 注册；空表时模型拿不到任何工具，只会对话。
		toolReg, err := agenttools.NewRegistry()
		if err != nil {
			return err
		}
		// 付费与画布写工具（AGT-24）：受理要用与 API 同一份装配出来的生成服务（T-3）——
		// 预设、报价、输入解析、退款、组子项 / 整图回写一条都不能少，所以走 wiring.BuildCore
		// 而不是在这里再拼一个 genapp.Service。目录复用上面那一份（两份 = 两份配置缓存）。
		agentCore, err := wiring.BuildCore(wiring.CoreInput{
			Cfg: cfg, Deps: deps, NextID: gen.Next, Logger: logger,
			Media: mediaSvc, Catalog: providers.catalog,
		})
		if err != nil {
			return err
		}
		if bus != nil {
			// Agent 经这一份画布服务写节点，同样要推 canvas.ops_applied。
			agentCore.Canvas.SetEvents(bus)
		}
		// 营销服务（规划选项钉版本、generate 取创意推荐画幅）：不看营销总开关 —— 开关只管入口，
		// 已建的营销会话仍要能规划和生成。付费工具与规划工具共用这一份。
		agentMarketing := wiring.NewMarketingServiceFor(cfg, deps.DB.Pool, providers.catalog)
		for _, t := range wiring.NewAgentPaidTools(wiring.AgentPaidToolsInput{
			AgentRepo: agentrepo.New(deps.DB.Pool), Gen: agentCore.Gen, Catalog: providers.catalog,
			Canvas: agentCore.Canvas, Billing: billingRepo, Bus: bus, NextID: gen.Next, Logger: logger,
			Script: agentCore.Script, // 营销会话 generate 把角色资产换成图（MKT-T-22）
			// 营销视频节点模型没写画幅时按所选创意的推荐补（会话 107398945801502720）。
			Marketing: agentMarketing,
		}) {
			if err := toolReg.Register(t); err != nil {
				return err
			}
		}
		// 规划阶段免费工具（AGT-23）：读画布、素材、报价、卡片、文档、规划确认、技能。
		for _, t := range wiring.NewAgentPlanningTools(wiring.AgentPlanningToolsInput{
			AgentRepo: agentrepo.New(deps.DB.Pool), Canvas: agentCore.Canvas, GenRepo: agentCore.GenRepo,
			Media: mediaSvc, Script: agentCore.Script, Catalog: providers.catalog,
			Billing: billingapp.New(billingRepo), Bus: bus, NextID: gen.Next, Logger: logger,
			// 营销会话规划（MKT-T-20）：不看营销总开关 —— 开关只管入口，已建的营销会话仍要能规划。
			Marketing: agentMarketing,
			// 批量变体上限（MKT-T-27）：与 /marketing/config 下发的是同一个配置值。
			MaxVariants: cfg.Marketing.MaxVariants,
		}) {
			if err := toolReg.Register(t); err != nil {
				return err
			}
		}
		// 多段工具（AGT-50）：脚本线 / 整图 / 时间线。与 HTTP 侧同一组服务（脚本、整图来自 BuildCore，
		// 时间线来自 wiring.NewTimelineService），付费的走 PaidCall 与额度。
		agentTimeline := wiring.NewTimelineService(wiring.TimelineInput{
			Pool: deps.DB.Pool, NextID: gen.Next, Media: mediaSvc, Script: agentCore.Script,
			Canvas: agentCore.Canvas, Catalog: providers.catalog,
			CompensationValidFor: cfg.CompensationValidFor, Logger: logger,
			// Agent 在 Worker 里改时间线也要推 timeline.updated（SCD-19，与 API 同一个出口）。
			Events: bus, Smartcut: cfg.Smartcut,
		})
		// 智能剪辑（SCT-41）：Agent 的 smartcut_create / get / plan 与 timeline_assemble 建节点都在 Worker 里跑 ——
		// AI 剪辑同步调文本模型（与网页同一套端口、同一份按用户限流），阶段推送走同一条实时总线。
		agentSmartcut := wiring.NewSmartcutService(wiring.SmartcutInput{
			Timeline: agentTimeline, Canvas: agentCore.Canvas, Media: mediaSvc, Generations: agentCore.GenRepo,
			Log: logger,
			Plan: &wiring.SmartcutPlanInput{Pool: deps.DB.Pool, Timeline: agentTimeline, Catalog: providers.catalog,
				Media: mediaSvc, Generations: agentCore.GenRepo, Text: agentCore.TextProvider, Events: smartcutEvents,
				Cfg: cfg.Smartcut, NextID: gen.Next, Logger: logger,
				Board: wiring.NewSmartcutBoard(agentCore.Script, mediaSvc)},
		})
		for _, t := range wiring.NewAgentMultiTools(wiring.AgentMultiToolsInput{
			Paid: wiring.AgentPaidToolsInput{
				AgentRepo: agentrepo.New(deps.DB.Pool), Gen: agentCore.Gen, Catalog: providers.catalog,
				Canvas: agentCore.Canvas, Billing: billingRepo, Bus: bus, NextID: gen.Next, Logger: logger,
				Script: agentCore.Script, Marketing: agentMarketing,
			},
			Script: agentCore.Script, ScriptRepo: scriptrepo.New(deps.DB.Pool),
			Workflow: agentCore.Workflow, Timeline: agentTimeline,
			// 不走脚本时按视频节点拼时间线（timeline_assemble 的 nodeIds）。
			GenRepo: agentCore.GenRepo,
			// 3D 导演台（DRT-29）：Agent 回合在 Worker 里跑，搭景服务用 BuildCore 装的这一份（API 那份不在这个进程里）。
			Director: agentCore.Director,
			// 智能剪辑（SCT-41）：上面装的这一份（带 AI 剪辑端口）。
			Smartcut: agentSmartcut,
			// 这一版只保留手动剪辑：AI 剪辑工具按开关装（缺省不装）。
			SmartcutAIEnabled: cfg.Smartcut.AIEnabled,
		}) {
			if err := toolReg.Register(t); err != nil {
				return err
			}
		}
		// 营销会话的场景段 / 营销输入与只读工具（MKT-T-18）。不看营销总开关：已建好的营销会话
		// 在开关关闭后仍要能继续跑（同 BuildCore 里的营销段来源）。
		agentScene := wiring.NewAgentSceneContext(wiring.AgentSceneInput{
			Marketing: wiring.NewMarketingService(wiring.MarketingServiceInput{
				Repo: marketingrepo.New(deps.DB.Pool), Catalog: providers.catalog,
				Settings: marketingapp.Settings{MaxProductImages: cfg.Marketing.MaxProductImages,
					MaxVariants: cfg.Marketing.MaxVariants, AgentEnabled: cfg.Agent.Enabled},
			}),
			Prompts: providers.catalog, Script: agentCore.Script, Variants: cfg.Marketing.MaxVariants, Logger: logger,
		})
		for _, t := range wiring.NewAgentMarketingTools(agentScene) {
			if err := toolReg.Register(t); err != nil {
				return err
			}
		}
		// 推理模型看商品图与 view_media（MKT-T-19）：一律服务端缩略后 base64 内联，不发外链（MKT-AC-05）。
		agentImages := wiring.AgentSceneImages{Catalog: providers.catalog, Media: mediaSvc,
			Width: cfg.Marketing.ReasoningImageWidth}
		if err := toolReg.Register(wiring.NewAgentViewMediaTool(agentrepo.New(deps.DB.Pool), agentImages)); err != nil {
			return err
		}
		runner := wiring.NewAgentRunner(wiring.AgentRunnerInput{
			AgentRepo: agentrepo.New(deps.DB.Pool), Chat: providers.http, Catalog: providers.catalog,
			Billing: billingRepo, BillingSvc: billingapp.New(billingRepo), Collab: collabrepo.New(deps.DB.Pool),
			Canvas: canvasSvc, Bus: bus, Tools: toolReg, Compensate: cfg.CompensationValidFor,
			NextID: gen.Next, Logger: logger,
			MaxCalls: cfg.Agent.TurnMaxCalls, ContextChars: cfg.Agent.ContextChars, Scene: agentScene,
			Images: agentImages, MaxOutputTokens: cfg.Agent.MaxOutputTokens,
		})
		for i := 0; i < cfg.Agent.RunnerConcurrency; i++ {
			c := mq.NewConsumer(deps.MQ, agentapp.TurnQueue, agentapp.TurnTopic)
			c.SetLogger(logger)
			// 每个消费者一个租约持有者标识（04 文档 P-10）：共用一个的话，同进程两个消费者分不出彼此的租约。
			rn := runner.Fork()
			go func() {
				err := c.Consume(ctx, 1, func(c context.Context, body []byte) mq.AckAction {
					outcome, err := rn.HandleMessage(c, body)
					if errors.Is(err, agentapp.ErrBadMessage) {
						logger.Error("agent 轮次消息格式非法，已转入死信队列", "error", err)
						return mq.AckDead
					}
					if err != nil {
						logger.Warn("agent 轮次结束时带错误", "outcome", outcome.String(), "error", err)
					}
					switch outcome {
					case agentapp.OutcomeDuplicate, agentapp.OutcomeBusy, agentapp.OutcomeLeaseLost:
						// at-least-once 的正常结局：已终态 / 别的 Worker 在跑，不进死信。
						return mq.AckDiscard
					case agentapp.OutcomeRequeue:
						return mq.AckRequeue
					}
					return mq.AckDone
				})
				if err != nil {
					logger.Error("agent 轮次消费者停止", "error", err)
				}
			}()
		}
		agentQueues = 1
		logger.Info("agent 轮次消费者已启动", "consumers", cfg.Agent.RunnerConcurrency,
			"runnerId", runner.RunnerID(), "maxCalls", cfg.Agent.TurnMaxCalls)

		// 扫描循环（AGT-25）：waiting_task 唤醒、租约过期重入队、排队轮、超时轮退款、卡片过期、额度归还。
		// ⚠️ 它同时就是重启恢复 —— 会话的「继续跑」不依赖任何进程内状态。
		scanner := wiring.NewAgentScanner(wiring.AgentScannerInput{
			AgentRepo: agentrepo.New(deps.DB.Pool), GenRepo: genRepo, Billing: billingRepo, Bus: bus,
			Compensate: cfg.CompensationValidFor, NextID: gen.Next, Logger: logger,
			MaxCalls: cfg.Agent.TurnMaxCalls, CardExpire: cfg.Agent.CardExpire,
			// 多段工具挂的组 / 整图运行 / 渲染 / 同步文本调用（AGT-50）：按引用唤醒与结算。
			ScriptRepo: scriptrepo.New(deps.DB.Pool), WorkflowRepo: workflowrepo.New(deps.DB.Pool),
			TimelineRepo: timelinerepo.New(deps.DB.Pool, gen.Next),
		})
		go runAgentScanner(ctx, scanner, logger, redisLocker{deps.Cache})
		logger.Info("agent 扫描循环已启动", "interval", agentapp.ScanInterval,
			"staleTurnAfter", scanner.Config().StaleTurnAfter, "cardExpire", scanner.Config().CardExpire)
	} else if !cfg.Agent.Enabled {
		logger.Info("画布 Agent 已关闭（SEEAITV_AGENT_ENABLED=false），不消费 agent.turns")
	}

	// 死信排空（P13-03；GAP-07 的另一半）。
	//
	// ⚠️ 死信此前**只存在于 RabbitMQ 的 `.dead` 队列里**，没有任何人看得到 ——
	// 而 `AckDead` 的两种情形（消息解不开、提交结果不确定）恰恰都要求人去对账。
	// 排空器把它们搬进 `dead_letter` 表，运维在管理端重投或丢弃。
	//
	// ⚠️ 它**只搬运不重投**：自动重投的话，一条必然失败的消息会在
	// "失败 → 死信 → 重投"之间无限循环，而每一圈都可能在上游产生一次真实调用。
	opsSvc := opsapp.New(opsrepo.New(deps.DB.Pool, gen.Next))
	opsSvc.SetLogger(logger)
	deadQueues := []struct{ queue, topic string }{
		{"generation.jobs", gendomain.OutboxTopic},
		{"export.jobs", mediaapp.ExportTopic},
		{"render.jobs", timelineapp.RenderTopic},
	}
	// agent.turns 的死信只在 Agent 开启时排空：关闭时这条队列根本没人声明。
	if agentQueues > 0 {
		deadQueues = append(deadQueues, struct{ queue, topic string }{agentapp.TurnQueue, agentapp.TurnTopic})
	}
	for _, q := range deadQueues {
		q := q
		go func() {
			c := mq.NewConsumer(deps.MQ, q.queue, q.topic)
			c.SetLogger(logger)
			err := c.DrainDead(ctx, func(c context.Context, m mq.DeadMessage) error {
				return opsSvc.RecordDead(c, m.Queue, m.RoutingKey, m.Reason, m.Body)
			})
			if err != nil {
				logger.Error("死信排空器停止", "queue", q.queue, "error", err)
			}
		}()
	}
	logger.Info("死信排空器已启动", "queues", len(deadQueues))

	// 请求日志清理（P13-04）。
	//
	// ⚠️ `api_request_log` 与 `provider_request_log` 是**唯二会无限增长的表**。
	// 到 P13-04 之前**两张都没有任何清理接线** —— Provider 那张的
	// `CleanupBefore` 早就写好了却从没被调用过，而这类"写了没接"只会在
	// 磁盘满的那天被发现（那时所有写库的操作一起失败，看起来完全不像日志的问题）。
	//
	// ⚠️ 间隔**每小时**而不是每天、单次**分批**：一次删几十万行会长时间持锁，
	// 把正在写日志的线程堵住 —— 而那时表现为**所有接口都变慢**。
	obsSvc := obsapp.New(obsrepo.New(deps.DB.Pool, gen.Next))
	obsSvc.SetLogger(logger)
	obsSvc.SetProviderLogs(providerrepo.New(deps.DB.Pool, gen.Next))
	// MCP 调用审计与报价的表不受 SEEAITV_MCP_ENABLED 影响：开关关掉之后，之前留下的行照样要按期清掉。
	mcpCalls := mcprepo.New(deps.DB.Pool)
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				apiN, provN := obsSvc.Cleanup(ctx, cfg.LogRetention, 50000, time.Now())
				if apiN > 0 || provN > 0 {
					logger.Info("清理过期请求日志", "api", apiN, "provider", provN,
						"retentionDays", int(cfg.LogRetention.Hours()/24))
				}
				// 画布操作日志（AGT-10）同样只增不减：保留 30 天（设计建议值），同样分批。
				if n, err := canvasSvc.CleanupOpLog(ctx, canvasapp.DefaultOpLogRetention, 50000,
					time.Now()); err != nil {
					logger.Warn("清理画布操作日志失败", "error", err)
				} else if n > 0 {
					logger.Info("清理过期画布操作日志", "rows", n)
				}
				// MCP 调用审计（MCPT-06，MCP-F-71）同样只增不减：保留 30 天（M-7 已定），单批上限同上。
				if n, err := mcpCalls.CleanupCallLogBefore(ctx, time.Now().Add(-mcpdomain.CallLogRetention),
					mcpdomain.CallLogCleanupBatch); err != nil {
					logger.Warn("清理 MCP 调用审计失败", "error", err)
				} else if n > 0 {
					logger.Info("清理过期 MCP 调用审计", "rows", n,
						"retentionDays", int(mcpdomain.CallLogRetention.Hours()/24))
				}
				// MCP 报价（MCPT-05/10）：过期后再留 QuoteRetention（30 天，设计建议值、待定）再删，同样分批。
				if n, err := mcpCalls.CleanupQuotesBefore(ctx, time.Now().Add(-mcpdomain.QuoteRetention),
					mcpdomain.QuoteCleanupBatch); err != nil {
					logger.Warn("清理过期 MCP 报价失败", "error", err)
				} else if n > 0 {
					logger.Info("清理过期 MCP 报价", "rows", n,
						"retentionDays", int(mcpdomain.QuoteRetention.Hours()/24))
				}
			}
		}
	}()

	// 恢复扫描：超时、提交不确定窗口、长时间未被领取的任务。
	go runRecovery(ctx, executor, logger, redisLocker{deps.Cache})

	logger.Info("worker 已就绪",
		"trashRetentionDays", int(cfg.TrashRetention.Hours()/24),
		"maxRunningPerPayer", executor.MaxRunningPerPayer)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("收到退出信号，开始优雅关闭")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return health.Shutdown(shutdownCtx)
	}
}

// buildProvider 组装执行器要的上游适配器。
//
// ⚠️ 真实协议与开发假适配器**按协议码共存**（见 provider/app.Routing）：
// 不是一个全局开关一刀切 —— 那样打开假适配器就等于所有模型都变假，
// 真实联调根本没法做。假适配器只在 dev 下挂上，production 由 config 拒绝启动。
// workerProviders 是 buildProvider 的产物：执行器用 routing，Agent 用 http 与 catalog。
type workerProviders struct {
	routing genapp.Provider
	http    *providerapp.HTTPProvider
	catalog *catalogapp.Service
}

func buildProvider(ctx context.Context, deps *bootstrap.Deps, cfg config.Config, nextID func() int64,
	logger *slog.Logger, ffSem *ffsem.Sem) workerProviders {
	policy := transport.URLPolicy{AllowPrivate: cfg.Dev.AllowPrivateNetwork}
	client := transport.NewClient(policy)

	catalogSvc := catalogapp.New(catalogrepo.New(deps.DB.Pool), nextID)
	// ⚠️ 密钥材料与 API 进程必须一致，否则这里解不开管理端录入的密文。
	// 开发下回落到 JWT 密钥（与 API 同一套回落规则）；production 由 config 强制独立配置。
	if cfg.Auth.SecretKey == "" {
		logger.Warn("未配置 SEEAITV_SECRET_KEY，回落到 JWT 密钥（仅开发）")
	}
	// ⚠️ 历史版本一起交进去（密钥轮换）—— 见 wiring.NewCipher。
	// 回落值保持原样传 cfg.Auth.JWTSecret（AGT-01 只收口构造，不改行为）。
	cipher, err := wiring.NewCipher(cfg, cfg.Auth.JWTSecret)
	if err == nil {
		catalogSvc.SetSecretCipher(cipher)
	} else {
		logger.Error("建立密钥解密器失败，需要密钥的 Provider 将无法调用", "error", err)
	}

	// ⚠️ 目录域直接实现 `runcfg.Resolver`、日志仓储直接实现 `runcfg.RequestLogger`，
	// 两个 main 里因此都不用写转换适配器 —— 写两份早晚会不一致，
	// 而不一致时没有编译错误，只会有一个进程行为不对。
	http := providerapp.NewHTTPProvider(client, catalogSvc, policy, logger)
	http.SetRequestLogger(providerrepo.New(deps.DB.Pool, nextID))
	// 输入区间截取（docs/agent/smartcut SCD-34）与渲染、预览素材扫描器共用 ffmpeg 名额。
	http.SetFFmpegSem(ffSem)
	if deps.ObjStore != nil {
		media := mediaapp.New(mediarepo.New(deps.DB.Pool), deps.ObjStore, nextID)
		http.SetMediaResolver(mediaRefAdapter{media, deps.ObjStore})
		var relayScratch providerapp.ScratchStore
		if rc := cfg.RelayStorage; rc.Endpoint != "" {
			// 公网中转（marketing dev/01 R-15）：只建客户端、不探测 —— 中转是可选的，连不上不该让 Worker 起不来；
			// 真用到时连不上，会在提交前以永久失败说清原因（不发上游、退款）。
			if store, err := objstore.New(context.Background(), rc); err != nil {
				logger.Error("建立公网中转存储客户端失败，只收地址的上游将拿不到参考媒体", "error", err)
			} else {
				http.SetMediaRelay(relay.New(media, store, cfg.RelayPrefix, logger))
				// 截取片段放在中转前缀下：残留的那份由中转存储的过期规则清理，与转存的参考媒体同一条规则。
				relayScratch = prefixedScratch{store, cfg.RelayPrefix}
				logger.Info("公网中转存储已启用", "endpoint", rc.Endpoint, "bucket", rc.Bucket, "prefix", cfg.RelayPrefix)
			}
		}
		http.SetScratch(deps.ObjStore, relayScratch)
		// 主存储里没走到终态的截取片段按时间兜底删（SCD-34）。
		go providerapp.NewScratchSweeper(deps.ObjStore, logger).Run(ctx)
	}

	var dev genapp.Provider
	if cfg.Dev.FakeProvider {
		dev = genapp.DevProvider{}
		logger.Warn("开发假适配器已启用，dev_fake 协议的模型不会调用真实上游")
	}
	return workerProviders{routing: providerapp.NewRouting(http, dev), http: http, catalog: catalogSvc}
}

// mediaRefAdapter 把媒体域接成参考媒体解析端口。
type mediaRefAdapter struct {
	svc   *mediaapp.Service
	store *objstore.Client
}

func (a mediaRefAdapter) SignedURL(ctx context.Context, mediaID int64) (string, string, error) {
	m, err := a.svc.Get(ctx, mediaID)
	if err != nil {
		return "", "", err
	}
	url, err := a.store.PresignGet(ctx, m.ObjectKey)
	return url, m.ContentType, err
}

// DurationMs 实现 providerapp.MediaDurationResolver：UniArt 要自报参考视频总时长。
func (a mediaRefAdapter) DurationMs(ctx context.Context, mediaID int64) (int, error) {
	m, err := a.svc.Get(ctx, mediaID)
	if err != nil {
		return 0, err
	}
	return m.DurationMs, nil
}

// Open 实现 providerapp.MediaOpener：截取输入区间要整段素材落临时文件，不能读进内存（SCD-34）。
func (a mediaRefAdapter) Open(ctx context.Context, mediaID int64) (io.ReadCloser, string, error) {
	body, m, err := a.svc.Open(ctx, mediaID)
	if err != nil {
		return nil, "", err
	}
	return body, m.ContentType, nil
}

// prefixedScratch 把截取片段的键放到公网中转的前缀下。
type prefixedScratch struct {
	store  *objstore.Client
	prefix string
}

func (s prefixedScratch) PutFile(ctx context.Context, key string, body io.ReadSeeker, size int64, mime string) error {
	return s.store.PutFile(ctx, s.prefix+key, body, size, mime)
}
func (s prefixedScratch) PresignGet(ctx context.Context, key string) (string, error) {
	return s.store.PresignGet(ctx, s.prefix+key)
}
func (s prefixedScratch) Delete(ctx context.Context, key string) error {
	return s.store.Delete(ctx, s.prefix+key)
}

func (a mediaRefAdapter) Inline(ctx context.Context, mediaID int64) ([]byte, string, error) {
	body, m, err := a.svc.Open(ctx, mediaID)
	if err != nil {
		return nil, "", err
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	return data, m.ContentType, err
}

// archiverAdapter 把媒体域接成执行器的归档端口。
type archiverAdapter struct{ svc *mediaapp.Service }

func (a archiverAdapter) Archive(ctx context.Context, in genapp.ArchiveInput) (int64, error) {
	up, err := a.svc.Upload(ctx, mediaapp.UploadInput{
		WorkspaceID: in.WorkspaceID, UserID: in.UserID, Purpose: "generation_output",
		DeclaredMIME: in.MIME, Size: in.Size, Body: in.Body,
	})
	if err != nil {
		return 0, err
	}
	return up.ID, nil
}

func (a archiverAdapter) RegisterUsage(ctx context.Context, mediaID int64, usageType string, refID int64) error {
	return a.svc.RegisterUsage(ctx, mediaID, usageType, refID)
}

// notifierAdapter 发站内通知。幂等由唯一索引兜底。
type notifierAdapter struct {
	repo   *genrepo.Repo
	nextID func() int64
}

func (a notifierAdapter) Notify(ctx context.Context, userID int64, typ, title, content, refType, refID string) error {
	return a.repo.InsertNotification(ctx, a.nextID(), userID, typ, title, content, refType, refID, "")
}

// refunderAdapter 把账本接成执行器的退款端口。
type refunderAdapter struct {
	repo       *billingrepo.Repo
	compensate time.Duration
}

func (a refunderAdapter) Refund(ctx context.Context, payerID int64, refType, refID, reason string) error {
	_, err := a.repo.Refund(ctx, billingrepo.RefundInput{
		PayerID: payerID, RefType: refType, RefID: refID, Reason: reason,
		CompensationValidFor: a.compensate,
	})
	// 幂等命中不是错误：退款的一次性由账本的唯一索引保证，
	// 这里再报一次只会让执行器误以为退款失败。
	if errors.Is(err, billingrepo.ErrDuplicate) {
		return nil
	}
	return err
}

// runRecovery 定期恢复扫描。
//
// ⚠️ 与消费者分开：消费者只处理"有消息的"任务，而恢复扫描要处理的恰恰是
// **没有消息的那些** —— 投递丢了、Worker 崩在半路、上游一直不回。
func runRecovery(ctx context.Context, e *genapp.Executor, log *slog.Logger, lock canvasapp.Locker) {
	const interval = time.Minute
	const orphanAfter = 10 * time.Minute
	tick := func() {
		if ok, err := lock.TryLock(ctx, "scan:generation-recovery", interval); err == nil && !ok {
			return
		}
		if n := e.RecoverStale(ctx, orphanAfter, 200); n > 0 {
			log.Info("恢复扫描处理完成", "handled", n)
		}
		// 失败了却没退成款的补上（CD-82）：失败路径上的退款失败只留日志
		if n := e.RecoverRefunds(ctx, time.Minute, 100); n > 0 {
			log.Info("补退款完成", "refunded", n)
		}
	}
	tick()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

// runExpiryScan 每小时扫一次到期积分、到期会员与超时订单。
//
// 与回收站清理同样的理由放在 Worker：后台作业的锁竞争不该打到用户请求上。
// 跨实例用同一把 Redis 锁避免重复劳动；正确性由 SQL 条件保证
// （`status='active' and expire_at <= now()`，重复执行也不会重复扣）。
// runAgentScanner 每 2s 扫一遍画布 Agent 的会话（AGT-25）。
//
// ⚠️ Redis 锁只为省重复劳动：拿不到锁（或 Redis 挂了）都照样能保证正确性 ——
// 扫描的每一步都在锁住会话行的事务里复核后 CAS 推进。Redis 出错时照跑，不因此停扫。
func runAgentScanner(ctx context.Context, s *agentapp.Scanner, log *slog.Logger, lock canvasapp.Locker) {
	tick := func() {
		if ok, err := lock.TryLock(ctx, "scan:agent", agentapp.ScanInterval); err == nil && !ok {
			return
		}
		st, err := s.Tick(ctx, 0)
		if err != nil && ctx.Err() == nil {
			log.Warn("agent 扫描部分失败（下次扫描重试）", "error", err)
		}
		if st.Total() > 0 {
			log.Info("agent 扫描处理完成", "woken", st.Woken, "requeued", st.Requeued,
				"queuedResumed", st.QueuedResumed, "queuedDropped", st.QueuedDropped, "orphaned", st.Orphaned,
				"staleFailed", st.StaleFailed, "cardsExpired", st.CardsExpired,
				"stepsFinished", st.StepsFinished, "budgetRefunded", st.BudgetRefunded,
				"budgetSettled", st.BudgetSettled)
		}
	}
	tick()
	t := time.NewTicker(agentapp.ScanInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

func runExpiryScan(ctx context.Context, r *billingrepo.Repo, log *slog.Logger, lock canvasapp.Locker) {
	const interval = time.Hour
	tick := func() {
		if ok, err := lock.TryLock(ctx, "scan:credit-expiry", interval); err == nil && !ok {
			return
		}
		n, err := r.ExpireGrants(ctx, 500)
		if err != nil {
			log.Error("积分过期扫描失败", "error", err)
			return
		}
		if n > 0 {
			log.Info("积分过期扫描完成", "expired", n)
		}
		// 会员到期与订单超时（P11-01）。搭同一趟车而不是另起一个循环：
		// 三者都是"按时间刷状态"的清理，间隔要求也一样。
		//
		// ⚠️ 两者的重要性**完全不同**：会员到期只影响列表上的状态字面值
		// （权益判定按 `expire_at > now()` 算，作业停了没人会多拿权益）；
		// 而**订单超时必须跑** —— 它占着幂等键，不清掉的话用户再下同一单
		// 会命中那张僵尸订单，看到的是一张早就付不了的单。
		if m, err := r.ExpireMemberships(ctx, 500); err != nil {
			log.Error("会员到期扫描失败", "error", err)
		} else if m > 0 {
			log.Info("会员到期扫描完成", "expired", m)
		}
		if o, err := r.ExpireOrders(ctx, 500); err != nil {
			log.Error("订单超时扫描失败", "error", err)
		} else if o > 0 {
			log.Info("订单超时关闭完成", "expired", o)
		}
	}
	tick()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

// redisLocker 用 SET NX EX 做跨实例互斥。
//
// ⚠️ 这把锁只避免重复劳动，**不承担正确性** —— 清理语句自带
// `status='deleted' and deleted_at < ?` 条件，多实例同时跑也不会删错行（GO-11）。
type redisLocker struct{ c *cache.Client }

func (l redisLocker) TryLock(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	if l.c == nil {
		return true, nil
	}
	return l.c.SetNX(ctx, l.c.Key("lock", key), buildinfo.Version, ttl).Result()
}

func healthRouter(deps *bootstrap.Deps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, req *http.Request) {
		res := deps.Ready(req.Context())
		status := http.StatusOK
		if res.Status != "ok" {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, res)
	})
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, buildinfo.Get())
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ---------------------------------------------------------------- 渲染适配器

// renderFetcher 把媒体域接成渲染的素材下载端口。
//
// ⚠️ **落临时文件不落内存**：一条片子的素材加起来动辄上 GB，
// 按内存缓冲并发两路就 OOM（与产物下载同一条）。
type renderFetcher struct{ svc *mediaapp.Service }

func (a renderFetcher) Fetch(ctx context.Context, mediaID int64) (timelinedomain.MediaFile, func(), error) {
	body, m, err := a.svc.Open(ctx, mediaID)
	if err != nil {
		return timelinedomain.MediaFile{}, nil, err
	}
	defer body.Close()

	f, err := os.CreateTemp("", "seeaitv-src-*"+mediaExt(m.ContentType))
	if err != nil {
		return timelinedomain.MediaFile{}, nil, err
	}
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}
	if _, err := io.Copy(f, body); err != nil {
		cleanup()
		return timelinedomain.MediaFile{}, nil, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return timelinedomain.MediaFile{}, nil, err
	}
	// ⚠️ 扩展名要对：ffmpeg 靠它和文件头一起判封装格式，
	// 名字全叫 `.tmp` 的话部分封装会被判错，而错误信息指向的是滤镜图。
	file := timelinedomain.MediaFile{Path: f.Name()}
	// 一份素材只探一次（以前每段画面各 ffprobe 一次）：有没有画面、音轨与几个声道。
	// ⚠️ 探不出来按「没有音轨」：按"有"的话滤镜图会引用一条不存在的流，整次渲染当场失败。
	if info, err := mediadomain.ProbeStreams(ctx, f.Name()); err == nil {
		file.HasVideo, file.HasAudio, file.AudioChannels = info.HasVideo, info.HasAudio, info.AudioChannels
	} else {
		file.HasVideo = strings.HasPrefix(m.ContentType, "video/") || strings.HasPrefix(m.ContentType, "image/")
	}
	return file, func() { _ = os.Remove(f.Name()) }, nil
}

func mediaExt(mime string) string {
	switch mime {
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	case "audio/mpeg":
		return ".mp3"
	case "audio/wav":
		return ".wav"
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	}
	return ".bin"
}

// renderArchiver 把成片写进对象存储。
type renderArchiver struct{ svc *mediaapp.Service }

// Discard 软删一份刚归档、但任务已被取消的成片（SCB-5）。走 Delete：引用检查照样做。
func (a renderArchiver) Discard(ctx context.Context, workspaceID, mediaID int64) error {
	return a.svc.Delete(ctx, workspaceID, mediaID)
}

func (a renderArchiver) Archive(ctx context.Context, workspaceID, userID int64,
	mime string, size int64, body io.Reader) (int64, error) {
	up, err := a.svc.Upload(ctx, mediaapp.UploadInput{
		WorkspaceID: workspaceID, UserID: userID, Purpose: "render_output",
		DeclaredMIME: mime, Size: size, Body: body,
	})
	if err != nil {
		return 0, err
	}
	return up.ID, nil
}

// renderRefunder 把账本接成渲染的退款端口。
type renderRefunder struct {
	repo     *billingrepo.Repo
	validFor time.Duration
}

func (a renderRefunder) Refund(ctx context.Context, payerID int64,
	refType, refID string, amount int, reason string) error {
	// 退款金额由账本按原消费流水算，不由调用方报（退款按原批次回补，
	// 而"原批次"只有账本知道）。
	_ = amount
	_, err := a.repo.Refund(ctx, billingrepo.RefundInput{
		PayerID: payerID, RefType: refType, RefID: refID,
		Reason: reason, CompensationValidFor: a.validFor,
	})
	return err
}

// envInt 读一个整数环境变量。
func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
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
