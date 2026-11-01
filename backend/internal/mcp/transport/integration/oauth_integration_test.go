//go:build integration

// 对外 MCP 的 OAuth 2.1 授权服务器（MCPT-12）的真库 + httptest 测试。
//
// ⚠️ 走真实路由：/oauth/*、/.well-known/*、/api/v1/mcp/consent（**真实的** identity RequireUser）与 /mcp。
// 授权码一次性、重放连坐、刷新轮换、受众绑定这些都是「两次请求之间」的性质，绕开路由调函数的测试
// 把 CAS 或受众比对整条删掉照样绿。
package integration

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	billingapp "github.com/li904338203/tabtv/backend/internal/billing/app"
	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	collabapp "github.com/li904338203/tabtv/backend/internal/collaboration/app"
	collabrepo "github.com/li904338203/tabtv/backend/internal/collaboration/repo"
	identityapp "github.com/li904338203/tabtv/backend/internal/identity/app"
	identityrepo "github.com/li904338203/tabtv/backend/internal/identity/repo"
	"github.com/li904338203/tabtv/backend/internal/identity/token"
	identityhttp "github.com/li904338203/tabtv/backend/internal/identity/transport"
	mcpapp "github.com/li904338203/tabtv/backend/internal/mcp/app"
	mcpoauth "github.com/li904338203/tabtv/backend/internal/mcp/oauth"
	mcprepo "github.com/li904338203/tabtv/backend/internal/mcp/repo"
	mcphttp "github.com/li904338203/tabtv/backend/internal/mcp/transport"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

const webURL = "http://web.test"

type oenv struct {
	t      *testing.T
	pool   *pgxpool.Pool
	srv    *httptest.Server
	oauth  *mcpoauth.Server
	tokens *token.Service
	ws     int64
	owner  int64
	mate   int64
	nobody int64
}

func newOAuthEnv(t *testing.T, opts ...func(*mcpoauth.Options)) *oenv {
	t.Helper()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	next := gen.Next
	ctx := context.Background()
	e := &oenv{t: t, pool: pool}
	e.owner = mkUser(t, pool, next, "负责人", "13900000021")
	e.mate = mkUser(t, pool, next, "同事", "13900000022")
	e.nobody = mkUser(t, pool, next, "外人", "13900000023")
	cr := collabrepo.New(pool)
	w, err := cr.CreateTeam(ctx, [3]int64{next(), next(), next()}, "制作组", e.owner)
	if err != nil {
		t.Fatal(err)
	}
	e.ws = w.ID
	if _, err := pool.Exec(ctx, `insert into workspace_member (id, workspace_id, user_id, role)
		values ($1,$2,$3,'editor')`, next(), e.ws, e.mate); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	collab := collabapp.New(cr, next)
	identity := identityapp.New(identityrepo.New(pool), nil, nil, nil, next)
	billing := billingapp.New(billingrepo.New(pool, next))
	store := mcprepo.New(pool)
	members := wiring.MCPMembers{Svc: collab}
	users := wiring.MCPUsers{Svc: identity}
	resolver := mcpapp.NewResolver(store, users, members, billing, logger)
	t.Cleanup(resolver.Flush)
	e.tokens, err = token.New(token.DefaultConfig([]byte(strings.Repeat("k", 32))))
	if err != nil {
		t.Fatal(err)
	}
	idh := identityhttp.NewHandler(identity, e.tokens)
	auditLog := audit.New(pool, next, logger)
	oo := mcpoauth.Options{
		Store: store, Users: users, Members: members, Audit: auditLog, NextID: next,
		WebPublicURL: webURL, RequestKey: mcpoauth.DeriveRequestKey(strings.Repeat("s", 32)),
		UserIDs: identityhttp.UserIDFrom, Logger: logger,
	}
	for _, f := range opts {
		f(&oo)
	}
	e.oauth = mcpoauth.New(oo)
	keys := mcpapp.NewKeys(store, members, auditLog, next)

	r := chi.NewRouter()
	r.Use(httpx.TraceID)
	r.Route("/api/v1", func(r chi.Router) {
		mcphttp.NewKeysHandler(keys, identityhttp.UserIDFrom).Mount(r, idh.RequireUser)
		e.oauth.MountConsent(r, idh.RequireUser)
	})
	mcphttp.NewHandler(mcphttp.Options{Registry: registry(), Auth: mcphttp.KeyAuth{Resolver: resolver}, Logger: logger}).Mount(r)
	e.oauth.MountRoot(r)
	e.srv = httptest.NewServer(r)
	t.Cleanup(e.srv.Close)
	return e
}

