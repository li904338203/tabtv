package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 3D 导演台工具（DRT-29）的单测：假画布（genCanvas）+ 假导演台端口。场景真写入、事件与来源在集成测试里验
// （agent/repo/integration：TestAgentDirector_建节点并搭景写入场景）。

const directorNotice = "需要用户打开导演台截图后，下游才有输出"

type fakeDirectors struct {
	owners   map[int64]string // 本项目已有的场景 → 绑定的节点
	creates  []app.DirectorSceneRequest
	builds   []app.DirectorBuildRequest
	buildErr error
}

func (f *fakeDirectors) CreateScene(_ context.Context, in app.DirectorSceneRequest) (int64, error) {
	f.creates = append(f.creates, in)
	for id, node := range f.owners {
		if node == in.CanvasNodeID {
			return id, nil
		}
	}
	if f.owners == nil {
		f.owners = map[int64]string{}
	}
	id := int64(9007199254740001 + len(f.creates))
	f.owners[id] = in.CanvasNodeID
	return id, nil
}

func (f *fakeDirectors) SceneNode(_ context.Context, _, _, sceneID int64, _ collab.Role) (string, error) {
	if node, ok := f.owners[sceneID]; ok {
		return node, nil
	}
	return "", app.ErrDirectorSceneNotFound
}

func (f *fakeDirectors) BuildAndApply(_ context.Context, in app.DirectorBuildRequest) (app.DirectorBuildOutcome, error) {
	f.builds = append(f.builds, in)
	if f.buildErr != nil {
		return app.DirectorBuildOutcome{}, f.buildErr
	}
	return app.DirectorBuildOutcome{SceneID: in.SceneID, Version: 2, Added: []string{"obj_character_2"},
		Updated: []string{"obj_camera_1"}, Defaults: []string{"老刀的姿势按默认站姿"}, Notice: directorNotice}, nil
}

func directorTools(c *genCanvas, d *fakeDirectors) (DirectorCreate, DirectorBuild) {
	deps := MultiDeps{Canvas: c, Directors: d}
	return DirectorCreate{deps}, DirectorBuild{deps}
}

// director_create：建节点（类型 director、默认标题）→ 按新节点建场景（来源 agent 的会话）→ 独立批次写回 sceneId。
func TestDirectorCreate_建节点建场景写回(t *testing.T) {
	c := &genCanvas{data: json.RawMessage(`{"nodes":[]}`)}
	d := &fakeDirectors{}
	create, _ := directorTools(c, d)
	res, err := create.Run(context.Background(), multiRC(7), json.RawMessage(`{"position":{"x":1,"y":2}}`))
	if err != nil {
		t.Fatal(err)
	}
	m := contentMap(t, res)
	if m["nodeId"] != "a_new" || m["sceneId"] != "9007199254740002" || !strings.Contains(m["next"].(string), "截图") {
		t.Fatalf("结果：%v", m)
	}
	if len(d.creates) != 1 || d.creates[0].CanvasNodeID != "a_new" || d.creates[0].SessionID != 3 ||
		d.creates[0].SourceSceneID != 0 {
		t.Fatalf("应按新节点建场景、来源为本会话：%+v", d.creates)
	}
	if len(c.applied) != 2 || !strings.Contains(string(c.applied[0].Ops), `"type":"director"`) ||
		!strings.Contains(string(c.applied[0].Ops), `"title":"3D 导演台"`) ||
		c.applied[1].BatchID != BatchID(multiRC(7))+":scene" ||
		!strings.Contains(string(c.applied[1].Ops), `"sceneId":"9007199254740002"`) {
		t.Fatalf("画布写入不对：%+v", c.applied)
	}
}

