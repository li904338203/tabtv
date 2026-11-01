package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/li904338203/tabtv/backend/internal/canvas/domain"
	"github.com/li904338203/tabtv/backend/internal/canvas/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	director "github.com/li904338203/tabtv/backend/internal/director/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 3D 导演台场景（DRT-02）的应用层单测：都在碰库之前失败或是纯函数，所以不需要数据库。

const directorWS = int64(42)

// fakeMediaLookup 按 ID 回固定的媒体信息；查不到的回错误（同媒体域「不存在与不是你的同一个错误」）。
type fakeMediaLookup map[int64]MediaInfo

func (f fakeMediaLookup) LookupInWorkspace(_ context.Context, ws, id int64) (MediaInfo, error) {
	info, ok := f[id]
	if !ok {
		return MediaInfo{}, errors.New("not found")
	}
	// 与媒体域 GetScoped 同口径：本工作区与平台素材（0）都查得出来，别的工作区查不出来。
	if info.WorkspaceID != ws && info.WorkspaceID != 0 {
		return MediaInfo{}, errors.New("not found")
	}
	return info, nil
}

// sceneWith 在默认场景上改几处，返回正文。
func sceneWith(t *testing.T, edit func(m map[string]any)) json.RawMessage {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(director.DefaultScene(), &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func withPanorama(mediaID string) func(m map[string]any) {
	return func(m map[string]any) {
		m["background"] = map[string]any{"mode": "panorama", "color": "#060608",
			"panorama": map[string]any{"sourceNodeId": "n_pano", "mediaId": mediaID, "yaw": 0, "radius": 50}}
	}
}

func withSplat(mediaID string) func(m map[string]any) {
	return func(m map[string]any) {
		m["splat"] = map[string]any{"mediaId": mediaID, "scale": 1, "visible": true}
	}
}

func bizCode(t *testing.T, err error) (errcode.Code, map[string]any) {
	t.Helper()
	var be *httpx.BizError
	if !errors.As(err, &be) {
		t.Fatalf("应返回业务错误，得到 %v", err)
	}
	return be.Code, be.Details
}

func updateEditor(s *Service, version int, raw json.RawMessage) error {
	_, err := s.UpdateDirectorScene(context.Background(), directorWS, 7, 9, DirectorActor{UserID: 1001},
		collab.RoleEditor, version, raw)
	return err
}

func TestDirectorConflictDetails_bySelf与editorKind(t *testing.T) {
	const me = int64(1001)
	at := time.Date(2026, 10, 26, 8, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		cur      repo.DirectorScene
		bySelf   bool
		kind     string
		session  string
		grant    string
		contains string
	}{
		{"同事真人保存", repo.DirectorScene{Version: 5, Source: repo.EditorUser, EditorID: 2, EditorName: "苏夏"},
			false, "user", "", "", "苏夏"},
		{"本人另一个窗口", repo.DirectorScene{Version: 5, Source: repo.EditorUser, EditorID: me, EditorName: "林澈"},
			true, "user", "", "", "另一个窗口"},
		{"Agent 代本人搭景", repo.DirectorScene{Version: 5, Source: repo.EditorAgent, EditorID: me, EditorName: "林澈",
			AgentSessionID: 77}, false, "agent", "77", "", "Agent"},
		{"MCP 代本人搭景", repo.DirectorScene{Version: 5, Source: repo.EditorMCP, EditorID: me, EditorName: "林澈",
			McpGrantID: 5001}, false, "mcp", "", "5001", "外部 AI 客户端（代 林澈）"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.cur.UpdatedAt = at
			d := directorConflictDetails(c.cur, DirectorActor{UserID: me})
			if d["bySelf"] != c.bySelf {
				t.Errorf("bySelf 得到 %v，期望 %v", d["bySelf"], c.bySelf)
			}
			// DRD-46：currentSource 与 editorKind 同值、两个都给。
			if d["editorKind"] != c.kind || d["currentSource"] != c.kind {
				t.Errorf("editorKind / currentSource 应都是 %q，得到 %v / %v", c.kind, d["editorKind"], d["currentSource"])
			}
			if d["agentSessionId"] != c.session || d["mcpGrantId"] != c.grant {
				t.Errorf("会话 / 授权 ID 得到 %v / %v", d["agentSessionId"], d["mcpGrantId"])
			}
			if d["currentVersion"] != 5 || d["currentEditorName"] != c.cur.EditorName {
				t.Errorf("当前版本与编辑者不对：%v", d)
			}
			if d["currentUpdatedAt"] != "2026-10-26T08:00:00Z" {
				t.Errorf("currentUpdatedAt 得到 %v", d["currentUpdatedAt"])
			}
			msg := directorConflictMessage(c.cur, directorBySelf(c.cur, DirectorActor{UserID: me}))
			if !strings.Contains(msg, c.contains) {
				t.Errorf("文案 %q 应包含 %q", msg, c.contains)
			}
			if c.kind != "user" && strings.Contains(msg, "另一个窗口") {
				t.Errorf("机器写入不能说成本人另一个窗口：%q", msg)
			}
		})
	}
	// 机器发起的保存撞上本人真人写入，也不是「本人另一个窗口」。
	cur := repo.DirectorScene{Source: repo.EditorUser, EditorID: me}
	if directorBySelf(cur, DirectorActor{UserID: me, AgentSessionID: 9}) {
		t.Error("Agent 保存时撞上的冲突不能算 bySelf")
	}
	// 最后编辑者未知时 currentEditorId 是空串（契约：空串表示未知），而不是 "0"。
	if d := directorConflictDetails(repo.DirectorScene{Source: repo.EditorUser}, DirectorActor{UserID: me}); d["currentEditorId"] != "" {
		t.Errorf("未知编辑者应为空串，得到 %#v", d["currentEditorId"])
	}
}

func TestUpdateDirectorScene_版本为0拒绝(t *testing.T) {
	code, _ := bizCode(t, updateEditor(&Service{}, 0, director.DefaultScene()))
	if code != errcode.InvalidArgument {
		t.Fatalf("version=0 应 40001，得到 %d", code)
	}
}

func TestUpdateDirectorScene_只读成员被拒(t *testing.T) {
	_, err := (&Service{}).UpdateDirectorScene(context.Background(), directorWS, 7, 9, DirectorActor{UserID: 1},
		collab.RoleViewer, 1, director.DefaultScene())
	if code, _ := bizCode(t, err); code != errcode.PermissionDenied {
		t.Fatalf("viewer 保存应 403，得到 %d", code)
	}
}

func TestUpdateDirectorScene_结构错误带字段路径(t *testing.T) {
	raw := sceneWith(t, func(m map[string]any) {
		objs := m["objects"].([]any)
		cam := objs[0].(map[string]any)["camera"].(map[string]any)
		cam["aspect"] = "2:1"
	})
	code, details := bizCode(t, updateEditor(&Service{}, 1, raw))
	if code != errcode.InvalidArgument || details["path"] != "objects[0].camera.aspect" {
		t.Fatalf("应 40001 且 path 指到 objects[0].camera.aspect，得到 %d %v", code, details)
	}
	// 比服务端新的结构版本：拒绝，并指到 schemaVersion。
	newer := sceneWith(t, func(m map[string]any) { m["schemaVersion"] = 2 })
	code, details = bizCode(t, updateEditor(&Service{}, 1, newer))
	if code != errcode.InvalidArgument || details["path"] != "schemaVersion" {
		t.Fatalf("结构版本过新应 40001 且 path=schemaVersion，得到 %d %v", code, details)
	}
}

func TestUpdateDirectorScene_媒体不属于本工作区拒绝(t *testing.T) {
	s := &Service{}
	s.SetMediaLookup(fakeMediaLookup{
		501: {WorkspaceID: 99, Kind: "image", Purpose: "upload_reference"},          // 别的工作区
		502: {WorkspaceID: 0, Kind: "image", Purpose: "material_cover"},             // 平台素材
		503: {WorkspaceID: directorWS, Kind: "video", Purpose: "generation_output"}, // 不是图片
	})
	cases := []struct {
		name, mediaID, reason string
	}{
		{"别的工作区", "501", "不存在"},
		{"平台素材", "502", "不存在"},
		{"根本没有", "509", "不存在"},
		{"不是图片", "503", "必须是图片"},
	}
	var first string
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := updateEditor(s, 1, sceneWith(t, withPanorama(c.mediaID)))
			code, details := bizCode(t, err)
			if code != errcode.InvalidArgument || details["path"] != "background.panorama.mediaId" {
				t.Fatalf("应 40001 且 path 指到全景媒体，得到 %d %v", code, details)
			}
			if !strings.Contains(err.Error(), c.reason) {
				t.Errorf("原因应包含 %q：%v", c.reason, err)
			}
			// 「别的工作区」与「根本没有」必须是同一句话：区分开等于提供一个按 ID 探测别人文件的接口。
			if c.reason == "不存在" {
				if first == "" {
					first = err.Error()
				} else if err.Error() != first {
					t.Errorf("不存在与不是你的应回同一句话：%q vs %q", err.Error(), first)
				}
			}
		})
	}
	// 没接媒体查询时不能放行（放行等于把别人的文件当成已授权）。
	err := updateEditor(&Service{}, 1, sceneWith(t, withPanorama("501")))
	if code, _ := bizCode(t, err); code == errcode.InvalidArgument || code == 0 {
		t.Fatalf("没接媒体查询时应报内部错误而不是放行或当成参数错误，得到 %d", code)
	}
}

