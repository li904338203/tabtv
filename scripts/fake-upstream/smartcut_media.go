package main

// 智能剪辑的片段级生成（docs/agent/smartcut SCT-38~39；SCT-42 的 E2E 复用）。
//
// 火山 MediaKit 去字幕（SCT-38）：
//
//   - `POST /api/v1/tools/erase-video-subtitle-pro`：Bearer；正文 `{video_url, mode?}`，video_url 必须是 http(s) 地址
//     （只收地址，内联数据回 400）→ `{"task_id": "erase-N"}`；
//   - `GET /api/v1/tasks/{task_id}`：第一次回 processing，第二次回 completed，结果在 `result.video_url`
//     （本地 `/files/erase-N.mp4`，一段真的 MP4）；根上照样回显输入的 video_url —— 验「只认 result.video_url」。
//
// 配乐与音效（SCT-39）：
//
//   - 火山豆包音乐：Action 风格的 OpenAPI，挂在 `/volc/` 下（Provider 的 base 配 `http://127.0.0.1:8195/volc`）。
//     `POST /volc/?Action=GenBGM&Version=2024-08-12`：V4 签名头齐全（`Authorization` 以 `HMAC-SHA256 Credential=` 开头、
//     `X-Date`、`X-Content-Sha256` 与正文的哈希一致）、正文 `{Text, Duration}`（30~60 秒）→ `{Result:{TaskID}}`；
//     `POST /volc/?Action=QuerySong&Version=2024-08-12` `{TaskID}`：第一次 Status 1、第二次 Status 2 + `SongDetail.AudioUrl`；
//   - ElevenLabs：`POST /v1/music?output_format=mp3_*`，`xi-api-key`，`{prompt, music_length_ms}` → 直接回 MP3 字节；
//   - 可灵视频配音效：`POST /v1/audio/video-to-audio`（Bearer，`video_url` 只收 http(s)）→ `{code:0, data:{task_id}}`；
//     `GET /v1/audio/video-to-audio/{id}`：第一次 processing、第二次 succeed + `task_result.audios[0].url_mp3`。
//
// 结果都是本地 `/files/` 下一段真的 MP4 / MP3（量得出时长）。
//
// ⚠️ 只校验**形状**（证明「我们拼对了」）：不去取 video_url（沙箱里它指向本机的存储，真实上游取不到，这里也不装）；
// V4 签名只核对头的形状与正文哈希，不重算签名（假上游没有 SK；完整的重算在 backend 的 provider/app 单测里）。
// 收到的请求记在 `GET /_probe/smartcut/media`，测试据此核对发出去的是截出来的那一段。
// 下一次查询的结果由 `POST /_probe/smartcut/media/next?outcome=failed` 设（一次有效，三家共用）。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const clipGenProbeKeep = 50

// clipGenRecord 是一次片段级生成请求里与断言有关的部分。
type clipGenRecord struct {
	Seq      int64  `json:"seq"`
	Tool     string `json:"tool"`
	TaskID   string `json:"taskId,omitempty"`
	Bearer   bool   `json:"bearer"`
	VideoURL string `json:"videoUrl,omitempty"`
	Mode     string `json:"mode,omitempty"`
	// Prompt / Seconds / Instrumental 配乐与音效的描述、时长（秒）、是否纯音乐；Signed V4 签名头齐全且正文哈希对得上。
	Prompt       string `json:"prompt,omitempty"`
	Seconds      int    `json:"seconds,omitempty"`
	Instrumental *bool  `json:"instrumental,omitempty"`
	Signed       bool   `json:"signed,omitempty"`
	Outcome      string `json:"outcome"`
	At           string `json:"at"`
}

type clipGenState struct {
	mu    sync.Mutex
	seq   atomic.Int64
	n     atomic.Int64
	recs  []clipGenRecord
	next  string
	tasks map[string]*eraseTask
}

