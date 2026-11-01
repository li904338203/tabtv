//go:build integration

// 角色库与营销角色 adopt（MKT-T-34）的真库 + httptest 测试。
//
// ⚠️ adopt 要**真复制字节**（MKD-30）：这里用一个最小的假 S3（httptest，内存里存对象）接真实的
// media/app.Service，验「副本是新媒体 ID、删掉源平台媒体后副本照样取得到」—— 只比 ID 不同的话，
// 一个「新建一行指向同一个 object_key」的实现也会是绿的。
// ⚠️ 走真实的工作区闸（RequireWorkspace + GuardByMethod）：只读成员 403 必须由路由层给出。
package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	collabapp "github.com/li904338203/tabtv/backend/internal/collaboration/app"
	collabrepo "github.com/li904338203/tabtv/backend/internal/collaboration/repo"
	collabhttp "github.com/li904338203/tabtv/backend/internal/collaboration/transport"
	marketingapp "github.com/li904338203/tabtv/backend/internal/marketing/app"
	marketingrepo "github.com/li904338203/tabtv/backend/internal/marketing/repo"
	marketinghttp "github.com/li904338203/tabtv/backend/internal/marketing/transport"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	mediarepo "github.com/li904338203/tabtv/backend/internal/media/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/config"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/objstore"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

// fakeS3 是只够 PutObject / GetObject 用的内存对象存储（path-style：/bucket/key）。
type fakeS3 struct {
	mu   sync.Mutex
	objs map[string][]byte
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Path
	switch r.Method {
	case http.MethodPut:
		var body []byte
		var err error
		// SDK 可能用 aws-chunked 编码上传（带 trailer 校验和）：拆块取原始字节。
		if strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") ||
			strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING") {
			body, err = decodeAWSChunked(r.Body)
		} else {
			body, err = io.ReadAll(r.Body)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.objs[key] = body
		f.mu.Unlock()
		w.Header().Set("ETag", `"x"`)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet, http.MethodHead:
		f.mu.Lock()
		b, ok := f.objs[key]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `<?xml version="1.0"?><Error><Code>NoSuchKey</Code></Error>`)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(b)
		}
	case http.MethodDelete:
		f.mu.Lock()
		delete(f.objs, key)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func decodeAWSChunked(r io.Reader) ([]byte, error) {
	br := bufio.NewReader(r)
	var out bytes.Buffer
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size := strings.TrimSpace(line)
		if i := strings.IndexByte(size, ';'); i >= 0 {
			size = size[:i]
		}
		n, err := strconv.ParseInt(size, 16, 64)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return out.Bytes(), nil // 之后是 trailer，不关心
		}
		if _, err := io.CopyN(&out, br, n); err != nil {
			return nil, err
		}
		if _, err := br.ReadString('\n'); err != nil { // 块尾 CRLF
			return nil, err
		}
	}
}

type characterRefs struct{ r *marketingrepo.Repo }

func (c characterRefs) Kind() string { return mediaapp.UsageMarketingCharacter }
func (c characterRefs) References(ctx context.Context, id int64) ([]int64, error) {
	return c.r.CharacterMediaReferences(ctx, id)
}

