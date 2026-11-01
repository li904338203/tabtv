package oauth

import (
	"encoding/json"
	"errors"
	"html"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// maxStateLen 是 state 的长度上限：它原样进句柄、再原样回到客户端的回调 URL 里。
const maxStateLen = 1024

// authorize 是 GET /oauth/authorize（授权码流程的入口，OAuth 2.1 §4.1）。
//
// 只做校验与跳转（见包注释）。错误分两类，处置不同：
//   - client_id 或 redirect_uri 本身不对 → **不回跳**，显示错误页。回跳的话，任何人都能拿
//     「一个合法的 client_id + 他自己的 redirect_uri」把我们的域名变成一个开放重定向器；
//   - 其余（response_type、PKCE、resource、scope）→ 按规范带 error 回跳到（已验证的）redirect_uri，
//     让客户端知道哪里不对。
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ctx := r.Context()
	client, err := s.resolveClient(ctx, q.Get("client_id"))
	if err != nil {
		errorPage(w, "客户端无效", err.Error())
		return
	}
	redirectURI := q.Get("redirect_uri")
	if redirectURI == "" && len(client.RedirectURIs) == 1 {
		redirectURI = client.RedirectURIs[0]
	}
	if redirectURI == "" || !MatchRedirectURI(client.RedirectURIs, redirectURI) {
		errorPage(w, "回调地址不匹配", "客户端「"+client.Name+"」请求的回调地址与它登记的不一致，授权已中止。"+
			"这可能是客户端配置错误，也可能是有人在冒用它。")
		return
	}
	iss := s.urls.Issuer(r)
	state := q.Get("state")
	fail := func(code, desc string) {
		http.Redirect(w, r, redirectWith(redirectURI, map[string]string{
			"error": code, "error_description": desc, "state": state, "iss": iss}), http.StatusFound)
	}
	if len(state) > maxStateLen {
		fail("invalid_request", "state is too long")
		return
	}
	if q.Get("response_type") != "code" {
		fail("unsupported_response_type", "only response_type=code is supported")
		return
	}
	if q.Get("code_challenge_method") != MethodS256 {
		fail("invalid_request", "PKCE is required: code_challenge_method must be S256")
		return
	}
	challenge := q.Get("code_challenge")
	if !ValidChallenge(challenge) {
		fail("invalid_request", "code_challenge must be a base64url-encoded SHA-256 digest")
		return
	}
	res := q["resource"]
	if len(res) != 1 {
		fail("invalid_target", "exactly one resource parameter is required (RFC 8707)")
		return
	}
	resource, ok := s.urls.CheckResource(r, res[0])
	if !ok {
		fail("invalid_target", "resource must be one of this server's MCP endpoints, e.g. "+s.urls.Resource(r, PathMCP))
		return
	}
	scopes, err := requestedScopes(q.Get("scope"))
	if err != nil {
		fail("invalid_scope", err.Error())
		return
	}
	handle, err := SignRequest(s.o.RequestKey, ConsentRequest{
		ClientID: client.ID, RedirectURI: redirectURI, Scopes: scopes, State: state,
		CodeChallenge: challenge, Resource: resource, Issuer: iss,
		Exp: s.now().Add(domain.ConsentRequestTTL).Unix(),
	})
	if err != nil {
		fail("server_error", "failed to build consent request")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, s.webURL(r)+"/mcp/authorize?req="+url.QueryEscape(handle), http.StatusFound)
}

// requestedScopes 解析授权请求里的 scope（空格分隔）。为空时视为申请全部 —— 用户在同意页上
// 自己挑，最终权限仍是「勾选的 scope ∩ 当前角色」。未知 scope 报错（不静默丢弃）。
func requestedScopes(raw string) ([]string, error) {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return domain.Scopes(), nil
	}
	for _, f := range fields {
		if !domain.IsScope(f) {
			return nil, errors.New("unknown scope: " + f)
		}
	}
	return domain.ParseScopes(fields)
}

