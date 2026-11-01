package canvasgen

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type outputsFake map[string]string

func (o outputsFake) OperationOutNodeType(_ context.Context, op string) (string, error) {
	if op == "boom" {
		return "", errors.New("db down")
	}
	return o[op], nil
}

func TestResolveNewNodeType(t *testing.T) {
	ctx := context.Background()
	src := outputsFake{"text2video": "video", "text2audio": "audio", "storyboard_gen": "script"}
	cases := []struct {
		name, op, declared string
		src                OperationOutputs
		want, errHas       string
	}{
		{"缺 nodeType 按产出类型推导", "text2video", "", src, "video", ""},
		{"显式一致", "text2audio", "audio", src, "audio", ""},
		{"显式不一致", "text2video", "image", src, "", "与操作 text2video 的产出类型 \"video\" 不一致"},
		{"产出不是可新建的节点", "storyboard_gen", "", src, "", "产出类型是 \"script\"，不能直接建节点"},
		{"操作不在目录且没给类型", "nope", "", src, "", "不在目录里"},
		{"操作不在目录但给了合法类型", "nope", "image", src, "image", ""},
		{"没接目录时退回显式规则", "text2video", "", nil, "", "nodeType 必须是"},
		{"没接目录、显式合法", "text2video", "video", nil, "video", ""},
	}
	for _, c := range cases {
		got, err := ResolveNewNodeType(ctx, c.src, c.op, c.declared)
		if c.errHas != "" {
			var nte *NodeTypeError
			if err == nil || !errors.As(err, &nte) || !strings.Contains(err.Error(), c.errHas) {
				t.Fatalf("%s：应报参数错误含 %q，得到 %v", c.name, c.errHas, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Fatalf("%s：得到 %q %v，期望 %q", c.name, got, err, c.want)
		}
	}
	// 查目录失败是内部错误，不是参数错误。
	_, err := ResolveNewNodeType(ctx, src, "boom", "")
	var nte *NodeTypeError
	if err == nil || errors.As(err, &nte) {
		t.Fatalf("查询失败不该归为参数错误：%v", err)
	}
}