var noFollow = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (e *oenv) jwt(uid int64) string {
	e.t.Helper()
	is, err := e.tokens.IssueAccess(uid, token.KindUser, 0, "a-"+strconv.FormatInt(uid, 10), "r-"+strconv.FormatInt(uid, 10))
	if err != nil {
		e.t.Fatal(err)
	}
	return is.Token
}

// register 走 DCR 注册一个回调在回环地址上的公共客户端。
func (e *oenv) register(redirect string) string {
	e.t.Helper()
	resp, err := http.Post(e.srv.URL+"/oauth/register", "application/json", strings.NewReader(
		`{"client_name":"Test Client","redirect_uris":["`+redirect+`"],"token_endpoint_auth_method":"none"}`))
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 201 {
		e.t.Fatalf("注册：%d %v", resp.StatusCode, out)
	}
	return out["client_id"].(string)
}

type pkce struct{ verifier, challenge string }

func newPKCE() pkce {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	v := base64.RawURLEncoding.EncodeToString(b)
	return pkce{v, mcpoauth.ChallengeS256(v)}
}

func (e *oenv) resource(path string) string { return e.srv.URL + path }

// authorize 打 /oauth/authorize（不跟随跳转）。
func (e *oenv) authorize(q url.Values) *http.Response {
	e.t.Helper()
	resp, err := noFollow.Get(e.srv.URL + "/oauth/authorize?" + q.Encode())
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func authQuery(clientID, redirect string, p pkce, resource string) url.Values {
	return url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect},
		"code_challenge": {p.challenge}, "code_challenge_method": {"S256"}, "resource": {resource},
		"scope": {"seeaitv:view seeaitv:edit seeaitv:generate"}, "state": {"st-1"}}
}

// reqHandle 从授权端点跳往同意页的地址里取出 req 句柄。
func (e *oenv) reqHandle(resp *http.Response) string {
	e.t.Helper()
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(loc, webURL+"/mcp/authorize?req=") {
		b, _ := io.ReadAll(resp.Body)
		e.t.Fatalf("授权端点应跳到同意页：%d %q %s", resp.StatusCode, loc, b)
	}
	u, _ := url.Parse(loc)
	return u.Query().Get("req")
}

func (e *oenv) api(method, path, bearer string, body any) (int, map[string]any) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, e.srv.URL+"/api/v1"+path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// consent 以 uid 身份允许，返回回调地址里的 code（并核对 state=st-1 与 iss）。
func (e *oenv) consent(uid int64, handle string, extra map[string]any) string {
	e.t.Helper()
	u := e.consentURL(uid, handle, extra)
	if u.Query().Get("state") != "st-1" {
		e.t.Fatalf("回跳地址的 state 不对：%s", u)
	}
	return u.Query().Get("code")
}

// consentURL 以 uid 身份允许，返回回调地址（核对 iss）。
func (e *oenv) consentURL(uid int64, handle string, extra map[string]any) *url.URL {
	e.t.Helper()
	body := map[string]any{"req": handle, "decision": "allow", "workspaceId": strconv.FormatInt(e.ws, 10)}
	for k, v := range extra {
		body[k] = v
	}
	code, out := e.api(http.MethodPost, "/mcp/consent", e.jwt(uid), body)
	if code != 200 {
		e.t.Fatalf("同意：%d %v", code, out)
	}
	u, _ := url.Parse(out["redirectTo"].(string))
	if u.Query().Get("iss") != e.srv.URL || u.Query().Get("code") == "" {
		e.t.Fatalf("回跳地址缺 code / iss：%s", u)
	}
	return u
}

