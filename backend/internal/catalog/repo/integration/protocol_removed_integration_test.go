//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/catalog/domain"
	"github.com/li904338203/tabtv/backend/migrations"
)

// 代码里删掉的协议（bblabu_video、uniart_video，2026-11-03）可能还留在老用户的库里，甚至有模型在用。
// 启动时的协议同步不能因此失败（失败 = 应用起不来）：它只把这些行标成未注册，不删也不报错。
// 00084 迁移只清掉没有模型在用的那几行 —— model.api_protocol 是真外键，删在用的行会让迁移失败。
func TestProtocolSync_库里有已删除的协议照常启动(t *testing.T) {
	r, pool, next := setupCatalog(t)
	ctx := context.Background()

	for _, code := range []string{"bblabu_video", "uniart_video"} {
		mustExec(t, pool, `insert into api_protocol (code, name, is_async, requires_query_path, is_registered)
			values ($1, $1, true, true, true)`, code)
	}
	// 一个老模型还在用 uniart_video。
	seedModel(t, pool, next, modelSeed{code: "old-uni", providerStatus: "enabled", modelStatus: "enabled",
		domain: "video", op: "text2video", opEnabled: true})
	mustExec(t, pool, `update model set api_protocol = 'uniart_video' where code = 'old-uni'`)

	if err := r.UpsertProtocols(ctx, domain.Protocols()); err != nil {
		t.Fatalf("库里有已删除的协议时同步不该失败: %v", err)
	}
	for _, code := range []string{"bblabu_video", "uniart_video"} {
		var registered bool
		if err := pool.QueryRow(ctx, `select is_registered from api_protocol where code = $1`, code).Scan(&registered); err != nil {
			t.Fatalf("%s 这一行应保留（同步不删行）: %v", code, err)
		}
		if registered {
			t.Fatalf("%s 已从代码里删除，应标成未注册", code)
		}
	}
	for _, p := range domain.Protocols() {
		if p.Code == "bblabu_video" || p.Code == "uniart_video" {
			t.Fatalf("注册表里不该再有 %s", p.Code)
		}
	}

	// 00084：只删没有模型在用的。
	mustExec(t, pool, migrationUp(t, "00084_drop_bblabu_uniart.sql"))
	var left []string
	rows, err := pool.Query(ctx, `select code from api_protocol where code in ('bblabu_video','uniart_video') order by code`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var c string
		_ = rows.Scan(&c)
		left = append(left, c)
	}
	rows.Close()
	if len(left) != 1 || left[0] != "uniart_video" {
		t.Fatalf("没人用的 bblabu_video 应删掉、还有模型在用的 uniart_video 应保留，实际剩下 %v", left)
	}
}

// migrationUp 取迁移文件里 `-- +goose Up` 与 `-- +goose Down` 之间的 SQL —— 测的就是要发出去的那一份。
func migrationUp(t *testing.T, name string) string {
	t.Helper()
	b, err := migrations.FS.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	up := strings.Index(s, "-- +goose Up")
	down := strings.Index(s, "-- +goose Down")
	if up < 0 || down < up {
		t.Fatalf("%s 没有 Up / Down 段", name)
	}
	return s[up+len("-- +goose Up") : down]
}
