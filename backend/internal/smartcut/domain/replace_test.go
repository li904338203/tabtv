package domain

import (
	"strings"
	"testing"
)

func readyMedia(no, node, media string, ms int) Input {
	d := ms
	return Input{No: no, SourceNodeID: node, Kind: InputVideo, Status: InputReady, MediaID: media, DurationMs: &d}
}

func TestMarkChanged_上游重新生成后新产物标changed(t *testing.T) {
	items := []Input{readyMedia("1", "a", "new", 3000), readyMedia("2", "b", "kept", 4000)}
	clips := []UsedClip{{SourceNodeID: "a", MediaID: "old"}, {SourceNodeID: "b", MediaID: "kept"}}
	// 走 ResolveInputs 同一个入口（Used 由它算）：这里直接构造已解析的素材条，只验 changed 的判定。
	for i := range items {
		for _, c := range clips {
			if c.SourceNodeID == items[i].SourceNodeID && c.MediaID == items[i].MediaID {
				items[i].Used = true
			}
		}
	}
	markChanged(items, clips)
	if !items[0].Changed || items[1].Changed {
		t.Fatalf("a 的片段还用着旧产物 → 新产物 changed；b 没变：%+v", items)
	}
	// 只是多了一个没用上的产物（多产物节点）：片段用的仍是现在的产物，不算变了。
	items = []Input{readyMedia("3-1", "c", "c1", 3000), readyMedia("3-2", "c", "c2", 3000)}
	items[0].Used = true
	markChanged(items, []UsedClip{{SourceNodeID: "c", MediaID: "c1"}})
	if items[1].Changed {
		t.Fatal("多产物节点里没用上的那个不算变了")
	}
}

func TestPlanReplaceChanged(t *testing.T) {
	items := []Input{
		readyMedia("1", "a", "a-new", 3000),
		readyMedia("2-1", "b", "b1", 2000), readyMedia("2-2", "b", "b2", 2500),
		readyMedia("3", "c", "c", 4000),
	}
	items[3].Used = true
	clips := []ClipUse{
		{ID: "11", SourceNodeID: "a", MediaID: "a-old"},
		{ID: "12", SourceNodeID: "b", MediaID: "b-old1"},
		{ID: "13", SourceNodeID: "c", MediaID: "c"},
		{ID: "14", SourceNodeID: "b", MediaID: "b-old2"},
		{ID: "15", SourceNodeID: "gone", MediaID: "x"}, // 来源已断开：不动
	}
	got := PlanReplaceChanged(items, clips)
	var parts []string
	for _, r := range got {
		parts = append(parts, r.ClipID+":"+r.OldMediaID+"→"+r.NewMediaID)
	}
	if strings.Join(parts, ",") != "11:a-old→a-new,12:b-old1→b1,14:b-old2→b2" {
		t.Fatalf("同一来源节点按顺序换成新产物：%v", parts)
	}
	if got[0].NewDurationMs != 3000 {
		t.Fatalf("带上新产物的时长（截短用）：%+v", got[0])
	}
	if len(PlanReplaceChanged(items[3:], clips[2:3])) != 0 {
		t.Fatal("没有变化时是空计划")
	}
}

func TestInputsStale(t *testing.T) {
	cases := []struct {
		planned, synced string
		want            bool
	}{{"", "x", false}, {"x", "", false}, {"x", "x", false}, {"x", "y", true}}
	for _, c := range cases {
		if InputsStale(c.planned, c.synced) != c.want {
			t.Errorf("%+v", c)
		}
	}
}
