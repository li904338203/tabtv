// Package config 从环境变量装载运行配置。
//
// 分层原则（ADR-0001 §8.1）：环境变量/Secret Manager 提供静态运行配置；
// Provider、模型、画布等业务配置放数据库，不走这里。
//
// 本包只做装载与校验，**不建立任何连接** —— 连接在 P01-02 各自的 platform 包里。
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// 运行环境。production 下所有开发开关必须关闭，否则启动即失败。
const (
	EnvDev        = "dev"
	EnvStaging    = "staging"
	EnvProduction = "production"
)

type Config struct {
	Env      string
	HTTPAddr string
	// WSAllowedOrigins 是允许**跨源**建立实时通道（`/ws/events`）的页面来源：host[:port]，可带通配（`*.example.com`）。
	//
	// ⚠️ 为空 = 只接受同源。生产里页面与 API 走同一个域名（反向代理），同源即可、不必配；
	//    开发时页面在 :3000、API 在 :8180 —— 不配的话握手直接被拒（`Origin is not authorized`），
	//    实时通道**从来连不上**，前端反复重连而界面只显示「离线」，任务进度全靠轮询。
	// ⚠️ production 下不许配 `*`：实时通道的鉴权虽然在首帧 token，来源校验仍是跨站劫持的第一道闸。
	WSAllowedOrigins []string
	// HealthCheckTimeout 是单个依赖的探活上限。
	// 探活卡住比依赖挂掉更糟：readyz 迟迟不返回，编排系统会重启一个其实健康的进程。
	HealthCheckTimeout time.Duration
	Log                Log
	Postgres           Postgres
	Redis              Redis
	Queue              Queue
	Storage            Storage
	// RelayStorage 是可选的「公网中转」对象存储（marketing dev/01 R-15）。Endpoint 为空 = 不启用。
	//
	// 主存储签出的地址上游够不着（本地 MinIO），而协议又只收地址（UniArt，protocol.MediaURLRequiring）时，
	// Worker 把要发出去的那几张参考媒体转存一份到这里，发这边的签名地址。主存储因此可以留在本地：
	// 前端照常快，只有真要发给这类上游的文件才走一趟中转（实测把主存储整体搬到带宽 5Mbps 的服务器上，
	// 前端取图全部 30 秒超时）。签名有效期沿用 Storage.SignTTL。
	RelayStorage Storage
	// RelayPrefix 是中转副本在桶里的前缀（默认 relay/）：与桶里的其他对象分开，好单独配过期规则。
	RelayPrefix string
	Auth        Auth
	Dev         Dev
	// TrashRetention 回收站保留期。⚠️ 这是**设计建议值**（30 天），不是用户已批准的政策。
	TrashRetention time.Duration
	// LogRetention 请求日志保留期（P13-04）。
	//
	// ⚠️ `api_request_log` 与 `provider_request_log` 是**唯二会无限增长的表**
	// （每次请求 / 每次上游调用各一条，还带着请求体）—— 不清理的话磁盘迟早满，
	// 而那一天所有写库的操作会一起失败，看起来完全不像日志的问题。
	//
	// ⚠️ 同样是**设计建议值**（30 天），正式留存期是 PARAM-05 的内容。
	LogRetention time.Duration
	// CompensationValidFor 过期批次退款时新发补偿批次的有效期。
	// ⚠️ **PARAM-02，策略未批准**：30 天是设计建议值，等待商业侧确认。
	CompensationValidFor time.Duration
	// GenerationMaxRunningPerPayer 单个计费主体的运行中任务上限。
	//
	// ⚠️ 这是**运行态节流，不是队列上限**：批量提交 N 条正常全部入队，
	// 只是同时在跑的不超过这个数。发起时就拦的话，用户批量提交十个会有七个直接失败。
	GenerationMaxRunningPerPayer int
	// Agent 是画布 Agent 的配置（dev/01 §12）。
	Agent Agent
	// MCP 是对外 MCP 服务的配置（docs/mcp/dev/01 §10）。M2 的配置项随 M2 任务加入。
	MCP MCP
	// Marketing 是营销 Agent 的配置（docs/agent/marketing dev/01 §11，MKT-T-04）。
	Marketing Marketing
	// Director 是 3D 导演台的配置（docs/agent/director dev/01 §9）。
	Director Director
	// Smartcut 是智能剪辑的配置（docs/agent/smartcut dev/01 §10）。后续卡片的配置项陆续加进来。
	Smartcut Smartcut
}

// Smartcut 是智能剪辑的配置（dev/01 §10）。各项都是**设计建议值**。
type Smartcut struct {
	// FFmpegMaxConcurrent 每个 Worker 进程同时跑几个 ffmpeg（渲染与预览素材扫描器共用，SCD-30）。
	//
	// ⚠️ 两边各自限流的话合起来照样超：一次导出撞上一段长视频的预览转码，Worker 把机器啃满。
	FFmpegMaxConcurrent int
	// AnalysisScanInterval 预览素材扫描器的兜底间隔：排队时会经 Redis 叫醒，叫醒丢了最多晚这么久。
	AnalysisScanInterval time.Duration
	// AnalysisLease 分析的租约。执行者每 1/3 租约续一次；进程崩溃后最多这么久被别的实例接管。
	AnalysisLease time.Duration
	// RenderUserConcurrency / RenderUserPerHour 导出按用户限流（导出免费，SC-PARAM-19：同时 1 个、每小时 20 次）。
	RenderUserConcurrency int
	RenderUserPerHour     int
	// RenderDownloadTTL 成片下载签名链接的有效期（SCK-10，默认 5 分钟）。
	RenderDownloadTTL time.Duration
	// PlanConcurrency / PlanPerHour AI 剪辑按用户限流（免费，SC-PARAM-19：同时 1 次、每小时 20 次，多方案计 1 次）。
	PlanConcurrency int
	PlanPerHour     int
	// PlanTimeout / PlanVisionTimeout 单次模型调用上限（SC-PARAM-29：3 分钟；带缩略图拼图时 5 分钟）。
	PlanTimeout       time.Duration
	PlanVisionTimeout time.Duration
	// PlanStaleAfter 「进行中」超过它由恢复扫描判失败。⚠️ 必须大于长耗时路由的上限（15 分钟）。
	PlanStaleAfter time.Duration
	// AIEnabled 是 Agent / MCP 能不能用 AI 剪辑（smartcut_plan 工具注不注册）。缺省关：这一版智能剪辑只保留手动剪辑
	// （2026-10-29 用户决定；网页那侧由 NUXT_PUBLIC_SMARTCUT_AI 管）。关着时建节点、查看、导出照常。
	AIEnabled bool
}

