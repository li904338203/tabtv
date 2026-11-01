package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/li904338203/tabtv/backend/internal/marketing/domain"
	"github.com/li904338203/tabtv/backend/internal/marketing/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 商品库（MKT-T-33；02 MK-22、MKT-F-92~97；dev/05 MKD-31、34、35、37）。
//
// ⚠️ 工作区隔离靠仓储里每条 SQL 的 workspace_id 条件；这里不再「先取出来比一下」——
// 那会让越权防线有两处，而两处迟早不一致。
// ⚠️ 本服务**不负责权限**：读要 view、写要 edit，由路由层的工作区闸给（MKD-32、CD-59）。

// ProductStore 是商品库用到的读写（marketing/repo 实现）。
type ProductStore interface {
	InsertProduct(ctx context.Context, p repo.Product, max int) (repo.Product, error)
	GetProduct(ctx context.Context, workspaceID, id int64) (repo.Product, error)
	UpdateProduct(ctx context.Context, p repo.Product, version int) (repo.Product, error)
	SoftDeleteProduct(ctx context.Context, workspaceID, id, userID int64) (repo.Product, error)
	ListProducts(ctx context.Context, f repo.ProductFilter) ([]repo.Product, bool, error)
}

// LibrarySettings 是库的配置值（环境变量，MKD-34）。
type LibrarySettings struct {
	MaxProductImages int
	MaxProducts      int
	// MaxCharacters 每个工作区未删除角色数上限（SEEAITV_MARKETING_MAX_CHARACTERS，MKT-T-34）。
	MaxCharacters int
}

// LibraryActor 是操作者：工作区来自工作区闸，用户来自登录态。
type LibraryActor struct {
	WorkspaceID int64
	UserID      int64
}

// Library 是商品库与角色库（MKT-T-34，见 character.go）的用户端服务。
type Library struct {
	products ProductStore
	// characters / presets / copier 由 SetCharacters 注入（MKT-T-34）；为 nil 时角色接口回 500。
	characters CharacterStore
	presets    PresetReader
	copier     MediaCopier
	media      WorkspaceMediaLookup
	deleter    MediaDeleter
	cfg        LibrarySettings
	nextID     func() int64
	log        *slog.Logger
}

// NewLibrary 构造。deleter 可为 nil（删除商品时不清理媒体，只记日志）。
func NewLibrary(products ProductStore, media WorkspaceMediaLookup, deleter MediaDeleter,
	cfg LibrarySettings, nextID func() int64) *Library {
	return &Library{products: products, media: media, deleter: deleter, cfg: cfg, nextID: nextID,
		log: slog.Default()}
}

// SetLogger 注入 logger。
func (l *Library) SetLogger(lg *slog.Logger) {
	if lg != nil {
		l.log = lg
	}
}

func (l *Library) productLimits() domain.ProductLimits {
	return domain.ProductLimits{MaxImages: l.cfg.MaxProductImages, MaxProducts: l.cfg.MaxProducts}.Normalize()
}

// ProductQuery 是列表条件（transport 解析后的）。
type ProductQuery struct {
	Q      string
	Sort   string
	Cursor string
	Limit  int
}

// ProductPage 是一页商品。
type ProductPage struct {
	Items      []repo.Product
	NextCursor string
}

const (
	defaultLibraryPageSize = 20
	maxLibraryPageSize     = 100
)

// ListProducts 列出本工作区商品（MKT-F-92：服务端搜索 + 游标分页，不做客户端筛选）。
func (l *Library) ListProducts(ctx context.Context, a LibraryActor, q ProductQuery) (ProductPage, error) {
	sort := repo.ProductSort(q.Sort)
	if q.Sort == "" {
		sort = repo.ProductSortUpdated
	}
	if !sort.Valid() {
		return ProductPage{}, httpx.Errf(errcode.InvalidArgument, "sort 只能是 updated / created / name")
	}
	limit := q.Limit
	switch {
	case limit <= 0:
		limit = defaultLibraryPageSize
	case limit > maxLibraryPageSize:
		limit = maxLibraryPageSize
	}
	items, more, err := l.products.ListProducts(ctx, repo.ProductFilter{WorkspaceID: a.WorkspaceID,
		Q: q.Q, Sort: sort, Cursor: q.Cursor, Limit: limit})
	if errors.Is(err, repo.ErrBadCursor) {
		return ProductPage{}, httpx.Errf(errcode.InvalidArgument, "cursor 非法（排序变了要从第一页开始）")
	}
	if err != nil {
		return ProductPage{}, httpx.Wrap(errcode.Internal, err)
	}
	page := ProductPage{Items: items}
	if more && len(items) > 0 {
		page.NextCursor = repo.EncodeProductCursor(sort, items[len(items)-1])
	}
	return page, nil
}

