package protocol

// MediaURLRequiring（可选）：协议的上游**只收地址形态**的参考媒体，不收内联 base64（data URI）。
//
// 参考媒体默认「签名外链优先，上游够不着时内联」（provider/app.mediaValue）。声明了的协议不再回落内联：
// 签不出上游够得着的地址时（本地 MinIO 签出的是 127.0.0.1），在提交之前以永久失败报出真实原因 ——
// 不扣上游的钱，也不让用户去看一句与原因无关的上游报错。
//
// ⚠️ 只给**有实测证据**的上游声明：多数上游收内联（grok 中转实测收过 3.16MB 的内联 PNG），
// 对它们来说内联是本地开发唯一跑得通的路，一刀切会把本地能跑的都挡掉。
type MediaURLRequiring interface {
	RequiresMediaURL() bool
}

// RequiresMediaURL 报告协议是否只收地址形态的参考媒体；没声明按「收内联」处理。
func RequiresMediaURL(p Protocol) bool {
	r, ok := p.(MediaURLRequiring)
	return ok && r.RequiresMediaURL()
}