func (e *oenv) token(form url.Values) (int, map[string]any) {
	e.t.Helper()
	resp, err := http.PostForm(e.srv.URL+"/oauth/token", form)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Cache-Control") != "no-store" {
		e.t.Fatalf("令牌响应必须 no-store：%q", resp.Header.Get("Cache-Control"))
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func codeForm(clientID, redirect, code string, p pkce) url.Values {
	return url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "redirect_uri": {redirect},
		"code": {code}, "code_verifier": {p.verifier}}
}

func refreshForm(clientID, rt string) url.Values {
	return url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {rt}}
}

type flow struct {
	clientID, redirect string
	access, refresh    string
}

// fullFlow 走完一次「注册 → 授权 → 同意 → 兑换」，令牌受众为 path 对应的端点。
func (e *oenv) fullFlow(uid int64, path string) flow {
	e.t.Helper()
	redirect := "http://127.0.0.1:33418/callback"
	cid := e.register(redirect)
	p := newPKCE()
	code := e.consent(uid, e.reqHandle(e.authorize(authQuery(cid, redirect, p, e.resource(path)))), nil)
	st, out := e.token(codeForm(cid, redirect, code, p))
	if st != 200 || out["token_type"] != "Bearer" {
		e.t.Fatalf("兑换：%d %v", st, out)
	}
	return flow{clientID: cid, redirect: redirect, access: out["access_token"].(string), refresh: out["refresh_token"].(string)}
}

// mcpInit 以 bearer 打一次 initialize，返回状态码与 WWW-Authenticate。
func (e *oenv) mcpInit(path, bearer string) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+path, strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"raw","version":"0"}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header.Get("WWW-Authenticate")
}

// ---------------------------------------------------------------- 用例

func TestOAuth_完整流程与库里只有哈希(t *testing.T) {
	e := newOAuthEnv(t)
	f := e.fullFlow(e.mate, "/mcp")
	if code, _ := e.mcpInit("/mcp", f.access); code != 200 {
		t.Fatalf("访问令牌打 /mcp：%d", code)
	}
	cs := connect(t, e.srv.URL+"/mcp", "2026-07-28", f.access)
	if got := toolNames(t, cs); len(got) != 3 {
		t.Fatalf("editor + view/edit/generate 应看到三个工具：%v", got)
	}
	// 库里查不到任何令牌、授权码明文（MCP-N-01）。
	var n int
	if err := e.pool.QueryRow(context.Background(), `
		select count(*) from mcp_token t
		where position(convert_to($1,'UTF8') in t.token_hash) > 0 or position(convert_to($2,'UTF8') in t.token_hash) > 0
		   or encode(t.token_hash,'escape') like '%mcp_%'`, f.access, f.refresh).Scan(&n); err != nil || n != 0 {
		t.Fatalf("令牌表里出现了明文：%d %v", n, err)
	}
	var dump string
	_ = e.pool.QueryRow(context.Background(), `select coalesce(string_agg(t::text, '|'),'') from (
		select row_to_json(x)::text as t from mcp_token x union all select row_to_json(y)::text from mcp_auth_code y
		union all select row_to_json(z)::text from mcp_grant z) s`).Scan(&dump)
	for _, secret := range []string{f.access, f.refresh, "mcp_at_", "mcp_rt_", "mcp_ac_"} {
		if strings.Contains(dump, secret) {
			t.Fatalf("库里出现了令牌 / 授权码明文：%s", secret)
		}
	}
	// grant：source=oauth、client_id 非空。
	var src string
	var clientID *int64
	_ = e.pool.QueryRow(context.Background(), `select source, client_id from mcp_grant where user_id=$1`, e.mate).Scan(&src, &clientID)
	if src != "oauth" || clientID == nil {
		t.Fatalf("grant：source=%s client_id=%v", src, clientID)
	}
	var reg string
	_ = e.pool.QueryRow(context.Background(), `select registration from mcp_client where client_key=$1`, f.clientID).Scan(&reg)
	if reg != "dcr" {
		t.Fatalf("DCR 注册的客户端应标 dcr：%s", reg)
	}
}

