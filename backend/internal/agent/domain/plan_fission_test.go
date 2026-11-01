package domain

import (
	"reflect"
	"strings"
	"testing"
)

func TestVideo2VideoItem_只收一个视频(t *testing.T) {
	limit := 3
	it := PlanItem{Kind: "video", Key: "v1", ModelCode: "m", OperationCode: "ref2video", ProductIndexes: []int{1},
		ProductMediaIDs: []string{"101"}, ProductTotal: 3, ImageLimit: &limit, Inputs: map[string]int{"image": 2}}
	got := Video2VideoItem(it, "777")
	if got.OperationCode != OpVideo2Video || !reflect.DeepEqual(got.Inputs, map[string]int{"video": 1}) ||
		got.ProductIndexes != nil || got.ProductMediaIDs != nil || got.ProductTotal != 0 || got.ImageLimit != nil ||
		got.SourceVideoID != "777" || got.FissionRoute != FissionVideo2Video {
		t.Fatalf("video2video 形态不对：%+v", got)
	}
	if it.OperationCode != "ref2video" || len(it.ProductMediaIDs) != 1 {
		t.Fatal("不应改入参")
	}
}

func TestRegenerateItem_不发源视频(t *testing.T) {
	r := RegenerateItem(PlanItem{Kind: "video", OperationCode: OpVideo2Video}, "777", "  原因  ")
	if r.OperationCode != "" || r.FissionRoute != FissionRegenerate || r.FissionReason != "原因" || r.SourceVideoID != "777" {
		t.Fatalf("模型写的 video2video 退回默认操作：%+v", r)
	}
	if r := RegenerateItem(PlanItem{Kind: "video", OperationCode: "ref2video"}, "777", ""); r.OperationCode != "ref2video" {
		t.Fatalf("其余操作保持：%+v", r)
	}
	// 换图重算输入：重新生成的条目照样只有商品图 + 角色，没有 video。
	in := MarketingInputs{Products: []string{"101", "102"}, Characters: 1, SourceVideo: true}
	it, err := ApplyProductSelection(r, []int{2}, in)
	if err != nil || !reflect.DeepEqual(it.Inputs, map[string]int{"image": 2}) {
		t.Fatalf("重新生成不计源视频：%v %v", it.Inputs, err)
	}
	if it, _ := ApplyProductSelection(PlanItem{Kind: "video"}, []int{1}, in); it.Inputs["video"] != 0 {
		t.Fatalf("会话有源视频不等于每一项都带它：%v", it.Inputs)
	}
}

func TestFissionNote_写明路径与费用(t *testing.T) {
	v := Video2VideoItem(PlanItem{Kind: "video", Credits: 50}, "777")
	if n := FissionNote(v); !strings.Contains(n, "视频生视频") || !strings.Contains(n, "✦50") {
		t.Fatalf("video2video 说明：%q", n)
	}
	r := RegenerateItem(PlanItem{Kind: "video", Credits: 40}, "777", "所选模型不支持以视频为输入（视频生视频）。")
	n := FissionNote(r)
	if !strings.Contains(n, "重新生成") || !strings.Contains(n, "不支持以视频为输入") || !strings.Contains(n, "✦40") ||
		strings.Contains(n, "。，") {
		t.Fatalf("重新生成说明：%q", n)
	}
	r.BlockedCode = "MODEL_UNAVAILABLE"
	if n := FissionNote(r); !strings.Contains(n, "暂不可用") || strings.Contains(n, "✦") {
		t.Fatalf("不可用时不报金额：%q", n)
	}
	if FissionNote(PlanItem{Kind: "video", Credits: 9}) != "" {
		t.Fatal("非裂变条目没有说明")
	}
	// 重新报价后说明跟着金额走。
	v.Credits = 70
	out := WithFissionNotes([]PlanItem{v, {Kind: "image"}})
	if !strings.Contains(out[0].FissionNote, "✦70") || out[1].FissionNote != "" {
		t.Fatalf("重写说明：%+v", out)
	}
}
