package repo

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/li904338203/tabtv/backend/internal/marketing/domain"
)

// Category 是 marketing_category 的一行。
type Category struct {
	ID int64
	domain.Category
	CreatedAt time.Time
	UpdatedAt time.Time
}

const categoryCols = `id, kind, code, name, sort, is_enabled, created_at, updated_at`

func scanCategory(row pgx.Row) (Category, error) {
	var c Category
	var kind string
	if err := row.Scan(&c.ID, &kind, &c.Code, &c.Name, &c.Sort, &c.IsEnabled, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return Category{}, mapNoRows(err)
	}
	c.Kind = domain.Kind(kind)
	return c, nil
}

// ListCategories 列出分类（kind 为空 = 全部；含停用的），按 kind、sort、id 升序。
func (r *Repo) ListCategories(ctx context.Context, kind domain.Kind) ([]Category, error) {
	rows, err := r.pool.Query(ctx, `select `+categoryCols+` from marketing_category
		where ($1 = '' or kind = $1) order by kind, sort, id`, string(kind))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Category
	for rows.Next() {
		c, err := scanCategory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetCategory 按 ID 取。
func (r *Repo) GetCategory(ctx context.Context, id int64) (Category, error) {
	return scanCategory(r.pool.QueryRow(ctx, `select `+categoryCols+` from marketing_category where id = $1`, id))
}

// GetCategoryByCode 按 (kind, code) 取。
func (r *Repo) GetCategoryByCode(ctx context.Context, kind domain.Kind, code string) (Category, error) {
	return scanCategory(r.pool.QueryRow(ctx, `select `+categoryCols+` from marketing_category
		where kind = $1 and code = $2`, string(kind), code))
}

// InsertCategory 新建分类。(kind, code) 重复返回 ErrDuplicate。
func (r *Repo) InsertCategory(ctx context.Context, c Category) (Category, error) {
	out, err := scanCategory(r.pool.QueryRow(ctx, `
		insert into marketing_category (id, kind, code, name, sort, is_enabled)
		values ($1, $2, $3, $4, $5, $6) returning `+categoryCols,
		c.ID, string(c.Kind), c.Code, c.Name, c.Sort, c.IsEnabled))
	if err != nil {
		return Category{}, mapDuplicate(err)
	}
	return out, nil
}

// UpdateCategory 改名 / 启停。⚠️ 没有改 code、改 kind 的路径：选项按 (kind, code) 引用分类。
func (r *Repo) UpdateCategory(ctx context.Context, id int64, name string, enabled bool) (Category, error) {
	return scanCategory(r.pool.QueryRow(ctx, `
		update marketing_category set name = $2, is_enabled = $3, updated_at = now()
		where id = $1 returning `+categoryCols, id, name, enabled))
}

// SortCategories 按 ids 的顺序把同一 kind 下的分类排成 1..N；任一不属于该 kind 时整笔回滚（ErrNotFound）。
func (r *Repo) SortCategories(ctx context.Context, kind domain.Kind, ids []int64) error {
	return r.sortRows(ctx, `
		update marketing_category c set sort = t.ord, updated_at = now()
		from unnest($1::bigint[]) with ordinality as t(id, ord)
		where c.id = t.id and c.kind = $2`, ids, string(kind))
}
