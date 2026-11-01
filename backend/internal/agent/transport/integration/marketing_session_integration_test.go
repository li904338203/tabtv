//go:build integration

// 建营销会话（MKT-T-17）的真库 + httptest 测试：真实路由、真实引用归属（媒体 / 脚本域）、
// 真实营销域（选项可见性与钉版本、槽位规则）。
//
// ⚠️ 失败路径断言「账本无新行 + 不建项目 / 会话」：全部校验必须发生在建项目与扣第一轮费用之前。
package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	marketingapp "github.com/li904338203/tabtv/backend/internal/marketing/app"
	mktdomain "github.com/li904338203/tabtv/backend/internal/marketing/domain"
	marketingrepo "github.com/li904338203/tabtv/backend/internal/marketing/repo"
	mcpapp "github.com/li904338203/tabtv/backend/internal/mcp/app"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	mediarepo "github.com/li904338203/tabtv/backend/internal/media/repo"
	scriptapp "github.com/li904338203/tabtv/backend/internal/script/app"
	scriptrepo "github.com/li904338203/tabtv/backend/internal/script/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

type mktEnv struct {
	*env
	mkt      *marketingrepo.Repo
	lib      *marketingapp.Library
	media    *mediaapp.Service
	products []int64 // 本工作区的 21 张图
	video    int64   // 本工作区的视频
	voice    int64   // 本工作区的音频（角色 C 的参考音频）
	char     int64   // 本工作区的角色资产
	sceneAst int64   // 本工作区的场景资产
	otherImg int64   // 别的工作区的图
	otherAst int64   // 别的工作区的角色资产
	other    int64   // 别的工作区
	// 商品库 / 角色库（MKT-T-37）：P = 3 张图（products[2], [0], [1]，[2] 为主图）；C = 2 张图 + 参考音频。
	product   int64
	character int64
	otherProd int64 // 别的工作区的商品
	otherChar int64 // 别的工作区的角色
	optIDs    map[string]int64
}

// refChecker 把仓储查询接成媒体引用检查器（同 cmd/api 的 mediaRefChecker）。
type refChecker struct {
	kind string
	fn   func(context.Context, int64) ([]int64, error)
}

func (c refChecker) Kind() string { return c.kind }
func (c refChecker) References(ctx context.Context, id int64) ([]int64, error) {
	return c.fn(ctx, id)
}

