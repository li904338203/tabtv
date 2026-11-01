package app

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// PresenceStore 由 platform/cache 实现。
//
// ⚠️ presence **只存 Redis，不进 PG**。它是"此刻谁在看"，生命周期以秒计、
// 每人每 15 秒写一次 —— 落库就是一张高频写入且没有任何历史价值的表。
// 更要命的是：进程崩溃时库里那行不会消失，成员条上会永远挂着一个幽灵，
// 而唯一的清理办法又是再写一个作业。Redis 的 TTL 天生就是这件事的答案。
//
// ⚠️ Redis 挂掉只能让**提示消失**，绝不能让画布打不开（GO-11：Redis 只做加速）。
// 所以本服务的每个方法在出错时都退化成"没有人在线"而不是报错。
type PresenceStore interface {
	// Touch 写入/续期一条记录，并把整个资源的键过期时间顺延。
	Touch(ctx context.Context, key, field, value string, ttl time.Duration) error
	// Entries 取一个资源下的全部记录。
	Entries(ctx context.Context, key string) (map[string]string, error)
	// Drop 删掉一条记录（用户主动离开时）。
	Drop(ctx context.Context, key, field string) error
}

// SetPresence 注入存储。为 nil 时 presence 整体降级为"空列表"。
func (s *Service) SetPresence(p PresenceStore) { s.presence = p }

// presenceKey 是一个资源的在线记录集合。
//
// ⚠️ 键里必须带 workspaceID：resourceID 是雪花号不会撞，但**带上它才能
// 保证跨工作区一定看不到对方的 presence** —— 不带的话，一旦将来有哪个
// 资源类型改用自增 ID，两个工作区的同号资源会共用一份在线列表。
func presenceKey(workspaceID int64, resourceType string, resourceID int64) string {
	return "presence:" + itoa(workspaceID) + ":" + resourceType + ":" + itoa(resourceID)
}

// PresenceInput 是一次心跳。
type PresenceInput struct {
	WorkspaceID  int64
	ResourceType string
	ResourceID   int64
	UserID       int64
	Nickname     string
	ClientID     string
}

