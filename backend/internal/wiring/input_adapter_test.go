package wiring

import (
	"encoding/json"
	"reflect"
	"testing"

	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
)

// 画布域的解析结果要**逐字段**抄进生成域：漏抄没有编译错误，DirectorKinds 漏了的话导演台约束段悄悄不出现（DRD-18）。
// 用反射核对：AcceptedInputs 的每个字段在 ResolvedInputs 里都有同名字段，且值被抄过去了 —— 将来再加字段也会当场红。
func TestInputAdapter_带上DirectorKinds(t *testing.T) {
	in := canvasapp.AcceptedInputs{
		Inputs: json.RawMessage(`[{"kind":"reference"}]`), PromptSuffix: "水墨",
		RoleCounts: map[string]int{"image": 1}, Issues: []string{"x"}, DirectorKinds: []string{"capture", "preview"},
		RangeLengthMs: 3000,
	}
	out := toResolvedInputs(in)
	src, dst := reflect.ValueOf(in), reflect.ValueOf(out)
	for i := 0; i < src.NumField(); i++ {
		name := src.Type().Field(i).Name
		if src.Field(i).IsZero() {
			t.Fatalf("用例要给 %s 赋值，否则这条测试核不出漏抄", name)
		}
		f := dst.FieldByName(name)
		if !f.IsValid() {
			t.Errorf("生成域的 ResolvedInputs 没有 %s 字段", name)
			continue
		}
		if !reflect.DeepEqual(f.Interface(), src.Field(i).Interface()) {
			t.Errorf("%s 没有抄过去：%v", name, f.Interface())
		}
	}
}
