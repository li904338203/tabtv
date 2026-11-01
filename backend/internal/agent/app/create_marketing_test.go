package app

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 营销分支（MKT-T-17）的单测：假端口验校验顺序与失败点名；槽位规则本身的真实实现
// （marketing/domain.ValidateSlotFill 经 wiring.AgentScene）在 transport 集成测试里验。

type fakeScene struct {
	resolveCalls int
	resolveErr   error
	slotCalls    int
	lastFill     SceneSlotFill
	lastDecl     json.RawMessage
	// slots 不为 nil 时作为槽位校验结果。
	slots func(SceneSlotFill) []SlotIssue
}

func (f *fakeScene) Resolve(_ context.Context, picks []ScenePickRef, _ time.Time) ([]ScenePin, error) {
	f.resolveCalls++
	if f.resolveErr != nil {
		return nil, f.resolveErr
	}
	out := make([]ScenePin, 0, len(picks))
	for i, p := range picks {
		out = append(out, ScenePin{Kind: p.Kind, Code: p.Code, Version: i + 3, Name: "名称-" + p.Code})
	}
	return out, nil
}

func (f *fakeScene) ValidateSlotFill(decl json.RawMessage, fill SceneSlotFill) []SlotIssue {
	f.slotCalls++
	f.lastFill, f.lastDecl = fill, decl
	if f.slots != nil {
		return f.slots(fill)
	}
	return nil
}

// fakeMarketingRefs 只认本工作区（ws=1）的媒体与资产。
type fakeMarketingRefs struct {
	calls  int
	media  map[int64]string
	assets map[int64]string
}

func (f *fakeMarketingRefs) MediaKind(_ context.Context, ws, id int64) (string, error) {
	f.calls++
	if k, ok := f.media[id]; ok && ws == 1 {
		return k, nil
	}
	return "", httpx.Errf(errcode.NotFound, "素材不存在")
}

func (f *fakeMarketingRefs) Asset(_ context.Context, ws, id int64) (string, string, error) {
	f.calls++
	if typ, ok := f.assets[id]; ok && ws == 1 {
		return "角色" + strconv.FormatInt(id, 10), typ, nil
	}
	return "", "", httpx.Errf(errcode.NotFound, "资产不存在")
}

// fakeLibrary 是营销库快照：只认本工作区（ws=1）的商品 301（3 张图）与角色 401（带音频）/ 402。
type fakeLibrary struct {
	calls     int
	lastProd  int64
	lastChars []int64
}

func (f *fakeLibrary) Snapshot(_ context.Context, ws, productID int64, chars []int64) (*SceneProductSnap,
	[]SceneCharacterSnap, error) {
	f.calls++
	f.lastProd, f.lastChars = productID, chars
	var p *SceneProductSnap
	if productID > 0 {
		if productID != 301 || ws != 1 {
			return nil, nil, &SceneLibraryMissing{Slot: "product", ID: productID}
		}
		p = &SceneProductSnap{ID: 301, Version: 3, Name: "保温杯", SellingPoints: "24 小时保温",
			Description: "双层真空", ImageMediaIDs: []int64{9003, 9001, 9002}}
	}
	out := []SceneCharacterSnap{}
	for _, id := range chars {
		switch {
		case id == 401 && ws == 1:
			out = append(out, SceneCharacterSnap{ID: 401, Name: "柚子君", MainMediaID: 9401, Appearance: "圆脸",
				VoiceText: "少年音", VoiceMediaID: 9499})
		case id == 402 && ws == 1:
			out = append(out, SceneCharacterSnap{ID: 402, Name: "小绿", MainMediaID: 9402})
		default:
			return nil, nil, &SceneLibraryMissing{Slot: "character", ID: id}
		}
	}
	return p, out, nil
}

func newMarketingSvc(scene *fakeScene, refs *fakeMarketingRefs) *Service {
	return newMarketingSvcLib(scene, refs, &fakeLibrary{})
}

