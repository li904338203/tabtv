package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/marketing/domain"
	"github.com/li904338203/tabtv/backend/internal/marketing/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 角色库服务的单测：校验与错误形状、adopt 的分支。重名 / 隔离 / 复制字节在真库集成测试里验。

type memCharacters struct {
	rows      map[int64]repo.Character
	dupNames  map[string]bool // 插入时视为重名的名称
	quota     bool
	inserted  []repo.Character
	copyExist bool // 下一次插入报「副本已存在」（模拟并发 adopt）
}

func newMemCharacters() *memCharacters {
	return &memCharacters{rows: map[int64]repo.Character{}, dupNames: map[string]bool{}}
}

func (m *memCharacters) InsertCharacter(_ context.Context, c repo.Character, _ int) (repo.Character, error) {
	if m.quota {
		return repo.Character{}, repo.ErrQuota
	}
	if m.copyExist {
		m.copyExist = false
		c.ID = 999
		c.Version = 1
		m.rows[c.ID] = c
		return repo.Character{}, repo.ErrPresetCopyExists
	}
	if m.dupNames[c.Name] {
		return repo.Character{}, repo.ErrDuplicate
	}
	c.Version = 1
	m.rows[c.ID] = c
	m.inserted = append(m.inserted, c)
	return c, nil
}
func (m *memCharacters) GetCharacter(_ context.Context, ws, id int64) (repo.Character, error) {
	c, ok := m.rows[id]
	if !ok || c.WorkspaceID != ws {
		return repo.Character{}, repo.ErrNotFound
	}
	return c, nil
}
func (m *memCharacters) GetCharacterByPreset(_ context.Context, ws, pid int64, v int) (repo.Character, error) {
	for _, c := range m.rows {
		if c.WorkspaceID == ws && c.SourcePresetID == pid && c.SourcePresetVersion == v {
			return c, nil
		}
	}
	return repo.Character{}, repo.ErrNotFound
}
func (m *memCharacters) UpdateCharacter(_ context.Context, c repo.Character, v int) (repo.Character, error) {
	if m.dupNames[c.Name] {
		return repo.Character{}, repo.ErrDuplicate
	}
	c.Version = v + 1
	m.rows[c.ID] = c
	return c, nil
}
func (m *memCharacters) SoftDeleteCharacter(_ context.Context, ws, id, _ int64) (repo.Character, error) {
	c, ok := m.rows[id]
	if !ok || c.WorkspaceID != ws {
		return repo.Character{}, repo.ErrNotFound
	}
	delete(m.rows, id)
	return c, nil
}
func (m *memCharacters) ListCharacters(context.Context, repo.CharacterFilter) ([]repo.Character, bool, error) {
	return nil, false, nil
}

type memPresets map[int64]repo.PresetCharacter

func (m memPresets) ListEnabledPresets(context.Context, repo.PresetFilter) ([]repo.PresetCharacter, bool, error) {
	return nil, false, nil
}
func (m memPresets) GetEnabledPreset(_ context.Context, id int64) (repo.PresetCharacter, error) {
	p, ok := m[id]
	if !ok || !p.IsEnabled {
		return repo.PresetCharacter{}, repo.ErrNotFound
	}
	return p, nil
}

type recCopier struct {
	calls []int64
	fail  error
}

func (c *recCopier) CopyToWorkspace(_ context.Context, mid, _, _ int64) (int64, error) {
	if c.fail != nil {
		return 0, c.fail
	}
	c.calls = append(c.calls, mid)
	return mid + 10000, nil
}

func newCharLib(chars *memCharacters, presets memPresets, media memMedia, cp *recCopier) *Library {
	n := int64(100)
	lib := NewLibrary(&memProducts{rows: map[int64]repo.Product{}}, media, &recDeleter{}, LibrarySettings{},
		func() int64 { n++; return n })
	lib.SetCharacters(chars, presets, cp)
	return lib
}

func bizCode(t *testing.T, err error) (errcode.Code, map[string]any) {
	t.Helper()
	var be *httpx.BizError
	if !errors.As(err, &be) {
		t.Fatalf("应为业务错误，得 %v", err)
	}
	return be.Code, be.Details
}

const charWS = int64(7)