// Director 是 3D 导演台的配置（dev/01 §9）。各项都是**设计建议值**（DR-PARAM-07 / 10 / 11 / 13）。
type Director struct {
	// PreviewMaxBytes 预演视频上限：只能收紧视频的默认上限（媒体域 Limits.Tighten）。
	PreviewMaxBytes int64
	// SplatMaxBytes 泼溅文件上限。
	SplatMaxBytes int64
	// BuildConcurrency 同一用户同时进行的 AI 搭景数（DR-PARAM-07）。
	BuildConcurrency int
	// BuildPerHour 同一用户每小时 AI 搭景次数（DR-PARAM-07）。
	BuildPerHour int
	// BuildTimeout 单次模型调用上限（DR-PARAM-13）。
	BuildTimeout time.Duration
	// BuildStaleAfter 「进行中」超过它由恢复扫描判失败。⚠️ 必须大于长耗时路由的上限（15 分钟）：
	// 小于它的话，一个合法地还在等模型的请求会被判死，随后又写回成功（CLAUDE.md §5.7 同一条）。
	BuildStaleAfter time.Duration
	// BuildImageWidth 参考图内联的缩略图档位，只接受媒体域已有的 160 / 320 / 640。
	BuildImageWidth int
}

// DirectorLongRouteLimit 是长耗时路由（同步调模型）的请求上限，与 cmd/api 的 requestTimeout 同一个数。
// 搭景的判死阈值必须大于它（见 Director.BuildStaleAfter）。
const DirectorLongRouteLimit = 15 * time.Minute

// Marketing 是营销 Agent 的配置（dev/01 §11、MKD-16）。
//
// ⚠️ 两个上限是**设计建议值**（MK-11 / MK-12），经 `GET /marketing/config` 下发、前后端用同一个数；
// 前端写死的话改了环境变量界面还是旧数，表现为「界面允许 20 张、提交时被拒」。
// 改环境变量要重启进程（dev/01 §12 #1 按建议放环境变量，未做后台设置表）。
type Marketing struct {
	// Enabled 总开关：关闭时营销路由不挂（404），侧栏入口由 `/marketing/config` 的 404 决定隐藏。
	Enabled bool
	// MaxVariants 一次批量生成 / 裂变的变体上限（MK-11）。
	MaxVariants int
	// MaxProductImages **每个商品**的图片上限（MK-22，默认 10）；技能槽位声明的 maxCount 也不得超过它。
	//
	// ⚠️ 语义在 MKT-T-33 由「一次创作临时上传的商品图上限（MK-12，默认 20）」改为「商品库里每个商品的图片上限」，
	// 默认值随之 20 → 10。已建的营销会话不受影响：会话里是建会话时冻结的快照（MKD-25），
	// 下游（选图、换图、生成）只读快照、不按这个数重新校验，旧会话里那 20 张照常可用。
	MaxProductImages int
	// MaxProducts 每个工作区未删除商品数上限（配额，MKT-PARAM-18，设计建议值 500）。
	MaxProducts int
	// MaxCharacters 每个工作区未删除角色数上限（配额，MKT-PARAM-18，设计建议值 200；角色库在 MKT-T-34 接入）。
	MaxCharacters int
	// ReasoningImageWidth 给推理模型看的缩略图档位，只接受媒体域已有档位（160 / 320 / 640）。
	ReasoningImageWidth int
}

