// Package repo 是营销域的数据访问（marketing_category / marketing_option / marketing_option_version）。
//
// ⚠️ 写一律带 version 乐观锁（where id = $1 and version = $2）：两个运营同时编辑同一条选项时，
// 后保存的那个收到冲突而不是静默覆盖前者。
// ⚠️ 保存 = 更新 option + version+1 + 插一行版本历史，**同一事务**：分开写的话中途失败会留下
// 「当前版本在历史里不存在」，而会话钉的正是那个版本号。
// ⚠️ 不提供删除：历史会话与生成快照按 code 引用（只停用）。
package repo

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrNotFound 是行不存在。
	ErrNotFound = errors.New("不存在")
	// ErrStale 是 version 不匹配（乐观锁冲突）。
	ErrStale = errors.New("版本已变化")
	// ErrDuplicate 是 code 重复（唯一索引冲突）。
	ErrDuplicate = errors.New("重复")
)

// Querier 是 pool 与 tx 的共同子集。
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Repo 是营销域仓储。
type Repo struct{ pool *pgxpool.Pool }

// New 构造。
func New(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

func mapDuplicate(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrDuplicate
	}
	return err
}

func mapNoRows(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func jsonOr(v json.RawMessage, def string) json.RawMessage {
	if len(v) == 0 || strings.TrimSpace(string(v)) == "null" {
		return json.RawMessage(def)
	}
	return v
}

// escapeLike 转义 like 的元字符：关键词来自后台输入框，`%` 不转义的话会拖出全表。
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "%", "\\%")
	return strings.ReplaceAll(s, "_", "\\_")
}
