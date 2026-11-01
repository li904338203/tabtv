package app

import (
	"context"
	"errors"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/marketing/domain"
	"github.com/li904338203/tabtv/backend/internal/marketing/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 商品库服务的单测：图片归属 / 格式校验、配额与冲突的错误形状、删除时的媒体清理分流。
// 隔离、乐观锁真并发、配额并发在 repo/integration 与 transport/integration 里用真库测。

type memProducts struct {
	rows    map[int64]repo.Product
	quota   bool
	stale   bool
	deleted []int64
}

func (m *memProducts) InsertProduct(_ context.Context, p repo.Product, _ int) (repo.Product, error) {
	if m.quota {
		return repo.Product{}, repo.ErrQuota
	}
	p.Version = 1
	m.rows[p.ID] = p
	return p, nil
}
func (m *memProducts) GetProduct(_ context.Context, ws, id int64) (repo.Product, error) {
	p, ok := m.rows[id]
	if !ok || p.WorkspaceID != ws {
		return repo.Product{}, repo.ErrNotFound
	}
	return p, nil
}
func (m *memProducts) UpdateProduct(_ context.Context, p repo.Product, v int) (repo.Product, error) {
	if m.stale {
		return repo.Product{}, repo.ErrStale
	}
	p.Version = v + 1
	m.rows[p.ID] = p
	return p, nil
}
func (m *memProducts) SoftDeleteProduct(_ context.Context, ws, id, _ int64) (repo.Product, error) {
	p, ok := m.rows[id]
	if !ok || p.WorkspaceID != ws {
		return repo.Product{}, repo.ErrNotFound
	}
	delete(m.rows, id)
	return p, nil
}
func (m *memProducts) ListProducts(context.Context, repo.ProductFilter) ([]repo.Product, bool, error) {
	return nil, false, nil
}

type memMedia map[int64]MediaFacts

func (m memMedia) LookupWorkspaceMedia(_ context.Context, ws, id int64) (MediaFacts, bool, error) {
	f, ok := m[id]
	if !ok || (f.WorkspaceID != ws && f.WorkspaceID != 0) {
		return MediaFacts{}, false, nil
	}
	return f, true, nil
}

type recDeleter struct {
	got    []int64
	refuse map[int64]bool
}

func (d *recDeleter) DeleteMedia(_ context.Context, _ int64, id int64) error {
	d.got = append(d.got, id)
	if d.refuse[id] {
		return httpx.Errf(errcode.ResourceInUse, "该文件正被 营销会话 引用，无法删除")
	}
	return nil
}

const ws1 = 10

func newLib() (*Library, *memProducts, *recDeleter) {
	store := &memProducts{rows: map[int64]repo.Product{}}
	media := memMedia{
		1: {ID: 1, WorkspaceID: ws1, Purpose: "upload_reference", ContentType: "image/png"},
		2: {ID: 2, WorkspaceID: ws1, Purpose: "upload_reference", ContentType: "image/gif"},
		3: {ID: 3, WorkspaceID: ws1, Purpose: "upload_reference", ContentType: "video/mp4"},
		4: {ID: 4, WorkspaceID: 99, Purpose: "upload_reference", ContentType: "image/png"}, // 他人工作区
		5: {ID: 5, WorkspaceID: 0, Purpose: "material_cover", ContentType: "image/png"},    // 平台素材
		6: {ID: 6, WorkspaceID: ws1, Purpose: "generation_output", ContentType: "image/webp"},
		7: {ID: 7, WorkspaceID: ws1, Purpose: "upload_reference", ContentType: "image/jpeg"},
	}
	del := &recDeleter{refuse: map[int64]bool{7: true}}
	n := int64(100)
	lib := NewLibrary(store, media, del, LibrarySettings{MaxProductImages: 10, MaxProducts: 500},
		func() int64 { n++; return n })
	return lib, store, del
}

func bizOf(t *testing.T, err error) *httpx.BizError {
	t.Helper()
	var be *httpx.BizError
	if !errors.As(err, &be) {
		t.Fatalf("应为业务错误，得到 %v", err)
	}
	return be
}

func TestCreateProduct_图片归属与格式一次报完(t *testing.T) {
	lib, _, _ := newLib()
	a := LibraryActor{WorkspaceID: ws1, UserID: 1}
	_, err := lib.CreateProduct(context.Background(), a, domain.ProductInput{Name: "柚子汽水",
		ImageMediaIDs: []int64{1, 2, 3, 4, 5, 404}})
	be := bizOf(t, err)
	if be.Code != errcode.InvalidArgument {
		t.Fatalf("应 40001，得 %d", be.Code)
	}
	issues, _ := be.Details["issues"].([]domain.Issue)
	got := map[string]domain.IssueCode{}
	for _, i := range issues {
		got[i.Field] = i.Code
	}
	want := map[string]domain.IssueCode{
		"imageMediaIds[1]": domain.IssueBadFormat, // GIF
		"imageMediaIds[2]": domain.IssueBadFormat, // 视频
		"imageMediaIds[3]": domain.IssueBadValue,  // 他人工作区
		"imageMediaIds[4]": domain.IssueBadValue,  // 平台素材不算本工作区
		"imageMediaIds[5]": domain.IssueBadValue,  // 不存在
	}
	if len(got) != len(want) {
		t.Fatalf("问题不对：%v", got)
	}
	for f, c := range want {
		if got[f] != c {
			t.Errorf("%s 期望 %s，得到 %v", f, c, got)
		}
	}
	if p, err := lib.CreateProduct(context.Background(), a, domain.ProductInput{Name: " 汽水 ",
		ImageMediaIDs: []int64{7, 1}}); err != nil || p.Name != "汽水" || p.ImageMediaIDs[0] != 7 {
		t.Fatalf("合法商品应保存成功且名称去空白：%v %+v", err, p)
	}
}

func TestCreateProduct_配额满409带reason(t *testing.T) {
	lib, store, _ := newLib()
	store.quota = true
	_, err := lib.CreateProduct(context.Background(), LibraryActor{WorkspaceID: ws1},
		domain.ProductInput{Name: "x", ImageMediaIDs: []int64{1}})
	be := bizOf(t, err)
	if be.Code != errcode.Conflict || be.Details["reason"] != "quota" || be.Details["max"] != 500 {
		t.Fatalf("配额满应 409 + reason=quota + max：%d %v", be.Code, be.Details)
	}
}

func TestUpdateProduct_冲突说得出是谁(t *testing.T) {
	lib, store, _ := newLib()
	store.rows[50] = repo.Product{ID: 50, WorkspaceID: ws1, Name: "旧", ImageMediaIDs: []int64{1}, Version: 3,
		UpdatedBy: 2, UpdatedByName: "同事"}
	store.stale = true
	_, err := lib.UpdateProduct(context.Background(), LibraryActor{WorkspaceID: ws1, UserID: 1}, 50, 2,
		domain.ProductInput{Name: "新", ImageMediaIDs: []int64{1}})
	be := bizOf(t, err)
	if be.Code != errcode.VersionConflict || be.Details["currentVersion"] != 3 ||
		be.Details["currentEditorName"] != "同事" || be.Details["bySelf"] != false {
		t.Fatalf("冲突详情不对：%d %v", be.Code, be.Details)
	}
	// 他人工作区：先 404，不先报图片问题。
	_, err = lib.UpdateProduct(context.Background(), LibraryActor{WorkspaceID: 99}, 50, 3,
		domain.ProductInput{Name: "新", ImageMediaIDs: []int64{4}})
	if be := bizOf(t, err); be.Code != errcode.NotFound {
		t.Fatalf("他人工作区应 404，得 %d", be.Code)
	}
}

func TestDeleteProduct_只删自有上传且被引用的保留(t *testing.T) {
	lib, store, del := newLib()
	store.rows[60] = repo.Product{ID: 60, WorkspaceID: ws1, ImageMediaIDs: []int64{1, 6, 7}}
	if err := lib.DeleteProduct(context.Background(), LibraryActor{WorkspaceID: ws1}, 60); err != nil {
		t.Fatalf("被引用的图删不掉不应影响商品删除：%v", err)
	}
	// 6 是生成产物（不是商品自有的上传）不碰；7 被拒但仍尝试过。
	if len(del.got) != 2 || del.got[0] != 1 || del.got[1] != 7 {
		t.Fatalf("应只尝试删除自有上传 1、7：%v", del.got)
	}
	if err := lib.DeleteProduct(context.Background(), LibraryActor{WorkspaceID: ws1}, 60); bizOf(t, err).Code != errcode.NotFound {
		t.Fatal("重复删除应 404")
	}
}