// MCP 是对外 MCP 服务的配置（docs/mcp/dev/01 §10、§12 R6/R7）。
type MCP struct {
	// Enabled 平台总开关（环境变量层）：关闭时**不挂路由**，/mcp、/mcp/readonly、/mcp/x/* 一律 404。
	// 运行时开关（管理端可改）在 MCPT-14 的 mcp_setting 表。
	Enabled bool
	// PublicURL 是 MCP 对外地址（如 https://example.com/mcp，也接受根 https://example.com）：
	// 其 Origin 被视为同源；去掉末尾 /mcp 后是 OAuth 的 issuer 与受保护资源 resource 的根（MCPT-12）。
	// 现有配置里没有可推导的公网地址（R6），需显式配置。
	// ⚠️ production 下必填且必须 https：配错的话元数据给出错误的 resource，客户端表现为「授权成功但每次 401」。
	// 非 production 留空时按请求的 Host 推导（本地开发）。
	PublicURL string
	// WebPublicURL 是用户端地址（SEEAITV_WEB_PUBLIC_URL，如 https://example.com）：OAuth 授权端点把用户
	// 送到 <它>/mcp/authorize 的同意页（MCPT-12/13）；花钱工具的页面审批链接是 <它>/mcp/approve/{quoteId}
	// （MCPT-13，开发下留空时回落 http://localhost:3000）。production 下必填且必须 https；
	// 非 production 留空时取请求主机名 + :3000（本地用户端端口）。
	WebPublicURL string
	// ResultMaxTokens 单次工具结果的截断阈值（估算 token，MCP-I-07）。
	ResultMaxTokens int
	// MediaURLTTL 是 media.get 下发的签名直链有效期（MCP-PARAM-06，**设计建议值** 1 小时）。
	// ⚠️ 与对象存储的 SignTTL（6 小时，给上游取参考图）分开：这条地址交给外部客户端，越短泄漏面越小。
	MediaURLTTL time.Duration
	// RateUserClient / RateWorkspace 是两级限流的每分钟工具调用上限（「用户 × 客户端」/「工作区」，
	// MCP-F-75）。60 / 300 是 MCP-PARAM-01 的**设计建议值**，不是已批准的政策；0 = 不限。
	RateUserClient int
	RateWorkspace  int
	// QuoteTTL 是 quote 签发的 quoteId 有效期（MCP-PARAM-02，**设计建议值** 10 分钟，MCPT-05）。
	// 过期后要重新报价：价格与余额都可能已经变了。
	QuoteTTL time.Duration
	// WaitMax 是 tasks.wait 单次最长等待（MCP-PARAM-03，**设计建议值** 50 秒，MCPT-11）。
	// ⚠️ 必须小于单次 tools/call 的截止（60 秒，mcp/transport.DefaultCallTimeout）：
	// 等满之后还要再查一次、序列化、写审计，撞上截止的话客户端拿到的是「超时被中止」而不是当前状态。
	WaitMax time.Duration
}

// Agent 是画布 Agent 的配置（dev/01 §12）。
type Agent struct {
	// Enabled 总开关：关闭时 API 路由 404、Worker 不消费 agent.turns。
	Enabled bool
	// TurnMaxCalls 一轮内推理模型的调用上限（AG-PARAM-06，设计建议值 20）。
	TurnMaxCalls int
	// ContextChars 发给模型的会话消息字符预算（AG-PARAM-04，设计建议值 60000，按 Unicode 码点）。
	ContextChars int
	// RunnerConcurrency Worker 并发跑的轮数（agent.turns 的 prefetch）。
	RunnerConcurrency int
	// CardExpire 卡片过期时间（设计建议值 7 天）。
	CardExpire time.Duration
	// MaxOutputTokens 推理模型单次输出上限的缺省值（04 文档 P-09，设计建议值 8192）。
	// 只对必须带上限的协议生效（Anthropic 的 max_tokens 必填，此前写死 4096：
	// 一份长 /plan.md 或一次多项 submit_plan 的参数就会被截断）。0 = 用协议缺省。
	// ⚠️ 不要超过所用推理模型支持的最大输出，超了上游直接 400。
	MaxOutputTokens int
}

type Auth struct {
	// JWTSecret 至少 32 字节。过短会在启动时被拒绝，而不是等线上被爆破。
	JWTSecret string
	// SecretKey 用于加密落库的 Provider/SMTP/短信凭据。
	// ⚠️ 与 JWTSecret **分开**：签名密钥泄漏只影响会话，
	// 而加密密钥泄漏会让库里所有上游凭据被解开。
	SecretKey string
	// SecretKeyVersion 当前加密用的密钥版本号（密文前缀 `v<n>:`）。默认 1。
	SecretKeyVersion int
	// OldSecretKeys 历史版本的密钥材料，**只用于解密**。
	// 键是版本号，来自 `SEEAITV_SECRET_KEY_V<n>`。
	//
	// ⚠️ 没有这一项的话**密钥轮换在物理上不可能**（P14-03 实测）：
	// `secret` 包从一开始就支持带版本的密文（`v1:`、`v2:`…）并在注释里写明
	// "否则轮换必须停机重写全表，实际上永远不会发生"，但两个 main 都写死
	// `secret.New(map[int]string{1: keyMaterial}, 1)` —— 于是运维按安全策略
	// 换掉 `SEEAITV_SECRET_KEY` 之后，库里全部 `v1:` 密文都是**用旧 key 加的、
	// 而 v1 现在指向新 key**：所有 Provider 密钥、支付商户私钥、SMTP 凭据
	// 当场变成解不开的字节。症状是每一次模型调用和每一笔支付都失败，
	// 错误是"解密失败"，**看起来像数据损坏**；而唯一的退路是把旧 key 找回来——
	// 可"轮换"通常意味着旧 key 已经作废了。
	OldSecretKeys map[int]string
}

// SecretKeys 汇成 `secret.New` 要的那张表：历史版本 + 当前版本。
func (a Auth) SecretKeys() map[int]string {
	out := make(map[int]string, len(a.OldSecretKeys)+1)
	for v, m := range a.OldSecretKeys {
		out[v] = m
	}
	out[a.CurrentKeyVersion()] = a.SecretKey
	return out
}

// CurrentKeyVersion 当前版本号，缺省 1。
func (a Auth) CurrentKeyVersion() int {
	if a.SecretKeyVersion <= 0 {
		return 1
	}
	return a.SecretKeyVersion
}

type Log struct {
	Level  string // debug|info|warn|error
	Format string // json|text
}

type Postgres struct {
	DSN             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
}

type Redis struct {
	Addr string
	DB   int
	// Namespace 是新系统独占的键前缀。绝不能与旧系统共用（INIT-07）。
	Namespace string
}

type Queue struct {
	URL string
	// Exchange 前缀。基线 seeaitv.*，禁止与旧系统的 aitv.* 撞名（ADR-0001 TECH-05）。
	ExchangePrefix string
	Prefetch       int
}