// GetProduct 取详情。他人工作区 / 已删除 / 不存在一律 404（MKT-AC-33）。
func (l *Library) GetProduct(ctx context.Context, a LibraryActor, id int64) (repo.Product, error) {
	p, err := l.products.GetProduct(ctx, a.WorkspaceID, id)
	if err != nil {
		return repo.Product{}, productErr(err)
	}
	return p, nil
}

// CreateProduct 新建商品。配额满 → 409 + details{reason:"quota", max}。
func (l *Library) CreateProduct(ctx context.Context, a LibraryActor, in domain.ProductInput) (repo.Product, error) {
	in = in.Normalized()
	lim := l.productLimits()
	if err := l.validateProduct(ctx, a.WorkspaceID, in, lim); err != nil {
		return repo.Product{}, err
	}
	p, err := l.products.InsertProduct(ctx, repo.Product{ID: l.nextID(), WorkspaceID: a.WorkspaceID,
		Name: in.Name, SellingPoints: in.SellingPoints, Description: in.Description,
		ImageMediaIDs: in.ImageMediaIDs, CreatedBy: a.UserID}, lim.MaxProducts)
	if errors.Is(err, repo.ErrQuota) {
		return repo.Product{}, quotaErr("商品", lim.MaxProducts)
	}
	if err != nil {
		return repo.Product{}, httpx.Wrap(errcode.Internal, err)
	}
	return p, nil
}

// UpdateProduct 整份更新（乐观锁，MKT-F-95）。版本不匹配 → 40902 + 当前版本与最后编辑者。
func (l *Library) UpdateProduct(ctx context.Context, a LibraryActor, id int64, version int,
	in domain.ProductInput) (repo.Product, error) {
	if version < 1 {
		return repo.Product{}, issuesErr(errcode.InvalidArgument, "商品内容有问题",
			[]domain.Issue{errIssue("version", domain.IssueRequired, "缺少 version（乐观锁）")}, nil)
	}
	in = in.Normalized()
	// 先确认存在：他人工作区的商品要回 404，而不是先把「图片不属于本工作区」报给他。
	if _, err := l.products.GetProduct(ctx, a.WorkspaceID, id); err != nil {
		return repo.Product{}, productErr(err)
	}
	if err := l.validateProduct(ctx, a.WorkspaceID, in, l.productLimits()); err != nil {
		return repo.Product{}, err
	}
	p, err := l.products.UpdateProduct(ctx, repo.Product{ID: id, WorkspaceID: a.WorkspaceID, Name: in.Name,
		SellingPoints: in.SellingPoints, Description: in.Description, ImageMediaIDs: in.ImageMediaIDs,
		UpdatedBy: a.UserID}, version)
	if errors.Is(err, repo.ErrStale) {
		return repo.Product{}, l.productConflict(ctx, a, id)
	}
	if err != nil {
		return repo.Product{}, productErr(err)
	}
	return p, nil
}

// DeleteProduct 软删，然后**尽力**删除它自有的上传媒体（MKD-31）。
//
// ⚠️ 顺序：先软删、再删媒体 —— 商品检查器只认未删除的商品，反过来的话每张图都会被
// 「正被营销商品引用」拦下（拦它的正是这个商品自己）。
// ⚠️ 媒体删除失败**不影响**商品删除的结果：被会话等引用而拒绝是正常的（会话靠快照引用它），
// 只记 Info；不是 upload_reference 的图（生成产物、切片等）不碰 —— 那不是商品「自有」的上传，
// 在素材库 / 任务里还看得见。
func (l *Library) DeleteProduct(ctx context.Context, a LibraryActor, id int64) error {
	p, err := l.products.SoftDeleteProduct(ctx, a.WorkspaceID, id, a.UserID)
	if err != nil {
		return productErr(err)
	}
	l.releaseMedia(ctx, a.WorkspaceID, "marketing_product", p.ID, p.ImageMediaIDs)
	return nil
}

// releaseMedia 逐张尝试删除对象自有的上传媒体；结果只进日志。
func (l *Library) releaseMedia(ctx context.Context, ws int64, objType string, objID int64, ids []int64) {
	if l.deleter == nil || len(ids) == 0 {
		return
	}
	for _, mid := range ids {
		if l.media != nil {
			f, found, err := l.media.LookupWorkspaceMedia(ctx, ws, mid)
			if err != nil || !found || f.WorkspaceID != ws || f.Purpose != mediaPurposeUpload {
				l.log.Info("营销库删除：跳过非自有上传的媒体", "object", objType, "id", objID, "mediaId", mid,
					"found", found, "purpose", f.Purpose, "error", err)
				continue
			}
		}
		if err := l.deleter.DeleteMedia(ctx, ws, mid); err != nil {
			l.log.Info("营销库删除：媒体仍被引用或暂时删不掉，保留", "object", objType, "id", objID,
				"mediaId", mid, "error", err)
		}
	}
}