func TestOAuth_RedirectURIMismatch_NoRedirect(t *testing.T) {
	e := newOAuthEnv(t)
	cid := e.register("http://127.0.0.1:33418/callback")
	p := newPKCE()
	for _, bad := range []string{"https://evil.example/cb", "http://127.0.0.1:33418/other"} {
		resp := e.authorize(authQuery(cid, bad, p, e.resource("/mcp")))
		if resp.StatusCode != 400 || resp.Header.Get("Location") != "" ||
			!strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
			t.Fatalf("%s：%d Location=%q", bad, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	// client_id 不认识：同样不回跳。
	resp := e.authorize(authQuery("mcpc_nope", "http://127.0.0.1:1/cb", p, e.resource("/mcp")))
	if resp.StatusCode != 400 || resp.Header.Get("Location") != "" {
		t.Fatalf("未知客户端：%d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// 回环端口可变：照常跳同意页。
	e.reqHandle(e.authorize(authQuery(cid, "http://127.0.0.1:51000/callback", p, e.resource("/mcp"))))
}

// 其余错误按规范带 error 回跳（client 与 redirect_uri 已验证）：缺 PKCE、plain、resource 不是本服务、未知 scope。
func TestOAuth_授权请求错误带error回跳(t *testing.T) {
	e := newOAuthEnv(t)
	redirect := "http://127.0.0.1:33418/callback"
	cid := e.register(redirect)
	p := newPKCE()
	cases := map[string]func(url.Values){
		"invalid_request/缺PKCE":           func(q url.Values) { q.Del("code_challenge"); q.Del("code_challenge_method") },
		"invalid_request/plain":           func(q url.Values) { q.Set("code_challenge_method", "plain") },
		"invalid_target/别的资源":             func(q url.Values) { q.Set("resource", "https://other.example/mcp") },
		"invalid_target/缺resource":        func(q url.Values) { q.Del("resource") },
		"invalid_scope/未知":                func(q url.Values) { q.Set("scope", "seeaitv:view admin") },
		"unsupported_response_type/token": func(q url.Values) { q.Set("response_type", "token") },
	}
	for name, mut := range cases {
		q := authQuery(cid, redirect, p, e.resource("/mcp"))
		mut(q)
		resp := e.authorize(q)
		u, _ := url.Parse(resp.Header.Get("Location"))
		want, _, _ := strings.Cut(name, "/")
		if resp.StatusCode != 302 || !strings.HasPrefix(u.String(), redirect) || u.Query().Get("error") != want ||
			u.Query().Get("state") != "st-1" {
			t.Errorf("%s：%d %s", name, resp.StatusCode, u)
		}
	}
}

func TestOAuth_CodeReplay_RevokesGrantTokens(t *testing.T) {
	e := newOAuthEnv(t)
	redirect := "http://127.0.0.1:33418/callback"
	cid := e.register(redirect)
	p := newPKCE()
	code := e.consent(e.mate, e.reqHandle(e.authorize(authQuery(cid, redirect, p, e.resource("/mcp")))), nil)
	st, out := e.token(codeForm(cid, redirect, code, p))
	if st != 200 {
		t.Fatalf("第一次兑换：%d %v", st, out)
	}
	access, refresh := out["access_token"].(string), out["refresh_token"].(string)
	if c, _ := e.mcpInit("/mcp", access); c != 200 {
		t.Fatalf("重放前应能用：%d", c)
	}
	st, out = e.token(codeForm(cid, redirect, code, p))
	if st != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("重放应 invalid_grant：%d %v", st, out)
	}
	if c, _ := e.mcpInit("/mcp", access); c != 401 {
		t.Fatalf("重放后第一次签发的令牌也应作废：%d", c)
	}
	if st, out := e.token(refreshForm(cid, refresh)); st != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("重放后第一次签发的刷新令牌也应作废：%d %v", st, out)
	}
}

func TestOAuth_PKCEVerifierWrong(t *testing.T) {
	e := newOAuthEnv(t)
	redirect := "http://127.0.0.1:33418/callback"
	cid := e.register(redirect)
	p := newPKCE()
	code := e.consent(e.mate, e.reqHandle(e.authorize(authQuery(cid, redirect, p, e.resource("/mcp")))), nil)
	st, out := e.token(codeForm(cid, redirect, code, newPKCE()))
	if st != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("错误的 verifier：%d %v", st, out)
	}
	// 码已被消耗：即使随后给对 verifier 也不行（防止对着 verifier 反复试）。
	if st, out = e.token(codeForm(cid, redirect, code, p)); st != 400 {
		t.Fatalf("消耗过的码：%d %v", st, out)
	}
	// redirect_uri 不一致、client_id 不一致同样拒。
	code = e.consent(e.mate, e.reqHandle(e.authorize(authQuery(cid, redirect, p, e.resource("/mcp")))), nil)
	if st, out = e.token(codeForm(cid, "http://127.0.0.1:33418/other", code, p)); st != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("redirect_uri 不一致：%d %v", st, out)
	}
}