type Storage struct {
	// Type 为 s3 或 local。local 仅限单机开发：它无法预签名，
	// 参考图只能内联 base64，且 API 与 Worker 分机部署时读不到对方的盘（ADR-0001 TECH-07）。
	Type      string
	Endpoint  string
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	// SignTTL 默认 6h。不要调短：地址在提交任务时签发，
	// 上游可能排队几分钟才来取，失败重试更晚。
	SignTTL time.Duration
	// PathStyle 寻址方式（SEEAITV_STORAGE_PATH_STYLE，默认 true）。MinIO 等自建服务只认 path-style；
	// 火山 TOS 的 S3 兼容端点**只认 virtual-hosted**（2026-11-02 接入），要配成 false。
	PathStyle bool
	// Prefix 全部对象键的公共前缀（SEEAITV_STORAGE_PREFIX，默认空）。几个环境共用一个桶时用它隔开：
	// 不隔开的话，各环境的清理作业（临时片段、过期导出、媒体回收）会删掉别的环境的文件。
	// 只在存储客户端里加 / 去，库里存的对象键不带它。
	Prefix string
}

// Dev 是开发开关。production 下任意一项为 true 都会导致启动失败。
type Dev struct {
	FakeProvider        bool
	FakePayment         bool
	AllowPrivateNetwork bool
}

