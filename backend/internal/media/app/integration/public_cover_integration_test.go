//go:build integration

package integration

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"strconv"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	mediarepo "github.com/li904338203/tabtv/backend/internal/media/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 公开作品的封面图（2026-11-01，`/showcase/{workId}/cover`）。
//
// ⚠️ 钉住的都是「错了不报错」的：
//  1. 视频给的是封面帧（十几 KB 的 JPEG），**不是整段视频** —— 吐整段的话作品墙照常显示，只是又回到
//     「几段视频一起下、占满浏览器连接、页面接口排队好几秒」；
//  2. 封面帧还没截出来、或音频这类没有封面的，回 404 而不是整段文件；
//  3. 原来的取图（OpenPublic）不受影响：视频播放走它，不带宽度时它默认按 320 缩，视频必须原样给。
func TestOpenPublicCover_视频给封面帧图片给缩略图其余404(t *testing.T) {
	ctx := context.Background()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	store, _ := testsupport.FakeObjectStore(t)
	svc := mediaapp.New(mediarepo.New(pool), store, gen.Next)

	put := func(mime, purpose string, body []byte, source int64, deriveKind string) int64 {
		t.Helper()
		mid := gen.Next()
		key := "test/" + mime + "/" + strconv.FormatInt(mid, 10)
		if _, err := store.S3().PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String("seeaitv"), Key: aws.String(key), Body: bytes.NewReader(body),
		}); err != nil {
			t.Fatal(err)
		}
		var src, kind any
		if source != 0 {
			src, kind = source, deriveKind
		}
		if _, err := pool.Exec(ctx, `
			insert into media_object (id, workspace_id, bucket, object_key, content_type, size_bytes,
			                          purpose, created_by_user_id, status, source_media_id, derive_kind)
			values ($1, $2, 'seeaitv', $3, $4, $5, $6, 1, 'active', $7, $8)`,
			mid, ws, key, mime, len(body), purpose, src, kind); err != nil {
			t.Fatal(err)
		}
		return mid
	}

	video := put("video/mp4", "generation_output", bytes.Repeat([]byte("mp4-bytes."), 1000), 0, "")
	put("image/jpeg", "generation_output", encodeJPEG(t, 1280, 720), video, "video_poster")
	bare := put("video/mp4", "generation_output", []byte("no poster yet"), 0, "")
	picture := put("image/png", "generation_output", encodePNG(t, 1024, 1024), 0, "")
	sound := put("audio/mpeg", "generation_output", []byte("mp3"), 0, "")

	body, m, err := svc.OpenPublicCover(ctx, video, 320)
	if err != nil {
		t.Fatalf("视频的封面：%v", err)
	}
	if m.ContentType != "image/jpeg" {
		t.Fatalf("视频的封面应是封面帧（image/jpeg），得到 %s", m.ContentType)
	}
	if w := decodedWidth(t, body); w != 320 {
		t.Errorf("视频的封面应缩到 320 宽，得到 %d", w)
	}

	body, m, err = svc.OpenPublicCover(ctx, picture, 160)
	if err != nil {
		t.Fatalf("图片的封面：%v", err)
	}
	if m.ContentType != "image/png" {
		t.Errorf("图片的封面就是它自己的缩略图，得到 %s", m.ContentType)
	}
	if w := decodedWidth(t, body); w != 160 {
		t.Errorf("图片的封面应缩到 160 宽，得到 %d", w)
	}

	for name, mid := range map[string]int64{"还没截封面帧的视频": bare, "音频": sound} {
		if _, _, err := svc.OpenPublicCover(ctx, mid, 320); codeOf(err) != errcode.NotFound {
			t.Errorf("%s：应回 404，而不是吐出整个文件，得到 %v", name, err)
		}
	}

	// 原来的取图不受影响：视频带宽度也原样给整段（视频播放走它）
	raw, m, err := svc.OpenPublic(ctx, video, 320)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	got, _ := io.ReadAll(raw)
	if m.ContentType != "video/mp4" || len(got) != 10000 {
		t.Errorf("OpenPublic 对视频应原样给整段，得到 %s / %d 字节", m.ContentType, len(got))
	}
}

func codeOf(err error) errcode.Code {
	var e *httpx.BizError
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}

func testImage(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y += 8 {
		for x := 0; x < w; x += 8 {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 120, A: 255})
		}
	}
	return img
}

func encodeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, testImage(w, h), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encodePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, testImage(w, h)); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func decodedWidth(t *testing.T, body io.ReadCloser) int {
	t.Helper()
	defer body.Close()
	cfg, _, err := image.DecodeConfig(body)
	if err != nil {
		t.Fatalf("封面不是能解码的图片：%v", err)
	}
	return cfg.Width
}
