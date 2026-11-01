package tools

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/canvasgen"
	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 3D 导演台工具（DRT-29）的单测：假端口 + 与 transport 相同的四步（call）。场景真写入、来源 mcp 与事件
// 在集成测试里用真库验（tools/integration：TestMcpDirector_搭景写入来源为mcp）。

type fakeDirectors struct {
	created  []app.DirectorCreateInput
	built    []app.DirectorBuildInput
	buildErr error
}

func (f *fakeDirectors) Create(_ context.Context, _ app.Principal, in app.DirectorCreateInput) (string, int64, error) {
	f.created = append(f.created, in)
	return "a_1", 9007199254740993, nil
}

func (f *fakeDirectors) Build(_ context.Context, _ app.Principal, in app.DirectorBuildInput) (app.DirectorBuildResult, error) {
	f.built = append(f.built, in)
	if f.buildErr != nil {
		return app.DirectorBuildResult{}, f.buildErr
	}
	return app.DirectorBuildResult{SceneID: 9007199254740993, Version: 3, Added: []string{"obj_character_2"},
		Notice: "需要用户打开导演台截图后，下游才有输出"}, nil
}

// 只读端点下两个工具都看不到；默认端点（画布工具集默认开启）看得到（DR-F-163、DR-AC-47）。
func TestDirectorTools_只读端点不可见(t *testing.T) {
	names := listNames(t, connectAs(t, fakeDeps(), ownerAll(), "/mcp", bearerRT{}, nil))
	for _, n := range []string{"director_create", "director_build"} {
		if !slices.Contains(names, n) {
			t.Errorf("/mcp 应可见 %s：%v", n, names)
		}
	}
	names = listNames(t, connectAs(t, fakeDeps(), ownerAll(), "/mcp/readonly", allToolsetsRT{}, nil))
	for _, n := range []string{"director_create", "director_build"} {
		if slices.Contains(names, n) {
			t.Errorf("/mcp/readonly 不应暴露 %s：%v", n, names)
		}
	}
}

// 两个都是不花钱的写（edit，不是 generate）、不破坏；director_build 调用外部模型，如实标 openWorldHint。
func TestDirectorTools_非付费写工具(t *testing.T) {
	reg := NewRegistry(Deps{})
	for _, name := range []string{"director_create", "director_build"} {
		tl, ok := reg.Lookup(name)
		if !ok {
			t.Fatalf("没有工具 %s", name)
		}
		if len(tl.Methods) != 1 || tl.Methods[0].Name != "" || tl.Methods[0].Paid || !tl.Methods[0].Write ||
			tl.Methods[0].Action != "edit" || tl.Toolset != "canvas" {
			t.Errorf("%s 应是画布工具集的单方法、edit、Write、非 Paid：%+v %s", name, tl.Methods, tl.Toolset)
		}
		a := tl.Annotations()
		if a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint || a.IdempotentHint {
			t.Errorf("%s 注解应为非只读、非破坏、非幂等：%+v", name, a)
		}
		wantOpen := name == "director_build"
		if a.OpenWorldHint == nil || *a.OpenWorldHint != wantOpen {
			t.Errorf("%s openWorldHint 应为 %v：%+v", name, wantOpen, a)
		}
	}
}

// DRB-8：canvas_apply 的节点描述不再写死类型清单，并指向 director_create。
func TestCanvasApplyDescription_不写死节点类型(t *testing.T) {
	tl, _ := NewRegistry(Deps{}).Lookup("canvas_apply")
	raw, _ := json.Marshal(tl.InputSchema())
	s := string(raw)
	if strings.Contains(s, "text / image / video / audio") {
		t.Errorf("canvas_apply 的 schema 仍写死节点类型：%s", s)
	}
	if !strings.Contains(s, "director_create") || !strings.Contains(s, "canvas_read") {
		t.Errorf("node 描述应指向 canvas_read 与 director_create：%s", s)
	}
}

