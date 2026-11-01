package ws

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"

	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
)

// Authenticator 校验首帧 token，返回用户 ID。
type Authenticator interface {
	AuthenticateWS(ctx context.Context, rawToken string) (userID int64, sessionID string, err error)
}

// TopicAuthorizer 逐条判定订阅授权。
type TopicAuthorizer interface {
	CanSubscribe(ctx context.Context, userID int64, t Topic) (bool, error)
}

type Handler struct {
	hub    *Hub
	auth   Authenticator
	topics TopicAuthorizer
	logger *slog.Logger
	// AllowedOrigins 为空表示只接受同源。
	AllowedOrigins []string
}

func NewHandler(hub *Hub, auth Authenticator, topics TopicAuthorizer, logger *slog.Logger) *Handler {
	return &Handler{hub: hub, auth: auth, topics: topics, logger: logger}
}

const (
	authTimeout  = 10 * time.Second
	pingInterval = 30 * time.Second
	writeTimeout = 5 * time.Second
	maxFrameSize = 32 * 1024
)

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: h.AllowedOrigins,
	})
	if err != nil {
		return // Accept 已写过响应
	}
	defer c.CloseNow()
	c.SetReadLimit(maxFrameSize)

	ctx := r.Context()

	// ⚠️ 必须在限定时间内完成认证，否则未认证连接会一直占着资源。
	authCtx, cancel := context.WithTimeout(ctx, authTimeout)
	userID, sessionID, err := h.authenticate(authCtx, c)
	cancel()
	if err != nil {
		return
	}

	conn := h.hub.Add(userID)
	defer h.hub.Remove(conn)

	_ = writeJSON(ctx, c, authOK{Type: "auth.ok",
		UserID: strconv.FormatInt(userID, 10), SessionID: sessionID})

	go h.writeLoop(ctx, c, conn)
	h.readLoop(ctx, c, conn)
}

func (h *Handler) authenticate(ctx context.Context, c *websocket.Conn) (int64, string, error) {
	var f inFrame
	if err := readJSON(ctx, c, &f); err != nil {
		return 0, "", err
	}
	if f.Type != "auth" || f.Token == "" {
		_ = writeJSON(ctx, c, authError{Type: "auth.error",
			Code: int(errcode.Unauthenticated), Message: "首帧必须是 auth 且带 token"})
		return 0, "", errors.New("首帧非法")
	}
	userID, sessionID, err := h.auth.AuthenticateWS(ctx, f.Token)
	if err != nil {
		_ = writeJSON(ctx, c, authError{Type: "auth.error",
			Code: int(errcode.Unauthenticated), Message: "凭证无效"})
		return 0, "", err
	}
	return userID, sessionID, nil
}

func (h *Handler) readLoop(ctx context.Context, c *websocket.Conn, conn *Conn) {
	for {
		var f inFrame
		if err := readJSON(ctx, c, &f); err != nil {
			return
		}
		switch f.Type {
		case "subscribe":
			h.handleSubscribe(ctx, c, conn, f.Topics)
		case "unsubscribe":
			for _, raw := range f.Topics {
				conn.unsubscribe(raw)
			}
		case "ping":
			_ = writeJSON(ctx, c, map[string]string{"type": "pong"})
		}
	}
}

// handleSubscribe 逐条鉴权。
// ⚠️ 不通过的放进 rejected 而**不是**整次失败：
// 一个失效的 topic 不该让用户其余正常订阅也丢掉（CD-09）。
func (h *Handler) handleSubscribe(ctx context.Context, c *websocket.Conn, conn *Conn, raws []string) {
	accepted := make([]string, 0, len(raws))
	denied := make([]rejected, 0)

	for _, raw := range raws {
		t, err := ParseTopic(raw)
		if err != nil {
			denied = append(denied, rejected{Topic: raw, Code: int(errcode.InvalidArgument)})
			continue
		}
		ok, err := h.topics.CanSubscribe(ctx, conn.UserID(), t)
		if err != nil {
			denied = append(denied, rejected{Topic: raw, Code: int(errcode.Internal)})
			continue
		}
		if !ok {
			denied = append(denied, rejected{Topic: raw, Code: int(errcode.PermissionDenied)})
			continue
		}
		conn.subscribe(raw)
		accepted = append(accepted, raw)
	}

	_ = writeJSON(ctx, c, subscribeOK{Type: "subscribe.ok", Topics: accepted, Rejected: denied})
}

func (h *Handler) writeLoop(ctx context.Context, c *websocket.Conn, conn *Conn) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-conn.Done():
			// hub 已移除本连接（撤销或服务端关闭）。
			// 关闭前尽力把缓冲里剩下的消息发完 —— 撤销通知就在里面，
			// 客户端收不到它就只会当成一次普通断线并立刻重连。
			drainAndClose(ctx, c, conn)
			return
		case msg := <-conn.Recv():
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.Write(wctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				return
			}
		case <-ticker.C:
			pctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

// drainAndClose 尽力发完缓冲中的消息后关闭连接。
func drainAndClose(ctx context.Context, c *websocket.Conn, conn *Conn) {
	for {
		select {
		case msg := <-conn.Recv():
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.Write(wctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				return
			}
		default:
			c.Close(websocket.StatusNormalClosure, "session revoked")
			return
		}
	}
}

func readJSON(ctx context.Context, c *websocket.Conn, dst any) error {
	_, data, err := c.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, dst)
}

func writeJSON(ctx context.Context, c *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return c.Write(wctx, websocket.MessageText, data)
}
