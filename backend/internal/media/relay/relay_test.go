package relay

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/media/repo"
)

type fakeSource struct {
	m      repo.MediaObject
	data   []byte
	opened int
	err    error
}

func (f *fakeSource) Get(context.Context, int64) (repo.MediaObject, error) { return f.m, f.err }

func (f *fakeSource) Open(context.Context, int64) (io.ReadCloser, repo.MediaObject, error) {
	f.opened++
	return io.NopCloser(bytes.NewReader(f.data)), f.m, nil
}

type fakeStore struct {
	objects   map[string][]byte
	types     map[string]string
	puts      int
	existsErr error
	putErr    error
	tmpPath   string
}

func (s *fakeStore) Exists(_ context.Context, key string) (bool, error) {
	if s.existsErr != nil {
		return false, s.existsErr
	}
	_, ok := s.objects[key]
	return ok, nil
}

func (s *fakeStore) PutFile(_ context.Context, key string, body io.ReadSeeker, size int64, ct string) error {
	s.puts++
	if f, ok := body.(*os.File); ok {
		s.tmpPath = f.Name()
	}
	if s.putErr != nil {
		return s.putErr
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if int64(len(b)) != size {
		return errors.New("size 与 body 不一致")
	}
	s.objects[key], s.types[key] = b, ct
	return nil
}

func (s *fakeStore) PresignGet(_ context.Context, key string) (string, error) {
	return "https://relay.example.com/b/" + key + "?sig=x", nil
}

func newFakes() (*fakeSource, *fakeStore) {
	return &fakeSource{
			m:    repo.MediaObject{ID: 42, ObjectKey: "upload_reference/2026/10/24/42.png", ContentType: "image/png"},
			data: []byte("\x89PNG-商品图"),
		},
		&fakeStore{objects: map[string][]byte{}, types: map[string]string{}}
}

func TestURL_不在中转存储时先转存再签名_同一对象只传一次(t *testing.T) {
	src, dst := newFakes()
	r := New(src, dst, "relay/", nil)

	u, mime, err := r.URL(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	const key = "relay/upload_reference/2026/10/24/42.png"
	if u != "https://relay.example.com/b/"+key+"?sig=x" || mime != "image/png" {
		t.Fatalf("应返回中转存储的签名地址：%s %s", u, mime)
	}
	if !bytes.Equal(dst.objects[key], src.data) || dst.types[key] != "image/png" {
		t.Fatalf("字节与类型应原样转存：%q %q", dst.objects[key], dst.types[key])
	}
	// 临时文件用完即删（每次留一个的话，参考视频几天就能把盘写满）。
	if _, err := os.Stat(dst.tmpPath); !os.IsNotExist(err) || filepath.Base(dst.tmpPath) == "." {
		t.Fatalf("临时文件应已删除：%s %v", dst.tmpPath, err)
	}

	// 第二次：那边已经有了，不再读主存储、不再上传。
	if _, _, err := r.URL(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	if src.opened != 1 || dst.puts != 1 {
		t.Fatalf("同一对象只传一次：读主存储 %d 次、上传 %d 次", src.opened, dst.puts)
	}
}

func TestURL_出错时说清是哪一步(t *testing.T) {
	cases := []struct {
		name string
		set  func(*fakeSource, *fakeStore)
		want string
	}{
		{"取媒体", func(s *fakeSource, _ *fakeStore) { s.err = errors.New("没有这条媒体") }, "取媒体 42 失败"},
		// 查询失败不当成「不存在」去重传：权限 / 网络问题要原样报出来。
		{"查询中转存储", func(_ *fakeSource, d *fakeStore) { d.existsErr = errors.New("403") }, "查询中转存储失败"},
		{"上传", func(_ *fakeSource, d *fakeStore) { d.putErr = errors.New("连接中断") }, "转存到中转存储失败：连接中断"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src, dst := newFakes()
			c.set(src, dst)
			_, _, err := New(src, dst, "relay/", nil).URL(context.Background(), 42)
			if err == nil || !strings.Contains(strings.ReplaceAll(err.Error(), ": ", "："), c.want) {
				t.Fatalf("应报 %q：%v", c.want, err)
			}
			if c.name == "查询中转存储" && dst.puts != 0 {
				t.Fatal("查询失败时不该去上传")
			}
		})
	}
}