// director_build：节点上没有 sceneId 时先按节点补建（并写回），再搭景；搭景请求带会话、角色与会员等级。
func TestDirectorBuild_节点没有场景先补建(t *testing.T) {
	c := &genCanvas{data: json.RawMessage(`{"nodes":[{"id":"d1","type":"director","params":{}}]}`)}
	d := &fakeDirectors{}
	_, build := directorTools(c, d)
	rc := multiRC(8)
	rc.MemberLevelID = 4
	res, err := build.Run(context.Background(), rc,
		json.RawMessage(`{"nodeId":"d1","prompt":"两个人对峙，低机位","referenceMediaIds":["77",78]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.creates) != 1 || d.creates[0].CanvasNodeID != "d1" || d.creates[0].SourceSceneID != 0 {
		t.Fatalf("应先按节点补建场景：%+v", d.creates)
	}
	sceneID := int64(9007199254740002)
	if len(d.builds) != 1 || d.builds[0].SceneID != sceneID || d.builds[0].SessionID != 3 || d.builds[0].UserID != 5 ||
		d.builds[0].MemberLevelID != 4 || d.builds[0].Prompt != "两个人对峙，低机位" ||
		len(d.builds[0].ReferenceMediaIDs) != 2 || d.builds[0].ReferenceMediaIDs[1] != 78 {
		t.Fatalf("搭景请求不对：%+v", d.builds)
	}
	if len(c.applied) != 1 || c.applied[0].BatchID != BatchID(rc)+":scene" ||
		!strings.Contains(string(c.applied[0].Ops), `"sceneId":"9007199254740002"`) {
		t.Fatalf("补建的场景要写回节点：%+v", c.applied)
	}
	if m := contentMap(t, res); m["repair"] == nil || m["sceneId"] != "9007199254740002" {
		t.Fatalf("应说明补建了场景：%v", m)
	}
}

// 搭景的结果固定带截图提醒（DR-F-162），并给出变更清单；节点本来就指向自己的场景时不建、不写。
func TestDirectorBuild_结果带截图提醒(t *testing.T) {
	c := &genCanvas{data: json.RawMessage(`{"nodes":[{"id":"d1","type":"director","params":{"sceneId":"55"}}]}`)}
	d := &fakeDirectors{owners: map[int64]string{55: "d1"}}
	_, build := directorTools(c, d)
	res, err := build.Run(context.Background(), multiRC(9), json.RawMessage(`{"nodeId":"d1","prompt":"三人围坐"}`))
	if err != nil {
		t.Fatal(err)
	}
	m := contentMap(t, res)
	if m["notice"] != directorNotice || !strings.Contains(m["next"].(string), "截图") || m["version"] != float64(2) ||
		m["sceneId"] != "55" || m["repair"] != nil {
		t.Fatalf("结果：%v", m)
	}
	if added, _ := m["added"].([]any); len(added) != 1 || added[0] != "obj_character_2" {
		t.Fatalf("变更清单：%v", m)
	}
	if removed, ok := m["removed"].([]any); !ok || len(removed) != 0 {
		t.Fatalf("没有删除时 removed 是空数组而不是 null：%v", m["removed"])
	}
	if !strings.Contains(string(res.Display), directorNotice) {
		t.Fatalf("步骤行也要带提醒：%s", res.Display)
	}
	if len(d.creates) != 0 || len(c.applied) != 0 {
		t.Fatalf("自己的场景不应再建或写回：%+v %+v", d.creates, c.applied)
	}
}

// 限流 / 方案无法解析 / 不可用 / 冲突：原因、错误码与详情（retry_after_seconds 等）原样交给模型（DR-F-164）。
func TestDirectorBuild_限流原因原样返回(t *testing.T) {
	cases := []struct {
		err      error
		code     string
		detail   string
		nextWord string
	}{
		{httpx.Errf(errcode.RateLimited, "本小时的 AI 搭景次数已用完（每小时 20 次），12 分钟后再试").
			WithDetails(map[string]any{"retry_after_seconds": 700, "scope": "hourly", "limit": 20}),
			"RATE_LIMITED", "retry_after_seconds", "不要再调用"},
		{httpx.Errf(errcode.ConcurrencyLimited, "上一次 AI 搭景还没结束，请等它完成（同时最多 1 个）").
			WithDetails(map[string]any{"retry_after_seconds": 5, "scope": "concurrency", "limit": 1}),
			"CONCURRENCY_LIMITED", "retry_after_seconds", "不要连续重试"},
		{httpx.Errf(errcode.DirectorDraftInvalid, "搭景方案无法解析，请换个说法再试：没有对象").
			WithDetails(map[string]any{"reason": "没有对象"}), "DIRECTOR_DRAFT_INVALID", "reason", "换个"},
		{httpx.Errf(errcode.CapabilityUnsupported, "当前会员等级不能使用 AI 搭景").
			WithDetails(map[string]any{"userFixable": true}), "CAPABILITY_UNSUPPORTED", "userFixable", "用户自己能解决"},
	}
	for _, cs := range cases {
		c := &genCanvas{data: json.RawMessage(`{"nodes":[{"id":"d1","type":"director","params":{"sceneId":"55"}}]}`)}
		_, build := directorTools(c, &fakeDirectors{owners: map[int64]string{55: "d1"}, buildErr: cs.err})
		_, err := build.Run(context.Background(), multiRC(10), json.RawMessage(`{"nodeId":"d1","prompt":"x"}`))
		var rej *domain.ToolRejection
		if !errors.As(err, &rej) {
			t.Fatalf("%s：应是 ToolRejection（原样回给模型）：%v", cs.code, err)
		}
		var be *httpx.BizError
		errors.As(cs.err, &be)
		var content map[string]any
		_ = json.Unmarshal(rej.Content(), &content)
		if content["error"] != cs.code || content["message"] != be.Message || content[cs.detail] == nil ||
			content["code"] != float64(be.Code) || !strings.Contains(content["next"].(string), cs.nextWord) {
			t.Errorf("%s：给模型的内容不对：%v", cs.code, content)
		}
		// 步骤的 error_code 取 ToolRejection.Code（agent/app.stepTagOf）。
		if rej.Code != cs.code {
			t.Errorf("%s：步骤错误码应为 %s，得到 %s", cs.code, cs.code, rej.Code)
		}
	}
}

// 节点不对：不存在 / 不是导演台都说清怎么办，不建场景、不调模型。
func TestDirectorBuild_节点不是导演台(t *testing.T) {
	c := &genCanvas{data: json.RawMessage(`{"nodes":[{"id":"img","type":"image","params":{}}]}`)}
	d := &fakeDirectors{}
	_, build := directorTools(c, d)
	for node, want := range map[string]string{"img": "不是导演台", "nope": "没有节点"} {
		_, err := build.Run(context.Background(), multiRC(11), json.RawMessage(`{"nodeId":"`+node+`","prompt":"x"}`))
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "director_create") {
			t.Errorf("%s：%v", node, err)
		}
	}
	if len(d.creates) != 0 || len(d.builds) != 0 {
		t.Fatalf("不应建场景或搭景：%+v %+v", d.creates, d.builds)
	}
}

// 参数缺失在调用任何服务之前就拒；端口没接上时说「未接入」。
func TestDirectorTools_参数与未接入(t *testing.T) {
	c := &genCanvas{data: json.RawMessage(`{"nodes":[]}`)}
	d := &fakeDirectors{}
	_, build := directorTools(c, d)
	for _, args := range []string{`{"prompt":"x"}`, `{"nodeId":"d1","prompt":"  "}`, `{"nodeId":"d1","prompt":"x","bogus":1}`} {
		if _, err := build.Run(context.Background(), multiRC(12), json.RawMessage(args)); err == nil {
			t.Errorf("%s 应被拒", args)
		}
	}
	if len(d.builds) != 0 {
		t.Fatal("参数不对不应调到搭景")
	}
	if _, err := (DirectorCreate{MultiDeps{Canvas: c}}).Run(context.Background(), multiRC(13), json.RawMessage(`{}`)); err == nil ||
		!strings.Contains(err.Error(), "未接入") {
		t.Fatalf("没接导演台端口应报未接入：%v", err)
	}
}
