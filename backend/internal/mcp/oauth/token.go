package oauth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/repo"
)

// 令牌端点（OAuth 2.1 §3.2）与撤销端点（RFC 7009）。
//
// 令牌是不透明随机串（mcp_at_ / mcp_rt_ 前缀），库里**只存 sha256**；响应 `Cache-Control: no-store`。
// 错误按 RFC 6749 §5.2 的 JSON 形态回（不是本平台的 Result 信封）：客户端库只认那个形态。

// tokenResp 是令牌响应。
type tokenResp struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

func oauthErr(code, desc string) map[string]string {
	return map[string]string{"error": code, "error_description": desc}
}

func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
}

// parseForm 读 application/x-www-form-urlencoded 的请求体（上限 64 KiB）。只读 body 不读 query：
// 授权码、verifier、刷新令牌出现在 URL 里会进访问日志与浏览器历史。
func parseForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		noStore(w)
		writeJSON(w, http.StatusBadRequest, oauthErr("invalid_request", "body must be application/x-www-form-urlencoded"))
		return false
	}
	return true
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	noStore(w)
	var (
		resp tokenResp
		code string
		desc string
	)
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		resp, code, desc = s.exchangeCode(r)
	case "refresh_token":
		resp, code, desc = s.refresh(r)
	default:
		code, desc = "unsupported_grant_type", "grant_type must be authorization_code or refresh_token"
	}
	if code != "" {
		status := http.StatusBadRequest
		if code == "server_error" {
			status = http.StatusInternalServerError
		}
		writeJSON(w, status, oauthErr(code, desc))
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// exchangeCode 兑换授权码。
//
// ⚠️ **先 CAS 标记已用，再比对其余参数**：比对失败也要把码消耗掉 —— 否则拿着偷来的码的人可以对着
// code_verifier 反复试。代价是合法客户端若参数写错要重新授权，这是 OAuth 2.1 的标准取舍。
//
// ⚠️ **重放**（同一个码第二次出现）意味着码泄漏了：撤销该 grant 下已签发的全部令牌
// （第一次兑换的可能就是攻击者），并拒绝本次。
func (s *Server) exchangeCode(r *http.Request) (tokenResp, string, string) {
	ctx := r.Context()
	f := r.PostForm
	raw := f.Get("code")
	if !domain.IsAuthCode(raw) {
		return tokenResp{}, "invalid_grant", "invalid authorization code"
	}
	hash := domain.HashKey(raw)
	ac, err := s.o.Store.GetAuthCode(ctx, hash)
	if errors.Is(err, repo.ErrNotFound) {
		return tokenResp{}, "invalid_grant", "invalid authorization code"
	}
	if err != nil {
		return s.serverError("查授权码失败", err)
	}
	if ac.UsedAt != nil {
		s.codeReplay(ctx, ac)
		return tokenResp{}, "invalid_grant", "authorization code has already been used"
	}
	if !s.now().Before(ac.ExpiresAt) {
		return tokenResp{}, "invalid_grant", "authorization code has expired"
	}
	consumed, err := s.o.Store.ConsumeAuthCode(ctx, hash)
	if err != nil {
		return s.serverError("标记授权码已用失败", err)
	}
	if !consumed { // 并发兑换输了：另一份请求刚刚用掉了它，同样按重放处理
		s.codeReplay(ctx, ac)
		return tokenResp{}, "invalid_grant", "authorization code has already been used"
	}
	c, err := s.o.Store.GetClient(ctx, ac.ClientID)
	if err != nil || c.Key != f.Get("client_id") {
		return tokenResp{}, "invalid_grant", "authorization code was issued to another client"
	}
	if f.Get("redirect_uri") != ac.RedirectURI {
		return tokenResp{}, "invalid_grant", "redirect_uri does not match the authorization request"
	}
	if res := f.Get("resource"); res != "" {
		if canon, err := CanonicalResource(res); err != nil || canon != ac.Resource {
			return tokenResp{}, "invalid_target", "resource does not match the authorization request"
		}
	}
	if !VerifyS256(f.Get("code_verifier"), ac.CodeChallenge) {
		return tokenResp{}, "invalid_grant", "PKCE verification failed"
	}
	g, code, desc := s.activeGrant(ctx, ac.GrantID)
	if code != "" {
		return tokenResp{}, code, desc
	}
	resp, toks, _, err := s.mint(g, ac.Resource)
	if err != nil {
		return s.serverError("生成令牌失败", err)
	}
	if err := s.o.Store.IssueTokens(ctx, g.ID, toks[1].ID, toks); err != nil {
		return s.serverError("写令牌失败", err)
	}
	return resp, "", ""
}

func (s *Server) codeReplay(ctx context.Context, ac repo.AuthCode) {
	n, err := s.o.Store.RevokeGrantTokens(ctx, ac.GrantID)
	s.log.Warn("MCP OAuth 授权码重放：已撤销该授权下的全部令牌", "grantId", ac.GrantID, "revoked", n, "error", err)
}

// refresh 用刷新令牌换一对新的，并**轮换**（旧的作废、replaced_by 指向新的）。
//
// 被轮换掉的刷新令牌再次出现：
//   - 宽限期（30 秒，domain.RefreshReuseWindow）内 —— 多半是「服务端轮换了、响应没回到客户端」，
//     只回 invalid_grant，**不连坐**（否则一次网络抖动就要用户重新授权）；
//   - 超过宽限期 —— 视为泄漏（RFC 9700 §4.14.2），**撤销整条链**：攻击者与合法客户端此刻谁拿着
//     最新的那一枚我们分不出来，只能都作废，让用户重新授权一次。
func (s *Server) refresh(r *http.Request) (tokenResp, string, string) {
	ctx := r.Context()
	f := r.PostForm
	raw := f.Get("refresh_token")
	if !domain.IsRefreshToken(raw) {
		return tokenResp{}, "invalid_grant", "invalid refresh token"
	}
	t, err := s.o.Store.FindToken(ctx, domain.HashKey(raw))
	if errors.Is(err, repo.ErrNotFound) || (err == nil && t.Kind != repo.TokenRefresh) {
		return tokenResp{}, "invalid_grant", "invalid refresh token"
	}
	if err != nil {
		return s.serverError("查刷新令牌失败", err)
	}
	c, err := s.o.Store.GetClient(ctx, t.ClientID)
	if err != nil || c.Key != f.Get("client_id") {
		return tokenResp{}, "invalid_grant", "refresh token was issued to another client"
	}
	now := s.now()
	if t.ReplacedBy != nil && t.RevokedAt != nil {
		if now.Sub(*t.RevokedAt) <= domain.RefreshReuseWindow {
			return tokenResp{}, "invalid_grant", "refresh token has already been rotated"
		}
		n, err := s.o.Store.RevokeFamily(ctx, t.FamilyID)
		s.log.Warn("MCP OAuth 刷新令牌重放（超过宽限期）：已撤销整条链", "grantId", t.GrantID,
			"familyId", t.FamilyID, "revoked", n, "error", err)
		return tokenResp{}, "invalid_grant", "refresh token reuse detected; the authorization has been revoked"
	}
	if t.RevokedAt != nil {
		return tokenResp{}, "invalid_grant", "refresh token has been revoked"
	}
	if !now.Before(t.ExpiresAt) {
		return tokenResp{}, "invalid_grant", "refresh token has expired"
	}
	if res := f.Get("resource"); res != "" {
		if canon, err := CanonicalResource(res); err != nil || canon != t.Audience {
			return tokenResp{}, "invalid_target", "resource must equal the original audience " + t.Audience
		}
	}
	for _, sc := range strings.Fields(f.Get("scope")) {
		if !strings.Contains(" "+strings.Join(t.Grant.Scopes, " ")+" ", " "+sc+" ") {
			return tokenResp{}, "invalid_scope", "scope exceeds the original grant: " + sc
		}
	}
	g, code, desc := s.activeGrant(ctx, t.GrantID)
	if code != "" {
		return tokenResp{}, code, desc
	}
	resp, toks, refreshID, err := s.mint(g, t.Audience)
	if err != nil {
		return s.serverError("生成令牌失败", err)
	}
	ok, err := s.o.Store.RotateRefresh(ctx, t.ID, g.ID, t.FamilyID, refreshID, toks)
	if err != nil {
		return s.serverError("轮换刷新令牌失败", err)
	}
	if !ok { // 并发刷新输了：它刚被另一份请求轮换掉（落在宽限期内，不连坐）
		return tokenResp{}, "invalid_grant", "refresh token has already been rotated"
	}
	return resp, "", ""
}

// activeGrant 取授权并核对：授权 active、账号未封禁。
// ⚠️ 账号状态在这里也要查：不查的话封禁之后他手里的刷新令牌照样能换出新的访问令牌
// （访问令牌那一侧 Resolver 也会拦，但不该给他签出来）。
func (s *Server) activeGrant(ctx context.Context, grantID int64) (repo.Grant, string, string) {
	g, err := s.o.Store.GetGrantByID(ctx, grantID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return repo.Grant{}, "invalid_grant", "authorization not found"
		}
		_, c, d := s.serverError("查授权失败", err)
		return repo.Grant{}, c, d
	}
	if g.Status != repo.GrantActive {
		return repo.Grant{}, "invalid_grant", "the authorization has been revoked"
	}
	if s.o.Users != nil {
		active, err := s.o.Users.Active(ctx, g.UserID)
		if err != nil || !active {
			return repo.Grant{}, "invalid_grant", "the user account is not available"
		}
	}
	return g, "", ""
}