// 建导演台：同一个 idempotencyKey 得到同一个批次（重试只建一个节点）；结果带节点与场景 ID（字符串）。
func TestDirectorCreate_幂等批次与结果(t *testing.T) {
	d := fakeDeps()
	dirs := d.Directors.(*fakeDirectors)
	reg := NewRegistry(d)
	args := `{"projectId":"11","idempotencyKey":"same"}`
	m, _ := call(t, reg, "director_create", args, 6000)
	call(t, reg, "director_create", args, 6000)
	call(t, reg, "director_create", `{"projectId":"11"}`, 6000)
	if m["nodeId"] != "a_1" || m["sceneId"] != "9007199254740993" || !strings.Contains(m["message"].(string), "director_build") {
		t.Fatalf("结果：%v", m)
	}
	if len(dirs.created) != 3 || dirs.created[0].BatchID != dirs.created[1].BatchID ||
		dirs.created[2].BatchID == dirs.created[0].BatchID || dirs.created[0].ProjectID != 11 {
		t.Fatalf("同键同批、不带键每次新批：%+v", dirs.created)
	}
}

// 搭景：结果带固定提醒与变更清单（空的是 [] 不是 null）；限流的码与 retry_after_seconds 原样回给客户端。
func TestDirectorBuild_提醒与限流原样返回(t *testing.T) {
	d := fakeDeps()
	dirs := d.Directors.(*fakeDirectors)
	reg := NewRegistry(d)
	m, out := call(t, reg, "director_build", `{"projectId":"11","nodeId":"a_1","prompt":"三人围坐","referenceMediaIds":["77","78"]}`, 6000)
	if m["notice"] != "需要用户打开导演台截图后，下游才有输出" || !strings.Contains(out.Summary, "截图") {
		t.Fatalf("结果与摘要都要带截图提醒：%v %s", m, out.Summary)
	}
	if removed, ok := m["removed"].([]any); !ok || len(removed) != 0 {
		t.Fatalf("removed 应是空数组：%v", m["removed"])
	}
	if got := dirs.built[0]; got.NodeID != "a_1" || got.Prompt != "三人围坐" || len(got.ReferenceMediaIDs) != 2 ||
		got.BatchID == "" {
		t.Fatalf("搭景入参：%+v", got)
	}

	limited := httpx.Errf(errcode.RateLimited, "本小时的 AI 搭景次数已用完（每小时 20 次），12 分钟后再试").
		WithDetails(map[string]any{"retry_after_seconds": 700, "scope": "hourly", "limit": 20})
	dirs.buildErr = limited
	err := callErr(t, reg, ctxWithPrincipal(), "director_build", `{"projectId":"11","nodeId":"a_1","prompt":"x"}`)
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.RateLimited || be.Details["retry_after_seconds"] != 700 ||
		be.Error() != limited.Error() {
		t.Fatalf("限流应原样返回：%v", err)
	}

	// 节点不对：不存在 404、不是导演台 400，说清怎么办。
	dirs.buildErr = canvasgen.ErrNodeNotFound
	if err := callErr(t, reg, ctxWithPrincipal(), "director_build", `{"projectId":"11","nodeId":"zz","prompt":"x"}`); !errors.As(err, &be) ||
		be.Code != errcode.NotFound || !strings.Contains(be.Error(), "director_create") {
		t.Fatalf("节点不存在：%v", err)
	}
	dirs.buildErr = &canvasgen.NotDirectorError{NodeID: "img", Type: "image"}
	if err := callErr(t, reg, ctxWithPrincipal(), "director_build", `{"projectId":"11","nodeId":"img","prompt":"x"}`); !errors.As(err, &be) ||
		be.Code != errcode.InvalidArgument || !strings.Contains(be.Error(), "不是导演台") {
		t.Fatalf("不是导演台：%v", err)
	}

	// 参数在调端口之前就拒。
	n := len(dirs.built)
	// （缺 nodeId / prompt 由 inputSchema 的 required 挡在解码那一步。）
	for _, args := range []string{`{"projectId":"11","nodeId":"a_1","prompt":"  "}`, `{"projectId":"11","nodeId":" ","prompt":"x"}`,
		`{"projectId":"11","nodeId":"a_1","prompt":"` + strings.Repeat("长", 2001) + `"}`,
		`{"projectId":"11","nodeId":"a_1","prompt":"x","referenceMediaIds":["abc"]}`} {
		if err := callErr(t, reg, ctxWithPrincipal(), "director_build", args); err == nil {
			t.Errorf("%.60s 应被拒", args)
		}
	}
	if len(dirs.built) != n {
		t.Fatal("参数不对不应调到搭景")
	}
}
