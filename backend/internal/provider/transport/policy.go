// Package transport 是所有 Provider 共用的 HTTP 传输层。
//
// ⚠️ 协议适配器**不各自实现一份**（08 §6）：URL 策略、认证头、禁止重定向、
// 三类超时、错误分类、请求日志都在这里收口。各抄一份的结果是
// 某个适配器漏了一条约束，而那条约束恰恰是上次事故的修复。
package transport

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

var (
	ErrURLScheme  = errors.New("只允许 http/https")
	ErrPrivateNet = errors.New("不允许访问内网地址")
	ErrURLHost    = errors.New("URL 缺少主机名")
)

// URLPolicy 约束出网地址。
//
// ⚠️ `AllowPrivate` 在 production 下**必须为 false**（config 启动时强制）：
// 允许内网意味着任何能配置 Provider base_url 的人都可以让服务端
// 去访问 169.254.169.254（云元数据）或内网数据库 —— 这是 SSRF。
type URLPolicy struct {
	AllowPrivate bool
}

// CheckURL 校验一个出网地址。
//
// ⚠️ 这里只能挡住"写死在配置里的内网地址"。**DNS 重绑定挡不住** ——
// 域名解析结果随时会变，校验时是公网 IP、实际连接时可能已经指向内网。
// 真正的防线是 `DialContext` 上的连接期校验（见 Client），两层都要有。
func (p URLPolicy) CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("URL 非法: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: %s", ErrURLScheme, u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return ErrURLHost
	}
	if p.AllowPrivate {
		return nil
	}
	// 字面量 IP 直接判；域名交给连接期校验。
	if ip := net.ParseIP(host); ip != nil && IsPrivateIP(ip) {
		return fmt.Errorf("%w: %s", ErrPrivateNet, host)
	}
	// localhost 这类名字不必等到解析：它们的意图很明确。
	if isLoopbackName(host) {
		return fmt.Errorf("%w: %s", ErrPrivateNet, host)
	}
	return nil
}

func isLoopbackName(host string) bool {
	h := strings.ToLower(host)
	return h == "localhost" || strings.HasSuffix(h, ".localhost") ||
		strings.HasSuffix(h, ".local") || strings.HasSuffix(h, ".internal")
}

// IsPrivateIP 判定内网/特殊地址。
//
// ⚠️ 覆盖面要全：只判 10/172.16/192.168 会漏掉云元数据端点
// （169.254.169.254，链路本地）与 IPv6 的 ULA/链路本地。
func IsPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	// 100.64.0.0/10 运营商级 NAT：在云上常被用作内部网段。
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			return true
		}
		// 0.0.0.0/8 与 240.0.0.0/4 保留段。
		if v4[0] == 0 || v4[0] >= 240 {
			return true
		}
	}
	return false
}

// SameOrigin 判定两个 URL 是否同源（scheme + host + port）。
//
// ⚠️ **只有与 Provider 同源的下载才携带它的认证**（GO-31）。
// 上游返回的产物地址常常是 CDN 或对象存储的签名链接，
// 把 Provider 的 API key 一起发过去等于把密钥泄漏给第三方。
func SameOrigin(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) &&
		strings.EqualFold(ua.Host, ub.Host)
}

// ErrorKind 是一次请求失败的类别，进请求日志便于排查。
//
// ⚠️ 区分开是因为处理方式不同：`dns`/`connect` 多半是配置错了，
// `timeout` 可能是上游慢也可能是我们的超时配小了，
// `stream` 是"连上了但中途断了"——而这三种在日志里长得很像。
type ErrorKind string

const (
	ErrorNone    ErrorKind = "none"
	ErrorDNS     ErrorKind = "dns"
	ErrorConnect ErrorKind = "connect"
	ErrorTimeout ErrorKind = "timeout"
	ErrorStream  ErrorKind = "stream"
	ErrorHTTP    ErrorKind = "http"
	ErrorPolicy  ErrorKind = "policy"
)

// ClassifyError 把底层错误归类。
func ClassifyError(err error) ErrorKind {
	if err == nil {
		return ErrorNone
	}
	if errors.Is(err, ErrPrivateNet) || errors.Is(err, ErrURLScheme) || errors.Is(err, ErrURLHost) {
		return ErrorPolicy
	}
	if errors.Is(err, ErrIdleTimeout) {
		return ErrorStream
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return ErrorDNS
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ErrorTimeout
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return ErrorConnect
	}
	if strings.Contains(err.Error(), "context deadline exceeded") {
		return ErrorTimeout
	}
	return ErrorHTTP
}
