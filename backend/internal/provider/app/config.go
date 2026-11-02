// Package app 把传输层、协议适配与模型配置接成执行器要的 Provider。
//
// 依赖方向是 `adapter → app interfaces`（08 §3）：端口 `generation/app.Provider`
// 由生成域定义（谁定义端口谁拥有契约），这里实现它。
//
// ⚠️ 这一层的职责边界很窄：**解析配置 → 交给协议构造请求 → 发出去 → 把响应交回协议 → 落地产物**。
// 业务判断（该不该发、扣多少钱、失败怎么退）全在生成域，
// 协议细节（请求体长什么样、产物在哪个字段）全在 protocol 包。
// 三者混在一起就是老系统那个「改一个模型要动核心链路」的状态。
package app

import (
	"context"

	"github.com/li904338203/tabtv/backend/internal/provider/runcfg"
)

// RunConfig / Resolver / LogEntry / RequestLogger 的定义在 `provider/runcfg` ——
// 目录域与日志仓储可以直接实现它们，两个 main 里就不用各写一份转换适配器了
// （那两份早晚会不一致，且不一致时没有编译错误）。
type (
	RunConfig     = runcfg.RunConfig
	Resolver      = runcfg.Resolver
	LogEntry      = runcfg.LogEntry
	RequestLogger = runcfg.RequestLogger
)

// MediaResolver 由媒体域实现：把可信媒体 ID 变成上游能用的形态。
//
// ⚠️ 两个方法都在，是因为**上游能不能用 URL 取决于我们的部署形态**：
// 对象存储签出来的地址如果指向内网（本地 MinIO），上游一个字节也拉不到。
// 见 mediaValue 的回落规则。
type MediaResolver interface {
	// SignedURL 签发有时效的下载地址。
	SignedURL(ctx context.Context, mediaID int64) (url string, mime string, err error)
	// Inline 读出字节，用于签不出外链或外链上游够不着时回落。
	Inline(ctx context.Context, mediaID int64) (data []byte, mime string, err error)
}

// MediaRelay（可选）：把参考媒体转存到上游够得着的「公网中转」存储，返回那边的签名地址
// （marketing dev/01 R-15；实现在 media/relay）。
//
// 只在**协议只收地址**（protocol.MediaURLRequiring）而主存储签出的地址上游够不着时用：
// 主存储因此可以留在本地（前端快），只有真要发给这类上游的那几个文件走一趟中转。
// 收内联的协议照旧内联 —— 本地开发时那条路更快，也不占中转服务器的带宽。
type MediaRelay interface {
	URL(ctx context.Context, mediaID int64) (url string, mime string, err error)
}

// MediaDurationResolver 是 MediaResolver 的可选扩展：取音视频时长（毫秒，0 = 未知）。
//
// 单独成一个接口而不是加进 MediaResolver：只有要用到参考视频时长的协议（可灵视频配音效）用得上，
// 加进主接口会让每个测试替身都得补一个与它无关的方法。
type MediaDurationResolver interface {
	DurationMs(ctx context.Context, mediaID int64) (int, error)
}