func newMarketingSvcLib(scene *fakeScene, refs *fakeMarketingRefs, lib *fakeLibrary) *Service {
	return &Service{p: ServicePorts{Scene: scene, Refs: refs, Library: lib}, now: time.Now}
}

func strp(s string) *string { return &s }

func bizOf(t *testing.T, err error) *httpx.BizError {
	t.Helper()
	var be *httpx.BizError
	if !errors.As(err, &be) {
		t.Fatalf("期望业务错误，得到 %v", err)
	}
	return be
}

func marketingIn() CreateSessionInput {
	return CreateSessionInput{
		IdempotencyKey: "k1", CreateProjectName: strp("营销项目"), Text: "做一条带货视频",
		Refs: []RefInput{
			{Kind: RefMarketingProduct, ID: "301", Slot: "product"},
			{Kind: RefAsset, ID: "201", Slot: "character", Order: 2},
			{Kind: RefMarketingCharacter, ID: "401", Slot: "character", Order: 1},
		},
		Marketing: &MarketingInput{
			Picks: MarketingPicks{Creative: "cr_a", Style: "st_b"},
		},
	}
}

func goodRefs() *fakeMarketingRefs {
	return &fakeMarketingRefs{media: map[int64]string{101: "image", 102: "image", 103: "video"},
		assets: map[int64]string{201: "character", 202: "scene"}}
}

func TestCreateSession_营销分支校验顺序(t *testing.T) {
	ctx := context.Background()
	a := SessionActor{WorkspaceID: 1, UserID: 9}

	t.Run("营销未开放", func(t *testing.T) {
		s := &Service{p: ServicePorts{Refs: goodRefs()}, now: time.Now}
		be := bizOf(t, func() error { _, err := s.prepareMarketing(ctx, a, marketingIn()); return err }())
		if be.Code != errcode.InvalidArgument || be.Details["field"] != "scene" {
			t.Fatalf("期望 40001 field=scene，得到 %v %v", be.Code, be.Details)
		}
	})

	t.Run("①建项目先于一切", func(t *testing.T) {
		sc, refs := &fakeScene{resolveErr: errors.New("不该被调用")}, goodRefs()
		in := marketingIn()
		in.CreateProjectName = nil
		in.Refs = append(in.Refs, RefInput{Kind: RefAsset, ID: "999", Slot: "character"}) // 越权资产也在
		_, err := newMarketingSvc(sc, refs).prepareMarketing(ctx, a, in)
		be := bizOf(t, err)
		if be.Details["field"] != "createProject" || refs.calls != 0 || sc.slotCalls != 0 || sc.resolveCalls != 0 {
			t.Fatalf("期望先报 createProject 且不查归属 / 槽位 / 选项：%v refs=%d slot=%d resolve=%d",
				be.Details, refs.calls, sc.slotCalls, sc.resolveCalls)
		}
	})

	t.Run("②张数预检先于查归属", func(t *testing.T) {
		sc, refs := &fakeScene{}, goodRefs()
		in := marketingIn()
		in.Refs = nil
		for i := 0; i < 21; i++ {
			in.Refs = append(in.Refs, RefInput{Kind: RefAsset, ID: strconv.Itoa(500 + i), Slot: "character"})
		}
		lib := &fakeLibrary{}
		_, err := newMarketingSvcLib(sc, refs, lib).prepareMarketing(ctx, a, in)
		be := bizOf(t, err)
		if be.Code != errcode.InvalidArgument || be.Details["slot"] != "character" || refs.calls != 0 || lib.calls != 0 {
			t.Fatalf("21 个角色应 40001 点名角色槽且不查归属 / 快照：%v %v refs=%d lib=%d", be.Code, be.Details,
				refs.calls, lib.calls)
		}
	})

	t.Run("②归属失败不进槽位与选项", func(t *testing.T) {
		sc := &fakeScene{}
		in := marketingIn()
		in.Refs = in.Refs[:2]
		in.Refs = append(in.Refs, RefInput{Kind: RefAsset, ID: "999", Slot: "character", Order: 1})
		_, err := newMarketingSvc(sc, goodRefs()).prepareMarketing(ctx, a, in)
		be := bizOf(t, err)
		if be.Code != errcode.InvalidArgument || sc.slotCalls != 0 || sc.resolveCalls != 0 {
			t.Fatalf("越权引用应 40001 且不往下走：%v slot=%d resolve=%d", be, sc.slotCalls, sc.resolveCalls)
		}
	})

	t.Run("③槽位失败不钉选项", func(t *testing.T) {
		sc := &fakeScene{slots: func(SceneSlotFill) []SlotIssue {
			return []SlotIssue{{Slot: "character", Field: "character", Code: "too_many", Message: "角色最多 2 项"}}
		}}
		_, err := newMarketingSvc(sc, goodRefs()).prepareMarketing(ctx, a, marketingIn())
		be := bizOf(t, err)
		if be.Details["slot"] != "character" || sc.resolveCalls != 0 {
			t.Fatalf("槽位失败应点名且不调 Resolve：%v resolve=%d", be.Details, sc.resolveCalls)
		}
	})

	t.Run("通过时交给槽位校验的填充", func(t *testing.T) {
		sc := &fakeScene{}
		p, err := newMarketingSvc(sc, goodRefs()).prepareMarketing(ctx, a, marketingIn())
		if err != nil {
			t.Fatal(err)
		}
		f := sc.lastFill
		if len(f.Refs["product"]) != 1 || f.Refs["product"][0] != RefMarketingProduct ||
			len(f.Refs["character"]) != 2 || f.Refs["character"][0] != RefMarketingCharacter ||
			f.Refs["character"][1] != "character_asset" || len(f.Fields) != 0 {
			t.Fatalf("填充不对：%+v", f)
		}
		if sc.lastDecl != nil {
			t.Fatalf("未选技能应传空声明（用场景默认）：%s", sc.lastDecl)
		}
		if p.input.Product.Name != "保温杯" {
			t.Fatalf("商品名来自快照：%q", p.input.Product.Name)
		}
	})
}

