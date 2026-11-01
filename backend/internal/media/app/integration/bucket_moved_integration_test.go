//go:build integration

package integration

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	mediarepo "github.com/li904338203/tabtv/backend/internal/media/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

// 换存储之后（2026-11-02 迁到火山 TOS）：行里记的 bucket 还是旧桶名，文件已按原键拷到新桶。
// 读取必须用当前配置的桶 —— 照行里的旧桶名取的话，每个旧文件都 404，而新上传的一切正常。
func TestOpen_行里记的是旧桶名也从当前桶取(t *testing.T) {
	ctx := context.Background()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	store, _ := testsupport.FakeObjectStore(t)
	svc := mediaapp.New(mediarepo.New(pool), store, gen.Next)

	mid := gen.Next()
	key := "migrated/" + strconv.FormatInt(mid, 10) + ".txt"
	body := []byte("moved to a new bucket")
	if _, err := store.S3().PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(store.Bucket()), Key: aws.String(key), Body: bytes.NewReader(body),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into media_object (id, workspace_id, bucket, object_key, content_type, size_bytes,
		                          purpose, created_by_user_id, status)
		values ($1, $2, 'old-minio-bucket', $3, 'text/plain', $4, 'generation_output', 1, 'active')`,
		mid, ws, key, len(body)); err != nil {
		t.Fatal(err)
	}
	rc, _, err := svc.Open(ctx, mid)
	if err != nil {
		t.Fatalf("行里是旧桶名时应从当前桶取：%v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, body) {
		t.Fatalf("取回内容不对：%q", got)
	}
}
