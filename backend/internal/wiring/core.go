// Package wiring 是两个进程共用的服务装配（T-3、AGT-01）。
//
// 它装出生成、目录、画布、账本、脚本、整图这几个域的服务与跨域适配器。
// 此前这些都写在 cmd/api/main.go 里；Worker 要调受理（generation.Accept）
// 等「API 侧服务」时只能再抄一份，而两份早晚不一致且没有编译错误。
//
// ⚠️ 本包**不得** import 任何领域 HTTP 层（`internal/<域>/transport`）与
// `internal/transport/ws`：Worker 不能因为装配被拖进 HTTP 层（见 imports_test.go）。
// HTTP handler、WebSocket hub 与路由仍由各自的 main 组装。
package wiring

import (
	"context"
	"log/slog"

	timelinerepo "github.com/li904338203/tabtv/backend/internal/timeline/repo"

	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	canvasrepo "github.com/li904338203/tabtv/backend/internal/canvas/repo"
	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	directorapp "github.com/li904338203/tabtv/backend/internal/director/app"
	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	genrepo "github.com/li904338203/tabtv/backend/internal/generation/repo"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/platform/bootstrap"
	"github.com/li904338203/tabtv/backend/internal/platform/config"
	"github.com/li904338203/tabtv/backend/internal/platform/objstore"
	providerapp "github.com/li904338203/tabtv/backend/internal/provider/app"
	providerrepo "github.com/li904338203/tabtv/backend/internal/provider/repo"
	providertransport "github.com/li904338203/tabtv/backend/internal/provider/transport"
	scriptapp "github.com/li904338203/tabtv/backend/internal/script/app"
	scriptrepo "github.com/li904338203/tabtv/backend/internal/script/repo"
	workflowapp "github.com/li904338203/tabtv/backend/internal/workflow/app"
	workflowrepo "github.com/li904338203/tabtv/backend/internal/workflow/repo"
)

// CoreInput 是 BuildCore 的入参。
//
// Media 与 Catalog 由调用方构造并传入：API 还要拿它们挂 handler、跑 SyncProtocols；
// Worker 将来接入时也要自己构造（⚠️ 别在同一个进程里建两份 catalog —— 两份配置缓存）。
type CoreInput struct {
	Cfg     config.Config
	Deps    *bootstrap.Deps
	NextID  func() int64
	Logger  *slog.Logger
	Audit   *audit.Logger
	Media   *mediaapp.Service
	Catalog *catalogapp.Service
}

// Core 是装配好的共享服务。
type Core struct {
	Canvas       *canvasapp.Service
	Gen          *genapp.Service
	Script       *scriptapp.Service
	Workflow     *workflowapp.Service
	GenRepo      *genrepo.Repo
	TextProvider *providerapp.HTTPProvider
	Billing      *billingrepo.Repo
	// Director 是 3D 导演台的 AI 搭景（DRT-19）。两个进程都装：Agent / MCP 的搭景工具在 Worker 里执行。
	Director *directorapp.Service
}

