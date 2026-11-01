package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/repo"
	ptransport "github.com/li904338203/tabtv/backend/internal/provider/transport"
)

// 客户端登记：**优先 CIMD**（客户端元数据文档，MCP 2026-07-28 推荐），DCR（RFC 7591）只作兼容
// （规范已标为废弃）。两者都落 mcp_client，registration 区分来源 —— 同意页据此告诉用户
// 「这个客户端的身份经过了域名验证」还是「这是一个自称叫 X 的未验证客户端」。

const (
	// cimdMaxBytes 是元数据文档的大小上限（64 KiB）。正常文档不过几百字节；不设上限的话，
	// 一个恶意 client_id 能让授权端点去下载任意大的东西。
	cimdMaxBytes = 64 << 10
	// cimdTimeout 是拉取的连接与整体超时。授权端点是用户在浏览器里等着的同步请求。
	cimdTimeout = 5 * time.Second
	// maxRedirectURIs 是一个客户端最多登记的回调地址数。
	maxRedirectURIs = 10
	// maxRedirectURILen 与 mcp_auth_code.redirect_uri 列宽一致。
	maxRedirectURILen = 1024
	// maxClientKeyLen 与 mcp_client.client_key 列宽一致。
	maxClientKeyLen = 512
	// maxClientNameRunes 与 mcp_client.name 列宽一致。
	maxClientNameRunes = 100
	// DCRClientPrefix 是 DCR 生成的 client_id 前缀（与 CIMD 的 https URL 在形态上不可能撞上）。
	DCRClientPrefix = "mcpc_"
)

// ---------------------------------------------------------------- CIMD 拉取

// FetchResult 是一次元数据文档拉取的结果。
type FetchResult struct {
	Status      int
	ContentType string
	Body        []byte
}

// Fetcher 拉取客户端元数据文档。实现必须走共享出网策略（TransportFetcher）。
type Fetcher interface {
	Fetch(ctx context.Context, rawURL string) (FetchResult, error)
}

// errTooLarge：文档超过 cimdMaxBytes。
var errTooLarge = fmt.Errorf("客户端元数据文档超过 %d 字节上限", cimdMaxBytes)

// TransportFetcher 用 provider/transport 的共享客户端拉取：URL 策略（production 禁内网）+ 连接期地址
// 校验（防 DNS 重绑定）+ **不跟随重定向**，都由那一层保证。
//
// ⚠️ CIMD 是服务端按客户端给的 URL 主动出网 —— 一个标准的 SSRF 面。**只能复用这一套**，别新写 http.Get：
// 新写的那份迟早漏掉其中一条（尤其是连接期校验，URL 校验挡不住 DNS 重绑定）。
type TransportFetcher struct{ Client *ptransport.Client }

func (f TransportFetcher) Fetch(ctx context.Context, rawURL string) (FetchResult, error) {
	var body []byte
	resp, err := f.Client.Stream(ctx, ptransport.Request{
		Method: http.MethodGet, URL: rawURL,
		Header: map[string]string{"Accept": "application/json"},
		Opts:   ptransport.Options{ConnectTimeout: cimdTimeout, RequestTimeout: cimdTimeout, IdleTimeout: cimdTimeout},
	}, func(chunk []byte) error {
		if len(body)+len(chunk) > cimdMaxBytes {
			return errTooLarge
		}
		body = append(body, chunk...)
		return nil
	})
	if err != nil {
		return FetchResult{}, err
	}
	if resp.StatusCode >= 400 {
		body = resp.Body
	}
	return FetchResult{Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Body: body}, nil
}

// ValidCIMDURL 校验 client_id 作为元数据文档地址是否合法：https、有主机、有非根路径、
// 无 fragment / userinfo、无 . / .. 段（CIMD 草案 §3）。
func ValidCIMDURL(raw string) error {
	if len(raw) > maxClientKeyLen {
		return fmt.Errorf("client_id 超过 %d 字符", maxClientKeyLen)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("CIMD 的 client_id 必须是 https URL")
	}
	if u.Fragment != "" || strings.Contains(raw, "#") || u.User != nil {
		return errors.New("CIMD 的 client_id 不能带 fragment 或 userinfo")
	}
	if u.Path == "" || u.Path == "/" {
		return errors.New("CIMD 的 client_id 必须带路径")
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == "." || seg == ".." {
			return errors.New("CIMD 的 client_id 路径里不能有 . 或 ..")
		}
	}
	return nil
}

