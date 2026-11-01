package domain

import "testing"

func TestInferPlanItemKey(t *testing.T) {
	items := []PlanItem{
		{Key: "item1", Kind: "video", ModelCode: "seedance_2_5", OperationCode: "ref2video"},
		{Key: "a1", Kind: "audio", ModelCode: "tts", OperationCode: "tts"},
		{Key: "r1", Kind: "render"}, // 不归 generate
	}
	keys := func(in []PlanItem) []string {
		out := []string{}
		for _, it := range in {
			out = append(out, it.Key)
		}
		return out
	}
	cases := []struct {
		name                string
		steps               []PlanStep
		nodeType, model, op string
		want                string
		cands               int
	}{
		// 会话 107492880150429696：模型照通用会话的写法给了类型 / 模型 / 操作，唯一对得上 item1。
		{"按类型模型操作唯一对上", nil, "video", "seedance_2_5", "ref2video", "item1", 1},
		{"只给了类型也唯一", nil, "video", "", "", "item1", 1},
		{"什么都没给：视频与配音两项都是候选", nil, "", "", "", "", 2},
		{"操作对不上：没有候选", nil, "video", "", "img2video", "", 0},
		{"已在跑的不算候选（不把同一项再生成一遍）", []PlanStep{{Key: "item1", Status: "waiting"}}, "video", "", "", "", 0},
		{"已成功的不算候选", []PlanStep{{Key: "item1", Status: "success"}}, "", "", "", "a1", 1},
		{"失败过的仍是候选（重试是正当的）",
			[]PlanStep{{Key: "item1", Status: "success"}, {Key: "item1", Status: "failed"}}, "video", "", "", "item1", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, cands := InferPlanItemKey(items, c.steps, c.nodeType, c.model, c.op)
			if got != c.want || len(cands) != c.cands {
				t.Fatalf("got %q 候选 %v，期望 %q / %d 个", got, keys(cands), c.want, c.cands)
			}
			for _, it := range cands {
				if it.Key == "r1" {
					t.Fatal("渲染条目不归 generate，不该是候选")
				}
			}
		})
	}
}
