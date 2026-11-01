package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCheckURL_只允许公网http(t *testing.T) {
	p := URLPolicy{AllowPrivate: false}
	bad := map[string]string{
		"内网 10 段":   "http://10.0.0.1/api",
		"内网 192 段":  "https://192.168.1.1/api",
		"内网 172 段":  "http://172.16.0.5/api",
		"回环":        "http://127.0.0.1:8080/api",
		"localhost": "http://localhost/api",
		".local":    "http://printer.local/api",
		".internal": "http://db.internal/api",
		"云元数据":      "http://169.254.169.254/latest/meta-data/",
		"IPv6 回环":   "http://[::1]/api",
		"运营商级 NAT":  "http://100.100.0.1/api",
		"file 协议":   "file:///etc/passwd",
		"gopher 协议": "gopher://x/",
		"缺主机":       "http:///path",
	}
	for name, u := range bad {
		if err := p.CheckURL(u); err == nil {
			t.Errorf("%s 应被拒: %s", name, u)
		}
	}
	for _, u := range []string{
		"https://api.openai.com/v1/chat", "http://example.com:8080/x",
	} {
		if err := p.CheckURL(u); err != nil {
			t.Errorf("%s 应放行，实际 %v", u, err)
		}
	}
}

// 开发环境要能连本机的假上游；production 由 config 强制关掉这个开关。
func TestCheckURL_允许内网时放行(t *testing.T) {
	p := URLPolicy{AllowPrivate: true}
	for _, u := range []string{"http://127.0.0.1:9/x", "http://10.0.0.1/y"} {
		if err := p.CheckURL(u); err != nil {
			t.Errorf("开发模式应放行 %s，实际 %v", u, err)
		}
	}
}

func TestIsPrivateIP(t *testing.T) {
	private := []string{
		"127.0.0.1", "10.1.2.3", "192.168.0.1", "172.16.0.1", "172.31.255.255",
		"169.254.169.254", "0.0.0.0", "100.64.0.1", "240.0.0.1",
		"::1", "fe80::1", "fc00::1",
	}
	for _, s := range private {
		if !IsPrivateIP(net.ParseIP(s)) {
			t.Errorf("%s 应判为内网", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700::1111", "172.32.0.1"} {
		if IsPrivateIP(net.ParseIP(s)) {
			t.Errorf("%s 不该判为内网", s)
		}
	}
}

// 上游返回的产物地址常是 CDN 签名链接，把 API key 发过去等于泄漏给第三方。
func TestSameOrigin(t *testing.T) {
	base := "https://api.provider.com/v1/submit"
	same := []string{
		"https://api.provider.com/v1/result/1",
		"https://API.PROVIDER.COM/x",
	}
	for _, u := range same {
		if !SameOrigin(base, u) {
			t.Errorf("%s 应判为同源", u)
		}
	}
	diff := []string{
		"https://cdn.provider.com/file.png", // 不同子域＝不同源
		"http://api.provider.com/v1/x",      // 协议不同
		"https://evil.com/x",
		"https://api.provider.com:8443/x", // 端口不同
	}
	for _, u := range diff {
		if SameOrigin(base, u) {
			t.Errorf("%s 不该判为同源 —— 带上凭据就是泄漏", u)
		}
	}
}

// ⚠️ 这条是线上事故换来的：每次请求新建客户端会让连接与 goroutine 只增不减。
func TestClient_按连接超时分桶复用(t *testing.T) {
	c := NewClient(URLPolicy{AllowPrivate: true})
	a1 := c.httpClient(5 * time.Second)
	a2 := c.httpClient(5 * time.Second)
	b := c.httpClient(10 * time.Second)

	if a1 != a2 {
		t.Fatal("同一超时值必须复用同一个客户端 —— 每次新建会让线程与连接只增不减")
	}
	if a1 == b {
		t.Fatal("不同连接超时应当是不同客户端")
	}

	// 并发取用也只应产生一个实例。
	var wg sync.WaitGroup
	got := make([]*http.Client, 20)
	wg.Add(20)
	for i := 0; i < 20; i++ {
		go func(i int) { defer wg.Done(); got[i] = c.httpClient(3 * time.Second) }(i)
	}
	wg.Wait()
	for i := range got {
		if got[i] != got[0] {
			t.Fatal("并发取用应返回同一个客户端")
		}
	}
}

// 跟随重定向会把 Authorization 头带到重定向目标。
func TestClient_不跟随重定向(t *testing.T) {
	var gotAuthAtTarget string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthAtTarget = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer target.Close()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer src.Close()

	c := NewClient(URLPolicy{AllowPrivate: true})
	resp, err := c.Do(context.Background(), Request{
		Method: "GET", URL: src.URL,
		Header: map[string]string{"Authorization": "Bearer secret"},
		Opts:   Options{RequestTimeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("应原样返回 302 交给调用方处理，实际 %d", resp.StatusCode)
	}
	if gotAuthAtTarget != "" {
		t.Fatal("凭据被带到了重定向目标 —— 这正是要防的泄漏")
	}
}

// 连接期再校验一次：URL 校验挡不住 DNS 重绑定。
func TestClient_连接期拦截内网(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	// 用 IP 直连会被 CheckURL 挡住；这里构造一个能过 URL 校验、
	// 但解析到回环的名字，验证 DialContext 那一层。
	c := NewClient(URLPolicy{AllowPrivate: false})
	host := strings.TrimPrefix(srv.URL, "http://")
	_, err := c.Do(context.Background(), Request{
		Method: "GET", URL: "http://" + host + "/",
		Opts: Options{RequestTimeout: 3 * time.Second},
	})
	if err == nil {
		t.Fatal("连到回环地址应被拒")
	}
	if !errors.Is(err, ErrPrivateNet) && ClassifyError(err) != ErrorPolicy &&
		!strings.Contains(err.Error(), "内网") {
		t.Fatalf("应因内网策略被拒，实际 %v", err)
	}
}

// ⚠️ 整包缓冲下，上游挂起不关连接时一个字节都拿不到，要等满整个请求超时。
func TestStream_空闲超时早于整体超时(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte("data: first\n\n"))
		w.(http.Flusher).Flush()
		// 之后一直挂着不关连接，也不再发数据。
		time.Sleep(3 * time.Second)
	}))
	defer srv.Close()

	c := NewClient(URLPolicy{AllowPrivate: true})
	var chunks []string
	started := time.Now()
	_, err := c.Stream(context.Background(), Request{
		Method: "GET", URL: srv.URL,
		Opts: Options{
			// 整体给 30 秒，空闲只给 300ms —— 应当在 300ms 左右就结束。
			RequestTimeout: 30 * time.Second, IdleTimeout: 300 * time.Millisecond,
		},
	}, func(b []byte) error {
		chunks = append(chunks, string(b))
		return nil
	})
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("空闲超时应报错")
	}
	if !errors.Is(err, ErrIdleTimeout) && ClassifyError(err) != ErrorStream {
		t.Fatalf("应归类为流式错误，实际 %v (%s)", err, ClassifyError(err))
	}
	if elapsed > 3*time.Second {
		t.Fatalf("空闲超时没有生效，等了 %v —— 这正是「干等满整个请求超时」的老问题", elapsed)
	}
	// 已经收到的数据要交给调用方，不能因为后面超时就整批丢掉。
	if len(chunks) == 0 || !strings.Contains(chunks[0], "first") {
		t.Errorf("超时前收到的数据应已交付，实际 %v", chunks)
	}
}

