package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/marketing/app"
	"github.com/li904338203/tabtv/backend/internal/marketing/repo"
)

// 角色库路由层：挂没挂对、公开的营销角色不漏媒体 ID、停用即 404。
// 只读成员 403 走真实工作区闸，见 marketing/transport/integration（TestCharacterRoutes_只读成员403）。

type nopCharacters struct{}

func (nopCharacters) InsertCharacter(context.Context, repo.Character, int) (repo.Character, error) {
	return repo.Character{}, nil
}
func (nopCharacters) GetCharacter(context.Context, int64, int64) (repo.Character, error) {
	return repo.Character{}, repo.ErrNotFound
}
func (nopCharacters) GetCharacterByPreset(context.Context, int64, int64, int) (repo.Character, error) {
	return repo.Character{}, repo.ErrNotFound
}
func (nopCharacters) UpdateCharacter(context.Context, repo.Character, int) (repo.Character, error) {
	return repo.Character{}, repo.ErrNotFound
}
func (nopCharacters) SoftDeleteCharacter(context.Context, int64, int64, int64) (repo.Character, error) {
	return repo.Character{}, repo.ErrNotFound
}
func (nopCharacters) ListCharacters(context.Context, repo.CharacterFilter) ([]repo.Character, bool, error) {
	return nil, false, nil
}

// fixedPresets 只回启用的（与仓储口径一致）。
type fixedPresets []repo.PresetCharacter

func (f fixedPresets) ListEnabledPresets(context.Context, repo.PresetFilter) ([]repo.PresetCharacter, bool, error) {
	var out []repo.PresetCharacter
	for _, p := range f {
		if p.IsEnabled {
			out = append(out, p)
		}
	}
	return out, false, nil
}
func (f fixedPresets) GetEnabledPreset(_ context.Context, id int64) (repo.PresetCharacter, error) {
	for _, p := range f {
		if p.ID == id && p.IsEnabled {
			return p, nil
		}
	}
	return repo.PresetCharacter{}, repo.ErrNotFound
}

func characterRouter(presets fixedPresets) (*chi.Mux, *stubMedia, *int) {
	h := NewHandler(app.NewService(stubStore{}, stubCatalog{}, nil, app.Settings{}), true)
	lib := app.NewLibrary(nopProducts{}, nil, nil, app.LibrarySettings{}, func() int64 { return 1 })
	lib.SetCharacters(nopCharacters{}, presets, nil)
	h.SetLibrary(lib, func(*http.Request) int64 { return 1 })
	m := &stubMedia{}
	h.SetMedia(m)
	hits := 0
	guard := func(n http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			hits++
			n.ServeHTTP(w, req)
		})
	}
	r := chi.NewRouter()
	h.MountPublic(r)
	h.MountLibrary(r, func(n http.Handler) http.Handler { return n }, guard)
	return r, m, &hits
}

var characterRoutes = []struct{ method, path string }{
	{http.MethodGet, "/marketing/characters"},
	{http.MethodPost, "/marketing/characters"},
	{http.MethodGet, "/marketing/characters/123"},
	{http.MethodPut, "/marketing/characters/123"},
	{http.MethodDelete, "/marketing/characters/123"},
	{http.MethodPost, "/marketing/preset-characters/123/adopt"},
}

// 角色 5 个 + adopt 都经过工作区闸；公开的营销角色列表不经过（未登录也能看）。
func TestCharacterRoutes_走工作区闸且与公开路由共存(t *testing.T) {
	r, _, hits := characterRouter(fixedPresets{{ID: 5, Name: "柚子君", ImageMediaIDs: []int64{77}, IsEnabled: true,
		Version: 1}})
	for _, rt := range characterRoutes {
		before := *hits
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(rt.method, rt.path, strings.NewReader("{}")))
		if *hits != before+1 || rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s 应经过工作区闸且无上下文时 403，得 %d：%s", rt.method, rt.path, rec.Code, rec.Body.String())
		}
	}
	before := *hits
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/marketing/preset-characters", nil))
	if rec.Code != http.StatusOK || *hits != before {
		t.Fatalf("公开列表应 200 且不经过工作区闸，得 %d", rec.Code)
	}
}

