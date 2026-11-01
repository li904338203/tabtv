package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"

	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	"github.com/li904338203/tabtv/backend/internal/platform/ffsem"
	"github.com/li904338203/tabtv/backend/internal/provider/protocol"
	"github.com/li904338203/tabtv/backend/internal/provider/runcfg"
	"github.com/li904338203/tabtv/backend/internal/provider/transport"
)

// HTTPProvider 是接了真实上游的适配器。
//
// ⚠️ 它实现 `generation/app.Provider`，与开发假适配器（DevProvider）可互换：
// 执行器不知道自己调的是哪一个，因此故障注入与闭环测试用假的、生产用真的，
// **两条路走的是同一套状态机**。
type HTTPProvider struct {
	client   *transport.Client
	resolver Resolver
	media    MediaResolver
	relay    MediaRelay
	logs     RequestLogger
	log      *slog.Logger
	policy   transport.URLPolicy

	// 输入区间截取（cut.go，SCD-34）：截出来的片段放哪、ffmpeg 与谁共用 CPU 名额。
	scratch, relayScratch ScratchStore
	ffSem                 *ffsem.Sem

	// MaxInlineBytes 参考媒体内联 base64 的上限，0 取默认。
	MaxInlineBytes int64
	// MaxDownloadBytes 单个产物下载的上限，0 取默认。
	MaxDownloadBytes int64
}

func NewHTTPProvider(client *transport.Client, r Resolver, policy transport.URLPolicy,
	log *slog.Logger) *HTTPProvider {
	return &HTTPProvider{client: client, resolver: r, policy: policy, log: log}
}

func (p *HTTPProvider) SetMediaResolver(m MediaResolver) { p.media = m }
func (p *HTTPProvider) SetMediaRelay(r MediaRelay)       { p.relay = r }
func (p *HTTPProvider) SetRequestLogger(l RequestLogger) { p.logs = l }

// CanQueryByIdempotencyKey 上游是否支持按我们的幂等键反查。
//
// ⚠️ 目前一律 false：已纳入的协议都没有"按调用方的键查任务"的接口。
// 这决定了提交不确定窗口的处理方式 —— **绝不盲目重发**，
// 重发可能在上游创建第二个任务、让用户被上游计费两次而我们只认得一个。
func (p *HTTPProvider) CanQueryByIdempotencyKey() bool { return false }

