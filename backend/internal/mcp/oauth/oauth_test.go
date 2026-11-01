package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ptransport "github.com/li904338203/tabtv/backend/internal/provider/transport"
)

// ---------------------------------------------------------------- PKCE

func TestPKCE_S256(t *testing.T) {
	// RFC 7636 附录 B 的样例。
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	const challenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if ChallengeS256(verifier) != challenge {
		t.Fatalf("ChallengeS256 与 RFC 样例不一致：%s", ChallengeS256(verifier))
	}
	if !VerifyS256(verifier, challenge) {
		t.Fatal("正确的 verifier 应通过")
	}
	if VerifyS256(strings.Replace(verifier, "d", "e", 1), challenge) {
		t.Fatal("错误的 verifier 必须被拒")
	}
	// plain：challenge == verifier（43 字符、合法字符集）—— 必须被拒，S256 下它对不上。
	if VerifyS256(verifier, verifier) {
		t.Fatal("plain（challenge 即 verifier）必须被拒")
	}
	for _, bad := range []string{"", "short", strings.Repeat("a", 129), strings.Repeat("a", 42) + "!"} {
		if ValidVerifier(bad) {
			t.Errorf("verifier %q 应非法", bad)
		}
	}
	if ValidChallenge(challenge+"A") || ValidChallenge("") || ValidChallenge(strings.Repeat("!", 43)) {
		t.Fatal("非 43 位 base64url 的 challenge 应非法")
	}
}

// ---------------------------------------------------------------- redirect_uri

func TestRedirectURIMatch(t *testing.T) {
	reg := []string{"https://app.example.com/cb", "http://127.0.0.1:33418/callback", "http://localhost/oauth?x=1"}
	cases := []struct {
		name string
		req  string
		want bool
	}{
		{"精确", "https://app.example.com/cb", true},
		{"回环端口可变", "http://127.0.0.1:51234/callback", true},
		{"回环无端口→有端口", "http://localhost:8080/oauth?x=1", true},
		{"路径不同被拒", "https://app.example.com/cb2", false},
		{"https 尾斜杠也算不同", "https://app.example.com/cb/", false},
		{"https→http 降级被拒", "http://app.example.com/cb", false},
		{"回环路径不同被拒", "http://127.0.0.1:51234/other", false},
		{"回环 query 不同被拒", "http://localhost:8080/oauth?x=2", false},
		{"回环换主机被拒", "http://[::1]:51234/callback", false},
		{"非回环 http 端口不同被拒", "https://app.example.com:8443/cb", false},
		{"带 fragment 被拒", "http://127.0.0.1:1/callback#x", false},
	}
	for _, c := range cases {
		if got := MatchRedirectURI(reg, c.req); got != c.want {
			t.Errorf("%s：%s → %v，期望 %v", c.name, c.req, got, c.want)
		}
	}
	for _, ok := range []string{"https://a.com/cb", "http://127.0.0.1:9/cb", "http://[::1]/cb", "http://localhost:1/cb"} {
		if err := ValidRedirectURI(ok); err != nil {
			t.Errorf("%s 应合法：%v", ok, err)
		}
	}
	for _, bad := range []string{"http://a.com/cb", "https://a.com/cb#f", "/relative", "javascript:alert(1)", "myapp://cb"} {
		if err := ValidRedirectURI(bad); err == nil {
			t.Errorf("%s 应非法", bad)
		}
	}
}

// ---------------------------------------------------------------- resource