type eraseTask struct {
	videoURL string
	polls    int
}

func (c *clipGenState) record(r clipGenRecord) {
	r.Seq, r.At = c.seq.Add(1), time.Now().UTC().Format(time.RFC3339Nano)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = append(c.recs, r)
	if len(c.recs) > clipGenProbeKeep {
		c.recs = append([]clipGenRecord(nil), c.recs[len(c.recs)-clipGenProbeKeep:]...)
	}
}

func (c *clipGenState) takeNext() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.next
	c.next = ""
	return n
}

func (c *clipGenState) handleProbe(w http.ResponseWriter, _ *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	writeJSON(w, map[string]any{"items": append([]clipGenRecord{}, c.recs...)})
}

func (c *clipGenState) handleNext(w http.ResponseWriter, r *http.Request) {
	out := r.URL.Query().Get("outcome")
	if out != "failed" {
		http.Error(w, "outcome 只能是 failed", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	c.next = out
	c.mu.Unlock()
	writeJSON(w, map[string]any{"next": out})
}

// mediaKitError 照 MediaKit 的错误结构回（`error{code,message}`；结构待核实，见 dev/01 R-19）。
func mediaKitError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": msg}})
}

func (s *server) mediaKitErase(w http.ResponseWriter, r *http.Request) {
	rec := clipGenRecord{Tool: "desubtitle", Bearer: strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ")}
	fail := func(status int, code, msg string) {
		rec.Outcome = "bad_request"
		s.clipGen.record(rec)
		mediaKitError(w, status, code, msg)
	}
	if r.Method != http.MethodPost {
		fail(http.StatusMethodNotAllowed, "MethodNotAllowed", "要 POST")
		return
	}
	if !rec.Bearer {
		fail(http.StatusUnauthorized, "AuthFailed", "缺少 Authorization: Bearer <API key>")
		return
	}
	var body struct {
		VideoURL string `json:"video_url"`
		Mode     string `json:"mode"`
	}
	raw, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(raw, &body); err != nil {
		fail(http.StatusBadRequest, "InvalidParameter", "正文不是 JSON")
		return
	}
	rec.VideoURL, rec.Mode = body.VideoURL, body.Mode
	switch {
	case !strings.HasPrefix(body.VideoURL, "http://") && !strings.HasPrefix(body.VideoURL, "https://"):
		fail(http.StatusBadRequest, "InvalidParameter", "video_url 要是 http(s) 地址（不收内联数据）")
		return
	case body.Mode != "" && body.Mode != "subtitle" && body.Mode != "text":
		fail(http.StatusBadRequest, "InvalidParameter", "mode 只能是 subtitle / text")
		return
	}
	id := fmt.Sprintf("erase-%d", s.clipGen.n.Add(1))
	s.clipGen.mu.Lock()
	if s.clipGen.tasks == nil {
		s.clipGen.tasks = map[string]*eraseTask{}
	}
	s.clipGen.tasks[id] = &eraseTask{videoURL: body.VideoURL}
	s.clipGen.mu.Unlock()
	rec.TaskID, rec.Outcome = id, "submitted"
	s.clipGen.record(rec)
	log.Printf("  去字幕（MediaKit）：%s 模式 %q → %s", body.VideoURL, body.Mode, id)
	writeJSON(w, map[string]any{"task_id": id, "status": "pending"})
}

func (s *server) mediaKitTask(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/tasks/")
	rec := clipGenRecord{Tool: "desubtitle", TaskID: id, Bearer: strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ")}
	if !rec.Bearer {
		rec.Outcome = "bad_request"
		s.clipGen.record(rec)
		mediaKitError(w, http.StatusUnauthorized, "AuthFailed", "缺少 Authorization: Bearer <API key>")
		return
	}
	s.clipGen.mu.Lock()
	task, ok := s.clipGen.tasks[id]
	polls := 0
	if ok {
		task.polls++
		polls = task.polls
	}
	s.clipGen.mu.Unlock()
	switch {
	case !ok:
		rec.Outcome = "unknown_task"
		s.clipGen.record(rec)
		mediaKitError(w, http.StatusNotFound, "TaskNotFound", "没有这个任务")
		return
	case polls == 1:
		rec.Outcome = "processing"
		s.clipGen.record(rec)
		writeJSON(w, map[string]any{"task_id": id, "status": "processing", "progress": 50})
		return
	}
	rec.VideoURL = task.videoURL
	if s.clipGen.takeNext() == "failed" {
		rec.Outcome = "failed"
		s.clipGen.record(rec)
		writeJSON(w, map[string]any{"task_id": id, "status": "failed",
			"error": map[string]any{"code": "InvalidVideo", "message": "input resolution exceeds 2K"}})
		return
	}
	rec.Outcome = "completed"
	s.clipGen.record(rec)
	writeJSON(w, map[string]any{"task_id": id, "status": "completed", "video_url": task.videoURL,
		"result": map[string]any{"video_url": s.addr + "/files/" + id + ".mp4"}})
}

// ---------------------------------------------------------------- 配乐与音效（SCT-39）

// newTask 记一个异步任务（查询次数从 0 起），返回任务 ID。
func (c *clipGenState) newTask(prefix, input string) string {
	id := fmt.Sprintf("%s-%d", prefix, c.n.Add(1))
	c.mu.Lock()
	if c.tasks == nil {
		c.tasks = map[string]*eraseTask{}
	}
	c.tasks[id] = &eraseTask{videoURL: input}
	c.mu.Unlock()
	return id
}

// poll 查一次任务：不认识的回 ok=false；否则回这是第几次查询。
func (c *clipGenState) poll(id string) (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.tasks[id]
	if !ok {
		return 0, false
	}
	t.polls++
	return t.polls, true
}

// volcSigned V4 签名头的形状与正文哈希（假上游没有 SK，不重算签名）。
func volcSigned(r *http.Request, body []byte) bool {
	sum := sha256.Sum256(body)
	_, err := time.Parse("20060102T150405Z", r.Header.Get("X-Date"))
	return err == nil && strings.HasPrefix(r.Header.Get("Authorization"), "HMAC-SHA256 Credential=") &&
		strings.Contains(r.Header.Get("Authorization"), "SignedHeaders=") &&
		r.Header.Get("X-Content-Sha256") == hex.EncodeToString(sum[:])
}

func volcOpenAPIError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ResponseMetadata": map[string]any{
		"RequestId": "fake", "Error": map[string]any{"Code": code, "Message": msg}}})
}

