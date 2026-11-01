package wiring

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	catalogrepo "github.com/li904338203/tabtv/backend/internal/catalog/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/bootstrap"
	"github.com/li904338203/tabtv/backend/internal/platform/config"
	"github.com/li904338203/tabtv/backend/internal/platform/db"
)

// MKR-11：营销片段来源必须在 BuildCore 里注入 —— API 与 Worker 共用这一处装配，
// Agent 的 generate 在 Worker 里调受理。只在 API 注入的话 Worker 里发出的生成静默缺少营销片段
// （效果差而不报错），这条装配测试是唯一的绊线。
func TestBuildCore_已注入MarketingSource(t *testing.T) {
	// pgxpool.New 是惰性的：不发起连接，装配期也不查库。
	pool, err := pgxpool.New(context.Background(), "postgres://u:p@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	n := int64(0)
	next := func() int64 { n++; return n }
	core, err := BuildCore(CoreInput{Cfg: config.Config{}, Deps: &bootstrap.Deps{DB: &db.Pool{Pool: pool}},
		NextID: next, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Catalog: catalogapp.New(catalogrepo.New(pool), next)})
	if err != nil {
		t.Fatal(err)
	}
	if !core.Gen.HasMarketing() {
		t.Fatal("BuildCore 产出的生成服务必须已注入 MarketingSource（API 与 Worker 共用）")
	}
	// TestCore_注入导演台约束段来源（DRT-07）：漏了的话带导演台截图的生成静默缺少约束段。
	if !core.Gen.HasDirectorGuard() {
		t.Fatal("BuildCore 产出的生成服务必须已注入 DirectorGuardSource（API 与 Worker 共用）")
	}
	if core.Director == nil {
		t.Fatal("BuildCore 应装出 AI 搭景服务（DRT-19，两个进程共用）")
	}
}
