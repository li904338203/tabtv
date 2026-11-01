package repo

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// 商品库数据访问（MKT-T-33；dev/05 §2.2、MKD-31、MKD-34、MKD-37）。
//
// ⚠️ 用户侧每条 SQL 都带 `workspace_id = $n and deleted_at is null` —— 那是越权防线的落点
// （CD-04 第 2 条）。他人工作区与已删除回**同一个 ErrNotFound**。
// 唯一的例外是 ProductMediaReferences：媒体引用检查器按定义**跨工作区**查（media/app 的要求）。
// ⚠️ image_media_ids 存**字符串**数组（与 scene_input.products 同形），GIN 索引按
// `@> '["<id>"]'` 命中；存成数字的话检查器的 `@>` 永远对不上，删除保护整条失效而不报错。

// ErrQuota 是工作区商品数已达上限（MKD-34）。
var ErrQuota = errors.New("配额已满")

// Product 是一行商品。
type Product struct {
	ID            int64
	WorkspaceID   int64
	Name          string
	SellingPoints string
	Description   string
	ImageMediaIDs []int64
	Version       int
	CreatedBy     int64
	UpdatedBy     int64
	// UpdatedByName 最后编辑者昵称（冲突提示「是谁改的」用，CD-58）；为空 = 未知。
	UpdatedByName string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// ProductSort 是列表排序（MKD-37）。
type ProductSort string

const (
	ProductSortUpdated ProductSort = "updated" // updated_at desc, id desc（默认）
	ProductSortCreated ProductSort = "created" // created_at desc, id desc
	ProductSortName    ProductSort = "name"    // lower(name) asc, id asc
)

// Valid 判断排序取值。
func (s ProductSort) Valid() bool {
	return s == ProductSortUpdated || s == ProductSortCreated || s == ProductSortName
}

// ProductFilter 是列表条件。
type ProductFilter struct {
	WorkspaceID int64
	Q           string
	Sort        ProductSort
	// Cursor 是上一页最后一行的排序键（不透明串，EncodeProductCursor 生成）；空 = 第一页。
	Cursor string
	Limit  int
}

// ErrBadCursor 是游标解不开或与排序不匹配。
var ErrBadCursor = errors.New("cursor 非法")

type productCursor struct {
	S string `json:"s"` // 排序
	K string `json:"k"` // 排序键（时间 RFC3339Nano / 小写名称）
	I int64  `json:"i"` // id
}

// EncodeProductCursor 生成下一页游标。
//
// ⚠️ 游标里带上排序：拿 `updated` 的游标去翻 `name` 排序的页，键的类型都对不上 —— 报错
// 比翻出一页错乱的数据好。
func EncodeProductCursor(sort ProductSort, p Product) string {
	c := productCursor{S: string(sort), I: p.ID}
	switch sort {
	case ProductSortCreated:
		c.K = p.CreatedAt.UTC().Format(time.RFC3339Nano)
	case ProductSortName:
		c.K = strings.ToLower(p.Name)
	default:
		c.K = p.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	b, _ := json.Marshal(c)
	return encodeCursor(b)
}

func decodeProductCursor(sort ProductSort, s string) (productCursor, error) {
	b, err := decodeCursor(s)
	if err != nil {
		return productCursor{}, ErrBadCursor
	}
	var c productCursor
	if err := json.Unmarshal(b, &c); err != nil || c.S != string(sort) || c.I <= 0 {
		return productCursor{}, ErrBadCursor
	}
	if sort != ProductSortName {
		if _, err := time.Parse(time.RFC3339Nano, c.K); err != nil {
			return productCursor{}, ErrBadCursor
		}
	}
	return c, nil
}

func encodeCursor(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func decodeCursor(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

func idsToJSON(ids []int64) ([]byte, error) {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = strconv.FormatInt(id, 10)
	}
	return json.Marshal(s)
}

func idsFromJSON(raw []byte) ([]int64, error) {
	var s []string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(s))
	for _, x := range s {
		id, err := strconv.ParseInt(x, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("image_media_ids 含非法 ID %q", x)
		}
		out = append(out, id)
	}
	return out, nil
}

const productCols = `p.id, p.workspace_id, p.name, p.selling_points, p.description, p.image_media_ids,
	p.version, coalesce(p.created_by_user_id, 0), coalesce(p.updated_by_user_id, 0), coalesce(u.nickname, ''),
	p.created_at, p.updated_at`

// ⚠️ `left join` 不是 `join`：最后编辑者账号已清理时，内连接会让整行消失 —— 界面说"商品不存在"。
const productFrom = ` from marketing_product p left join app_user u on u.id = p.updated_by_user_id `

func scanProduct(row pgx.Row) (Product, error) {
	var p Product
	var raw []byte
	if err := row.Scan(&p.ID, &p.WorkspaceID, &p.Name, &p.SellingPoints, &p.Description, &raw,
		&p.Version, &p.CreatedBy, &p.UpdatedBy, &p.UpdatedByName, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return Product{}, mapNoRows(err)
	}
	ids, err := idsFromJSON(raw)
	if err != nil {
		return Product{}, err
	}
	p.ImageMediaIDs = ids
	return p, nil
}

// InsertProduct 新建商品；工作区未删除商品数已达 max 时返回 ErrQuota。
//
// ⚠️ 计数与插入在**同一事务**里、并先拿工作区级 advisory 锁（MKD-34、MKR-19）：在事务外先查后插，
// 两个人同时新增第 500 个会一起越过上限。锁只在本工作区的商品新增之间互斥，不碰 workspace 行
// （那一行别的域也在 `for update`）。
func (r *Repo) InsertProduct(ctx context.Context, p Product, max int) (Product, error) {
	imgs, err := idsToJSON(p.ImageMediaIDs)
	if err != nil {
		return Product{}, err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Product{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended('marketing_product:' || $1::bigint::text, 0))`,
		p.WorkspaceID); err != nil {
		return Product{}, err
	}
	var n int
	if err := tx.QueryRow(ctx, `select count(*) from marketing_product
		where workspace_id = $1 and deleted_at is null`, p.WorkspaceID).Scan(&n); err != nil {
		return Product{}, err
	}
	if n >= max {
		return Product{}, ErrQuota
	}
	if _, err := tx.Exec(ctx, `
		insert into marketing_product (id, workspace_id, name, selling_points, description, image_media_ids,
		                               created_by_user_id, updated_by_user_id)
		values ($1, $2, $3, $4, $5, $6::jsonb, nullif($7::bigint, 0), nullif($7::bigint, 0))`,
		p.ID, p.WorkspaceID, p.Name, p.SellingPoints, p.Description, string(imgs), p.CreatedBy); err != nil {
		return Product{}, err
	}
	out, err := scanProduct(tx.QueryRow(ctx, `select `+productCols+productFrom+`
		where p.id = $1 and p.workspace_id = $2 and p.deleted_at is null`, p.ID, p.WorkspaceID))
	if err != nil {
		return Product{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Product{}, err
	}
	return out, nil
}

// GetProduct 取本工作区未删除的商品。
func (r *Repo) GetProduct(ctx context.Context, workspaceID, id int64) (Product, error) {
	return scanProduct(r.pool.QueryRow(ctx, `select `+productCols+productFrom+`
		where p.id = $1 and p.workspace_id = $2 and p.deleted_at is null`, id, workspaceID))
}

// UpdateProduct 整份更新（乐观锁）。版本不匹配 → ErrStale；不存在 / 他人工作区 / 已删除 → ErrNotFound。
func (r *Repo) UpdateProduct(ctx context.Context, p Product, version int) (Product, error) {
	imgs, err := idsToJSON(p.ImageMediaIDs)
	if err != nil {
		return Product{}, err
	}
	tag, err := r.pool.Exec(ctx, `
		update marketing_product
		   set name = $4, selling_points = $5, description = $6, image_media_ids = $7::jsonb,
		       version = version + 1, updated_by_user_id = nullif($8::bigint, 0), updated_at = now()
		 where id = $1 and workspace_id = $2 and deleted_at is null and version = $3`,
		p.ID, p.WorkspaceID, version, p.Name, p.SellingPoints, p.Description, string(imgs), p.UpdatedBy)
	if err != nil {
		return Product{}, err
	}
	if tag.RowsAffected() == 0 {
		// 区分「被别人改了」与「根本不在」：前者要给重新加载，后者是 404。
		if _, gerr := r.GetProduct(ctx, p.WorkspaceID, p.ID); gerr != nil {
			return Product{}, gerr
		}
		return Product{}, ErrStale
	}
	return r.GetProduct(ctx, p.WorkspaceID, p.ID)
}

// SoftDeleteProduct 软删，返回删除前的行（调用方据此尽力删媒体，MKD-31）。重复删除 → ErrNotFound。
func (r *Repo) SoftDeleteProduct(ctx context.Context, workspaceID, id, userID int64) (Product, error) {
	var raw []byte
	p := Product{ID: id, WorkspaceID: workspaceID}
	err := r.pool.QueryRow(ctx, `
		update marketing_product
		   set deleted_at = now(), updated_by_user_id = nullif($3::bigint, 0), updated_at = now()
		 where id = $1 and workspace_id = $2 and deleted_at is null
		returning name, image_media_ids`, id, workspaceID, userID).Scan(&p.Name, &raw)
	if err != nil {
		return Product{}, mapNoRows(err)
	}
	ids, err := idsFromJSON(raw)
	if err != nil {
		return Product{}, err
	}
	p.ImageMediaIDs = ids
	return p, nil
}

// ListProducts 列出本工作区未删除商品（服务端搜索 + 游标分页，MKD-37）。返回 (行, 是否还有下一页)。
func (r *Repo) ListProducts(ctx context.Context, f ProductFilter) ([]Product, bool, error) {
	if !f.Sort.Valid() {
		f.Sort = ProductSortUpdated
	}
	if f.Limit <= 0 {
		f.Limit = 20
	}
	args := []any{f.WorkspaceID}
	add := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	cond := []string{"p.workspace_id = $1", "p.deleted_at is null"}
	if q := strings.TrimSpace(f.Q); q != "" {
		// ⚠️ 关键词来自输入框：`%` `_` 不转义的话，搜「100%」会拖出全部商品。
		cond = append(cond, "p.name ilike '%' || "+add(escapeLike(q))+" || '%' escape '\\'")
	}
	var order string
	switch f.Sort {
	case ProductSortCreated:
		order = "p.created_at desc, p.id desc"
	case ProductSortName:
		order = "lower(p.name) asc, p.id asc"
	default:
		order = "p.updated_at desc, p.id desc"
	}
	if f.Cursor != "" {
		c, err := decodeProductCursor(f.Sort, f.Cursor)
		if err != nil {
			return nil, false, err
		}
		switch f.Sort {
		case ProductSortCreated:
			cond = append(cond, "(p.created_at, p.id) < ("+add(c.K)+"::timestamptz, "+add(c.I)+")")
		case ProductSortName:
			cond = append(cond, "(lower(p.name), p.id) > ("+add(c.K)+", "+add(c.I)+")")
		default:
			cond = append(cond, "(p.updated_at, p.id) < ("+add(c.K)+"::timestamptz, "+add(c.I)+")")
		}
	}
	sql := `select ` + productCols + productFrom + ` where ` + strings.Join(cond, " and ") +
		` order by ` + order + ` limit ` + add(f.Limit+1)
	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []Product{}
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > f.Limit
	if more {
		out = out[:f.Limit]
	}
	return out, more, nil
}

// ProductMediaReferences 返回引用该媒体的**未删除**商品 ID（media/app.ReferenceChecker，MKT-D-53）。
//
// ⚠️ **跨工作区查**（同 agent 检查器的注释）：按工作区过滤会漏掉引用，而漏掉的后果是把还在用的文件删了。
// ⚠️ 只认未删除的：软删商品之后它的图就不再算被引用（MKD-31），否则商品删了图永远收不走。
// 条件与部分 GIN 索引 idx_marketing_product_media 一致（deleted_at is null），走索引。
func (r *Repo) ProductMediaReferences(ctx context.Context, mediaID int64) ([]int64, error) {
	ref, err := idsToJSON([]int64{mediaID})
	if err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `select id from marketing_product
		where image_media_ids @> $1::jsonb and deleted_at is null order by id`, string(ref))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
