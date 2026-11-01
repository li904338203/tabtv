package objstore

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/aws/smithy-go/middleware"
)

// NormalizePrefix 校验并规范公共前缀：空串原样返回；否则不许以 / 开头、不许含 ..，并保证以 / 结尾。
func NormalizePrefix(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", nil
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, "..") {
		return "", fmt.Errorf("SEEAITV_STORAGE_PREFIX 取值非法: %q（不能以 / 开头、不能含 ..）", p)
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p, nil
}

// prefixMiddleware 在每个请求序列化之前给入参里的 Key / Prefix 加上公共前缀。
//
// ⚠️ 放在客户端上而不是逐个调用点加：媒体域有好几处直接拿 S3() 调 PutObject / GetObject / DeleteObject，
// 预签名地址也走这个客户端 —— 漏加一处不会有编译错误，而那一处的文件会落到别的环境的地盘上，
// 或者被别的环境的清理作业删掉。放在中间件里，新的调用点自动生效。
//
// 按字段名反射处理（所有对象级接口的入参都叫 Key，列举接口叫 Prefix），新接口不用改这里。
func prefixMiddleware(prefix string) func(*middleware.Stack) error {
	return func(stack *middleware.Stack) error {
		return stack.Initialize.Add(middleware.InitializeMiddlewareFunc("seeaitvKeyPrefix",
			func(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (
				middleware.InitializeOutput, middleware.Metadata, error) {
				// 改的是入参的一份浅拷贝：直接改调用方的结构体的话，它事后记日志 / 复用这份入参时
				// 看到的是被改过的键。Initialize 每次调用只走一遍（重试发生在更后面的步骤），不会加两次。
				in.Parameters = withPrefix(in.Parameters, prefix)
				return next.HandleInitialize(ctx, in)
			}), middleware.Before)
	}
}

func withPrefix(params any, prefix string) any {
	v := reflect.ValueOf(params)
	if v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return params
	}
	cp := reflect.New(v.Elem().Type())
	cp.Elem().Set(v.Elem())
	e := cp.Elem()
	strPtr := reflect.TypeOf((*string)(nil))
	for _, name := range []string{"Key", "Prefix"} {
		f := e.FieldByName(name)
		if !f.IsValid() || !f.CanSet() || f.Type() != strPtr {
			continue
		}
		s := prefix
		if !f.IsNil() {
			s += f.Elem().String()
		} else if name == "Key" {
			continue
		}
		// Prefix 为空（列举时没给）= 列本环境的全部，而不是整个桶。
		f.Set(reflect.ValueOf(&s))
	}
	return cp.Interface()
}
