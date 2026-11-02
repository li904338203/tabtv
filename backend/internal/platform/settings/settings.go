// Package settings 是本机设置（TT-T-12，dev/01 §11）：一个键一行存在 app_setting 表里。
//
// ⚠️ 键是白名单（Definitions），不是任意 KV：未知键一律拒绝 —— 否则前端拼错一个键名，
//
//	保存「成功」而什么都没生效，与来源系统里 json.Decoder 静默忽略未知字段是同一种错误。
//
// ⚠️ 每个键带「立即生效 / 重启后生效」：出网策略、可选 S3 是在装配时建客户端的，改了要重启，
//
//	设置页据此提示，而不是让用户以为已经生效。
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Kind 是值的类型。
type Kind string

const (
	KindBool   Kind = "bool"
	KindInt    Kind = "int"
	KindString Kind = "string"
	KindSecret Kind = "secret" // 读取时只给掩码（不回显完整值，TT-F-28）
)

// Definition 描述一个设置键。
type Definition struct {
	Key     string
	Kind    Kind
	Default any
	// Min / Max 只对 KindInt 生效（含边界）。
	Min, Max int
	// Restart 为 true 表示改了要重启才生效。
	Restart bool
}

// 设置键（dev/01 §11）。
const (
	GenerationMaxRunning = "generation.max_running"
	RenderMaxConcurrent  = "render.max_concurrent"
	ProviderAllowPrivate = "provider.allow_private_network"
	RequestLogRetention  = "request_log.retention_days"
	UpdateAutoCheck      = "update.auto_check"
	FFmpegPath           = "ffmpeg.path"
	FFprobePath          = "ffprobe.path"
	S3Endpoint           = "storage.s3.endpoint"
	S3Region             = "storage.s3.region"
	S3Bucket             = "storage.s3.bucket"
	S3AccessKey          = "storage.s3.access_key"
	S3SecretKey          = "storage.s3.secret_key"
	S3PathStyle          = "storage.s3.path_style"
	S3Prefix             = "storage.s3.prefix"
	S3SignTTLHours       = "storage.s3.sign_ttl_hours"
	// AppCloseMinimize 有任务在跑时关窗口不再询问、总是最小化（TT-T-30）。
	AppCloseMinimize = "app.close_always_minimize"
)

// Definitions 是全部合法的键。
var Definitions = []Definition{
	{Key: GenerationMaxRunning, Kind: KindInt, Default: 4, Min: 1, Max: 32},
	{Key: RenderMaxConcurrent, Kind: KindInt, Default: 1, Min: 1, Max: 4},
	{Key: ProviderAllowPrivate, Kind: KindBool, Default: false, Restart: true},
	{Key: RequestLogRetention, Kind: KindInt, Default: 30, Min: 1, Max: 365},
	{Key: UpdateAutoCheck, Kind: KindBool, Default: true},
	{Key: FFmpegPath, Kind: KindString, Default: "", Restart: true},
	{Key: FFprobePath, Kind: KindString, Default: "", Restart: true},
	{Key: S3Endpoint, Kind: KindString, Default: "", Restart: true},
	{Key: S3Region, Kind: KindString, Default: "", Restart: true},
	{Key: S3Bucket, Kind: KindString, Default: "", Restart: true},
	{Key: S3AccessKey, Kind: KindString, Default: "", Restart: true},
	{Key: S3SecretKey, Kind: KindSecret, Default: "", Restart: true},
	{Key: S3PathStyle, Kind: KindBool, Default: true, Restart: true},
	{Key: S3Prefix, Kind: KindString, Default: "relay/", Restart: true},
	{Key: S3SignTTLHours, Kind: KindInt, Default: 6, Min: 1, Max: 168, Restart: true},
	{Key: AppCloseMinimize, Kind: KindBool, Default: false},
}

func definition(key string) (Definition, bool) {
	for _, d := range Definitions {
		if d.Key == key {
			return d, true
		}
	}
	return Definition{}, false
}

// ErrUnknownKey 表示不在白名单里的键。
var ErrUnknownKey = errors.New("未知的设置项")

// Store 读写设置，带进程内缓存与变更回调。
type Store struct {
	pool *pgxpool.Pool

	mu       sync.RWMutex
	values   map[string]any
	watchers map[string][]func(any)
}