func Load() (Config, error) {
	c := Config{
		Env:                env("SEEAITV_ENV", EnvDev),
		HTTPAddr:           env("SEEAITV_HTTP_ADDR", ":8180"),
		WSAllowedOrigins:   envList("SEEAITV_WS_ALLOWED_ORIGINS"),
		HealthCheckTimeout: envDuration("SEEAITV_HEALTH_TIMEOUT", 2*time.Second),
		Log: Log{
			Level:  env("SEEAITV_LOG_LEVEL", "info"),
			Format: env("SEEAITV_LOG_FORMAT", "text"),
		},
		TrashRetention:               envDuration("SEEAITV_TRASH_RETENTION", 30*24*time.Hour),
		LogRetention:                 envDuration("SEEAITV_LOG_RETENTION", 30*24*time.Hour),
		CompensationValidFor:         envDuration("SEEAITV_CREDIT_COMPENSATION_VALID_FOR", 30*24*time.Hour),
		GenerationMaxRunningPerPayer: envInt("SEEAITV_GENERATION_MAX_RUNNING_PER_PAYER", 3),
		Agent: Agent{
			Enabled:           envBool("SEEAITV_AGENT_ENABLED", true),
			TurnMaxCalls:      envInt("SEEAITV_AGENT_TURN_MAX_CALLS", 20),
			ContextChars:      envInt("SEEAITV_AGENT_CONTEXT_CHARS", 60000),
			RunnerConcurrency: envInt("SEEAITV_AGENT_RUNNER_CONCURRENCY", 4),
			CardExpire:        envDuration("SEEAITV_AGENT_CARD_EXPIRE", 168*time.Hour),
			MaxOutputTokens:   envInt("SEEAITV_AGENT_MAX_OUTPUT_TOKENS", 8192),
		},
		Marketing: Marketing{
			Enabled:             envBool("SEEAITV_MARKETING_ENABLED", true),
			MaxVariants:         envInt("SEEAITV_MARKETING_MAX_VARIANTS", 4),
			MaxProductImages:    envInt("SEEAITV_MARKETING_MAX_PRODUCT_IMAGES", 10),
			MaxProducts:         envInt("SEEAITV_MARKETING_MAX_PRODUCTS", 500),
			MaxCharacters:       envInt("SEEAITV_MARKETING_MAX_CHARACTERS", 200),
			ReasoningImageWidth: envInt("SEEAITV_MARKETING_REASONING_IMAGE_WIDTH", 640),
		},
		Director: Director{
			PreviewMaxBytes:  int64(envInt("SEEAITV_DIRECTOR_PREVIEW_MAX_BYTES", 50<<20)),
			SplatMaxBytes:    int64(envInt("SEEAITV_DIRECTOR_SPLAT_MAX_BYTES", 100<<20)),
			BuildConcurrency: envInt("SEEAITV_DIRECTOR_BUILD_CONCURRENCY", 1),
			BuildPerHour:     envInt("SEEAITV_DIRECTOR_BUILD_PER_HOUR", 20),
			BuildTimeout:     envDuration("SEEAITV_DIRECTOR_BUILD_TIMEOUT", 3*time.Minute),
			BuildStaleAfter:  envDuration("SEEAITV_DIRECTOR_BUILD_STALE_AFTER", 20*time.Minute),
			BuildImageWidth:  envInt("SEEAITV_DIRECTOR_BUILD_IMAGE_WIDTH", 640),
		},
		Smartcut: Smartcut{
			FFmpegMaxConcurrent:   envInt("SEEAITV_FFMPEG_MAX_CONCURRENT", 2),
			AnalysisScanInterval:  envDuration("SEEAITV_ANALYSIS_SCAN_INTERVAL", 5*time.Second),
			AnalysisLease:         envDuration("SEEAITV_ANALYSIS_LEASE", 10*time.Minute),
			RenderUserConcurrency: envInt("SEEAITV_RENDER_USER_CONCURRENCY", 1),
			RenderUserPerHour:     envInt("SEEAITV_RENDER_USER_PER_HOUR", 20),
			RenderDownloadTTL:     envDuration("SEEAITV_RENDER_DOWNLOAD_TTL", 5*time.Minute),
			PlanConcurrency:       envInt("SEEAITV_SMARTCUT_PLAN_CONCURRENCY", 1),
			PlanPerHour:           envInt("SEEAITV_SMARTCUT_PLAN_PER_HOUR", 20),
			PlanTimeout:           envDuration("SEEAITV_SMARTCUT_PLAN_TIMEOUT", 3*time.Minute),
			PlanVisionTimeout:     envDuration("SEEAITV_SMARTCUT_PLAN_VISION_TIMEOUT", 5*time.Minute),
			PlanStaleAfter:        envDuration("SEEAITV_SMARTCUT_PLAN_STALE_AFTER", 20*time.Minute),
			AIEnabled:             envBool("SEEAITV_SMARTCUT_AI_ENABLED", false),
		},
		MCP: MCP{
			Enabled:         envBool("SEEAITV_MCP_ENABLED", true),
			PublicURL:       env("SEEAITV_MCP_PUBLIC_URL", ""),
			WebPublicURL:    env("SEEAITV_WEB_PUBLIC_URL", ""),
			ResultMaxTokens: envInt("SEEAITV_MCP_RESULT_MAX_TOKENS", 6000),
			RateUserClient:  envInt("SEEAITV_MCP_RATE_USER_CLIENT", 60),
			RateWorkspace:   envInt("SEEAITV_MCP_RATE_WORKSPACE", 300),
			MediaURLTTL:     envDuration("SEEAITV_MCP_MEDIA_URL_TTL", time.Hour),
			QuoteTTL:        envDuration("SEEAITV_MCP_QUOTE_TTL", 10*time.Minute),
			WaitMax:         envDuration("SEEAITV_MCP_WAIT_MAX", 50*time.Second),
		},
		Postgres: Postgres{
			DSN:             env("SEEAITV_PG_DSN", ""),
			MaxConns:        int32(envInt("SEEAITV_PG_MAX_CONNS", 10)),
			MinConns:        int32(envInt("SEEAITV_PG_MIN_CONNS", 2)),
			MaxConnLifetime: envDuration("SEEAITV_PG_MAX_CONN_LIFETIME", time.Hour),
		},
		Redis: Redis{
			Addr:      env("SEEAITV_REDIS_ADDR", ""),
			DB:        envInt("SEEAITV_REDIS_DB", 0),
			Namespace: env("SEEAITV_REDIS_NAMESPACE", "seeaitv"),
		},
		Queue: Queue{
			URL:            env("SEEAITV_QUEUE_URL", ""),
			ExchangePrefix: env("SEEAITV_QUEUE_EXCHANGE_PREFIX", "seeaitv"),
			Prefetch:       envInt("SEEAITV_QUEUE_PREFETCH", 1),
		},
		Storage: Storage{
			Type:      env("SEEAITV_STORAGE_TYPE", "local"),
			Endpoint:  env("SEEAITV_STORAGE_ENDPOINT", ""),
			Region:    env("SEEAITV_STORAGE_REGION", "us-east-1"),
			Bucket:    env("SEEAITV_STORAGE_BUCKET", ""),
			AccessKey: env("SEEAITV_STORAGE_ACCESS_KEY", ""),
			SecretKey: env("SEEAITV_STORAGE_SECRET_KEY", ""),
			SignTTL:   envDuration("SEEAITV_STORAGE_SIGN_TTL", 6*time.Hour),
			PathStyle: envBool("SEEAITV_STORAGE_PATH_STYLE", true),
			Prefix:    env("SEEAITV_STORAGE_PREFIX", ""),
		},
		RelayStorage: Storage{
			Type:      "s3",
			Endpoint:  env("SEEAITV_RELAY_STORAGE_ENDPOINT", ""),
			Region:    env("SEEAITV_RELAY_STORAGE_REGION", "us-east-1"),
			Bucket:    env("SEEAITV_RELAY_STORAGE_BUCKET", ""),
			AccessKey: env("SEEAITV_RELAY_STORAGE_ACCESS_KEY", ""),
			SecretKey: env("SEEAITV_RELAY_STORAGE_SECRET_KEY", ""),
			SignTTL:   envDuration("SEEAITV_STORAGE_SIGN_TTL", 6*time.Hour),
			// 公网中转是自建 MinIO：只认 path-style（零值 false 会悄悄切成 virtual-hosted）。
			PathStyle: true,
		},
		RelayPrefix: env("SEEAITV_RELAY_STORAGE_PREFIX", "relay/"),
		Auth: Auth{
			JWTSecret:        env("SEEAITV_JWT_SECRET", ""),
			SecretKey:        env("SEEAITV_SECRET_KEY", ""),
			SecretKeyVersion: envInt("SEEAITV_SECRET_KEY_VERSION", 1),
			OldSecretKeys:    oldSecretKeys(),
		},
		Dev: Dev{
			FakeProvider:        envBool("SEEAITV_DEV_FAKE_PROVIDER", false),
			FakePayment:         envBool("SEEAITV_DEV_FAKE_PAYMENT", false),
			AllowPrivateNetwork: envBool("SEEAITV_DEV_ALLOW_PRIVATE_NETWORK", false),
		},
	}
	return c, c.Validate()
}

// LoadRaw 装载但**不校验**。
//
// ⚠️ 只给发布前自检用（`migrate preflight`）。`Load` 在配置非法时直接返回错误，
// 于是自检里那些更具体的检查（schema 版本、seed、管理员、存储细节）根本没机会跑 ——
// 运维要先修完配置、再跑一次才看得到后半截，而那正是"修一个再跑一次循环五轮"。
// 自检要的是**一次报完**，所以它自己把 Validate 的错误收进报告，而不是被它挡在门外。
//
// ⚠️ **别在服务启动路径上用它**：启动就该在配置非法时 fail fast。
func LoadRaw() Config {
	c, _ := Load()
	return c
}