// mediaPurposeUpload 是用户上传参考图的 purpose（media_object.purpose，POST /media/references）。
const mediaPurposeUpload = "upload_reference"

// validateProduct 字段校验 + 图片归属 / 格式校验，一次报完（MKT-AC-32）。
func (l *Library) validateProduct(ctx context.Context, ws int64, in domain.ProductInput,
	lim domain.ProductLimits) error {
	issues := domain.ValidateProduct(in, lim)
	bad := map[string]bool{}
	for _, i := range issues {
		bad[i.Field] = true
	}
	mediaIssues, err := l.imageIssues(ctx, ws, in.ImageMediaIDs, lim.MaxImages, bad, "商品图")
	if err != nil {
		return err
	}
	issues = append(issues, mediaIssues...)
	if domain.HasError(issues) {
		return issuesErr(errcode.InvalidArgument, "商品内容有问题", issues, nil)
	}
	return nil
}

// imageIssues 逐张查媒体：属于本工作区 ∧ 是图片 ∧ 格式在白名单（MKD-35）。
//
// ⚠️ 按媒体库里记录的 content_type（上传时按文件头嗅探）判，不信客户端。
// ⚠️ 只查前 limit+1 张：张数已经超了的请求不值得逐张打库（超限本身已经报了）。
func (l *Library) imageIssues(ctx context.Context, ws int64, ids []int64, limit int,
	skip map[string]bool, what string) ([]domain.Issue, error) {
	if l.media == nil {
		return nil, httpx.Errf(errcode.Internal, "服务端未接入媒体域，无法校验"+what)
	}
	var out []domain.Issue
	for i, id := range ids {
		if i > limit {
			break
		}
		field := fmt.Sprintf("imageMediaIds[%d]", i)
		if skip[field] {
			continue
		}
		f, found, err := l.media.LookupWorkspaceMedia(ctx, ws, id)
		if err != nil {
			return nil, httpx.Wrap(errcode.Internal, err)
		}
		switch {
		case !found || f.WorkspaceID != ws:
			// 不存在、已删除、他人工作区、平台素材：同一句话（不提供按 ID 探测的口子）。
			out = append(out, errIssue(field, domain.IssueBadValue,
				fmt.Sprintf("第 %d 张图片不存在或不属于当前工作区", i+1)))
		case !domain.AllowedImageMime(f.ContentType):
			out = append(out, errIssue(field, domain.IssueBadFormat,
				fmt.Sprintf("第 %d 张不是 PNG / JPG / WEBP 图片（GIF、视频、音频不能作为%s）", i+1, what)))
		}
	}
	return out, nil
}

// productConflict 给出「被谁改了」（CD-58 冲突可解释）。
func (l *Library) productConflict(ctx context.Context, a LibraryActor, id int64) error {
	be := httpx.Errf(errcode.VersionConflict, "商品已被修改，请重新加载后再保存（你的修改不会被静默覆盖）")
	cur, err := l.products.GetProduct(ctx, a.WorkspaceID, id)
	if err != nil {
		return be
	}
	bySelf := cur.UpdatedBy != 0 && cur.UpdatedBy == a.UserID
	if bySelf {
		be = httpx.Errf(errcode.VersionConflict, "你在另一个窗口修改过这个商品，请重新加载后再保存")
	} else if cur.UpdatedByName != "" {
		be = httpx.Errf(errcode.VersionConflict,
			fmt.Sprintf("「%s」刚修改过这个商品，请重新加载后再保存（你的修改不会被静默覆盖）", cur.UpdatedByName))
	}
	return be.WithDetails(map[string]any{
		"currentVersion":    cur.Version,
		"currentUpdatedAt":  cur.UpdatedAt.UTC().Format(time.RFC3339),
		"currentEditorId":   strconv.FormatInt(cur.UpdatedBy, 10),
		"currentEditorName": cur.UpdatedByName,
		"bySelf":            bySelf,
	})
}

// quotaErr 是配额满（MKD-34）。用 409 CONFLICT + details.reason="quota"（不新增错误码，dev/05 §3.4 / §8 #5）。
func quotaErr(what string, max int) error {
	return httpx.Errf(errcode.Conflict, fmt.Sprintf("当前工作区的%s已达上限 %d 个，请先删除不用的%s", what, max, what)).
		WithDetails(map[string]any{"reason": "quota", "max": max})
}

func productErr(err error) error {
	if errors.Is(err, repo.ErrNotFound) {
		return httpx.Errf(errcode.NotFound, "商品不存在")
	}
	return httpx.Wrap(errcode.Internal, err)
}
