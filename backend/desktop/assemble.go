package desktop

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	agentrepo "github.com/li904338203/tabtv/backend/internal/agent/repo"
	agenthttp "github.com/li904338203/tabtv/backend/internal/agent/transport"
	canvasrepo "github.com/li904338203/tabtv/backend/internal/canvas/repo"
	canvashttp "github.com/li904338203/tabtv/backend/internal/canvas/transport"
	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	catalogrepo "github.com/li904338203/tabtv/backend/internal/catalog/repo"
	cataloghttp "github.com/li904338203/tabtv/backend/internal/catalog/transport"
	collabapp "github.com/li904338203/tabtv/backend/internal/collaboration/app"
	collabrepo "github.com/li904338203/tabtv/backend/internal/collaboration/repo"
	collabhttp "github.com/li904338203/tabtv/backend/internal/collaboration/transport"
	contentapp "github.com/li904338203/tabtv/backend/internal/content/app"
	contentrepo "github.com/li904338203/tabtv/backend/internal/content/repo"
	contenthttp "github.com/li904338203/tabtv/backend/internal/content/transport"
	directorhttp "github.com/li904338203/tabtv/backend/internal/director/transport"
	genhttp "github.com/li904338203/tabtv/backend/internal/generation/transport"
	identityapp "github.com/li904338203/tabtv/backend/internal/identity/app"
	"github.com/li904338203/tabtv/backend/internal/identity/channel"
	identityrepo "github.com/li904338203/tabtv/backend/internal/identity/repo"
	identityhttp "github.com/li904338203/tabtv/backend/internal/identity/transport"
	marketingapp "github.com/li904338203/tabtv/backend/internal/marketing/app"
	marketingrepo "github.com/li904338203/tabtv/backend/internal/marketing/repo"
	marketinghttp "github.com/li904338203/tabtv/backend/internal/marketing/transport"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	mediarepo "github.com/li904338203/tabtv/backend/internal/media/repo"
	mediahttp "github.com/li904338203/tabtv/backend/internal/media/transport"
	obsapp "github.com/li904338203/tabtv/backend/internal/observability/app"
	obsrepo "github.com/li904338203/tabtv/backend/internal/observability/repo"
	obshttp "github.com/li904338203/tabtv/backend/internal/observability/transport"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/platform/bootstrap"
	"github.com/li904338203/tabtv/backend/internal/platform/buildinfo"
	"github.com/li904338203/tabtv/backend/internal/platform/config"
	"github.com/li904338203/tabtv/backend/internal/platform/datadir"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/secret"
	"github.com/li904338203/tabtv/backend/internal/platform/settings"
	"github.com/li904338203/tabtv/backend/internal/projectpkg"
	providerrepo "github.com/li904338203/tabtv/backend/internal/provider/repo"
	providertransport "github.com/li904338203/tabtv/backend/internal/provider/transport"
	scriptapp "github.com/li904338203/tabtv/backend/internal/script/app"
	scripthttp "github.com/li904338203/tabtv/backend/internal/script/transport"
	smartcuthttp "github.com/li904338203/tabtv/backend/internal/smartcut/transport"
	timelinehttp "github.com/li904338203/tabtv/backend/internal/timeline/transport"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
	"github.com/li904338203/tabtv/backend/internal/wiring"
	workflowhttp "github.com/li904338203/tabtv/backend/internal/workflow/transport"
)

// 单进程装配（TT-T-07，dev/01 TTD-1/§3）。
//
// 来源系统的 cmd/api（HTTP、受理、同步模型调用、整图派发）与 cmd/worker（消费队列、执行、渲染、
// 清理与恢复扫描）合成这一个装配。两边原本各建一份的服务在这里只建一份 —— 来源系统里那些
// 「两个进程都要装，否则另一边静默缺失」的注释，在单进程下自然成立。
//
// 不装配的域（TT-F-90~95）：支付、账本的用户与管理路由、MCP、运维台、API 请求日志与统计、
// 内容运营的管理端（Banner / 公告 / 作品审核）。身份域只用来签发与校验本机令牌（见 local.go）。

// app 汇总装配结果。
type assembled struct {
	handler http.Handler
	bg      *background
}