// Submit 提交一次任务。
func (p *HTTPProvider) Submit(ctx context.Context, job genapp.Job) (out genapp.SubmitResult, err error) {
	// 截取的片段（输入区间）用完即删：只有异步任务被受理时留到查询终态（上游稍后才来取）。
	defer func() {
		if !keepScratch(out) {
			p.dropScratch(ctx, job)
		}
	}()
	cfg, proto, res, ok := p.prepare(ctx, job)
	if !ok {
		return res, nil
	}

	pjob, err := p.buildJob(ctx, job, cfg, protocol.RequiresMediaURL(proto))
	if err != nil {
		// 输入取不到（媒体丢了、外链签不出来）：这是我们这边的问题，
		// 重发多少次都一样，直接判失败让用户重新发起。
		return rejected(err.Error()), nil
	}

	// 情绪调节（docs/agent/imagetools R-5）：只把框选人物所在的那一块裁出来发给上游，结果再贴回原图。
	if err := p.cropToolInput(ctx, job, &pjob); err != nil {
		return rejected("情绪调节裁切源图失败: " + err.Error()), nil
	}

	req, err := proto.BuildSubmit(pjob)
	if err != nil {
		// 协议说这个组合它表达不了（比如中转接口没有首尾帧的位置）。
		// ⚠️ 走到这里说明能力矩阵没配全 —— 正常情况下受理侧就该拒掉，
		// 那时还没扣费。这里是最后一道兜底，仍然判永久失败并退款。
		return rejected("构造上游请求失败: " + err.Error()), nil
	}
	path, err := protocol.ResolveSubmitPath(proto, pjob, submitPathOf(cfg, req))
	if err != nil {
		return rejected(err.Error()), nil
	}

	resp, sendErr := p.send(ctx, cfg, job, req, path)
	if sendErr != nil {
		var ce *configError
		if errors.As(sendErr, &ce) {
			// 请求根本没发出去（签不出名）：不是「不确定」，是我们这边的配置问题，判永久失败并退款。
			return rejected(ce.Error()), nil
		}
		return p.classifySendError(resp, sendErr), nil
	}

	result, err := protocol.ParseSubmitResponse(proto, resp.StatusCode, resp.Header, resp.Body)
	if err != nil {
		var se *protocol.StatusError
		if errors.As(err, &se) {
			// 协议说清了按哪个状态码分类（业务错误藏在 HTTP 200 里时，例如火山识别的响应头）。
			return genapp.SubmitResult{
				Outcome: gendomain.SubmitRejected, HTTPStatus: se.Status,
				Message: se.Msg, Raw: rawOf(resp.Body),
			}, nil
		}
		if resp.StatusCode >= 400 {
			return genapp.SubmitResult{
				Outcome: gendomain.SubmitRejected, HTTPStatus: resp.StatusCode,
				Message: err.Error(), Raw: rawOf(resp.Body),
			}, nil
		}
		// 2xx 但解析不出来：多半是我们的产物路径没接上，不是上游的错。
		return rejected("解析上游响应失败: " + err.Error()), nil
	}

	if result.Async {
		return genapp.SubmitResult{
			Outcome: gendomain.SubmitAccepted, ProviderTaskID: result.TaskID,
			Raw: rawOf(resp.Body),
		}, nil
	}

	outs, err := p.materialize(ctx, cfg, job.ResultType, result.Outputs)
	if err == nil {
		outs, err = p.finishTool(ctx, job, outs)
	}
	if err != nil {
		// 产物取不回来：任务在上游是成功的，但我们没拿到东西。
		// 判失败并退款 —— 让用户为一个看不见的产物付费更糟。
		return rejected(err.Error()), nil
	}
	if len(outs) == 0 {
		msg := result.Message
		if msg == "" {
			msg = "上游返回成功但未解析出产物"
		}
		return rejected(msg), nil
	}
	return genapp.SubmitResult{
		Outcome: gendomain.SubmitCompleted, Outputs: outs, Raw: rawOf(resp.Body),
	}, nil
}

// Query 查询异步任务。
func (p *HTTPProvider) Query(ctx context.Context, job genapp.Job, taskID string) (
	genapp.QueryResult, error) {
	cfg, proto, _, ok := p.prepare(ctx, job)
	if !ok {
		return genapp.QueryResult{Done: true, Success: false,
			Message: "模型配置或协议不可用，无法查询任务"}, nil
	}
	pjob, err := p.buildJob(ctx, job, cfg, false)
	if err != nil {
		return genapp.QueryResult{}, err
	}
	req, has := proto.BuildQuery(pjob, taskID)
	if !has {
		return genapp.QueryResult{Done: true, Success: false,
			Message: "同步协议不应进入轮询"}, nil
	}
	path := cfg.QueryPath
	if path == "" {
		path = req.Path
	}
	// ⚠️ 占位符替换必须做 URL 转义：任务 ID 是上游给的，可能含 / 或 ?，
	// 直接拼进路径会把查询地址拼成另一个端点。
	path = strings.ReplaceAll(path, "{taskId}", url.PathEscape(taskID))
	path = strings.ReplaceAll(path, "{task_id}", url.PathEscape(taskID))

	resp, err := p.send(ctx, cfg, job, req, path)
	if err != nil {
		// ⚠️ 查询失败**不判任务失败**：上游可能只是这一次没回，
		// 任务还在那边跑着。返回 error 让执行器下一轮再查，由死线兜底。
		return genapp.QueryResult{}, err
	}

	result, err := protocol.ParseQueryResponse(proto, resp.StatusCode, resp.Header, resp.Body)
	if err != nil {
		return genapp.QueryResult{}, err
	}
	if result.State == protocol.StateSuccess || result.State == protocol.StateFailed {
		// 到终态了，上游不会再来取截取的片段。
		defer p.dropScratch(ctx, job)
	}
	switch result.State {
	case protocol.StateSuccess:
		outs, err := p.materialize(ctx, cfg, job.ResultType, result.Outputs)
		if err == nil {
			// 工具后处理（情绪调节贴回原图，docs/agent/imagetools）：失败按任务失败、退款。
			outs, err = p.finishTool(ctx, job, outs)
		}
		if err != nil {
			return genapp.QueryResult{Done: true, Success: false,
				Message: err.Error(), Raw: rawOf(resp.Body)}, nil
		}
		return genapp.QueryResult{Done: true, Success: true, Progress: 100,
			Outputs: outs, Raw: rawOf(resp.Body)}, nil
	case protocol.StateFailed:
		msg := result.Message
		if msg == "" {
			msg = "上游报告任务失败"
		}
		return genapp.QueryResult{Done: true, Success: false, Message: msg,
			Raw: rawOf(resp.Body)}, nil
	}
	return genapp.QueryResult{Done: false, Raw: rawOf(resp.Body)}, nil
}

