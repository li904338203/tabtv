package objstore

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/li904338203/tabtv/backend/internal/platform/config"
)

func newTestClient(t *testing.T, cfg config.Storage) *Client {
	t.Helper()
	if cfg.Bucket == "" {
		cfg.Bucket = "tabtv-test"
	}
	cfg.AccessKey, cfg.SecretKey = "ak", "sk"
	c, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TOS 的 S3 端点只认 virtual-hosted：桶名要出现在主机名里，而不是路径的第一段。
func TestPresign_寻址方式跟着配置走(t *testing.T) {
	tos := newTestClient(t, config.Storage{Endpoint: "https://tos-s3-cn-beijing.volces.com", Region: "cn-beijing",
		PathStyle: false, SignTTL: 3600e9})
	u, err := tos.PresignGet(context.Background(), "media/a.png")
	if err != nil {
		t.Fatal(err)
	}
	p, _ := url.Parse(u)
	if p.Host != "tabtv-test.tos-s3-cn-beijing.volces.com" || p.Path != "/media/a.png" {
		t.Errorf("TOS 应为 virtual-hosted 地址，实际 host=%s path=%s", p.Host, p.Path)
	}

	minio := newTestClient(t, config.Storage{Endpoint: "http://127.0.0.1:9100", Region: "us-east-1",
		PathStyle: true, SignTTL: 3600e9})
	u, _ = minio.PresignGet(context.Background(), "media/a.png")
	p, _ = url.Parse(u)
	if p.Host != "127.0.0.1:9100" || p.Path != "/tabtv-test/media/a.png" {
		t.Errorf("MinIO 应为 path-style 地址，实际 host=%s path=%s", p.Host, p.Path)
	}
}

// 预签名地址不能带 x-amz-checksum-mode 之类的签名头：上游 / 浏览器取图时不会带上它，签名就对不上。
func TestPresign_不带校验和参数(t *testing.T) {
	c := newTestClient(t, config.Storage{Endpoint: "https://tos-s3-cn-beijing.volces.com", Region: "cn-beijing", SignTTL: 3600e9})
	u, _ := c.PresignGet(context.Background(), "media/a.png")
	if strings.Contains(strings.ToLower(u), "checksum") {
		t.Errorf("预签名地址里出现了校验和参数：%s", u)
	}
}

// 公共前缀：预签名与直接拿 S3() 调用都要加上，且不改调用方的入参。
func TestPrefix_统一加在客户端上(t *testing.T) {
	c := newTestClient(t, config.Storage{Endpoint: "https://tos-s3-cn-beijing.volces.com", Region: "cn-beijing",
		SignTTL: 3600e9, Prefix: "local"})
	u, _ := c.PresignGet(context.Background(), "media/a.png")
	p, _ := url.Parse(u)
	if p.Path != "/local/media/a.png" {
		t.Errorf("预签名地址应带前缀，实际 path=%s", p.Path)
	}
	in := &s3.GetObjectInput{Bucket: aws.String("tabtv-test"), Key: aws.String("media/b.png")}
	out := withPrefix(in, "local/").(*s3.GetObjectInput)
	if *out.Key != "local/media/b.png" || *in.Key != "media/b.png" {
		t.Errorf("应改拷贝不改原入参：拷贝 %s 原入参 %s", *out.Key, *in.Key)
	}
	list := withPrefix(&s3.ListObjectsV2Input{Bucket: aws.String("b")}, "local/").(*s3.ListObjectsV2Input)
	if list.Prefix == nil || *list.Prefix != "local/" {
		t.Errorf("列举不给前缀时应只列本环境：%v", list.Prefix)
	}
}

func TestNormalizePrefix(t *testing.T) {
	for in, want := range map[string]string{"": "", "local": "local/", "dev/": "dev/", " dev ": "dev/"} {
		got, err := NormalizePrefix(in)
		if err != nil || got != want {
			t.Errorf("%q → %q, %v（应为 %q）", in, got, err, want)
		}
	}
	for _, bad := range []string{"/abs", "a/../b"} {
		if _, err := NormalizePrefix(bad); err == nil {
			t.Errorf("%q 应报错", bad)
		}
	}
}
