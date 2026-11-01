package transport

import (
	"encoding/json"
	"testing"
)

// maxDurationSec 的三态（AGT-05）：没传 = 保留、null = 清除、数字 = 设置。
// ⚠️ 用 *int 的话前两者解出来一样，不带字段的老调用方一保存就会把已配的时长清掉。
func TestOptionalInt_三态(t *testing.T) {
	type req struct {
		V optionalInt `json:"maxDurationSec"`
	}
	cases := []struct {
		body    string
		set     bool
		wantNil bool
		want    int
	}{
		{`{}`, false, true, 0},
		{`{"maxDurationSec":null}`, true, true, 0},
		{`{"maxDurationSec":10}`, true, false, 10},
	}
	for _, c := range cases {
		var r req
		if err := json.Unmarshal([]byte(c.body), &r); err != nil {
			t.Fatalf("%s: %v", c.body, err)
		}
		if r.V.Set != c.set || (r.V.Value == nil) != c.wantNil || (!c.wantNil && *r.V.Value != c.want) {
			t.Fatalf("%s: 得到 Set=%v Value=%v", c.body, r.V.Set, r.V.Value)
		}
	}
	var r req
	if err := json.Unmarshal([]byte(`{"maxDurationSec":"10"}`), &r); err == nil {
		t.Fatal("字符串时长应解析失败")
	}
}
