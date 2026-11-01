package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 写工具（MCPT-11）的单测：假端口 + 与 transport 相同的四步（call）。所有权、乐观锁、退款只一次这些
// 由各域判的规则在集成测试里用真库验（tools/integration/write_integration_test.go）。

// ---------------------------------------------------------------- 假端口

// autoClock 自动推进：After(d) 立刻把时间拨过 d —— tasks.wait 在单测里不真的等。
type autoClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *autoClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *autoClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.now = c.now.Add(d)
	t := c.now
	c.mu.Unlock()
	ch := make(chan time.Time, 1)
	ch <- t
	return ch
}

type fakeTaskControl struct {
	calls []string
}

func (f *fakeTaskControl) Cancel(_ context.Context, _ app.Principal, kind string, id int64, _ string) (app.CancelResult, error) {
	f.calls = append(f.calls, fmt.Sprintf("%s:%d", kind, id))
	if id == doneTaskID {
		return app.CancelResult{Task: app.TaskDetail{Kind: kind, ID: id, RawStatus: "success"}}, nil
	}
	return app.CancelResult{Cancelled: true, Task: app.TaskDetail{Kind: kind, ID: id, RawStatus: "cancelled"}}, nil
}

type fakeUploader struct{ got app.UploadSource }

func (f *fakeUploader) UploadReference(_ context.Context, _ app.Principal, src app.UploadSource) (app.Reference, error) {
	f.got = src
	return app.Reference{ID: 4242, Kind: "image", MIME: "image/png", SizeBytes: int64(len(src.Data)), Width: 2, Height: 2}, nil
}

type fakeCanvasWriter struct {
	got     app.CanvasApplyInput
	batches []string
	err     error
}

func (f *fakeCanvasWriter) Apply(_ context.Context, _ app.Principal, in app.CanvasApplyInput) (app.CanvasApplyResult, error) {
	f.got = in
	f.batches = append(f.batches, in.BatchID)
	if f.err != nil {
		return app.CanvasApplyResult{}, f.err
	}
	return app.CanvasApplyResult{NodeIDs: []string{"a_1"}, FromVersion: 3, ToVersion: 4}, nil
}

type fakeDocWriter struct{ version int }

func (f *fakeDocWriter) Put(_ context.Context, _ app.Principal, _ int64, path, _ string, version int) (app.Document, bool, error) {
	f.version = version
	return app.Document{DocumentSummary: app.DocumentSummary{ID: 8, Path: path, Version: version + 1, Source: "mcp",
		UpdatedAt: t0}}, version == 0, nil
}

type fakeProjectCreator struct{ keys []string }

func (f *fakeProjectCreator) Create(_ context.Context, _ app.Principal, name, key string) (app.ProjectSummary, string, bool, error) {
	f.keys = append(f.keys, key)
	return app.ProjectSummary{ID: 77, Name: name, Version: 1, CreatedAt: t0, UpdatedAt: t0}, "active",
		len(f.keys) > 1, nil
}