func TestCreateSession_缺槽点名(t *testing.T) {
	sc := &fakeScene{slots: func(f SceneSlotFill) []SlotIssue {
		if len(f.Refs["product"]) == 0 {
			return []SlotIssue{{Slot: "product", Field: "product", Code: "required", Message: "请填写「商品」"}}
		}
		return nil
	}}
	in := marketingIn()
	in.Refs = in.Refs[1:] // 只剩角色
	_, err := newMarketingSvc(sc, goodRefs()).prepareMarketing(context.Background(), SessionActor{WorkspaceID: 1}, in)
	be := bizOf(t, err)
	if be.Code != errcode.InvalidArgument || be.Details["slot"] != "product" || be.Details["code"] != "required" {
		t.Fatalf("缺商品槽应 40001 details.slot=product：%v %v", be.Code, be.Details)
	}
	if issues, _ := be.Details["issues"].([]map[string]any); len(issues) != 1 {
		t.Fatalf("应一次报完全部问题：%v", be.Details["issues"])
	}
}

func TestCreateSession_营销必须建项目(t *testing.T) {
	ctx := context.Background()
	in := marketingIn()
	in.CreateProjectName, in.ProjectID = nil, 123
	_, err := newMarketingSvc(&fakeScene{}, goodRefs()).prepareMarketing(ctx, SessionActor{WorkspaceID: 1}, in)
	if be := bizOf(t, err); be.Code != errcode.InvalidArgument || be.Details["field"] != "createProject" {
		t.Fatalf("挂到已有项目应 40001：%v %v", be.Code, be.Details)
	}
	in = marketingIn()
	in.ProjectID = 123
	_, err = newMarketingSvc(&fakeScene{}, goodRefs()).prepareMarketing(ctx, SessionActor{WorkspaceID: 1}, in)
	if be := bizOf(t, err); be.Details["field"] != "projectId" {
		t.Fatalf("同时带 projectId 应拒绝：%v", be.Details)
	}
}