// mint 生成一对令牌（明文只出现在响应里）。toks[0] 是访问令牌、toks[1] 是刷新令牌；
// 返回的 refreshID 即 toks[1].ID（授权码兑换时它同时是整条链的 family_id）。
func (s *Server) mint(g repo.Grant, audience string) (tokenResp, []repo.NewToken, int64, error) {
	at, err := domain.GenerateAccessToken(s.rand)
	if err != nil {
		return tokenResp{}, nil, 0, err
	}
	rt, err := domain.GenerateRefreshToken(s.rand)
	if err != nil {
		return tokenResp{}, nil, 0, err
	}
	now := s.now()
	refreshID := s.o.NextID()
	toks := []repo.NewToken{
		{ID: s.o.NextID(), Kind: repo.TokenAccess, Hash: domain.HashKey(at), Audience: audience,
			ExpiresAt: now.Add(domain.AccessTokenTTL)},
		{ID: refreshID, Kind: repo.TokenRefresh, Hash: domain.HashKey(rt), Audience: audience,
			ExpiresAt: now.Add(domain.RefreshTokenTTL)},
	}
	return tokenResp{AccessToken: at, TokenType: "Bearer", ExpiresIn: int64(domain.AccessTokenTTL.Seconds()),
		RefreshToken: rt, Scope: strings.Join(g.Scopes, " ")}, toks, refreshID, nil
}

