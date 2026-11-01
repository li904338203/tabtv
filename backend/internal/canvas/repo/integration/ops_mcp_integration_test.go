//go:build integration

// 画布来源泛化（MCPT-09）的真库测试：MCP 授权作为第三种发起方走同一条 ApplyOps。
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	"github.com/li904338203/tabtv/backend/internal/canvas/domain"
	"github.com/li904338203/tabtv/backend/internal/canvas/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

const opsGrant = int64(5001)

func mcpActor() domain.Actor {
	return domain.Actor{Kind: domain.ActorMCP, UserID: opsUser, GrantID: opsGrant, ClientName: "Claude Code"}
}

func TestApplyOps_MCP与SaveCanvas并发版本连续且改动都在(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	svc := canvasapp.New(r, next)
	id := mustInsert(t, r, next, ws, "MCP 并发")

	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 2*n)
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := svc.ApplyOps(ctx, canvasapp.ApplyOpsInput{WorkspaceID: ws, ProjectID: id,
				Actor: mcpActor(), BatchID: fmt.Sprintf("mcp:%d", i), Ops: addNodeOp(fmt.Sprintf("mcp-%d", i))})
			if err != nil {
				errs <- fmt.Errorf("ApplyOps %d: %w", i, err)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			<-start
			nodeID := fmt.Sprintf("u_%d", i)
			p, err := r.GetWithCanvas(ctx, ws, id)
			if err != nil {
				errs <- err
				return
			}
			doc, version := withUserNode(t, p.CanvasData, nodeID), p.Version
			for attempt := 0; attempt < 200; attempt++ {
				_, err := svc.SaveCanvas(ctx, ws, id, opsUser+int64(i)+1, collab.RoleEditor, version, doc)
				if err == nil {
					return
				}
				if bizCode(err) != errcode.VersionConflict {
					errs <- fmt.Errorf("SaveCanvas %d: %w", i, err)
					return
				}
				res, err := svc.OpsSince(ctx, ws, id, collab.RoleEditor, version)
				if err != nil {
					errs <- err
					return
				}
				if res.NeedsReload {
					p, err := r.GetWithCanvas(ctx, ws, id)
					if err != nil {
						errs <- err
						return
					}
					doc, version = withUserNode(t, p.CanvasData, nodeID), p.Version
					continue
				}
				doc = replay(t, doc, res.Batches)
				version = res.CurrentVersion
			}
			errs <- fmt.Errorf("SaveCanvas %d 重试耗尽", i)
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		return
	}

	p, _ := r.GetWithCanvas(ctx, ws, id)
	if p.Version != 1+2*n {
		t.Fatalf("最终版本应为 %d，实际 %d", 1+2*n, p.Version)
	}
	var tree struct {
		Nodes []map[string]any `json:"nodes"`
	}
	if err := json.Unmarshal(p.CanvasData, &tree); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, nd := range tree.Nodes {
		title, _ := nd["title"].(string)
		seen[title]++
		// MCP 建的节点都带服务端强写的 origin，真人保存（含重放）没把它弄丢。
		if _, hasOrigin := nd["origin"]; strings.HasPrefix(title, "mcp-") && !hasOrigin {
			t.Errorf("MCP 节点 %s 丢了 origin：%v", title, nd)
		}
	}
	for i := 0; i < n; i++ {
		if seen[fmt.Sprintf("mcp-%d", i)] != 1 || seen[fmt.Sprintf("u_%d", i)] != 1 {
			t.Errorf("第 %d 组改动丢了或重复了: %v", i, seen)
		}
	}
	var logs, mcpLogs int
	_ = pool.QueryRow(ctx, `select count(*), count(*) filter (where actor_kind = 'mcp' and mcp_grant_id = $2
		and agent_session_id is null) from canvas_op_log where project_id = $1`, id, opsGrant).Scan(&logs, &mcpLogs)
	if logs != n || mcpLogs != n {
		t.Errorf("应有 %d 条 mcp 日志（带授权、无会话），实际 %d / %d", n, mcpLogs, logs)
	}
}

