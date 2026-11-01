package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/li904338203/tabtv/backend/internal/provider/runcfg"
)

// ErrIdleTimeout 流式读取时长时间没有新数据。
var ErrIdleTimeout = errors.New("流式响应空闲超时")

// Options 是一个 Provider 的传输配置。
type Options struct {
	ConnectTimeout time.Duration
	// RequestTimeout 整体上限。
	RequestTimeout time.Duration
	// IdleTimeout 流式读取时"多久没有新数据"的上限。
	//
	// ⚠️ 它与 RequestTimeout 是**两件事**：长文本可以合法地跑十分钟，
	// 但 60 秒不吐字就是死了。只配整体超时的话，上游挂起而不关连接时
	// 我们会干等满整个 RequestTimeout（老系统实测干等过 900 秒，日志里只有 0 字节）。
	IdleTimeout time.Duration
	MaxRetries  int
	Policy      URLPolicy
}

// Client 是共用的 HTTP 客户端。
//
// ⚠️ **必须复用**（这条是线上事故换来的）：Go 的 http.Client 与 Transport
// 内部持有连接池与后台 goroutine，每次请求新建一个会让连接与 goroutine
// 只增不减 —— 老系统的 Java 版实测 Worker 跑 1h45m 累积 57 个客户端实例、
// 224 个线程、单进程 211% CPU，把数据库挤到抢不到时间片，
// 所有查询接口从毫秒劣化到 8~10 秒。
//
// 按 `ConnectTimeout` 分桶缓存：不同 Provider 的连接超时不同，
// 但同一个超时值可以共用一个客户端。
type Client struct {
	mu      sync.Mutex
	clients map[time.Duration]*http.Client
	policy  URLPolicy
}

func NewClient(policy URLPolicy) *Client {
	return &Client{clients: map[time.Duration]*http.Client{}, policy: policy}
}

func (c *Client) httpClient(connectTimeout time.Duration) *http.Client {
	if connectTimeout <= 0 {
		connectTimeout = 10 * time.Second
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if cl, ok := c.clients[connectTimeout]; ok {
		return cl
	}
	dialer := &net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		// ⚠️ 连接期再校验一次地址：URL 校验挡不住 DNS 重绑定
		// （校验时解析到公网、连接时已改成内网）。
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if !c.policy.AllowPrivate {
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				if ip := net.ParseIP(host); ip != nil && IsPrivateIP(ip) {
					return nil, fmt.Errorf("%w: %s", ErrPrivateNet, host)
				}
			}
			return dialer.DialContext(ctx, network, addr)
		},
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: connectTimeout,
	}
	cl := &http.Client{
		Transport: tr,
		// ⚠️ **不跟随重定向**：跟随会把 Authorization 头带到重定向目标，
		// 而那个目标可能是任何地方（GO-31）。需要跟随时由调用方显式处理。
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	c.clients[connectTimeout] = cl
	return cl
}

// Request 是一次上游调用。
type Request struct {
	Method string
	URL    string
	Header map[string]string
	Body   io.Reader
	Opts   Options
}

// Response 是一次调用的结果。
type Response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
	Kind       ErrorKind
	Elapsed    time.Duration
}

// Do 发一次非流式请求。
func (c *Client) Do(ctx context.Context, req Request) (Response, error) {
	if err := c.policy.CheckURL(req.URL); err != nil {
		return Response{Kind: ErrorPolicy}, err
	}
	timeout := req.Opts.RequestTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := time.Now()
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, req.Body)
	if err != nil {
		return Response{Kind: ClassifyError(err)}, err
	}
	for k, v := range req.Header {
		httpReq.Header.Set(k, v)
	}
	resp, err := c.httpClient(req.Opts.ConnectTimeout).Do(httpReq)
	if err != nil {
		return Response{Kind: ClassifyError(err), Elapsed: time.Since(started)}, err
	}
	defer resp.Body.Close()

	// 限制读取量：上游返回一个几 GB 的响应不该把进程撑爆。
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return Response{StatusCode: resp.StatusCode, Kind: ErrorStream,
			Elapsed: time.Since(started)}, err
	}
	kind := ErrorNone
	if resp.StatusCode >= 400 {
		kind = ErrorHTTP
	}
	return Response{
		StatusCode: resp.StatusCode, Header: resp.Header, Body: body,
		Kind: kind, Elapsed: time.Since(started),
	}, nil
}

// maxResponseBytes 单次非流式响应的上限。产物字节走 Stream，不走这里。
const maxResponseBytes = 32 << 20

