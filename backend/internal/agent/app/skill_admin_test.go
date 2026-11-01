package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// fakeSkillStore 是内存版技能仓储（只覆盖管理端用到的方法与语义）。
type fakeSkillStore struct {
	skills map[int64]repo.Skill
	refs   map[int64]repo.SkillReference
}

func newFakeSkillStore() *fakeSkillStore {
	return &fakeSkillStore{skills: map[int64]repo.Skill{}, refs: map[int64]repo.SkillReference{}}
}

func (f *fakeSkillStore) ListAllSkills(context.Context) ([]repo.AdminSkill, error) {
	var out []repo.AdminSkill
	for _, s := range f.skills {
		out = append(out, repo.AdminSkill{Skill: s})
	}
	return out, nil
}

func (f *fakeSkillStore) GetSkillByID(_ context.Context, id int64) (repo.Skill, error) {
	s, ok := f.skills[id]
	if !ok {
		return repo.Skill{}, repo.ErrNotFound
	}
	return s, nil
}

func (f *fakeSkillStore) ListSkillReferences(_ context.Context, id int64) ([]repo.SkillReference, error) {
	var out []repo.SkillReference
	for _, r := range f.refs {
		if r.SkillID == id {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeSkillStore) InsertSkill(_ context.Context, s repo.Skill) (repo.Skill, error) {
	for _, x := range f.skills {
		if x.Code == s.Code {
			return repo.Skill{}, repo.ErrDuplicate
		}
	}
	s.Version = 1
	f.skills[s.ID] = s
	return s, nil
}

func (f *fakeSkillStore) UpdateSkill(_ context.Context, s repo.Skill, v int) (repo.Skill, error) {
	cur, ok := f.skills[s.ID]
	if !ok {
		return repo.Skill{}, repo.ErrNotFound
	}
	if cur.Version != v {
		return repo.Skill{}, repo.ErrStale
	}
	s.Code, s.IsEnabled, s.Version = cur.Code, cur.IsEnabled, v+1
	f.skills[s.ID] = s
	return s, nil
}

func (f *fakeSkillStore) SetSkillEnabled(_ context.Context, id int64, en bool) (repo.Skill, bool, error) {
	cur, ok := f.skills[id]
	if !ok {
		return repo.Skill{}, false, repo.ErrNotFound
	}
	if cur.IsEnabled == en {
		return cur, false, nil
	}
	cur.IsEnabled, cur.Version = en, cur.Version+1
	f.skills[id] = cur
	return cur, true, nil
}

func (f *fakeSkillStore) SaveSkillReference(_ context.Context, ref repo.SkillReference, _ bool, v int) (int, error) {
	cur := f.skills[ref.SkillID]
	if cur.Version != v {
		return 0, repo.ErrStale
	}
	cur.Version++
	f.skills[ref.SkillID] = cur
	f.refs[ref.ID] = ref
	return cur.Version, nil
}

func (f *fakeSkillStore) DeleteSkillReference(_ context.Context, sid, rid int64, v int) (int, error) {
	cur := f.skills[sid]
	if cur.Version != v {
		return 0, repo.ErrStale
	}
	cur.Version++
	f.skills[sid] = cur
	delete(f.refs, rid)
	return cur.Version, nil
}

type fakeSkillModels []SkillModelInfo

func (m fakeSkillModels) SkillModels(context.Context) ([]SkillModelInfo, error) { return m, nil }

// strictRender 与 catalog/domain.RenderPrompt 同语义的最小实现：缺变量即报错。
type strictRender struct{}

func (strictRender) RenderSkill(content string, vars map[string]string) (string, error) {
	rest := content
	for {
		i := strings.Index(rest, "{{")
		if i < 0 {
			return content, nil
		}
		j := strings.Index(rest[i:], "}}")
		if j < 0 {
			return "", errors.New("占位符未闭合")
		}
		name := strings.TrimSpace(rest[i+2 : i+j])
		if _, ok := vars[name]; !ok {
			return "", errors.New("渲染缺少变量：" + name)
		}
		rest = rest[i+j+2:]
	}
}

type memAudit struct{ entries []audit.Entry }

func (m *memAudit) Write(_ context.Context, e audit.Entry) { m.entries = append(m.entries, e) }

func newTestSkillAdmin() (*SkillAdmin, *fakeSkillStore, *memAudit) {
	st := newFakeSkillStore()
	id := int64(100)
	a := NewSkillAdmin(st, fakeSkillModels{
		{Code: "chat_ok", Domain: "text", AgentCapable: true},
		{Code: "chat_plain", Domain: "text"},
		{Code: "kling", Domain: "video"},
		{Code: "flux", Domain: "image"},
	}, strictRender{}, func() int64 { id++; return id })
	au := &memAudit{}
	a.SetAudit(au)
	return a, st, au
}

func goodInput() SkillInput {
	return SkillInput{Code: "story_topic", Name: "故事选题", Category: "创作", Body: "当前 {{state}}",
		Steps: []domain.SkillStep{{No: 1, Name: "了解"}}, Tools: []string{"read_canvas"},
		Bindings: domain.SkillBindings{Reasoning: "chat_ok", Video: "kling"}}
}

func bizCode(err error) errcode.Code {
	var be *httpx.BizError
	if errors.As(err, &be) {
		return be.Code
	}
	return 0
}

func TestSkillAdmin_新建一律停用且进审计(t *testing.T) {
	a, _, au := newTestSkillAdmin()
	s, err := a.Create(context.Background(), 7, goodInput())
	if err != nil {
		t.Fatalf("新建失败：%v", err)
	}
	if s.IsEnabled {
		t.Fatal("新建的技能不应直接启用")
	}
	if len(au.entries) != 1 || au.entries[0].Action != "agent_skill.create" || au.entries[0].ActorID != 7 {
		t.Fatalf("审计不对：%+v", au.entries)
	}
	if _, err := a.Create(context.Background(), 7, goodInput()); bizCode(err) != errcode.DuplicateName {
		t.Fatalf("重复 code 应报 DuplicateName，得到 %v", err)
	}
}

func TestSkillAdmin_各类问题分别被拒并说清原因(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*SkillInput)
		want string
	}{
		{"未注册工具", func(in *SkillInput) { in.Tools = []string{"shell"} }, "「shell」不是已注册的工具"},
		{"推理模型不支持 Agent", func(in *SkillInput) { in.Bindings.Reasoning = "chat_plain" }, "不支持 Agent"},
		{"推理模型不是文本", func(in *SkillInput) { in.Bindings.Reasoning = "kling" }, "是视频模型，不是文本模型"},
		{"视频模型不存在", func(in *SkillInput) { in.Bindings.Video = "nope" }, "在模型目录里不存在"},
		{"图片绑了视频模型", func(in *SkillInput) { in.Bindings.Image = "kling" }, "不是图片模型"},
		{"步骤不连续", func(in *SkillInput) { in.Steps = []domain.SkillStep{{No: 2, Name: "x"}} }, "从 1 起连续"},
		{"模板渲染失败", func(in *SkillInput) { in.Body = "按 {{story}} 写" }, "主说明渲染失败"},
		{"占位符未闭合", func(in *SkillInput) { in.Body = "按 {{state 写" }, "主说明渲染失败"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, st, _ := newTestSkillAdmin()
			in := goodInput()
			c.mod(&in)
			_, err := a.Create(context.Background(), 1, in)
			if bizCode(err) != errcode.InvalidArgument || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("期望 InvalidArgument 且含 %q，得到 %v", c.want, err)
			}
			if len(st.skills) != 0 {
				t.Fatal("校验失败仍然落库了")
			}
		})
	}
}

