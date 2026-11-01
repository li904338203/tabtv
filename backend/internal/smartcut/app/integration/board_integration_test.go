//go:build integration

// 素材混剪与分镜成片（docs/agent/smartcut SCT-28）：SC-AC-38~41，模型回复用测试替身固定。
package integration

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	mediarepo "github.com/li904338203/tabtv/backend/internal/media/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/config"
	scriptapp "github.com/li904338203/tabtv/backend/internal/script/app"
	scriptrepo "github.com/li904338203/tabtv/backend/internal/script/repo"
	smartcutdomain "github.com/li904338203/tabtv/backend/internal/smartcut/domain"
	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

// scriptWithShots 建一个脚本（画布节点 nodeID），镜头按 dialogues 给台词；视频组里 ok[i] 为真的镜头有一条成功的视频（时长 ms），
// 否则是一条失败的子项。返回脚本 ID 与视频组 ID。
func (e planEnv) scriptWithShots(t *testing.T, nodeID string, ms int, ok []bool, dialogues []string) (int64, int64) {
	t.Helper()
	sid := e.next()
	e.exec(t, `insert into script_instance (id, workspace_id, project_id, canvas_node_id, source_type) values ($1,$2,$3,$4,'manual')`,
		sid, ws, e.pid, nodeID)
	gid := e.next()
	e.exec(t, `insert into group_node (id, workspace_id, project_id, canvas_node_id, group_type, script_id)
		values ($1,$2,$3,$4,'video_generator',$5)`, gid, ws, e.pid, "g-"+nodeID, sid)
	for i, good := range ok {
		shot := e.next()
		e.exec(t, `insert into shot (id, script_id, shot_no, duration_sec, description, dialogue) values ($1,$2,$3,5,$4,$5)`,
			shot, sid, i+1, fmt.Sprintf("第 %d 镜的画面", i+1), dialogues[i])
		if good {
			m := e.mediaOf(t, ws, "video/mp4", ms)
			gen := e.generation(t, ws, e.pid, "success", 9302, m)
			e.exec(t, `insert into group_item (id, group_node_id, item_no, shot_id, generation_id, status) values ($1,$2,$3,$4,$5,'success')`,
				e.next(), gid, i+1, shot, gen)
		} else {
			e.exec(t, `insert into group_item (id, group_node_id, item_no, shot_id, status, error_message)
				values ($1,$2,$3,$4,'failed','上游超时')`, e.next(), gid, i+1, shot)
		}
	}
	return sid, gid
}

func setupBoard(t *testing.T) planEnv {
	t.Helper()
	e := setupPlan(t, config.Smartcut{})
	media := mediaapp.New(mediarepo.New(e.pool), nil, e.next)
	scripts := scriptapp.New(scriptrepo.New(e.pool), e.next)
	e.ports.Board = wiring.NewSmartcutBoard(scripts, media)
	e.svc.SetPlanPorts(e.ports)
	return e
}