func TestOAuth_AudienceMismatch_401(t *testing.T) {
	e := newOAuthEnv(t)
	f := e.fullFlow(e.mate, "/mcp")
	for _, p := range []string{"/mcp/readonly", "/mcp/x/canvas"} {
		code, www := e.mcpInit(p, f.access)
		if code != 401 || !strings.Contains(www, `resource_metadata="`+e.srv.URL+"/.well-known/oauth-protected-resource"+p+`"`) {
			t.Fatalf("%s 用 /mcp 的令牌：%d %q", p, code, www)
		}
	}
	ro := e.fullFlow(e.mate, "/mcp/readonly")
	if code, _ := e.mcpInit("/mcp/readonly", ro.access); code != 200 {
		t.Fatalf("/mcp/readonly 的令牌打自己：%d", code)
	}
	if code, _ := e.mcpInit("/mcp", ro.access); code != 401 {
		t.Fatalf("/mcp/readonly 的令牌打 /mcp：%d", code)
	}
	// 刷新时给出不同的 resource 被拒。
	form := refreshForm(f.clientID, f.refresh)
	form.Set("resource", e.resource("/mcp/readonly"))
	if st, out := e.token(form); st != 400 || out["error"] != "invalid_target" {
		t.Fatalf("刷新换受众：%d %v", st, out)
	}
}

func TestOAuth_RefreshAfterRevoke_Fails(t *testing.T) {
	e := newOAuthEnv(t)
	f := e.fullFlow(e.mate, "/mcp")
	// 刷新每次轮换：新的与旧的不同。
	st, out := e.token(refreshForm(f.clientID, f.refresh))
	if st != 200 || out["refresh_token"] == f.refresh {
		t.Fatalf("刷新：%d %v", st, out)
	}
	rt := out["refresh_token"].(string)
	access := out["access_token"].(string)
	// 用户在「已连接的客户端」里撤销这条授权。
	_, grants := e.listGrants(e.mate)
	gid := grants[0]["id"].(string)
	if code, out := e.api(http.MethodDelete, "/me/mcp/grants/"+gid, e.jwt(e.mate), nil); code != 204 {
		t.Fatalf("撤销授权：%d %v", code, out)
	}
	if st, out := e.token(refreshForm(f.clientID, rt)); st != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("撤销后刷新：%d %v", st, out)
	}
	if code, _ := e.mcpInit("/mcp", access); code != 401 {
		t.Fatalf("撤销后访问令牌：%d", code)
	}
}

