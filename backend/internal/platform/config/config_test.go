package config

import (
	"strings"
	"testing"
	"time"
)

// production 下任何一个开发开关打开都必须启动失败。
// 这条不是风格问题：假 Provider 或假支付跑在生产上，就是直接给出未付费的产物。
func TestValidate_生产环境拒绝开发开关(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{"假Provider", func(c *Config) { c.Dev.FakeProvider = true }, "SEEAITV_DEV_FAKE_PROVIDER"},
		{"假支付", func(c *Config) { c.Dev.FakePayment = true }, "SEEAITV_DEV_FAKE_PAYMENT"},
		{"允许内网", func(c *Config) { c.Dev.AllowPrivateNetwork = true }, "SEEAITV_DEV_ALLOW_PRIVATE_NETWORK"},
		{"本地存储", func(c *Config) { c.Storage.Type = "local" }, "local 存储"},
		{"实时通道放行任意来源", func(c *Config) { c.WSAllowedOrigins = []string{"app.example.com", "*"} }, "SEEAITV_WS_ALLOWED_ORIGINS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validProduction()
			tc.mutate(&c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("期望校验失败，实际通过")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("错误信息未指明原因\n期望包含: %s\n实际: %v", tc.wantSub, err)
			}
		})
	}
}

// 同样的开关在 dev 下必须允许，否则本地根本没法开发。
func TestValidate_开发环境允许开发开关(t *testing.T) {
	c := validProduction()
	c.Env = EnvDev
	c.Storage.Type = "local"
	c.Dev = Dev{FakeProvider: true, FakePayment: true, AllowPrivateNetwork: true}
	if err := c.Validate(); err != nil {
		t.Fatalf("dev 环境不应报错，实际: %v", err)
	}
}

// 多个问题要一次性全报出来，不能只报第一个 —— 否则运维得改一次重启一次。
func TestValidate_聚合全部错误(t *testing.T) {
	c := validProduction()
	c.Dev = Dev{FakeProvider: true, FakePayment: true, AllowPrivateNetwork: true}
	c.Storage.Type = "local"
	err := c.Validate()
	if err == nil {
		t.Fatal("期望校验失败")
	}
	for _, want := range []string{
		"SEEAITV_DEV_FAKE_PROVIDER",
		"SEEAITV_DEV_FAKE_PAYMENT",
		"SEEAITV_DEV_ALLOW_PRIVATE_NETWORK",
		"local 存储",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("聚合错误里缺少 %q\n实际: %v", want, err)
		}
	}
}

func TestValidate_枚举取值(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"环境", func(c *Config) { c.Env = "prod" }}, // 正确值是 production
		{"日志格式", func(c *Config) { c.Log.Format = "logfmt" }},
		{"存储类型", func(c *Config) { c.Storage.Type = "oss" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := validProduction()
			tc.mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("非法枚举值应当被拒绝")
			}
		})
	}
}

// env 读取要把纯空白视为未设置，否则 `KEY= ` 会把默认值顶掉成空串。
func TestEnv_空白视为未设置(t *testing.T) {
	t.Setenv("SEEAITV_TEST_BLANK", "   ")
	if got := env("SEEAITV_TEST_BLANK", "fallback"); got != "fallback" {
		t.Fatalf("期望回落到默认值，实际 %q", got)
	}
	t.Setenv("SEEAITV_TEST_BLANK", "  value  ")
	if got := env("SEEAITV_TEST_BLANK", "fallback"); got != "value" {
		t.Fatalf("期望去除首尾空白，实际 %q", got)
	}
}

// 实时通道的来源白名单：逗号分隔、去空白、丢空项；没配就是 nil（= 只接受同源）。
func TestEnvList_实时通道来源白名单(t *testing.T) {
	t.Setenv("SEEAITV_WS_ALLOWED_ORIGINS", " localhost:3000, ,127.0.0.1:3000 ")
	got := envList("SEEAITV_WS_ALLOWED_ORIGINS")
	if strings.Join(got, "|") != "localhost:3000|127.0.0.1:3000" {
		t.Fatalf("期望两项，实际 %q", got)
	}
	t.Setenv("SEEAITV_WS_ALLOWED_ORIGINS", "  ")
	if got := envList("SEEAITV_WS_ALLOWED_ORIGINS"); got != nil {
		t.Fatalf("没配时应为 nil（只接受同源），实际 %q", got)
	}
}

func validProduction() Config {
	return Config{
		Env:      EnvProduction,
		HTTPAddr: ":8080",
		Log:      Log{Level: "info", Format: "json"},
		Storage:  Storage{Type: "s3", Bucket: "seeaitv"},
		Director: validDirector(),
		Smartcut: Smartcut{FFmpegMaxConcurrent: 2, AnalysisScanInterval: 5 * time.Second, AnalysisLease: 10 * time.Minute,
			RenderUserConcurrency: 1, RenderUserPerHour: 20, RenderDownloadTTL: 5 * time.Minute,
			PlanConcurrency: 1, PlanPerHour: 20, PlanTimeout: 3 * time.Minute, PlanVisionTimeout: 5 * time.Minute,
			PlanStaleAfter: 20 * time.Minute},
	}
}

