package protocol

import (
	"net/http"
	"testing"
)

// fakePlain 只实现基本接口；fakeHeaders 另外实现 HeaderAware（状态在响应头里，同火山识别）。
type fakePlain struct{}

func (fakePlain) Code() string                     { return "fake_plain" }
func (fakePlain) Async() bool                      { return true }
func (fakePlain) RequiresQueryPath() bool          { return false }
func (fakePlain) BuildSubmit(Job) (Request, error) { return Request{}, nil }
func (fakePlain) ParseSubmit(int, []byte) (Result, error) {
	return Result{Message: "plain-submit"}, nil
}
func (fakePlain) BuildQuery(Job, string) (Request, bool) { return Request{}, true }
func (fakePlain) ParseQuery(int, []byte) (Result, error) {
	return Result{Message: "plain-query"}, nil
}

type fakeHeaders struct{ fakePlain }

func (fakeHeaders) ParseSubmitWithHeaders(_ int, h http.Header, _ []byte) (Result, error) {
	return Result{Message: "headers-submit:" + h.Get("X-Api-Status-Code")}, nil
}
func (fakeHeaders) ParseQueryWithHeaders(_ int, h http.Header, _ []byte) (Result, error) {
	state := StatePending
	if h.Get("X-Api-Status-Code") == "20000000" {
		state = StateSuccess
	}
	return Result{State: state, Message: "headers-query"}, nil
}

func TestParseResponse_实现了HeaderAware才把响应头交给协议(t *testing.T) {
	h := http.Header{}
	h.Set("X-Api-Status-Code", "20000000")

	if r, _ := ParseSubmitResponse(fakePlain{}, 200, h, nil); r.Message != "plain-submit" {
		t.Fatalf("没实现 HeaderAware 的协议照旧走 ParseSubmit: %+v", r)
	}
	if r, _ := ParseQueryResponse(fakePlain{}, 200, h, nil); r.Message != "plain-query" {
		t.Fatalf("没实现 HeaderAware 的协议照旧走 ParseQuery: %+v", r)
	}
	if r, _ := ParseSubmitResponse(fakeHeaders{}, 200, h, nil); r.Message != "headers-submit:20000000" {
		t.Fatalf("实现了 HeaderAware 的协议要拿到响应头: %+v", r)
	}
	if r, _ := ParseQueryResponse(fakeHeaders{}, 200, h, nil); r.State != StateSuccess {
		t.Fatalf("查询也要拿到响应头: %+v", r)
	}
	if r, _ := ParseQueryResponse(fakeHeaders{}, 200, nil, nil); r.State != StatePending {
		t.Fatalf("没有响应头时（nil）不该崩: %+v", r)
	}
}

// 哪些协议读响应头要在这里显式登记：没登记的协议实现了 HeaderAware 会当场红（它的解析从此不看正文里的状态），
// 登记了却没实现同样红（状态在头里的协议读不到头，处理中会被当成完成）。
func TestHeaderAware_现有协议登记(t *testing.T) {
	want := map[string]bool{"volc_asr": true, "volc_asr_standard": true}
	for code, p := range registry {
		if _, ok := p.(HeaderAware); ok != want[code] {
			t.Errorf("协议 %s 的 HeaderAware 与登记不符（实现 = %v）", code, ok)
		}
	}
}
