package domain

import "testing"

func TestRatio_NoDataMinusOne(t *testing.T) {
	if got := Ratio(0, 0); got != -1 {
		t.Fatalf("没有数据应为 -1，got %v", got)
	}
	if got := Ratio(0, 10); got != 0 {
		t.Fatalf("0/10 应为 0，got %v", got)
	}
	if got := Ratio(1, 3); got != 0.3333 {
		t.Fatalf("1/3 应四舍五入到 0.3333，got %v", got)
	}
}

func TestTopClients_SumEqualsTotal(t *testing.T) {
	in := []ClientShare{
		{Name: "Claude", Calls: 50, Errors: 1},
		{Name: "", Calls: 7, Errors: 2},
		{Name: "Cursor", Calls: 30},
		{Name: "Codex", Calls: 9, Errors: 3},
		{Name: "opencode", Calls: 4},
	}
	out := TopClients(in, 2)
	var calls, errs, total, totalErr int64
	for _, c := range in {
		total += c.Calls
		totalErr += c.Errors
	}
	for _, c := range out {
		calls += c.Calls
		errs += c.Errors
	}
	if calls != total || errs != totalErr {
		t.Fatalf("各项之和 %d/%d 应等于总数 %d/%d：%+v", calls, errs, total, totalErr, out)
	}
	if len(out) != 4 || out[0].Name != "Claude" || out[1].Name != "Cursor" ||
		out[2].Kind != ClientKindOther || out[2].Calls != 13 || out[3].Kind != ClientKindUnknown || out[3].Calls != 7 {
		t.Fatalf("前 2 名 + 其他 + 未知：%+v", out)
	}
}

func TestTopClients_Empty(t *testing.T) {
	if out := TopClients(nil, 5); out == nil || len(out) != 0 {
		t.Fatalf("空输入应回空切片（非 nil）：%+v", out)
	}
}