// Heartbeat 续期一条在线记录并回带当前在线列表。
//
// ⚠️ 心跳与列表**合成一个接口**：客户端本来就要每 15 秒发一次心跳，
// 再单独轮询一次列表等于把请求翻倍，而两者的数据完全一样。
// 分开的话还会出现"心跳成功但列表是上一轮的"这种自相矛盾的界面。
func (s *Service) Heartbeat(ctx context.Context, in PresenceInput) ([]domain.OnlineUser, error) {
	if err := domain.ValidateClientID(in.ClientID); err != nil {
		return nil, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	if in.ResourceType == "" || in.ResourceID <= 0 {
		return nil, httpx.Errf(errcode.InvalidArgument, "资源标识非法")
	}
	if s.presence == nil {
		return []domain.OnlineUser{}, nil
	}
	// ⚠️ 昵称由服务端查，**不采信请求体**（见 transport 的说明）。
	// 一次主键查询换掉一个零成本的冒充面，值得。
	nickname, _, err := s.repo.UserName(ctx, in.UserID)
	if err != nil {
		// 查不到名字不该让心跳失败：没有名字的在线提示仍然比没有提示好。
		s.log.Warn("presence 取昵称失败", "error", err, "userId", in.UserID)
	}
	in.Nickname = nickname

	key := presenceKey(in.WorkspaceID, in.ResourceType, in.ResourceID)
	now := time.Now()
	// 值的格式是 `userID|seenAtUnixMilli|nickname`。
	// ⚠️ 昵称放最后且**不做转义**：它可能含任何字符（包括竖线），
	// 放最后的话按前两个分隔符切就永远不会切坏。
	val := itoa(in.UserID) + "|" + itoa(now.UnixMilli()) + "|" + in.Nickname
	if err := s.presence.Touch(ctx, key, in.ClientID, val, domain.PresenceTTL); err != nil {
		// Redis 不可用时退化成"只有我自己"，不让画布因为一个提示功能打不开。
		s.log.Warn("presence 写入失败，已降级", "error", err, "resource", in.ResourceType)
		return []domain.OnlineUser{{UserID: in.UserID, Nickname: in.Nickname,
			Clients: 1, SeenAt: now, IsSelf: true}}, nil
	}
	return s.online(ctx, key, in.WorkspaceID, in.UserID, now), nil
}

// Online 只读当前在线列表（不续期）。
//
// 单独留一个只读入口是给"我只是想看看"的场景（例如项目列表上的小圆点）——
// 用心跳去查的话，一个从没打开过画布的人会把自己写进在线列表里。
func (s *Service) Online(ctx context.Context, workspaceID int64, resourceType string,
	resourceID, selfUserID int64) ([]domain.OnlineUser, error) {
	if s.presence == nil {
		return []domain.OnlineUser{}, nil
	}
	key := presenceKey(workspaceID, resourceType, resourceID)
	return s.online(ctx, key, workspaceID, selfUserID, time.Now()), nil
}

// Leave 主动离开。
//
// ⚠️ 它是**优化不是正确性**：不调也会在 TTL 之后自动消失（关标签页、
// 断电、进程被杀都不会调到这里）。调用失败一律忽略 ——
// 为了一个"提前几十秒从列表里消失"的优化去打断用户离开页面不值得。
func (s *Service) Leave(ctx context.Context, in PresenceInput) {
	if s.presence == nil || in.ClientID == "" {
		return
	}
	key := presenceKey(in.WorkspaceID, in.ResourceType, in.ResourceID)
	if err := s.presence.Drop(ctx, key, in.ClientID); err != nil {
		s.log.Warn("presence 清理失败（不影响正确性，会自然过期）", "error", err)
	}
}

func (s *Service) online(ctx context.Context, key string, workspaceID, selfUserID int64,
	now time.Time) []domain.OnlineUser {
	raw, err := s.presence.Entries(ctx, key)
	if err != nil {
		s.log.Warn("presence 读取失败，已降级", "error", err)
		return []domain.OnlineUser{}
	}
	entries := make([]domain.PresenceEntry, 0, len(raw))
	for clientID, v := range raw {
		e, ok := parsePresenceValue(v)
		if !ok {
			// 坏值跳过而不是整张列表报错：一条记录格式不对
			// 不该让成员条打不开（同共享权限里的脏值处理）。
			continue
		}
		e.ClientID = clientID
		entries = append(entries, e)
	}

	// 成员过滤：被移出工作区的人 TTL 到期前仍在 Redis 里，
	// 而他已经进不来了 —— 显示他在线会让别人以为他正在改。
	var members map[int64]bool
	if rows, err := s.repo.ListMembers(ctx, workspaceID); err == nil {
		members = make(map[int64]bool, len(rows))
		for _, m := range rows {
			members[m.UserID] = true
		}
	}
	return domain.MergeOnline(entries, members, selfUserID, now)
}

func parsePresenceValue(v string) (domain.PresenceEntry, bool) {
	// 只按前两个分隔符切：昵称在最后，含竖线也切不坏。
	first := strings.IndexByte(v, '|')
	if first < 0 {
		return domain.PresenceEntry{}, false
	}
	rest := v[first+1:]
	second := strings.IndexByte(rest, '|')
	if second < 0 {
		return domain.PresenceEntry{}, false
	}
	uid, err1 := strconv.ParseInt(v[:first], 10, 64)
	ms, err2 := strconv.ParseInt(rest[:second], 10, 64)
	if err1 != nil || err2 != nil || uid <= 0 {
		return domain.PresenceEntry{}, false
	}
	return domain.PresenceEntry{
		UserID: uid, Nickname: rest[second+1:],
		SeenAt: time.UnixMilli(ms),
	}, true
}
