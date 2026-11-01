package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/logredact"
)

// 调用审计（MCPT-06，MCP-F-70、MCP-N-03/06，开发 01 §9）。
//
// 每次 tools/call 一条 mcp_call_log，**含被拒与被限流的**。写入方只有 transport 的两个统一拦截点
// （SDK 接收中间件与 HTTP 限流中间件），**工具自己不记** —— 每个工具各记一行的话，漏掉的那一个
// 不会有任何编译错误，而它恰好是没人知道被调用过的那一个。
//
// 三条硬约束（同 api_request_log，CLAUDE.md §5.22）：
//   - **异步**：带缓冲 channel + 单写协程；缓冲满时**丢弃并计数告警**，绝不阻塞调用；
//   - **独立 context**：每条用 context.WithTimeout(Background, 5s)。用请求 ctx 的话响应一返回就被取消，
//     每一条都 context canceled，表现只是「表一直是空的」；
//   - **失败只 Warn**：审计写不进去不能让工具调用失败（MCP-N-06）。

// CallLogStore 是调用审计的落库端口（mcp/repo.Repo 实现）。
type CallLogStore interface {
	InsertCallLog(ctx context.Context, c repo.CallLog) error
}

// CallEntry 是一次调用的事实；指纹与脱敏摘要在写协程里算（不占调用路径的时间）。
type CallEntry struct {
	GrantID     int64
	UserID      int64
	WorkspaceID int64
	ClientName  string
	Tool        string
	Method      string
	// Args 是客户端发来的原始参数。⚠️ 只用来算指纹与脱敏摘要，**原文不落库**（MCP-N-03）。
	// Record 之后调用方不得再修改它。
	Args      json.RawMessage
	Credits   int
	Result    string // domain.Call*
	ErrorCode int
	Duration  time.Duration
	TraceID   string
	At        time.Time
}

// EntryFor 用 Principal 填好主体字段。
func EntryFor(p Principal) CallEntry {
	return CallEntry{GrantID: p.GrantID, UserID: p.UserID, WorkspaceID: p.WorkspaceID, ClientName: p.ClientName}
}

// DefaultCallLogBuffer 是缓冲条数：一个写协程每条几毫秒，缓冲吸收的是突发与短暂的库抖动。
const DefaultCallLogBuffer = 2048

const callLogWriteTimeout = 5 * time.Second

// 列宽（与 00050 的 DDL 一致）：超出的截断，不能让一个超长的工具名把整条审计写失败。
const (
	maxToolLen    = 48
	maxMethodLen  = 32
	maxClientLen  = 100
	maxTraceIDLen = 64
)

// CallLogger 是调用审计的异步写入器。
type CallLogger struct {
	store CallLogStore
	next  func() int64
	log   *slog.Logger

	mu     sync.RWMutex // 保护 closed 与 ch 的关闭（Record 与 Close 并发时不往已关闭的 channel 发）
	closed bool
	ch     chan CallEntry
	done   chan struct{}
	start  sync.Once

	dropped  atomic.Int64
	warnMu   sync.Mutex
	lastWarn time.Time
}

// NewCallLogger 构造写入器。buffer <= 0 取 DefaultCallLogBuffer。调用 Run 才开始写。
func NewCallLogger(store CallLogStore, next func() int64, log *slog.Logger, buffer int) *CallLogger {
	if log == nil {
		log = slog.Default()
	}
	if buffer <= 0 {
		buffer = DefaultCallLogBuffer
	}
	return &CallLogger{store: store, next: next, log: log, ch: make(chan CallEntry, buffer), done: make(chan struct{})}
}

// Record 投递一条审计，**从不阻塞**：缓冲满或已关闭时丢弃（并计数告警）。
func (l *CallLogger) Record(e CallEntry) {
	if l == nil {
		return
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		return
	}
	select {
	case l.ch <- e:
	default:
		l.drop(e)
	}
}

// Dropped 是累计因缓冲满被丢弃的条数。
func (l *CallLogger) Dropped() int64 { return l.dropped.Load() }

// drop 计数并告警（每分钟至多一条：缓冲满通常意味着库在抖，每条都打日志只会雪上加霜）。
func (l *CallLogger) drop(e CallEntry) {
	n := l.dropped.Add(1)
	l.warnMu.Lock()
	defer l.warnMu.Unlock()
	if now := time.Now(); l.lastWarn.IsZero() || now.Sub(l.lastWarn) >= time.Minute {
		l.lastWarn = now
		l.log.Warn("MCP 调用审计缓冲已满，丢弃（审计不阻塞调用）", "droppedTotal", n,
			"tool", e.Tool, "grantId", e.GrantID, "traceId", e.TraceID)
	}
}

// Run 是单写协程：逐条落库，直到 Close（排空缓冲后返回）或 ctx 结束（尽力排空已缓冲的再返回）。
// 只能跑一个；重复调用直接返回。
func (l *CallLogger) Run(ctx context.Context) {
	started := false
	l.start.Do(func() { started = true })
	if !started {
		return
	}
	defer close(l.done)
	for {
		select {
		case e, ok := <-l.ch:
			if !ok {
				return
			}
			l.write(e)
		case <-ctx.Done():
			for {
				select {
				case e, ok := <-l.ch:
					if !ok {
						return
					}
					l.write(e)
				default:
					return
				}
			}
		}
	}
}