func (e *oenv) listGrants(uid int64) (int, []map[string]any) {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/me/mcp/grants", nil)
	req.Header.Set("Authorization", "Bearer "+e.jwt(uid))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestOAuth_RefreshRotation_OldTokenReuseRevokesChain(t *testing.T) {
	e := newOAuthEnv(t)
	f := e.fullFlow(e.mate, "/mcp")
	st, out := e.token(refreshForm(f.clientID, f.refresh))
	if st != 200 {
		t.Fatalf("刷新：%d %v", st, out)
	}
	newRT, newAT := out["refresh_token"].(string), out["access_token"].(string)
	// 超过 30 秒宽限期后，被轮换掉的旧刷新令牌又出现 → 视为泄漏，整条链作废。
	e.oauth.SetClock(func() time.Time { return time.Now().Add(time.Minute) })
	if st, out := e.token(refreshForm(f.clientID, f.refresh)); st != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("旧刷新令牌重放：%d %v", st, out)
	}
	e.oauth.SetClock(time.Now)
	if st, out := e.token(refreshForm(f.clientID, newRT)); st != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("链被撤销后新刷新令牌也应失效：%d %v", st, out)
	}
	if code, _ := e.mcpInit("/mcp", newAT); code != 401 {
		t.Fatalf("链被撤销后新访问令牌也应失效：%d", code)
	}
}

func TestOAuth_RefreshReuseWithinGrace_NoChainRevoke(t *testing.T) {
	e := newOAuthEnv(t)
	f := e.fullFlow(e.mate, "/mcp")
	st, out := e.token(refreshForm(f.clientID, f.refresh))
	if st != 200 {
		t.Fatalf("刷新：%d %v", st, out)
	}
	newRT := out["refresh_token"].(string)
	// 宽限期内（服务端轮换了、响应没回到客户端）：只回 invalid_grant，不连坐。
	if st, out := e.token(refreshForm(f.clientID, f.refresh)); st != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("宽限期内重放：%d %v", st, out)
	}
	if st, out := e.token(refreshForm(f.clientID, newRT)); st != 200 {
		t.Fatalf("宽限期内重放不该撤销整条链：%d %v", st, out)
	}
	// 别的客户端拿它来刷新：拒。
	other := e.register("http://127.0.0.1:1/cb")
	if st, _ := e.token(refreshForm(other, newRT)); st != 400 {
		t.Fatalf("别的客户端刷新：%d", st)
	}
}

func TestOAuth_UserJWTRejectedOnMCP(t *testing.T) {
	e := newOAuthEnv(t)
	if code, www := e.mcpInit("/mcp", e.jwt(e.mate)); code != 401 || !strings.Contains(www, "resource_metadata=") {
		t.Fatalf("用户端 JWT 打 /mcp：%d %q", code, www)
	}
	// 刷新令牌也不能当访问令牌用。
	f := e.fullFlow(e.mate, "/mcp")
	if code, _ := e.mcpInit("/mcp", f.refresh); code != 401 {
		t.Fatalf("刷新令牌打 /mcp：%d", code)
	}
}

func TestOAuth_MCPTokenRejectedOnUserAPI(t *testing.T) {
	e := newOAuthEnv(t)
	f := e.fullFlow(e.mate, "/mcp")
	for _, path := range []string{"/me/mcp/grants", "/mcp/consent?req=x"} {
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1"+path, nil)
		req.Header.Set("Authorization", "Bearer "+f.access)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("mcp_at_ 打用户端 %s：%d", path, resp.StatusCode)
		}
	}
}

// ⚠️ 偏离卡片：封禁账号回 403 ACCOUNT_BANNED（与访问密钥同一段 principalFor），不是 401 ——
// 401 会让 OAuth 客户端自动重走授权，而封禁的账号重新授权也没用。刷新令牌也换不出新的。
func TestOAuth_BannedUser_Rejected(t *testing.T) {
	e := newOAuthEnv(t)
	f := e.fullFlow(e.mate, "/mcp")
	if _, err := e.pool.Exec(context.Background(), `update app_user set status='banned' where id=$1`, e.mate); err != nil {
		t.Fatal(err)
	}
	if code, www := e.mcpInit("/mcp", f.access); code != 403 || www != "" {
		t.Fatalf("封禁后访问令牌：%d %q", code, www)
	}
	if st, out := e.token(refreshForm(f.clientID, f.refresh)); st != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("封禁后刷新：%d %v", st, out)
	}
}