// cimdDoc 是我们读取的元数据字段（其余忽略）。
type cimdDoc struct {
	ClientID                string   `json:"client_id"`
	ClientName              string   `json:"client_name"`
	ClientURI               string   `json:"client_uri"`
	LogoURI                 string   `json:"logo_uri"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	SoftwareID              string   `json:"software_id"`
	SoftwareVersion         string   `json:"software_version"`
}

// ParseClientMetadata 校验一次拉取结果并转成待写入的客户端（纯函数）：
// 200、JSON、文档里的 client_id **逐字等于** URL、redirect_uris 合法、只接受公共客户端。
//
// ⚠️ 3xx 一律拒（不跟随重定向）：元数据的身份就是那个 URL，跳到别处的文档不能代表它。
// ⚠️ client_id 必须等于 URL：否则任何人都能在自己的域名上放一份声称是别的客户端的文档。
func ParseClientMetadata(docURL string, res FetchResult) (repo.Client, error) {
	if res.Status >= 300 && res.Status < 400 {
		return repo.Client{}, fmt.Errorf("客户端元数据地址返回了重定向（%d），不跟随", res.Status)
	}
	if res.Status != http.StatusOK {
		return repo.Client{}, fmt.Errorf("客户端元数据地址返回 HTTP %d", res.Status)
	}
	if len(res.Body) > cimdMaxBytes {
		return repo.Client{}, errTooLarge
	}
	mt, _, err := mime.ParseMediaType(res.ContentType)
	if err != nil || (mt != "application/json" && !strings.HasSuffix(mt, "+json")) {
		return repo.Client{}, fmt.Errorf("客户端元数据不是 JSON（Content-Type: %q）", res.ContentType)
	}
	var doc cimdDoc
	if err := json.Unmarshal(res.Body, &doc); err != nil {
		return repo.Client{}, fmt.Errorf("客户端元数据不是合法 JSON：%w", err)
	}
	if doc.ClientID != docURL {
		return repo.Client{}, fmt.Errorf("客户端元数据里的 client_id（%q）与文档地址不一致", doc.ClientID)
	}
	if m := doc.TokenEndpointAuthMethod; m != "" && m != "none" {
		return repo.Client{}, fmt.Errorf("只支持公共客户端（token_endpoint_auth_method=none），文档声明的是 %q", m)
	}
	uris, err := validRedirectURIs(doc.RedirectURIs)
	if err != nil {
		return repo.Client{}, err
	}
	name := clientName(doc.ClientName)
	if name == "" {
		u, _ := url.Parse(docURL)
		name = u.Hostname()
	}
	meta, _ := json.Marshal(map[string]any{
		"client_uri": doc.ClientURI, "logo_uri": doc.LogoURI,
		"software_id": doc.SoftwareID, "software_version": doc.SoftwareVersion,
	})
	return repo.Client{Key: docURL, Name: name, RedirectURIs: uris, Registration: repo.RegistrationCIMD, Metadata: meta}, nil
}

// fetchCIMD 拉取并校验一份元数据文档。
func fetchCIMD(ctx context.Context, f Fetcher, docURL string) (repo.Client, error) {
	if f == nil {
		return repo.Client{}, errors.New("未启用客户端元数据文档拉取")
	}
	res, err := f.Fetch(ctx, docURL)
	if err != nil {
		return repo.Client{}, fmt.Errorf("拉取客户端元数据失败：%w", err)
	}
	return ParseClientMetadata(docURL, res)
}

// resolveClient 按 client_id 找客户端：https URL → CIMD（24 小时缓存、过期懒刷新、拉取失败沿用旧缓存
// 最多再 24 小时并告警）；否则 → DCR 登记的 ID。
func (s *Server) resolveClient(ctx context.Context, clientID string) (repo.Client, error) {
	if clientID == "" {
		return repo.Client{}, errors.New("缺少 client_id")
	}
	if !strings.HasPrefix(clientID, "https://") {
		if !strings.HasPrefix(clientID, DCRClientPrefix) || len(clientID) > maxClientKeyLen {
			return repo.Client{}, errors.New("client_id 既不是客户端元数据文档地址（https URL），也不是本服务注册的客户端")
		}
		c, err := s.o.Store.GetClientByKey(ctx, clientID)
		if err != nil || c.Registration != repo.RegistrationDCR {
			if err != nil && !errors.Is(err, repo.ErrNotFound) {
				return repo.Client{}, err
			}
			return repo.Client{}, errors.New("未注册的 client_id")
		}
		return c, nil
	}
	if err := ValidCIMDURL(clientID); err != nil {
		return repo.Client{}, err
	}
	cached, err := s.o.Store.GetClientByKey(ctx, clientID)
	hasCache := err == nil && cached.Registration == repo.RegistrationCIMD && cached.FetchedAt != nil
	if err != nil && !errors.Is(err, repo.ErrNotFound) {
		return repo.Client{}, err
	}
	now := s.now()
	if hasCache && now.Sub(*cached.FetchedAt) < domain.CIMDCacheTTL {
		return cached, nil
	}
	fresh, ferr := fetchCIMD(ctx, s.o.Fetcher, clientID)
	if ferr == nil {
		fresh.ID = s.o.NextID()
		if hasCache {
			fresh.ID = cached.ID
		}
		c, err := s.o.Store.UpsertCIMDClient(ctx, fresh)
		if err != nil {
			return repo.Client{}, err
		}
		return c, nil
	}
	if hasCache && now.Sub(*cached.FetchedAt) < domain.CIMDCacheTTL+domain.CIMDStaleGrace {
		s.log.Warn("刷新 MCP 客户端元数据失败，沿用旧缓存", "clientId", clientID,
			"fetchedAt", cached.FetchedAt, "error", ferr)
		return cached, nil
	}
	return repo.Client{}, ferr
}

// ---------------------------------------------------------------- 回调地址

// validRedirectURIs 校验一组回调地址：非空、至多 10 个、每个都合法；去重保序。
func validRedirectURIs(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, errors.New("redirect_uris 不能为空")
	}
	if len(in) > maxRedirectURIs {
		return nil, fmt.Errorf("redirect_uris 最多 %d 个", maxRedirectURIs)
	}
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, u := range in {
		if err := ValidRedirectURI(u); err != nil {
			return nil, err
		}
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out, nil
}

// ValidRedirectURI：绝对 URI、无 fragment；https，或回环地址上的 http（RFC 8252 §7.3 本机应用）。
// 其他明文 http 一律拒（授权码会在网络上明文传）。
func ValidRedirectURI(raw string) error {
	if len(raw) > maxRedirectURILen {
		return fmt.Errorf("redirect_uri 超过 %d 字符", maxRedirectURILen)
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return fmt.Errorf("redirect_uri 不是合法的绝对地址：%q", raw)
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return fmt.Errorf("redirect_uri 不能带 fragment：%q", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("redirect_uri 只能是 https 或回环地址上的 http：%q", raw)
	}
	return fmt.Errorf("redirect_uri 只能是 https 或回环地址上的 http：%q", raw)
}

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// MatchRedirectURI 判断请求的回调地址是否与已登记的某一个匹配。
//
// **精确匹配**（逐字相等）—— 只有一个例外：回环地址允许端口不同（RFC 8252 §7.3：本机应用每次启动
// 临时监听一个端口，登记时不可能知道是哪个）。scheme、主机、路径、query 仍须逐字相同；
// https → http 的降级、换路径、换主机一律不匹配。
func MatchRedirectURI(registered []string, requested string) bool {
	for _, r := range registered {
		if r == requested {
			return true
		}
	}
	q, err := url.Parse(requested)
	if err != nil || q.Scheme != "http" || !isLoopbackHost(q.Hostname()) || q.Fragment != "" {
		return false
	}
	for _, r := range registered {
		u, err := url.Parse(r)
		if err != nil || u.Scheme != q.Scheme || !isLoopbackHost(u.Hostname()) {
			continue
		}
		if strings.EqualFold(u.Hostname(), q.Hostname()) && u.EscapedPath() == q.EscapedPath() && u.RawQuery == q.RawQuery {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- DCR（兼容）

type dcrRequest struct {
	RedirectURIs            []string `json:"redirect_uris"`
	ClientName              string   `json:"client_name"`
	ClientURI               string   `json:"client_uri"`
	LogoURI                 string   `json:"logo_uri"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	SoftwareID              string   `json:"software_id"`
	SoftwareVersion         string   `json:"software_version"`
}

