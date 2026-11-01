//go:build integration

// 我的资产（迁移 00074）的真库测试：保存幂等、只收本工作区成功的产物、文本另存显示的内容、文件夹删除退回顶层、
// 标签筛选是交集、评级整个工作区共用且可清除 —— 全是 SQL 决定的，只能在真库上验。
package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/content/repo"
)

func TestLibrary_保存幂等_只收本工作区成功的产物(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	const ws, other = int64(901), int64(902)
	addImg, mid := mediaOutput(t, pool, next, ws, 0)
	gen := seedCreation(t, pool, next, creationSeed{ws: ws, project: 11, status: "success", typ: "image"}, addImg)
	failed := seedCreation(t, pool, next, creationSeed{ws: ws, project: 11, status: "failed", typ: "image"})

	in := repo.SaveLibraryItemInput{WorkspaceID: ws, UserID: 1, GenerationID: gen, OutputIdx: 0, Title: "海边", Tags: []string{"场景"}}
	id1, created, err := r.SaveLibraryItem(ctx, in)
	if err != nil || !created {
		t.Fatalf("第一次保存：%v created=%v", err, created)
	}
	id2, created, err := r.SaveLibraryItem(ctx, in)
	if err != nil || created || id2 != id1 {
		t.Fatalf("同一项产物再存应原样返回：id=%d/%d created=%v err=%v", id2, id1, created, err)
	}
	it, err := r.GetLibraryItem(ctx, ws, id1)
	if err != nil || it.MediaID != mid || it.Kind != "image" || it.Title != "海边" || len(it.Tags) != 1 {
		t.Fatalf("取回：%+v %v", it, err)
	}
	// 别的工作区、没成功的、没有这一项的 —— 都存不进来
	for name, bad := range map[string]repo.SaveLibraryItemInput{
		"别的工作区": {WorkspaceID: other, GenerationID: gen, Title: "x"},
		"失败的":   {WorkspaceID: ws, GenerationID: failed, Title: "x"},
		"没有这一项": {WorkspaceID: ws, GenerationID: gen, OutputIdx: 3, Title: "x"},
	} {
		if _, _, err := r.SaveLibraryItem(ctx, bad); !errors.Is(err, repo.ErrNotSavable) {
			t.Errorf("%s：期望 ErrNotSavable，得到 %v", name, err)
		}
	}
	// 移出之后可以再存（部分唯一索引只管未移出的）
	if n, err := r.RemoveLibraryItems(ctx, ws, []int64{id1}); err != nil || n != 1 {
		t.Fatalf("移出：%d %v", n, err)
	}
	if _, created, err := r.SaveLibraryItem(ctx, in); err != nil || !created {
		t.Fatalf("移出后再存应新建：%v %v", created, err)
	}
}

func TestLibrary_文本存显示的内容(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	const ws = int64(911)
	gen := seedCreation(t, pool, next, creationSeed{ws: ws, project: 11, status: "success", typ: "text"}, textOutput(t, pool, next, "原文"))
	id, _, err := r.SaveLibraryItem(ctx, repo.SaveLibraryItemInput{WorkspaceID: ws, GenerationID: gen, Title: "剧本", Text: "# 改过的"})
	if err != nil {
		t.Fatal(err)
	}
	it, _ := r.GetLibraryItem(ctx, ws, id)
	if it.Text != "# 改过的" || it.MediaID != 0 {
		t.Fatalf("文本应存卡片上显示的内容：%+v", it)
	}
	// 没给文字时取原文
	gen2 := seedCreation(t, pool, next, creationSeed{ws: ws, project: 11, status: "success", typ: "text"}, textOutput(t, pool, next, "原文2"))
	id2, _, _ := r.SaveLibraryItem(ctx, repo.SaveLibraryItemInput{WorkspaceID: ws, GenerationID: gen2, Title: "b"})
	if it2, _ := r.GetLibraryItem(ctx, ws, id2); it2.Text != "原文2" {
		t.Fatalf("没给文字应取原文：%q", it2.Text)
	}
}

