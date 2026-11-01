// Package preflight 是发布前的配置与环境自检（P14-04）。
//
// ⚠️ 它存在的理由是：**大部分发布事故不是代码错了，是环境没配对**，
// 而这些错误里绝大多数**不会让进程起不来** —— 它们让进程起来、健康检查全绿、
// 然后在第一个真实请求上失败。到那时发布已经被判定为成功。
//
// 所以自检必须满足三条：
//   - **在启动之前**能单独跑（`migrate preflight`），不必先把服务放上去；
//   - **一次报完所有问题**，不是撞到第一个就退出 —— 运维要的是一张清单，
//     而不是"修一个再跑一次"循环五轮；
//   - **区分「致命」与「提醒」**：把两者混在一起的话，人会开始忽略整份输出。
package preflight

import (
	"context"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/platform/config"
	"github.com/li904338203/tabtv/backend/internal/platform/migrate"
)

// Level 是一条检查结果的级别。
type Level int

const (
	// LevelOK 通过。
	LevelOK Level = iota
	// LevelWarn 提醒：不拦发布，但必须有人看一眼。
	LevelWarn
	// LevelFatal 致命：这样上线一定出问题。
	LevelFatal
)

func (l Level) String() string {
	switch l {
	case LevelFatal:
		return "致命"
	case LevelWarn:
		return "提醒"
	default:
		return "通过"
	}
}

// Finding 是一条检查结果。
type Finding struct {
	Level Level
	// Name 检查项，短句。
	Name string
	// Detail 说清**会怎样**，不是只说"配置不对"。
	Detail string
}

// Report 是一次自检的全部结果。
type Report struct {
	Findings []Finding
	Env      string
}

// Fatal 是否存在致命项。
func (r Report) Fatal() bool {
	for _, f := range r.Findings {
		if f.Level == LevelFatal {
			return true
		}
	}
	return false
}

// Counts 按级别统计。
func (r Report) Counts() (ok, warn, fatal int) {
	for _, f := range r.Findings {
		switch f.Level {
		case LevelFatal:
			fatal++
		case LevelWarn:
			warn++
		default:
			ok++
		}
	}
	return
}

type checker struct {
	rep *Report
}

func (c *checker) ok(name, detail string)    { c.add(LevelOK, name, detail) }
func (c *checker) warn(name, detail string)  { c.add(LevelWarn, name, detail) }
func (c *checker) fatal(name, detail string) { c.add(LevelFatal, name, detail) }
func (c *checker) add(l Level, name, detail string) {
	c.rep.Findings = append(c.rep.Findings, Finding{Level: l, Name: name, Detail: detail})
}

// Run 跑全部检查。pool 为 nil 时跳过需要数据库的那几项。
//
// ⚠️ **一次报完**：每一项都执行，不在第一个致命项处返回。
func Run(ctx context.Context, cfg config.Config, pool *pgxpool.Pool,
	fsys fs.FS, dir string) Report {
	rep := Report{Env: cfg.Env}
	c := &checker{rep: &rep}

	checkEnums(c, cfg)
	checkDevSwitches(c, cfg)
	checkSecrets(c, cfg)
	checkRotationConsistency(c, cfg)
	checkStorage(c, cfg)
	checkRetention(c, cfg)
	checkMarketing(c, cfg)
	checkMCP(c, cfg)
	if pool != nil {
		checkSchema(ctx, c, pool, fsys, dir)
		checkSeed(ctx, c, pool)
		checkAdmin(ctx, c, pool)
	} else {
		c.warn("数据库检查已跳过", "未提供数据库连接，schema 版本、seed 与管理员都没有核对")
	}
	return rep
}

// checkDevSwitches 开发开关。
//
// ⚠️ `config.Validate` 已经在 production 下拦了这几项，这里**重复一遍是有意的**：
// 自检要能在**部署之前**对着一份配置文件跑出结论，而不是把服务放上去撞一次。
func checkDevSwitches(c *checker, cfg config.Config) {
	prod := cfg.IsProduction()
	type sw struct {
		on   bool
		name string
		harm string
	}
	for _, s := range []sw{
		{cfg.Dev.FakeProvider, "SEEAITV_DEV_FAKE_PROVIDER",
			"假适配器会按协议码替换真实 Provider —— 用户付了积分，拿到的是假产物"},
		{cfg.Dev.FakePayment, "SEEAITV_DEV_FAKE_PAYMENT",
			"开发支付端点会直接把订单标成已付并发放权益，**任何人都能白拿会员**"},
		{cfg.Dev.AllowPrivateNetwork, "SEEAITV_DEV_ALLOW_PRIVATE_NETWORK",
			"允许出网到内网地址 —— 后台里配一个 Provider 地址就能探测内网（SSRF）"},
	} {
		switch {
		case s.on && prod:
			c.fatal("开发开关未关闭："+s.name, s.harm)
		case s.on:
			c.warn("开发开关开着："+s.name, "非 production 下允许，但"+s.harm)
		default:
			c.ok("开发开关已关闭："+s.name, "")
		}
	}
}