func assemble(ctx context.Context, cfg config.Config, deps *bootstrap.Deps, ident *Identity,
	tokens tokenService, gen *id.Generator, logger *slog.Logger, st *settings.Store, dirs datadir.Dirs) (*assembled, error) {
	pool := deps.DB.Pool
	auditLog := audit.New(pool, gen.Next, logger)

	// ---- 身份：只校验本机令牌（不挂登录、注册、找回密码等路由）
	identitySvc := identityapp.New(identityrepo.New(pool), tokens.svc, nil,
		channel.NewRegistry(channel.LogSender{K: channel.Email, Logger: logger}), gen.Next)
	cipher := secret.Plain() // Key 明文存库（TTD-13）
	identitySvc.SetSecretCipher(cipher)
	identitySvc.SetAudit(auditLog)
	identity := identityhttp.NewHandler(identitySvc, tokens.svc)

	// ---- 协作：只用工作区上下文与权限闸（RequireWorkspace / GuardByMethod）
	collabSvc := collabapp.New(collabrepo.New(pool), gen.Next)
	collabSvc.SetAudit(auditLog)
	collabSvc.SetLogger(logger)
	collab := collabhttp.NewHandler(collabSvc, identityhttp.UserIDFrom)

	// ---- 目录（模型、厂商、操作、提示词、素材、预设）
	catalogSvc := catalogapp.New(catalogrepo.New(pool), gen.Next)
	catalogSvc.SetAudit(auditLog)
	catalogSvc.SetSecretCipher(cipher)
	catalogSvc.SetLogger(logger)
	catalogSvc.SetCache(catalogapp.NewConfigCache(30 * time.Minute))
	catalogSvc.SetBuiltinPresets(builtinPresets{})
	if err := catalogSvc.SyncProtocols(ctx); err != nil {
		return nil, err
	}
	subjectID := func(r *http.Request) int64 {
		c := identityhttp.ClaimsFrom(r.Context())
		if c == nil {
			return 0
		}
		v, _ := c.SubjectID()
		return v
	}
	userID := func(r *http.Request) int64 {
		v, _ := identityhttp.UserIDFrom(r.Context())
		return v
	}
	catalogHandler := cataloghttp.NewHandler(catalogSvc, subjectID)
	catalogHandler.SetProber(providerProber{svc: catalogSvc,
		client: providertransport.NewClient(providertransport.URLPolicy{AllowPrivate: cfg.Dev.AllowPrivateNetwork})})
	catalogHandler.SetUserIDResolver(subjectID)

	// ---- 媒体
	analysisWake := wiring.NewAnalysisWake()
	mediaSvc := mediaapp.New(mediarepo.New(pool), deps.ObjStore, gen.Next)
	mediaSvc.SetAnalysisWaker(analysisWake)
	mediaSvc.SetLogger(logger)
	registerMediaRefs(mediaSvc, pool, gen.Next)
	mediaHandler := mediahttp.NewHandler(mediaSvc, subjectID, subjectID)

	// ---- 内容：首页最近项目、我的资产库、创作记录
	contentSvc := contentapp.New(contentrepo.New(pool, gen.Next))
	contentSvc.SetAudit(auditLog)
	contentSvc.SetLogger(logger)
	contentSvc.SetProjectAgents(projectAgents{canvasrepo.New(pool)})
	contentHandler := contenthttp.NewHandler(contentSvc, identityhttp.UserIDFrom)
	contentHandler.SetMedia(mediaHandler)

	// ---- 生成 / 画布 / 脚本 / 整图（与来源系统同一份共享装配）
	core, err := wiring.BuildCore(wiring.CoreInput{
		Cfg: cfg, Deps: deps, NextID: gen.Next, Logger: logger,
		Audit: auditLog, Media: mediaSvc, Catalog: catalogSvc,
	})
	if err != nil {
		return nil, err
	}
	canvasSvc, genSvc, scriptSvc, wfSvc := core.Canvas, core.Gen, core.Script, core.Workflow
	canvasSvc.SetEvents(deps.Bus)

	// ---- 时间线（含渲染受理）与智能剪辑
	tlSvc := wiring.NewTimelineService(wiring.TimelineInput{
		Pool: pool, NextID: gen.Next, Media: mediaSvc, Script: scriptSvc, Canvas: canvasSvc,
		Catalog: catalogSvc, CompensationValidFor: cfg.CompensationValidFor, Logger: logger,
		Events: deps.Bus, Smartcut: cfg.Smartcut,
	})
	contentHandler.SetWorkSource(workSourceGuard{gen: genSvc, tl: tlSvc})
	smartcutSvc := wiring.NewSmartcutService(wiring.SmartcutInput{
		Timeline: tlSvc, Canvas: canvasSvc, Media: mediaSvc, Generations: core.GenRepo, Log: logger,
		Plan: &wiring.SmartcutPlanInput{Pool: pool, Timeline: tlSvc, Catalog: catalogSvc, Media: mediaSvc,
			Generations: core.GenRepo, Text: core.TextProvider, Events: deps.Bus, Cfg: cfg.Smartcut,
			NextID: gen.Next, Logger: logger, RenderTimeline: timelinehttp.ViewJSON,
			Board: wiring.NewSmartcutBoard(scriptSvc, mediaSvc)},
		Speech: &wiring.SmartcutSpeechInput{Catalog: catalogSvc, Gen: genSvc, Media: mediaSvc, Timeline: tlSvc,
			Events: deps.Bus},
		Clip: &wiring.SmartcutClipInput{Gen: genSvc, GenRepo: core.GenRepo, Media: mediaSvc, Timeline: tlSvc,
			Render: timelinehttp.ViewJSON, Catalog: catalogSvc},
	})
	genSvc.SetSmartcutBinder(wiring.SmartcutBinder{Svc: smartcutSvc})

	// 会员等级：本机没有会员体系，恒为 0（全部模型的门槛都是 0，TT-T-09）。
	memberLevelOf := func(*http.Request) int64 { return 0 }

	// ---- 营销与 Agent（API 侧：收消息、建会话、规划审批）
	marketingSvc := wiring.NewMarketingService(wiring.MarketingServiceInput{
		Repo: marketingrepo.New(pool), Catalog: catalogSvc,
		Settings: marketingapp.Settings{MaxProductImages: cfg.Marketing.MaxProductImages,
			MaxVariants: cfg.Marketing.MaxVariants, AgentEnabled: cfg.Agent.Enabled,
			MaxProducts: cfg.Marketing.MaxProducts, MaxCharacters: cfg.Marketing.MaxCharacters},
	})
	marketingLib := wiring.NewMarketingLibrary(wiring.MarketingLibraryInput{
		Repo: marketingrepo.New(pool), Media: mediaSvc, NextID: gen.Next, Logger: logger,
		Settings: marketingapp.LibrarySettings{MaxProductImages: cfg.Marketing.MaxProductImages,
			MaxProducts: cfg.Marketing.MaxProducts, MaxCharacters: cfg.Marketing.MaxCharacters},
	})
	var agentScene agentapp.SceneResolver
	var agentLib *marketingapp.Library
	if cfg.Marketing.Enabled {
		agentScene, agentLib = wiring.AgentScene{Svc: marketingSvc}, marketingLib
	}
	billingRepo := core.Billing
	agentSvc := wiring.NewAgentService(wiring.AgentAPIInput{
		Repo: agentrepo.New(pool), NextID: gen.Next,
		Canvas: canvasSvc, Media: mediaSvc, Script: scriptSvc, Catalog: catalogSvc,
		Billing: newBillingService(pool, gen.Next, auditLog, logger), Gen: genSvc,
		Events: wiring.AgentEvents{Bus: deps.Bus}, Logger: logger,
		Chat: core.TextProvider, Ledger: billingRepo, Compensate: cfg.CompensationValidFor,
		Scene: agentScene, MaxVariants: cfg.Marketing.MaxVariants, Library: agentLib,
	})
	agentHandler := agenthttp.NewHandler(agentSvc, userID, cfg.Agent.Enabled)
	agentHandler.SetMemberLevelResolver(memberLevelOf)
	agentHandler.SetMedia(mediaHandler)
	skillAdmin := agentapp.NewSkillAdmin(agentrepo.New(pool), skillModels{catalogSvc}, wiring.AgentSkillRenderer{}, gen.Next)
	skillAdmin.SetAudit(auditLog)
	skillAdmin.SetBuiltins(builtinSkills{})
	skillAdmin.SetMarketingChecks(wiring.AgentSkillCopy{}, wiring.AgentSkillSlots{})
	agentAdmin := agenthttp.NewAdminHandler(skillAdmin, subjectID)

	marketingHandler := marketinghttp.NewHandler(marketingSvc, cfg.Marketing.Enabled)
	marketingHandler.SetMedia(mediaHandler)
	marketingHandler.SetLibrary(marketingLib, subjectID)
	marketingHandler.SetCharacterGen(wiring.NewMarketingCharacterGen(genSvc, catalogSvc,
		wiring.MarketingWorkspaceMedia{Media: mediaSvc}), memberLevelOf)
	marketingAdmin := marketinghttp.NewAdminHandler(wiring.NewMarketingAdmin(wiring.MarketingAdminInput{
		Repo: marketingrepo.New(pool), Catalog: catalogSvc, Agent: agentrepo.New(pool),
		Media: mediaSvc, NextID: gen.Next, Audit: auditLog,
	}), subjectID)
	presetAdmin := marketingapp.NewPresetAdmin(marketingrepo.New(pool), wiring.MarketingMedia{Media: mediaSvc}, gen.Next)
	presetAdmin.SetAudit(auditLog)
	marketingAdmin.SetPresets(presetAdmin)

	// ---- HTTP handlers
	canvasHandler := canvashttp.NewHandler(canvasSvc, identityhttp.UserIDFrom)
	quickGen := canvashttp.NewQuickHandler(canvasSvc, userID)
	quickGen.SetMemberLevelResolver(memberLevelOf)
	genHandler := genhttp.NewHandler(genSvc)
	genHandler.SetMemberLevelResolver(memberLevelOf)
	catalogHandler.SetMemberLevelResolver(memberLevelOf)
	workflowHandler := workflowhttp.New(wfSvc, userID)
	workflowHandler.SetMemberLevelResolver(memberLevelOf)
	directorHandler := directorhttp.NewHandler(core.Director, identityhttp.UserIDFrom)
	directorHandler.SetMemberLevelResolver(memberLevelOf)
	timelineHandler := timelinehttp.NewHandler(tlSvc, userID)
	timelineHandler.SetMemberLevelResolver(memberLevelOf)
	timelineAdmin := timelinehttp.NewAdminHandler(tlSvc, subjectID)
	smartcutHandler := smartcuthttp.NewHandler(smartcutSvc)
	smartcutHandler.SetMemberLevelResolver(memberLevelOf)
	smartcutHandler.SetUserIDs(identityhttp.UserIDFrom)
	scriptHandler := scripthttp.NewHandler(scriptSvc, identityhttp.UserIDFrom)

	r := chi.NewRouter()
	r.Use(httpx.TraceID)
	r.Use(httpx.Recover)
	r.Use(ident.LocalAuth)
	r.Use(requestTimeout(30*time.Second, scriptapp.SyncLLMRequestTimeout))

	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"status": "ok", "version": buildinfo.Version})
		})
		identity.MountAdmin(r) // 管理端 /admin/auth/me 等（设置页读当前权限用）
		mountLocalRoutes(r, ident, pool)
		mountSettingsRoutes(r, st, dirs)
		mountDiagnostics(r, pool)
		// 上游调用日志（设置 → 调用日志，TT-F-36）：沿用来源系统的管理端检索 /admin/request-logs/provider。
		obsSvc := obsapp.New(obsrepo.New(pool, gen.Next))
		obsSvc.SetProviderLogs(providerrepo.New(pool, gen.Next))
		obshttp.NewHandler(obsSvc).Mount(r, identity.RequireAdmin, identity.RequirePermission)
		wsGuard := func(next http.Handler) http.Handler {
			return collab.RequireWorkspace(collabhttp.GuardByMethod(next))
		}
		wsCtx := collab.RequireWorkspace
		catalogHandler.MountUser(r, identity.RequireUser)
		catalogHandler.MountQuote(r, identity.RequireUser)
		catalogHandler.MountAdmin(r, identity.RequireAdmin, identity.RequirePermission)
		catalogHandler.MountModelAdmin(r, identity.RequireAdmin, identity.RequirePermission)
		mountModelTemplates(r, modelTemplates{svc: catalogSvc, repo: catalogrepo.New(pool), who: subjectID},
			identity.RequireAdmin, identity.RequirePermission)
		mountModelConfig(r, modelConfig{svc: catalogSvc, repo: catalogrepo.New(pool), st: st,
			decrypt: cipher.Decrypt, who: subjectID}, identity.RequireAdmin, identity.RequirePermission)
		catalogHandler.MountMaterial(r, identity.RequireUser, identity.RequireAdmin, identity.RequirePermission)
		catalogHandler.MountPrompt(r, identity.RequireAdmin, identity.RequirePermission)
		catalogHandler.MountPresetAdmin(r, identity.RequireAdmin, identity.RequirePermission)
		timelineAdmin.MountAdmin(r, identity.RequireAdmin, identity.RequirePermission)
		mediaHandler.Mount(r, identity.RequireUser, identity.RequireAdmin, wsGuard)
		canvasHandler.Mount(r, identity.RequireUser, wsGuard)
		directorHandler.Mount(r, identity.RequireUser, wsGuard)
		quickGen.Mount(r, identity.RequireUser, wsGuard)
		scriptHandler.Mount(r, identity.RequireUser, wsGuard)
		workflowHandler.Mount(r, identity.RequireUser, wsGuard, wsCtx)
		timelineHandler.Mount(r, identity.RequireUser, wsGuard, wsCtx)
		smartcutHandler.Mount(r, identity.RequireUser, wsGuard)
		agentHandler.Mount(r, identity.RequireUser, wsGuard)
		agentAdmin.MountAdmin(r, identity.RequireAdmin, identity.RequirePermission)
		marketingAdmin.MountAdmin(r, identity.RequireAdmin, identity.RequirePermission)
		marketingHandler.MountPublic(r)
		marketingHandler.MountLibrary(r, identity.RequireUser, wsGuard)
		marketingHandler.MountCharacterGen(r, identity.RequireUser, wsCtx)
		contentHandler.MountUser(r, identity.RequireUser, wsCtx)
		contentHandler.MountTasks(r, identity.RequireUser, wsGuard, wsCtx)
		genHandler.Mount(r, identity.RequireUser, wsGuard, userID)
		// 项目 zip 导出 / 导入（TT-T-33）：跨画布、脚本、时间线、导演台多张表的整体搬运，单机版在装配层编排。
		projectpkg.NewHandler(projectpkg.New(pool, deps.ObjStore, gen.Next, buildinfo.Version),
			func(*http.Request) projectpkg.Owner {
				return projectpkg.Owner{WorkspaceID: ident.WorkspaceID, UserID: ident.UserID, PayerID: ident.PayerID}
			}).Mount(r, identity.RequireUser, wsGuard)
	})

	bg := &background{
		cfg: cfg, deps: deps, gen: gen, logger: logger,
		media: mediaSvc, catalog: catalogSvc, core: core, timeline: tlSvc, smartcut: smartcutSvc,
		agent: agentSvc, analysisWake: analysisWake, billing: billingRepo, settings: st,
	}
	return &assembled{handler: r, bg: bg}, nil
}