func TestPresetPublic_停用404且无媒体ID(t *testing.T) {
	presets := fixedPresets{
		{ID: 5, Name: "柚子君", Summary: "清爽少年", ImageMediaIDs: []int64{770001, 770002}, VoiceMediaID: 880001,
			IsEnabled: true, Version: 3, Sort: 1},
		{ID: 6, Name: "下线的", ImageMediaIDs: []int64{990001}, VoiceMediaID: 990002, IsEnabled: false, Version: 1},
	}
	r, m, _ := characterRouter(presets)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/marketing/preset-characters", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("列表应 200，得 %d", rec.Code)
	}
	body := rec.Body.String()
	// ⚠️ 任何媒体 ID 都不能出现在响应里（图片、音频）。
	for _, leak := range []string{"770001", "770002", "880001", "990001", "Media"} {
		if strings.Contains(body, leak) {
			t.Fatalf("公开列表漏了媒体 ID / 媒体字段 %q：%s", leak, body)
		}
	}
	var out struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 1 || out.Items[0]["id"] != "5" || out.Items[0]["imageCount"] != float64(2) ||
		out.Items[0]["hasVoice"] != true || out.Items[0]["version"] != float64(3) {
		t.Fatalf("停用的不出现、启用的字段齐全：%+v", out.Items)
	}
	// 字段集合钉死：新增字段要过一遍「会不会漏媒体」。
	keys := []string{}
	for k := range out.Items[0] {
		keys = append(keys, k)
	}
	if want := "hasVoice id imageCount name summary version"; sortedJoin(keys) != want {
		t.Fatalf("公开字段集合变了：%s（期望 %s）", sortedJoin(keys), want)
	}

	// 图片 / 音频：启用的出、下标从 0、越界与停用同一个 404。
	cases := []struct {
		path string
		code int
		mid  int64
	}{
		{"/marketing/preset-characters/5/images/0", 200, 770001},
		{"/marketing/preset-characters/5/images/1?w=160", 200, 770002},
		{"/marketing/preset-characters/5/voice", 200, 880001},
		{"/marketing/preset-characters/5/images/2", 404, 0},
		{"/marketing/preset-characters/5/images/-1", 404, 0},
		{"/marketing/preset-characters/6/images/0", 404, 0},
		{"/marketing/preset-characters/6/voice", 404, 0},
		{"/marketing/preset-characters/abc/voice", 404, 0},
	}
	for _, c := range cases {
		before := len(m.got)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
		if rec.Code != c.code {
			t.Fatalf("%s 应 %d，得 %d", c.path, c.code, rec.Code)
		}
		if c.code == 200 {
			if m.got[len(m.got)-1] != c.mid || rec.Header().Get("Cache-Control") != "public, max-age=300" {
				t.Fatalf("%s 应吐媒体 %d 且带公开缓存头", c.path, c.mid)
			}
		} else if len(m.got) != before {
			t.Fatalf("%s 不可见时不应输出任何媒体", c.path)
		}
	}
}

func sortedJoin(ss []string) string {
	for i := range ss {
		for j := i + 1; j < len(ss); j++ {
			if ss[j] < ss[i] {
				ss[i], ss[j] = ss[j], ss[i]
			}
		}
	}
	return strings.Join(ss, " ")
}

// 契约与路由里没有「可见范围 / 公开 / 真人认证」（MK-23/24，MKT-AC-37 后端部分）。
func TestCharacterContract_无可见范围与真人字段(t *testing.T) {
	banned := regexp.MustCompile(`(?i)visibility|realPerson|real_person|verif|实名|真人|扫码`)
	for _, v := range []any{characterReq{}, characterResp{}, characterSummaryResp{}, publicPresetResp{}} {
		b, _ := json.Marshal(v)
		if banned.Match(b) {
			t.Fatalf("%T 出现了排除项字段：%s", v, b)
		}
	}
	for _, rt := range characterRoutes {
		if banned.MatchString(rt.path) {
			t.Fatalf("路由出现排除项：%s", rt.path)
		}
	}
}
