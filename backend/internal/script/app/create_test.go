package app

import (
	"context"
	"errors"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 空节点 ID 在入口就拒绝（不碰库）：写得进去的话，同一项目里所有漏传它的脚本节点会落到同一条脚本上 ——
// 移植过来的前端照老契约发 `nodeId` 时建出来的就是这种，201 成功而毫无察觉。
func TestCreate_空节点ID在入口拒绝(t *testing.T) {
	s := New(nil, func() int64 { return 1 })
	for _, node := range []string{"", "   "} {
		_, err := s.Create(context.Background(), CreateInput{
			WorkspaceID: 1, ProjectID: 2, CanvasNodeID: node, SourceType: "script", RawInput: "三打白骨精",
		})
		var biz *httpx.BizError
		if !errors.As(err, &biz) || biz.Code != errcode.InvalidArgument {
			t.Fatalf("节点 ID %q 应当被拒（40001），得到 %v", node, err)
		}
	}
}