// volcMusic 火山豆包音乐：GenBGM 提交、QuerySong 查询。
func (s *server) volcMusic(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	action := r.URL.Query().Get("Action")
	rec := clipGenRecord{Tool: "music_volc", Signed: volcSigned(r, body)}
	fail := func(status int, code, msg string) {
		rec.Outcome = "bad_request"
		s.clipGen.record(rec)
		volcOpenAPIError(w, status, code, msg)
	}
	switch {
	case r.Method != http.MethodPost:
		fail(http.StatusMethodNotAllowed, "InvalidMethod", "要 POST")
		return
	case !rec.Signed:
		fail(http.StatusUnauthorized, "SignatureDoesNotMatch", "缺少 V4 签名头，或 X-Content-Sha256 与正文对不上")
		return
	case r.URL.Query().Get("Version") != "2024-08-12":
		fail(http.StatusBadRequest, "InvalidVersion", "Version 要是 2024-08-12")
		return
	}
	switch action {
	case "GenBGM":
		var req struct {
			Text     string `json:"Text"`
			Duration int    `json:"Duration"`
		}
		if json.Unmarshal(body, &req) != nil || strings.TrimSpace(req.Text) == "" {
			fail(http.StatusBadRequest, "InvalidParameter", "Text 不能为空")
			return
		}
		rec.Prompt, rec.Seconds = req.Text, req.Duration
		if req.Duration < 30 || req.Duration > 60 {
			fail(http.StatusBadRequest, "InvalidParameter", "Duration 要在 30~60 秒")
			return
		}
		rec.TaskID = s.clipGen.newTask("bgm", "")
		rec.Outcome = "submitted"
		s.clipGen.record(rec)
		log.Printf("  豆包音乐：%q %d 秒 → %s", req.Text, req.Duration, rec.TaskID)
		writeJSON(w, map[string]any{"ResponseMetadata": map[string]any{"RequestId": "fake", "Action": "GenBGM"},
			"Result": map[string]any{"TaskID": rec.TaskID}})
	case "QuerySong":
		var req struct {
			TaskID string `json:"TaskID"`
		}
		_ = json.Unmarshal(body, &req)
		rec.TaskID = req.TaskID
		n, ok := s.clipGen.poll(req.TaskID)
		switch {
		case !ok:
			fail(http.StatusNotFound, "TaskNotFound", "没有这个任务")
			return
		case n == 1:
			rec.Outcome = "processing"
			s.clipGen.record(rec)
			writeJSON(w, map[string]any{"Result": map[string]any{"TaskID": req.TaskID, "Status": 1, "Progress": 50}})
			return
		}
		if s.clipGen.takeNext() == "failed" {
			rec.Outcome = "failed"
			s.clipGen.record(rec)
			writeJSON(w, map[string]any{"Result": map[string]any{"TaskID": req.TaskID, "Status": 3,
				"FailureReason": map[string]any{"Code": 50412, "Msg": "text contains sensitive content"}}})
			return
		}
		rec.Outcome = "completed"
		s.clipGen.record(rec)
		writeJSON(w, map[string]any{"Result": map[string]any{"TaskID": req.TaskID, "Status": 2, "Progress": 100,
			"SongDetail": map[string]any{"AudioUrl": s.addr + "/files/" + req.TaskID + ".mp3"}}})
	default:
		fail(http.StatusBadRequest, "InvalidAction", "不认识的 Action："+action)
	}
}

