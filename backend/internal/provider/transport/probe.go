package transport

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ProbeResult 是一次连接测试的结果。
type ProbeResult struct {
	OK         bool          `json:"ok"`
	StatusCode int           `json:"statusCode,omitempty"`
	ErrorKind  ErrorKind     `json:"errorKind"`
	Message    string        `json:"message"`
	LatencyMS  int64         `json:"latencyMs"`
	Elapsed    time.Duration `json:"-"`
}

// Probe 测试一个 Provider 是否可达（P03-02 挂账，在此补上）。
//
// ⚠️ 判定标准是**能不能连上并得到 HTTP 响应**，不是 2xx：
// 很多上游的探活路径需要参数，会回 400/404 —— 那也说明"网络与鉴权这一层是通的"。
// 只认 2xx 会让运维在一个其实正常的配置上反复折腾。
//
// ⚠️ 401/403 要**单独说出来**：它们意味着连通但**密钥不对**，
// 这是运维最需要知道的一种失败，混进"连接失败"里就查不出来了。
func (c *Client) Probe(ctx context.Context, baseURL, testPath string,
	header map[string]string, opts Options) ProbeResult {
	url := ProbeURL(baseURL, testPath)

	// 探活用短超时：这是交互式操作，运维在页面上等着。
	if opts.RequestTimeout <= 0 || opts.RequestTimeout > 15*time.Second {
		opts.RequestTimeout = 15 * time.Second
	}
	resp, err := c.Do(ctx, Request{Method: "GET", URL: url, Header: header, Opts: opts})
	if err != nil {
		return ProbeResult{
			OK: false, ErrorKind: ClassifyError(err),
			Message:   probeMessage(ClassifyError(err), err),
			LatencyMS: resp.Elapsed.Milliseconds(), Elapsed: resp.Elapsed,
		}
	}

	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return ProbeResult{
			OK: false, StatusCode: resp.StatusCode, ErrorKind: ErrorHTTP,
			Message:   "连接正常但鉴权被拒，请检查 API Key",
			LatencyMS: resp.Elapsed.Milliseconds(), Elapsed: resp.Elapsed,
		}
	case resp.StatusCode >= 500:
		return ProbeResult{
			OK: false, StatusCode: resp.StatusCode, ErrorKind: ErrorHTTP,
			Message:   fmt.Sprintf("上游返回 %d，服务可能不可用", resp.StatusCode),
			LatencyMS: resp.Elapsed.Milliseconds(), Elapsed: resp.Elapsed,
		}
	}
	return ProbeResult{
		OK: true, StatusCode: resp.StatusCode, ErrorKind: ErrorNone,
		Message:   fmt.Sprintf("连接正常（HTTP %d）", resp.StatusCode),
		LatencyMS: resp.Elapsed.Milliseconds(), Elapsed: resp.Elapsed,
	}
}

// ProbeURL 是连接测试要打的地址。签名类认证（volc_v4）要先拿到它算签名，所以单独给出。
func ProbeURL(baseURL, testPath string) string {
	url := strings.TrimRight(baseURL, "/")
	if testPath != "" {
		url += "/" + strings.TrimLeft(testPath, "/")
	}
	return url
}

// probeMessage 把错误类别翻成运维看得懂的话 ——
// 「dial tcp: i/o timeout」对运维没有帮助，「连不上，请检查地址与防火墙」才有。
func probeMessage(kind ErrorKind, err error) string {
	switch kind {
	case ErrorDNS:
		return "域名解析失败，请检查 base URL 是否写对"
	case ErrorConnect:
		return "连接被拒绝，请检查地址、端口与防火墙"
	case ErrorTimeout:
		return "连接超时，上游无响应或网络不通"
	case ErrorPolicy:
		return "地址不被允许：" + err.Error()
	case ErrorStream:
		return "连接中断：" + err.Error()
	}
	return err.Error()
}
