// Command canvasops 是**开发联调工具**（AGT-10，供 AGT-11 手工联调前端重放）：
// 把共享 fixture（api/fixtures/canvas-ops/*.json）里的 ops 以 Agent 身份应用到指定项目，
// 走的是与 Agent 完全相同的 canvas/app.ApplyOps（行锁事务 + 操作日志 + 经总线推 canvas.ops_applied）。
//
// ⚠️ 不进发布制品（release 只构建 cmd/*）。它会真实改写项目画布，只对开发库用。
//
// 环境变量与 `make run-api` 同一组（SEEAITV_PG_* / SEEAITV_REDIS_* …），所以要经 make 的
// 默认值跑，或自己导出那组变量：
//
//	cd backend && go run ./tools/canvasops -fixture ../api/fixtures/canvas-ops/001-add-node.json \
//	    -workspace <工作区ID> -project <项目ID> -user <用户ID> -session 7001
//
// 以 MCP 授权身份（MCPT-09）：加 `-actor mcp -grant 5001 -client "Claude Code"`（-session 被忽略）。
//
// fixture 里的 ops 若引用了 before 中的节点（002~006），先加 -reset：以真人身份把画布整份
// 保存成 fixture 的 before（走 SaveCanvas 乐观锁，会让打开着的前端看到一次真人改动）。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	"github.com/li904338203/tabtv/backend/internal/canvas/domain"
	canvasrepo "github.com/li904338203/tabtv/backend/internal/canvas/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/platform/bootstrap"
	"github.com/li904338203/tabtv/backend/internal/platform/config"
	"github.com/li904338203/tabtv/backend/internal/platform/eventbus"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	plog "github.com/li904338203/tabtv/backend/internal/platform/log"
)

// idNode 与 api / worker（节点 0）错开，避免同一毫秒内撞号。
const idNode = 1001

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "canvasops:", err)
		os.Exit(1)
	}
}

func run() error {
	fixture := flag.String("fixture", "", "fixture 文件路径（必填）")
	ws := flag.Int64("workspace", 0, "工作区 ID（必填）")
	project := flag.Int64("project", 0, "项目 ID（必填）")
	user := flag.Int64("user", 0, "代谁执行（会话发起人，必填）")
	session := flag.Int64("session", 7001, "Agent 会话 ID（-actor agent 时）")
	actorKind := flag.String("actor", domain.ActorAgent, "操作者种类：agent / mcp")
	grant := flag.Int64("grant", 0, "MCP 授权 ID（-actor mcp 时必填）")
	client := flag.String("client", "", "MCP 客户端名（-actor mcp 时写进节点 origin.clientName）")
	batch := flag.String("batch", "", "批次 ID（幂等键）；缺省按 fixture 名 + 时间戳生成")
	reset := flag.Bool("reset", false, "应用前先把画布整份保存成 fixture 的 before")
	flag.Parse()
	if *fixture == "" || *ws <= 0 || *project <= 0 || *user <= 0 {
		flag.Usage()
		return errors.New("-fixture / -workspace / -project / -user 必填")
	}

	actor := domain.Actor{Kind: *actorKind, UserID: *user}
	switch *actorKind {
	case domain.ActorAgent:
		actor.SessionID = *session
	case domain.ActorMCP:
		if *grant <= 0 {
			return errors.New("-actor mcp 时 -grant 必填")
		}
		actor.GrantID, actor.ClientName = *grant, *client
	default:
		return fmt.Errorf("-actor 只能是 agent / mcp，得到 %q", *actorKind)
	}

	raw, err := os.ReadFile(*fixture)
	if err != nil {
		return err
	}
	var f struct {
		Before json.RawMessage `json:"before"`
		Ops    []domain.Op     `json:"ops"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("fixture 解析失败: %w", err)
	}
	if *batch == "" {
		*batch = "dev:" + filepath.Base(*fixture) + ":" + strconv.FormatInt(time.Now().UnixMilli(), 36)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := plog.New(cfg.Log.Level, cfg.Log.Format)
	ctx := context.Background()
	deps, err := bootstrap.Open(ctx, cfg, bootstrap.Want{DB: true, Cache: true}, logger)
	if err != nil {
		return err
	}
	defer deps.Close(logger)

	gen, err := id.New(idNode)
	if err != nil {
		return err
	}
	r := canvasrepo.New(deps.DB.Pool)
	svc := canvasapp.New(r, gen.Next)
	svc.SetLogger(logger)
	if deps.Cache != nil {
		svc.SetEvents(eventbus.NewPublisher(deps.Cache.Client, deps.Cache.Key))
	}

	if *reset {
		p, err := r.Get(ctx, *ws, *project)
		if err != nil {
			return fmt.Errorf("取项目失败: %w", err)
		}
		res, err := svc.SaveCanvas(ctx, *ws, *project, *user, collab.RoleOwner, p.Version, f.Before)
		if err != nil {
			return fmt.Errorf("重置画布失败: %w", err)
		}
		fmt.Printf("已重置画布为 fixture 的 before：version=%d\n", res.Version)
	}

	res, err := svc.ApplyOps(ctx, canvasapp.ApplyOpsInput{
		WorkspaceID: *ws, ProjectID: *project, BatchID: *batch, Ops: f.Ops,
		Actor: actor,
	})
	if err != nil {
		return fmt.Errorf("应用失败: %w", err)
	}
	fmt.Printf("batch=%s version %d → %d nodeIds=%v replayed=%v\n",
		*batch, res.FromVersion, res.ToVersion, res.NodeIDs, res.Replayed)
	return nil
}
