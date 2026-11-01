package preflight

import (
	"context"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/platform/config"
)

func prod() config.Config {
	return config.Config{
		Env:      config.EnvProduction,
		Log:      config.Log{Format: "json"},
		Storage:  config.Storage{Type: "s3", Bucket: "b", AccessKey: "a", SecretKey: "s"},
		Auth:     config.Auth{JWTSecret: strings.Repeat("j", 32), SecretKey: strings.Repeat("k", 32)},
		Postgres: config.Postgres{},
	}
}

func run(cfg config.Config) Report {
	// pool=nil：只跑不依赖数据库的那几项。
	return Run(context.Background(), cfg, nil, nil, "")
}

func names(r Report, lvl Level) []string {
	var out []string
	for _, f := range r.Findings {
		if f.Level == lvl {
			out = append(out, f.Name)
		}
	}
	return out
}

func hasContaining(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// 一份干净的 production 配置不该有致命项。
func TestRun_干净配置无致命项(t *testing.T) {
	r := run(prod())
	if r.Fatal() {
		t.Fatalf("干净配置不该有致命项: %v", names(r, LevelFatal))
	}
}

// ⚠️ **一次报完**：撞到第一个致命项就返回的话，运维要"修一个再跑一次"循环五轮。
func TestRun_一次报完所有问题(t *testing.T) {
	cfg := prod()
	cfg.Dev.FakePayment = true
	cfg.Dev.FakeProvider = true
	cfg.Auth.JWTSecret = ""
	cfg.Storage.Type = "local"

	fatal := names(run(cfg), LevelFatal)
	for _, want := range []string{
		"SEEAITV_DEV_FAKE_PAYMENT", "SEEAITV_DEV_FAKE_PROVIDER",
		"SEEAITV_JWT_SECRET", "对象存储是 local",
	} {
		if !hasContaining(fatal, want) {
			t.Errorf("四个问题要一次报完，缺了 %q（实际 %v）", want, fatal)
		}
	}
}

// 加密密钥与签名密钥相同是致命的：一次泄漏同时失守会话与全部上游凭据。
func TestRun_加密密钥不得与JWT相同(t *testing.T) {
	cfg := prod()
	cfg.Auth.SecretKey = cfg.Auth.JWTSecret
	if !hasContaining(names(run(cfg), LevelFatal), "与 JWT 密钥相同") {
		t.Fatal("共用密钥必须判致命")
	}
}

// ⚠️ 留存期**只报当前值不判对错**：判对错等于把实现者挑的数字冒充成批准过的政策。
func TestRun_留存期只报不判(t *testing.T) {
	cfg := prod()
	cfg.TrashRetention = 1 // 荒谬的值
	r := run(cfg)
	for _, f := range r.Findings {
		if strings.Contains(f.Name, "留存期") && f.Level != LevelOK {
			t.Fatal("留存期是 PARAM-05 未定稿，不能判对错")
		}
	}
}

// ⚠️ 非 production 下开发开关是**提醒**不是致命 —— 否则本地永远跑不了自检。
func TestRun_开发环境下开关只提醒(t *testing.T) {
	cfg := prod()
	cfg.Env = config.EnvDev
	cfg.Dev.FakePayment = true
	r := run(cfg)
	if r.Fatal() {
		t.Fatalf("dev 下不该致命: %v", names(r, LevelFatal))
	}
	if !hasContaining(names(r, LevelWarn), "SEEAITV_DEV_FAKE_PAYMENT") {
		t.Fatal("dev 下仍要提醒 —— 不说的话它会被一路带到 staging")
	}
}

// 轮换未收尾要提醒：不说的话半年后会有人把"没用的"旧密钥配置删掉，
// 而删掉那一刻旧密文就永久解不开了。
func TestRun_轮换未收尾要提醒(t *testing.T) {
	cfg := prod()
	cfg.Auth.SecretKeyVersion = 2
	cfg.Auth.OldSecretKeys = map[int]string{1: "old"}
	if !hasContaining(names(run(cfg), LevelWarn), "历史密钥版本") {
		t.Fatal("带着历史密钥要提醒轮换尚未收尾")
	}
}

// 轮换配置自相矛盾是致命的（与 config.Validate 同一条规则，判定只放一处）。
func TestRun_轮换配置自相矛盾判致命(t *testing.T) {
	cfg := prod()
	cfg.Auth.SecretKeyVersion = 1
	cfg.Auth.OldSecretKeys = map[int]string{1: "别的值"}
	if !hasContaining(names(run(cfg), LevelFatal), "冲突") {
		t.Fatal("同一版本号取值冲突必须判致命")
	}

	cfg2 := prod()
	cfg2.Auth.SecretKeyVersion = 1
	cfg2.Auth.OldSecretKeys = map[int]string{2: "x"}
	if !hasContaining(names(run(cfg2), LevelFatal), "高于当前版本") {
		t.Fatal("历史版本号高于当前版本必须判致命")
	}
}

// 数据库检查被跳过时要说出来 —— 否则一份"全绿"的报告会让人以为 schema 也核过了。
func TestRun_跳过数据库检查要说出来(t *testing.T) {
	if !hasContaining(names(run(prod()), LevelWarn), "数据库检查已跳过") {
		t.Fatal("跳过了就要说，否则全绿的报告会被误读成「什么都核过了」")
	}
}

// 开着 MCP 的 production 配置缺公网地址 / 不是 https 要判致命（MCPT-12）；开发环境只提醒。
func TestRun_MCP公网地址(t *testing.T) {
	cfg := prod()
	cfg.MCP.Enabled = true
	r := run(cfg)
	fatal := names(r, LevelFatal)
	if !hasContaining(fatal, "SEEAITV_MCP_PUBLIC_URL") || !hasContaining(fatal, "SEEAITV_WEB_PUBLIC_URL") {
		t.Fatalf("两项都缺应各一条致命项: %v", fatal)
	}
	cfg.MCP.PublicURL, cfg.MCP.WebPublicURL = "http://example.com/mcp", "https://example.com"
	if f := names(run(cfg), LevelFatal); !hasContaining(f, "SEEAITV_MCP_PUBLIC_URL 不是 https") {
		t.Fatalf("http 应判致命: %v", f)
	}
	cfg.MCP.PublicURL = "https://example.com/mcp"
	if r := run(cfg); r.Fatal() {
		t.Fatalf("两项都配好不该有致命项: %v", names(r, LevelFatal))
	}
	cfg.Env, cfg.MCP.PublicURL, cfg.MCP.WebPublicURL = config.EnvDev, "", ""
	r = run(cfg)
	if hasContaining(names(r, LevelFatal), "PUBLIC_URL") || !hasContaining(names(r, LevelWarn), "SEEAITV_WEB_PUBLIC_URL") {
		t.Fatalf("开发环境留空只提醒: fatal=%v warn=%v", names(r, LevelFatal), names(r, LevelWarn))
	}
}

// 营销的两个上限是设计建议值（MK-11 / MK-12）：只报当前值，不判对错（同留存期）。
func TestRun_营销上限只报不判(t *testing.T) {
	cfg := prod()
	cfg.Marketing = config.Marketing{Enabled: true, MaxVariants: 999, MaxProductImages: 1, ReasoningImageWidth: 640,
		MaxProducts: 7, MaxCharacters: 8}
	r := run(cfg)
	found := false
	for _, f := range r.Findings {
		if strings.Contains(f.Name, "营销上限") {
			found = true
			if f.Level != LevelOK {
				t.Fatal("营销上限是设计建议值，不能判对错")
			}
			if !strings.Contains(f.Detail, "999") || !strings.Contains(f.Detail, "商品图上限 1") ||
				!strings.Contains(f.Detail, "商品 7") || !strings.Contains(f.Detail, "角色 8") {
				t.Fatalf("要报出当前值：%q", f.Detail)
			}
		}
	}
	if !found {
		t.Fatal("缺少营销上限提醒项")
	}
}
