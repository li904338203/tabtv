package wiring

import (
	"encoding/json"
	"testing"
)

// 组子项的提示词必须写进 params（CD-81）：Worker 只读 `params.prompt`。
//
// ⚠️ 起因：组适配器只把提示词放在受理的 Text 上，每一条分镜图 / 视频子项都是空提示词发出去的。
// 写的位置是**最后**：子项快照里即使带了一个 prompt 键，也不能把脚本域按镜头拼好的那段顶掉。
func TestWithPrompt_提示词最后写且其余键原样保留(t *testing.T) {
	out, err := withPrompt(json.RawMessage(`{"aspect_ratio":"16:9","operation":"img2img","duration":5,"prompt":"旧词"}`), "第 1 镜的分镜提示词")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["prompt"]) != `"第 1 镜的分镜提示词"` {
		t.Fatalf("提示词应是脚本域给的那段，得到 %s", got["prompt"])
	}
	if string(got["aspect_ratio"]) != `"16:9"` || string(got["operation"]) != `"img2img"` || string(got["duration"]) != "5" {
		t.Fatalf("其余键应原样保留：%s", out)
	}
}

func TestWithPrompt_空快照也能写进去(t *testing.T) {
	for _, raw := range []string{"", "null", "{}"} {
		out, err := withPrompt(json.RawMessage(raw), "猫")
		if err != nil {
			t.Fatalf("%q：%v", raw, err)
		}
		if string(out) != `{"prompt":"猫"}` {
			t.Fatalf("%q：得到 %s", raw, out)
		}
	}
	if _, err := withPrompt(json.RawMessage(`[1]`), "猫"); err == nil {
		t.Fatal("快照不是对象时应报错，而不是悄悄丢掉提示词")
	}
}