// newCharEnv 同 newEnv，但媒体服务接了（假的）对象存储，并注册角色检查器。
func newCharEnv(t *testing.T) *env {
	t.Helper()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	next := gen.Next
	ctx := context.Background()
	e := &env{t: t, pool: pool, next: next, held: map[int64]bool{}}
	e.owner = mkUser(t, pool, next, "发起人", "13900000011")
	e.viewer = mkUser(t, pool, next, "只读", "13900000012")
	e.outsdr = mkUser(t, pool, next, "外人", "13900000013")
	cr := collabrepo.New(pool)
	w, err := cr.CreateTeam(ctx, [3]int64{next(), next(), next()}, "营销组", e.owner)
	if err != nil {
		t.Fatal(err)
	}
	e.ws = w.ID
	if _, err := pool.Exec(ctx, `insert into workspace_member (id, workspace_id, user_id, role)
		values ($1,$2,$3,'viewer')`, next(), e.ws, e.viewer); err != nil {
		t.Fatal(err)
	}
	w2, err := cr.CreateTeam(ctx, [3]int64{next(), next(), next()}, "别的组", e.outsdr)
	if err != nil {
		t.Fatal(err)
	}
	e.otherWS = w2.ID

	s3srv := httptest.NewServer(&fakeS3{objs: map[string][]byte{}})
	t.Cleanup(s3srv.Close)
	store, err := objstore.New(ctx, config.Storage{Bucket: "seeaitv", Region: "us-east-1",
		Endpoint: s3srv.URL, AccessKey: "ak", SecretKey: "sk", SignTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	mrepo := marketingrepo.New(pool)
	e.media = mediaapp.New(mediarepo.New(pool), store, next)
	e.media.AddReferenceChecker(productRefs{mrepo})
	e.media.AddReferenceChecker(characterRefs{mrepo})
	e.media.AddReferenceChecker(fakeSessionRefs{held: e.held})
	lib := wiring.NewMarketingLibrary(wiring.MarketingLibraryInput{Repo: mrepo, Media: e.media, NextID: next,
		Settings: marketingapp.LibrarySettings{MaxProductImages: 10, MaxProducts: 3, MaxCharacters: 10}})

	userIDs := func(ctx context.Context) (int64, bool) {
		v, ok := ctx.Value(userKey{}).(int64)
		return v, ok
	}
	collabH := collabhttp.NewHandler(collabapp.New(cr, next), userIDs)
	requireUser := func(n http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			uid, err := strconv.ParseInt(r.Header.Get("X-Test-User"), 10, 64)
			if err != nil {
				httpx.Fail(w, r, httpx.Err(errcode.Unauthenticated))
				return
			}
			n.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, uid)))
		})
	}
	wsGuard := func(n http.Handler) http.Handler { return collabH.RequireWorkspace(collabhttp.GuardByMethod(n)) }
	h := marketinghttp.NewHandler(wiring.NewMarketingService(wiring.MarketingServiceInput{Repo: mrepo,
		Settings: marketingapp.Settings{MaxProductImages: 10, MaxVariants: 4}}), true)
	h.SetLibrary(lib, func(r *http.Request) int64 { v, _ := userIDs(r.Context()); return v })
	r := chi.NewRouter()
	r.Use(httpx.TraceID)
	h.MountPublic(r)
	h.MountLibrary(r, requireUser, wsGuard)
	e.srv = httptest.NewServer(r)
	t.Cleanup(e.srv.Close)
	return e
}

func pngBytes(t *testing.T, shade uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for x := 0; x < 4; x++ {
		for y := 0; y < 4; y++ {
			img.Set(x, y, color.RGBA{R: shade, G: 10, B: 20, A: 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// wavBytes 是一段 0.01 秒的静音 WAV（文件头嗅探成 audio/wav）。
func wavBytes() []byte {
	data := make([]byte, 160)
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+len(data)))
	b.WriteString("WAVEfmt ")
	_ = binary.Write(&b, binary.LittleEndian, uint32(16))
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))
	_ = binary.Write(&b, binary.LittleEndian, uint32(16000))
	_ = binary.Write(&b, binary.LittleEndian, uint32(16000))
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))
	_ = binary.Write(&b, binary.LittleEndian, uint16(8))
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(data)))
	b.Write(data)
	return b.Bytes()
}

func (e *env) upload(ws int64, purpose string, body []byte) int64 {
	e.t.Helper()
	up, err := e.media.Upload(context.Background(), mediaapp.UploadInput{WorkspaceID: ws, UserID: e.owner,
		Purpose: purpose, Size: int64(len(body)), Body: bytes.NewReader(body)})
	if err != nil {
		e.t.Fatal(err)
	}
	return up.ID
}