// writeCalls 覆盖全部写 method（TestMCPTools_输出符合outputSchema 据此判覆盖）。
var writeCalls = []struct{ tool, method, args string }{
	{"tasks", "cancel", `{"method":"cancel","kind":"generation","id":"301","reason":"不要了"}`},
	{"media", "upload", `{"method":"upload","base64":"` + base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n")) +
		`","fileName":"a.png"}`},
	{"canvas_apply", "", `{"projectId":"11","idempotencyKey":"k1","ops":[{"kind":"add_node","ref":"ref:a",` +
		`"node":{"type":"image","title":"猫","params":{"prompt":"一只猫"}},"position":{"x":10,"y":20}},` +
		`{"kind":"add_edge","edge":{"source":"n1","target":"ref:a"}}]}`},
	{"documents", "put", `{"method":"put","projectId":"11","path":"/notes/a.md","content":"x","version":0}`},
	{"projects", "create", `{"method":"create","name":"新片","idempotencyKey":"abc"}`},
	// 多段工具集（MCPT-16）的不花钱写 method。
	{"script", "create", `{"method":"create","projectId":"11","text":"三打白骨精","idempotencyKey":"s1"}`},
	{"workflow", "cancel", `{"method":"cancel","runId":"41"}`},
	{"timeline", "assemble", `{"method":"assemble","scriptId":"21","dryRun":false}`},
	// 3D 导演台（DRT-29）：不花钱的单方法写工具。
	{"director_create", "", `{"projectId":"11","title":"对峙","position":{"x":1,"y":2},"idempotencyKey":"d1"}`},
	{"director_build", "", `{"projectId":"11","nodeId":"a_1","prompt":"两个人对峙","referenceMediaIds":["77"]}`},
	// 智能剪辑（SCT-41）：不花钱的单方法写工具。
	{"smartcut_create", "", `{"projectId":"11","title":"新品","inputNodeIds":["v1"],"idempotencyKey":"c1"}`},
	{"smartcut_plan", "", `{"projectId":"11","nodeId":"a_sc","mode":"mix","instruction":"15 秒卡点"}`},
}

func writeCovered(tool, method string) bool {
	for _, c := range writeCalls {
		if c.tool == tool && c.method == method {
			return true
		}
	}
	return false
}

func TestMCPWriteTools_输出符合outputSchema(t *testing.T) {
	reg := NewRegistry(fakeDeps())
	for _, c := range writeCalls {
		m, _ := call(t, reg, c.tool, c.args, 6000)
		walkIDs(t, c.tool, "", m)
	}
}

func TestMCPWriteTools_声明如实(t *testing.T) {
	reg := NewRegistry(Deps{})
	want := map[string][3]bool{ // Write, Paid, Destructive
		"tasks.cancel": {true, false, true}, "tasks.wait": {false, false, false},
		"media.upload": {true, false, false}, "canvas_apply.": {true, false, false},
		"documents.put": {true, false, false}, "projects.create": {true, false, false},
		"director_create.": {true, false, false}, "director_build.": {true, false, false},
	}
	for _, tl := range reg.List() {
		for _, m := range tl.Methods {
			w, ok := want[tl.Name+"."+m.Name]
			if !ok {
				continue
			}
			if got := [3]bool{m.Write, m.Paid, m.Destructive}; got != w {
				t.Errorf("%s.%s 声明 Write/Paid/Destructive=%v，应为 %v", tl.Name, m.Name, got, w)
			}
			delete(want, tl.Name+"."+m.Name)
		}
	}
	if len(want) > 0 {
		t.Errorf("缺 method：%v", want)
	}
	ca, _ := reg.Lookup("canvas_apply")
	if a := ca.Annotations(); a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint {
		t.Errorf("canvas_apply 应非只读、非破坏：%+v", a)
	}
}

func TestCancel_导出任务明确不支持(t *testing.T) {
	d := fakeDeps()
	tc := d.TaskControl.(*fakeTaskControl)
	reg := NewRegistry(d)
	err := callErr(t, reg, ctxWithPrincipal(), "tasks", `{"method":"cancel","kind":"export","id":"5"}`)
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.InvalidArgument || !strings.Contains(be.Error(), "不支持取消") {
		t.Fatalf("导出取消应明确拒绝：%v", err)
	}
	if len(tc.calls) != 0 {
		t.Fatalf("导出取消不应调到任何取消入口：%v", tc.calls)
	}
	// 已结束的任务：cancelled=false、不报错。
	m, _ := call(t, reg, "tasks", `{"method":"cancel","kind":"render","id":"999"}`, 6000)
	if m["cancelled"] != false || m["task"].(map[string]any)["status"] != "completed" {
		t.Fatalf("已结束的任务应 cancelled=false：%v", m)
	}
	m, _ = call(t, reg, "tasks", `{"method":"cancel","kind":"workflow","id":"301"}`, 6000)
	if m["cancelled"] != true || m["task"].(map[string]any)["status"] != "cancelled" {
		t.Fatalf("取消成功应 cancelled=true：%v", m)
	}
}