func checkSecrets(c *checker, cfg config.Config) {
	prod := cfg.IsProduction()

	switch {
	case cfg.Auth.JWTSecret == "" && prod:
		c.fatal("SEEAITV_JWT_SECRET 未配置", "production 下必须显式配置")
	case cfg.Auth.JWTSecret == "":
		c.warn("SEEAITV_JWT_SECRET 未配置",
			"启动时会生成一次性密钥 —— **每次重启所有用户都被登出**")
	case len(cfg.Auth.JWTSecret) < 32:
		c.fatal("SEEAITV_JWT_SECRET 过短", fmt.Sprintf("当前 %d 字节，至少 32", len(cfg.Auth.JWTSecret)))
	default:
		c.ok("SEEAITV_JWT_SECRET 已配置", "")
	}

	switch {
	case cfg.Auth.SecretKey == "" && prod:
		c.fatal("SEEAITV_SECRET_KEY 未配置",
			"落库凭据（Provider key、支付私钥、SMTP）需要独立的加密密钥")
	case cfg.Auth.SecretKey == "":
		c.warn("SEEAITV_SECRET_KEY 未配置", "会回落到 JWT 密钥 —— 仅限开发")
	case cfg.Auth.SecretKey == cfg.Auth.JWTSecret:
		c.fatal("SEEAITV_SECRET_KEY 与 JWT 密钥相同",
			"一次泄漏会同时失守会话与库里**全部**上游凭据")
	case len(cfg.Auth.SecretKey) < 32:
		c.fatal("SEEAITV_SECRET_KEY 过短", fmt.Sprintf("当前 %d 字节，至少 32", len(cfg.Auth.SecretKey)))
	default:
		c.ok("SEEAITV_SECRET_KEY 已配置且与 JWT 分开", "")
	}

	// 轮换状态。⚠️ 留着旧版本密钥**不是错误**，但它是一个"未完成的轮换"——
	// 说出来才不会在半年后被人当成无用配置删掉（删掉那一刻旧密文就解不开了）。
	cur := cfg.Auth.CurrentKeyVersion()
	if n := len(cfg.Auth.OldSecretKeys); n > 0 {
		c.warn(fmt.Sprintf("仍带着 %d 个历史密钥版本（当前 v%d）", n, cur),
			"说明轮换尚未收尾。跑 `migrate rotate-secrets`，**确认它报「可以安全摘掉」之后**"+
				"再摘掉 SEEAITV_SECRET_KEY_V<旧版本> —— 提前摘掉旧密文就永久解不开了")
	} else if cur > 1 {
		c.ok(fmt.Sprintf("密钥版本 v%d，无历史密钥残留", cur), "")
	}
}

func checkStorage(c *checker, cfg config.Config) {
	if cfg.Storage.Type == "local" {
		if cfg.IsProduction() {
			c.fatal("对象存储是 local",
				"local 无法预签名（参考图只能内联 base64，上游会拒收），"+
					"且 API 与 Worker 分机部署时互相读不到盘 —— 产物下载全部失败")
		} else {
			c.warn("对象存储是 local", "仅限单机开发；联调/压测要用 s3")
		}
		return
	}
	var missing []string
	if cfg.Storage.Bucket == "" {
		missing = append(missing, "BUCKET")
	}
	if cfg.Storage.AccessKey == "" {
		missing = append(missing, "ACCESS_KEY")
	}
	if cfg.Storage.SecretKey == "" {
		missing = append(missing, "SECRET_KEY")
	}
	if len(missing) > 0 {
		c.fatal("对象存储配置不全", "缺少 SEEAITV_STORAGE_"+strings.Join(missing, " / SEEAITV_STORAGE_"))
	} else {
		c.ok("对象存储配置完整（s3）", "")
	}
	// ⚠️ 签名有效期不能调太短：地址在**提交任务时**签发，上游可能排队几分钟
	// 才来取，失败重试更晚 —— 短 TTL 的症状是"偶发地取不到参考图"。
	if cfg.Storage.SignTTL > 0 && cfg.Storage.SignTTL < time.Hour {
		c.warn(fmt.Sprintf("签名有效期只有 %s", cfg.Storage.SignTTL),
			"地址在提交任务时签发，上游可能排队几分钟才来取 —— 过短会表现为偶发取不到参考图")
	}
}