func (e *env) readMedia(mid int64) []byte {
	e.t.Helper()
	rc, _, err := e.media.Open(context.Background(), mid)
	if err != nil {
		e.t.Fatalf("取媒体 %d：%v", mid, err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return b
}

// mkPreset 建一个营销角色（平台媒体 workspace_id=0），返回 (预置 ID, 图片 ID 列表, 音频 ID)。
func (e *env) mkPreset(name string, enabled bool) (int64, []int64, int64) {
	e.t.Helper()
	imgs := []int64{e.upload(0, "material_cover", pngBytes(e.t, 1)), e.upload(0, "material_cover", pngBytes(e.t, 2))}
	voice := e.upload(0, "material_cover", wavBytes())
	pid := e.next()
	if _, err := e.pool.Exec(context.Background(), `insert into marketing_preset_character
		(id, name, summary, image_media_ids, appearance, voice_text, voice_media_id, sort, is_enabled)
		values ($1,$2,'清爽少年',$3::jsonb,'圆脸','少年音',$4,1,$5)`,
		pid, name, `["`+sid(imgs[0])+`","`+sid(imgs[1])+`"]`, voice, enabled); err != nil {
		e.t.Fatal(err)
	}
	return pid, imgs, voice
}

func strs(v any) []string {
	arr, _ := v.([]any)
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

func TestAdopt_复制字节且幂等(t *testing.T) {
	e := newCharEnv(t)
	pid, srcImgs, srcVoice := e.mkPreset("柚子君", true)
	path := "/marketing/preset-characters/" + sid(pid) + "/adopt"

	r := e.do("POST", path, e.owner, e.ws, nil)
	if r.status != http.StatusCreated {
		t.Fatalf("首次添加应 201：%d %v", r.status, r.body)
	}
	if r.body["source"] != "preset" || r.body["sourcePresetId"] != sid(pid) || r.body["sourcePresetVersion"] != float64(1) ||
		r.body["appearance"] != "圆脸" || r.body["voiceText"] != "少年音" {
		t.Fatalf("副本来源与内容不对：%v", r.body)
	}
	imgs := strs(r.body["imageMediaIds"])
	voice, _ := r.body["voiceMediaId"].(string)
	if len(imgs) != 2 || voice == "" {
		t.Fatalf("图片 2 张 + 音频都要复制：%v", r.body)
	}
	for i, s := range imgs {
		if s == sid(srcImgs[i]) {
			t.Fatalf("第 %d 张仍是平台媒体 ID（没复制）", i+1)
		}
	}
	if voice == sid(srcVoice) {
		t.Fatal("音频仍是平台媒体 ID（没复制）")
	}
	// 副本属于本工作区，且源平台媒体被删之后照样取得到（真复制了字节，不是共用 object_key）。
	var ws int64
	newImg, _ := strconv.ParseInt(imgs[0], 10, 64)
	if err := e.pool.QueryRow(context.Background(), `select workspace_id from media_object where id = $1`, newImg).
		Scan(&ws); err != nil || ws != e.ws {
		t.Fatalf("副本应属本工作区：%v %d", err, ws)
	}
	want := e.readMedia(srcImgs[0])
	if _, err := e.pool.Exec(context.Background(),
		`update media_object set status = 'deleted' where id = any($1)`, srcImgs); err != nil {
		t.Fatal(err)
	}
	if got := e.readMedia(newImg); !bytes.Equal(got, want) || len(got) == 0 {
		t.Fatal("源媒体删除后副本应仍可取且字节一致")
	}

	// 重复添加同一版：200 回同一份。
	r2 := e.do("POST", path, e.owner, e.ws, nil)
	if r2.status != http.StatusOK || r2.body["id"] != r.body["id"] {
		t.Fatalf("重复添加应 200 回同一份：%d %v", r2.status, r2.body)
	}
	// 「我的」列表出现副本。
	l := e.do("GET", "/marketing/characters", e.viewer, e.ws, nil)
	items, _ := l.body["items"].([]any)
	if l.status != 200 || len(items) != 1 || items[0].(map[string]any)["source"] != "preset" {
		t.Fatalf("我的列表应有 1 个副本：%d %v", l.status, l.body)
	}
}

func TestAdopt_改版得新副本(t *testing.T) {
	e := newCharEnv(t)
	pid, _, _ := e.mkPreset("柚子君", true)
	path := "/marketing/preset-characters/" + sid(pid) + "/adopt"
	r1 := e.do("POST", path, e.owner, e.ws, nil)
	if r1.status != http.StatusCreated {
		t.Fatalf("首次添加：%d %v", r1.status, r1.body)
	}
	// 运营保存一次内容：版本 +1。
	if _, err := e.pool.Exec(context.Background(),
		`update marketing_preset_character set version = 2, appearance = '新形象' where id = $1`, pid); err != nil {
		t.Fatal(err)
	}
	r2 := e.do("POST", path, e.owner, e.ws, nil)
	if r2.status != http.StatusCreated || r2.body["id"] == r1.body["id"] || r2.body["sourcePresetVersion"] != float64(2) {
		t.Fatalf("改版后应得新副本：%d %v", r2.status, r2.body)
	}
	// 名字与 v1 的副本撞了：自动加后缀。
	if r2.body["name"] != "柚子君-营销角色" || r2.body["appearance"] != "新形象" {
		t.Fatalf("撞名应加后缀并带新内容：%v", r2.body)
	}
	// 旧副本不受影响。
	g := e.do("GET", "/marketing/characters/"+r1.body["id"].(string), e.owner, e.ws, nil)
	if g.status != 200 || g.body["appearance"] != "圆脸" || g.body["sourcePresetVersion"] != float64(1) {
		t.Fatalf("旧副本应不变：%v", g.body)
	}
}

func TestAdopt_停用不可添加(t *testing.T) {
	e := newCharEnv(t)
	pid, _, _ := e.mkPreset("柚子君", true)
	path := "/marketing/preset-characters/" + sid(pid) + "/adopt"
	r1 := e.do("POST", path, e.owner, e.ws, nil)
	if r1.status != http.StatusCreated {
		t.Fatalf("首次添加：%d %v", r1.status, r1.body)
	}
	if l := e.do("GET", "/marketing/preset-characters", 0, 0, nil); len(l.body["items"].([]any)) != 1 {
		t.Fatalf("启用时公开列表应有 1 个：%v", l.body)
	}
	if _, err := e.pool.Exec(context.Background(),
		`update marketing_preset_character set is_enabled = false where id = $1`, pid); err != nil {
		t.Fatal(err)
	}
	if r := e.do("POST", path, e.outsdr, e.otherWS, nil); r.status != http.StatusNotFound {
		t.Fatalf("停用后添加应 404：%d %v", r.status, r.body)
	}
	if l := e.do("GET", "/marketing/preset-characters", 0, 0, nil); len(l.body["items"].([]any)) != 0 {
		t.Fatalf("停用后公开列表不应出现：%v", l.body)
	}
	// 已有副本照常可读、媒体照常可取。
	g := e.do("GET", "/marketing/characters/"+r1.body["id"].(string), e.owner, e.ws, nil)
	if g.status != 200 {
		t.Fatalf("停用后已有副本应照常可读：%d", g.status)
	}
	mid, _ := strconv.ParseInt(strs(g.body["imageMediaIds"])[0], 10, 64)
	if len(e.readMedia(mid)) == 0 {
		t.Fatal("停用后副本的图应照常可取")
	}
}

func TestCharacterRoutes_只读成员403(t *testing.T) {
	e := newCharEnv(t)
	pid, _, _ := e.mkPreset("柚子君", true)
	img := e.upload(e.ws, "upload_reference", pngBytes(t, 9))
	body := map[string]any{"name": "小美", "imageMediaIds": []string{sid(img)}}
	if r := e.do("GET", "/marketing/characters", e.viewer, e.ws, nil); r.status != 200 {
		t.Fatalf("只读成员列表应 200：%d", r.status)
	}
	created := e.do("POST", "/marketing/characters", e.owner, e.ws, body)
	if created.status != http.StatusCreated {
		t.Fatalf("owner 新建应 201：%d %v", created.status, created.body)
	}
	cid := created.body["id"].(string)
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/marketing/characters", body},
		{"PUT", "/marketing/characters/" + cid, map[string]any{"name": "改", "imageMediaIds": []string{sid(img)}, "version": 1}},
		{"DELETE", "/marketing/characters/" + cid, nil},
		{"POST", "/marketing/preset-characters/" + sid(pid) + "/adopt", nil},
	} {
		if r := e.do(c.method, c.path, e.viewer, e.ws, c.body); r.status != http.StatusForbidden {
			t.Fatalf("只读成员 %s %s 应 403，得 %d %v", c.method, c.path, r.status, r.body)
		}
	}
	// 外人用别的工作区取不到。
	if r := e.do("GET", "/marketing/characters/"+cid, e.outsdr, e.otherWS, nil); r.status != http.StatusNotFound {
		t.Fatalf("他人工作区应 404：%d", r.status)
	}
}

func TestCharacter_接口校验重名与媒体保护(t *testing.T) {
	e := newCharEnv(t)
	img := e.upload(e.ws, "upload_reference", pngBytes(t, 3))
	voice := e.upload(e.ws, "upload_reference", wavBytes())
	ok := map[string]any{"name": "小美", "imageMediaIds": []string{sid(img)}, "voiceMediaId": sid(voice),
		"appearance": "短发", "voiceText": "温柔"}
	c := e.do("POST", "/marketing/characters", e.owner, e.ws, ok)
	if c.status != http.StatusCreated || c.body["voiceMediaId"] != sid(voice) || c.body["source"] != "user" {
		t.Fatalf("新建：%d %v", c.status, c.body)
	}
	// 重名（大小写无关由库保证，这里验中文）→ 409 field=name code=duplicate。
	dup := e.do("POST", "/marketing/characters", e.owner, e.ws, ok)
	det, _ := dup.body["details"].(map[string]any)
	if dup.status != http.StatusConflict || det["field"] != "name" || det["code"] != "duplicate" {
		t.Fatalf("重名应 409：%d %v", dup.status, dup.body)
	}
	// 音频当图片、图片当音频、两段音频、来源字段 → 40001。
	bad := []map[string]any{
		{"name": "甲", "imageMediaIds": []string{sid(voice)}},
		{"name": "乙", "imageMediaIds": []string{sid(img)}, "voiceMediaId": sid(img)},
		{"name": "丙", "imageMediaIds": []string{sid(img)}, "voiceMediaId": []string{sid(voice), sid(voice)}},
		{"name": "丁", "imageMediaIds": []string{sid(img)}, "source": "preset"},
		{"name": "戊", "imageMediaIds": []string{}},
	}
	for i, b := range bad {
		if r := e.do("POST", "/marketing/characters", e.owner, e.ws, b); r.status != http.StatusBadRequest ||
			r.code() != int(errcode.InvalidArgument) {
			t.Fatalf("第 %d 个坏请求应 40001：%d %v", i+1, r.status, r.body)
		}
	}
	// 未删除的角色引用着：图片与音频都删不掉，文案点名「营销角色」。
	for _, mid := range []int64{img, voice} {
		err := e.media.Delete(context.Background(), e.ws, mid)
		if err == nil || !strings.Contains(err.Error(), "营销角色") {
			t.Fatalf("媒体 %d 应被「营销角色」引用拦下：%v", mid, err)
		}
	}
	// 删除角色 → 自有上传的图与音频一并尝试删除（没有别的引用 → 删掉）。
	if r := e.do("DELETE", "/marketing/characters/"+c.body["id"].(string), e.owner, e.ws, nil); r.status != http.StatusNoContent {
		t.Fatalf("删除应 204：%d %v", r.status, r.body)
	}
	if e.mediaStatus(img) != "deleted" || e.mediaStatus(voice) != "deleted" {
		t.Fatalf("删除角色后自有媒体应被清理：%s %s", e.mediaStatus(img), e.mediaStatus(voice))
	}
	// 删除后可重名。
	img2 := e.upload(e.ws, "upload_reference", pngBytes(t, 4))
	if r := e.do("POST", "/marketing/characters", e.owner, e.ws, map[string]any{"name": "小美",
		"imageMediaIds": []string{sid(img2)}}); r.status != http.StatusCreated {
		t.Fatalf("删除后应可重名：%d %v", r.status, r.body)
	}
}