func TestCreateCharacter_图片音频归属与格式一次报完(t *testing.T) {
	media := memMedia{
		1: {ID: 1, WorkspaceID: charWS, ContentType: "image/png"},
		2: {ID: 2, WorkspaceID: charWS, ContentType: "image/gif"},
		3: {ID: 3, WorkspaceID: 99, ContentType: "image/png"},
		4: {ID: 4, WorkspaceID: 0, ContentType: "image/png"}, // 平台素材：不能直接当角色图
		5: {ID: 5, WorkspaceID: charWS, ContentType: "video/mp4"},
	}
	lib := newCharLib(newMemCharacters(), nil, media, &recCopier{})
	_, err := lib.CreateCharacter(context.Background(), LibraryActor{WorkspaceID: charWS, UserID: 1},
		domain.CharacterInput{Name: "柚子@君", ImageMediaIDs: []int64{1, 2, 3, 4}, VoiceMediaID: 5})
	code, det := bizCode(t, err)
	if code != errcode.InvalidArgument {
		t.Fatalf("应 40001，得 %d", code)
	}
	got := map[string]domain.IssueCode{}
	for _, i := range det["issues"].([]domain.Issue) {
		got[i.Field] = i.Code
	}
	want := map[string]domain.IssueCode{"name": domain.IssueBadFormat, "imageMediaIds[1]": domain.IssueBadFormat,
		"imageMediaIds[2]": domain.IssueBadValue, "imageMediaIds[3]": domain.IssueBadValue,
		"voiceMediaId": domain.IssueBadFormat}
	for f, c := range want {
		if got[f] != c {
			t.Errorf("%s 期望 %s，得 %v（全部 %v）", f, c, got[f], got)
		}
	}
	if _, ok := got["imageMediaIds[0]"]; ok {
		t.Error("合法的第 1 张不应报错")
	}
	// 合法：音频是 audio/mpeg。
	media[6] = MediaFacts{ID: 6, WorkspaceID: charWS, ContentType: "audio/mpeg"}
	c, err := lib.CreateCharacter(context.Background(), LibraryActor{WorkspaceID: charWS, UserID: 1},
		domain.CharacterInput{Name: " 柚子君 ", ImageMediaIDs: []int64{1}, VoiceMediaID: 6})
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "柚子君" || c.Source != repo.CharacterSourceUser || c.VoiceMediaID != 6 {
		t.Fatalf("落库内容不对：%+v", c)
	}
}

func TestCreateCharacter_重名与配额409(t *testing.T) {
	media := memMedia{1: {ID: 1, WorkspaceID: charWS, ContentType: "image/webp"}}
	chars := newMemCharacters()
	chars.dupNames["小美"] = true
	lib := newCharLib(chars, nil, media, &recCopier{})
	a := LibraryActor{WorkspaceID: charWS, UserID: 1}
	_, err := lib.CreateCharacter(context.Background(), a, domain.CharacterInput{Name: "小美", ImageMediaIDs: []int64{1}})
	code, det := bizCode(t, err)
	if code != errcode.Conflict || det["field"] != "name" || det["code"] != "duplicate" {
		t.Fatalf("重名应 40901 + field=name code=duplicate，得 %d %v", code, det)
	}
	chars.quota = true
	_, err = lib.CreateCharacter(context.Background(), a, domain.CharacterInput{Name: "小丽", ImageMediaIDs: []int64{1}})
	code, det = bizCode(t, err)
	if code != errcode.Conflict || det["reason"] != "quota" || det["max"] != domain.DefaultMaxCharacters {
		t.Fatalf("配额满应 409 reason=quota max=200，得 %d %v", code, det)
	}
}

func TestDeleteCharacter_图片与音频都尝试释放(t *testing.T) {
	media := memMedia{
		1: {ID: 1, WorkspaceID: charWS, ContentType: "image/png", Purpose: mediaPurposeUpload},
		2: {ID: 2, WorkspaceID: charWS, ContentType: "image/png", Purpose: "generation_output"},
		3: {ID: 3, WorkspaceID: charWS, ContentType: "audio/wav", Purpose: mediaPurposeUpload},
	}
	chars := newMemCharacters()
	chars.rows[50] = repo.Character{ID: 50, WorkspaceID: charWS, ImageMediaIDs: []int64{1, 2}, VoiceMediaID: 3}
	del := &recDeleter{}
	lib := NewLibrary(&memProducts{rows: map[int64]repo.Product{}}, media, del, LibrarySettings{},
		func() int64 { return 1 })
	lib.SetCharacters(chars, nil, nil)
	if err := lib.DeleteCharacter(context.Background(), LibraryActor{WorkspaceID: charWS}, 50); err != nil {
		t.Fatal(err)
	}
	if len(del.got) != 2 || del.got[0] != 1 || del.got[1] != 3 {
		t.Fatalf("应只删自有上传的图与音频（生成产物跳过），得 %v", del.got)
	}
	if err := lib.DeleteCharacter(context.Background(), LibraryActor{WorkspaceID: charWS}, 50); err == nil {
		t.Fatal("重复删除应 404")
	}
}

