package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/li904338203/tabtv/backend/internal/marketing/domain"
	"github.com/li904338203/tabtv/backend/internal/marketing/repo"
)

// 建营销会话时的商品 / 角色快照（MKT-T-37；dev/05 MKD-25）。
//
// agent 经它自己定义的端口 SceneLibrary 调这里（wiring 适配）：会话不引用商品库 / 角色库的行，
// 建会话那一刻把内容冻结进 agent_session.scene_input，之后编辑 / 删除商品与角色都不改变已建会话。
// ⚠️ 工作区条件在仓储的每条 SQL 里（GetProduct / GetCharacter 只认本工作区未删除的行）：
// 他人工作区、已删除、不存在一律同一个 SnapshotMissing，不提供按 ID 探测的口子。

// SnapshotMissing 是快照里取不到的那一项（Kind = product / character）。
type SnapshotMissing struct {
	Kind string
	ID   int64
}

func (e *SnapshotMissing) Error() string {
	return fmt.Sprintf("营销%s %d 不存在或已删除", map[string]string{"product": "商品", "character": "角色"}[e.Kind], e.ID)
}

// SessionSnapshot 是一次建会话用到的商品（可为 nil = 没选商品）与角色（按请求顺序）。
type SessionSnapshot struct {
	Product    *repo.Product
	Characters []repo.Character
}

// SnapshotForSession 按工作区取商品与角色的当前内容（productID ≤ 0 表示不取商品）。
//
// 角色没有图片（库里约束是 ≥ 1 张，这里是兜底）按取不到处理：快照要拿主图当 `kind=media` 的输入，
// 空主图会让生成那一步拿到一个 ID 为 0 的参考图。
func (l *Library) SnapshotForSession(ctx context.Context, ws, productID int64, characterIDs []int64) (
	SessionSnapshot, error) {
	var out SessionSnapshot
	if productID > 0 {
		p, err := l.products.GetProduct(ctx, ws, productID)
		if errors.Is(err, repo.ErrNotFound) || (err == nil && len(p.ImageMediaIDs) == 0) {
			return SessionSnapshot{}, &SnapshotMissing{Kind: "product", ID: productID}
		}
		if err != nil {
			return SessionSnapshot{}, err
		}
		out.Product = &p
	}
	if len(characterIDs) > 0 && l.characters == nil {
		return SessionSnapshot{}, errors.New("服务端未接入角色库")
	}
	for _, id := range characterIDs {
		c, err := l.characters.GetCharacter(ctx, ws, id)
		if errors.Is(err, repo.ErrNotFound) || (err == nil && domain.MainImage(c.ImageMediaIDs) == 0) {
			return SessionSnapshot{}, &SnapshotMissing{Kind: "character", ID: id}
		}
		if err != nil {
			return SessionSnapshot{}, err
		}
		out.Characters = append(out.Characters, c)
	}
	return out, nil
}
