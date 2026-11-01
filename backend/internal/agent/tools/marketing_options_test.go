package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
)

type fakeOptionLister struct {
	q   app.MarketingOptionsQuery
	res app.MarketingOptionsResult
}

func (f *fakeOptionLister) ListMarketingOptions(_ context.Context, q app.MarketingOptionsQuery) (
	app.MarketingOptionsResult, error) {
	f.q = q
	return f.res, nil
}

func TestListMarketingOptionsTool_参数与截断(t *testing.T) {
	f := &fakeOptionLister{res: app.MarketingOptionsResult{Items: []app.MarketingOption{{
		Kind: "hook", Code: "ask_open", Name: strings.Repeat("名", 60), Description: strings.Repeat("说", 200),
	}}, More: true}}
	tool := ListMarketingOptions{Options: f}
	if tool.Name() != domain.ToolListMarketingOptions || tool.Paid() {
		t.Fatal("工具名 / 付费属性不对")
	}
	res, err := tool.Run(context.Background(), RunContext{},
		json.RawMessage(`{"kind":"hook","category":" opening ","q":" 提问 ","limit":99}`))
	if err != nil {
		t.Fatal(err)
	}
	if f.q != (app.MarketingOptionsQuery{Kind: "hook", Category: "opening", Q: "提问", Limit: 30}) {
		t.Errorf("查询参数：%+v", f.q)
	}
	var got app.MarketingOptionsResult
	if err := json.Unmarshal(res.Content, &got); err != nil || !got.More || len(got.Items) != 1 {
		t.Fatalf("结果：%s %v", res.Content, err)
	}
	if n := len([]rune(got.Items[0].Name)); n > marketingNameRunes+1 {
		t.Errorf("名称应截短，得到 %d 字", n)
	}
	if n := len([]rune(got.Items[0].Description)); n > marketingDescRunes+1 {
		t.Errorf("说明应截短，得到 %d 字", n)
	}
	// 默认条数、空结果回空数组、未知参数拒绝、端口未接入报错。
	f.res = app.MarketingOptionsResult{}
	res, err = tool.Run(context.Background(), RunContext{}, json.RawMessage(`{}`))
	if err != nil || f.q.Limit != defaultMarketingOptionLimit || !strings.Contains(string(res.Content), `"items":[]`) {
		t.Errorf("默认：%s %+v %v", res.Content, f.q, err)
	}
	if _, err := tool.Run(context.Background(), RunContext{}, json.RawMessage(`{"prompt":"x"}`)); err == nil {
		t.Error("未知参数应拒绝")
	}
	if _, err := (ListMarketingOptions{}).Run(context.Background(), RunContext{}, json.RawMessage(`{}`)); err == nil {
		t.Error("端口未接入应报错")
	}
	// 注册表只收规格表里的名字：它在表里。
	if _, err := NewRegistry(tool); err != nil {
		t.Errorf("应能注册：%v", err)
	}
}