func (s *Server) serverError(what string, err error) (tokenResp, string, string) {
	s.log.Error("MCP OAuth 令牌端点："+what, "error", err)
	return tokenResp{}, "server_error", "internal error"
}

// revoke 是 RFC 7009 撤销。刷新令牌 → 撤销整条链（含由它换出的访问令牌）；访问令牌 → 只撤它自己。
// **一律回 200**（含不认识的令牌、client_id 对不上的）：区分开等于提供一个「这个令牌存不存在」的探测接口。
func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	noStore(w)
	raw := r.PostForm.Get("token")
	if raw == "" {
		writeJSON(w, http.StatusBadRequest, oauthErr("invalid_request", "token is required"))
		return
	}
	if !domain.IsAccessToken(raw) && !domain.IsRefreshToken(raw) {
		w.WriteHeader(http.StatusOK)
		return
	}
	ctx := r.Context()
	t, err := s.o.Store.FindToken(ctx, domain.HashKey(raw))
	if err != nil {
		if !errors.Is(err, repo.ErrNotFound) {
			s.log.Error("MCP OAuth 撤销：查令牌失败", "error", err)
			writeJSON(w, http.StatusServiceUnavailable, oauthErr("temporarily_unavailable", "try again later"))
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	if c, err := s.o.Store.GetClient(ctx, t.ClientID); err != nil || c.Key != r.PostForm.Get("client_id") {
		w.WriteHeader(http.StatusOK)
		return
	}
	if t.Kind == repo.TokenRefresh {
		_, err = s.o.Store.RevokeFamily(ctx, t.FamilyID)
	} else {
		err = s.o.Store.RevokeToken(ctx, t.ID)
	}
	if err != nil {
		s.log.Error("MCP OAuth 撤销失败", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, oauthErr("temporarily_unavailable", "try again later"))
		return
	}
	w.WriteHeader(http.StatusOK)
}