// SC-AC-41：连一个 4 镜的脚本（第 3 镜失败）：按镜号装配 3 段，缺口报「第 3 镜：生成失败」；台词字幕按镜头时长排好并跟随片段；
// 两个脚本按连线顺序拼接；视频组节点只取这个组的子项。
func TestPlan_分镜成片(t *testing.T) {
	e := setupBoard(t)
	sid, _ := e.scriptWithShots(t, "s1", 3000, []bool{true, true, false, true}, []string{"你好。", "今天下雨了。", "没拍成", "走吧"})
	sid2, gid2 := e.scriptWithShots(t, "s2", 2000, []bool{true}, []string{"第二个脚本"})
	// 同一个脚本的另一个视频组，产出更新、更长：画布上连的是 g2，不能按镜号把它挑走（CLAUDE.md §5.10）。
	other := e.next()
	e.exec(t, `insert into group_node (id, workspace_id, project_id, canvas_node_id, group_type, script_id)
		values ($1,$2,$3,'g-other','video_generator',$4)`, other, ws, e.pid, sid2)
	var shot2 int64
	_ = e.pool.QueryRow(e.ctx, `select id from shot where script_id = $1`, sid2).Scan(&shot2)
	otherMedia := e.mediaOf(t, ws, "video/mp4", 9000)
	otherGen := e.generation(t, ws, e.pid, "success", 9302, otherMedia)
	e.exec(t, `insert into group_item (id, group_node_id, item_no, shot_id, generation_id, status) values ($1,$2,1,$3,$4,'success')`,
		e.next(), other, shot2, otherGen)
	e.setCanvas(t, e.pid, []map[string]any{
		nodeWith("sc", "smartcut", nil),
		{"id": "s1", "type": "script", "title": "西游", "params": map[string]any{"scriptId": fmt.Sprint(sid)}},
		{"id": "g2", "type": "group", "title": "视频组二", "params": map[string]any{"groupType": "video_generator",
			"scriptId": fmt.Sprint(sid2), "groupId": fmt.Sprint(gid2)}},
	}, []map[string]any{edgeTo("s1", 1), edgeTo("g2", 2)})
	e.text.replies = []string{`{"segments":[{"clip":"b2","transition":"dissolve"},{"clip":"b3","transition":"fade","subtitle":""}],
		"title":{"text":"雨夜"}}`}
	r := e.req(smartcutdomain.ModeBoard)
	r.BaseVersion = e.currentVersion(t)
	res, err := e.svc.Plan(e.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	v := res.Variants[0]
	if v.Status != "success" || len(v.UsedClips) != 4 {
		t.Fatalf("3 + 1 个镜头：%+v", v)
	}
	gap := false
	for _, w := range v.Warnings {
		gap = gap || strings.HasPrefix(w, "第 3 镜：生成失败") && strings.Contains(w, "西游")
	}
	if !gap {
		t.Fatalf("缺口逐条报镜号与原因：%v", v.Warnings)
	}
	vt, err := e.tl.Get(e.ctx, ws, e.tid)
	if err != nil {
		t.Fatal(err)
	}
	c := timelineapp.ContentFromView(vt)
	lay := domain.Layout(c)
	var clips []domain.ContentClip
	for _, tr := range c.Tracks {
		if tr.Kind == domain.TrackVideo {
			clips = tr.Clips
		}
	}
	if len(clips) != 4 || clips[1].TransitionIn == nil || clips[1].TransitionIn.Kind != "dissolve" || clips[3].SourceOutMs != 2000 ||
		clips[3].MediaID == otherMedia {
		t.Fatalf("按镜号、按连线顺序拼接；转场来自方案：%+v", clips)
	}
	var subs []string
	for _, s := range c.Subtitles {
		var a struct {
			ClipID string `json:"clipId"`
		}
		_ = json.Unmarshal(s.Anchor, &a)
		if _, ok := lay.Clips[a.ClipID]; !ok {
			t.Fatalf("字幕跟随片段：%s", s.Anchor)
		}
		subs = append(subs, s.Text)
	}
	if strings.Join(subs, "|") != "你好。|今天下雨了。|第二个脚本" {
		// 第 3 镜（b3 = 原第 4 镜「走吧」）写了空字幕 → 不要台词字幕
		t.Fatalf("台词字幕：%v", subs)
	}
	title := false
	for _, tx := range c.Texts {
		title = title || tx.Text == "雨夜"
	}
	if !title {
		t.Fatal("开头标题")
	}
	if !strings.Contains(e.text.requests()[0].User, "b1：第 1 镜（西游），3 秒。画面：第 1 镜的画面。台词：你好。") {
		t.Fatalf("镜头清单交给模型：%s", e.text.requests()[0].User)
	}
}

// SC-AC-38：7 段各约 2 秒、目标 20 秒：成片约 14 秒，写明素材不够，不放慢不重复；带 allowSlow 之后接近 20 秒且不慢于 0.75×。
func TestPlan_素材不够(t *testing.T) {
	e := setupPlan(t, config.Smartcut{})
	nodes := []map[string]any{nodeWith("sc", "smartcut", nil)}
	var edges []map[string]any
	var segs []string
	for i := 1; i <= 7; i++ {
		m := e.mediaOf(t, ws, "video/mp4", 2040)
		id := fmt.Sprintf("v%d", i)
		nodes = append(nodes, nodeWith(id, "video", mediaResult(m)))
		edges = append(edges, edgeTo(id, i))
		segs = append(segs, fmt.Sprintf(`{"clip":"%d","take":"full","role":"body","weight":1,"transition":"cut"}`, i))
	}
	e.setCanvas(t, e.pid, nodes, edges)
	e.text.replies = []string{`{"segments":[` + strings.Join(segs, ",") + `]}`}
	r := e.req(smartcutdomain.ModeMix)
	r.TargetSec, r.BaseVersion = 20, e.currentVersion(t)
	res, err := e.svc.Plan(e.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	v := res.Variants[0]
	if v.TotalMs != 14280 || v.Shortage == nil || v.Shortage.TargetMs != 20000 || !hasPrefix(v.Warnings, "素材共 14.3 秒，不足目标 20 秒") {
		t.Fatalf("如实说明：%+v", v)
	}
	r.AllowSlow, r.BaseVersion = true, e.currentVersion(t)
	res, err = e.svc.Plan(e.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	v = res.Variants[0]
	if v.TotalMs < 18500 {
		t.Fatalf("允许放慢后接近 20 秒：%d", v.TotalMs)
	}
	vt, _ := e.tl.Get(e.ctx, ws, e.tid)
	for _, tr := range vt.Tracks {
		for _, c := range tr.Clips {
			if tr.Track.Kind == domain.TrackVideo && c.Speed < smartcutdomain.MinSlowSpeed {
				t.Fatalf("没有片段慢于 0.75×：%+v", c)
			}
		}
	}
}

func hasPrefix(list []string, p string) bool {
	for _, s := range list {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// SC-AC-39：120 BPM 的配乐：各片段切换点与最近节拍的偏差不超过一帧。
func TestPlan_卡点(t *testing.T) {
	e := setupPlan(t, config.Smartcut{})
	var beats []int
	for ms := 0; ms <= 20000; ms += 500 {
		beats = append(beats, ms)
	}
	raw, _ := json.Marshal(map[string]any{"bpm": 120, "beatsMs": beats, "downbeatsMs": []int{0, 2000, 4000}, "downbeatApprox": true})
	// 同步时已经给配乐排了节拍分析（排队中）：把它直接写成「分析完成」。
	e.exec(t, `update media_analysis set status = 'success', result = $2::jsonb where media_object_id = $1 and kind = 'beats'`,
		e.music, string(raw))
	r := e.req(smartcutdomain.ModeMix)
	// 三段可用 5 + 1.6 + 2.4 秒、两处转场各重叠 0.5 秒：目标 7 秒够得着（9 秒就是素材不够，另有测试）。
	r.Instruction, r.TargetSec = "卡点剪一版", 7
	res, err := e.svc.Plan(e.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.text.requests()[0].User, "约 120 BPM，切换点会自动对齐到节拍上") {
		t.Fatalf("节拍摘要交给模型：%s", e.text.requests()[0].User)
	}
	vt, _ := e.tl.Get(e.ctx, ws, e.tid)
	c := timelineapp.ContentFromView(vt)
	lay := domain.Layout(c)
	for _, tr := range c.Tracks {
		if tr.Kind != domain.TrackVideo {
			continue
		}
		for k, cl := range tr.Clips[1:] {
			sw := lay.Clips[cl.ID].StartMs
			if cl.TransitionIn != nil {
				sw += cl.TransitionIn.DurationMs / 2
			}
			if d := min(sw%500, 500-sw%500); d > 33 {
				t.Fatalf("第 %d 处切换点 %d 离节拍 %dms", k+1, sw, d)
			}
		}
	}
	if res.Variants[0].Shortage != nil {
		t.Fatalf("素材够：%+v", res.Variants[0])
	}
	if d := res.Variants[0].TotalMs - 7000; d > 500 || d < -500 {
		t.Fatalf("总长与目标差不超过一拍：%d", res.Variants[0].TotalMs)
	}
}

// SC-AC-40：素材描述里只有编号 3 含「产品」：方案把 3 放在开头，分配时长不少于其余段的平均值。
func TestPlan_突出产品(t *testing.T) {
	e := setupPlan(t, config.Smartcut{})
	nodes := []map[string]any{nodeWith("sc", "smartcut", nil)}
	var edges []map[string]any
	var three int64
	for i := 1; i <= 5; i++ {
		m := e.mediaOf(t, ws, "video/mp4", 6000)
		gen := e.generation(t, ws, e.pid, "success", 9302, m)
		prompt := "街头人群"
		if i == 3 {
			prompt, three = "产品特写，白色桌面", m
		}
		e.exec(t, `update generation set params_snapshot = jsonb_build_object('prompt', $2::text) where id = $1`, gen, prompt)
		id := fmt.Sprintf("v%d", i)
		nodes = append(nodes, nodeWith(id, "video", genResult(gen)))
		edges = append(edges, edgeTo(id, i))
	}
	e.setCanvas(t, e.pid, nodes, edges)
	e.text.replies = []string{`{"segments":[
		{"clip":"1","take":"full","role":"body","weight":1,"transition":"cut"},
		{"clip":"3","take":"full","role":"hook","weight":3,"transition":"cut","reason":"产品特写"},
		{"clip":"2","take":"full","role":"body","weight":1,"transition":"cut"},
		{"clip":"5","take":"full","role":"end","weight":1,"transition":"cut"}]}`}
	r := e.req(smartcutdomain.ModeMix)
	r.TargetSec, r.BaseVersion = 15, e.currentVersion(t)
	if _, err := e.svc.Plan(e.ctx, r); err != nil {
		t.Fatal(err)
	}
	user := e.text.requests()[0].User
	if !strings.Contains(user, "编号 3：视频，6 秒。标题：节点 v3。描述：产品特写，白色桌面") || strings.Count(user, "产品") != 1+strings.Count(r.Instruction, "产品") {
		t.Fatalf("只有编号 3 的描述含产品（来自生成提示词）：%s", user)
	}
	vt, _ := e.tl.Get(e.ctx, ws, e.tid)
	var clips []domain.Clip
	for _, tr := range vt.Tracks {
		if tr.Track.Kind == domain.TrackVideo {
			clips = tr.Clips
		}
	}
	if len(clips) != 4 || clips[0].MediaID != three {
		t.Fatalf("产品镜头在开头：%+v", clips)
	}
	others := 0
	for _, c := range clips[1:] {
		others += c.PlayLen()
	}
	if clips[0].PlayLen()*3 < others {
		t.Fatalf("分配时长不少于其余段的平均值：%d vs %d", clips[0].PlayLen(), others/3)
	}
}