// prepare 取配置与协议。第四个返回值为 false 时，第三个是已经组装好的失败结果。
func (p *HTTPProvider) prepare(ctx context.Context, job genapp.Job) (
	RunConfig, protocol.Protocol, genapp.SubmitResult, bool) {
	cfg, err := p.resolver.ResolveForRun(ctx, job.ModelCode)
	if err != nil {
		return RunConfig{}, nil, rejected("取模型配置失败: " + err.Error()), false
	}
	// 协议以**任务快照**为准：后台此后改模型绑定都不得改变这次任务的解释。
	code := parseSnapshot(job.Params).APIProtocol
	if code == "" {
		code = cfg.ProtocolCode
	} else if code != cfg.ProtocolCode {
		// 目录域在模型有历史任务后就锁死了协议字段，走到这里说明锁被绕过了。
		p.logf("任务快照与当前配置的协议不一致，按快照执行",
			"modelCode", job.ModelCode, "snapshot", code, "config", cfg.ProtocolCode)
	}
	cfg.ProtocolCode = code

	proto, ok := protocol.Get(code)
	if !ok {
		// ⚠️ 这正是老系统最隐蔽的那个症状：后台一切正常、模型能启用，
		// **Worker 执行时才报"不支持的协议"**。新系统靠注册表同步
		// （protocol.All → api_protocol）堵住入口，这里是最后一道兜底。
		return cfg, nil, rejected("不支持的模型 API 协议: " + code), false
	}
	return cfg, proto, genapp.SubmitResult{}, true
}

// send 发一次请求并落日志。
func (p *HTTPProvider) send(ctx context.Context, cfg RunConfig, job genapp.Job,
	req protocol.Request, path string) (transport.Response, error) {
	tr, full, body, err := p.buildTransportRequest(ctx, cfg, req, path)
	if err != nil {
		return transport.Response{Kind: buildErrKind(err)}, err
	}
	resp, sendErr := p.client.Do(ctx, tr)
	p.writeLog(ctx, cfg, job, full, body, resp, sendErr)
	return resp, sendErr
}