func TestLibrary_文件夹_标签筛选_删除文件夹退回顶层(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	const ws = int64(921)
	save := func(title string, tags ...string) int64 {
		add, _ := mediaOutput(t, pool, next, ws, 0)
		g := seedCreation(t, pool, next, creationSeed{ws: ws, project: 11, status: "success", typ: "image"}, add)
		id, _, err := r.SaveLibraryItem(ctx, repo.SaveLibraryItemInput{WorkspaceID: ws, GenerationID: g, Title: title, Tags: tags})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	a := save("A", "角色", "风格")
	b := save("B", "角色")
	save("C")
	f, err := r.CreateLibraryFolder(ctx, ws, 1, "历史资产")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateLibraryFolder(ctx, ws, 1, "历史资产"); !errors.Is(err, repo.ErrConflict) {
		t.Fatalf("同名文件夹应冲突：%v", err)
	}
	if n, err := r.MoveLibraryItems(ctx, ws, []int64{a, b}, f.ID); err != nil || n != 2 {
		t.Fatalf("挪进文件夹：%d %v", n, err)
	}
	count := func(filter repo.LibraryFilter) int {
		filter.WorkspaceID, filter.Limit = ws, 50
		items, err := r.ListLibraryItems(ctx, filter)
		if err != nil {
			t.Fatal(err)
		}
		n, err := r.CountLibraryItems(ctx, filter)
		if err != nil || n != len(items) {
			t.Fatalf("计数与列表不一致：%d vs %d %v", n, len(items), err)
		}
		return n
	}
	root, inFolder := int64(0), f.ID
	if got := count(repo.LibraryFilter{FolderID: &root}); got != 1 {
		t.Errorf("顶层应剩 1 项，得到 %d", got)
	}
	if got := count(repo.LibraryFilter{FolderID: &inFolder}); got != 2 {
		t.Errorf("文件夹里应有 2 项，得到 %d", got)
	}
	if got := count(repo.LibraryFilter{Tags: []string{"角色", "风格"}}); got != 1 {
		t.Errorf("两个标签同时带着的只有 A，得到 %d", got)
	}
	if got := count(repo.LibraryFilter{Query: "b"}); got != 1 {
		t.Errorf("搜标题不分大小写，得到 %d", got)
	}
	if err := r.DeleteLibraryFolder(ctx, ws, f.ID); err != nil {
		t.Fatal(err)
	}
	if got := count(repo.LibraryFilter{FolderID: &root}); got != 3 {
		t.Errorf("删文件夹后资产应退回顶层（3 项），得到 %d", got)
	}
}

func TestLibrary_评级工作区共用_可清除_列表带出(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	const ws = int64(931)
	add, _ := mediaOutput(t, pool, next, ws, 0)
	g := seedCreation(t, pool, next, creationSeed{ws: ws, project: 11, status: "success", typ: "image"}, add)
	if err := r.SetGenerationRating(ctx, 999, g, 1, 4); !errors.Is(err, repo.ErrNotSavable) {
		t.Fatalf("别的工作区不能打分：%v", err)
	}
	if err := r.SetGenerationRating(ctx, ws, g, 1, 4); err != nil {
		t.Fatal(err)
	}
	if err := r.SetGenerationRating(ctx, ws, g, 2, 5); err != nil {
		t.Fatal(err)
	}
	id, _, _ := r.SaveLibraryItem(ctx, repo.SaveLibraryItemInput{WorkspaceID: ws, GenerationID: g, Title: "x"})
	marks, err := r.GenerationMarks(ctx, ws, []int64{g})
	if err != nil || marks[g].Rating != 5 || marks[g].SavedItems[0] != id {
		t.Fatalf("评级与保存情况：%+v %v", marks[g], err)
	}
	if it, _ := r.GetLibraryItem(ctx, ws, id); it.Rating != 5 {
		t.Fatalf("列表应带出评级：%d", it.Rating)
	}
	five := 5
	items, _ := r.ListLibraryItems(ctx, repo.LibraryFilter{WorkspaceID: ws, MinRating: five, Limit: 10})
	if len(items) != 1 {
		t.Fatalf("按评级筛选：%d", len(items))
	}
	if err := r.SetGenerationRating(ctx, ws, g, 1, 0); err != nil {
		t.Fatal(err)
	}
	if marks, _ := r.GenerationMarks(ctx, ws, []int64{g}); marks[g].Rating != 0 {
		t.Fatalf("清除后应为 0：%d", marks[g].Rating)
	}
}