func TestSkillAdmin_版本冲突不静默覆盖(t *testing.T) {
	a, _, _ := newTestSkillAdmin()
	ctx := context.Background()
	s, _ := a.Create(ctx, 1, goodInput())
	in := goodInput()
	in.Name = "甲改的"
	if _, err := a.Update(ctx, 1, s.ID, s.Version, in); err != nil {
		t.Fatalf("第一次保存失败：%v", err)
	}
	in.Name = "乙改的"
	_, err := a.Update(ctx, 2, s.ID, s.Version, in)
	if bizCode(err) != errcode.VersionConflict {
		t.Fatalf("后保存者应收到版本冲突，得到 %v", err)
	}
	cur, _ := a.Get(ctx, s.ID)
	if cur.Skill.Name != "甲改的" {
		t.Fatalf("被静默覆盖：%q", cur.Skill.Name)
	}
	// 附属文档的写也走同一把锁。
	if _, err := a.SaveRef(ctx, 2, s.ID, 0, s.Version, SkillRefInput{Name: "指南", Body: "x"}); bizCode(err) != errcode.VersionConflict {
		t.Fatalf("附属文档用旧版本写应冲突，得到 %v", err)
	}
}

func TestSkillAdmin_改code被忽略(t *testing.T) {
	a, _, _ := newTestSkillAdmin()
	ctx := context.Background()
	s, _ := a.Create(ctx, 1, goodInput())
	in := goodInput()
	in.Code = "renamed"
	out, err := a.Update(ctx, 1, s.ID, s.Version, in)
	if err != nil || out.Code != "story_topic" {
		t.Fatalf("code 不应被修改：%v %q", err, out.Code)
	}
}

