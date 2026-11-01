//go:build integration

// 3D 导演台约束段在受理链路里的真库测试（docs/agent/director，DRT-07，DRD-18）。
//
// 目录域是真的（约束段模板在 prompt_template；Fresh 清了表 → 先走代码兜底），输入解析换成回导演台截图的假实现。
package integration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	catalogrepo "github.com/li904338203/tabtv/backend/internal/catalog/repo"
	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	"github.com/li904338203/tabtv/backend/internal/generation/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
)

// directorInputs 模拟画布域：输入里有一张导演台截图（purpose = director_capture）。
type directorInputs struct{}

func (directorInputs) ResolveForAccept(context.Context, int64, int64, string, string,
	json.RawMessage) (genapp.ResolvedInputs, error) {
	return genapp.ResolvedInputs{
		Inputs:     json.RawMessage(`[{"kind":"reference","order":1,"mediaId":91,"mediaKind":"image","referenceId":91}]`),
		RoleCounts: map[string]int{"image": 1}, DirectorKinds: []string{"capture"},
	}, nil
}

func acceptDirector(t *testing.T, svc *genapp.Service, key string) genapp.AcceptResult {
	t.Helper()
	res, err := svc.Accept(context.Background(), genapp.AcceptInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user, ProjectID: proj,
		NodeID: "n_image_1", OperationCode: "img2img", ModelCode: "dev_image",
		IdempotencyKey: key, Count: 1, Params: json.RawMessage(`{"prompt":"两个人在街头对峙"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// DR-AC-23：改了约束段模板之后，新受理的任务用新措辞，已受理任务的快照不变（快照冻结，06 §3.1）。
func TestAccept_改约束模板不影响已受理任务(t *testing.T) {
	svc, _, ledger, pool := setup(t, 10)
	fund(t, ledger, 1000)
	gen, _ := id.New(2)
	svc.SetInputResolver(directorInputs{})
	svc.SetDirectorGuard(catalogapp.New(catalogrepo.New(pool), gen.Next))

	first := acceptDirector(t, svc, "dir-1")
	if s := suffixOf(t, first); !strings.Contains(s, "参考图是 3D 白模构图预演") {
		t.Fatalf("输入里有导演台截图时快照应带约束段（此时走代码兜底），得到 %q", s)
	}

	if _, err := pool.Exec(context.Background(), `insert into prompt_template (id, code, content, status, version)
		values ($1, 'director.reference_guard.capture', '新措辞：只参考站位与机位。', 'published', 2)`, gen.Next()); err != nil {
		t.Fatal(err)
	}
	second := acceptDirector(t, svc, "dir-2")
	if s := suffixOf(t, second); !strings.Contains(s, "新措辞：只参考站位与机位。") || strings.Contains(s, "参考图是 3D 白模构图预演") {
		t.Fatalf("新受理的任务应用新措辞，得到 %q", s)
	}

	again, err := repo.New(pool).Get(context.Background(), ws, first.Generation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s := suffixOf(t, genapp.AcceptResult{Generation: again}); !strings.Contains(s, "参考图是 3D 白模构图预演") {
		t.Fatalf("已受理任务的快照不该跟着模板变，得到 %q", s)
	}
}