// newMktEnv 起一套营销会话环境；marketing=false 表示营销总开关关闭（Scene 端口不接入）。
func newMktEnv(t *testing.T, marketing bool) *mktEnv {
	t.Helper()
	m := &mktEnv{optIDs: map[string]int64{}}
	m.env = newEnvWith(t, true, func(e *env, p *agentapp.ServicePorts) {
		m.mkt = marketingrepo.New(e.pool)
		m.media = mediaapp.New(mediarepo.New(e.pool), nil, e.next)
		// 删除保护与 cmd/api 同一组检查器（营销会话快照、商品库、角色库）。
		m.media.AddReferenceChecker(refChecker{mediaapp.UsageAgentMarketing, repo.New(e.pool).MarketingMediaReferences})
		m.media.AddReferenceChecker(refChecker{mediaapp.UsageMarketingProduct, m.mkt.ProductMediaReferences})
		m.media.AddReferenceChecker(refChecker{mediaapp.UsageMarketingCharacter, m.mkt.CharacterMediaReferences})
		m.lib = wiring.NewMarketingLibrary(wiring.MarketingLibraryInput{Repo: m.mkt, Media: m.media, NextID: e.next})
		p.Refs = wiring.AgentRefs{Media: m.media, Script: scriptapp.New(scriptrepo.New(e.pool), e.next)}
		if marketing {
			p.Scene = wiring.AgentScene{Svc: marketingapp.NewService(m.mkt, nil, nil,
				marketingapp.Settings{MaxProductImages: 10, MaxVariants: 4, AgentEnabled: true})}
			p.Library = wiring.AgentSceneLibrary{Lib: m.lib}
		}
		p.SlotDecls = wiring.AgentSkillSlots{} // 同 wiring.NewAgentService：技能下发前规范化（MKD-26）
	})
	ctx := context.Background()
	m.other = m.next()
	media := func(ws int64, ct string) int64 {
		id := m.next()
		if _, err := m.pool.Exec(ctx, `
			insert into media_object (id, workspace_id, bucket, object_key, content_type, size_bytes,
			                          purpose, created_by_user_id)
			values ($1,$2,'seeaitv',$3,$4,1,'upload_reference',$5)`,
			id, ws, "t/"+strconv.FormatInt(id, 10), ct, m.owner); err != nil {
			t.Fatal(err)
		}
		return id
	}
	asset := func(ws int64, typ, name string) int64 {
		id := m.next()
		if _, err := m.pool.Exec(ctx, `
			insert into script_asset (id, workspace_id, asset_type, name, status)
			values ($1,$2,$3,$4,'ready')`, id, ws, typ, name); err != nil {
			t.Fatal(err)
		}
		return id
	}
	for i := 0; i < 21; i++ {
		m.products = append(m.products, media(m.ws, "image/png"))
	}
	m.video = media(m.ws, "video/mp4")
	m.voice = media(m.ws, "audio/mpeg")
	m.char = asset(m.ws, "character", "小美")
	m.sceneAst = asset(m.ws, "scene", "直播间")
	m.otherImg = media(m.other, "image/png")
	m.otherAst = asset(m.other, "character", "别人的角色")
	me := marketingapp.LibraryActor{WorkspaceID: m.ws, UserID: m.owner}
	them := marketingapp.LibraryActor{WorkspaceID: m.other, UserID: m.owner}
	p, err := m.lib.CreateProduct(ctx, me, mktdomain.ProductInput{Name: "保温杯", SellingPoints: "24 小时保温",
		Description: "双层真空\n316 不锈钢", ImageMediaIDs: []int64{m.products[2], m.products[0], m.products[1]}})
	if err != nil {
		t.Fatal(err)
	}
	m.product = p.ID
	c, err := m.lib.CreateCharacter(ctx, me, mktdomain.CharacterInput{Name: "柚子君",
		ImageMediaIDs: []int64{m.products[3], m.products[4]}, Appearance: "圆脸柚子拟人，绿色叶子发型",
		VoiceText: "活泼少年音", VoiceMediaID: m.voice})
	if err != nil {
		t.Fatal(err)
	}
	m.character = c.ID
	op, err := m.lib.CreateProduct(ctx, them, mktdomain.ProductInput{Name: "别人的商品", ImageMediaIDs: []int64{m.otherImg}})
	if err != nil {
		t.Fatal(err)
	}
	m.otherProd = op.ID
	oc, err := m.lib.CreateCharacter(ctx, them, mktdomain.CharacterInput{Name: "别人的角色", ImageMediaIDs: []int64{m.otherImg}})
	if err != nil {
		t.Fatal(err)
	}
	m.otherChar = oc.ID
	for _, o := range []struct {
		kind mktdomain.Kind
		code string
	}{{mktdomain.KindCreative, "cr_drop"}, {mktdomain.KindHook, "hk_sound"}, {mktdomain.KindStyle, "st_bright"}} {
		opt := mktdomain.Option{Kind: o.kind, Code: o.code, Name: "名称-" + o.code, CategoryCode: "cat",
			Prompt: "片段原文-" + o.code + "-v1", OutputTypes: []string{"video"}, CoverMediaIDs: []int64{}, Tags: []string{}}
		row, err := m.mkt.InsertOption(ctx, m.next(), m.next(), 1, opt, snapOf(opt, 1))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := m.mkt.SetOptionEnabled(ctx, row.ID, true, 1); err != nil {
			t.Fatal(err)
		}
		m.optIDs[o.code] = row.ID
	}
	return m
}

func snapOf(o mktdomain.Option, v int) json.RawMessage {
	b, _ := json.Marshal(mktdomain.Snapshot{Pick: mktdomain.Pick{Kind: o.Kind, Code: o.Code, Version: v},
		Name: o.Name, Prompt: o.Prompt})
	return b
}

func idS(v int64) string { return strconv.FormatInt(v, 10) }

func ref(kind string, id int64, slot string, order int) map[string]any {
	r := map[string]any{"kind": kind, "id": idS(id), "slot": slot}
	if order > 0 {
		r["order"] = order
	}
	return r
}

