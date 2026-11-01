package domain

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// marketingItem 是一条营销条目（20 张商品图里选了 #1 #3，本项上限 3 张）。
func marketingItem() PlanItem {
	return PlanItem{Kind: "video", ModelCode: "seedance", OperationCode: "ref2video",
		Inputs: map[string]int{"image": 3}, Label: "变体 1", Quantity: 10, Unit: "second", Credits: 50,
		Key: "v1", Variant: 1, VariantLabel: "反问开场", ProductIndexes: []int{1, 3},
		ProductMediaIDs: []string{"101", "103"}, ProductTotal: 20, ImageLimit: intp(3),
		Picks: []PlanPick{{Kind: "hook", Code: "hk_question", Version: 2, Name: "反问开场"}}}
}

func TestPlanItem_营销字段往返不丢(t *testing.T) {
	in := PlanConfirmPayload{Title: "营销", Items: []PlanItem{marketingItem()}, Total: 50, Balance: 100,
		CanApprove: true, Notices: []string{"画面为人物对镜讲解，不保证口型与语音逐字同步"}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out PlanConfirmPayload
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("往返后不一致：\n%+v\n%+v", in, out)
	}
	// imageLimit=0（该操作不收图）也要留下来：omitempty 的 int 会把它吞掉。
	z := marketingItem()
	z.ImageLimit = intp(0)
	b, _ = json.Marshal(z)
	if !strings.Contains(string(b), `"imageLimit":0`) {
		t.Fatalf("imageLimit=0 应保留：%s", b)
	}
	// 键名与契约（AgentPlanItem）逐字一致。
	for _, k := range []string{`"key":"v1"`, `"variant":1`, `"variantLabel":"反问开场"`, `"productIndexes":[1,3]`,
		`"productMediaIds":["101","103"]`, `"productTotal":20`, `"picks":[{"kind":"hook","code":"hk_question","version":2,"name":"反问开场"}]`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("缺少 %s：%s", k, b)
		}
	}
}