// elevenLabsMusic ElevenLabs：同步，直接回 MP3 字节。
func (s *server) elevenLabsMusic(w http.ResponseWriter, r *http.Request) {
	rec := clipGenRecord{Tool: "music_eleven", Bearer: r.Header.Get("xi-api-key") != ""}
	fail := func(status int, msg string) {
		rec.Outcome = "bad_request"
		s.clipGen.record(rec)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"detail": []any{map[string]any{"loc": []string{"body"}, "msg": msg,
			"type": "value_error"}}})
	}
	var req struct {
		Prompt            string `json:"prompt"`
		MusicLengthMs     int    `json:"music_length_ms"`
		ForceInstrumental *bool  `json:"force_instrumental"`
	}
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.Method != http.MethodPost:
		fail(http.StatusMethodNotAllowed, "要 POST")
		return
	case !rec.Bearer:
		fail(http.StatusUnauthorized, "缺少 xi-api-key")
		return
	case !strings.HasPrefix(r.URL.Query().Get("output_format"), "mp3_"):
		fail(http.StatusUnprocessableEntity, "output_format 要显式给 mp3（缺省 auto 由上游定格式）")
		return
	case json.Unmarshal(body, &req) != nil || strings.TrimSpace(req.Prompt) == "":
		fail(http.StatusUnprocessableEntity, "prompt 不能为空")
		return
	}
	rec.Prompt, rec.Seconds, rec.Instrumental = req.Prompt, req.MusicLengthMs/1000, req.ForceInstrumental
	if req.MusicLengthMs < 3000 || req.MusicLengthMs > 600000 {
		fail(http.StatusUnprocessableEntity, "music_length_ms 要在 3000~600000")
		return
	}
	rec.Outcome = "audio"
	s.clipGen.record(rec)
	log.Printf("  ElevenLabs 配乐：%q %d 毫秒", req.Prompt, req.MusicLengthMs)
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("song-id", fmt.Sprintf("song-%d", s.clipGen.n.Add(1)))
	_, _ = w.Write(decodeB64(twoSecondMP3))
}