// checkMCP 对外 MCP 的两个公网地址（MCPT-12）。
//
// ⚠️ 它们决定 OAuth 元数据里的 issuer / resource 与同意页地址。配错的症状极难排查：客户端
// 「授权成功但每次 401」（令牌受众与客户端连的地址对不上），或用户被送到一个打不开的同意页。
// config.Validate 在 production 下也会拦，这里重复是为了**部署之前**就能对着配置跑出结论。
func checkMCP(c *checker, cfg config.Config) {
	if !cfg.MCP.Enabled {
		c.ok("对外 MCP 未开启", "SEEAITV_MCP_ENABLED=false：/mcp 与 OAuth 端点都不挂")
		return
	}
	prod := cfg.IsProduction()
	for _, v := range []struct{ name, val, harm string }{
		{"SEEAITV_MCP_PUBLIC_URL", cfg.MCP.PublicURL,
			"OAuth 的 issuer 与受保护资源 resource 由它给出 —— 配错时客户端「授权成功但每次 401」"},
		{"SEEAITV_WEB_PUBLIC_URL", cfg.MCP.WebPublicURL,
			"OAuth 授权会把用户送到 <它>/mcp/authorize 同意页、花钱审批链接是 <它>/mcp/approve/… —— 配错时用户打开的是一个不存在的页面"},
	} {
		switch {
		case v.val == "" && prod:
			c.fatal(v.name+" 未配置", v.harm)
		case v.val == "":
			c.warn(v.name+" 未配置", "非 production 下按请求推导（仅限本地开发）；"+v.harm)
		case prod && !strings.HasPrefix(v.val, "https://"):
			c.fatal(v.name+" 不是 https", fmt.Sprintf("当前 %q：授权码与令牌会在网络上明文传", v.val))
		default:
			c.ok(v.name+" 已配置", v.val)
		}
	}
}

// checkRetention 留存期。
//
// ⚠️ 全部是**设计建议值**（PARAM-05 未定稿），所以这里**只报当前值不判对错** ——
// 判对错就等于把实现者挑的数字冒充成批准过的政策（同 CD-67）。
func checkRetention(c *checker, cfg config.Config) {
	c.ok("留存期（均为设计建议值，PARAM-05 未定稿）",
		fmt.Sprintf("回收站 %.0f 天 · 请求日志 %.0f 天",
			cfg.TrashRetention.Hours()/24, cfg.LogRetention.Hours()/24))
}

// checkMarketing 营销的两个上限（MKT-T-04，dev/01 §11）。
//
// ⚠️ 与留存期同一条：变体 4 / 每个商品 10 张图 / 配额 500·200 是**设计建议值**（MK-11 / MK-22 / MKT-PARAM-18），
// 这里**只报当前值不判对错**；坏值（≤0、非档位宽度）由 config.Validate 在启动时拒，不在这里重复。
func checkMarketing(c *checker, cfg config.Config) {
	if !cfg.Marketing.Enabled {
		c.ok("营销 Agent 已关闭", "SEEAITV_MARKETING_ENABLED=false：/marketing/* 不挂路由")
		return
	}
	c.ok("营销上限（均为设计建议值，只报不判）",
		fmt.Sprintf("变体上限 %d · 商品图上限 %d 张/个 · 推理看图宽度 %d · 工作区配额 商品 %d / 角色 %d",
			cfg.Marketing.MaxVariants, cfg.Marketing.MaxProductImages, cfg.Marketing.ReasoningImageWidth,
			cfg.Marketing.MaxProducts, cfg.Marketing.MaxCharacters))
}

func checkSchema(ctx context.Context, c *checker, pool *pgxpool.Pool, fsys fs.FS, dir string) {
	state, fatal, msg, err := migrate.CheckSchema(ctx, pool, fsys, dir)
	switch {
	case err != nil:
		c.fatal("无法读取 schema 版本", err.Error()+"（新库请先跑 `migrate up`）")
	case fatal:
		c.fatal(fmt.Sprintf("schema 落后：库 v%d，二进制需要 v%d", state.Applied, state.Expected), msg)
	case msg != "":
		c.warn(fmt.Sprintf("schema 超前：库 v%d，二进制 v%d", state.Applied, state.Expected), msg)
	default:
		c.ok(fmt.Sprintf("schema 版本一致（v%d）", state.Applied), "")
	}
}

