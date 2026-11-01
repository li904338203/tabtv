package oauth

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
)

const (
	wellKnownPR = "/.well-known/oauth-protected-resource"
	wellKnownAS = "/.well-known/oauth-authorization-server"
	// resourceName 是受保护资源元数据里的 resource_name（客户端展示用）。
	resourceName = "seeAitv MCP"
)

// MCP 端点路径（与 mcp/transport.Mount 一致）。
const (
	PathMCP      = "/mcp"
	PathReadOnly = "/mcp/readonly"
	pathToolset  = "/mcp/x/"
)

// IsEndpointPath 判断 path 是不是本服务的一个 MCP 端点（/mcp、/mcp/readonly、/mcp/x/{已知工具集}）。
func IsEndpointPath(path string) bool {
	switch path {
	case PathMCP, PathReadOnly:
		return true
	}
	if ts, ok := strings.CutPrefix(path, pathToolset); ok {
		return domain.IsToolset(ts)
	}
	return false
}

// Endpoints 计算授权服务器与受保护资源的对外地址。
//
// 根地址（base）来自 SEEAITV_MCP_PUBLIC_URL：既接受 /mcp 端点地址（如 https://example.com/mcp，
// 与客户端配置片段同一个值）也接受根（https://example.com）—— 末尾的 /mcp 会被去掉。
// issuer = base；resource = base + 端点路径。
//
// ⚠️ 为空时**按请求推导**（scheme + Host）：只为本地开发（局域网里换一台设备用 IP 访问也能用）。
// production 下 config 强制配置且必须 https —— 按 Host 推导的话 resource 由请求头决定，
// 而公网地址配错时客户端表现为「授权成功但每次 401」（受众对不上）。
type Endpoints struct{ base string }

// NewEndpoints 由公网地址构造；非法或为空时按请求推导。
func NewEndpoints(publicURL string) Endpoints {
	u := strings.TrimSpace(publicURL)
	if u == "" {
		return Endpoints{}
	}
	c, err := CanonicalResource(u)
	if err != nil {
		return Endpoints{}
	}
	c = strings.TrimSuffix(c, PathMCP)
	return Endpoints{base: c}
}

// Configured 是否显式配置了公网地址。
func (e Endpoints) Configured() bool { return e.base != "" }

// Base 是根地址（无尾斜杠）。
func (e Endpoints) Base(r *http.Request) string {
	if e.base != "" {
		return e.base
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	if c, err := CanonicalResource(scheme + "://" + r.Host); err == nil {
		return c
	}
	return scheme + "://" + strings.ToLower(r.Host)
}

// Issuer 是授权服务器标识（RFC 8414 / RFC 9207 的 iss）。
func (e Endpoints) Issuer(r *http.Request) string { return e.Base(r) }

// Resource 是某个 MCP 端点的规范 URI（受保护资源标识 = 令牌受众）。
func (e Endpoints) Resource(r *http.Request, path string) string { return e.Base(r) + path }

// ResourceMetadataURL 是某个端点的受保护资源元数据地址（RFC 9728 §3.1：
// well-known 段插在主机与路径之间）。
func (e Endpoints) ResourceMetadataURL(r *http.Request, path string) string {
	origin, prefix := splitBase(e.Base(r))
	return origin + wellKnownPR + prefix + path
}

// CheckResource 规范化客户端给的 resource 并判断它是不是本服务的一个 MCP 端点；是则返回规范 URI。
func (e Endpoints) CheckResource(r *http.Request, raw string) (string, bool) {
	c, err := CanonicalResource(raw)
	if err != nil {
		return "", false
	}
	base := e.Base(r)
	path, ok := strings.CutPrefix(c, base)
	if !ok || !IsEndpointPath(path) {
		return "", false
	}
	return c, true
}

// splitBase 把 base 拆成 origin（scheme://host[:port]）与路径前缀（可为空）。
func splitBase(base string) (string, string) {
	u, err := url.Parse(base)
	if err != nil {
		return base, ""
	}
	return u.Scheme + "://" + u.Host, u.Path
}

var errBadResource = errors.New("resource 不是合法的绝对 URI")

// CanonicalResource 规范化一个资源 URI（RFC 8707 §2 + RFC 3986 §6）：
// scheme 与主机小写、去掉默认端口、去掉末尾斜杠；**不允许** fragment、query 与 userinfo。
// 路径大小写保留（路径区分大小写）。
func CanonicalResource(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !u.IsAbs() || u.Host == "" || u.Opaque != "" {
		return "", errBadResource
	}
	if u.Fragment != "" || u.RawFragment != "" || strings.Contains(raw, "#") {
		return "", errors.New("resource 不能带 fragment")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return "", errors.New("resource 不能带 query")
	}
	if u.User != nil {
		return "", errors.New("resource 不能带 userinfo")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New("resource 只能是 http(s)")
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	hostport := host
	if strings.Contains(host, ":") { // IPv6 字面量
		hostport = "[" + host + "]"
	}
	if port != "" {
		hostport = net.JoinHostPort(host, port)
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	return scheme + "://" + hostport + path, nil
}

// ---------------------------------------------------------------- 元数据端点

// protectedResource 是 RFC 9728 受保护资源元数据。根路径对应 /mcp；带路径后缀的对应那个端点，
// 且 resource 与该端点**逐字一致**（客户端会拿它与自己配置的 URL 比对，不一致就拒绝继续）。
func (s *Server) protectedResource(w http.ResponseWriter, r *http.Request) {
	suffix := strings.TrimPrefix(r.URL.Path, wellKnownPR)
	path := PathMCP
	if suffix != "" && suffix != "/" {
		_, prefix := splitBase(s.urls.Base(r))
		p, ok := strings.CutPrefix(suffix, prefix)
		if !ok || !IsEndpointPath(p) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found",
				"error_description": "no MCP endpoint at " + suffix})
			return
		}
		path = p
	}
	name := resourceName
	if path == PathReadOnly {
		name += " (read-only)"
	} else if ts, ok := strings.CutPrefix(path, pathToolset); ok {
		name += " (" + ts + ")"
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 s.urls.Resource(r, path),
		"authorization_servers":    []string{s.urls.Issuer(r)},
		"scopes_supported":         domain.Scopes(),
		"bearer_methods_supported": []string{"header"},
		"resource_name":            name,
	})
}

// authorizationServer 是 RFC 8414 授权服务器元数据。
func (s *Server) authorizationServer(w http.ResponseWriter, r *http.Request) {
	iss := s.urls.Issuer(r)
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                         iss,
		"authorization_endpoint":                         iss + "/oauth/authorize",
		"token_endpoint":                                 iss + "/oauth/token",
		"registration_endpoint":                          iss + "/oauth/register",
		"revocation_endpoint":                            iss + "/oauth/revoke",
		"response_types_supported":                       []string{"code"},
		"response_modes_supported":                       []string{"query"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"revocation_endpoint_auth_methods_supported":     []string{"none"},
		"scopes_supported":                               domain.Scopes(),
		"client_id_metadata_document_supported":          true,
		"authorization_response_iss_parameter_supported": true,
	})
}

func trimSlash(s string) string { return strings.TrimRight(strings.TrimSpace(s), "/") }
