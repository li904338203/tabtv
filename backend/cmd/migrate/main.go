// Command migrate 是 schema 迁移与初始化入口。
//
// 职责（08 §2）：schema 校验、数据对账、资源扫描、死信重放（受审计）。
// 不负责：绕过领域服务直接修改余额或订单。
//
// 迁移文件以 embed.FS 打进二进制（ADR-0001 TECH-04），交付物只有一个文件，
// 不依赖目录挂载 —— 这是新环境独立上线（FULL-09）的前提。
//
// 新库从 V1 直接建最终结构，**不重放旧 Flyway 的 V1~V60**。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"log/slog"
	"os"

	"github.com/li904338203/tabtv/backend/internal/platform/bootstrapadmin"
	"github.com/li904338203/tabtv/backend/internal/platform/buildinfo"
	"github.com/li904338203/tabtv/backend/internal/platform/config"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	plog "github.com/li904338203/tabtv/backend/internal/platform/log"
	"github.com/li904338203/tabtv/backend/internal/platform/migrate"
	"github.com/li904338203/tabtv/backend/internal/platform/preflight"
	"github.com/li904338203/tabtv/backend/internal/platform/secret"
	"github.com/li904338203/tabtv/backend/internal/platform/seed"
	"github.com/li904338203/tabtv/backend/migrations"
)

const usage = `用法: migrate <command>

命令:
  up        执行全部未应用的迁移
  down      回滚最近一次迁移
  status    显示迁移状态
  seed      幂等写入基础配置（节点/operation/权限/默认 Prompt）
  bootstrap 创建首个管理员与 RBAC
  preflight       发布前自检：配置/开发开关/密钥/存储/schema/seed/管理员（P14-04）
  rotate-secrets  把存量密文重写成当前密钥版本（P14-03）
  storage-check   对当前配置的对象存储做一轮读写探测（含用预签名地址裸取）
  storage-copy    把旧存储（SEEAITV_COPY_SRC_*）的对象按原键拷到当前存储；幂等，可重跑
  version   显示构建信息

注意: seed 与 bootstrap 必须可重复执行（INIT-01/02）。
      seed 只含非用户业务配置，不含任何用户、余额、订单、项目或媒体。

密钥轮换（rotate-secrets）：
  1. 把当前 SEEAITV_SECRET_KEY 原样抄到 SEEAITV_SECRET_KEY_V<当前版本>
  2. SEEAITV_SECRET_KEY 换成新材料，SEEAITV_SECRET_KEY_VERSION 加一
  3. 重启 api 与 worker（新写入用新版本，旧密文仍解得开，不停机）
  4. 跑 rotate-secrets —— 它幂等，中断了直接重跑
  5. **确认它报「可以安全摘掉」之后**，再把 SEEAITV_SECRET_KEY_V<旧版本> 摘掉
  提前摘掉旧密钥，剩下的旧版本密文就永久解不开了。
`

func main() {
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()

	if err := run(flag.Arg(0)); err != nil {
		slog.Error("migrate 执行失败", "error", err)
		os.Exit(1)
	}
}