func TestOAuth_ReconsentUpdatesSameGrant(t *testing.T) {
	e := newOAuthEnv(t)
	redirect := "http://127.0.0.1:33418/callback"
	cid := e.register(redirect)
	p := newPKCE()
	e.consent(e.mate, e.reqHandle(e.authorize(authQuery(cid, redirect, p, e.resource("/mcp")))), map[string]any{
		"scopes": []string{"seeaitv:view"}})
	e.consent(e.mate, e.reqHandle(e.authorize(authQuery(cid, redirect, p, e.resource("/mcp")))), map[string]any{
		"scopes": []string{"seeaitv:edit"}, "requirePageConfirm": true})
	var n int
	var scopes []string
	var rpc bool
	if err := e.pool.QueryRow(context.Background(), `select count(*) over (), scopes, require_page_confirm
		from mcp_grant where user_id=$1 and source='oauth' and status='active'`, e.mate).Scan(&n, &scopes, &rpc); err != nil {
		t.Fatal(err)
	}
	if n != 1 || strings.Join(scopes, ",") != "seeaitv:view,seeaitv:edit" || !rpc {
		t.Fatalf("重复授权应更新同一条 grant：n=%d scopes=%v rpc=%v", n, scopes, rpc)
	}
	// 审计：一条 create、一条 update，不含授权码。
	var actions string
	_ = e.pool.QueryRow(context.Background(), `select string_agg(action, ',' order by id) from audit_log
		where action like 'mcp.grant.oauth_%'`).Scan(&actions)
	if actions != "mcp.grant.oauth_create,mcp.grant.oauth_update" {
		t.Fatalf("审计：%s", actions)
	}
}

// 同意接口：非成员 403、勾选超出申请范围 400、句柄被篡改 / 过期 400 MCP_CONSENT_EXPIRED、拒绝回跳 access_denied。
func TestOAuth_同意接口校验(t *testing.T) {
	e := newOAuthEnv(t)
	redirect := "http://127.0.0.1:33418/callback"
	cid := e.register(redirect)
	q := authQuery(cid, redirect, newPKCE(), e.resource("/mcp"))
	q.Set("scope", "seeaitv:view")
	h := e.reqHandle(e.authorize(q))

	code, info := e.api(http.MethodGet, "/mcp/consent?req="+url.QueryEscape(h), e.jwt(e.mate), nil)
	client, _ := info["client"].(map[string]any)
	if code != 200 || client["registration"] != "dcr" || client["verified"] != false || info["redirectHost"] != "127.0.0.1:33418" {
		t.Fatalf("同意页数据：%d %v", code, info)
	}
	if code, out := e.api(http.MethodPost, "/mcp/consent", e.jwt(e.nobody), map[string]any{
		"req": h, "decision": "allow", "workspaceId": strconv.FormatInt(e.ws, 10)}); code != 403 {
		t.Fatalf("非成员同意：%d %v", code, out)
	}
	if code, out := e.api(http.MethodPost, "/mcp/consent", e.jwt(e.mate), map[string]any{
		"req": h, "decision": "allow", "workspaceId": strconv.FormatInt(e.ws, 10), "scopes": []string{"seeaitv:generate"}}); code != 400 {
		t.Fatalf("超出申请范围：%d %v", code, out)
	}
	if code, out := e.api(http.MethodGet, "/mcp/consent?req="+url.QueryEscape(h+"x"), e.jwt(e.mate), nil); code != 400 ||
		out["code"] != float64(40007) {
		t.Fatalf("篡改的句柄：%d %v", code, out)
	}
	e.oauth.SetClock(func() time.Time { return time.Now().Add(11 * time.Minute) })
	if code, out := e.api(http.MethodGet, "/mcp/consent?req="+url.QueryEscape(h), e.jwt(e.mate), nil); code != 400 {
		t.Fatalf("过期的句柄：%d %v", code, out)
	}
	e.oauth.SetClock(time.Now)
	code, out := e.api(http.MethodPost, "/mcp/consent", e.jwt(e.mate), map[string]any{"req": h, "decision": "deny"})
	u, _ := url.Parse(out["redirectTo"].(string))
	if code != 200 || u.Query().Get("error") != "access_denied" || u.Query().Get("code") != "" {
		t.Fatalf("拒绝：%d %v", code, out)
	}
}

