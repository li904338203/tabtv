//go:build integration

// 3D 导演台下游约束段的渲染（docs/agent/director，DRT-07，DRD-18）。
//
// 模板（prompt_template 的已发布行）优先于代码兜底：运营调措辞只改库、不发版。
package integration

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
)

func TestDirectorGuardPrompt_模板优先于兜底(t *testing.T) {
	r, pool, next := setupCatalog(t)
	ctx := context.Background()
	svc := catalogapp.New(r, next)

	// 库里没有行：用代码兜底，两段都在、先图后视频。
	got, err := svc.DirectorGuardPrompt(ctx, []string{"preview", "capture"})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(got, "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "参考图是 3D 白模构图预演") || !strings.Contains(lines[1], "参考视频是 3D 白模预演") {
		t.Fatalf("兜底应先图后视频各一行，得到 %q", got)
	}

	// 运营发布了新措辞：新受理用新措辞。
	mustExec(t, pool, `insert into prompt_template (id, code, content, status, version)
		values ($1, 'director.reference_guard.capture', '新措辞：只看站位与机位。', 'published', 2)`, next())
	got, err = svc.DirectorGuardPrompt(ctx, []string{"capture"})
	if err != nil || got != "新措辞：只看站位与机位。" {
		t.Fatalf("已发布的模板优先于兜底，得到 %q %v", got, err)
	}
	// 没有导演台输入：什么都不加。
	if got, err := svc.DirectorGuardPrompt(ctx, nil); err != nil || got != "" {
		t.Fatalf("没有种类时应为空，得到 %q %v", got, err)
	}
}

func TestDirectorGuardPrompt_渲染失败回退兜底并告警(t *testing.T) {
	r, pool, next := setupCatalog(t)
	ctx := context.Background()
	var logs bytes.Buffer
	svc := catalogapp.New(r, next)
	svc.SetLogger(slog.New(slog.NewTextHandler(&logs, nil)))
	// 一行坏掉的模板（带了 Static 不该有的占位符，绕过了后台校验）：渲染失败时回落代码兜底，不让受理失败，也不静默少一段。
	mustExec(t, pool, `insert into prompt_template (id, code, content, status, version)
		values ($1, 'director.reference_guard.capture', '坏模板 {{oops}}', 'published', 3)`, next())
	got, err := svc.DirectorGuardPrompt(ctx, []string{"capture"})
	if err != nil {
		t.Fatalf("模板坏了应回落兜底而不是报错：%v", err)
	}
	if !strings.Contains(got, "参考图是 3D 白模构图预演") {
		t.Fatalf("应回落到代码兜底，得到 %q", got)
	}
	if !strings.Contains(logs.String(), "导演台约束段模板渲染失败") {
		t.Fatalf("回落时要留告警，日志：%s", logs.String())
	}
}