// Open 读出全部设置（缺省的取默认值）。
func Open(ctx context.Context, pool *pgxpool.Pool) (*Store, error) {
	s := &Store{pool: pool, values: map[string]any{}, watchers: map[string][]func(any){}}
	rows, err := pool.Query(ctx, `select key, value from app_setting`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var raw []byte
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, err
		}
		d, ok := definition(key)
		if !ok {
			continue // 旧版本留下的键：忽略，不报错
		}
		if v, err := decode(d, raw); err == nil {
			s.values[key] = v
		}
	}
	return s, rows.Err()
}

func decode(d Definition, raw []byte) (any, error) {
	switch d.Kind {
	case KindBool:
		var b bool
		err := json.Unmarshal(raw, &b)
		return b, err
	case KindInt:
		var n int
		err := json.Unmarshal(raw, &n)
		return n, err
	default:
		var s string
		err := json.Unmarshal(raw, &s)
		return s, err
	}
}

// Get 取值（没设置过的取默认值）。
func (s *Store) Get(key string) any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if v, ok := s.values[key]; ok {
		return v
	}
	d, _ := definition(key)
	return d.Default
}

// Int / Bool / String 是带类型的便捷读取。
func (s *Store) Int(key string) int {
	v, _ := s.Get(key).(int)
	return v
}
func (s *Store) Bool(key string) bool {
	v, _ := s.Get(key).(bool)
	return v
}
func (s *Store) String(key string) string {
	v, _ := s.Get(key).(string)
	return v
}

// Watch 注册变更回调（立即生效的键用它把新值推给运行中的组件）。
func (s *Store) Watch(key string, fn func(any)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watchers[key] = append(s.watchers[key], fn)
}

// Set 校验并写入一组设置（一个事务）。值的类型与范围不对整批拒绝。
// 返回其中需要重启才生效的键。
func (s *Store) Set(ctx context.Context, in map[string]json.RawMessage) ([]string, error) {
	parsed := map[string]any{}
	var restart []string
	for key, raw := range in {
		d, ok := definition(key)
		if !ok {
			return nil, fmt.Errorf("%w：%s", ErrUnknownKey, key)
		}
		v, err := decode(d, raw)
		if err != nil {
			return nil, fmt.Errorf("%s 的值类型不对（应为 %s）", key, d.Kind)
		}
		if d.Kind == KindInt {
			n := v.(int)
			if n < d.Min || n > d.Max {
				return nil, fmt.Errorf("%s 应在 %d~%d 之间", key, d.Min, d.Max)
			}
		}
		parsed[key] = v
		if d.Restart {
			restart = append(restart, key)
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for key, v := range parsed {
		raw, _ := json.Marshal(v)
		if _, err := tx.Exec(ctx, `
			insert into app_setting (key, value, updated_at) values ($1, $2, now())
			on conflict (key) do update set value = excluded.value, updated_at = now()`, key, raw); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	var fire []func()
	for key, v := range parsed {
		s.values[key] = v
		for _, w := range s.watchers[key] {
			w, v := w, v
			fire = append(fire, func() { w(v) })
		}
	}
	s.mu.Unlock()
	for _, f := range fire {
		f()
	}
	sort.Strings(restart)
	return restart, nil
}

// Item 是对外的一项设置（密钥类只给掩码与「是否已设置」）。
type Item struct {
	Key     string `json:"key"`
	Kind    Kind   `json:"kind"`
	Value   any    `json:"value"`
	Default any    `json:"default"`
	Set     bool   `json:"set"`
	Restart bool   `json:"restart"`
	Min     int    `json:"min,omitempty"`
	Max     int    `json:"max,omitempty"`
}

// List 返回全部设置项（按定义顺序）。
func (s *Store) List() []Item {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Item, 0, len(Definitions))
	for _, d := range Definitions {
		v, set := s.values[d.Key]
		if !set {
			v = d.Default
		}
		if d.Kind == KindSecret {
			str, _ := v.(string)
			v = mask(str)
		}
		out = append(out, Item{Key: d.Key, Kind: d.Kind, Value: v, Default: d.Default, Set: set,
			Restart: d.Restart, Min: d.Min, Max: d.Max})
	}
	return out
}

// mask 只露末 4 位（TT-F-28）。
func mask(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return ""
	}
	if len(r) <= 4 {
		return "****"
	}
	return "****" + string(r[len(r)-4:])
}
