//go:build integration

// 项目 zip 导出 / 导入（TT-T-33）：真库 + 本地对象存储。
//
// 覆盖：包里没有 Key（TT-F-10）、往返后除 ID 外一致且未知字段保留、媒体字节一致、
// 同一个包导入两次是两个独立项目、恶意包（zip slip / 未知大版本 / 不是 zip）拒绝且无残留、
// 中途失败整体回滚（库里无行、存储里无文件）。
package integration

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/objstore"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
	"github.com/li904338203/tabtv/backend/internal/projectpkg"
)

const secretKey = "sk-TABTV-TEST-SECRET-must-never-leak"

var owner = projectpkg.Owner{WorkspaceID: 500, UserID: 501, PayerID: 502}

type env struct {
	pool  *pgxpool.Pool
	store *objstore.Client
	root  string
	next  func() int64
	svc   *projectpkg.Service
	ids   map[string]int64
	files map[string][]byte // 旧媒体 ID → 字节
}

func setup(t *testing.T) *env {
	t.Helper()
	pool := testsupport.Fresh(t)
	root := t.TempDir()
	store, err := objstore.NewLocal(root, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	gen, _ := id.New(0)
	e := &env{pool: pool, store: store, root: root, next: gen.Next, ids: map[string]int64{}, files: map[string][]byte{}}
	e.svc = projectpkg.New(pool, store, gen.Next, "test")
	e.seed(t)
	return e
}

func (e *env) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s：%v", sql, err)
	}
}

func (e *env) id(name string) int64 {
	if v, ok := e.ids[name]; ok {
		return v
	}
	v := e.next()
	e.ids[name] = v
	return v
}

func (e *env) media(t *testing.T, name, ct, purpose string, body []byte, withFile bool) int64 {
	t.Helper()
	mid := e.id(name)
	key := fmt.Sprintf("%s/2026/11/02/%d%s", purpose, mid, map[string]string{"image/png": ".png", "video/mp4": ".mp4"}[ct])
	if withFile {
		if err := e.store.Put(context.Background(), key, bytes.NewReader(body), int64(len(body)), ct); err != nil {
			t.Fatal(err)
		}
		e.files[fmt.Sprint(mid)] = body
	}
	e.exec(t, `insert into media_object (id, workspace_id, bucket, object_key, content_type, size_bytes, width, height, purpose, created_by_user_id)
		values ($1, $2, $3, $4, $5, $6, 64, 64, $7, $8)`, mid, owner.WorkspaceID, e.store.Bucket(), key, ct, len(body), purpose, owner.UserID)
	return mid
}