func TestResourceCanonical(t *testing.T) {
	cases := map[string]string{
		"https://Example.COM/mcp":         "https://example.com/mcp",
		"https://example.com:443/mcp/":    "https://example.com/mcp",
		"http://example.com:80/mcp":       "http://example.com/mcp",
		"http://localhost:8180/mcp/x/gen": "http://localhost:8180/mcp/x/gen",
		"https://example.com":             "https://example.com",
		"https://example.com/MCP":         "https://example.com/MCP", // 路径大小写保留
	}
	for in, want := range cases {
		got, err := CanonicalResource(in)
		if err != nil || got != want {
			t.Errorf("%s → %q %v，期望 %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "/mcp", "https://example.com/mcp#a", "https://example.com/mcp?x=1",
		"https://u:p@example.com/mcp", "ftp://example.com/mcp"} {
		if _, err := CanonicalResource(bad); err == nil {
			t.Errorf("%q 应非法", bad)
		}
	}

	e := NewEndpoints("https://Example.com/mcp/")
	r := httptest.NewRequest(http.MethodGet, "http://ignored/", nil)
	if e.Base(r) != "https://example.com" || e.Resource(r, PathReadOnly) != "https://example.com/mcp/readonly" {
		t.Fatalf("base / resource 不对：%s %s", e.Base(r), e.Resource(r, PathReadOnly))
	}
	if e.ResourceMetadataURL(r, PathReadOnly) != "https://example.com/.well-known/oauth-protected-resource/mcp/readonly" {
		t.Fatalf("元数据地址不对：%s", e.ResourceMetadataURL(r, PathReadOnly))
	}
	for raw, want := range map[string]bool{
		"https://example.com/mcp": true, "https://EXAMPLE.com/mcp/": true, "https://example.com/mcp/readonly": true,
		"https://example.com/mcp/x/generation": true, "https://example.com/mcp/x/nope": false,
		"https://example.com/other": false, "https://evil.com/mcp": false, "http://example.com/mcp": false,
	} {
		if _, ok := e.CheckResource(r, raw); ok != want {
			t.Errorf("CheckResource(%s) = %v，期望 %v", raw, ok, want)
		}
	}
	// 未配置：按请求推导（仅开发）。
	d := NewEndpoints("")
	r2 := httptest.NewRequest(http.MethodGet, "http://LocalHost:8180/mcp", nil)
	if d.Resource(r2, PathMCP) != "http://localhost:8180/mcp" {
		t.Fatalf("推导的 resource：%s", d.Resource(r2, PathMCP))
	}
}

// ---------------------------------------------------------------- req 句柄

func TestReqHandle_TamperAndExpiry(t *testing.T) {
	key := DeriveRequestKey("material")
	now := time.Unix(1_800_000_000, 0)
	h, err := SignRequest(key, ConsentRequest{ClientID: 7, RedirectURI: "https://a.com/cb", Scopes: []string{"seeaitv:view"},
		CodeChallenge: "c", Resource: "https://x/mcp", Issuer: "https://x", Exp: now.Add(10 * time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseRequest(key, h, now)
	if err != nil || got.ClientID != 7 || got.RedirectURI != "https://a.com/cb" {
		t.Fatalf("正常句柄应解析：%+v %v", got, err)
	}
	if _, err := ParseRequest(key, h, now.Add(10*time.Minute)); !errors.Is(err, ErrRequestExpired) {
		t.Fatalf("过期应 ErrRequestExpired：%v", err)
	}
	payload, sig, _ := strings.Cut(h, ".")
	// 改 payload 的一个字符（换掉回调地址之类）：签名对不上。
	b := []byte(payload)
	b[5] ^= 1
	for _, bad := range []string{string(b) + "." + sig, payload + "." + sig[:len(sig)-1] + "A", payload, "", "a.b"} {
		if _, err := ParseRequest(key, bad, now); !errors.Is(err, ErrRequestInvalid) {
			t.Errorf("篡改 %q 应 ErrRequestInvalid：%v", bad, err)
		}
	}
	if _, err := ParseRequest(DeriveRequestKey("other"), h, now); !errors.Is(err, ErrRequestInvalid) {
		t.Fatalf("别的密钥签的应无效：%v", err)
	}
}

// ---------------------------------------------------------------- CIMD

func cimdServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// fetcher 走真实的 provider/transport（允许内网，因为 httptest 在 127.0.0.1）。
func fetcher(allowPrivate bool) TransportFetcher {
	return TransportFetcher{Client: ptransport.NewClient(ptransport.URLPolicy{AllowPrivate: allowPrivate})}
}

func doc(id string) []byte {
	b, _ := json.Marshal(map[string]any{"client_id": id, "client_name": "Demo",
		"redirect_uris": []string{"http://127.0.0.1/callback"}, "token_endpoint_auth_method": "none"})
	return b
}

func TestCIMD_Parses(t *testing.T) {
	var srv *httptest.Server
	srv = cimdServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(doc(srv.URL + "/client.json"))
	})
	c, err := fetchCIMD(context.Background(), fetcher(true), srv.URL+"/client.json")
	if err != nil || c.Name != "Demo" || len(c.RedirectURIs) != 1 {
		t.Fatalf("合法文档应解析：%+v %v", c, err)
	}
}

func TestCIMD_RejectsRedirect(t *testing.T) {
	var target *httptest.Server
	target = cimdServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(doc(target.URL + "/client.json"))
	})
	src := cimdServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/client.json", http.StatusFound)
	})
	_, err := fetchCIMD(context.Background(), fetcher(true), src.URL+"/client.json")
	if err == nil || !strings.Contains(err.Error(), "重定向") {
		t.Fatalf("重定向必须被拒（不跟随）：%v", err)
	}
}

