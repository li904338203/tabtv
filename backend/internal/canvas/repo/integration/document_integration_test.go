//go:build integration

// 项目文档（AGT-09）的真库测试。
//
// ⚠️ 必须真库：部分唯一索引（只约束 active 行）、乐观锁的 0 行判定、
// workspace/project 双条件、复制时的同事务 —— 都由 SQL 本身决定。
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	"github.com/li904338203/tabtv/backend/internal/canvas/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

func codeOf(err error) errcode.Code {
	var be *httpx.BizError
	if errors.As(err, &be) {
		return be.Code
	}
	return 0
}

func newDoc(t *testing.T, svc *canvasapp.Service, workspace, projectID int64, path, content string) repo.Document {
	t.Helper()
	d, err := svc.CreateDocument(context.Background(), workspace, projectID,
		canvasapp.DocumentActor{UserID: 1}, collab.RoleOwner,
		canvasapp.CreateDocumentInput{Path: path, Content: content})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDocument_同项目同路径唯一(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	svc := canvasapp.New(r, next)
	p1 := mustInsert(t, r, next, ws, "项目一")
	p2 := mustInsert(t, r, next, ws, "项目二")

	newDoc(t, svc, ws, p1, "/plan.md", "a")
	_, err := svc.CreateDocument(ctx, ws, p1, canvasapp.DocumentActor{UserID: 1}, collab.RoleOwner,
		canvasapp.CreateDocumentInput{Path: "/plan.md"})
	if codeOf(err) != errcode.DuplicateName {
		t.Fatalf("同项目同路径应报 40904，实际 %v", err)
	}
	// 另一个项目同路径不冲突。
	newDoc(t, svc, ws, p2, "/plan.md", "b")

	// 软删后同路径可以重建（部分唯一索引只约束 active）。
	if _, err := pool.Exec(ctx, `update project_document set status='deleted' where project_id=$1`, p1); err != nil {
		t.Fatal(err)
	}
	newDoc(t, svc, ws, p1, "/plan.md", "c")

	// 路径白名单。
	_, err = svc.CreateDocument(ctx, ws, p1, canvasapp.DocumentActor{UserID: 1}, collab.RoleOwner,
		canvasapp.CreateDocumentInput{Path: "/notes/../plan.md"})
	if codeOf(err) != errcode.InvalidArgument {
		t.Fatalf("含 .. 的路径应报参数错误，实际 %v", err)
	}
}

func TestDocument_旧版本保存冲突(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	svc := canvasapp.New(r, next)
	pid := mustInsert(t, r, next, ws, "项目")
	d := newDoc(t, svc, ws, pid, "/notes/a.md", "v1")
	if d.Version != 1 || d.Source != "user" {
		t.Fatalf("新文档应为 version 1 / user，实际 %d / %s", d.Version, d.Source)
	}

	saved, err := svc.UpdateDocument(ctx, ws, pid, d.ID, canvasapp.DocumentActor{UserID: 2},
		collab.RoleEditor, 1, "v2")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Version != 2 || saved.Content != "v2" || saved.EditorID != 2 {
		t.Fatalf("保存结果不对: %+v", saved)
	}

	// 拿旧版本再存 → 40902，且详情说得出是谁改的。
	_, err = svc.UpdateDocument(ctx, ws, pid, d.ID, canvasapp.DocumentActor{UserID: 1},
		collab.RoleOwner, 1, "我的旧稿")
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.VersionConflict {
		t.Fatalf("旧版本保存应报 40902，实际 %v", err)
	}
	if be.Details["currentVersion"] != 2 || be.Details["bySelf"] != false {
		t.Errorf("冲突详情不对: %v", be.Details)
	}
	cur, _ := svc.GetDocument(ctx, ws, pid, d.ID, collab.RoleViewer)
	if cur.Content != "v2" {
		t.Errorf("冲突不该覆盖内容，实际 %q", cur.Content)
	}

	// 只读成员能读不能写。
	_, err = svc.UpdateDocument(ctx, ws, pid, d.ID, canvasapp.DocumentActor{UserID: 3},
		collab.RoleViewer, 2, "x")
	if codeOf(err) != errcode.PermissionDenied {
		t.Fatalf("只读成员保存应报 403，实际 %v", err)
	}

	// 不存在的文档报 404 而不是冲突。
	_, err = svc.UpdateDocument(ctx, ws, pid, 424242, canvasapp.DocumentActor{UserID: 1},
		collab.RoleOwner, 1, "x")
	if codeOf(err) != errcode.NotFound {
		t.Fatalf("不存在的文档应报 404，实际 %v", err)
	}
}

func TestDocument_工作区隔离(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	svc := canvasapp.New(r, next)
	pid := mustInsert(t, r, next, ws, "项目")
	d := newDoc(t, svc, ws, pid, "/plan.md", "秘密")

	const other = int64(9002)
	otherProject := mustInsert(t, r, next, other, "别人的项目")
	if _, err := svc.GetDocument(ctx, other, pid, d.ID, collab.RoleOwner); codeOf(err) != errcode.NotFound {
		t.Errorf("跨工作区读应 404，实际 %v", err)
	}
	// 带着自己的项目 ID 去读别人的文档 ID，也是 404。
	if _, err := svc.GetDocument(ctx, other, otherProject, d.ID, collab.RoleOwner); codeOf(err) != errcode.NotFound {
		t.Errorf("用自己的项目读别人的文档应 404，实际 %v", err)
	}
	if _, err := svc.UpdateDocument(ctx, other, pid, d.ID, canvasapp.DocumentActor{UserID: 9},
		collab.RoleOwner, 1, "x"); codeOf(err) != errcode.NotFound {
		t.Errorf("跨工作区写应 404，实际 %v", err)
	}
	list, err := svc.ListDocuments(ctx, other, otherProject, collab.RoleOwner)
	if err != nil || len(list) != 0 {
		t.Errorf("别人的项目文档列表应为空，实际 %v %v", list, err)
	}
}

func TestDocument_回收站项目不可写(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	svc := canvasapp.New(r, next)
	pid := mustInsert(t, r, next, ws, "项目")
	d := newDoc(t, svc, ws, pid, "/plan.md", "v1")
	if err := r.SoftDelete(ctx, ws, pid); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateDocument(ctx, ws, pid, d.ID, canvasapp.DocumentActor{UserID: 1},
		collab.RoleOwner, 1, "x"); codeOf(err) != errcode.InvalidStateTransition {
		t.Errorf("回收站项目保存文档应报 40905，实际 %v", err)
	}
	if _, err := svc.CreateDocument(ctx, ws, pid, canvasapp.DocumentActor{UserID: 1}, collab.RoleOwner,
		canvasapp.CreateDocumentInput{Path: "/notes/b.md"}); codeOf(err) != errcode.InvalidStateTransition {
		t.Errorf("回收站项目建文档应报 40905，实际 %v", err)
	}
	// 读仍可以。
	if _, err := svc.GetDocument(ctx, ws, pid, d.ID, collab.RoleViewer); err != nil {
		t.Errorf("回收站项目的文档应可读: %v", err)
	}
}

func TestDuplicate_文档随项目复制且docId重绑(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	svc := canvasapp.New(r, next)
	pid := mustInsert(t, r, next, ws, "原项目")
	d := newDoc(t, svc, ws, pid, "/plan.md", "原文")

	canvas := json.RawMessage(`{"nodes":[{"id":"n1","type":"document","title":"规划",
		"position":{"x":0,"y":0},"params":{"docId":"` + itoa10(d.ID) + `"}}],"edges":[]}`)
	if _, err := r.SaveCanvas(ctx, ws, pid, 1, 1, canvas); err != nil {
		t.Fatal(err)
	}

	cp, err := svc.Duplicate(ctx, ws, 1, pid, collab.RoleOwner, "")
	if err != nil {
		t.Fatal(err)
	}
	docs, err := svc.ListDocuments(ctx, ws, cp.ID, collab.RoleViewer)
	if err != nil || len(docs) != 1 {
		t.Fatalf("副本应有 1 份文档，实际 %v %v", docs, err)
	}
	copyDoc := docs[0]
	if copyDoc.ID == d.ID || copyDoc.Path != "/plan.md" {
		t.Fatalf("副本文档 ID 应不同、路径相同: %+v", copyDoc)
	}

	got, err := r.GetWithCanvas(ctx, ws, cp.ID)
	if err != nil {
		t.Fatal(err)
	}
	var tree struct {
		Nodes []struct {
			Params map[string]any `json:"params"`
		} `json:"nodes"`
	}
	_ = json.Unmarshal(got.CanvasData, &tree)
	if tree.Nodes[0].Params["docId"] != itoa10(copyDoc.ID) {
		t.Fatalf("副本节点的 docId 应重绑到 %d，实际 %v", copyDoc.ID, tree.Nodes[0].Params["docId"])
	}

	// 各自独立：改副本不影响原件。
	full, _ := svc.GetDocument(ctx, ws, cp.ID, copyDoc.ID, collab.RoleViewer)
	if full.Content != "原文" || full.Version != 1 {
		t.Fatalf("副本内容/版本不对: %+v", full)
	}
	if _, err := svc.UpdateDocument(ctx, ws, cp.ID, copyDoc.ID, canvasapp.DocumentActor{UserID: 1},
		collab.RoleOwner, 1, "副本改过"); err != nil {
		t.Fatal(err)
	}
	orig, _ := svc.GetDocument(ctx, ws, pid, d.ID, collab.RoleViewer)
	if orig.Content != "原文" {
		t.Errorf("改副本不该影响原件，原件现为 %q", orig.Content)
	}
}

// 来源是「最后一次由谁写入」（MCPT-11）：保存时随写入方改写。只在新建时记的话，用户改过的 Agent 文档仍显示
// 「Agent 写入」，MCP 改过的用户文档也说不出是机器写的；MCP 的写入也带着用户本人的 ID，冲突时不能说成
// 「你在另一个窗口改的」（否则「以我为准」会覆盖客户端刚写的内容）。
func TestDocument_来源随写入方改写(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	svc := canvasapp.New(r, next)
	pid := mustInsert(t, r, next, ws, "项目")
	d, err := svc.CreateDocument(ctx, ws, pid, canvasapp.DocumentActor{UserID: 1, AgentSessionID: 7}, collab.RoleOwner,
		canvasapp.CreateDocumentInput{Path: "/plan.md", Content: "a"})
	if err != nil || d.Source != "agent" {
		t.Fatalf("Agent 新建应记 agent：%+v %v", d, err)
	}
	d, err = svc.UpdateDocument(ctx, ws, pid, d.ID, canvasapp.DocumentActor{UserID: 1}, collab.RoleOwner, 1, "b")
	if err != nil || d.Source != "user" {
		t.Fatalf("用户保存后应记 user：%+v %v", d, err)
	}
	d, err = svc.UpdateDocument(ctx, ws, pid, d.ID, canvasapp.DocumentActor{UserID: 1, McpGrantID: 9},
		collab.RoleOwner, 2, "c")
	if err != nil || d.Source != "mcp" || d.EditorID != 1 {
		t.Fatalf("MCP 写入后应记 mcp、编辑者仍是本人：%+v %v", d, err)
	}
	_, err = svc.UpdateDocument(ctx, ws, pid, d.ID, canvasapp.DocumentActor{UserID: 1}, collab.RoleOwner, 2, "旧稿")
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.VersionConflict || be.Details["bySelf"] != false ||
		be.Details["currentSource"] != "mcp" {
		t.Fatalf("MCP 写入后的冲突不是「本人另一个窗口」：%v", err)
	}
}