func TestCreateSession_Resolve失败42215(t *testing.T) {
	want := httpx.Errf(errcode.MarketingOptionUnavailable, "创意「x」已下线").
		WithDetails(map[string]any{"kind": "creative", "code": "cr_a", "reason": "disabled", "userFixable": false})
	sc := &fakeScene{resolveErr: want}
	_, err := newMarketingSvc(sc, goodRefs()).prepareMarketing(context.Background(), SessionActor{WorkspaceID: 1}, marketingIn())
	be := bizOf(t, err)
	if be.Code != errcode.MarketingOptionUnavailable || be.Details["reason"] != "disabled" || be.Details["kind"] != "creative" {
		t.Fatalf("42215 应原样返回：%v %v", be.Code, be.Details)
	}
}

func TestCreateSession_营销输入形状(t *testing.T) {
	lib := &fakeLibrary{}
	p, err := newMarketingSvcLib(&fakeScene{}, goodRefs(), lib).prepareMarketing(context.Background(),
		SessionActor{WorkspaceID: 1}, marketingIn())
	if err != nil {
		t.Fatal(err)
	}
	if lib.lastProd != 301 || len(lib.lastChars) != 1 || lib.lastChars[0] != 401 {
		t.Fatalf("快照应按工作区取商品与角色库角色（不含脚本资产）：%d %v", lib.lastProd, lib.lastChars)
	}
	var si map[string]any
	if err := json.Unmarshal(p.sceneInput, &si); err != nil {
		t.Fatal(err)
	}
	prods, _ := si["products"].([]any)
	if len(prods) != 3 || prods[0] != "9003" || prods[2] != "9002" {
		t.Fatalf("商品图应按商品里的顺序展开、ID 为字符串：%v", si["products"])
	}
	if si["productId"] != "301" || si["productVersion"] != float64(3) {
		t.Fatalf("应记来源商品与版本：%v %v", si["productId"], si["productVersion"])
	}
	prod := si["product"].(map[string]any)
	if prod["name"] != "保温杯" || prod["sellingPoints"] != "24 小时保温" || prod["description"] != "双层真空" {
		t.Fatalf("商品文字来自快照：%v", prod)
	}
	picks := si["picks"].(map[string]any)
	cr := picks["creative"].(map[string]any)
	if cr["code"] != "cr_a" || cr["version"] != float64(3) || picks["hook"] != nil || picks["style"] == nil {
		t.Fatalf("picks 应钉 (code,version)、未选为 null：%v", picks)
	}
	if _, ok := cr["prompt"]; ok {
		t.Fatal("scene_input 不得含片段")
	}
	chars := si["characters"].([]any)
	if len(chars) != 2 || si["sourceVideo"] != nil {
		t.Fatalf("角色 / 源视频不对：%v %v", si["characters"], si["sourceVideo"])
	}
	c0, c1 := chars[0].(map[string]any), chars[1].(map[string]any)
	if c0["kind"] != "media" || c0["id"] != "9401" || c0["characterId"] != "401" || c0["name"] != "柚子君" ||
		c0["appearance"] != "圆脸" || c0["voiceText"] != "少年音" || c0["voiceMediaId"] != "9499" {
		t.Fatalf("角色库角色应快照成主图 media + 文字 + 参考音频：%v", c0)
	}
	if c1["kind"] != "asset" || c1["id"] != "201" || c1["characterId"] != nil {
		t.Fatalf("脚本角色资产原样（按槽内 order）：%v", c1)
	}
	if pins, _ := si["pins"].([]any); pins == nil || len(pins) != 0 {
		t.Fatalf("pins 建会话时应为空数组：%v", si["pins"])
	}
	var refs []ResolvedRef
	_ = json.Unmarshal(p.refs, &refs)
	if len(refs) != 3 || refs[0].Kind != RefMarketingProduct || refs[0].ID != "301" || refs[0].Label != "保温杯" ||
		refs[0].Order != 1 || refs[1].Kind != RefMarketingCharacter || refs[1].Label != "柚子君" || refs[1].Order != 1 ||
		refs[2].Slot != "character" || refs[2].Order != 2 {
		t.Fatalf("refs 应带 slot、名称且 order 规范成槽内 1..N：%+v", refs)
	}
	if strings.Contains(string(p.refs), "9499") || strings.Contains(string(p.refs), "9401") {
		t.Fatalf("首条消息 refs（会进模型上下文）不得含媒体 ID：%s", p.refs)
	}
}

