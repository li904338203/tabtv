//go:build integration

package testsupport

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/li904338203/tabtv/backend/internal/platform/config"
	"github.com/li904338203/tabtv/backend/internal/platform/objstore"
)

// FakeS3 是只够 PutObject / GetObject / DeleteObject 用的内存对象存储（path-style：/bucket/key）。
//
// ⚠️ 不起 MinIO 容器：本机 colima 只有 2GB，集成测试再起一个 MinIO 会把正在跑的开发环境挤到 OOM。
// 营销与 MCP 的集成测试各有一份同样的实现（早于这里），新测试用这一份。
type FakeS3 struct {
	mu   sync.Mutex
	objs map[string][]byte
}

// FakeObjectStore 起一个假的对象存储并返回接在它上面的客户端（桶名 seeaitv）。测试结束自动关闭。
func FakeObjectStore(t *testing.T) (*objstore.Client, *FakeS3) {
	t.Helper()
	f := &FakeS3{objs: map[string][]byte{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	store, err := objstore.New(context.Background(), config.Storage{Bucket: "seeaitv", Region: "us-east-1", PathStyle: true,
		Endpoint: srv.URL, AccessKey: "ak", SecretKey: "sk", SignTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return store, f
}

// Object 取一个对象的字节（key 不带桶名）。
func (f *FakeS3) Object(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objs["/seeaitv/"+key]
	return b, ok
}

// Keys 列出全部对象（带桶名前缀的路径，排序后返回）。
func (f *FakeS3) Keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.objs))
	for k := range f.objs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (f *FakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