func run(cmd string) error {
	if cmd == "version" {
		info := buildinfo.Get()
		fmt.Printf("version=%s commit=%s buildTime=%s go=%s\n",
			info.Version, info.Commit, info.BuildTime, info.GoVersion)
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		// ⚠️ `preflight` 的职责就是把配置问题**一次报完**，所以它不能被
		// 配置校验挡在门外 —— 否则运维只看得到前半截，修完再跑才看到后半截。
		if cmd != "preflight" {
			return err
		}
		cfg = config.LoadRaw()
	}
	logger := plog.New(cfg.Log.Level, cfg.Log.Format)
	slog.SetDefault(logger)

	runner, err := migrate.Open(cfg.Postgres.DSN, migrations.FS, migrations.Dir, os.Stdout)
	if err != nil {
		return err
	}
	defer runner.Close()

	ctx := context.Background()
	switch cmd {
	case "up":
		if err := runner.Up(ctx); err != nil {
			return err
		}
		v, _ := runner.Version(ctx)
		logger.Info("迁移完成", "version", v)
		return nil
	case "down":
		return runner.Down(ctx)
	case "status":
		return runner.Status(ctx)
	case "seed":
		gen, err := id.New(0)
		if err != nil {
			return err
		}
		if err := seed.Apply(ctx, runner.DB(), gen.Next); err != nil {
			return err
		}
		logger.Info("seed 完成（幂等，可重复执行）")
		return nil
	case "bootstrap":
		gen, err := id.New(0)
		if err != nil {
			return err
		}
		res, err := bootstrapadmin.Apply(ctx, runner.DB(), gen.Next,
			os.Getenv("SEEAITV_BOOTSTRAP_ADMIN"), os.Getenv("SEEAITV_BOOTSTRAP_PASSWORD"))
		if err != nil {
			return err
		}
		if !res.Created {
			logger.Info("管理员已存在，跳过（不会重置密码）", "username", res.Username)
			return nil
		}
		logger.Info("管理员已创建", "username", res.Username)
		if res.Password != "" {
			// 随机密码只打印这一次，不落库也不进结构化日志字段（会被日志采集抓走）。
			fmt.Printf("\n初始密码（仅显示一次，请立即保存并修改）: %s\n\n", res.Password)
		}
		return nil
	case "rotate-secrets":
		// 把存量密文重写成当前密钥版本（P14-03）。
		//
		// ⚠️ 跑之前必须同时配好新旧密钥（`SEEAITV_SECRET_KEY` +
		// `SEEAITV_SECRET_KEY_V<旧版本>`），否则旧密文解不开 —— 那时
		// 这个命令会**报错退出而不写坏任何一行**。
		if cfg.Auth.SecretKey == "" {
			return errors.New("未配置 SEEAITV_SECRET_KEY，无法轮换")
		}
		cipher, err := secret.New(cfg.Auth.SecretKeys(), cfg.Auth.CurrentKeyVersion())
		if err != nil {
			return err
		}
		rep, err := secret.Rotate(ctx, runner.DB(), cipher)
		if err != nil {
			return err
		}
		logger.Info("密钥轮换完成",
			"currentVersion", cfg.Auth.CurrentKeyVersion(),
			"rewritten", rep.Rewritten, "skipped", rep.Skipped,
			"remaining", rep.Remaining, "versions", fmt.Sprint(rep.Versions))
		if rep.Remaining > 0 {
			// ⚠️ 这里**必须报错退出**，不能只打一条 warn：唯一会读这行输出的人
			// 正准备去摘掉旧 key，而此刻摘掉就意味着那些密文永久解不开。
			// 让命令以非零码结束，才能挡住脚本里的下一步。
			return fmt.Errorf(
				"仍有 %d 处密文不是当前版本 v%d —— **不要摘掉旧密钥**，"+
					"先查明是哪张表（版本分布: %v）",
				rep.Remaining, cfg.Auth.CurrentKeyVersion(), rep.Versions)
		}
		fmt.Printf("\n全部密文已是 v%d，可以安全摘掉 SEEAITV_SECRET_KEY_V<旧版本>。\n\n",
			cfg.Auth.CurrentKeyVersion())
		return nil
	case "storage-check":
		return storageCheck(ctx, cfg.Storage)
	case "storage-copy":
		return storageCopy(ctx, cfg.Storage)
	case "preflight":
		// 发布前自检（P14-04）。⚠️ 一次报完所有问题，不是撞到第一个就退出 ——
		// 运维要的是一张清单，而不是"修一个再跑一次"循环五轮。
		pool, perr := pgxpool.New(ctx, cfg.Postgres.DSN)
		if perr != nil {
			logger.Warn("无法连接数据库，将跳过依赖库的检查", "error", perr)
			pool = nil
		} else {
			defer pool.Close()
			if err := pool.Ping(ctx); err != nil {
				logger.Warn("数据库 ping 失败，将跳过依赖库的检查", "error", err)
				pool.Close()
				pool = nil
			}
		}
		// ⚠️ **不把 `config.Validate` 的错误再并一遍**：preflight 自己覆盖了
		// 同一组规则，并且说得更清楚（每条都写明"会怎样"）。合并的结果是
		// 一个问题报两次 —— 4 个真实问题显示成 8 条致命项，而重复的噪声
		// 与混级别是同一种伤害：人会开始不读整份输出。判定只放一处。
		rep := preflight.Run(ctx, cfg, pool, migrations.FS, migrations.Dir)
		okN, warnN, fatalN := rep.Counts()
		fmt.Printf("\n发布前自检（env=%s）\n\n", rep.Env)
		for _, f := range rep.Findings {
			mark := "  ok "
			switch f.Level {
			case preflight.LevelFatal:
				mark = "FATAL"
			case preflight.LevelWarn:
				mark = " warn"
			}
			if f.Detail == "" {
				fmt.Printf("%s  %s\n", mark, f.Name)
			} else {
				fmt.Printf("%s  %s\n         %s\n", mark, f.Name, f.Detail)
			}
		}
		fmt.Printf("\n通过 %d · 提醒 %d · 致命 %d\n\n", okN, warnN, fatalN)
		if rep.Fatal() {
			// ⚠️ 非零退出码是这条命令的**全部意义**：发布脚本靠它卡住下一步。
			// 只打印不退码的话，它就只是一段没人读的日志。
			return fmt.Errorf("自检发现 %d 个致命项，不要继续发布", fatalN)
		}
		return nil
	case "":
		flag.Usage()
		return errors.New("缺少子命令")
	default:
		flag.Usage()
		return fmt.Errorf("未知子命令: %q", cmd)
	}
}