// 旧请求形状（MKD-27）：三种各 40001 legacy_shape 点名，且不查归属、不取快照、不钉选项。
func TestCreateSession_旧形状三种拒绝(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*CreateSessionInput)
		key    string
		want   string
	}{
		{"marketing.product", func(in *CreateSessionInput) { in.Marketing.LegacyProduct = true }, "field", "marketing.product"},
		{"商品槽给媒体", func(in *CreateSessionInput) {
			in.Refs[0] = RefInput{Kind: RefMedia, ID: "101", Slot: "product", Order: 1}
		}, "slot", "product"},
		{"角色槽给媒体", func(in *CreateSessionInput) {
			in.Refs[2] = RefInput{Kind: RefMedia, ID: "101", Slot: "character", Order: 1}
		}, "slot", "character"},
	}
	for _, c := range cases {
		sc, refs, lib := &fakeScene{}, goodRefs(), &fakeLibrary{}
		in := marketingIn()
		c.mutate(&in)
		_, err := newMarketingSvcLib(sc, refs, lib).prepareMarketing(context.Background(), SessionActor{WorkspaceID: 1}, in)
		be := bizOf(t, err)
		if be.Code != errcode.InvalidArgument || be.Details[c.key] != c.want || be.Details["code"] != "legacy_shape" {
			t.Errorf("%s：期望 40001 %s=%s code=legacy_shape，得到 %v %v", c.name, c.key, c.want, be.Code, be.Details)
		}
		if refs.calls != 0 || lib.calls != 0 || sc.slotCalls != 0 || sc.resolveCalls != 0 {
			t.Errorf("%s：旧形状应在一切查库之前拒绝：refs=%d lib=%d slot=%d resolve=%d", c.name, refs.calls, lib.calls,
				sc.slotCalls, sc.resolveCalls)
		}
	}
	// 源视频槽给媒体是新形状，不在拒绝之列。
	in := marketingIn()
	in.Refs = []RefInput{{Kind: RefMedia, ID: "103", Slot: "source_video"}}
	if err := rejectLegacyShape(in); err != nil {
		t.Fatalf("源视频槽收媒体：%v", err)
	}
}

