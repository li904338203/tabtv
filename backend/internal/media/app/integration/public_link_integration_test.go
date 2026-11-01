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
	"net/http"
	"strconv"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	mediarepo "github.com/li904338203/tabtv/backend/internal/media/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

// 公开图片直连对象存储（2026-11-02）：钉住的都是「错了也能出图、只是又慢又费」的：
//  1. 给的是缩略图（JPEG、几十 KB），不是原图；
//  2. 缩略图只生成一次（库里一行派生件），第二次直接复用、签名地址也复用（浏览器缓存才命中得了）；
//  3. 缩略图不进素材库，并随源软删。
func TestPublicLink_缩略图生成一次_直连地址可取(t *testing.T) {
	ctx := context.Background()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	store, _ := testsupport.FakeObjectStore(t)
	svc := mediaapp.New(mediarepo.New(pool), store, gen.Next)

	// 模拟照片：伪随机噪声（PNG 压不动、JPEG 能大幅压缩），且不透明。
	img := image.NewRGBA(image.Rect(0, 0, 1200, 800))
	seed := uint32(7)
	for y := 0; y < 800; y++ {
		for x := 0; x < 1200; x++ {
			seed = seed*1664525 + 1013904223
			img.Set(x, y, color.RGBA{uint8(seed >> 24), uint8(seed >> 16), uint8(seed >> 8), 255})
		}
	}
	var src bytes.Buffer
	if err := png.Encode(&src, img); err != nil {
		t.Fatal(err)
	}
	mid := gen.Next()
	key := "upload_reference/test/" + strconv.FormatInt(mid, 10) + ".png"
	if _, err := store.S3().PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(store.Bucket()), Key: aws.String(key),
		Body: bytes.NewReader(src.Bytes())}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into media_object (id, workspace_id, bucket, object_key, content_type, size_bytes,
		                          purpose, created_by_user_id, status, width, height)
		values ($1, $2, 'seeaitv', $3, 'image/png', $4, 'upload_reference', 1, 'active', 1200, 800)`,
		mid, ws, key, src.Len()); err != nil {
		t.Fatal(err)
	}

	u1, err := svc.PublicLink(ctx, mid, 320, false)
	if err != nil {
		t.Fatalf("签直连地址：%v / %v", err, errors.Unwrap(err))
	}
	resp, err := http.Get(u1)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("直连地址取图 HTTP %d", resp.StatusCode)
	}
	th, err := jpeg.Decode(bytes.NewReader(got))
	if err != nil {
		t.Fatalf("不透明的照片应给 JPEG 缩略图：%v", err)
	}
	if th.Bounds().Dx() != 320 {
		t.Errorf("缩略图宽应为 320，实际 %d", th.Bounds().Dx())
	}
	if len(got)*5 > src.Len() {
		t.Errorf("缩略图 %d 字节，相对原图 %d 字节没有明显变小", len(got), src.Len())
	}

	u2, err := svc.PublicLink(ctx, mid, 320, false)
	if err != nil || u2 != u1 {
		t.Errorf("第二次应复用同一个签名地址（浏览器缓存才命中得了）：%v", err)
	}
	var n int
	_ = pool.QueryRow(ctx, `select count(*) from media_object where source_media_id = $1 and derive_kind = 'public_thumb'`, mid).Scan(&n)
	if n != 1 {
		t.Fatalf("缩略图应只生成一次，实际 %d 行", n)
	}

	// 不进素材库
	list, err := mediarepo.New(pool).ListByWorkspace(ctx, ws, "", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range list {
		if m.ID != mid {
			t.Errorf("素材库里出现了缩略图 %d", m.ID)
		}
	}
	// 随源软删
	if err := svc.Delete(ctx, ws, mid); err != nil {
		t.Fatalf("删源：%v", err)
	}
	_ = pool.QueryRow(ctx, `select count(*) from media_object where source_media_id = $1 and derive_kind = 'public_thumb' and status = 'active'`, mid).Scan(&n)
	if n != 0 {
		t.Errorf("源删了，缩略图应随之软删，还剩 %d 行", n)
	}
}