// seed 建一个覆盖画布 / 生成 / 上传 / 脚本 / 组 / 时间线 / 导演台 / 项目文档的项目，外加一个配了 Key 的厂商。
func (e *env) seed(t *testing.T) {
	t.Helper()
	e.exec(t, `insert into api_protocol (code, name) values ('openai_images', 'OpenAI Images') on conflict do nothing`)
	e.exec(t, `insert into provider_config (id, code, name, adapter_type, base_url, api_key_cipher) values ($1, 'openai', 'OpenAI', 'http', 'https://api.example.invalid', $2)`,
		e.id("provider"), secretKey)
	e.exec(t, `insert into operation (id, code, name, domain, out_node_type) values ($1, 'text2img', '文生图', 'image', 'image')`, e.id("op"))
	e.exec(t, `insert into model (id, code, name, provider_id, domain, upstream_model_code, api_protocol) values ($1, 'm-img', '图', $2, 'image', 'gpt-image', 'openai_images')`,
		e.id("model"), e.id("provider"))

	m1 := e.media(t, "m_out", "image/png", "generation_output", []byte("PNG-output-bytes"), true)
	m2 := e.media(t, "m_upload", "image/png", "upload_reference", []byte("PNG-upload-bytes"), true)
	m3 := e.media(t, "m_asset", "image/png", "script_asset", []byte("PNG-asset-bytes"), true)
	m4 := e.media(t, "m_video", "video/mp4", "generation_output", []byte("MP4-video-bytes"), true)
	e.media(t, "m_gone", "image/png", "upload_reference", nil, false) // 行在、文件没了

	pid, g1, g2 := e.id("project"), e.id("gen1"), e.id("gen2")
	canvas := map[string]any{
		"schemaVersion":  2,
		"futureTopLevel": map[string]any{"keep": true},
		"nodes": []any{
			map[string]any{"id": "img1", "type": "image", "position": map[string]any{"x": 1, "y": 2}, "futureField": []any{1, "a"},
				"params": map[string]any{"prompt": "一只猫", "references": []any{fmt.Sprint(m2), fmt.Sprint(e.id("m_gone"))}},
				"result": map[string]any{"id": fmt.Sprint(g1), "status": "success", "outputType": "image",
					"outputUrl": fmt.Sprintf("/api/v1/generations/%d/outputs/0", g1)}},
			map[string]any{"id": "s1", "type": "script", "params": map[string]any{"scriptId": fmt.Sprint(e.id("script"))}},
			map[string]any{"id": "tl1", "type": "smartcut", "params": map[string]any{"timelineId": fmt.Sprint(e.id("timeline"))}},
		},
		"edges": []any{map[string]any{"id": "e1", "source": "img1", "target": "s1", "order": 1}},
	}
	raw, _ := json.Marshal(canvas)
	e.exec(t, `insert into project (id, workspace_id, created_by_user_id, name, canvas_data, cover_media_id, version)
		values ($1, $2, $3, 'P', $4::jsonb, $5, 7)`, pid, owner.WorkspaceID, owner.UserID, string(raw), m1)

	e.exec(t, `insert into generation (id, workspace_id, payer_account_id, created_by_user_id, project_id, node_id, operation_id, model_id,
		result_type, params_snapshot, idempotency_key, status, provider_task_id, provider_raw_result, error_detail)
		values ($1, $2, $3, $4, $5, 'img1', $6, $7, 'image', '{"prompt":"一只猫"}', 'k1', 'success', 'up-task-1', '{"upstream":"raw"}', 'upstream said x')`,
		g1, owner.WorkspaceID, owner.PayerID, owner.UserID, pid, e.id("op"), e.id("model"))
	e.exec(t, `insert into generation_output (id, generation_id, idx, media_object_id, output_type) values ($1, $2, 0, $3, 'image')`, e.next(), g1, m1)
	// 进行中的任务：导入后应判失败
	e.exec(t, `insert into generation (id, workspace_id, payer_account_id, created_by_user_id, project_id, node_id, operation_id, model_id,
		result_type, idempotency_key, status) values ($1, $2, $3, $4, $5, 'img1', $6, $7, 'video', 'k2', 'running')`,
		g2, owner.WorkspaceID, owner.PayerID, owner.UserID, pid, e.id("op"), e.id("model"))
	e.exec(t, `insert into generation_output (id, generation_id, idx, media_object_id, output_type) values ($1, $2, 0, $3, 'video')`, e.next(), g2, m4)

	e.exec(t, `insert into upload_reference (id, workspace_id, project_id, node_id, media_object_id, media_type, created_by_user_id)
		values ($1, $2, $3, 'img1', $4, 'image', $5)`, e.next(), owner.WorkspaceID, pid, m2, owner.UserID)

	e.exec(t, `insert into script_instance (id, workspace_id, project_id, canvas_node_id, source_type) values ($1, $2, $3, 's1', 'script')`,
		e.id("script"), owner.WorkspaceID, pid)
	e.exec(t, `insert into shot (id, script_id, shot_no) values ($1, $2, 1)`, e.id("shot"), e.id("script"))
	e.exec(t, `insert into script_asset (id, workspace_id, asset_type, name, media_object_id, status) values ($1, $2, 'character', '小猫', $3, 'ready')`,
		e.id("asset"), owner.WorkspaceID, m3)
	e.exec(t, `insert into shot_asset_ref (id, shot_id, script_asset_id) values ($1, $2, $3)`, e.next(), e.id("shot"), e.id("asset"))
	e.exec(t, `insert into group_node (id, workspace_id, project_id, canvas_node_id, group_type) values ($1, $2, $3, 'grp1', 'storyboard_image')`,
		e.id("group"), owner.WorkspaceID, pid)
	e.exec(t, `insert into group_item (id, group_node_id, item_no, shot_id, generation_id, status) values ($1, $2, 1, $3, $4, 'queued')`,
		e.id("gitem"), e.id("group"), e.id("shot"), g1)

	e.exec(t, `insert into timeline (id, workspace_id, project_id, name, canvas_node_id) values ($1, $2, $3, 'T', 'tl1')`,
		e.id("timeline"), owner.WorkspaceID, pid)
	e.exec(t, `insert into timeline_track (id, timeline_id, kind, idx) values ($1, $2, 'video', 1)`, e.id("track"), e.id("timeline"))
	e.exec(t, `insert into timeline_clip (id, track_id, idx, media_object_id, source_in_ms, source_out_ms, start_ms, duration_ms)
		values ($1, $2, 1, $3, 0, 1000, 0, 1000)`, e.next(), e.id("track"), m4)

	e.exec(t, `insert into director_scene (id, workspace_id, project_id, canvas_node_id, scene, render_fingerprint, media_ids)
		values ($1, $2, $3, 'd1', '{"objects":[]}', 'fp', $4)`, e.id("scene"), owner.WorkspaceID, pid, []string{fmt.Sprint(m4)})
	e.exec(t, `insert into project_document (id, workspace_id, project_id, path, content, source) values ($1, $2, $3, 'notes.md', '剧本笔记', 'user')`,
		e.next(), owner.WorkspaceID, pid)
}