// 快照取不到（他人工作区 / 已删除 / 不存在）：40001 点名槽位 code=not_found，不钉选项。
// prepareMarketing 在 CreateSession 里先于建项目（EnsureProject）调用 —— 展开失败就不会留下空项目。
func TestCreateSession_展开失败不建项目(t *testing.T) {
	for _, c := range []struct {
		name string
		ref  RefInput
		idx  int
		slot string
	}{
		{"商品不存在", RefInput{Kind: RefMarketingProduct, ID: "399", Slot: "product"}, 0, "product"},
		{"角色不存在", RefInput{Kind: RefMarketingCharacter, ID: "499", Slot: "character", Order: 1}, 2, "character"},
	} {
		sc := &fakeScene{}
		in := marketingIn()
		in.Refs[c.idx] = c.ref
		_, err := newMarketingSvc(sc, goodRefs()).prepareMarketing(context.Background(), SessionActor{WorkspaceID: 1}, in)
		be := bizOf(t, err)
		if be.Code != errcode.InvalidArgument || be.Details["slot"] != c.slot || be.Details["code"] != "not_found" ||
			sc.resolveCalls != 0 {
			t.Errorf("%s：%v %v resolve=%d", c.name, be.Code, be.Details, sc.resolveCalls)
		}
	}
	// 别的工作区（fakeLibrary 只认 ws=1）：同一个 not_found。
	_, err := newMarketingSvc(&fakeScene{}, goodRefs()).prepareMarketing(context.Background(),
		SessionActor{WorkspaceID: 2}, CreateSessionInput{CreateProjectName: strp("x"),
			Refs: []RefInput{{Kind: RefMarketingProduct, ID: "301", Slot: "product"}}})
	if be := bizOf(t, err); be.Details["code"] != "not_found" {
		t.Fatalf("他人工作区的商品：%v", be.Details)
	}
	// 营销库没接入：500，而不是悄悄建一个没有商品的会话。
	s := &Service{p: ServicePorts{Scene: &fakeScene{}, Refs: goodRefs()}, now: time.Now}
	if _, err := s.prepareMarketing(context.Background(), SessionActor{WorkspaceID: 1}, marketingIn()); bizOf(t, err).Code != errcode.Internal {
		t.Fatalf("未接入营销库应 500：%v", err)
	}
	// 商品库引用不在槽位里：拒绝。
	in := marketingIn()
	in.Refs = append(in.Refs, RefInput{Kind: RefMarketingProduct, ID: "301"})
	if _, err := newMarketingSvc(&fakeScene{}, goodRefs()).prepareMarketing(context.Background(),
		SessionActor{WorkspaceID: 1}, in); bizOf(t, err).Code != errcode.InvalidArgument {
		t.Fatalf("无槽位的商品库引用应 400：%v", err)
	}
}

func TestNormalizeSlotOrder_混填与重复拒绝(t *testing.T) {
	mixed := []ResolvedRef{{ID: "1", Slot: "product", Order: 1}, {ID: "2", Slot: "product"}}
	if _, err := normalizeSlotOrder(mixed); err == nil || bizOf(t, err).Details["slot"] != "product" {
		t.Fatalf("混填 order 应拒绝并点名槽：%v", err)
	}
	dup := []ResolvedRef{{ID: "1", Slot: "product", Order: 1}, {ID: "2", Slot: "product", Order: 1}}
	if _, err := normalizeSlotOrder(dup); err == nil {
		t.Fatal("重复 order 应拒绝")
	}
	none := []ResolvedRef{{ID: "a", Slot: "character"}, {ID: "x"}, {ID: "b", Slot: "character"}}
	got, err := normalizeSlotOrder(none)
	if err != nil || got[0].ID != "x" || got[1].ID != "a" || got[1].Order != 1 || got[2].Order != 2 {
		t.Fatalf("都不带 order 应按出现顺序编号：%+v %v", got, err)
	}
}

func TestRejectMarketingFields_通用会话带营销字段拒绝(t *testing.T) {
	if err := rejectMarketingFields(CreateSessionInput{Marketing: &MarketingInput{}}); err == nil {
		t.Fatal("通用会话带 marketing 应拒绝")
	}
	if err := rejectMarketingFields(CreateSessionInput{Refs: []RefInput{{Kind: RefMedia, ID: "1", Slot: "product"}}}); err == nil {
		t.Fatal("通用会话带 slot 应拒绝")
	}
	if err := rejectMarketingFields(CreateSessionInput{Refs: []RefInput{{Kind: RefMedia, ID: "1"}}}); err != nil {
		t.Fatalf("普通引用不受影响：%v", err)
	}
	for _, k := range []string{RefMarketingProduct, RefMarketingCharacter} {
		if err := rejectMarketingFields(CreateSessionInput{Refs: []RefInput{{Kind: k, ID: "1"}}}); err == nil {
			t.Fatalf("通用会话带 %s 应拒绝", k)
		}
	}
}