// sendStream 发一次流式请求，逐块交给 onChunk。**不落日志**：流式调用的日志要记
// 最终摘要而不是整条流，摘要只有调用方（解析完之后）才知道，由调用方落。
//
// ⚠️ 复用 p.client（按连接超时分桶缓存的客户端），**不要新建 http.Client** ——
// 每次新建一个会让连接池与后台 goroutine 只增不减（线上事故换来的约束，见 transport.Client）。
// ⚠️ 空闲超时为 0 时显式给 60 秒，不依赖传输层的隐式缺省：配置页看到的值与实际生效的值
// 应当是同一个数。
func (p *HTTPProvider) sendStream(ctx context.Context, cfg RunConfig, req protocol.Request,
	path string, onChunk func([]byte) error) (resp transport.Response, full string, body []byte, err error) {
	tr, full, body, err := p.buildTransportRequest(ctx, cfg, req, path)
	if err != nil {
		return transport.Response{Kind: buildErrKind(err)}, full, body, err
	}
	if tr.Opts.IdleTimeout <= 0 {
		tr.Opts.IdleTimeout = defaultStreamIdleTimeout
	}
	resp, err = p.client.Stream(ctx, tr, onChunk)
	return resp, full, body, err
}

// configError 标记「认证配置不完整、签不出名」（volc_v4 缺地域、密钥不是 AK:SK）：请求没有发出去。
//
// ⚠️ 不能落到 classifySendError 的默认分支：那里是「提交后没拿到确定结果」，任务会被当成
// 可能已在上游计费、要人对账的死信 —— 而它连上游的门都没敲。
type configError struct{ err error }

func (e *configError) Error() string { return e.err.Error() }
func (e *configError) Unwrap() error { return e.err }

// urlError 标记「拼地址失败」：配置的 base URL 不合法，归为出网策略类（重试无用）。
type urlError struct{ err error }

func (e *urlError) Error() string { return e.err.Error() }
func (e *urlError) Unwrap() error { return e.err }

// buildErrKind 给构造请求阶段的错误分类：地址非法算策略拒绝；取文件失败等保持原样
// （零值 Kind，与重构前一致）。
func buildErrKind(err error) transport.ErrorKind {
	var ue *urlError
	if errors.As(err, &ue) {
		return transport.ErrorPolicy
	}
	return ""
}

// defaultStreamIdleTimeout 是流式调用未配空闲超时时的取值（60 秒不吐字就是死了）。
const defaultStreamIdleTimeout = 60 * time.Second

// buildTransportRequest 拼出一次上游调用：地址 + 认证头 + 自定义头 + Content-Type + 传输配置。
//
// 同步（send）与流式（sendStream）共用这一段，两条路的认证与超时不会各写一份。
// 返回的 body 是实际发出的字节（multipart 已编码），供请求日志脱敏后落库；
// 每次调用都生成新的 Body reader，重试时重新调用即可。
func (p *HTTPProvider) buildTransportRequest(ctx context.Context, cfg RunConfig,
	req protocol.Request, path string) (transport.Request, string, []byte, error) {
	opts := transport.Options{
		ConnectTimeout: cfg.ConnectTimeout, RequestTimeout: cfg.RequestTimeout,
		IdleTimeout: cfg.IdleTimeout, MaxRetries: cfg.MaxRetries, Policy: p.policy,
	}
	full, err := joinURL(cfg.BaseURL, path)
	if err != nil {
		return transport.Request{Opts: opts}, "", nil, &urlError{err}
	}

	header := authHeaders(cfg)
	for k, v := range req.Header {
		header[k] = v
	}

	body := req.Body
	contentType := req.ContentType
	if req.IsMultipart() {
		files, err := p.materializeFiles(ctx, cfg, req.Files)
		if err != nil {
			return transport.Request{Opts: opts}, full, nil, err
		}
		body, contentType, err = protocol.EncodeMultipartWith(req.Fields, req.RepeatedFields, files)
		if err != nil {
			return transport.Request{Opts: opts}, full, nil, err
		}
	}
	if contentType == "" && len(body) > 0 {
		contentType = "application/json"
	}
	if contentType != "" {
		header["Content-Type"] = contentType
	}
	// ⚠️ 签名类认证（volc_v4）放在最后：签名覆盖最终发出的正文与 Content-Type，
	// 在 multipart 编码之前签的话上游一律回签名错误（SCD-38）。
	if cfg.AuthType == runcfg.AuthVolcV4 {
		signed, err := credentialsOf(cfg).Headers(req.Method, full, header, body, time.Now())
		if err != nil {
			return transport.Request{Opts: opts}, full, nil, &configError{err}
		}
		for k, v := range signed {
			header[k] = v
		}
	}

	tr := transport.Request{Method: req.Method, URL: full, Header: header, Opts: opts}
	if len(body) > 0 {
		tr.Body = bytes.NewReader(body)
	}
	return tr, full, body, nil
}