// Validate 在启动时 fail fast。宁可起不来，也不要带着危险开关跑在生产上。
func (c Config) Validate() error {
	var errs []error

	switch c.Env {
	case EnvDev, EnvStaging, EnvProduction:
	default:
		errs = append(errs, fmt.Errorf("SEEAITV_ENV 取值非法: %q（应为 dev/staging/production）", c.Env))
	}

	switch c.Log.Format {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("SEEAITV_LOG_FORMAT 取值非法: %q", c.Log.Format))
	}

	switch c.Storage.Type {
	case "s3", "local":
	default:
		errs = append(errs, fmt.Errorf("SEEAITV_STORAGE_TYPE 取值非法: %q（应为 s3/local）", c.Storage.Type))
	}
	// 中转存储配了地址就要配齐：缺一项的话启动时不报错，要等第一次转存才失败，而那时任务已经扣费。
	if r := c.RelayStorage; r.Endpoint != "" && (r.Bucket == "" || r.AccessKey == "" || r.SecretKey == "") {
		errs = append(errs, errors.New("配了 SEEAITV_RELAY_STORAGE_ENDPOINT 就要同时配 _BUCKET / _ACCESS_KEY / _SECRET_KEY"))
	}

	// 开发环境允许留空（启动时生成临时密钥），生产必须显式配置。
	if c.IsProduction() && len(c.Auth.JWTSecret) < 32 {
		errs = append(errs, errors.New("production 下必须配置 SEEAITV_JWT_SECRET 且不短于 32 字节"))
	}
	// 加密密钥必须与签名密钥分开：共用会让一次泄漏同时失守会话与所有上游凭据。
	if c.IsProduction() {
		if len(c.Auth.SecretKey) < 32 {
			errs = append(errs, errors.New("production 下必须配置 SEEAITV_SECRET_KEY 且不短于 32 字节"))
		} else if c.Auth.SecretKey == c.Auth.JWTSecret {
			errs = append(errs, errors.New("SEEAITV_SECRET_KEY 不得与 SEEAITV_JWT_SECRET 相同"))
		}
	}

	// 轮换配置的自洽性。⚠️ 这两条都要在**启动时**拒绝，不能等到第一次解密失败：
	// 那时故障已经发生，而错误信息（"解密失败"）指向的是数据而不是配置。
	cur := c.Auth.CurrentKeyVersion()
	if old, ok := c.Auth.OldSecretKeys[cur]; ok && old != c.Auth.SecretKey {
		errs = append(errs, fmt.Errorf(
			"SEEAITV_SECRET_KEY_V%d 与 SEEAITV_SECRET_KEY 都指向当前版本 %d 却取值不同 —— "+
				"当前版本的材料只能由 SEEAITV_SECRET_KEY 给出", cur, cur))
	}
	for v := range c.Auth.OldSecretKeys {
		if v > cur {
			errs = append(errs, fmt.Errorf(
				"存在版本 %d 的历史密钥，却声明当前版本是 %d —— "+
					"轮换只能向前，版本号退回会让新写的密文用旧版本号而解不开", v, cur))
		}
	}

	if c.IsProduction() {
		// ADR-0001 §8.1：开发开关在 production 必须显式禁止并启动时 fail fast。
		if c.Dev.FakeProvider {
			errs = append(errs, errors.New("production 下禁止开启 SEEAITV_DEV_FAKE_PROVIDER"))
		}
		if c.Dev.FakePayment {
			errs = append(errs, errors.New("production 下禁止开启 SEEAITV_DEV_FAKE_PAYMENT"))
		}
		if c.Dev.AllowPrivateNetwork {
			errs = append(errs, errors.New("production 下禁止开启 SEEAITV_DEV_ALLOW_PRIVATE_NETWORK"))
		}
		// local 存储无法预签名，且 API/Worker 分机部署时互相读不到盘。
		if c.Storage.Type == "local" {
			errs = append(errs, errors.New("production 下禁止使用 local 存储，须配置 s3"))
		}
		for _, o := range c.WSAllowedOrigins {
			if o == "*" {
				errs = append(errs, errors.New(
					"production 下 SEEAITV_WS_ALLOWED_ORIGINS 不得为 *：要么留空（同源），要么逐个列出页面所在的域名"))
			}
		}
	}

	// ⚠️ 0 或负数在这几项上都没有「不限」的含义：调用上限 0 = 每轮一次模型都调不了、
	// 字符预算 0 = 一条历史都不带、并发 0 = 消费者 prefetch 非法。启动时拒，不等第一轮才暴露。
	if c.Agent.Enabled {
		if c.Agent.TurnMaxCalls <= 0 {
			errs = append(errs, fmt.Errorf("SEEAITV_AGENT_TURN_MAX_CALLS 必须为正数（当前 %d）", c.Agent.TurnMaxCalls))
		}
		if c.Agent.ContextChars <= 0 {
			errs = append(errs, fmt.Errorf("SEEAITV_AGENT_CONTEXT_CHARS 必须为正数（当前 %d）", c.Agent.ContextChars))
		}
		if c.Agent.RunnerConcurrency <= 0 {
			errs = append(errs, fmt.Errorf("SEEAITV_AGENT_RUNNER_CONCURRENCY 必须为正数（当前 %d）", c.Agent.RunnerConcurrency))
		}
		if c.Agent.MaxOutputTokens < 0 {
			errs = append(errs, fmt.Errorf("SEEAITV_AGENT_MAX_OUTPUT_TOKENS 不能为负（当前 %d，0 = 用协议缺省）",
				c.Agent.MaxOutputTokens))
		}
	}
	// 营销：上限 0 = 一张商品图都传不了 / 一个变体都出不来，没有「不限」的含义；
	// 缩略图档位写成媒体域没有的值的话，第一次给推理模型看图时才失败。
	if c.Marketing.Enabled {
		if c.Marketing.MaxVariants <= 0 {
			errs = append(errs, fmt.Errorf("SEEAITV_MARKETING_MAX_VARIANTS 必须为正数（当前 %d）", c.Marketing.MaxVariants))
		}
		if c.Marketing.MaxProductImages <= 0 {
			errs = append(errs, fmt.Errorf("SEEAITV_MARKETING_MAX_PRODUCT_IMAGES 必须为正数（当前 %d）", c.Marketing.MaxProductImages))
		}
		// 配额 0 等于禁止新建，同样没有「不限」的含义。
		if c.Marketing.MaxProducts <= 0 {
			errs = append(errs, fmt.Errorf("SEEAITV_MARKETING_MAX_PRODUCTS 必须为正数（当前 %d）", c.Marketing.MaxProducts))
		}
		if c.Marketing.MaxCharacters <= 0 {
			errs = append(errs, fmt.Errorf("SEEAITV_MARKETING_MAX_CHARACTERS 必须为正数（当前 %d）", c.Marketing.MaxCharacters))
		}
		switch c.Marketing.ReasoningImageWidth {
		case 160, 320, 640:
		default:
			errs = append(errs, fmt.Errorf("SEEAITV_MARKETING_REASONING_IMAGE_WIDTH 只接受 160 / 320 / 640（当前 %d）",
				c.Marketing.ReasoningImageWidth))
		}
	}
	// 导演台：上限 0 或负数等于这类文件一个都传不上来，没有「不限」的含义。
	if c.Smartcut.FFmpegMaxConcurrent < 1 {
		errs = append(errs, fmt.Errorf("SEEAITV_FFMPEG_MAX_CONCURRENT 至少为 1（当前 %d）", c.Smartcut.FFmpegMaxConcurrent))
	}
	if c.Smartcut.AnalysisScanInterval <= 0 {
		errs = append(errs, fmt.Errorf("SEEAITV_ANALYSIS_SCAN_INTERVAL 必须为正（当前 %s）", c.Smartcut.AnalysisScanInterval))
	}
	// 租约太短的话，一次正常的续期抖动（库慢了几秒）就会让别的实例把还活着的任务接走、重转一遍。
	if c.Smartcut.AnalysisLease < time.Minute {
		errs = append(errs, fmt.Errorf("SEEAITV_ANALYSIS_LEASE 至少 1 分钟（当前 %s）", c.Smartcut.AnalysisLease))
	}
	// 下载链接要在点击的那一刻能用、又不能长期有效（链接本身就是凭据）：(0, 1h]。
	if d := c.Smartcut.RenderDownloadTTL; d <= 0 || d > time.Hour {
		errs = append(errs, fmt.Errorf("SEEAITV_RENDER_DOWNLOAD_TTL 应在 (0, 1h] 之间（当前 %s）", d))
	}
	if c.Director.PreviewMaxBytes <= 0 {
		errs = append(errs, fmt.Errorf("SEEAITV_DIRECTOR_PREVIEW_MAX_BYTES 必须为正数（当前 %d）", c.Director.PreviewMaxBytes))
	}
	if c.Director.SplatMaxBytes <= 0 {
		errs = append(errs, fmt.Errorf("SEEAITV_DIRECTOR_SPLAT_MAX_BYTES 必须为正数（当前 %d）", c.Director.SplatMaxBytes))
	}
	// AI 搭景：并发 / 次数 0 等于永远不能搭景，没有「不限」的含义。
	if c.Director.BuildConcurrency <= 0 || c.Director.BuildPerHour <= 0 {
		errs = append(errs, fmt.Errorf("SEEAITV_DIRECTOR_BUILD_CONCURRENCY / _PER_HOUR 必须为正数（当前 %d / %d）",
			c.Director.BuildConcurrency, c.Director.BuildPerHour))
	}
	if c.Director.BuildTimeout <= 0 || c.Director.BuildTimeout > DirectorLongRouteLimit {
		errs = append(errs, fmt.Errorf("SEEAITV_DIRECTOR_BUILD_TIMEOUT 必须在 (0, %s] 内（当前 %s）",
			DirectorLongRouteLimit, c.Director.BuildTimeout))
	}
	if c.Director.BuildStaleAfter <= DirectorLongRouteLimit {
		errs = append(errs, fmt.Errorf("SEEAITV_DIRECTOR_BUILD_STALE_AFTER 必须大于长耗时路由的上限 %s（当前 %s）："+
			"否则还在等模型的搭景会被判失败", DirectorLongRouteLimit, c.Director.BuildStaleAfter))
	}
	switch c.Director.BuildImageWidth {
	case 160, 320, 640:
	default:
		errs = append(errs, fmt.Errorf("SEEAITV_DIRECTOR_BUILD_IMAGE_WIDTH 只接受 160 / 320 / 640（当前 %d）",
			c.Director.BuildImageWidth))
	}
	// AI 剪辑（SCT-27）：同导演台的 AI 搭景 —— 并发 / 次数 0 等于永远不能剪，调用上限要在长路由之内，
	// 判死阈值要大于长路由（否则还在等模型的请求会被判失败，随后又写回成功）。
	if c.Smartcut.PlanConcurrency <= 0 || c.Smartcut.PlanPerHour <= 0 {
		errs = append(errs, fmt.Errorf("SEEAITV_SMARTCUT_PLAN_CONCURRENCY / _PER_HOUR 必须为正数（当前 %d / %d）",
			c.Smartcut.PlanConcurrency, c.Smartcut.PlanPerHour))
	}
	for _, d := range []struct {
		name string
		val  time.Duration
	}{{"SEEAITV_SMARTCUT_PLAN_TIMEOUT", c.Smartcut.PlanTimeout}, {"SEEAITV_SMARTCUT_PLAN_VISION_TIMEOUT", c.Smartcut.PlanVisionTimeout}} {
		// 回灌修复要再调一次：两次加起来仍要在长路由的上限之内。
		if d.val <= 0 || 2*d.val > DirectorLongRouteLimit {
			errs = append(errs, fmt.Errorf("%s 必须在 (0, %s] 内（当前 %s；不合规时要再调一次修复，两次合计不能超过长路由上限 %s）",
				d.name, DirectorLongRouteLimit/2, d.val, DirectorLongRouteLimit))
		}
	}
	if c.Smartcut.PlanStaleAfter <= DirectorLongRouteLimit {
		errs = append(errs, fmt.Errorf("SEEAITV_SMARTCUT_PLAN_STALE_AFTER 必须大于长耗时路由的上限 %s（当前 %s）："+
			"否则还在等模型的 AI 剪辑会被判失败", DirectorLongRouteLimit, c.Smartcut.PlanStaleAfter))
	}
	// MCP：截断阈值 0 会让每个结果都只剩空数组；公网地址写错会让 Origin 同源判断永远不成立。
	if c.MCP.Enabled {
		// 负数没有含义；写错成负数时静默当成「不限」的话，运维以为限流开着而其实没有。
		if c.MCP.RateUserClient < 0 || c.MCP.RateWorkspace < 0 {
			errs = append(errs, fmt.Errorf("SEEAITV_MCP_RATE_USER_CLIENT / _WORKSPACE 不能为负（当前 %d / %d，0 = 不限）",
				c.MCP.RateUserClient, c.MCP.RateWorkspace))
		}
		if c.MCP.ResultMaxTokens <= 0 {
			errs = append(errs, fmt.Errorf("SEEAITV_MCP_RESULT_MAX_TOKENS 必须为正数（当前 %d）", c.MCP.ResultMaxTokens))
		}
		if c.MCP.MediaURLTTL <= 0 || c.MCP.MediaURLTTL > 24*time.Hour {
			errs = append(errs, fmt.Errorf("SEEAITV_MCP_MEDIA_URL_TTL 必须在 (0, 24h] 内（当前 %s）", c.MCP.MediaURLTTL))
		}
		if c.MCP.QuoteTTL <= 0 || c.MCP.QuoteTTL > 24*time.Hour {
			errs = append(errs, fmt.Errorf("SEEAITV_MCP_QUOTE_TTL 必须在 (0, 24h] 内（当前 %s）", c.MCP.QuoteTTL))
		}
		if c.MCP.WaitMax <= 0 || c.MCP.WaitMax > 55*time.Second {
			errs = append(errs, fmt.Errorf("SEEAITV_MCP_WAIT_MAX 必须在 (0, 55s] 内（当前 %s；单次调用截止 60 秒）", c.MCP.WaitMax))
		}
		for _, v := range []struct{ name, val string }{
			{"SEEAITV_MCP_PUBLIC_URL", c.MCP.PublicURL}, {"SEEAITV_WEB_PUBLIC_URL", c.MCP.WebPublicURL},
		} {
			switch {
			case c.IsProduction() && !strings.HasPrefix(v.val, "https://"):
				// OAuth 的 issuer / resource / 同意页地址都由它们给出（MCPT-12）：production 下按请求推导的话
				// resource 由请求头决定，http 的话授权码与令牌在网络上明文传。
				errs = append(errs, fmt.Errorf("production 下 %s 必须配置为 https 地址（当前 %q）", v.name, v.val))
			case v.val != "" && !strings.HasPrefix(v.val, "https://") && !strings.HasPrefix(v.val, "http://"):
				errs = append(errs, fmt.Errorf("%s 必须是 http(s) 地址（当前 %q）", v.name, v.val))
			}
		}
	}

	return errors.Join(errs...)
}

