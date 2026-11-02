package app

import "encoding/json"

// EventName 是前端订阅的 Wails 事件名（frontend/utils/realtime.ts）。
const EventName = "tabtv:event"

// Event 是一条实时事件：topic 形如 workspace:{id} / project:{id}，payload 是原样的事件 JSON。
type Event struct {
	Topic   string          `json:"topic"`
	Payload json.RawMessage `json:"payload"`
}