// 智能剪辑的三项：ffmpeg 并发 0 会让所有渲染与预览永远排队；租约短于 1 分钟的话，
// 一次正常的续期抖动就会让别的实例把还活着的任务接走、重转一遍。启动时就拒。
func TestValidate_智能剪辑(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Smartcut)
		wantSub string
	}{
		{"ffmpeg 并发为 0", func(s *Smartcut) { s.FFmpegMaxConcurrent = 0 }, "SEEAITV_FFMPEG_MAX_CONCURRENT"},
		{"扫描间隔为 0", func(s *Smartcut) { s.AnalysisScanInterval = 0 }, "SEEAITV_ANALYSIS_SCAN_INTERVAL"},
		{"租约太短", func(s *Smartcut) { s.AnalysisLease = 30 * time.Second }, "SEEAITV_ANALYSIS_LEASE"},
		{"下载链接太长", func(s *Smartcut) { s.RenderDownloadTTL = 2 * time.Hour }, "SEEAITV_RENDER_DOWNLOAD_TTL"},
		// AI 剪辑（SCT-27）：额度 0 等于永远剪不了；调用上限两次（含回灌修复）要在长路由之内；判死阈值要大于长路由。
		{"AI 剪辑额度为 0", func(s *Smartcut) { s.PlanPerHour = 0 }, "SEEAITV_SMARTCUT_PLAN_CONCURRENCY"},
		{"AI 剪辑调用上限太长", func(s *Smartcut) { s.PlanVisionTimeout = 8 * time.Minute }, "SEEAITV_SMARTCUT_PLAN_VISION_TIMEOUT"},
		{"AI 剪辑判死阈值不大于路由上限", func(s *Smartcut) { s.PlanStaleAfter = 15 * time.Minute }, "SEEAITV_SMARTCUT_PLAN_STALE_AFTER"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validProduction()
			c.Auth = Auth{JWTSecret: strings.Repeat("j", 32), SecretKey: strings.Repeat("k", 32)}
			tc.mutate(&c.Smartcut)
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("应拒绝并点名 %s：%v", tc.wantSub, err)
			}
		})
	}
}

func validDirector() Director {
	return Director{PreviewMaxBytes: 50 << 20, SplatMaxBytes: 100 << 20, BuildConcurrency: 1, BuildPerHour: 20,
		BuildTimeout: 3 * time.Minute, BuildStaleAfter: 20 * time.Minute, BuildImageWidth: 640}
}

// 搭景的判死阈值必须大于长耗时路由的上限（15 分钟）：否则一个合法地还在等模型的请求会被判失败，
// 随后又写回成功（CLAUDE.md §5.7）。启动时就拒，不等第一次误判。
func TestConfig_搭景判死阈值必须大于路由上限(t *testing.T) {
	c := validProduction()
	c.Auth = Auth{JWTSecret: strings.Repeat("j", 32), SecretKey: strings.Repeat("k", 32)}
	for _, stale := range []time.Duration{10 * time.Minute, 15 * time.Minute} {
		c.Director = validDirector()
		c.Director.BuildStaleAfter = stale
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "SEEAITV_DIRECTOR_BUILD_STALE_AFTER") {
			t.Errorf("判死阈值 %s 不大于 15 分钟应拒绝，得到 %v", stale, err)
		}
	}
	c.Director = validDirector()
	c.Director.BuildImageWidth = 500
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "SEEAITV_DIRECTOR_BUILD_IMAGE_WIDTH") {
		t.Errorf("缩略图档位只能是 160 / 320 / 640，得到 %v", err)
	}
	c.Director = validDirector()
	if err := c.Validate(); err != nil {
		t.Fatalf("默认值应通过：%v", err)
	}
}

// 导演台的上传上限为 0 或负数等于一个文件都传不上来：启动时就拒，而不是等用户第一次上传。
func TestValidate_导演台上传上限必须为正(t *testing.T) {
	c := validProduction()
	c.Auth = Auth{JWTSecret: strings.Repeat("j", 32), SecretKey: strings.Repeat("k", 32)}
	if err := c.Validate(); err != nil {
		t.Fatalf("基线配置应通过：%v", err)
	}
	c.Director = Director{PreviewMaxBytes: 0, SplatMaxBytes: -1}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "SEEAITV_DIRECTOR_PREVIEW_MAX_BYTES") ||
		!strings.Contains(err.Error(), "SEEAITV_DIRECTOR_SPLAT_MAX_BYTES") {
		t.Fatalf("两项都非法时应各报一条：%v", err)
	}
}

