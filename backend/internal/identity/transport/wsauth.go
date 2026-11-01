package transport

import (
	"context"
	"errors"
	"strconv"

	"github.com/li904338203/tabtv/backend/internal/identity/token"
	"github.com/li904338203/tabtv/backend/internal/transport/ws"
)

// WSAuth 实现 ws.Authenticator 与 ws.TopicAuthorizer。
type WSAuth struct {
	tokens *token.Service
	svc    interface {
		CanSubscribe(ctx context.Context, userID int64, kind string, id int64) (bool, error)
	}
}

func NewWSAuth(tokens *token.Service, svc interface {
	CanSubscribe(ctx context.Context, userID int64, kind string, id int64) (bool, error)
}) *WSAuth {
	return &WSAuth{tokens: tokens, svc: svc}
}

// AuthenticateWS 校验首帧 token。只接受用户 access token。
func (a *WSAuth) AuthenticateWS(_ context.Context, raw string) (int64, string, error) {
	claims, err := a.tokens.Parse(raw, token.KindUser, token.TypeAccess)
	if err != nil {
		return 0, "", err
	}
	uid, err := claims.SubjectID()
	if err != nil {
		return 0, "", errors.New("subject 非法")
	}
	return uid, claims.ID, nil
}

// CanSubscribe 逐条判定订阅授权（api/events/README.md 的授权表）。
func (a *WSAuth) CanSubscribe(ctx context.Context, userID int64, t ws.Topic) (bool, error) {
	// user:{id} 只能订阅自己。
	if t.Kind == "user" {
		return t.ID == userID, nil
	}
	return a.svc.CanSubscribe(ctx, userID, t.Kind, t.ID)
}

var _ = strconv.FormatInt