func TestWait_工具到点返回working且不含失败(t *testing.T) {
	reg := NewRegistry(fakeDeps())
	m, out := call(t, reg, "tasks", `{"method":"wait","kind":"generation","id":"301","timeoutSec":10}`, 6000)
	task := m["task"].(map[string]any)
	if task["status"] != "working" || m["stillRunning"] != true || m["waitedSec"] != float64(10) {
		t.Fatalf("到点应返回 working + stillRunning：%v", m)
	}
	raw, _ := json.Marshal(m)
	for _, s := range []string{out.Summary, string(raw)} {
		if strings.Contains(s, "失败") || strings.Contains(s, "failed") {
			t.Fatalf("等待超时的结果不能出现「失败」：%s", s)
		}
	}
	// 请求 120 秒被夹到上限（默认 50 秒）。
	m, _ = call(t, reg, "tasks", `{"method":"wait","kind":"generation","id":"301","timeoutSec":120}`, 6000)
	if m["waitedSec"] != float64(50) {
		t.Fatalf("应夹到 50 秒：%v", m["waitedSec"])
	}
	// 已结束：立即返回。
	m, _ = call(t, reg, "tasks", `{"method":"wait","kind":"generation","id":"999"}`, 6000)
	if m["stillRunning"] != false || m["waitedSec"] != float64(0) || m["task"].(map[string]any)["status"] != "completed" {
		t.Fatalf("终态应立即返回：%v", m)
	}
}