func TestPlanItem_通用卡片逐字不变(t *testing.T) {
	// 改动前的通用卡片序列化结果（逐字）：营销字段全部 omitempty，一个都不能多出来。
	p := PlanConfirmPayload{Title: "三打白骨精", DocPath: "/plan.md", PlanDocVersion: 2,
		Items: []PlanItem{
			{Kind: "video", ModelCode: "m1", OperationCode: "img2video", Inputs: map[string]int{"image": 1},
				Label: "镜头 1", Quantity: 5, Unit: "second", Credits: 30},
			{Kind: "image", ModelCode: "m2", Label: "海报", Quantity: 1, Unit: "image",
				BlockedCode: "MODEL_UNAVAILABLE", BlockedReason: "模型当前不可用"},
		},
		Total: 30, Balance: 10, Shortfall: 20}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"title":"三打白骨精","docPath":"/plan.md","planDocVersion":2,"items":[` +
		`{"kind":"video","modelCode":"m1","operationCode":"img2video","inputs":{"image":1},"label":"镜头 1","quantity":5,"unit":"second","credits":30},` +
		`{"kind":"image","modelCode":"m2","label":"海报","quantity":1,"unit":"image","credits":0,"blockedCode":"MODEL_UNAVAILABLE","blockedReason":"模型当前不可用"}],` +
		`"total":30,"balance":10,"shortfall":20,"canApprove":false}`
	if string(b) != want {
		t.Fatalf("通用卡片 JSON 变了：\n得到 %s\n期望 %s", b, want)
	}
}

func planCard(t *testing.T, items ...PlanItem) Card {
	return Card{Kind: CardPlanConfirm, Payload: mustJSON(t, PlanConfirmPayload{Items: items})}
}

func TestValidateSubmission_itemOverrides(t *testing.T) {
	general := PlanItem{Kind: "video", ModelCode: "m1", Label: "通用", Quantity: 5, Unit: "second"}
	card := planCard(t, marketingItem(), general)
	cases := []struct {
		name string
		sub  string
		ok   bool
		msg  string
	}{
		{"换成合法的两张", `{"decision":"approve","itemOverrides":[{"key":"v1","productIndexes":[2,20]}]}`, true, ""},
		{"恰好上限", `{"decision":"approve","itemOverrides":[{"key":"v1","productIndexes":[5,6,7]}]}`, true, ""},
		{"不带覆盖", `{"decision":"approve"}`, true, ""},
		{"键不存在", `{"decision":"approve","itemOverrides":[{"key":"v9","productIndexes":[1]}]}`, false, "不在这张卡片上"},
		{"越界", `{"decision":"approve","itemOverrides":[{"key":"v1","productIndexes":[21]}]}`, false, "#21 不存在"},
		{"零号", `{"decision":"approve","itemOverrides":[{"key":"v1","productIndexes":[0]}]}`, false, "不存在"},
		{"重复", `{"decision":"approve","itemOverrides":[{"key":"v1","productIndexes":[2,2]}]}`, false, "重复"},
		{"超上限", `{"decision":"approve","itemOverrides":[{"key":"v1","productIndexes":[1,2,3,4]}]}`, false, "最多 3 张"},
		{"空列表", `{"decision":"approve","itemOverrides":[{"key":"v1","productIndexes":[]}]}`, false, "至少选择一张"},
		{"同一条目两次", `{"decision":"approve","itemOverrides":[{"key":"v1","productIndexes":[1]},{"key":"v1","productIndexes":[2]}]}`, false, "重复换图"},
		{"随 adjust 提交", `{"decision":"adjust","comment":"换一下","itemOverrides":[{"key":"v1","productIndexes":[1]}]}`, false, "只能随规划确认卡的 approve"},
		{"未知字段", `{"decision":"approve","itemOverrides":[{"key":"v1","indexes":[1]}]}`, false, "格式错误"},
	}
	for _, c := range cases {
		err := ValidateSubmission(card, json.RawMessage(c.sub))
		if c.ok != (err == nil) {
			t.Errorf("%s：期望 ok=%v，得到 %v", c.name, c.ok, err)
			continue
		}
		if err != nil {
			if !errors.Is(err, ErrInvalidSubmission) || !strings.Contains(err.Error(), c.msg) {
				t.Errorf("%s：错误应含「%s」且是 ErrInvalidSubmission：%v", c.name, c.msg, err)
			}
		}
	}
	// 通用条目没有键，不能被换图；追加额度卡不收 itemOverrides。
	noKey := planCard(t, general)
	if err := ValidateSubmission(noKey, json.RawMessage(`{"decision":"approve","itemOverrides":[{"key":"","productIndexes":[1]}]}`)); err == nil {
		t.Error("通用条目不应接受换图")
	}
	budget := Card{Kind: CardBudget, Payload: mustJSON(t, BudgetPayload{Requested: 10})}
	if err := ValidateSubmission(budget, json.RawMessage(`{"decision":"approve","itemOverrides":[{"key":"v1","productIndexes":[1]}]}`)); err == nil {
		t.Error("追加额度卡不应接受 itemOverrides")
	}
}

func TestSelectProductIndexes_缺省越界重复超上限(t *testing.T) {
	if got, _ := SelectProductIndexes(20, nil, 3); !reflect.DeepEqual(got, []int{1, 2, 3}) {
		t.Errorf("缺省取前 3 张：%v", got)
	}
	if got, _ := SelectProductIndexes(2, nil, 3); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Errorf("不足上限取全部：%v", got)
	}
	if got, _ := SelectProductIndexes(5, nil, 0); len(got) != 0 {
		t.Errorf("不收图取 0 张：%v", got)
	}
	if got, _ := SelectProductIndexes(5, []int{4, 2}, 3); !reflect.DeepEqual(got, []int{4, 2}) {
		t.Errorf("顺序即发出顺序，不重排：%v", got)
	}
	for _, c := range []struct {
		idx  []int
		max  int
		code string
	}{{[]int{6}, 3, ProductIndexOutOfRange}, {[]int{1, 1}, 3, ProductIndexDuplicate}, {[]int{1, 2}, 1, ProductIndexTooMany},
		{[]int{1}, 0, ProductIndexTooMany}} {
		_, err := SelectProductIndexes(5, c.idx, c.max)
		var pe *ProductIndexError
		if !errors.As(err, &pe) || pe.Code != c.code {
			t.Errorf("%v max=%d 应为 %s，得到 %v", c.idx, c.max, c.code, err)
		}
	}
}

func TestApplyOverrides_重算商品图与输入计数(t *testing.T) {
	products := make([]string, 20)
	for i := range products {
		products[i] = string(rune('a' + i))
	}
	in := MarketingInputs{Products: products, Characters: 1, SourceVideo: false}
	items := []PlanItem{marketingItem(), {Kind: "audio", Label: "配音", Key: "a1"}}
	out, changed, err := ApplyOverrides(items, []ItemOverride{{Key: "v1", ProductIndexes: []int{20}}}, in)
	if err != nil || !changed {
		t.Fatalf("应改动：%v %v", changed, err)
	}
	if !reflect.DeepEqual(out[0].ProductMediaIDs, []string{"t"}) || !reflect.DeepEqual(out[0].ProductIndexes, []int{20}) {
		t.Errorf("应换成 #20：%+v", out[0])
	}
	// 输入计数 = 商品图 1 + 角色 1，与执行时发出的输入同源。
	if !reflect.DeepEqual(out[0].Inputs, map[string]int{"image": 2}) {
		t.Errorf("输入计数应重算：%v", out[0].Inputs)
	}
	if !reflect.DeepEqual(out[1], items[1]) || !reflect.DeepEqual(items[0], marketingItem()) {
		t.Error("未覆盖的条目不变、入参不被改动")
	}
	_, changed, _ = ApplyOverrides(items, []ItemOverride{{Key: "v1", ProductIndexes: []int{1, 3}}}, in)
	if changed {
		t.Error("与原选择相同不算改动")
	}
}

func TestMarketingRoleCounts_零值不写(t *testing.T) {
	if got := MarketingRoleCounts(0, 0, false); got != nil {
		t.Errorf("什么都没有时为 nil：%v", got)
	}
	if got := MarketingRoleCounts(2, 1, true); !reflect.DeepEqual(got, map[string]int{"image": 3, "video": 1}) {
		t.Errorf("商品图 + 角色都是 image：%v", got)
	}
	if ProductImageLimit(4, 1) != 3 || ProductImageLimit(1, 2) != 0 {
		t.Error("上限减去角色图，不小于 0")
	}
}

func TestToolRejection_结构化内容(t *testing.T) {
	r := &ToolRejection{Code: ToolErrTooManyImages, Message: "最多 3 张",
		Details: map[string]any{"item": "v1", "max": 3, "error": "被覆盖"}}
	var m map[string]any
	if err := json.Unmarshal(r.Content(), &m); err != nil {
		t.Fatal(err)
	}
	if m["error"] != "too_many_images" || m["item"] != "v1" || m["max"].(float64) != 3 || m["message"] != "最多 3 张" {
		t.Fatalf("内容不对：%v", m)
	}
	if !strings.HasPrefix(r.Error(), "too_many_images") {
		t.Errorf("Error 以码开头：%s", r.Error())
	}
	if !ValidPlanKey("v1") || !ValidPlanKey(DefaultPlanKey(3)) || ValidPlanKey("有中文") || ValidPlanKey(strings.Repeat("a", 33)) {
		t.Error("条目键格式")
	}
}