// DefaultDCRPerHour 是同一 IP 每小时可注册的客户端数（**设计建议值**，不是已批准的政策）。
const DefaultDCRPerHour = 20

// register 是 RFC 7591 动态客户端注册（兼容）。只接受公共客户端（auth method = none）、
// 回调地址为 https 或回环 http；登记为 registration='dcr'（同意页显示「未验证的客户端」）。
//
// ⚠️ 按 IP 限流：注册不需要任何身份，不限的话一个脚本能往 mcp_client 里灌任意多行。
func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	if s.o.Limiter != nil && s.o.DCRPerHour > 0 {
		d := s.o.Limiter.AllowFixed(r.Context(), []string{"mcp", "rl", "dcr", clientIP(r)}, s.o.DCRPerHour, time.Hour)
		if !d.Allowed {
			w.Header().Set("Retry-After", fmt.Sprint(d.RetryAfter))
			writeJSON(w, http.StatusTooManyRequests, oauthErr("too_many_requests", "client registration rate limit exceeded"))
			return
		}
	}
	var in dcrRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, cimdMaxBytes)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, oauthErr("invalid_client_metadata", "request body must be a JSON object"))
		return
	}
	if m := in.TokenEndpointAuthMethod; m != "" && m != "none" {
		writeJSON(w, http.StatusBadRequest, oauthErr("invalid_client_metadata",
			"only public clients are supported (token_endpoint_auth_method=none)"))
		return
	}
	for _, g := range in.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			writeJSON(w, http.StatusBadRequest, oauthErr("invalid_client_metadata", "unsupported grant_type: "+g))
			return
		}
	}
	for _, rt := range in.ResponseTypes {
		if rt != "code" {
			writeJSON(w, http.StatusBadRequest, oauthErr("invalid_client_metadata", "unsupported response_type: "+rt))
			return
		}
	}
	uris, err := validRedirectURIs(in.RedirectURIs)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, oauthErr("invalid_redirect_uri", err.Error()))
		return
	}
	key, err := domain.GenerateAuthCode(s.rand) // 只借用随机串形态
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, oauthErr("server_error", "failed to generate client_id"))
		return
	}
	key = DCRClientPrefix + strings.TrimPrefix(key, domain.AuthCodePrefix)
	name := clientName(in.ClientName)
	if name == "" {
		name = "未命名客户端"
	}
	meta, _ := json.Marshal(map[string]any{
		"client_uri": in.ClientURI, "logo_uri": in.LogoURI,
		"software_id": in.SoftwareID, "software_version": in.SoftwareVersion, "registered_ip": clientIP(r),
	})
	c, err := s.o.Store.CreateDCRClient(r.Context(), repo.Client{ID: s.o.NextID(), Key: key, Name: name,
		RedirectURIs: uris, Metadata: meta})
	if err != nil {
		s.log.Error("DCR 注册写库失败", "error", err)
		writeJSON(w, http.StatusInternalServerError, oauthErr("server_error", "failed to register client"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  c.Key,
		"client_id_issued_at":        c.CreatedAt.Unix(),
		"client_name":                c.Name,
		"redirect_uris":              c.RedirectURIs,
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	})
}

// clientName 规范化客户端名：去空白、按列宽截断（按字符不按字节）。
func clientName(s string) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > maxClientNameRunes {
		s = string([]rune(s)[:maxClientNameRunes])
	}
	return s
}

// clientIP 取请求方 IP（cmd/api 已挂 middleware.RealIP）。⚠️ 只用于限流，**不用于任何授权判断**。
func clientIP(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}