// materializeFiles 把协议声明的文件位取成字节。
func (p *HTTPProvider) materializeFiles(ctx context.Context, cfg RunConfig,
	parts []protocol.FilePart) ([]protocol.MaterializedFile, error) {
	out := make([]protocol.MaterializedFile, 0, len(parts))
	for _, part := range parts {
		data, mime, err := p.fetchRef(ctx, cfg, part.Ref)
		if err != nil {
			return nil, fmt.Errorf("取 %s 文件失败: %w", part.Field, err)
		}
		out = append(out, protocol.MaterializedFile{
			Field: part.Field, Filename: part.Filename, ContentType: mime, Data: data,
		})
	}
	return out, nil
}

// fetchRef 把一个媒体引用（data: 或 http 地址）取成字节。
func (p *HTTPProvider) fetchRef(ctx context.Context, cfg RunConfig, ref string) (
	[]byte, string, error) {
	if strings.HasPrefix(ref, "data:") {
		return decodeDataURI(ref)
	}
	header := map[string]string{}
	if transport.SameOrigin(cfg.BaseURL, ref) {
		header = authHeaders(cfg)
	}
	d, err := p.client.Download(ctx, ref, header, transport.Options{
		ConnectTimeout: cfg.ConnectTimeout, RequestTimeout: cfg.RequestTimeout,
	}, p.maxInlineBytes())
	if err != nil {
		return nil, "", err
	}
	defer d.Close()
	buf := &bytes.Buffer{}
	if _, err := buf.ReadFrom(d); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), d.MIME, nil
}

// classifySendError 把传输层错误翻成提交结论。
//
// ⚠️ 这个分类直接决定**会不会重发**，是整个链路最该想清楚的一处：
//   - 连不上 / DNS 解不出：请求**根本没到上游**，重发安全。
//   - 超时 / 传到一半断了：**不知道上游收没收到**，绝不能重发
//     （重发可能在上游创建第二个任务，用户被计费两次而我们只认得一个）。
func (p *HTTPProvider) classifySendError(resp transport.Response, err error) genapp.SubmitResult {
	switch resp.Kind {
	case transport.ErrorPolicy:
		// 出网策略拦下：配置指向了不允许的地址，重试多少次都一样。
		return rejected("上游地址不被出网策略允许: " + err.Error())
	case transport.ErrorConnect, transport.ErrorDNS:
		// 用 503 表达"暂时性"，由执行器按可重试分类处理。
		return genapp.SubmitResult{
			Outcome: gendomain.SubmitRejected, HTTPStatus: 503,
			Message: "连接上游失败: " + err.Error(),
		}
	}
	return genapp.SubmitResult{
		Outcome: gendomain.SubmitUncertain,
		Message: "提交上游后未取得确定结果: " + err.Error(),
	}
}

// submitPathOf 取配置的提交路径，空时回落协议的默认路径。
func submitPathOf(cfg RunConfig, req protocol.Request) string {
	if cfg.SubmitPath != "" {
		return cfg.SubmitPath
	}
	return req.Path
}

func rejected(msg string) genapp.SubmitResult {
	// HTTP 400 语义：我们这边的问题，重试不会变好。
	return genapp.SubmitResult{Outcome: gendomain.SubmitRejected, HTTPStatus: 400, Message: msg}
}