// longRunningRoutes 是同步调模型或大文件上传的端点（与来源系统 cmd/api 同一份清单）。
var longRunningRoutes = []*regexp.Regexp{
	regexp.MustCompile(`^/api/v1/scripts/[^/]+/storyboard$`),
	regexp.MustCompile(`^/api/v1/scripts/[^/]+/prompts/compose$`),
	regexp.MustCompile(`^/api/v1/agent/prompt-optimize$`),
	regexp.MustCompile(`^/api/v1/projects/[^/]+/director-scenes/[^/]+/(previews|assets)$`),
	regexp.MustCompile(`^/api/v1/projects/[^/]+/director-scenes/[^/]+/build$`),
	regexp.MustCompile(`^/api/v1/projects/[^/]+/smartcuts/[^/]+/(plan|edit|translate)$`),
	regexp.MustCompile(`^/api/v1/projects/[^/]+/smartcuts/[^/]+/plan/[^/]+/apply$`),
	// 桌面版：媒体上传也放宽（本地盘写入快，但大视频读请求体仍可能超过 30 秒）。
	regexp.MustCompile(`^/api/v1/media$`),
	// 项目包：打包 / 解包要搬全部媒体字节（TT-T-33）。
	regexp.MustCompile(`^/api/v1/projects/[^/]+/package$`),
	regexp.MustCompile(`^/api/v1/projects/import$`),
}

// requestTimeout：同步调模型的端点用 long，其余用 normal。
//
// ⚠️ 不能在路由组里再挂一个更长的 Timeout 来「放宽」：context 的截止时间只会取更早的那个。
// ⚠️ 媒体下载（GET /media/{id}）不设上限：几百 MB 的成片在 30 秒内传不完，而 WebView 是在本机读。
func requestTimeout(normal, long time.Duration) func(http.Handler) http.Handler {
	short, extended := middleware.Timeout(normal), middleware.Timeout(long)
	return func(next http.Handler) http.Handler {
		shortH, longH := short(next), extended(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && mediaBytesRoute.MatchString(r.URL.Path) {
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

var mediaBytesRoute = regexp.MustCompile(`^/api/v1/(media|generations/[^/]+/outputs)/`)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
