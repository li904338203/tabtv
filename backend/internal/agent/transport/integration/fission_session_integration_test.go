//go:build integration

// 素材裂变的源视频槽（MKT-T-28、MKT-AC-20）：建营销会话时 source_video 按工作区校验归属、只收视频、
// 只收 1 个；被拒时点名槽位（details.slot，前端据此标红），且账本无新行、不建项目 / 会话。
package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
)

// fissionEnv 在营销环境上加一个裂变技能（源视频必填）与几条不同来源的媒体。
type fissionEnv struct {
	*mktEnv
	genVideo   int64 // 本工作区的生成产物视频
	otherVideo int64 // 别的工作区的视频
}

func newFissionEnv(t *testing.T) *fissionEnv {
	t.Helper()
	m := newMktEnv(t, true)
	ctx := context.Background()
	if _, err := repo.New(m.pool).InsertSkill(ctx, repo.Skill{ID: m.next(), Code: "mk_remix", Name: "素材裂变",
		IsEnabled: true, Scenes: json.RawMessage(`["marketing"]`),
		InputSlots: json.RawMessage(`[{"key":"source_video","label":"源视频","accept":["video"],"required":true,"maxCount":1}]`),
	}); err != nil {
		t.Fatal(err)
	}
	media := func(ws int64, purpose string) int64 {
		id := m.next()
		if _, err := m.pool.Exec(ctx, `
			insert into media_object (id, workspace_id, bucket, object_key, content_type, size_bytes,
			                          purpose, created_by_user_id)
			values ($1,$2,'seeaitv',$3,'video/mp4',1,$4,$5)`,
			id, ws, "t/"+strconv.FormatInt(id, 10), purpose, m.owner); err != nil {
			t.Fatal(err)
		}
		return id
	}
	return &fissionEnv{mktEnv: m, genVideo: media(m.ws, "generation_output"), otherVideo: media(m.next(), "generation_output")}
}

// fissionBody 是裂变技能的建会话请求：只有源视频槽（技能声明里没有商品槽）。
func (f *fissionEnv) fissionBody(refs ...any) map[string]any {
	b := f.body()
	b["skillCode"] = "mk_remix"
	b["message"].(map[string]any)["refs"] = refs
	delete(b["marketing"].(map[string]any), "product")
	return b
}

func (f *fissionEnv) noSideEffects(t *testing.T, projects int) {
	t.Helper()
	if n := f.count(`select count(*) from credit_transaction`); n != 0 {
		t.Fatalf("被拒不得扣费：账本 %d 行", n)
	}
	if n := f.count(`select count(*) from agent_session`); n != 0 {
		t.Fatalf("被拒不得建会话：%d", n)
	}
	if n := f.count(`select count(*) from project`); n != projects {
		t.Fatalf("被拒不得建项目：%d → %d", projects, n)
	}
}

func TestFission_本工作区视频可作源(t *testing.T) {
	f := newFissionEnv(t)
	ctx := context.Background()
	// 上传的视频与本工作区的生成产物都能作源（生成产物以它归档后的媒体 ID 引用）。
	for i, id := range []int64{f.video, f.genVideo} {
		r := f.do("POST", "/agent/sessions", f.owner, "k-fission-ok-"+strconv.Itoa(i),
			f.fissionBody(ref("media", id, "source_video", 0)))
		if r.status != http.StatusCreated {
			t.Fatalf("本工作区视频 %d 应可作源：%d %v", id, r.status, r.body)
		}
		var sv agentapp.SceneRef
		if err := f.pool.QueryRow(ctx, `select scene_input->'sourceVideo' from agent_session
			where scene = 'marketing' order by created_at desc limit 1`).Scan(&sv); err != nil ||
			sv != (agentapp.SceneRef{Kind: "media", ID: idS(id)}) {
			t.Fatalf("sourceVideo 应落库为 media:%d：%+v %v", id, sv, err)
		}
	}
	if n := f.count(`select count(*) from credit_transaction`); n != 0 {
		t.Fatalf("建会话本身不扣费（第一轮在 Worker 里扣）：%d", n)
	}
}

func TestFission_他人视频被拒(t *testing.T) {
	f := newFissionEnv(t)
	projects := f.count(`select count(*) from project`)
	r := f.do("POST", "/agent/sessions", f.owner, "k-fission-other",
		f.fissionBody(ref("media", f.otherVideo, "source_video", 0)))
	if r.status != http.StatusBadRequest || r.code() != int(errcode.InvalidArgument) ||
		detail(r, "slot") != "source_video" {
		t.Fatalf("他人工作区的视频应 400/40001 并点名 source_video：%d %v", r.status, r.body)
	}
	// 新项目的画布是空的：节点引用同样被拒并点名槽位。
	r = f.do("POST", "/agent/sessions", f.owner, "k-fission-node",
		f.fissionBody(map[string]any{"kind": "node", "id": "n_video", "slot": "source_video"}))
	if r.status != http.StatusBadRequest || detail(r, "slot") != "source_video" {
		t.Fatalf("节点引用应被拒并点名槽位：%d %v", r.status, r.body)
	}
	f.noSideEffects(t, projects)
}

func TestFission_非视频被拒(t *testing.T) {
	f := newFissionEnv(t)
	projects := f.count(`select count(*) from project`)
	for i, c := range []struct {
		name string
		refs []any
	}{
		{"源视频槽给了图片", []any{ref("media", f.products[0], "source_video", 0)}},
		{"源视频槽给了角色资产", []any{ref("asset", f.char, "source_video", 0)}},
		{"两条源视频", []any{ref("media", f.video, "source_video", 1), ref("media", f.genVideo, "source_video", 2)}},
	} {
		r := f.do("POST", "/agent/sessions", f.owner, "k-fission-bad-"+strconv.Itoa(i), f.fissionBody(c.refs...))
		if r.status != http.StatusBadRequest || r.code() != int(errcode.InvalidArgument) ||
			detail(r, "slot") != "source_video" {
			t.Errorf("%s 应 400/40001 并点名 source_video：%d %v", c.name, r.status, r.body)
		}
	}
	f.noSideEffects(t, projects)
}