// checkSeed 基础配置是否已写入。
//
// ⚠️ 空 seed 的症状**不是报错**：画布上一个节点都没有、模型下拉是空的、
// 管理员登录后每一页都 403（权限行不存在）—— 看起来像"功能没做"。
func checkSeed(ctx context.Context, c *checker, pool *pgxpool.Pool) {
	type q struct {
		name  string
		sql   string
		least int
		harm  string
	}
	for _, x := range []q{
		{"节点类型", "select count(*) from node_type", 1, "画布上一个节点都建不出来"},
		{"操作", "select count(*) from operation", 1, "任何生成都会报「操作不存在」"},
		{"权限", "select count(*) from permission", 1, "管理员登录后每一页都 403"},
		{"连线规则", "select count(*) from connection_rule", 1, "画布上连不了线"},
	} {
		var n int
		if err := pool.QueryRow(ctx, x.sql).Scan(&n); err != nil {
			c.fatal("无法检查 seed："+x.name, err.Error())
			continue
		}
		if n < x.least {
			c.fatal("seed 缺失："+x.name, x.harm+"（跑 `migrate seed`，它是幂等的）")
		} else {
			c.ok(fmt.Sprintf("seed 已写入：%s（%d 条）", x.name, n), "")
		}
	}
}

// checkAdmin 是否已有管理员。
//
// ⚠️ 没有管理员时系统**看起来是好的**，只是没有任何人能进后台配 Provider ——
// 而那是让整个系统能用的第一步。
func checkAdmin(ctx context.Context, c *checker, pool *pgxpool.Pool) {
	var n int
	if err := pool.QueryRow(ctx, `select count(*) from admin_user`).Scan(&n); err != nil {
		c.fatal("无法检查管理员", err.Error())
		return
	}
	if n == 0 {
		c.fatal("没有任何管理员",
			"没人能进后台配 Provider / 模型 / 价格 —— 跑 `migrate bootstrap`")
	} else {
		c.ok(fmt.Sprintf("管理员已存在（%d 个）", n), "")
	}
}

// checkEnums 取值合法性。
//
// ⚠️ 这几条与 `config.Validate` 重叠，但**判定只放一处**：自检把
// Validate 的错误再并一遍的话，同一个问题会在报告里出现两次 ——
// 8 条致命项其实只有 4 个问题，而**重复的噪声与混级别是同一种伤害**：
// 人会开始不读整份输出。所以这里自己判，`preflight` 命令不再合并 Validate。
func checkEnums(c *checker, cfg config.Config) {
	switch cfg.Env {
	case config.EnvDev, config.EnvStaging, config.EnvProduction:
		c.ok("运行环境："+cfg.Env, "")
	default:
		c.fatal("SEEAITV_ENV 取值非法："+cfg.Env, "应为 dev / staging / production")
	}
	switch cfg.Log.Format {
	case "json", "text":
	default:
		c.fatal("SEEAITV_LOG_FORMAT 取值非法："+cfg.Log.Format, "应为 json / text")
	}
	switch cfg.Storage.Type {
	case "s3", "local":
	default:
		c.fatal("SEEAITV_STORAGE_TYPE 取值非法："+cfg.Storage.Type, "应为 s3 / local")
	}
}

// checkRotationConsistency 轮换配置自洽性（与 config.Validate 同一条规则）。
//
// ⚠️ 两种都会让**新写入的密文用错版本号**，而错误要到第一次解密时才出现，
// 那时的报错指向数据而不是配置。
func checkRotationConsistency(c *checker, cfg config.Config) {
	cur := cfg.Auth.CurrentKeyVersion()
	if old, ok := cfg.Auth.OldSecretKeys[cur]; ok && old != cfg.Auth.SecretKey {
		c.fatal(fmt.Sprintf("SEEAITV_SECRET_KEY_V%d 与 SEEAITV_SECRET_KEY 冲突", cur),
			"同一个版本号被两处给出且取值不同 —— 写进去的密文用哪个 key 加的没人说得清")
	}
	for v := range cfg.Auth.OldSecretKeys {
		if v > cur {
			c.fatal(fmt.Sprintf("历史密钥版本 v%d 高于当前版本 v%d", v, cur),
				"轮换只能向前；版本号退回会让新密文用旧版本号，之后解不开")
		}
	}
}