// 协议：官方 go-sdk 客户端的 OAuth 流程（PRM 发现 → AS 元数据 → DCR → 授权（模拟浏览器 + 同意页）→ 兑换）
// 走完后调用工具。
func TestOAuth_GoSDK客户端完整流程(t *testing.T) {
	e := newOAuthEnv(t)
	redirect := "http://127.0.0.1:33418/callback"
	uid := e.mate
	fetcher := func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		resp, err := noFollow.Get(args.URL) // 浏览器打开授权地址
		if err != nil {
			return nil, err
		}
		resp.Body.Close()
		u := e.consentURL(uid, e.reqHandle(resp), nil) // 用户在同意页点「允许」
		if u.Query().Get("state") != stateOf(args.URL) {
			return nil, fmt.Errorf("state 未原样回传：%s", u)
		}
		return &auth.AuthorizationResult{Code: u.Query().Get("code"), State: u.Query().Get("state"),
			Iss: u.Query().Get("iss")}, nil
	}
	h, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
			ClientName: "go-sdk", RedirectURIs: []string{redirect}, TokenEndpointAuthMethod: "none",
			GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"},
		}},
		RedirectURL: redirect, AuthorizationCodeFetcher: fetcher,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "mcpt12-it", Version: "0"}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: e.srv.URL + "/mcp", OAuthHandler: h, MaxRetries: -1},
		&mcp.ClientSessionOptions{ProtocolVersion: "2026-07-28"})
	if err != nil {
		t.Fatalf("go-sdk OAuth 连接：%v", err)
	}
	defer cs.Close()
	if got := toolNames(t, cs); len(got) == 0 {
		t.Fatal("OAuth 连上后应列出工具")
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "canvas_read", Arguments: map[string]any{"projectId": "1"}})
	if err != nil || res.IsError {
		t.Fatalf("调用工具：%v %+v", err, res)
	}
}

func stateOf(raw string) string {
	u, _ := url.Parse(raw)
	return u.Query().Get("state")
}

// DCR（兼容）：只收公共客户端、回调 https 或回环 http；同一 IP 按小时限流（真实 Redis）。
func TestOAuth_DCR校验与按IP限流(t *testing.T) {
	rdb, key := newRedis(t)
	e := newOAuthEnv(t, func(o *mcpoauth.Options) {
		o.Limiter = mcpapp.NewLimiter(rdb, key, 0, 0, nil)
		o.DCRPerHour = 7 // 每次注册尝试都计数（含被拒的）：4 次非法 + 3 次合法，第 8 次超限
	})
	post := func(body string) (int, map[string]any) {
		resp, err := http.Post(e.srv.URL+"/oauth/register", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	for name, body := range map[string]string{
		"机密客户端":  `{"redirect_uris":["https://a.example/cb"],"token_endpoint_auth_method":"client_secret_basic"}`,
		"明文http": `{"redirect_uris":["http://a.example/cb"]}`,
		"无回调":    `{"client_name":"x"}`,
		"隐式授权":   `{"redirect_uris":["https://a.example/cb"],"response_types":["token"]}`,
	} {
		if code, out := post(body); code != 400 {
			t.Errorf("%s 应拒：%d %v", name, code, out)
		}
	}
	ok := `{"client_name":"x","redirect_uris":["https://a.example/cb"]}`
	for i := 0; i < 3; i++ {
		if code, out := post(ok); code != 201 || !strings.HasPrefix(out["client_id"].(string), "mcpc_") {
			t.Fatalf("第 %d 次注册：%d %v", i+1, code, out)
		}
	}
	if code, out := post(ok); code != 429 || out["error"] != "too_many_requests" {
		t.Fatalf("超限应 429：%d %v", code, out)
	}
}
