package app

import (
	"encoding/json"
	"testing"
)

// 受理时 Text 要进参数的 prompt（CD-81）。
//
// ⚠️ 起因：Worker 只从 `params.prompt` 拼提示词，而 Text 此前用完就丢 —— 组子项只传了 Text，
// 每一条分镜图都是空提示词发出去的，上游传完参考图才回 400，受理、扣费、排队一路都是成功的。
func TestPromptIntoParams_Text写进没有提示词的参数(t *testing.T) {
	cases := []struct {
		name   string
		params string
		text   string
		want   map[string]any
	}{
		{"只有 Text", `{"aspect_ratio":"16:9"}`, "一只猫", map[string]any{"aspect_ratio": "16:9", "prompt": "一只猫"}},
		{"没有参数", ``, "一只猫", map[string]any{"prompt": "一只猫"}},
		{"参数是 null", `null`, "一只猫", map[string]any{"prompt": "一只猫"}},
		{"空白 prompt 视同没有", `{"prompt":"  "}`, "一只猫", map[string]any{"prompt": "一只猫"}},
		// 调用方放在参数里的那段才是要发的（如脚本域拼好的提示词）：Text 不能把它盖掉
		{"已有 prompt 不动", `{"prompt":"分镜提示词"}`, "别的词", map[string]any{"prompt": "分镜提示词"}},
		{"没有 Text 不动", `{"a":1}`, "  ", map[string]any{"a": float64(1)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := promptIntoParams(json.RawMessage(c.params), c.text)
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("结果不是合法 JSON：%s", out)
			}
			if len(got) != len(c.want) {
				t.Fatalf("得到 %v，期望 %v", got, c.want)
			}
			for k, v := range c.want {
				if got[k] != v {
					t.Fatalf("%s：得到 %v，期望 %v（全部：%v）", k, got[k], v, got)
				}
			}
		})
	}
}

// 解不开的参数原样返回：这里只补一个键，不新增一种失败（后面的环节照原来的方式处理）。
func TestPromptIntoParams_解不开的参数原样返回(t *testing.T) {
	raw := json.RawMessage(`[1,2]`)
	if out := promptIntoParams(raw, "一只猫"); string(out) != string(raw) {
		t.Fatalf("应原样返回，得到 %s", out)
	}
}

// 其余键按原始字节保留：经 any 转一道的话大整数会变成浮点、丢精度。
func TestPromptIntoParams_其余键原样保留(t *testing.T) {
	out := promptIntoParams(json.RawMessage(`{"seed":12345678901234567890,"未来参数":{"x":[1,2]}}`), "猫")
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["seed"]) != "12345678901234567890" || string(got["未来参数"]) != `{"x":[1,2]}` {
		t.Fatalf("其余键被改写了：%s", out)
	}
}
