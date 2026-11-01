package wiring

import (
	"context"

	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	scriptapp "github.com/li904338203/tabtv/backend/internal/script/app"
)

// scriptBoardShot 是脚本域给出的一镜（scriptapp.BoardShotView 的别名：适配器与测试替身共用一个形状）。
type scriptBoardShot = scriptapp.BoardShotView

// ScriptBoardShots 把脚本服务接成分镜成片的镜头来源。
type ScriptBoardShots struct{ Svc *scriptapp.Service }

func (a ScriptBoardShots) BoardShots(ctx context.Context, ws, scriptID, groupID int64) ([]scriptBoardShot, error) {
	return a.Svc.BoardShots(ctx, ws, scriptID, groupID)
}

// NewSmartcutBoard 装出分镜成片的装配端口。
func NewSmartcutBoard(script *scriptapp.Service, media *mediaapp.Service) SmartcutBoard {
	return SmartcutBoard{Script: ScriptBoardShots{Svc: script}, Media: media}
}
