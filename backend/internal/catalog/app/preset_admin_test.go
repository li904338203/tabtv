package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/catalog/repo"
)

func TestValidatePreset_表驱动(t *testing.T) {
	ok := repo.AdminImagePreset{Code: "my_tone", Name: "质感", PresetGroup: "tuning", Spec: "retouch",
		InputMode: "either", Prompt: "调色", Params: json.RawMessage(`{"a":1}`)}
	if err := validatePreset(ok); err != nil {
		t.Fatalf("合法预设被拒：%v", err)
	}
	cases := []struct {
		name string
		mut  func(*repo.AdminImagePreset)
		want string
	}{
		{"编码", func(p *repo.AdminImagePreset) { p.Code = "Bad" }, "编码"},
		{"名称", func(p *repo.AdminImagePreset) { p.Name = " " }, "名称"},
		{"分组", func(p *repo.AdminImagePreset) { p.PresetGroup = "x" }, "分组"},
		{"产出类型", func(p *repo.AdminImagePreset) { p.Spec = "x" }, "产出类型"},
		{"输入要求", func(p *repo.AdminImagePreset) { p.InputMode = "x" }, "输入"},
		{"配方", func(p *repo.AdminImagePreset) { p.Prompt = "" }, "提示词"},
		{"参数不是对象", func(p *repo.AdminImagePreset) { p.Params = json.RawMessage(`[1]`) }, "参数"},
		{"维度不是数组", func(p *repo.AdminImagePreset) { p.Dimensions = json.RawMessage(`{}`) }, "维度"},
		{"维度缺选项", func(p *repo.AdminImagePreset) { p.Dimensions = json.RawMessage(`[{"code":"a","options":[]}]`) }, "维度"},
	}
	for _, c := range cases {
		p := ok
		c.mut(&p)
		err := validatePreset(p)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：期望含「%s」的错误，得到 %v", c.name, c.want, err)
		}
	}
}

func TestPreviewPreset_未选的维度按默认出词(t *testing.T) {
	s := &Service{}
	dims := json.RawMessage(`[{"code":"light","default":"soft","options":[{"value":"soft","prompt":"柔光"},{"value":"hard","prompt":"硬光"}]}]`)
	if got := s.PreviewPreset("基础配方", dims, nil); got != "基础配方\n柔光" {
		t.Fatalf("默认出词不对：%q", got)
	}
	if got := s.PreviewPreset("基础配方", dims, map[string]string{"light": "hard"}); got != "基础配方\n硬光" {
		t.Fatalf("选中值没生效：%q", got)
	}
}
