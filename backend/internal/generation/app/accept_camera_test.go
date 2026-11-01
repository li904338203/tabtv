package app

import (
	"context"
	"encoding/json"
	"testing"
)

// 视频节点「运镜」在受理链路里的拼接位置。真正的选项校验与说法在目录域（catalog/domain.ComposeCamera）。

type stubCamera struct{ part string }

func (c stubCamera) CameraPrompt(context.Context, json.RawMessage) (string, error) {
	return c.part, nil
}

func TestAccept_运镜紧跟预设排在营销段与素材词之前(t *testing.T) {
	mk := &stubMarketing{text: "【创意】A"}
	s := &Service{presets: stubPresets{""}, camera: stubCamera{"运镜：镜头运动推近。"}, marketing: mk}
	in := AcceptInput{Params: json.RawMessage(`{"prompt":"一只猫","camera":{"movement":"推近"},"marketing":{"picks":[]}}`)}
	got, _, _, err := s.composeSuffix(context.Background(), in, nil, nil, "水墨风格")
	if err != nil {
		t.Fatal(err)
	}
	if want := "运镜：镜头运动推近。\n【创意】A\n水墨风格"; got != want {
		t.Fatalf("得到 %q，期望 %q", got, want)
	}

	s.presets = stubPresets{"预设版式"}
	got, _, _, err = s.composeSuffix(context.Background(), in, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := "预设版式\n运镜：镜头运动推近。\n【创意】A"; got != want {
		t.Fatalf("有预设时：得到 %q，期望 %q", got, want)
	}
}

// 没选运镜时附加段与改动前逐字相等（含「没有预设来源时素材段原样保留」那条旧行为，MKR-10）。
func TestAccept_没选运镜后缀不变(t *testing.T) {
	in := AcceptInput{Params: json.RawMessage(`{"prompt":"一只猫"}`)}
	s := &Service{camera: stubCamera{""}}
	got, _, _, err := s.composeSuffix(context.Background(), in, nil, nil, " 水墨风格 ")
	if err != nil {
		t.Fatal(err)
	}
	if got != " 水墨风格 " {
		t.Fatalf("得到 %q，期望原样保留素材段", got)
	}
}