// body 是一份合法的营销建会话请求：商品 P + 角色（脚本资产「小美」排第 2、角色库 C 排第 1）+ 创意与风格。
func (m *mktEnv) body() map[string]any {
	return map[string]any{
		"createProject": map[string]any{"name": "保温杯营销"},
		"scene":         "marketing",
		"message": map[string]any{"text": "做一条 15 秒的带货视频", "refs": []any{
			ref("marketing_product", m.product, "product", 0),
			ref("asset", m.char, "character", 2),
			ref("marketing_character", m.character, "character", 1),
		}},
		"marketing": map[string]any{
			"picks": map[string]any{"creative": "cr_drop", "hook": nil, "style": "st_bright"},
		},
	}
}

func (m *mktEnv) sceneInput(sid any) agentapp.SceneInput {
	m.t.Helper()
	var si agentapp.SceneInput
	if err := m.pool.QueryRow(context.Background(), `select scene_input from agent_session where id = $1`, sid).
		Scan(&si); err != nil {
		m.t.Fatal(err)
	}
	return si
}

// MKT-AC-43：商品 P（3 张）+ 工作区角色 C + 脚本资产角色建会话，sceneInput 各字段与 P / C 一致。
func TestCreateMarketingSession_商品库快照(t *testing.T) {
	m := newMktEnv(t, true)
	ctx := context.Background()
	r := m.do("POST", "/agent/sessions", m.owner, "k-mkt-0001", m.body())
	if r.status != http.StatusCreated {
		t.Fatalf("合法请求应 201：%d %v", r.status, r.body)
	}
	sid, pid := r.body["sessionId"].(string), r.body["projectId"].(string)

	var origin *string
	if err := m.pool.QueryRow(ctx, `select origin from project where id = $1`, pid).Scan(&origin); err != nil {
		t.Fatal(err)
	}
	if origin == nil || *origin != "marketing" {
		t.Fatalf("项目来源应为 marketing：%v", origin)
	}
	var scene string
	if err := m.pool.QueryRow(ctx, `select scene from agent_session where id = $1`, sid).Scan(&scene); err != nil {
		t.Fatal(err)
	}
	si := m.sceneInput(sid)
	if scene != "marketing" || si.ProductID != idS(m.product) || si.ProductVersion != 1 ||
		len(si.Products) != 3 || si.Products[0] != idS(m.products[2]) || si.Products[1] != idS(m.products[0]) ||
		si.Product.Name != "保温杯" || si.Product.SellingPoints != "24 小时保温" ||
		si.Product.Description != "双层真空\n316 不锈钢" ||
		si.Picks.Creative == nil || si.Picks.Creative.Code != "cr_drop" || si.Picks.Creative.Version != 1 ||
		si.Picks.Hook != nil || si.Picks.Style == nil || si.Picks.Style.Version != 1 {
		t.Fatalf("scene_input 商品部分不对：%s %+v", scene, si)
	}
	if len(si.Characters) != 2 {
		t.Fatalf("角色应 2 个：%+v", si.Characters)
	}
	c0, c1 := si.Characters[0], si.Characters[1]
	if c0.Kind != "media" || c0.ID != idS(m.products[3]) || c0.CharacterID != idS(m.character) || c0.Name != "柚子君" ||
		c0.Appearance != "圆脸柚子拟人，绿色叶子发型" || c0.VoiceText != "活泼少年音" || c0.VoiceMediaID != idS(m.voice) {
		t.Fatalf("角色库角色应快照成主图 media + 文字 + 参考音频：%+v", c0)
	}
	if c1.Kind != "asset" || c1.ID != idS(m.char) || c1.CharacterID != "" {
		t.Fatalf("脚本资产角色原样（按 order 排第 2）：%+v", c1)
	}
	var refs []agentapp.ResolvedRef
	if err := m.pool.QueryRow(ctx, `select refs from agent_message where session_id = $1`, sid).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if len(refs) != 3 || refs[0].Kind != "marketing_product" || refs[0].Slot != "product" || refs[0].Order != 1 ||
		refs[0].Label != "保温杯" || refs[1].Kind != "marketing_character" || refs[1].Label != "柚子君" ||
		refs[2].Slot != "character" || refs[2].Order != 2 || refs[2].Label != "小美" {
		t.Fatalf("首条消息 refs 应带 slot / order / 名称：%+v", refs)
	}
	if n := m.count(`select count(*) from agent_turn where session_id = $1`, sid); n != 1 {
		t.Fatalf("应建 1 个轮次：%d", n)
	}
	if n := m.count(`select count(*) from outbox_message where aggregate_type = 'agent' and aggregate_id = $1`, sid); n != 1 {
		t.Fatalf("应写 1 条 outbox：%d", n)
	}

	// 详情：scene + sceneInput（ID 为字符串；不含 URL、不含片段原文）。
	d := m.do("GET", "/agent/sessions/"+sid, m.owner, "", nil)
	if d.status != http.StatusOK || d.body["scene"] != "marketing" {
		t.Fatalf("详情应带 scene：%d %v", d.status, d.body["scene"])
	}
	raw, _ := json.Marshal(d.body["sceneInput"])
	for _, bad := range []string{"http", "片段原文", "prompt", "objectKey"} {
		if strings.Contains(string(raw), bad) {
			t.Fatalf("sceneInput 不得含 %q：%s", bad, raw)
		}
	}
	for _, want := range []string{`"name":"名称-cr_drop"`, `"productId":"` + idS(m.product) + `"`,
		`"characterId":"` + idS(m.character) + `"`, `"description":"双层真空`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("sceneInput 缺 %s：%s", want, raw)
		}
	}

	// 钉版本（MKT-AC-08）：管理端改了所选创意的片段，会话里钉住的 version 不变；新会话钉新版本。
	opt := mktdomain.Option{Kind: mktdomain.KindCreative, Code: "cr_drop", Name: "名称-cr_drop", CategoryCode: "cat",
		Prompt: "片段原文-v2", OutputTypes: []string{"video"}, CoverMediaIDs: []int64{}, Tags: []string{}}
	if _, err := m.mkt.UpdateOption(ctx, m.optIDs["cr_drop"], 1, m.next(), 1, opt, snapOf(opt, 2)); err != nil {
		t.Fatal(err)
	}
	if si = m.sceneInput(sid); si.Picks.Creative.Version != 1 {
		t.Fatalf("改了选项后已建会话的 version 应不变：%d", si.Picks.Creative.Version)
	}
	r2 := m.do("POST", "/agent/sessions", m.owner, "k-mkt-0002", m.body())
	var v2 int
	if err := m.pool.QueryRow(ctx, `select (scene_input->'picks'->'creative'->>'version')::int from agent_session where id = $1`,
		r2.body["sessionId"]).Scan(&v2); err != nil || v2 != 2 {
		t.Fatalf("新会话应钉当前版本 2：%d %v", v2, err)
	}
}