func TestSkillAdmin_启用前重新试渲染附属文档(t *testing.T) {
	a, st, au := newTestSkillAdmin()
	ctx := context.Background()
	s, _ := a.Create(ctx, 1, goodInput())
	// 模拟存量脏数据：直接写进仓储、绕过保存校验。
	st.refs[999] = repo.SkillReference{ID: 999, SkillID: s.ID, Name: "旧指南", Body: "{{removedVar}}"}
	_, err := a.SetEnabled(ctx, 1, s.ID, true)
	if bizCode(err) != errcode.InvalidArgument || !strings.Contains(err.Error(), "附属文档「旧指南」渲染失败") {
		t.Fatalf("应拒绝启用并点名附属文档，得到 %v", err)
	}
	delete(st.refs, 999)
	out, err := a.SetEnabled(ctx, 1, s.ID, true)
	if err != nil || !out.IsEnabled {
		t.Fatalf("启用失败：%v", err)
	}
	n := len(au.entries)
	if _, err := a.SetEnabled(ctx, 1, s.ID, true); err != nil || len(au.entries) != n {
		t.Fatalf("重复启用应是无操作且不写审计：%v %d→%d", err, n, len(au.entries))
	}
	if _, err := a.SetEnabled(ctx, 1, s.ID, false); err != nil || au.entries[len(au.entries)-1].Action != "agent_skill.disable" {
		t.Fatalf("停用失败或未写审计：%v", err)
	}
}