// Close 停止接收，等缓冲排空后返回（Run 在跑就等它，没跑就自己写完）。
func (l *CallLogger) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		close(l.ch)
	}
	l.mu.Unlock()
	// 从没 Run 过（或 Run 的协程还没被调度到）：占住 once 并就地排空 —— 之后的 Run 直接返回。
	// 不排空的话「go Run 之后立刻 Close」会把已缓冲的审计全丢掉。
	ranHere := false
	l.start.Do(func() {
		ranHere = true
		for e := range l.ch {
			l.write(e)
		}
		close(l.done)
	})
	if !ranHere {
		<-l.done
	}
}

// write 落一条。每条**独立 context**、失败只 Warn。
func (l *CallLogger) write(e CallEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), callLogWriteTimeout)
	defer cancel()
	row := toRow(e)
	row.ID = l.next()
	if err := l.store.InsertCallLog(ctx, row); err != nil {
		l.log.Warn("写 MCP 调用审计失败", "error", err, "tool", row.Tool, "grantId", row.GrantID,
			"result", row.Result, "traceId", row.TraceID)
	}
}

// toRow 算指纹与脱敏摘要，并把字符串截到列宽。
func toRow(e CallEntry) repo.CallLog {
	r := repo.CallLog{
		GrantID: e.GrantID, UserID: e.UserID, WorkspaceID: e.WorkspaceID,
		ClientName: clip(e.ClientName, maxClientLen), Tool: clip(e.Tool, maxToolLen),
		Method:  clip(e.Method, maxMethodLen),
		Credits: max(e.Credits, 0), Result: e.Result, ErrorCode: e.ErrorCode,
		DurationMS: int(max(e.Duration, 0) / time.Millisecond), TraceID: clip(e.TraceID, maxTraceIDLen),
		CreatedAt: e.At,
	}
	if r.Result == "" {
		r.Result = domain.CallError
	}
	if r.Method == "" {
		r.Method = clip(methodOf(e.Args), maxMethodLen)
	}
	r.ArgsFingerprint = domain.Fingerprint(e.Tool, e.Args)
	if len(e.Args) > 0 {
		// 字段名命中 token/authorization… 的按字段打码，自由文本里的 sk_mcp_…/sk-…/Bearer 由正则打码，
		// base64 换成摘要，最后截到 logredact.MaxBodyChars。
		r.ArgsSummary = logredact.RedactJSON(e.Args)
	}
	return r
}

// methodOf 取多 method 工具参数里的 method（注册表给多 method 工具注入的必填枚举）；
// 取不到（单 method 工具、参数不是对象）返回空，落库为 null。在写协程里解析，不占调用路径。
func methodOf(args json.RawMessage) string {
	var v struct {
		Method any `json:"method"`
	}
	if len(args) == 0 || json.Unmarshal(args, &v) != nil {
		return ""
	}
	s, _ := v.Method.(string)
	return s
}

// clip 按码点截断（varchar(n) 的 n 是字符数），并清掉 PG 文本列拒收的字节。
func clip(s string, n int) string {
	s = logredact.SafeText(s)
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// ---------------------------------------------------------------- 结果回报

// CallOutcome 是一次 tools/call 执行过程中**由下游回报**的事实：审计拦截点在调用前放进 ctx，
// 工具（花了多少积分、是否停在「需确认」）与错误包装（业务码）往里写，调用结束后拦截点读出来分类。
//
// 只是一个回报槽，不参与任何判定；ctx 里没有它（例如测试直接调工具）时 Report* 什么都不做。
type CallOutcome struct {
	mu           sync.Mutex
	credits      int
	needsConfirm bool
	errorCode    int
	// resourceKind 是 resources/read 读的资源类别（MCPT-16：审计的 method 列）。
	resourceKind string
}

type outcomeKey struct{}

// WithCallOutcome 在 ctx 上放一个新的回报槽。
func WithCallOutcome(ctx context.Context) (context.Context, *CallOutcome) {
	o := &CallOutcome{}
	return context.WithValue(ctx, outcomeKey{}, o), o
}

func outcomeFrom(ctx context.Context) *CallOutcome {
	o, _ := ctx.Value(outcomeKey{}).(*CallOutcome)
	return o
}

// ReportCredits 回报本次调用实际花费的积分（受理成功后由花钱工具调用，MCPT-10 起；M1 恒为 0）。
func ReportCredits(ctx context.Context, credits int) {
	if o := outcomeFrom(ctx); o != nil && credits > 0 {
		o.mu.Lock()
		o.credits += credits
		o.mu.Unlock()
	}
}

// ReportNeedsConfirm 回报本次调用停在「需要用户确认」、没有受理（MCPT-10 起）。
func ReportNeedsConfirm(ctx context.Context) {
	if o := outcomeFrom(ctx); o != nil {
		o.mu.Lock()
		o.needsConfirm = true
		o.mu.Unlock()
	}
}

// ReportErrorCode 回报本次调用失败的业务码（transport 包装 isError 结果时调用）。
func ReportErrorCode(ctx context.Context, code int) {
	if o := outcomeFrom(ctx); o != nil {
		o.mu.Lock()
		o.errorCode = code
		o.mu.Unlock()
	}
}

// ReportResourceKind 回报本次 resources/read 读的资源类别（ResourceTemplate.Name）。
func ReportResourceKind(ctx context.Context, kind string) {
	if o := outcomeFrom(ctx); o != nil {
		o.mu.Lock()
		o.resourceKind = kind
		o.mu.Unlock()
	}
}

// ResourceKind 读出回报的资源类别（没有时空串）。
func (o *CallOutcome) ResourceKind() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.resourceKind
}

// Snapshot 读出回报的内容。
func (o *CallOutcome) Snapshot() (credits int, needsConfirm bool, errorCode int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.credits, o.needsConfirm, o.errorCode
}