// Stream 发一次流式请求，逐块交给 onChunk。
//
// ⚠️ **必须逐块读并带空闲看门狗**，不能整包缓冲：
// 整包缓冲下，上游挂起而不关连接时我们一个字节都拿不到 ——
// 哪怕它早就在 SSE 里推了 error 事件，也要等满整个 RequestTimeout
// （老系统实测干等过 900 秒，日志里只留 0 字节）。
//
// ⚠️ 中止读取只认 `ctx` 取消 + 关闭 Body。
func (c *Client) Stream(ctx context.Context, req Request,
	onChunk func([]byte) error) (Response, error) {
	if err := c.policy.CheckURL(req.URL); err != nil {
		return Response{Kind: ErrorPolicy}, err
	}
	total := req.Opts.RequestTimeout
	if total <= 0 {
		total = 10 * time.Minute
	}
	idle := req.Opts.IdleTimeout
	if idle <= 0 {
		idle = 60 * time.Second
	}

	ctx, cancel := context.WithTimeout(ctx, total)
	defer cancel()

	started := time.Now()
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, req.Body)
	if err != nil {
		return Response{Kind: ClassifyError(err)}, err
	}
	for k, v := range req.Header {
		httpReq.Header.Set(k, v)
	}
	resp, err := c.httpClient(req.Opts.ConnectTimeout).Do(httpReq)
	if err != nil {
		return Response{Kind: ClassifyError(err), Elapsed: time.Since(started)}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		return Response{StatusCode: resp.StatusCode, Header: resp.Header, Body: body,
			Kind: ErrorHTTP, Elapsed: time.Since(started)}, nil
	}

	// 空闲看门狗：每次读到数据就续期；超时则取消 ctx，让阻塞的 Read 返回。
	//
	// ⚠️ 用**显式标志**记录"是它触发的"，不要靠 `timer.Stop()` 的返回值反推 ——
	// Stop 在定时器已触发时返回 false，与"本来就停过"分不开，
	// 结果是空闲超时被归类成普通的 context canceled，日志里看不出是哪种超时。
	var idleFired atomic.Bool
	watchdog := time.AfterFunc(idle, func() {
		idleFired.Store(true)
		cancel()
	})
	defer watchdog.Stop()

	buf := make([]byte, 32<<10)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			// 已经收到的数据先交出去：不能因为后面超时就把它整批丢掉。
			if err := onChunk(buf[:n]); err != nil {
				return Response{StatusCode: resp.StatusCode, Kind: ErrorStream,
					Elapsed: time.Since(started)}, err
			}
			if !idleFired.Load() {
				watchdog.Reset(idle)
			}
		}
		if readErr != nil {
			if idleFired.Load() {
				return Response{StatusCode: resp.StatusCode, Kind: ErrorStream,
					Elapsed: time.Since(started)}, ErrIdleTimeout
			}
			if errors.Is(readErr, io.EOF) {
				return Response{StatusCode: resp.StatusCode, Header: resp.Header,
					Kind: ErrorNone, Elapsed: time.Since(started)}, nil
			}
			return Response{StatusCode: resp.StatusCode, Kind: ClassifyError(readErr),
				Elapsed: time.Since(started)}, readErr
		}
	}
}

// AuthHeader 按认证方式生成请求头。
//
// ⚠️ 返回的 map **只用于与 Provider 同源的请求**。下载产物时先用
// SameOrigin 判一次：上游返回的产物地址常常是 CDN 签名链接，
// 把 API key 发过去等于把密钥泄漏给第三方（GO-31）。
func AuthHeader(authType, apiKey, headerName string) map[string]string {
	switch authType {
	case "bearer":
		return map[string]string{"Authorization": "Bearer " + apiKey}
	case "header":
		name := headerName
		if name == "" {
			name = "X-API-Key"
		}
		return map[string]string{name: apiKey}
	}
	return map[string]string{}
}

// Credentials 是一个 Provider 的认证配置（密钥已解密），按一次请求生成认证头。
//
// bearer / header 与请求内容无关（同 AuthHeader）；volc_v4 要覆盖方法、地址、正文与时间，所以必须在
// 请求体完全定下之后再调（SCD-38）。提交、查询与连接测试都走它，签名规则只有一份。
type Credentials struct {
	Type       string
	Key        string
	HeaderName string
	// Region / Service 只对 volc_v4 有意义。
	Region, Service string
}

// Headers 返回这次请求要带的认证头。
func (c Credentials) Headers(method, rawURL string, header map[string]string, body []byte,
	now time.Time) (map[string]string, error) {
	if c.Type != runcfg.AuthVolcV4 {
		return AuthHeader(c.Type, c.Key, c.HeaderName), nil
	}
	ak, sk, err := ParseVolcKey(c.Key)
	if err != nil {
		return nil, err
	}
	return SignVolcV4(method, rawURL, header, body,
		VolcCredential{AccessKey: ak, SecretKey: sk, Region: c.Region, Service: c.Service}, now)
}
