package domain

import (
	"errors"
	"sort"
	"strings"
	"time"
)

// 在线状态（presence）的纯规则。
//
// ⚠️ **不做强锁**（NEW-19 的策略选择，见 CD-58）。画布编辑是长时间的：
// 锁整张画布等于退回单人编辑；锁单个节点又挡不住跨节点的连线。更要命的是
// 持锁人关掉页面或进程崩溃之后资源被锁死，只能靠超时释放 ——
// 而超时期间**谁都干不了活**，用户看到的是一个自己解不开的"正在被编辑"。
//
// 选定的策略是**乐观锁 + 在线提示**：保存仍由 version CAS 兜底（不会无声覆盖），
// presence 只负责让人在动手之前看见"还有谁在这儿"。它是**提示不是权限**，
// 出错的代价上限是"提示不准"，不会把任何人挡在门外。

// PresenceTTL 是一条在线记录的存活时间。
//
// ⚠️ 在线是**算出来的**（还在续期就是在线），不是标出来的：靠"离开时删掉"
// 的话，进程被杀、浏览器崩溃、网络直接断开这三种情况都不会发来离开事件 ——
// 成员列表里会永远挂着一个幽灵。与提示词 ready/stale、导出过期、
// 会员权益按 expire_at 判定是同一条规则。
const PresenceTTL = 45 * time.Second

// PresenceHeartbeat 是客户端应当的心跳间隔，必须**明显小于** TTL。
//
// 取 TTL 的三分之一：丢一次心跳不该让人从列表里消失（网络抖动很常见），
// 而丢两次就该消失了 —— 那时他多半真的走了。
const PresenceHeartbeat = 15 * time.Second

var (
	ErrEmptyClientID   = errors.New("缺少客户端标识")
	ErrClientIDTooLong = errors.New("客户端标识过长")
)

// ValidateClientID 校验客户端标识。
//
// ⚠️ `clientID` 是**每个标签页一个**，不是每个用户一个。用 userID 当键的话，
// 同一个人开三个标签页会互相续期 —— 关掉两个之后他仍然显示在线，
// 而且"有几个端在看"这个信息完全消失了。
//
// 长度上限不是防滥用，是防一个坏客户端把 Redis 的键空间撑爆。
func ValidateClientID(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return ErrEmptyClientID
	}
	if len(s) > 64 {
		return ErrClientIDTooLong
	}
	return nil
}

// PresenceEntry 是一条原始在线记录（一个标签页）。
type PresenceEntry struct {
	UserID   int64
	Nickname string
	ClientID string
	// SeenAt 是最后一次心跳时间。
	SeenAt time.Time
}

// OnlineUser 是合并之后的"一个人"。
type OnlineUser struct {
	UserID   int64
	Nickname string
	// Clients 是这个人开着几个端。⚠️ 要给出来：同一个人开两个标签页
	// 自己就能把自己撞成版本冲突，而他看到「有人修改了画布」时
	// 第一反应是去找同事 —— 这个数字直接回答了那个问题。
	Clients int
	// SeenAt 取该用户所有端里**最近**的一次心跳。
	SeenAt time.Time
	// IsSelf 标记这条是不是当前请求者自己。
	IsSelf bool
}

// MergeOnline 把原始记录按用户合并，并剔除已经过期与不再是成员的。
//
// ⚠️ **必须按用户合并**：不合并的话，一个人开三个标签页就在成员条上出现三次 ——
// 用户会以为团队里真的有三个人在改同一张画布。
//
// ⚠️ **members 过滤不能省**：成员被移出工作区之后，他在 Redis 里的记录还在，
// TTL 到期之前仍会显示在线 —— 而他其实已经进不来了（每个接口都会 403）。
// 显示一个进不来的人在线，比不显示更糟：别人会以为他正在改。
// 传 nil 表示不过滤（调用方已经保证了成员资格）。
func MergeOnline(entries []PresenceEntry, members map[int64]bool,
	selfUserID int64, now time.Time) []OnlineUser {
	byUser := map[int64]*OnlineUser{}
	for _, e := range entries {
		// 过期的丢掉。Redis 的 TTL 是按键算的，而我们把一个资源的所有端
		// 放在同一个 hash 里（这样列表只要一次查询），所以逐条自己判过期。
		if now.Sub(e.SeenAt) > PresenceTTL {
			continue
		}
		if members != nil && !members[e.UserID] {
			continue
		}
		u, ok := byUser[e.UserID]
		if !ok {
			u = &OnlineUser{UserID: e.UserID, Nickname: e.Nickname,
				IsSelf: e.UserID == selfUserID}
			byUser[e.UserID] = u
		}
		u.Clients++
		if e.SeenAt.After(u.SeenAt) {
			u.SeenAt = e.SeenAt
		}
		// 昵称取任意一条即可，但空的要被非空的替换 ——
		// 某个端没带上昵称时，列表里不该出现一个没有名字的人。
		if u.Nickname == "" {
			u.Nickname = e.Nickname
		}
	}
	out := make([]OnlineUser, 0, len(byUser))
	for _, u := range byUser {
		out = append(out, *u)
	}
	// ⚠️ 排序要稳定且**自己排在最前**：列表顺序每次刷新都变的话，
	// 用户没法扫一眼就知道"人还是那几个"。同 seenAt 时按 userID 兜底，
	// 否则 map 遍历顺序会让顺序时有时无地跳动。
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsSelf != out[j].IsSelf {
			return out[i].IsSelf
		}
		if !out[i].SeenAt.Equal(out[j].SeenAt) {
			return out[i].SeenAt.After(out[j].SeenAt)
		}
		return out[i].UserID < out[j].UserID
	})
	return out
}