func readZip(t *testing.T, path string) map[string][]byte {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		out[f.Name] = b
	}
	return out
}

func writeZip(t *testing.T, entries map[string][]byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "x.zip")
	f, _ := os.Create(p)
	zw := zip.NewWriter(f)
	for n, b := range entries {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(b)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return p
}

func (e *env) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s：%v", sql, err)
	}
	return n
}

func (e *env) storeFiles(t *testing.T) int {
	t.Helper()
	n := 0
	_ = filepath.Walk(e.root, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			n++
		}
		return nil
	})
	return n
}

var anyID = regexp.MustCompile(`\d{15,20}`)

// normalize 把 JSON 里所有雪花号换成 #，再按语义比（jsonb 会重排键）。
func normalize(t *testing.T, raw []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(anyID.ReplaceAllString(string(raw), "#")), &v); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func TestIntegration_导出导入往返(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	pid := e.id("project")

	zipPath, fileName, err := e.svc.Export(ctx, owner.WorkspaceID, pid)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(zipPath)
	if fileName != "P.tabtv.zip" {
		t.Errorf("文件名：%s", fileName)
	}
	entries := readZip(t, zipPath)

	// ---- 包里没有 Key、没有厂商配置、没有上游原文
	var all bytes.Buffer
	for n, b := range entries {
		all.WriteString(n)
		all.Write(b)
	}
	for _, banned := range []string{secretKey, "api_key_cipher", "provider_config", "Authorization", "upstream said x", "up-task-1", `"upstream":"raw"`} {
		if bytes.Contains(all.Bytes(), []byte(banned)) {
			t.Errorf("包里出现了 %q", banned)
		}
	}
	var m projectpkg.Manifest
	if err := json.Unmarshal(entries["manifest.json"], &m); err != nil {
		t.Fatal(err)
	}
	if m.SchemaVersion != projectpkg.SchemaVersion || m.Media != 4 || m.Tables["generation"] != 2 || m.Tables["timeline_clip"] != 1 ||
		m.Tables["script_asset"] != 1 || m.Tables["director_scene"] != 1 || m.Tables["project_document"] != 1 {
		t.Fatalf("清单不对：%+v", m)
	}
	if len(m.Warnings) != 1 || !strings.Contains(m.Warnings[0], fmt.Sprint(e.id("m_gone"))) {
		t.Errorf("取不到文件的媒体应记进 warnings：%v", m.Warnings)
	}
	if m.Catalog.Models[fmt.Sprint(e.id("model"))] != "m-img" || m.Catalog.Operations[fmt.Sprint(e.id("op"))] != "text2img" {
		t.Errorf("模型与操作应按 code 记：%+v", m.Catalog)
	}

	// ---- 导入（原项目还在：重名追加「（导入）」）
	res, err := e.svc.Import(ctx, owner, zipPath)
	if err != nil {
		t.Fatal(err)
	}
	if res.ProjectID == pid || res.Name != "P（导入）" {
		t.Fatalf("应是新项目且改名：%+v", res)
	}
	np := res.ProjectID

	// 画布：除 ID 外语义一致（含未知字段），且不再含任何旧 ID
	var oldCanvas, newCanvas []byte
	var version int
	_ = e.pool.QueryRow(ctx, `select canvas_data::text from project where id = $1`, pid).Scan(&oldCanvas)
	_ = e.pool.QueryRow(ctx, `select canvas_data::text, version from project where id = $1`, np).Scan(&newCanvas, &version)
	if normalize(t, oldCanvas) != normalize(t, newCanvas) {
		t.Fatalf("画布除 ID 外应一致：\n%s\n%s", oldCanvas, newCanvas)
	}
	if version != 1 {
		t.Errorf("导入后 version 应从 1 起：%d", version)
	}
	for _, name := range []string{"gen1", "m_upload", "script", "timeline"} {
		if strings.Contains(string(newCanvas), fmt.Sprint(e.id(name))) {
			t.Errorf("新画布里还有旧 ID（%s）", name)
		}
	}
	if !strings.Contains(string(newCanvas), "futureField") || !strings.Contains(string(newCanvas), "futureTopLevel") {
		t.Error("未知字段丢了")
	}

	// 节点产物：result.id 指向新项目里的生成记录，产物媒体字节与原来一致
	var canvas struct {
		Nodes []struct {
			ID     string         `json:"id"`
			Params map[string]any `json:"params"`
			Result map[string]any `json:"result"`
		} `json:"nodes"`
	}
	_ = json.Unmarshal(newCanvas, &canvas)
	newGen := fmt.Sprint(canvas.Nodes[0].Result["id"])
	if !strings.HasSuffix(fmt.Sprint(canvas.Nodes[0].Result["outputUrl"]), "/generations/"+newGen+"/outputs/0") {
		t.Errorf("outputUrl 应跟着换：%v", canvas.Nodes[0].Result)
	}
	var key, status, idem string
	var modelID int64
	err = e.pool.QueryRow(ctx, `select m.object_key, g.status, g.idempotency_key, g.model_id from generation g
		join generation_output o on o.generation_id = g.id join media_object m on m.id = o.media_object_id
		where g.id = $1 and g.project_id = $2 and m.workspace_id = $3`, newGen, np, owner.WorkspaceID).Scan(&key, &status, &idem, &modelID)
	if err != nil {
		t.Fatalf("新生成记录与产物：%v", err)
	}
	if status != "success" || modelID != e.id("model") || !strings.HasPrefix(idem, "import:") {
		t.Errorf("生成记录：%s %d %s", status, modelID, idem)
	}
	rc, err := e.store.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "PNG-output-bytes" {
		t.Errorf("产物字节不一致：%q", got)
	}
	if n := e.count(t, `select count(*) from generation where project_id = $1 and status = 'failed' and error_code = 'import_incomplete'`, np); n != 1 {
		t.Errorf("进行中的任务导入后应判失败：%d", n)
	}
	if n := e.count(t, `select count(*) from generation where project_id = $1 and (provider_raw_result is not null or provider_task_id is not null or error_detail is not null)`, np); n != 0 {
		t.Errorf("上游原文不该带过来：%d", n)
	}

	// 从属表：行数一致、都挂在新项目下、归属是本机主体
	for _, q := range []string{
		`select count(*) from script_instance where project_id = $1`,
		`select count(*) from shot s join script_instance si on si.id = s.script_id where si.project_id = $1`,
		`select count(*) from shot_asset_ref r join shot s on s.id = r.shot_id join script_instance si on si.id = s.script_id
		   join script_asset a on a.id = r.script_asset_id join media_object m on m.id = a.media_object_id where si.project_id = $1`,
		`select count(*) from group_item gi join group_node gn on gn.id = gi.group_node_id where gn.project_id = $1 and gi.status = 'failed'`,
		`select count(*) from timeline_clip c join timeline_track tr on tr.id = c.track_id join timeline tl on tl.id = tr.timeline_id
		   join media_object m on m.id = c.media_object_id where tl.project_id = $1`,
		`select count(*) from director_scene d where d.project_id = $1 and exists (select 1 from media_object m where m.id::text = any(d.media_ids))`,
		`select count(*) from project_document where project_id = $1 and content = '剧本笔记'`,
		`select count(*) from upload_reference u join media_object m on m.id = u.media_object_id where u.project_id = $1`,
	} {
		if n := e.count(t, q, np); n != 1 {
			t.Errorf("应恰好 1 行（%d）：%s", n, q)
		}
	}
	// 脚本节点参数里的 scriptId 指向新脚本
	if sid := fmt.Sprint(canvas.Nodes[1].Params["scriptId"]); e.count(t, `select count(*) from script_instance where id::text = $1 and project_id = $2`, sid, np) != 1 {
		t.Errorf("scriptId 没有重绑：%s", sid)
	}

	// ---- 同一个包再导一次：第三个独立项目
	res2, err := e.svc.Import(ctx, owner, zipPath)
	if err != nil {
		t.Fatal(err)
	}
	if res2.ProjectID == np || res2.Name != "P（导入 2）" {
		t.Fatalf("第二次导入：%+v", res2)
	}
	if n := e.count(t, `select count(*) from project where status = 'active'`); n != 3 {
		t.Errorf("应有 3 个项目：%d", n)
	}
	// 原项目一字未动
	var after []byte
	_ = e.pool.QueryRow(ctx, `select canvas_data::text from project where id = $1`, pid).Scan(&after)
	if string(after) != string(oldCanvas) {
		t.Error("导入改动了原项目")
	}
}