// MKT-AC-35：建会话后改 P 的主图与文字（以及 C）—— 已建会话的 sceneInput 一字不变。
func TestCreateMarketingSession_编辑商品不影响会话(t *testing.T) {
	m := newMktEnv(t, true)
	ctx := context.Background()
	r := m.do("POST", "/agent/sessions", m.owner, "k-mkt-edit", m.body())
	if r.status != http.StatusCreated {
		t.Fatalf("建会话：%d %v", r.status, r.body)
	}
	sid := r.body["sessionId"]
	var before string
	_ = m.pool.QueryRow(ctx, `select scene_input::text from agent_session where id = $1`, sid).Scan(&before)
	me := marketingapp.LibraryActor{WorkspaceID: m.ws, UserID: m.owner}
	if _, err := m.lib.UpdateProduct(ctx, me, m.product, 1, mktdomain.ProductInput{Name: "新名字", SellingPoints: "新卖点",
		Description: "新描述", ImageMediaIDs: []int64{m.products[5], m.products[2]}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.lib.UpdateCharacter(ctx, me, m.character, 1, mktdomain.CharacterInput{Name: "柚子君二代",
		ImageMediaIDs: []int64{m.products[6]}, Appearance: "方脸"}); err != nil {
		t.Fatal(err)
	}
	var after string
	_ = m.pool.QueryRow(ctx, `select scene_input::text from agent_session where id = $1`, sid).Scan(&after)
	if after != before {
		t.Fatalf("改了商品 / 角色后已建会话的快照应不变\nbefore %s\nafter  %s", before, after)
	}
	si := m.sceneInput(sid)
	if si.Products[0] != idS(m.products[2]) || si.Product.Name != "保温杯" || si.Characters[0].ID != idS(m.products[3]) {
		t.Fatalf("快照仍是旧图旧文字：%+v", si)
	}
	// 新会话拿到的是新内容、新版本。
	r2 := m.do("POST", "/agent/sessions", m.owner, "k-mkt-edit-2", m.body())
	si2 := m.sceneInput(r2.body["sessionId"])
	if si2.ProductVersion != 2 || si2.Products[0] != idS(m.products[5]) || si2.Product.Name != "新名字" ||
		si2.Characters[0].Name != "柚子君二代" || si2.Characters[0].VoiceMediaID != "" {
		t.Fatalf("新会话应快照当前内容：%+v", si2)
	}
}

// MKT-AC-34：删除 P / C 后，已建会话的快照不变，快照引用的图与参考音频被会话保护着（删不掉、回收器也不收）；
// 规划 / 换图 / 生成读的都是快照（执行侧见 agent/repo/integration 的 TestMarketingSession_商品库会话生成不发参考音频）。
func TestCreateMarketingSession_删除商品后照常生成(t *testing.T) {
	m := newMktEnv(t, true)
	ctx := context.Background()
	r := m.do("POST", "/agent/sessions", m.owner, "k-mkt-del", m.body())
	if r.status != http.StatusCreated {
		t.Fatalf("建会话：%d %v", r.status, r.body)
	}
	sid := r.body["sessionId"]
	before := m.sceneInput(sid)
	me := marketingapp.LibraryActor{WorkspaceID: m.ws, UserID: m.owner}
	if err := m.lib.DeleteProduct(ctx, me, m.product); err != nil {
		t.Fatal(err)
	}
	if err := m.lib.DeleteCharacter(ctx, me, m.character); err != nil {
		t.Fatal(err)
	}
	after := m.sceneInput(sid)
	if after.ProductID != before.ProductID || len(after.Products) != 3 || after.Characters[0].VoiceMediaID != idS(m.voice) {
		t.Fatalf("删除商品 / 角色后会话快照应不变：%+v", after)
	}
	// 商品 / 角色删了，它们自有的上传媒体被尽力清理 —— 但快照引用的那几张由会话保护，仍在。
	for name, id := range map[string]int64{"商品图#1": m.products[2], "商品图#3": m.products[1],
		"角色主图": m.products[3], "参考音频": m.voice} {
		var status string
		if err := m.pool.QueryRow(ctx, `select status from media_object where id = $1`, id).Scan(&status); err != nil ||
			status != "active" {
			t.Errorf("%s 被会话快照引用，应保留：%s %v", name, status, err)
		}
	}
	// 会话快照里的参考音频：直接删除被拒，文案点名「营销会话」。
	err := m.media.Delete(ctx, m.ws, m.voice)
	if err == nil || !strings.Contains(err.Error(), "营销会话") {
		t.Fatalf("快照里的参考音频删除应被拒：%v", err)
	}
	// 没被会话引用的角色第 2 张图随角色一起被清理。
	var status string
	_ = m.pool.QueryRow(ctx, `select status from media_object where id = $1`, m.products[4]).Scan(&status)
	if status != "deleted" {
		t.Errorf("未被会话引用的角色图应随角色清理：%s", status)
	}
	// 再用已删除的商品建会话：not_found 点名商品槽。
	r2 := m.do("POST", "/agent/sessions", m.owner, "k-mkt-del-2", m.body())
	if r2.status != http.StatusBadRequest || detail(r2, "slot") != "product" || detail(r2, "code") != "not_found" {
		t.Fatalf("已删除商品应 400 not_found：%d %v", r2.status, r2.body)
	}
}

func TestCreateMarketingSession_幂等重放(t *testing.T) {
	m := newMktEnv(t, true)
	a := m.do("POST", "/agent/sessions", m.owner, "k-mkt-replay", m.body())
	b := m.do("POST", "/agent/sessions", m.owner, "k-mkt-replay", m.body())
	if a.status != http.StatusCreated || b.status != http.StatusOK || b.body["replayed"] != true ||
		a.body["sessionId"] != b.body["sessionId"] || a.body["projectId"] != b.body["projectId"] {
		t.Fatalf("同键重放应返回同一会话：%v / %v", a.body, b.body)
	}
	if n := m.count(`select count(*) from agent_session where scene = 'marketing'`); n != 1 {
		t.Fatalf("应只建 1 个营销会话：%d", n)
	}
	if n := m.count(`select count(*) from project where origin = 'marketing'`); n != 1 {
		t.Fatalf("应只建 1 个营销项目：%d", n)
	}
}

func TestCreateMarketingSession_越权商品与角色被拒(t *testing.T) {
	m := newMktEnv(t, true)
	for i, c := range []struct {
		name string
		idx  int
		ref  map[string]any
		slot string
	}{
		{"别的工作区的商品", 0, ref("marketing_product", m.otherProd, "product", 0), "product"},
		{"别的工作区的角色", 2, ref("marketing_character", m.otherChar, "character", 1), "character"},
		{"别的工作区的角色资产", 1, ref("asset", m.otherAst, "character", 2), "character"},
		{"不存在的商品", 0, ref("marketing_product", m.next(), "product", 0), "product"},
	} {
		b := m.body()
		msg := b["message"].(map[string]any)
		msg["refs"].([]any)[c.idx] = c.ref
		r := m.do("POST", "/agent/sessions", m.owner, "k-mkt-xws-"+strconv.Itoa(i), b)
		if r.status != http.StatusBadRequest || r.code() != int(errcode.InvalidArgument) || detail(r, "slot") != c.slot ||
			detail(r, "code") != "not_found" {
			t.Errorf("%s 应 400/40001 点名 %s not_found：%d %v", c.name, c.slot, r.status, r.body)
		}
	}
	if n := m.count(`select count(*) from agent_session`); n != 0 {
		t.Fatalf("越权时不应建会话：%d", n)
	}
	if n := m.count(`select count(*) from project where origin = 'marketing'`); n != 0 {
		t.Fatalf("越权时不应建项目：%d", n)
	}
}

func TestCreateMarketingSession_失败不扣费(t *testing.T) {
	m := newMktEnv(t, true)
	ctx := context.Background()
	// 下线一个 Hook：新会话再选它 → 42215 disabled（MKT-AC-11 新会话部分）。
	if _, _, err := m.mkt.SetOptionEnabled(ctx, m.optIDs["hk_sound"], false, 1); err != nil {
		t.Fatal(err)
	}
	// 一个只属通用场景的技能 + 一个声明了源视频必填的营销技能。
	r := repo.New(m.pool)
	if _, err := r.InsertSkill(ctx, repo.Skill{ID: m.next(), Code: "gen_only", Name: "通用技能", IsEnabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.InsertSkill(ctx, repo.Skill{ID: m.next(), Code: "mk_remix", Name: "裂变", IsEnabled: true,
		Scenes:     json.RawMessage(`["marketing"]`),
		InputSlots: json.RawMessage(`[{"key":"source_video","label":"源视频","accept":["video"],"required":true,"maxCount":1}]`),
	}); err != nil {
		t.Fatal(err)
	}
	projects := m.count(`select count(*) from project`)
	refs := func(b map[string]any, list ...any) { b["message"].(map[string]any)["refs"] = list }

	cases := []struct {
		name   string
		mutate func(b map[string]any)
		status int
		code   errcode.Code
		check  func(r resp) bool
	}{
		{"缺商品槽", func(b map[string]any) { refs(b, ref("asset", m.char, "character", 0)) },
			400, errcode.InvalidArgument, func(r resp) bool { return detail(r, "slot") == "product" }},
		{"两个商品", func(b map[string]any) {
			refs(b, ref("marketing_product", m.product, "product", 1), ref("marketing_product", m.otherProd, "product", 2))
		}, 400, errcode.InvalidArgument, func(r resp) bool {
			return detail(r, "slot") == "product" && detail(r, "code") == "too_many"
		}},
		{"角色槽给了场景资产", func(b map[string]any) {
			refs(b, ref("marketing_product", m.product, "product", 0), ref("asset", m.sceneAst, "character", 0))
		}, 400, errcode.InvalidArgument, func(r resp) bool { return detail(r, "slot") == "character" }},
		{"三个角色", func(b map[string]any) {
			refs(b, ref("marketing_product", m.product, "product", 0), ref("asset", m.char, "character", 0),
				ref("marketing_character", m.character, "character", 0), ref("asset", m.otherAst, "character", 0))
		}, 400, errcode.InvalidArgument, func(r resp) bool { return detail(r, "slot") == "character" }},
		// 旧形状三种（MKD-27）：点名并给 legacy_shape。
		{"旧形状 marketing.product", func(b map[string]any) {
			b["marketing"].(map[string]any)["product"] = map[string]any{"name": "保温杯", "sellingPoints": "24 小时保温"}
		}, 400, errcode.InvalidArgument, func(r resp) bool {
			return detail(r, "field") == "marketing.product" && detail(r, "code") == "legacy_shape"
		}},
		{"旧形状 商品槽给媒体", func(b map[string]any) {
			refs(b, ref("media", m.products[1], "product", 2), ref("media", m.products[0], "product", 1))
		}, 400, errcode.InvalidArgument, func(r resp) bool {
			return detail(r, "slot") == "product" && detail(r, "code") == "legacy_shape"
		}},
		{"旧形状 角色槽给媒体", func(b map[string]any) {
			refs(b, ref("marketing_product", m.product, "product", 0), ref("media", m.products[5], "character", 0))
		}, 400, errcode.InvalidArgument, func(r resp) bool {
			return detail(r, "slot") == "character" && detail(r, "code") == "legacy_shape"
		}},
		{"不带 createProject", func(b map[string]any) {
			delete(b, "createProject")
			b["projectId"] = idS(m.project)
		}, 400, errcode.InvalidArgument, func(r resp) bool { return detail(r, "field") == "createProject" }},
		{"已下线的 Hook", func(b map[string]any) {
			b["marketing"].(map[string]any)["picks"] = map[string]any{"creative": "cr_drop", "hook": "hk_sound"}
		}, 422, errcode.MarketingOptionUnavailable, func(r resp) bool {
			return detail(r, "reason") == "disabled" && detail(r, "kind") == "hook"
		}},
		{"不存在的风格", func(b map[string]any) {
			b["marketing"].(map[string]any)["picks"] = map[string]any{"style": "st_nope"}
		}, 422, errcode.MarketingOptionUnavailable, func(r resp) bool { return detail(r, "reason") == "not_found" }},
		{"技能不属营销场景", func(b map[string]any) { b["skillCode"] = "gen_only" },
			400, errcode.InvalidArgument, func(r resp) bool { return detail(r, "field") == "skillCode" }},
		{"营销技能缺源视频", func(b map[string]any) {
			b["skillCode"] = "mk_remix"
			refs(b)
		}, 400, errcode.InvalidArgument, func(r resp) bool { return detail(r, "slot") == "source_video" }},
	}
	for i, c := range cases {
		b := m.body()
		c.mutate(b)
		r := m.do("POST", "/agent/sessions", m.owner, "k-mkt-fail-"+strconv.Itoa(i), b)
		if r.status != c.status || r.code() != int(c.code) || !c.check(r) {
			t.Errorf("%s：期望 %d/%d，得到 %d %v", c.name, c.status, c.code, r.status, r.body)
		}
	}
	if n := m.count(`select count(*) from credit_transaction`); n != 0 {
		t.Fatalf("失败路径不得扣费：账本 %d 行", n)
	}
	if n := m.count(`select count(*) from agent_session`); n != 0 {
		t.Fatalf("失败路径不得建会话：%d", n)
	}
	if n := m.count(`select count(*) from project`); n != projects {
		t.Fatalf("失败路径不得建项目：%d → %d", projects, n)
	}

	// 源视频填上就通过（技能声明里没有商品槽）。
	b := m.body()
	b["skillCode"] = "mk_remix"
	refs(b, ref("media", m.video, "source_video", 0))
	if r := m.do("POST", "/agent/sessions", m.owner, "k-mkt-remix-ok", b); r.status != http.StatusCreated {
		t.Fatalf("裂变技能填源视频应通过：%d %v", r.status, r.body)
	}
	var sv agentapp.SceneRef
	if err := m.pool.QueryRow(ctx, `select scene_input->'sourceVideo' from agent_session where scene = 'marketing'`).
		Scan(&sv); err != nil || sv.ID != idS(m.video) {
		t.Fatalf("sourceVideo 应落库：%+v %v", sv, err)
	}
}

func TestCreateMarketingSession_营销开关关闭被拒(t *testing.T) {
	m := newMktEnv(t, false)
	r := m.do("POST", "/agent/sessions", m.owner, "k-mkt-off", m.body())
	if r.status != http.StatusBadRequest || detail(r, "field") != "scene" {
		t.Fatalf("营销关闭时 scene=marketing 应 400：%d %v", r.status, r.body)
	}
	if n := m.count(`select count(*) from project where origin = 'marketing'`); n != 0 {
		t.Fatalf("不应建项目：%d", n)
	}
}

func TestCreateSession_通用会话scene默认general(t *testing.T) {
	m := newMktEnv(t, true)
	ctx := context.Background()
	sid := m.createSession("k-general-01")
	var scene, si string
	if err := m.pool.QueryRow(ctx, `select scene, scene_input::text from agent_session where id = $1`, sid).
		Scan(&scene, &si); err != nil {
		t.Fatal(err)
	}
	if scene != "general" || si != "{}" {
		t.Fatalf("通用会话应 scene=general、scene_input={}：%s %s", scene, si)
	}
	d := m.do("GET", "/agent/sessions/"+idS(sid), m.owner, "", nil)
	if d.body["scene"] != "general" || d.body["sceneInput"] != nil {
		t.Fatalf("通用会话详情应 scene=general、sceneInput=null：%v %v", d.body["scene"], d.body["sceneInput"])
	}
	// 通用会话带营销字段一律 400，不静默忽略。
	for i, b := range []map[string]any{
		{"projectId": idS(m.project), "message": map[string]any{"text": "x",
			"refs": []any{ref("media", m.products[0], "product", 1)}}},
		{"projectId": idS(m.project), "message": map[string]any{"text": "x"},
			"marketing": map[string]any{"picks": map[string]any{"creative": "cr_drop"}}},
		{"projectId": idS(m.project), "scene": "writing", "message": map[string]any{"text": "x"}},
		{"projectId": idS(m.project), "message": map[string]any{"text": "x",
			"refs": []any{map[string]any{"kind": "marketing_product", "id": idS(m.product)}}}},
	} {
		if r := m.do("POST", "/agent/sessions", m.owner, "k-general-bad-"+strconv.Itoa(i), b); r.status != http.StatusBadRequest {
			t.Errorf("第 %d 个应 400：%d %v", i, r.status, r.body)
		}
	}
	// 发消息带 slot 也拒绝（slot 只用于建营销会话）。
	if r := m.do("POST", "/agent/sessions/"+idS(sid)+"/messages", m.owner, "k-general-msg", map[string]any{
		"text": "x", "refs": []any{ref("media", m.products[0], "product", 1)}}); r.status != http.StatusBadRequest {
		t.Errorf("发消息带 slot 应 400：%d %v", r.status, r.body)
	}
	// PATCH 带 scene 一律 400：场景建后不可改（B-18）。
	if r := m.do("PATCH", "/agent/sessions/"+idS(sid), m.owner, "", map[string]any{"scene": "marketing"}); r.status != http.StatusBadRequest {
		t.Errorf("PATCH 带 scene 应 400：%d %v", r.status, r.body)
	}
	// MCP 委托建会话一律 general（MKD-22）。
	w, err := wiring.MCPAgentSessions{Svc: m.svc}.Start(ctx, mcpapp.Principal{UserID: m.owner, WorkspaceID: m.ws,
		Role: "owner", PayerAccountID: m.payer}, mcpapp.AgentStartInput{ProjectID: m.project, Text: "MCP 来的",
		IdempotencyKey: "k-mcp-0001"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.pool.QueryRow(ctx, `select scene from agent_session where id = $1`, w.SessionID).Scan(&scene); err != nil || scene != "general" {
		t.Fatalf("MCP 建会话应为 general：%s %v", scene, err)
	}
}

func detail(r resp, key string) any {
	d, _ := r.body["details"].(map[string]any)
	if d == nil {
		return nil
	}
	return d[key]
}