func TestSkillAdmin_工具选项来自规格表(t *testing.T) {
	a, _, _ := newTestSkillAdmin()
	opts, err := a.Options(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(opts.Tools) != len(domain.Tools()) {
		t.Fatalf("工具选项 %d 个，规格表 %d 个", len(opts.Tools), len(domain.Tools()))
	}
	for _, o := range opts.Tools {
		if o.Name == string(domain.ToolGenerate) && (o.Planning || !o.Executing || !o.Paid) {
			t.Fatalf("generate 的说明不对：%+v", o)
		}
	}
	if strings.Join(opts.Vars, ",") != strings.Join(skillVarNames(), ",") || len(opts.Vars) == 0 {
		t.Fatalf("变量清单不对：%v", opts.Vars)
	}
}

// fakeCopy 模拟营销域的口播检查：「数字人」拦截并点名字段；带「口播」标签而没有提示句告警。
// 同时记下收到的字段，钉住「哪些字段算用户可见」。
type fakeCopy struct{ got map[string]string }

func (f *fakeCopy) CheckCopy(fields map[string]string, tags []string) (errs, warns []CopyIssue) {
	f.got = fields
	hasNote := false
	for k, v := range fields {
		if strings.Contains(v, "数字人") {
			errs = append(errs, CopyIssue{Field: k, Code: "banned_word", Message: "「" + k + "」含禁用词「数字人」"})
		}
		if strings.Contains(v, "不保证口型") {
			hasNote = true
		}
	}
	for _, t := range tags {
		if t == "口播" && !hasNote {
			warns = append(warns, CopyIssue{Code: "missing_disclaimer", Message: "缺提示句"})
		}
	}
	return errs, warns
}

type fakeSlots struct{ issues []CopyIssue }

func (f fakeSlots) ValidateSlotDecl(raw json.RawMessage) (json.RawMessage, []CopyIssue) {
	return raw, f.issues
}

func TestValidateSkill_口播禁用词经CopyChecker(t *testing.T) {
	a, st, _ := newTestSkillAdmin()
	fc := &fakeCopy{}
	a.SetMarketingChecks(fc, fakeSlots{})
	ctx := context.Background()

	in := goodInput()
	in.Tags = []string{"口播"}
	pre := "让数字人帮你讲"
	in.PrefillText = &pre
	_, err := a.Create(ctx, 1, in)
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.InvalidArgument || !strings.Contains(err.Error(), "「prefillText」含禁用词") {
		t.Fatalf("预填含禁用词应被拦并点名字段，得到 %v", err)
	}
	issues, _ := be.Details["issues"].([]map[string]any)
	if len(issues) != 1 || issues[0]["field"] != "prefillText" {
		t.Fatalf("details.issues 应点名 prefillText：%v", be.Details)
	}
	if len(st.skills) != 0 {
		t.Fatal("被拦的技能落库了")
	}
	for _, k := range []string{"name", "description", "prefillText", "steps[0].name"} {
		if _, ok := fc.got[k]; !ok {
			t.Errorf("用户可见字段 %s 没交给口播检查：%v", k, fc.got)
		}
	}
	if _, ok := fc.got["body"]; ok {
		t.Error("主说明是给模型的，不应按用户可见文案检查")
	}

	// 缺提示句只告警：照常保存，warnings 带回去。
	pre = "上传商品图，生成对镜讲解视频"
	out, err := a.Create(ctx, 1, in)
	if err != nil || len(out.Warnings) != 1 || out.Warnings[0].Code != "missing_disclaimer" {
		t.Fatalf("缺提示句应保存成功并带告警：%v %+v", err, out.Warnings)
	}
}

func TestSkillAdmin_输入槽经端口校验(t *testing.T) {
	ctx := context.Background()
	in := goodInput()
	in.InputSlots = json.RawMessage(`[{"key":"product","label":"商品","accept":["image"],"maxCount":99}]`)

	// 没接校验器时，声明了槽位的技能存不进去（缺了等于放行一切）。
	a, _, _ := newTestSkillAdmin()
	if _, err := a.Create(ctx, 1, in); bizCode(err) != errcode.InvalidArgument || !strings.Contains(err.Error(), "未接入输入槽校验") {
		t.Fatalf("未接校验器应拒，得到 %v", err)
	}
	// 校验器报的问题原样拦下并一次报完。
	a.SetMarketingChecks(&fakeCopy{}, fakeSlots{issues: []CopyIssue{
		{Field: "inputSlots[0].maxCount", Message: "数量上限须在 1~20 之间"},
		{Field: "inputSlots[1].key", Message: "不认识「logo」"},
	}})
	_, err := a.Create(ctx, 1, in)
	if !strings.Contains(err.Error(), "1~20") || !strings.Contains(err.Error(), "logo") {
		t.Fatalf("应一次报完两处槽位问题，得到 %v", err)
	}
	// 没声明槽位（[] / 不传）不需要校验器。
	b, _, _ := newTestSkillAdmin()
	in.InputSlots = json.RawMessage(`[]`)
	if _, err := b.Create(ctx, 1, in); err != nil {
		t.Fatalf("空槽位不该被拦：%v", err)
	}
}

func TestSkillAdmin_修改不带新字段时保留原值(t *testing.T) {
	a, _, _ := newTestSkillAdmin()
	a.SetMarketingChecks(&fakeCopy{}, fakeSlots{})
	ctx := context.Background()
	in := goodInput()
	pre := "介绍一下你的商品"
	in.Scenes = []string{"marketing"}
	in.InputSlots = json.RawMessage(`[{"key":"product","label":"商品","accept":["image"],"required":true,"maxCount":20}]`)
	in.PrefillText = &pre
	batch := true
	in.IsBatch = &batch
	s, err := a.Create(ctx, 1, in)
	if err != nil {
		t.Fatal(err)
	}
	if !s.IsBatch {
		t.Fatal("新建时 isBatch=true 没存下")
	}
	// 旧表单（不认识三个新字段）只改名称。
	old := goodInput()
	old.Name = "改个名"
	out, err := a.Update(ctx, 1, s.ID, s.Version, old)
	if err != nil {
		t.Fatal(err)
	}
	if string(out.Scenes) != `["marketing"]` || string(out.InputSlots) != string(in.InputSlots) || out.PrefillText != pre {
		t.Fatalf("新字段被旧表单清掉了：%s %s %q", out.Scenes, out.InputSlots, out.PrefillText)
	}
	if !out.IsBatch {
		t.Fatal("旧表单不带 isBatch 时把批量标识清掉了（MKT-T-32）")
	}
	// 显式传空才清空。
	empty := ""
	off := false
	old.Scenes, old.InputSlots, old.PrefillText, old.IsBatch = []string{}, json.RawMessage(`[]`), &empty, &off
	out, err = a.Update(ctx, 1, s.ID, out.Version, old)
	if err != nil || string(out.Scenes) != `[]` || string(out.InputSlots) != `[]` || out.PrefillText != "" {
		t.Fatalf("显式清空没生效：%v %s %s %q", err, out.Scenes, out.InputSlots, out.PrefillText)
	}
	if out.IsBatch {
		t.Fatal("显式传 isBatch=false 没生效")
	}
}

func TestSkillAdmin_营销技能启用要求封面(t *testing.T) {
	a, st, _ := newTestSkillAdmin()
	ctx := context.Background()
	in := goodInput()
	in.Scenes = []string{"marketing"}
	s, err := a.Create(ctx, 1, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetEnabled(ctx, 1, s.ID, true); bizCode(err) != errcode.InvalidArgument || !strings.Contains(err.Error(), "必须配封面") {
		t.Fatalf("没封面的营销技能不应能启用，得到 %v", err)
	}
	cur := st.skills[s.ID]
	cur.CoverMediaID = 42
	st.skills[s.ID] = cur
	if _, err := a.SetEnabled(ctx, 1, s.ID, true); err != nil {
		t.Fatalf("配了封面应能启用：%v", err)
	}
}

// 04 文档 P-54②：封面闸原先只在「启用」那一步，启用之后去掉封面拦不住。
func TestSkillAdmin_已启用的营销技能改完仍要有封面(t *testing.T) {
	a, st, _ := newTestSkillAdmin()
	ctx := context.Background()
	in := goodInput()
	in.Scenes = []string{"marketing"}
	in.CoverMediaID = 501
	s, err := a.Create(ctx, 7, in)
	if err != nil {
		t.Fatal(err)
	}
	sk := st.skills[s.ID]
	sk.IsEnabled = true
	st.skills[s.ID] = sk
	in.CoverMediaID = 0
	if _, err := a.Update(ctx, 7, s.ID, sk.Version, in); err == nil || !strings.Contains(err.Error(), "必须配封面") {
		t.Fatalf("已启用的营销技能去掉封面应被拒：%v", err)
	}
	// 停用状态下照常能改（草稿可以先存，启用时才强制）。
	sk.IsEnabled = false
	st.skills[s.ID] = sk
	if _, err := a.Update(ctx, 7, s.ID, sk.Version, in); err != nil {
		t.Fatalf("停用的技能可以先不配封面：%v", err)
	}
}