func TestIntegration_恶意与不兼容的包被拒且无残留(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	zipPath, _, err := e.svc.Export(ctx, owner.WorkspaceID, e.id("project"))
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(zipPath)
	good := readZip(t, zipPath)
	projects := e.count(t, `select count(*) from project`)
	medias := e.count(t, `select count(*) from media_object`)
	files := e.storeFiles(t)

	variant := func(mut func(map[string][]byte)) string {
		cp := map[string][]byte{}
		for k, v := range good {
			cp[k] = v
		}
		mut(cp)
		return writeZip(t, cp)
	}
	notZip := filepath.Join(t.TempDir(), "x.zip")
	_ = os.WriteFile(notZip, []byte("not a zip"), 0o600)

	cases := map[string]string{
		"zip slip": variant(func(m map[string][]byte) { m["../../evil.txt"] = []byte("x") }),
		"绝对路径":     variant(func(m map[string][]byte) { m["/etc/x"] = []byte("x") }),
		"未知大版本": variant(func(m map[string][]byte) {
			m["manifest.json"] = bytes.Replace(m["manifest.json"], []byte(`"schemaVersion": 1`), []byte(`"schemaVersion": 99`), 1)
		}),
		"缺清单":    variant(func(m map[string][]byte) { delete(m, "manifest.json") }),
		"不是 zip": notZip,
		// 中途失败：镜头号为空违反 not null —— 媒体已经写进存储，事务回滚后要删掉。
		"中途失败回滚": variant(func(m map[string][]byte) {
			m["data/shot.json"] = []byte(`[{"id": 123456789012345678, "script_id": 223456789012345678, "shot_no": null}]`)
		}),
	}
	for name, p := range cases {
		_, err := e.svc.Import(ctx, owner, p)
		if err == nil {
			t.Errorf("%s：应被拒绝", name)
			continue
		}
		if name != "中途失败回滚" && !projectpkg.IsBadPackage(err) {
			t.Errorf("%s：应是包本身的问题（给出原因）：%v", name, err)
		}
		if name == "未知大版本" && !strings.Contains(err.Error(), "升级") {
			t.Errorf("未知大版本应说明原因：%v", err)
		}
	}
	if n := e.count(t, `select count(*) from project`); n != projects {
		t.Errorf("被拒的导入留下了项目：%d → %d", projects, n)
	}
	if n := e.count(t, `select count(*) from media_object`); n != medias {
		t.Errorf("被拒的导入留下了媒体行：%d → %d", medias, n)
	}
	if n := e.storeFiles(t); n != files {
		t.Errorf("被拒的导入在存储里留下了文件：%d → %d", files, n)
	}
}