func TestUpdateDirectorScene_泼溅必须是导演台素材(t *testing.T) {
	s := &Service{}
	s.SetMediaLookup(fakeMediaLookup{
		601: {WorkspaceID: directorWS, Kind: "", Purpose: "upload_reference"},
	})
	err := updateEditor(s, 1, sceneWith(t, withSplat("601")))
	code, details := bizCode(t, err)
	if code != errcode.InvalidArgument || details["path"] != "splat.mediaId" {
		t.Fatalf("参考图上传的文件不能当泼溅：应 40001 且 path=splat.mediaId，得到 %d %v", code, details)
	}
}

func TestUpdateDirectorScene_超大回41301(t *testing.T) {
	raw := sceneWith(t, func(m map[string]any) {
		m["note"] = strings.Repeat("x", director.MaxSceneBytes)
	})
	code, _ := bizCode(t, updateEditor(&Service{}, 1, raw))
	if code != errcode.PayloadTooLarge {
		t.Fatalf("超过 512 KiB 应 41301，得到 %d", code)
	}
}

// ---------------------------------------------------------------- 事件

func loadDirectorEventSchema(t *testing.T) (schemaNode, *regexp.Regexp) {
	t.Helper()
	raw, err := os.ReadFile("../../../../api/events/director-scene-updated.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		schemaNode
		Defs map[string]struct {
			Pattern string `json:"pattern"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s.schemaNode, regexp.MustCompile(s.Defs["id"].Pattern)
}

func TestDirectorSceneUpdated_与事件契约一致(t *testing.T) {
	schema, idPattern := loadDirectorEventSchema(t)
	now := time.Date(2026, 10, 26, 9, 30, 0, 0, time.UTC)
	cases := []struct {
		name string
		sc   repo.DirectorScene
	}{
		{"网页保存", repo.DirectorScene{ID: 3001, ProjectID: 77, CanvasNodeID: "n_dir", Version: 2,
			RenderFingerprint: "abc", Source: "user", EditorID: 1001, EditorName: "林澈"}},
		{"Agent 写入", repo.DirectorScene{ID: 3001, ProjectID: 77, CanvasNodeID: "n_dir", Version: 3,
			RenderFingerprint: "def", Source: "agent", EditorID: 1001, AgentSessionID: 7}},
		{"编辑者未知", repo.DirectorScene{ID: 3001, ProjectID: 77, CanvasNodeID: "n_dir", Version: 4,
			RenderFingerprint: "ghi", Source: "mcp", McpGrantID: 5001}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := json.Marshal(NewDirectorSceneUpdated(c.sc, now))
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatal(err)
			}
			checkObject(t, "事件", m, schema)
			if m["type"] != schema.Properties["type"].Const {
				t.Errorf("type 应为 %q，得到 %v", schema.Properties["type"].Const, m["type"])
			}
			for _, k := range []string{"projectId", "sceneId"} {
				if s, _ := m[k].(string); !idPattern.MatchString(s) {
					t.Errorf("%s 应是十进制字符串 ID，得到 %#v", k, m[k])
				}
			}
			if v := m["editorId"]; v != nil && !idPattern.MatchString(v.(string)) {
				t.Errorf("editorId 应是 null 或字符串 ID，得到 %#v", v)
			}
			if c.sc.EditorID == 0 && m["editorId"] != nil {
				t.Errorf("编辑者未知时 editorId 应为 null，得到 %#v", m["editorId"])
			}
			if !slices.Contains(schema.Properties["editorKind"].Enum, m["editorKind"].(string)) {
				t.Errorf("editorKind %v 不在契约枚举里", m["editorKind"])
			}
			if _, err := time.Parse(time.RFC3339Nano, m["occurredAt"].(string)); err != nil {
				t.Errorf("occurredAt 不是 date-time: %v", m["occurredAt"])
			}
		})
	}
}

func TestDirectorSceneUpdated_发到项目topic(t *testing.T) {
	pub := &capturePublisher{}
	s := &Service{}
	s.SetEvents(pub)
	s.publishDirectorScene(context.Background(), repo.DirectorScene{ID: 3001, ProjectID: 77, Version: 2,
		Source: "user", RenderFingerprint: "abc", CanvasNodeID: "n_dir"})
	if pub.topic != "project:77" || !json.Valid(pub.payload) {
		t.Fatalf("应发到 project:77 且载荷是合法 JSON，得到 %q %s", pub.topic, pub.payload)
	}
}

func TestDirectorSceneUpdated_保存失败不发(t *testing.T) {
	pub := &capturePublisher{}
	s := &Service{}
	s.SetEvents(pub)
	s.SetMediaLookup(fakeMediaLookup{})
	for _, raw := range []json.RawMessage{
		sceneWith(t, withPanorama("501")),                         // 媒体不存在
		sceneWith(t, func(m map[string]any) { m["objects"] = 1 }), // 结构错误
	} {
		if err := updateEditor(s, 1, raw); err == nil {
			t.Fatal("这份正文应保存失败")
		}
	}
	if err := updateEditor(s, 0, director.DefaultScene()); err == nil {
		t.Fatal("version=0 应保存失败")
	}
	if pub.topic != "" || pub.payload != nil {
		t.Fatalf("保存失败不该发事件，却发了 %q %s", pub.topic, pub.payload)
	}
}

// ---------------------------------------------------------------- 下游约束段（DRT-07）

// 输入解析按参考文件的 purpose 汇总导演台产物（DRD-18）；purpose 不进快照。
func TestResolveAccept_按purpose汇总DirectorKinds(t *testing.T) {
	s := &Service{}
	s.SetMediaLookup(fakeMediaLookup{
		701: {WorkspaceID: directorWS, Kind: "image", Purpose: "director_capture"},
		702: {WorkspaceID: directorWS, Kind: "video", Purpose: "director_preview"},
		703: {WorkspaceID: directorWS, Kind: "image", Purpose: "upload_reference"},
	})
	resolve := func(ids ...int64) []domain.ResolvedInput {
		var out []domain.ResolvedInput
		for i, id := range ids {
			r, issue := s.resolveReference(context.Background(), directorWS,
				domain.DeclaredInput{Kind: domain.InputReference, Order: i + 1, ReferenceID: id})
			if issue != nil {
				t.Fatalf("参考文件 %d 应解析成功：%+v", id, issue)
			}
			out = append(out, r)
		}
		return out
	}
	cases := []struct {
		name string
		ids  []int64
		want string
	}{
		{"只有截图", []int64{701}, "capture"},
		{"只有预演", []int64{702}, "preview"},
		{"两者都有，顺序固定先图后视频", []int64{702, 701, 701}, "capture,preview"},
		{"普通参考图不算", []int64{703}, ""},
	}
	for _, c := range cases {
		got := strings.Join(DirectorKindsOf(resolve(c.ids...)), ",")
		if got != c.want {
			t.Errorf("%s：DirectorKinds 应为 %q，得到 %q", c.name, c.want, got)
		}
	}
	// purpose 不进快照：输入快照的 JSON 与改动前同形（`json:"-"`）。
	b, _ := json.Marshal(resolve(701))
	if strings.Contains(string(b), "director_capture") || strings.Contains(string(b), "urpose") {
		t.Fatalf("purpose 不该写进输入快照：%s", b)
	}
}