func (c Config) IsProduction() bool { return c.Env == EnvProduction }

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(env(key, "")); err == nil {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, err := strconv.ParseBool(env(key, "")); err == nil {
		return v
	}
	return def
}

// envList 读逗号分隔的列表；去空白、丢空项。没配时为 nil。
func envList(key string) []string {
	var out []string
	for _, part := range strings.Split(env(key, ""), ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envDuration(key string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(env(key, "")); err == nil {
		return v
	}
	return def
}

// oldSecretKeys 扫 `SEEAITV_SECRET_KEY_V<n>`，收集**只用于解密**的历史密钥。
//
// 轮换的做法（运维手册里也是这么写的）：
//  1. 把当前值原样抄到 `SEEAITV_SECRET_KEY_V1`；
//  2. `SEEAITV_SECRET_KEY` 换成新材料，`SEEAITV_SECRET_KEY_VERSION=2`；
//  3. 重启 —— 此时新写入是 `v2:`，旧的 `v1:` 仍解得开，**不停机**；
//  4. 跑 `migrate rotate-secrets` 把存量密文重写成 v2；
//  5. 确认没有 v1 残留后，再把 `SEEAITV_SECRET_KEY_V1` 摘掉。
//
// ⚠️ 第 5 步**必须等第 4 步确认之后**：提前摘掉旧 key，剩下的 v1 密文就永远解不开了。
// `rotate-secrets` 因此会在结束时报还有多少行不是当前版本。
func oldSecretKeys() map[int]string {
	var out map[int]string
	for _, kv := range os.Environ() {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			continue
		}
		name, val := kv[:i], kv[i+1:]
		rest, ok := strings.CutPrefix(name, "SEEAITV_SECRET_KEY_V")
		if !ok || val == "" {
			continue
		}
		ver, err := strconv.Atoi(rest)
		if err != nil || ver <= 0 {
			// `SEEAITV_SECRET_KEY_VERSION` 也以这个前缀开头（"ERSION"），
			// Atoi 失败正好把它挡在外面。
			continue
		}
		if out == nil {
			out = map[int]string{}
		}
		out[ver] = val
	}
	return out
}