// webURL 是用户端地址。未配置时（仅非 production）取请求的主机名 + :3000（本地开发的用户端端口）。
func (s *Server) webURL(r *http.Request) string {
	if s.web != "" {
		return s.web
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return "http://" + host + ":3000"
}

// redirectWith 在回调地址上追加参数（保留它原有的 query），空值不写。
func redirectWith(redirectURI string, params map[string]string) string {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return redirectURI
	}
	q := u.Query()
	for k, v := range params {
		if v != "" {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// errorPage 是不能回跳时的错误页（防开放重定向）。
func errorPage(w http.ResponseWriter, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><title>授权失败 - seeAitv</title>` +
		`<meta name="viewport" content="width=device-width,initial-scale=1"></head>` +
		`<body style="font-family:system-ui,sans-serif;max-width:560px;margin:15vh auto;padding:0 24px;line-height:1.6">` +
		`<h1 style="font-size:20px">授权失败：` + html.EscapeString(title) + `</h1>` +
		`<p>` + html.EscapeString(detail) + `</p>` +
		`<p style="color:#888">请回到你的 AI 客户端重新发起连接；问题持续存在时请联系客户端的提供方。</p></body></html>`))
}

// ---------------------------------------------------------------- 同意（供用户端同意页，MCPT-13）

// consentInfoResp 对应 OpenAPI 的 McpConsentInfo。
type consentInfoResp struct {
	Client struct {
		ClientID     string `json:"clientId"`
		Name         string `json:"name"`
		Registration string `json:"registration"`
		// Verified：CIMD 客户端的身份由域名担保（元数据文档在那个域名上）；DCR 是自报的。
		Verified  bool   `json:"verified"`
		Host      string `json:"host"`
		ClientURI string `json:"clientUri"`
	} `json:"client"`
	RedirectURI     string   `json:"redirectUri"`
	RedirectHost    string   `json:"redirectHost"`
	RequestedScopes []string `json:"requestedScopes"`
	Resource        string   `json:"resource"`
	ReadOnly        bool     `json:"readOnly"`
	Toolset         string   `json:"toolset"`
	ExpiresAt       string   `json:"expiresAt"`
}

// consentInfo 是 GET /api/v1/mcp/consent?req=：同意页要展示的客户端与申请范围。
func (s *Server) consentInfo(w http.ResponseWriter, r *http.Request) {
	req, err := s.parseHandle(r.URL.Query().Get("req"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	c, err := s.o.Store.GetClient(r.Context(), req.ClientID)
	if err != nil {
		httpx.Fail(w, r, s.clientGone(err))
		return
	}
	var out consentInfoResp
	out.Client.ClientID, out.Client.Name, out.Client.Registration = c.Key, c.Name, c.Registration
	out.Client.Verified = c.Registration == repo.RegistrationCIMD
	if c.Registration == repo.RegistrationCIMD {
		if u, err := url.Parse(c.Key); err == nil {
			out.Client.Host = u.Hostname()
		}
	}
	var meta struct {
		ClientURI string `json:"client_uri"`
	}
	_ = json.Unmarshal(c.Metadata, &meta)
	out.Client.ClientURI = meta.ClientURI
	out.RedirectURI = req.RedirectURI
	if u, err := url.Parse(req.RedirectURI); err == nil {
		out.RedirectHost = u.Host
	}
	out.RequestedScopes = req.Scopes
	out.Resource = req.Resource
	out.ReadOnly = strings.HasSuffix(req.Resource, PathReadOnly)
	if i := strings.Index(req.Resource, pathToolset); i >= 0 {
		out.Toolset = req.Resource[i+len(pathToolset):]
	}
	out.ExpiresAt = time.Unix(req.Exp, 0).UTC().Format(time.RFC3339)
	httpx.JSON(w, http.StatusOK, out)
}

// consentReq 对应 OpenAPI 的 McpConsentSubmit。
type consentReq struct {
	Req                string   `json:"req"`
	Decision           string   `json:"decision"`
	WorkspaceID        string   `json:"workspaceId"`
	Scopes             []string `json:"scopes"`
	Toolsets           []string `json:"toolsets"`
	RequirePageConfirm bool     `json:"requirePageConfirm"`
}

// consentSubmit 是 POST /api/v1/mcp/consent：用户在同意页上点「允许」或「拒绝」。
//
//	allow：验句柄 → 校验所选工作区的 active 成员 → upsert grant（同客户端重复授权更新同一条）
//	       → 生成授权码（只存 sha256，5 分钟，一次性）→ redirectTo（含 code、state、iss）
//	deny ：redirectTo 带 error=access_denied
//
// ⚠️ 回跳地址只由服务端给（来自签名句柄里已验证过的 redirect_uri），同意页只做 location.assign。
func (s *Server) consentSubmit(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.o.UserIDs(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.Unauthenticated))
		return
	}
	var in consentReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		httpx.Fail(w, r, httpx.Err(errcode.InvalidJSON))
		return
	}
	req, err := s.parseHandle(in.Req)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	c, err := s.o.Store.GetClient(r.Context(), req.ClientID)
	if err != nil {
		httpx.Fail(w, r, s.clientGone(err))
		return
	}
	switch in.Decision {
	case "deny":
		httpx.JSON(w, http.StatusOK, map[string]string{"redirectTo": redirectWith(req.RedirectURI, map[string]string{
			"error": "access_denied", "error_description": "the user denied the request",
			"state": req.State, "iss": req.Issuer})})
		return
	case "allow":
	default:
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "decision 只能是 allow 或 deny"))
		return
	}
	wsID, err := strconv.ParseInt(in.WorkspaceID, 10, 64)
	if err != nil || wsID <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "workspaceId 非法"))
		return
	}
	scopes, err := grantedScopes(req.Scopes, in.Scopes)
	if err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, err.Error()))
		return
	}
	toolsets, err := domain.ParseToolsets(in.Toolsets)
	if err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, err.Error()))
		return
	}
	// 成员资格实时查（不是 active 成员 → 403 NOT_WORKSPACE_MEMBER，协作域给的原样回）。
	if _, err := s.o.Members.Resolve(r.Context(), uid, wsID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	g, created, err := s.o.Store.UpsertOAuthGrant(r.Context(), repo.Grant{
		ID: s.o.NextID(), UserID: uid, WorkspaceID: wsID, ClientName: c.Name,
		Scopes: scopes, Toolsets: toolsets, RequirePageConfirm: in.RequirePageConfirm,
	}, c.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.Wrap(errcode.Internal, err))
		return
	}
	code, err := domain.GenerateAuthCode(s.rand)
	if err != nil {
		httpx.Fail(w, r, httpx.Wrap(errcode.Internal, err))
		return
	}
	if err := s.o.Store.CreateAuthCode(r.Context(), repo.AuthCode{
		Hash: domain.HashKey(code), GrantID: g.ID, ClientID: c.ID, RedirectURI: req.RedirectURI,
		CodeChallenge: req.CodeChallenge, Resource: req.Resource, ExpiresAt: s.now().Add(domain.AuthCodeTTL),
	}); err != nil {
		httpx.Fail(w, r, httpx.Wrap(errcode.Internal, err))
		return
	}
	if s.o.Audit != nil {
		action := "mcp.grant.oauth_update"
		if created {
			action = "mcp.grant.oauth_create"
		}
		// ⚠️ 审计里没有授权码与令牌（明文与哈希都没有）。
		s.o.Audit.Write(r.Context(), audit.Entry{ActorType: audit.ActorUser, ActorID: uid, WorkspaceID: wsID,
			Action: action, ObjectType: "mcp_grant", ObjectID: g.ID, After: map[string]any{
				"grantId": g.ID, "clientId": c.Key, "registration": c.Registration, "clientName": c.Name,
				"scopes": g.Scopes, "toolsets": g.Toolsets, "requirePageConfirm": g.RequirePageConfirm,
				"resource": req.Resource,
			}})
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"redirectTo": redirectWith(req.RedirectURI, map[string]string{
		"code": code, "state": req.State, "iss": req.Issuer})})
}

// grantedScopes：用户勾选的 scope 必须 ⊆ 客户端申请的；为空时取申请的全部。补 view、定序。
func grantedScopes(requested, chosen []string) ([]string, error) {
	if len(chosen) == 0 {
		return domain.ParseScopes(requested)
	}
	for _, c := range chosen {
		c = strings.TrimSpace(c)
		if c != "" && !slices.Contains(requested, c) {
			return nil, errors.New("scope " + c + " 超出了客户端申请的范围（" + strings.Join(requested, ", ") + "）")
		}
	}
	return domain.ParseScopes(chosen)
}

// parseHandle 验句柄；无效与过期回同一个业务码（前端文案都是「请回到客户端重新连接」）。
func (s *Server) parseHandle(h string) (ConsentRequest, error) {
	req, err := ParseRequest(s.o.RequestKey, h, s.now())
	switch {
	case errors.Is(err, ErrRequestExpired):
		return ConsentRequest{}, httpx.Errf(errcode.McpConsentExpired, "授权请求已过期（有效期 10 分钟），请回到客户端重新连接")
	case err != nil:
		return ConsentRequest{}, httpx.Errf(errcode.McpConsentExpired, "授权请求无效，请回到客户端重新连接")
	}
	return req, nil
}

func (s *Server) clientGone(err error) error {
	if errors.Is(err, repo.ErrNotFound) {
		return httpx.Errf(errcode.McpConsentExpired, "发起授权的客户端已不存在，请回到客户端重新连接")
	}
	return httpx.Wrap(errcode.Internal, err)
}