func TestCanvasState_MCP写入后editorKind为mcp(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	svc := canvasapp.New(r, next)
	id := mustInsert(t, r, next, ws, "MCP 编辑者")

	// 先让 Agent 写一次，再让 MCP 写：会话列必须被清掉，不然两种机器来源同时在场。
	if _, err := svc.ApplyOps(ctx, canvasapp.ApplyOpsInput{WorkspaceID: ws, ProjectID: id,
		Actor: agentActor(), BatchID: "a1", Ops: addNodeOp("a")}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApplyOps(ctx, canvasapp.ApplyOpsInput{WorkspaceID: ws, ProjectID: id,
		Actor: mcpActor(), BatchID: "m1", Ops: addNodeOp("m")}); err != nil {
		t.Fatal(err)
	}
	st, err := svc.CanvasState(ctx, ws, id, collab.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	if st.EditorKind != repo.EditorMCP || st.McpGrantID != opsGrant || st.AgentSessionID != 0 ||
		st.EditorID != opsUser {
		t.Fatalf("MCP 写入后应为 mcp/%d/本人 %d，得到 %+v", opsGrant, opsUser, st)
	}
	// ⚠️ 本人：EditorID 就是他，但 bySelf 必须是 false（否则前端说「你在另一个窗口改过」）。
	if canvasapp.BySelf(st, opsUser) {
		t.Error("MCP 写入不能算本人亲手保存")
	}
	_, err = svc.SaveCanvas(ctx, ws, id, opsUser, collab.RoleEditor, 2, json.RawMessage(`{"nodes":[]}`))
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.VersionConflict {
		t.Fatalf("应冲突，得到 %v", err)
	}
	if be.Details["editorKind"] != repo.EditorMCP || be.Details["mcpGrantId"] != "5001" ||
		be.Details["agentSessionId"] != "" || be.Details["bySelf"] != false {
		t.Errorf("冲突详情不对: %v", be.Details)
	}
	// 补取接口给出的 actor 带授权、客户端名为 null（日志不存它）。
	res, err := svc.OpsSince(ctx, ws, id, collab.RoleViewer, 2)
	if err != nil || len(res.Batches) != 1 {
		t.Fatalf("补取得到 %+v, %v", res, err)
	}
	a := canvasapp.NewOpActor(res.Batches[0])
	if a.Kind != "mcp" || a.McpGrantID == nil || *a.McpGrantID != "5001" || a.AgentSessionID != nil ||
		a.ClientName != nil {
		t.Errorf("补取的 actor 不对: %+v", a)
	}
}

func TestSaveCanvas_清空MCP编辑者(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	svc := canvasapp.New(r, next)
	id := mustInsert(t, r, next, ws, "清空 MCP")

	if _, err := svc.ApplyOps(ctx, canvasapp.ApplyOpsInput{WorkspaceID: ws, ProjectID: id,
		Actor: mcpActor(), BatchID: "m1", Ops: addNodeOp("m")}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SaveCanvas(ctx, ws, id, opsUser, collab.RoleEditor, 2, json.RawMessage(`{"nodes":[]}`)); err != nil {
		t.Fatal(err)
	}
	st, _ := svc.CanvasState(ctx, ws, id, collab.RoleViewer)
	if st.EditorKind != repo.EditorUser || st.McpGrantID != 0 || !canvasapp.BySelf(st, opsUser) {
		t.Fatalf("真人保存后应为 user 且清空授权，得到 %+v", st)
	}
}

func TestApplyOps_MCP与Agent互不可改(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	svc := canvasapp.New(r, next)
	id := mustInsert(t, r, next, ws, "互不可改")

	agentRes, err := svc.ApplyOps(ctx, canvasapp.ApplyOpsInput{WorkspaceID: ws, ProjectID: id,
		Actor: agentActor(), BatchID: "a1", Ops: addNodeOp("agent 的")})
	if err != nil {
		t.Fatal(err)
	}
	mcpRes, err := svc.ApplyOps(ctx, canvasapp.ApplyOpsInput{WorkspaceID: ws, ProjectID: id,
		Actor: mcpActor(), BatchID: "m1", Ops: addNodeOp("mcp 的")})
	if err != nil {
		t.Fatal(err)
	}
	agentNode, mcpNode := agentRes.NodeIDs[0], mcpRes.NodeIDs[0]
	update := func(actor domain.Actor, batch, node string) error {
		_, err := svc.ApplyOps(ctx, canvasapp.ApplyOpsInput{WorkspaceID: ws, ProjectID: id, Actor: actor,
			BatchID: batch, Ops: []domain.Op{{Kind: domain.OpUpdateNode, NodeID: node,
				Patch: json.RawMessage(`{"title":"改"}`)}}})
		return err
	}
	otherGrant := mcpActor()
	otherGrant.GrantID = opsGrant + 1
	for _, c := range []struct {
		name  string
		actor domain.Actor
		node  string
	}{
		{"MCP 改 Agent 的", mcpActor(), agentNode},
		{"Agent 改 MCP 的", agentActor(), mcpNode},
		{"别的授权改 MCP 的", otherGrant, mcpNode},
	} {
		if err := update(c.actor, "x:"+c.name, c.node); bizCode(err) != errcode.CanvasOpNotOwned ||
			!errors.Is(err, domain.ErrNotOwned) {
			t.Errorf("%s 应报 CANVAS_OP_NOT_OWNED，得到 %v", c.name, err)
		}
	}
	if err := update(mcpActor(), "ok:mcp", mcpNode); err != nil {
		t.Errorf("MCP 改自己的应成功：%v", err)
	}
	if err := update(agentActor(), "ok:agent", agentNode); err != nil {
		t.Errorf("Agent 改自己的应成功：%v", err)
	}

	// 用户改过（前端 markTouched 写 origin.userTouched）之后，MCP 也不能再改。
	p, _ := r.GetWithCanvas(ctx, ws, id)
	var tree map[string]any
	_ = json.Unmarshal(p.CanvasData, &tree)
	for _, raw := range tree["nodes"].([]any) {
		n := raw.(map[string]any)
		if n["id"] == mcpNode {
			n["origin"].(map[string]any)["userTouched"] = true
		}
	}
	doc, _ := json.Marshal(tree)
	if _, err := svc.SaveCanvas(ctx, ws, id, opsUser, collab.RoleEditor, p.Version, doc); err != nil {
		t.Fatal(err)
	}
	if err := update(mcpActor(), "x:touched", mcpNode); bizCode(err) != errcode.CanvasOpNotOwned {
		t.Errorf("用户改过的 MCP 节点应报 CANVAS_OP_NOT_OWNED，得到 %v", err)
	}
	var origin string
	_ = pool.QueryRow(ctx, `select n->'origin'->>'grantId' from project, jsonb_array_elements(canvas_data->'nodes') n
		where id = $1 and n->>'id' = $2`, id, mcpNode).Scan(&origin)
	if origin != "5001" {
		t.Errorf("落库的 origin.grantId 应为 5001，得到 %q", origin)
	}
}
