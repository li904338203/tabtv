//go:build integration

// 智能剪辑之后的删除保护口径（docs/agent/smartcut SCT-10，SCD-36、SCK-18）：
// 节点在画布上 → 保护；节点删了（孤儿）→ 不保护；只在快照里 → 不保护；只在备选里且节点在 → 保护；
// 旧时间线（没绑节点）→ 保护；项目已不存在 → 不保护。
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

func TestIntegration_时间线引用检查的五种情形(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s：%v", sql, err)
		}
	}
	// project 建项目（画布上放这些节点），返回 ID。
	project := func(nodes ...string) int64 {
		list := []map[string]any{}
		for _, n := range nodes {
			list = append(list, map[string]any{"id": n, "type": "smartcut"})
		}
		raw, _ := json.Marshal(map[string]any{"nodes": list})
		pid := next()
		exec(`insert into project (id, workspace_id, created_by_user_id, name, canvas_data) values ($1,$2,1,'P',$3::jsonb)`,
			pid, ws, string(raw))
		return pid
	}
	// timeline 建一条时间线（node 为空 = 旧时间线），在画面轨上放一段 media，备选里放 alt。
	timeline := func(pid int64, node string, media, alt int64) int64 {
		tid, track := next(), next()
		var nodeArg any
		if node != "" {
			nodeArg = node
		}
		exec(`insert into timeline (id, workspace_id, project_id, name, width, height, fps, version, canvas_node_id)
			values ($1,$2,$3,'T',1080,1920,30,1,$4)`, tid, ws, pid, nodeArg)
		exec(`insert into timeline_track (id, timeline_id, kind, idx) values ($1,$2,'video',1)`, track, tid)
		alts := "[]"
		if alt != 0 {
			alts = fmt.Sprintf(`[{"mediaId":"%d","sourceInMs":0,"sourceOutMs":1000,"kind":"original"}]`, alt)
		}
		exec(`insert into timeline_clip (id, track_id, idx, media_object_id, source_in_ms, source_out_ms, start_ms, duration_ms, alternates)
			values ($1,$2,1,$3,0,1000,0,1000,$4::jsonb)`, next(), track, media, alts)
		return tid
	}
	refs := func(media int64) int {
		t.Helper()
		got, err := r.MediaReferences(ctx, media)
		if err != nil {
			t.Fatal(err)
		}
		return len(got)
	}

	// ① 节点在画布上：片段的素材与备选都保护。
	alive := project("s1")
	timeline(alive, "s1", 101, 102)
	if refs(101) != 1 || refs(102) != 1 {
		t.Fatalf("节点在画布上：片段与备选都应保护（%d / %d）", refs(101), refs(102))
	}
	// ② 节点删了：孤儿时间线不保护。
	orphan := project()
	timeline(orphan, "gone", 201, 202)
	if refs(201) != 0 || refs(202) != 0 {
		t.Fatalf("节点被删了：孤儿时间线不该挡住删除（%d / %d）", refs(201), refs(202))
	}
	// ③ 只在快照里：不保护。
	tid := timeline(project("s3"), "s3", 301, 0)
	exec(`insert into timeline_snapshot (id, timeline_id, version, content, reason)
		values ($1,$2,1,'{"tracks":[{"clips":[{"mediaId":"302"}]}]}'::jsonb,'ai_plan')`, next(), tid)
	if refs(302) != 0 {
		t.Fatal("只在快照里出现的素材不该被保护")
	}
	// ④ 旧时间线（脚本装配、Agent 拼接，没绑节点）：照常保护。
	timeline(project(), "", 401, 0)
	if refs(401) != 1 {
		t.Fatal("没绑节点的旧时间线照常保护素材")
	}
	// ⑤ 项目已不存在（清理修复之前留下的）：不保护；回收站里的项目照常保护。
	ghost := project("s5")
	timeline(ghost, "s5", 501, 0)
	exec(`delete from project where id = $1`, ghost)
	trashed := project("s6")
	timeline(trashed, "s6", 601, 0)
	exec(`update project set status = 'deleted', deleted_at = now() where id = $1`, trashed)
	if refs(501) != 0 || refs(601) != 1 {
		t.Fatalf("项目没了不保护、回收站里的照常保护（%d / %d）", refs(501), refs(601))
	}
}