func TestCIMD_SizeLimit(t *testing.T) {
	srv := cimdServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"client_id":"` + strings.Repeat("a", cimdMaxBytes) + `"}`))
	})
	_, err := fetchCIMD(context.Background(), fetcher(true), srv.URL+"/client.json")
	if err == nil || !errors.Is(err, errTooLarge) {
		t.Fatalf("超过 64 KiB 必须被拒：%v", err)
	}
	// 非 JSON 也拒。
	html := cimdServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html></html>"))
	})
	if _, err := fetchCIMD(context.Background(), fetcher(true), html.URL+"/client.json"); err == nil {
		t.Fatal("非 JSON 必须被拒")
	}
}

func TestCIMD_ClientIDMismatch(t *testing.T) {
	srv := cimdServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(doc("https://someone-else.example/client.json"))
	})
	_, err := fetchCIMD(context.Background(), fetcher(true), srv.URL+"/client.json")
	if err == nil || !strings.Contains(err.Error(), "client_id") {
		t.Fatalf("client_id 与地址不一致必须被拒：%v", err)
	}
}

func TestCIMD_PrivateAddressBlocked(t *testing.T) {
	hit := false
	srv := cimdServer(t, func(w http.ResponseWriter, _ *http.Request) { hit = true })
	_, err := fetchCIMD(context.Background(), fetcher(false), srv.URL+"/client.json")
	if err == nil || !errors.Is(err, ptransport.ErrPrivateNet) || hit {
		t.Fatalf("出网策略禁内网时必须拒且不发出请求：err=%v hit=%v", err, hit)
	}
	for _, bad := range []string{"http://a.com/c.json", "https://a.com", "https://a.com/", "https://a.com/../c",
		"https://u@a.com/c", "https://a.com/c#x"} {
		if ValidCIMDURL(bad) == nil {
			t.Errorf("%s 不是合法的 CIMD client_id", bad)
		}
	}
	if err := ValidCIMDURL("https://claude.ai/oauth/mcp-oauth-client-metadata"); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------- 元数据端点

func TestMetadata_端点内容(t *testing.T) {
	s := New(Options{Store: nilStore{}, NextID: func() int64 { return 1 }, RequestKey: []byte("k"),
		PublicURL: "https://example.com/mcp"})
	get := func(path string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "http://internal"+path, nil)
		if strings.HasPrefix(path, wellKnownAS) {
			s.authorizationServer(rec, r)
		} else {
			s.protectedResource(rec, r)
		}
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return rec.Code, m
	}
	for path, want := range map[string]string{
		wellKnownPR:                   "https://example.com/mcp",
		wellKnownPR + "/mcp":          "https://example.com/mcp",
		wellKnownPR + "/mcp/readonly": "https://example.com/mcp/readonly",
		wellKnownPR + "/mcp/x/script": "https://example.com/mcp/x/script",
	} {
		code, m := get(path)
		if code != 200 || m["resource"] != want {
			t.Errorf("%s → %d resource=%v，期望 %s", path, code, m["resource"], want)
		}
		if as, _ := m["authorization_servers"].([]any); len(as) != 1 || as[0] != "https://example.com" {
			t.Errorf("%s authorization_servers=%v", path, m["authorization_servers"])
		}
		if bm, _ := m["bearer_methods_supported"].([]any); len(bm) != 1 || bm[0] != "header" {
			t.Errorf("%s bearer_methods_supported=%v", path, m["bearer_methods_supported"])
		}
		if sc, _ := m["scopes_supported"].([]any); len(sc) != 4 {
			t.Errorf("%s scopes_supported=%v", path, m["scopes_supported"])
		}
	}
	if code, _ := get(wellKnownPR + "/mcp/x/nope"); code != 404 {
		t.Fatalf("未知端点的元数据应 404：%d", code)
	}
	code, m := get(wellKnownAS)
	if code != 200 || m["issuer"] != "https://example.com" || m["token_endpoint"] != "https://example.com/oauth/token" ||
		m["client_id_metadata_document_supported"] != true {
		t.Fatalf("授权服务器元数据：%d %v", code, m)
	}
	if cc, _ := m["code_challenge_methods_supported"].([]any); len(cc) != 1 || cc[0] != "S256" {
		t.Fatalf("只应声明 S256：%v", m["code_challenge_methods_supported"])
	}
	if am, _ := m["token_endpoint_auth_methods_supported"].([]any); len(am) != 1 || am[0] != "none" {
		t.Fatalf("只应声明 none：%v", m["token_endpoint_auth_methods_supported"])
	}
}

// nilStore 只为构造 Server（元数据端点不碰库）。
type nilStore struct{ Store }