// 轮换期间：当前版本 + 历史版本要一起交给 secret.New。
//
// ⚠️ 这一条是密钥轮换能不能做的**全部前提**。修复前两个 main 都写死
// `{1: key}`，于是换掉 SEEAITV_SECRET_KEY 就等于宣称"v1 是新 key" ——
// 库里全部 v1 密文（Provider 密钥、支付私钥、SMTP 凭据）当场解不开，
// 而错误是"解密失败"，看起来像数据损坏而不是配置问题。
func TestSecretKeys_轮换期间新旧一起交出去(t *testing.T) {
	a := Auth{
		SecretKey:        "new-material",
		SecretKeyVersion: 2,
		OldSecretKeys:    map[int]string{1: "old-material"},
	}
	keys := a.SecretKeys()
	if len(keys) != 2 || keys[1] != "old-material" || keys[2] != "new-material" {
		t.Fatalf("新旧密钥都要在: %v", keys)
	}
	if a.CurrentKeyVersion() != 2 {
		t.Fatal("当前版本应是 2")
	}
}

// 没声明版本时默认 v1（未轮换过的部署不需要改任何配置）。
func TestSecretKeys_默认版本为1(t *testing.T) {
	a := Auth{SecretKey: "material"}
	if a.CurrentKeyVersion() != 1 {
		t.Fatal("默认版本应是 1")
	}
	if keys := a.SecretKeys(); len(keys) != 1 || keys[1] != "material" {
		t.Fatalf("未轮换时只有 v1: %v", keys)
	}
}

// 配置自相矛盾要在**启动时**拒绝，不能等到第一次解密失败。
func TestValidate_轮换配置自相矛盾(t *testing.T) {
	base := func() Config {
		return Config{
			Env: EnvDev, Log: Log{Format: "text"}, Storage: Storage{Type: "local"}, Director: validDirector(),
			Smartcut: validProduction().Smartcut,
		}
	}

	// 同一个版本号被两处给出且取值不同 —— 到底哪个是 v1？
	c := base()
	c.Auth = Auth{
		SecretKey: "A", SecretKeyVersion: 1,
		OldSecretKeys: map[int]string{1: "B"},
	}
	if err := c.Validate(); err == nil {
		t.Fatal("同一版本号取值冲突必须拒绝 —— 放过的话写进去的密文用哪个 key 加的没人说得清")
	}

	// 历史版本号大于当前版本：轮换只能向前。
	c = base()
	c.Auth = Auth{
		SecretKey: "A", SecretKeyVersion: 1,
		OldSecretKeys: map[int]string{2: "B"},
	}
	if err := c.Validate(); err == nil {
		t.Fatal("历史版本号高于当前版本必须拒绝 —— 版本号退回会让新密文用旧号")
	}

	// 正常的轮换中配置要放行。
	c = base()
	c.Auth = Auth{
		SecretKey: "A", SecretKeyVersion: 2,
		OldSecretKeys: map[int]string{1: "B"},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("正常轮换配置被拒: %v", err)
	}
}

// `SEEAITV_SECRET_KEY_VERSION` 本身以 `SEEAITV_SECRET_KEY_V` 开头，
// 不能被当成"版本 ERSION 的历史密钥"收进来。
func TestOldSecretKeys_不把VERSION当成版本号(t *testing.T) {
	t.Setenv("SEEAITV_SECRET_KEY_VERSION", "2")
	t.Setenv("SEEAITV_SECRET_KEY_V1", "old")
	keys := oldSecretKeys()
	if len(keys) != 1 || keys[1] != "old" {
		t.Fatalf("只应收到 v1: %v", keys)
	}
}

// production 下开着 MCP 时，两个公网地址必须配置且是 https（MCPT-12）：OAuth 的 issuer、受保护资源的
// resource、同意页地址都由它们给出 —— 按请求推导或 http 的话，客户端表现为「授权成功但每次 401」，
// 或授权码与令牌在网络上明文传。
func TestValidate_生产环境MCP公网地址必须https(t *testing.T) {
	c := validProduction()
	c.Auth = Auth{JWTSecret: strings.Repeat("j", 32), SecretKey: strings.Repeat("k", 32)}
	c.MCP = MCP{Enabled: true, ResultMaxTokens: 1, MediaURLTTL: 1, QuoteTTL: 1, WaitMax: 1}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "SEEAITV_MCP_PUBLIC_URL") || !strings.Contains(err.Error(), "SEEAITV_WEB_PUBLIC_URL") {
		t.Fatalf("两项都缺时应各报一条：%v", err)
	}
	c.MCP.PublicURL, c.MCP.WebPublicURL = "http://example.com/mcp", "https://example.com"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "SEEAITV_MCP_PUBLIC_URL") {
		t.Fatalf("http 应被拒：%v", err)
	}
	c.MCP.PublicURL = "https://example.com/mcp"
	if err := c.Validate(); err != nil {
		t.Fatalf("两项都是 https 时应通过：%v", err)
	}
	// 非 production 留空可以（按请求推导）。
	c.Env, c.MCP.PublicURL, c.MCP.WebPublicURL = EnvDev, "", ""
	if err := c.Validate(); err != nil {
		t.Fatalf("开发环境留空应通过：%v", err)
	}
}