// rawOf 把上游响应整理成能存进 jsonb 列的形态。
//
// ⚠️ 非 JSON 的响应（TTS 的音频字节、被网关拦截的二进制错误页）**不能原样塞**：
// jsonb 拒绝 `\u0000` 转义，写不进去就整条 SaveOutputsAndFinish 回滚，
// 执行器判可重试、一直重试到撞死线 —— 而上游其实早就成功了。
// 日志里只留一句 SQLSTATE 22P05，排查时根本想不到是"响应体里有个 0 字节"。
func rawOf(body []byte) json.RawMessage {
	if json.Valid(body) {
		return json.RawMessage(body)
	}
	safe := transport.Truncate(string(body))
	// 清理后几乎什么都不剩，说明本来就是二进制：记个摘要比记一串乱码有用。
	if printableRatio(safe) < 0.5 {
		safe = fmt.Sprintf("[二进制响应 %d 字节]", len(body))
	}
	b, _ := json.Marshal(map[string]string{"raw": safe})
	return b
}

func printableRatio(s string) float64 {
	if s == "" {
		return 0
	}
	printable := 0
	total := 0
	for _, r := range s {
		total++
		if r != utf8.RuneError && r >= 0x20 {
			printable++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(printable) / float64(total)
}

// joinURL 拼接 base 与 path。
func joinURL(base, path string) (string, error) {
	if path == "" {
		return base, nil
	}
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		// 配置里直接写了完整地址：按它来，但仍要过出网策略。
		return path, nil
	}
	if _, err := url.Parse(base); err != nil {
		return "", fmt.Errorf("Provider base URL 非法: %w", err)
	}
	// ⚠️ 两条都是踩出来的：
	// ① 不用 url.JoinPath —— 它会把 base 末尾的路径段吃掉，
	//    而很多中转的 base 是 `https://x.com/v1`，吃掉 `/v1` 就全 404。
	// ② 不走 `u.Path = ...` 再 String() —— 那会把已经转义过的任务 ID
	//    再编码一遍（`%2F` 变成 `%252F`），查询地址直接指向一个不存在的任务。
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(path, "/"), nil
}

func (p *HTTPProvider) logf(msg string, args ...any) {
	if p.log != nil {
		p.log.Warn(msg, args...)
	}
}

// writeLog 落一条 Provider 请求日志。
//
// ⚠️ 入库前必须脱敏与截断：认证头、密钥字段要打码，
// 内嵌 base64 只留摘要 —— 一张内联参考图 base64 后好几 MB，
// 原样入库一条就能把表撑爆（老系统的教训）。
func (p *HTTPProvider) writeLog(ctx context.Context, cfg RunConfig, job genapp.Job,
	fullURL string, reqBody []byte, resp transport.Response, sendErr error) {
	if p.logs == nil {
		return
	}
	kind := resp.Kind
	if sendErr != nil && kind == transport.ErrorNone {
		kind = transport.ClassifyError(sendErr)
	}
	respBody := string(resp.Body)
	if sendErr != nil {
		respBody = sendErr.Error()
	}
	entry := LogEntry{
		GenerationID: job.GenerationID, ProviderCode: cfg.ProviderCode,
		ModelCode: cfg.ModelCode, OperationCode: job.OperationCode,
		URL: fullURL, HTTPStatus: resp.StatusCode, ErrorType: string(kind),
		Duration:    resp.Elapsed,
		RequestBody: transport.RedactJSON(reqBody),
		// 响应体同样要截断：上游有时会回一整页 HTML 错误页。
		ResponseBody: transport.Truncate(respBody),
	}
	// ⚠️ 用独立的 context：主 context 可能因为超时已经取消，
	// 而那恰恰是最需要留下日志的时候。
	logCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := p.logs.Write(logCtx, entry); err != nil {
		p.logf("写 Provider 请求日志失败", "generationId", job.GenerationID, "error", err)
	}
}

var errNoData = errors.New("data URI 缺少 base64 段")