func TestStream_正常结束(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 3; i++ {
			_, _ = w.Write([]byte("chunk\n"))
			w.(http.Flusher).Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer srv.Close()

	c := NewClient(URLPolicy{AllowPrivate: true})
	n := 0
	resp, err := c.Stream(context.Background(), Request{
		Method: "GET", URL: srv.URL,
		Opts: Options{RequestTimeout: 5 * time.Second, IdleTimeout: time.Second},
	}, func([]byte) error { n++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Kind != ErrorNone {
		t.Fatalf("应正常结束，实际 %+v", resp)
	}
	if n == 0 {
		t.Fatal("应收到数据块")
	}
}

// 4xx/5xx 要在流式路径上也能拿到响应体，否则排查时只知道"失败了"。
func TestStream_错误响应带回响应体(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer srv.Close()

	c := NewClient(URLPolicy{AllowPrivate: true})
	resp, err := c.Stream(context.Background(), Request{
		Method: "GET", URL: srv.URL,
		Opts: Options{RequestTimeout: 5 * time.Second, IdleTimeout: time.Second},
	}, func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 429 || resp.Kind != ErrorHTTP {
		t.Fatalf("应回报 HTTP 错误，实际 %+v", resp)
	}
	if !strings.Contains(string(resp.Body), "rate limited") {
		t.Errorf("响应体应带回来供排查，实际 %q", resp.Body)
	}
}

func TestClassifyError(t *testing.T) {
	cases := map[string]struct {
		err  error
		want ErrorKind
	}{
		"无错误":  {nil, ErrorNone},
		"策略":   {ErrPrivateNet, ErrorPolicy},
		"空闲超时": {ErrIdleTimeout, ErrorStream},
		"DNS":  {&net.DNSError{Err: "no such host", Name: "x"}, ErrorDNS},
		"超时":   {&net.OpError{Op: "dial", Err: timeoutErr{}}, ErrorTimeout},
		"连接":   {&net.OpError{Op: "dial", Err: errors.New("refused")}, ErrorConnect},
		"其他":   {errors.New("boom"), ErrorHTTP},
	}
	for name, c := range cases {
		if got := ClassifyError(c.err); got != c.want {
			t.Errorf("%s: 期望 %s，实际 %s", name, c.want, got)
		}
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestAuthHeader(t *testing.T) {
	if h := AuthHeader("bearer", "sk-1", ""); h["Authorization"] != "Bearer sk-1" {
		t.Errorf("bearer 形态不对: %v", h)
	}
	if h := AuthHeader("header", "sk-2", "X-Token"); h["X-Token"] != "sk-2" {
		t.Errorf("自定义头不对: %v", h)
	}
	// 没指定头名时给一个合理默认，而不是静默不带认证。
	if h := AuthHeader("header", "sk-3", ""); h["X-API-Key"] != "sk-3" {
		t.Errorf("默认头名不对: %v", h)
	}
	if h := AuthHeader("none", "sk-4", ""); len(h) != 0 {
		t.Errorf("none 不该带任何认证头，实际 %v", h)
	}
}

// 响应体要有上限：上游返回几 GB 不该把进程撑爆。
func TestDo_响应体有上限(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		chunk := strings.Repeat("x", 1<<20)
		for i := 0; i < 40; i++ {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	c := NewClient(URLPolicy{AllowPrivate: true})
	resp, err := c.Do(context.Background(), Request{
		Method: "GET", URL: srv.URL, Opts: Options{RequestTimeout: 10 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Body) > maxResponseBytes {
		t.Fatalf("响应体 %d 超过上限 %d", len(resp.Body), maxResponseBytes)
	}
}

// 连接测试：判定标准是"能连上并得到响应"，不是 2xx。
func TestProbe_非2xx也算连通(t *testing.T) {
	cases := []struct {
		name   string
		status int
		wantOK bool
		msgHas string
	}{
		{"200 正常", 200, true, "连接正常"},
		// 很多上游的探活路径需要参数会回 400/404 —— 那也说明这一层是通的。
		{"404 路径不存在但连通", 404, true, "连接正常"},
		{"400 参数缺失但连通", 400, true, "连接正常"},
		// ⚠️ 401/403 要单独说：连通但密钥不对，这是运维最需要知道的失败。
		{"401 鉴权失败", 401, false, "鉴权被拒"},
		{"403 无权限", 403, false, "鉴权被拒"},
		{"500 上游故障", 500, false, "服务可能不可用"},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
		}))
		client := NewClient(URLPolicy{AllowPrivate: true})
		got := client.Probe(context.Background(), srv.URL, "/health", nil, Options{})
		srv.Close()

		if got.OK != c.wantOK {
			t.Errorf("%s: OK 期望 %v，实际 %v（%s）", c.name, c.wantOK, got.OK, got.Message)
		}
		if !strings.Contains(got.Message, c.msgHas) {
			t.Errorf("%s: 提示应含 %q，实际 %q", c.name, c.msgHas, got.Message)
		}
		if got.StatusCode != c.status {
			t.Errorf("%s: 状态码应带回来，实际 %d", c.name, got.StatusCode)
		}
	}
}

// 「dial tcp: i/o timeout」对运维没有帮助，要翻成能照着做的话。
func TestProbe_错误提示对运维有用(t *testing.T) {
	c := NewClient(URLPolicy{AllowPrivate: true})
	// 连一个没人监听的端口。
	got := c.Probe(context.Background(), "http://127.0.0.1:9", "/x", nil,
		Options{RequestTimeout: 2 * time.Second, ConnectTimeout: time.Second})
	if got.OK {
		t.Fatal("连不上不该判成功")
	}
	if !strings.Contains(got.Message, "检查") && !strings.Contains(got.Message, "无响应") {
		t.Errorf("提示应告诉运维去查什么，实际 %q", got.Message)
	}

	// 内网地址在 production 策略下要明确说是"地址不被允许"。
	strict := NewClient(URLPolicy{AllowPrivate: false})
	got2 := strict.Probe(context.Background(), "http://10.0.0.1", "/x", nil, Options{})
	if got2.ErrorKind != ErrorPolicy {
		t.Errorf("应归类为策略拒绝，实际 %s", got2.ErrorKind)
	}
	if !strings.Contains(got2.Message, "不被允许") {
		t.Errorf("应说明是地址策略，实际 %q", got2.Message)
	}
}

// 探活要带上认证头，否则一个需要鉴权的上游永远探不通。
func TestProbe_携带认证头(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer srv.Close()
	c := NewClient(URLPolicy{AllowPrivate: true})
	c.Probe(context.Background(), srv.URL, "", AuthHeader("bearer", "sk-1", ""), Options{})
	if got != "Bearer sk-1" {
		t.Errorf("应带上认证头，实际 %q", got)
	}
}
