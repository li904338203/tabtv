package app

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

const oneImage = `[{"kind":"generation","order":1,"mediaId":7,"mediaKind":"image","width":1536,"height":1024}]`

func codeOf(err error) errcode.Code {
	var be *httpx.BizError
	if errors.As(err, &be) {
		return be.Code
	}
	return 0
}

// IMGD-2 / R-5：提示词、通用参数、裁切区域只由服务端写；客户端带来的服务端专属键与预设一律丢掉。
func TestToolStep_服务端写提示词与参数并丢弃客户端键(t *testing.T) {
	params := `{"tool":"upscale","toolOptions":{"level":"fine"},"upstream":{"x":1},"toolCrop":{"x":0,"y":0,"w":9,"h":9},
		"output":{"width":1},"preset":"cinematic_light","prompt":"随便写的","aspect_ratio":"1:1"}`
	if got := toolCodeOf(json.RawMessage(params)); got != "upscale" {
		t.Fatalf("工具码：%q", got)
	}
	tp, out, err := toolStep("upscale", json.RawMessage(params), json.RawMessage(oneImage))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	for _, k := range []string{"upstream", "toolCrop", "output", "preset"} {
		if _, ok := m[k]; ok {
			t.Fatalf("%s 不该留在参数里：%v", k, m)
		}
	}
	if !strings.Contains(m["prompt"].(string), "精细的高清重绘") || m["aspect_ratio"] != "3:2" || m["quality"] != "high" {
		t.Fatalf("提示词与通用参数由服务端写：%v", m)
	}
	if tp.Count != 1 || m["count"] != float64(1) {
		t.Fatalf("张数：%d %v", tp.Count, m["count"])
	}
}

func TestToolStep_情绪写裁切区域(t *testing.T) {
	params := `{"tool":"expression_edit","toolOptions":{"sourceMediaId":"7","count":2,
		"roles":[{"box":{"x":100,"y":200,"w":200,"h":200},"arousal":1,"warmth":0}]}}`
	_, out, err := toolStep("expression_edit", json.RawMessage(params), json.RawMessage(oneImage))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	crop, _ := m["toolCrop"].(map[string]any)
	if crop["x"] != float64(50) || crop["w"] != float64(300) || m["count"] != float64(2) {
		t.Fatalf("裁切区域与张数：%v %v", crop, m["count"])
	}
}

func TestToolStep_源图张数与错误码(t *testing.T) {
	two := `[{"mediaId":7,"mediaKind":"image","width":10,"height":10},{"mediaId":8,"mediaKind":"image"}]`
	if _, _, err := toolStep("relight", json.RawMessage(`{"tool":"relight"}`), json.RawMessage(two)); codeOf(err) != errcode.InvalidArgument {
		t.Fatalf("两张图应拒：%v", err)
	}
	if _, _, err := toolStep("relight", json.RawMessage(`{"tool":"relight"}`), json.RawMessage(`[]`)); codeOf(err) != errcode.InvalidArgument {
		t.Fatalf("没有图应拒：%v", err)
	}
	mask := `[{"mediaId":7,"mediaKind":"image","width":1536,"height":1024},{"mediaId":9,"mediaKind":"image","role":"mask"}]`
	if _, _, err := toolStep("relight", json.RawMessage(`{"tool":"relight","toolOptions":{"smart":true}}`), json.RawMessage(mask)); err != nil {
		t.Fatalf("蒙版不算源图：%v", err)
	}
	expr := `{"tool":"expression_edit","toolOptions":{"sourceMediaId":"8","roles":[{"box":{"x":0,"y":0,"w":100,"h":100},"arousal":1,"warmth":0}]}}`
	if _, _, err := toolStep("expression_edit", json.RawMessage(expr), json.RawMessage(oneImage)); codeOf(err) != errcode.ToolSourceChanged {
		t.Fatalf("源图变了回 42219：%v", err)
	}
	if _, _, err := toolStep("multi_angle", json.RawMessage(`{"tool":"multi_angle","toolOptions":{"vertical":200}}`), json.RawMessage(oneImage)); codeOf(err) != errcode.InvalidArgument {
		t.Fatalf("越界回 40001：%v", err)
	}
}

// 非工具节点的参数里也不许带服务端专属键（upstream / output / toolCrop）。
func TestStripServerOwned_任何参数都丢服务端专属键(t *testing.T) {
	out := stripServerOwned(json.RawMessage(`{"prompt":"x","upstream":{"zoom":99},"output":{"width":1},"toolCrop":{"x":1}}`))
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if len(m) != 1 || m["prompt"] != "x" {
		t.Fatalf("应只删服务端专属键：%s", out)
	}
	if got := stripServerOwned(json.RawMessage(`{"prompt":"x"}`)); string(got) != `{"prompt":"x"}` {
		t.Fatalf("没有专属键时原样返回：%s", got)
	}
}