func TestCanvasApplyArgs_不允许link_generation(t *testing.T) {
	for _, kind := range []string{"link_generation", "set_frame_members", "delete_node", ""} {
		_, _, err := canvasApplyArgs(canvasApplyIn{ProjectID: "11", Ops: []canvasOpIn{{Kind: kind}}})
		if err == nil || !strings.Contains(err.Error(), "不支持") {
			t.Errorf("kind %q 应被拒：%v", kind, err)
		}
	}
	if _, _, err := canvasApplyArgs(canvasApplyIn{ProjectID: "11"}); err == nil {
		t.Error("空 ops 应被拒")
	}
	if _, _, err := canvasApplyArgs(canvasApplyIn{ProjectID: "11", Ops: make([]canvasOpIn, 201)}); err == nil {
		t.Error("超过 200 个操作应被拒")
	}
	// 转换保留字段、不多出空键。
	_, raw, err := canvasApplyArgs(canvasApplyIn{ProjectID: "11", Ops: []canvasOpIn{
		{Kind: "move_node", NodeID: "a_1", Position: &canvasPointIn{X: 1, Y: 2}},
		{Kind: "update_node", NodeID: "a_1", Patch: map[string]any{"title": "新"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `[{"kind":"move_node","nodeId":"a_1","position":{"x":1,"y":2}},{"kind":"update_node","nodeId":"a_1","patch":{"title":"新"}}]` {
		t.Fatalf("转换结果不对：%s", raw)
	}
}

func TestCanvasApply_批次与所有权错误(t *testing.T) {
	d := fakeDeps()
	cw := d.CanvasWriter.(*fakeCanvasWriter)
	reg := NewRegistry(d)
	args := `{"projectId":"11","idempotencyKey":"same","ops":[{"kind":"add_node","node":{"type":"text"}}]}`
	call(t, reg, "canvas_apply", args, 6000)
	call(t, reg, "canvas_apply", args, 6000)
	noKey := `{"projectId":"11","ops":[{"kind":"add_node","node":{"type":"text"}}]}`
	call(t, reg, "canvas_apply", noKey, 6000)
	call(t, reg, "canvas_apply", noKey, 6000)
	b := cw.batches
	if b[0] != b[1] || b[2] == b[3] || b[0] == b[2] || !strings.HasPrefix(b[0], "mcp:5:k:") || len(b[2]) > 64 {
		t.Fatalf("同键同批、不带键每次新批：%v", b)
	}
	cw.err = fmt.Errorf("%w: a_9", app.ErrNodeNotOwned)
	err := callErr(t, reg, ctxWithPrincipal(), "canvas_apply", args)
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.McpNodeNotOwned || !strings.Contains(be.Error(), "请在旁边新建") {
		t.Fatalf("所有权不符应回 40912：%v", err)
	}
}

func TestMediaUploadArgs_base64超限与二选一(t *testing.T) {
	big := base64.StdEncoding.EncodeToString(make([]byte, maxBase64Bytes+1))
	ok := base64.StdEncoding.EncodeToString([]byte("img"))
	cases := []struct {
		in   mediaIn
		code errcode.Code
		has  string
	}{
		{mediaIn{}, errcode.InvalidArgument, "二选一"},
		{mediaIn{URL: "https://x/a.png", Base64: ok}, errcode.InvalidArgument, "只能给一个"},
		{mediaIn{Base64: ok}, errcode.InvalidArgument, "fileName"},
		{mediaIn{Base64: big, FileName: "a.png"}, errcode.PayloadTooLarge, "用 url"},
		{mediaIn{Base64: "!!!", FileName: "a.png"}, errcode.InvalidArgument, "解码失败"},
		{mediaIn{MediaID: "1", URL: "https://x"}, errcode.InvalidArgument, "mediaId"},
	}
	for _, c := range cases {
		_, err := uploadSource(c.in)
		var be *httpx.BizError
		if !errors.As(err, &be) || be.Code != c.code || !strings.Contains(be.Error(), c.has) {
			t.Errorf("%+v：应报 %d「%s」，实际 %v", c.in, c.code, c.has, err)
		}
	}
	src, err := uploadSource(mediaIn{Base64: "data:image/png;base64," + ok, FileName: "a.png", MimeType: "image/png"})
	if err != nil || string(src.Data) != "img" || !src.ImageOnly || src.DeclaredMIME != "image/png" {
		t.Fatalf("data: 前缀应被容忍且只收图片：%+v %v", src, err)
	}
	src, err = uploadSource(mediaIn{URL: " https://cdn.example/v.mp4 "})
	if err != nil || src.URL != "https://cdn.example/v.mp4" || src.ImageOnly || src.Data != nil {
		t.Fatalf("url 来源：%+v %v", src, err)
	}
}

func TestProjectsCreate_幂等键按授权哈希(t *testing.T) {
	d := fakeDeps()
	pc := d.ProjectCreator.(*fakeProjectCreator)
	reg := NewRegistry(d)
	m, _ := call(t, reg, "projects", `{"method":"create","name":"片","idempotencyKey":"k"}`, 6000)
	m2, _ := call(t, reg, "projects", `{"method":"create","name":"片","idempotencyKey":"k"}`, 6000)
	if m["replayed"] != false || m2["replayed"] != true || pc.keys[0] != pc.keys[1] || pc.keys[0] != domain.ProjectIdemKey(5, "k") {
		t.Fatalf("%v %v %v", m, m2, pc.keys)
	}
	if err := callErr(t, reg, ctxWithPrincipal(), "projects", `{"method":"create","name":"片"}`); err == nil ||
		!strings.Contains(err.Error(), "idempotencyKey") {
		t.Fatalf("不带幂等键应被拒：%v", err)
	}
}

func TestDocumentsPut_参数(t *testing.T) {
	reg := NewRegistry(fakeDeps())
	for _, args := range []string{
		`{"method":"put","projectId":"11","content":"x","version":0}`,
		`{"method":"put","projectId":"11","path":"/notes/a.md","version":0}`,
		`{"method":"put","projectId":"11","path":"/notes/a.md","content":"x"}`,
	} {
		if err := callErr(t, reg, ctxWithPrincipal(), "documents", args); err == nil {
			t.Errorf("%s 应被拒", args)
		}
	}
	m, _ := call(t, reg, "documents", `{"method":"put","projectId":"11","path":"/notes/a.md","content":"","version":3}`, 6000)
	if m["created"] != false || m["document"].(map[string]any)["source"] != "mcp" {
		t.Fatalf("%v", m)
	}
}
