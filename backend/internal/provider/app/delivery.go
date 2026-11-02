package app

import (
	"context"

	"github.com/li904338203/tabtv/backend/internal/provider/protocol"
)

// Presigner 签发对象的下载地址（objstore.Client 实现）。签名是本地计算，不发网络请求。
type Presigner interface {
	PresignGet(ctx context.Context, key string) (string, error)
}

// deliveryProbeKey 判「签出的地址够不够得着」用的键：判的是存储端点，与具体对象无关。
const deliveryProbeKey = "probe/media-delivery"

// MediaDelivery 实现 generation/app.MediaURLDelivery：只收地址的上游收不收得到这边的参考媒体
// （docs/agent/smartcut SCT-38）。
//
// 规则与 Worker 发参考媒体时（mediaValue / rangeValue）是同一条：主存储签出的地址上游够得着就发它；否则配了公网中转
// 就转存过去发那边的；两样都没有时 Worker 只能在提交前判永久失败、退款。受理在扣费之前问这一句，用户就不用
// 白等一轮，账本里也不会多出一对扣款与退款。判「够不够得着」用同一条固定的公网规则（upstreamReachable）。
//
// ⚠️ API 与 Worker 的存储配置要一致（Makefile 与部署都从同一份环境变量来）：这里按本进程的配置判，Worker 按它自己的。
type MediaDelivery struct {
	// Main 是主存储（nil = 没有对象存储，签不出地址）；Relay 是公网中转（nil = 没配）。
	Main, Relay Presigner
}

// Undeliverable 收不到时给原因（给用户看，不含配置细节）；协议不要求地址、或者收得到时为空串。
func (d MediaDelivery) Undeliverable(ctx context.Context, apiProtocol string) string {
	p, ok := protocol.Get(apiProtocol)
	if !ok || !protocol.RequiresMediaURL(p) {
		return ""
	}
	for _, s := range []Presigner{d.Main, d.Relay} {
		if s == nil {
			continue
		}
		if u, err := s.PresignGet(ctx, deliveryProbeKey); err == nil && upstreamReachable(u) {
			return ""
		}
	}
	return "这个模型只收公网地址的素材，而当前存储签出的地址它访问不到，也没有配置公网中转存储：请在「设置 → 存储」里配置一个 S3 兼容存储"
}