// BuildCore 按原 cmd/api 的顺序装出 canvas → gen → script → workflow。
//
// ⚠️ 顺序有依赖：genRepo 必须在 canvasSvc.SetOutputLookup 之前；
// genSvc.SetGroupBinder(scriptSvc)、genSvc.SetRunAdvancer(wfSvc)、genSvc.SetPresets(...)、
// genSvc.SetMarketing(...) 四条反向注入一条都不能漏（漏了没有编译错误：组子项永远 queued /
// 整图运行永远 running / 预设选了等于没选 / 营销片段静默缺失）。
//
// 不在这里做的：事件桥（genapp.NewEventBridge，依赖 WebSocket hub）、
// 共享资源注册、时间线/支付/内容/运维的装配 —— 仍在 main。
func BuildCore(in CoreInput) (*Core, error) {
	cfg, deps, logger := in.Cfg, in.Deps, in.Logger
	nextID := in.NextID
	auditLog := in.Audit
	mediaSvc := in.Media
	catalogSvc := in.Catalog

	canvasSvc := canvasapp.New(canvasrepo.New(deps.DB.Pool), nextID)
	canvasSvc.SetAudit(auditLog)
	// 复制项目时复制智能剪辑的时间线、清理项目时删掉它们（SCT-07，SC-F-170 / SC-F-171、SCB-11）：
	// 时间线仓储在画布域的事务里做这两件事。
	canvasSvc.SetProjectTimelines(timelinerepo.New(deps.DB.Pool, nextID))
	if cfg.TrashRetention > 0 {
		canvasSvc.SetRetention(cfg.TrashRetention)
	}
	// 项目彻底删除时解除它持有的媒体引用（GAP-17 的收尾）。
	canvasSvc.SetMedia(mediaSvc)
	// ⚠️ 生成域的仓储在这里就要建好：画布域的输入解析要用它查上游产出的归属。
	genRepo := genrepo.New(deps.DB.Pool)

	// 输入解析的查询端口（P04-05）。
	//
	// ⚠️ OutputLookup 必须接上：不接的话，引用 generation 的输入一律判"未就绪"，
	// 于是**整条「上一个节点的产出接成下一个节点的输入」链路都不通** ——
	// 而那正是节点画布的核心。P04 把它挂给 P05、P05 收尾时漏了，
	// P06-03 的视频协议联调（首尾帧、参考图都来自上游节点）才把它暴露出来。
	canvasSvc.SetOutputLookup(outputLookupAdapter{genRepo})
	canvasSvc.SetMediaLookup(mediaLookupAdapter{mediaSvc})
	canvasSvc.SetMaterialLookup(materialLookupAdapter{catalogSvc})
	// 3D 导演台的媒体上传（DRT-03）：截图、预演、泼溅走导演台自己的 purpose，不进「我的素材」。
	canvasSvc.SetDirectorMedia(directorMediaAdapter{mediaSvc})
	canvasSvc.SetDirectorUploadLimits(canvasapp.DirectorUploadLimits{
		PreviewMaxBytes: cfg.Director.PreviewMaxBytes, SplatMaxBytes: cfg.Director.SplatMaxBytes})
	canvasSvc.SetLogger(logger)

	// 生成受理：把报价、输入解析、账本三个端口接进来。
	// ⚠️ 适配器写在组装层，不写进任一领域 —— 让生成域 import 目录域的类型，
	// 或让目录域实现生成域的接口，都会把两个域焊死在一起（08 §1）。
	genSvc := genapp.New(genRepo, nextID)
	genSvc.SetCredits(billingrepo.New(deps.DB.Pool, nextID))
	genSvc.SetQuoter(quoterAdapter{catalogSvc})
	genSvc.SetInputResolver(inputAdapter{canvasSvc})
	// 取消要退款：用户主动取消一个还没跑完的任务，钱必须回去。
	genSvc.SetRefunder(cancelRefunder{billingrepo.New(deps.DB.Pool, nextID), cfg.CompensationValidFor})
	genSvc.SetLogger(logger)
	// 只收地址的上游收不收得到参考媒体（docs/agent/smartcut SCT-38）：在扣费之前说清，不让 Worker 收了钱再退。
	// 两个进程都装（Agent 的生成在 Worker 里受理）。
	genSvc.SetMediaURLDelivery(mediaDelivery(cfg, deps, logger))
	if deps.Cache != nil {
		// 取消与运维补偿在 API 进程里进入终态，也要推事件（同样走 Redis 广播：
		// 等这条任务的那个浏览器可能连在另一台 API 实例上）。
		// ⚠️ 订阅侧的事件桥（NewEventBridge）依赖 WebSocket hub，留在 cmd/api。
		genSvc.SetEvents(genapp.NewRedisEventPublisher(deps.Cache.Client, deps.Cache.Key))
	}

	// 脚本域（P07-01）。⚠️ 它与生成域走的是**两条链**：分镜与提示词合成
	// 是同步文本调用、费用记在 script_llm_call；资产图/分镜图/视频才走队列。
	scriptSvc := scriptapp.New(scriptrepo.New(deps.DB.Pool), nextID)
	scriptSvc.SetQuoter(textQuoteAdapter{catalogSvc})
	scriptSvc.SetLedger(scriptLedger{billingrepo.New(deps.DB.Pool, nextID), cfg.CompensationValidFor})
	scriptSvc.SetPrompts(catalogSvc)
	scriptSvc.SetProjectGuard(ProjectGuard{canvasSvc})
	scriptSvc.SetLogger(logger)
	// 文本模型走 Provider 域的同步入口（**文字模型只有这一个入口**）。
	//
	// ⚠️ API 进程也要能调上游：脚本分镜是**同步**的，不经过 Worker。
	// 出网策略与 Worker 用同一套（production 下禁止内网）。
	textProvider := providerapp.NewHTTPProvider(
		providertransport.NewClient(providertransport.URLPolicy{AllowPrivate: cfg.Dev.AllowPrivateNetwork}),
		catalogSvc, // 目录域直接实现 runcfg.Resolver
		providertransport.URLPolicy{AllowPrivate: cfg.Dev.AllowPrivateNetwork},
		logger)
	textProvider.SetRequestLogger(providerrepo.New(deps.DB.Pool, nextID))
	scriptSvc.SetTextModel(textModelAdapter{textProvider})
	// 资产图走 **generation 队列**（与同步 LLM 那条链分开）：产出是媒体，
	// 要归档、要重试、要按产物计价。
	scriptSvc.SetAssetGenerator(assetGenAdapter{genSvc})
	scriptSvc.SetGroupGenerator(groupGenAdapter{genSvc})
	scriptSvc.SetCanceller(groupCancelAdapter{genSvc})
	scriptSvc.SetBalanceReader(billingrepo.New(deps.DB.Pool, nextID))
	// ⚠️ 取消走 Service 不是 Executor：只在 Worker 那侧接的话，
	// 用户从任务列表取消一个组子项，generation 变成 cancelled 而组里
	// 那一格还显示"排队中"，组的聚合状态永远停在 running、整组再也执行不了。
	genSvc.SetGroupBinder(scriptSvc)
	// 图片预设：受理时把配方拼进提示词（P09-01）。
	genSvc.SetPresets(presetAdapter{catalogSvc})
	// 视频节点的运镜：同一位置拼进快照（两个进程都经过这里，Agent 在 Worker 里受理的生成也带上）。
	genSvc.SetCamera(catalogSvc)
	// 营销片段（MKT-T-21）：与预设同一位置拼进快照。⚠️ 两个进程都经过这里 ——
	// Agent 的 generate 在 Worker 里调受理，只在 API 注入的话那边的生成静默缺片段（MKR-11）。
	genSvc.SetMarketing(newMarketingSource(cfg, deps.DB.Pool, catalogSvc))
	// 3D 导演台约束段（DRT-07，DRD-18）：输入里有导演台截图 / 预演视频时追加。同样两个进程都要：
	// 整图派发与 Agent 的生成都可能带上截图，只接一边的话另一边的生成静默缺少约束段。
	genSvc.SetDirectorGuard(catalogSvc)
	// 创造页直接生成（AGT-42）：报价 / 余额 / 受理与画布节点生成走同一条路。
	canvasSvc.SetQuickAccepter(quickAccepter{
		catalog: catalogSvc, ledger: billingrepo.New(deps.DB.Pool, nextID), gen: genSvc,
	})

	// 整图 DAG 计划器（P08-01）。它只读：取画布、报价、组装计划，不写任何东西。
	wfSvc := workflowapp.New()
	wfSvc.SetCanvas(canvasSource{canvasSvc})
	wfSvc.SetOperations(operationResolver{catalogSvc})
	wfSvc.SetQuoter(nodeQuoter{catalogSvc})
	wfSvc.SetResults(resultLookup{genRepo})
	// 智能剪辑在整图里当静态素材（SCT-09）：导出过且没过期的直接用成片。
	wfSvc.SetSmartcutOutputs(smartcutOutputs{timelinerepo.New(deps.DB.Pool, nextID)})
	wfSvc.SetRuns(workflowrepo.New(deps.DB.Pool))
	wfSvc.SetNodeGen(nodeGenAdapter{genSvc})
	wfSvc.SetNextID(nextID)
	wfSvc.SetCanceller(groupCancelAdapter{genSvc})
	wfSvc.SetLogger(logger)
	// ⚠️ 与组子项同一条路：生成任务到终态后推进整图运行。
	// 只在 Worker 那侧接的话，从 API 取消的任务不会推进运行，
	// 整次运行永远停在 running。
	genSvc.SetRunAdvancer(wfSvc)

	return &Core{
		Canvas:       canvasSvc,
		Gen:          genSvc,
		Script:       scriptSvc,
		Workflow:     wfSvc,
		GenRepo:      genRepo,
		TextProvider: textProvider,
		Director: BuildDirector(DirectorInput{Pool: deps.DB.Pool, Canvas: canvasSvc, Catalog: catalogSvc,
			Media: mediaSvc, Text: textProvider, Cfg: cfg.Director, NextID: nextID, Logger: logger}),
		Billing: billingrepo.New(deps.DB.Pool, nextID),
	}, nil
}

// mediaDelivery 参考媒体的送达判断：主存储 + 公网中转（与 Worker 发参考媒体时用的是同一份配置）。
//
// 中转只在有主存储时才用得上（Worker 也是这样装的：转存要先从主存储取字节）。中转的客户端建不起来时按「没配」处理：
// 结论是「收不到」，受理时说清，而不是让任务扣了费再在 Worker 里失败。
func mediaDelivery(cfg config.Config, deps *bootstrap.Deps, logger *slog.Logger) providerapp.MediaDelivery {
	var d providerapp.MediaDelivery
	if deps.ObjStore == nil {
		return d
	}
	d.Main = deps.ObjStore
	if rc := cfg.RelayStorage; rc.Endpoint != "" {
		store, err := objstore.New(context.Background(), rc)
		if err != nil {
			logger.Warn("建立公网中转存储客户端失败，只收地址的上游将按「收不到素材」受理时拒绝", "error", err)
			return d
		}
		d.Relay = store
	}
	return d
}