// klingV2A 可灵视频配音效：提交与查询（路径以 /v1/audio/video-to-audio 开头）。
func (s *server) klingV2A(w http.ResponseWriter, r *http.Request) {
	rec := clipGenRecord{Tool: "sfx", Bearer: strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ")}
	fail := func(status, code int, msg string) {
		rec.Outcome = "bad_request"
		s.clipGen.record(rec)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": msg, "request_id": "fake"})
	}
	if !rec.Bearer {
		fail(http.StatusUnauthorized, 1001, "Authorization is empty")
		return
	}
	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/v1/audio/video-to-audio"), "/")
	if r.Method == http.MethodGet && id != "" {
		rec.TaskID = id
		n, ok := s.clipGen.poll(id)
		switch {
		case !ok:
			fail(http.StatusNotFound, 1203, "task not found")
			return
		case n == 1:
			rec.Outcome = "processing"
			s.clipGen.record(rec)
			writeJSON(w, map[string]any{"code": 0, "message": "SUCCEED", "data": map[string]any{"task_id": id, "task_status": "processing"}})
			return
		}
		if s.clipGen.takeNext() == "failed" {
			rec.Outcome = "failed"
			s.clipGen.record(rec)
			writeJSON(w, map[string]any{"code": 0, "data": map[string]any{"task_id": id, "task_status": "failed",
				"task_status_msg": "Failure to pass the risk control system"}})
			return
		}
		rec.Outcome = "succeed"
		s.clipGen.record(rec)
		writeJSON(w, map[string]any{"code": 0, "message": "SUCCEED", "data": map[string]any{"task_id": id, "task_status": "succeed",
			"task_result": map[string]any{
				"videos": []any{map[string]any{"id": id + "-v", "url": s.addr + "/files/" + id + "-dubbed.mp4"}},
				"audios": []any{map[string]any{"id": id + "-a", "url_mp3": s.addr + "/files/" + id + ".mp3",
					"url_wav": s.addr + "/files/" + id + ".wav"}}}}})
		return
	}
	if r.Method != http.MethodPost || id != "" {
		fail(http.StatusNotFound, 1202, "the requested method is invalid")
		return
	}
	var req struct {
		VideoURL          string `json:"video_url"`
		SoundEffectPrompt string `json:"sound_effect_prompt"`
	}
	body, _ := io.ReadAll(r.Body)
	if json.Unmarshal(body, &req) != nil {
		fail(http.StatusBadRequest, 1200, "invalid request parameters")
		return
	}
	rec.VideoURL, rec.Prompt = req.VideoURL, req.SoundEffectPrompt
	if !strings.HasPrefix(req.VideoURL, "http://") && !strings.HasPrefix(req.VideoURL, "https://") {
		fail(http.StatusBadRequest, 1201, "video_url 要是 http(s) 地址（不收内联数据）")
		return
	}
	if len([]rune(req.SoundEffectPrompt)) > 200 {
		fail(http.StatusBadRequest, 1201, "sound_effect_prompt 不能超过 200 字")
		return
	}
	rec.TaskID = s.clipGen.newTask("v2a", req.VideoURL)
	rec.Outcome = "submitted"
	s.clipGen.record(rec)
	log.Printf("  可灵视频配音效：%s %q → %s", req.VideoURL, req.SoundEffectPrompt, rec.TaskID)
	writeJSON(w, map[string]any{"code": 0, "message": "SUCCEED", "data": map[string]any{"task_id": rec.TaskID, "task_status": "submitted"}})
}
