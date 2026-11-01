package transport

import (
	"testing"

	"github.com/li904338203/tabtv/backend/internal/media/domain"
)

// 参考上传只有 file 必填（MKT-T-01）：mediaType 缺省 = 不限定大类，
// 由实际文件头决定；给了就按它收窄，非法取值才拒。
func TestReferenceExpectKind_mediaType可选(t *testing.T) {
	cases := []struct {
		in   string
		want domain.Kind
		ok   bool
	}{
		{"", "", true},
		{"image", domain.KindImage, true},
		{"audio", domain.KindAudio, true},
		{"video", domain.KindVideo, true},
		{"IMAGE", "", false},
		{"file", "", false},
	}
	for _, c := range cases {
		got, ok := referenceExpectKind(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("referenceExpectKind(%q) = (%q,%v)，期望 (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