func TestAdopt_分支(t *testing.T) {
	presets := memPresets{
		10: {ID: 10, Name: "柚子君", ImageMediaIDs: []int64{1, 2}, VoiceMediaID: 3, IsEnabled: true, Version: 2,
			Appearance: "圆脸", VoiceText: "少年音"},
		11: {ID: 11, Name: "停用的", ImageMediaIDs: []int64{1}, IsEnabled: false, Version: 1},
	}
	a := LibraryActor{WorkspaceID: charWS, UserID: 3}
	ctx := context.Background()

	t.Run("首次添加复制字节并记版本", func(t *testing.T) {
		chars, cp := newMemCharacters(), &recCopier{}
		lib := newCharLib(chars, presets, nil, cp)
		c, created, err := lib.Adopt(ctx, a, 10)
		if err != nil || !created {
			t.Fatalf("应新建：%v %v", created, err)
		}
		if c.Source != repo.CharacterSourcePreset || c.SourcePresetID != 10 || c.SourcePresetVersion != 2 {
			t.Fatalf("来源不对：%+v", c)
		}
		if len(c.ImageMediaIDs) != 2 || c.ImageMediaIDs[0] != 10001 || c.VoiceMediaID != 10003 {
			t.Fatalf("应换成复制出来的新媒体 ID：%+v", c)
		}
		if len(cp.calls) != 3 {
			t.Fatalf("图片 2 张 + 音频 1 段都要复制：%v", cp.calls)
		}
		// 再添加一次：回同一份，不再复制。
		c2, created, err := lib.Adopt(ctx, a, 10)
		if err != nil || created || c2.ID != c.ID || len(cp.calls) != 3 {
			t.Fatalf("重复添加应回已有那份且不复制：%v %v %v", created, err, cp.calls)
		}
	})
	t.Run("撞名加后缀重试一次", func(t *testing.T) {
		chars := newMemCharacters()
		chars.dupNames["柚子君"] = true
		lib := newCharLib(chars, presets, nil, &recCopier{})
		c, created, err := lib.Adopt(ctx, a, 10)
		if err != nil || !created || c.Name != "柚子君-营销角色" {
			t.Fatalf("应加后缀：%+v %v", c, err)
		}
		chars.dupNames["柚子君-营销角色"] = true
		chars.rows = map[int64]repo.Character{}
		_, _, err = lib.Adopt(ctx, a, 10)
		code, det := bizCode(t, err)
		if code != errcode.Conflict || det["field"] != "name" {
			t.Fatalf("加后缀仍冲突应 409 field=name：%d %v", code, det)
		}
	})
	t.Run("并发添加撞副本唯一回已有", func(t *testing.T) {
		chars := newMemCharacters()
		chars.copyExist = true
		lib := newCharLib(chars, presets, nil, &recCopier{})
		c, created, err := lib.Adopt(ctx, a, 10)
		if err != nil || created || c.ID != 999 {
			t.Fatalf("应回已有那份：%+v %v %v", c, created, err)
		}
	})
	t.Run("停用与不存在同一个404且不复制", func(t *testing.T) {
		cp := &recCopier{}
		lib := newCharLib(newMemCharacters(), presets, nil, cp)
		for _, id := range []int64{11, 12} {
			_, _, err := lib.Adopt(ctx, a, id)
			if code, _ := bizCode(t, err); code != errcode.NotFound {
				t.Fatalf("预置 %d 应 404，得 %d", id, code)
			}
		}
		if len(cp.calls) != 0 {
			t.Fatal("判可见之前不得复制任何媒体")
		}
	})
	t.Run("源媒体已清不漏媒体域的话", func(t *testing.T) {
		lib := newCharLib(newMemCharacters(), presets, nil,
			&recCopier{fail: httpx.Errf(errcode.NotFound, "源媒体已不存在")})
		_, _, err := lib.Adopt(ctx, a, 10)
		code, _ := bizCode(t, err)
		if code != errcode.Conflict || strings.Contains(err.Error(), "源媒体") {
			t.Fatalf("应改说「素材暂不可用」：%d %v", code, err)
		}
	})
}

func TestWithAdoptSuffix_不超过50码点(t *testing.T) {
	long := strings.Repeat("柚", domain.MaxCharacterName)
	got := withAdoptSuffix(long)
	if domain.RuneLen(got) != domain.MaxCharacterName || !strings.HasSuffix(got, adoptNameSuffix) {
		t.Fatalf("加后缀后应恰好 50 码点且以后缀结尾：%d %q", domain.RuneLen(got), got)
	}
	if withAdoptSuffix("小美") != "小美-营销角色" {
		t.Fatal("短名直接加后缀")
	}
}

func TestPresetMedia_越界与无音频404(t *testing.T) {
	lib := newCharLib(newMemCharacters(), memPresets{
		10: {ID: 10, ImageMediaIDs: []int64{1, 2}, IsEnabled: true, Version: 1},
	}, nil, nil)
	ctx := context.Background()
	if m, err := lib.PresetImageMedia(ctx, 10, 1); err != nil || m != 2 {
		t.Fatalf("下标从 0：%d %v", m, err)
	}
	for _, idx := range []int{2, -1} {
		if _, err := lib.PresetImageMedia(ctx, 10, idx); err == nil {
			t.Fatalf("下标 %d 应 404", idx)
		}
	}
	if _, err := lib.PresetVoiceMedia(ctx, 10); err == nil {
		t.Fatal("没有音频应 404")
	}
}

// 04 文档 P-49：撞名后缀本身必须是合法的角色名字符，否则副本改任何字段都回 40001。
func TestAdoptSuffix_加完后缀仍是合法角色名(t *testing.T) {
	for _, name := range []string{"小美", strings.Repeat("长", 50)} {
		got := withAdoptSuffix(name)
		if issues := domain.CharacterNameIssue("name", got); len(issues) != 0 {
			t.Fatalf("加完后缀的 %q 应是合法角色名：%+v", got, issues)
		}
	}
}
