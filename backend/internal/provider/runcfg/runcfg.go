// Package runcfg 是 Provider 运行配置的**中立定义**。
//
// ⚠️ 单独成包是为了消除一处重复：`cmd/api` 与 `cmd/worker` 都要把
// 「目录域的连接配置」接给「Provider 适配器」。类型定义在任一侧的话，
// 两个 main 各写一份转换适配器 —— 而两份适配器早晚会不一致，
// 且不一致时不会有编译错误，只会有一个进程行为不对。
//
// 放在这里之后，目录域**直接实现** Resolver、请求日志仓储**直接实现**
// RequestLogger，两个 main 里一行适配器都不用写。
// 这个包只依赖标准库，谁都能 import 而不会牵进别的域。
package runcfg

import (
	"context"
	"time"
)

// RunConfig 是跑一次上游调用所需的连接配置。
//
// ⚠️ 这里存的是**现在怎么连上游**（地址、密钥、超时），与任务快照里
// 冻结的**怎么解释这次任务**（协议、型号、价格）是两回事：
// 密钥会轮换、地址会搬迁，把它们冻进快照的话，轮换一次全部历史任务都跑不动，
// 而且快照会变成一个存明文密钥的地方。
type RunConfig struct {
	ModelCode     string
	UpstreamModel string
	ProtocolCode  string
	ProviderCode  string
	BaseURL       string
	SubmitPath    string
	QueryPath     string
	AuthType      string
	// APIKey 是**解密后的**密钥。只在进程内流转，绝不下发、绝不进日志。
	APIKey     string
	HeaderName string
	Headers    map[string]string
	// SignRegion / SignService 是火山 V4 签名（auth_type=volc_v4）的地域与服务。它们不是密钥，
	// 配在 Provider 的自定义请求头里（X-Volc-Region / X-Volc-Service），取出来之后不再随请求发出。
	SignRegion  string
	SignService string

	ConnectTimeout time.Duration
	RequestTimeout time.Duration
	IdleTimeout    time.Duration
	MaxRetries     int
}

// 火山 V4 签名（docs/agent/smartcut SCD-38）的认证方式取值与两个约定键。
//
// ⚠️ 放在这个叶子包里：目录域（解析配置、后台保存校验）与传输层（签名）都要用同一组名字，
// 各写一份字面量的话改一边不改另一边，症状是后台存得进去、签名时却说「缺少地域」。
const (
	AuthVolcV4        = "volc_v4"
	VolcRegionHeader  = "X-Volc-Region"
	VolcServiceHeader = "X-Volc-Service"
)

// Resolver 由目录域实现：按模型码取连接配置。
type Resolver interface {
	ResolveForRun(ctx context.Context, modelCode string) (RunConfig, error)
}

// LogEntry 是一条待落库的 Provider 请求日志（已脱敏、已截断）。
type LogEntry struct {
	TraceID       string
	GenerationID  int64
	ProviderCode  string
	ModelCode     string
	OperationCode string
	URL           string
	HTTPStatus    int
	ErrorType     string
	Duration      time.Duration
	RequestBody   string
	ResponseBody  string
}

// RequestLogger 落 Provider 请求日志。
//
// ⚠️ 写日志失败**绝不能影响主流程**：一次日志写不进去就让生成失败，
// 等于把观测设施变成单点故障。
type RequestLogger interface {
	Write(ctx context.Context, e LogEntry) error
}

// LogFilter 限定 Provider 请求日志检索。
//
// ⚠️ 读类型也放这个叶子包，理由与 `LogEntry` 一样：**写在 Provider 域、
// 读在可观测域**，类型定义在任一侧的话两边各写一份转换适配器 ——
// 而两份早晚会不一致**且没有编译错误**（不一致的形态是某个字段在日志页上
// 永远是空的，而库里明明有值）。
type LogFilter struct {
	TraceID      string
	GenerationID int64
	ProviderCode string
	ModelCode    string
	// FailedOnly 只看 error_type <> 'none'。
	//
	// ⚠️ **不能只看 http_status**：上游的业务错误常藏在 HTTP 200 里
	// （可灵在 `code`、方舟在 `error.message`），而超时/断连根本没有状态码。
	// `error_type` 才是"这次调用成没成"的唯一依据。
	FailedOnly bool
	From, To   time.Time
	Limit      int
	CursorID   int64
}

// LogRecord 是一条落库后的 Provider 请求日志。
type LogRecord struct {
	ID int64
	LogEntry
	CreatedAt time.Time
}
