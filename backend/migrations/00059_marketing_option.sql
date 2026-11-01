-- +goose Up
-- 起因（MKT-T-03 / MKD-1 / MKD-4 / MKD-5）：营销 Agent 的「创意 · Hook · 风格」是运营维护的配置，
-- 提示词片段在服务端拼（MK-8，同图片预设）。三类字段重合度高 → 一张表 + kind；分类有序且要在
-- 前台成列展示 → 单独字典表（技能现在「按首次出现顺序派生分类」对运营不可控）；
-- 会话与生成参数只钉 (code, version) → 版本历史单独一张只增的表。
--
-- ⚠️ 版本历史只增不改：会话与生成参数只记 (code, version)，原文从这里取（MKD-5）。
--    复制原文的话片段会落进节点 params —— 也就是 canvasData，违反 MK-8。
--    每次保存「option.version+1 + 插一行 version」在**同一事务**，否则会出现
--    「当前版本在历史里不存在」。
-- ⚠️ code 建后不可改：历史会话、生成快照、版本历史都靠它追溯。code 跨 kind 全局唯一。
-- ⚠️ 新建默认停用（is_enabled=false）：上线是一次显式操作，不混进保存
--    （同公告「新建落草稿」：一次误保存不该等于全站上线）。
-- ⚠️ 投放时段现算 [start_at, end_at)，不靠作业翻开关（CD-61）；CHECK 用 `<` 拒倒置与零长度。
-- ⚠️ 不提供删除：历史快照按 code 引用（选项与分类都只停用）。
-- ⚠️ 不写任何 seed（MKD-21）：未审定的建议稿不得随发布上线。
create table marketing_category (
  id          bigint primary key,
  kind        varchar(16) not null,
  code        varchar(48) not null,
  name        varchar(24) not null,
  sort        integer not null default 0,
  is_enabled  boolean not null default true,
  created_at  timestamptz not null default now(),
  updated_at  timestamptz not null default now(),
  constraint ck_marketing_category_kind check (kind in ('creative','hook','style'))
);
create unique index uk_marketing_category on marketing_category (kind, code);

create table marketing_option (
  id                      bigint primary key,
  kind                    varchar(16) not null,
  code                    varchar(64) not null,
  name                    varchar(24) not null,
  category_code           varchar(48) not null,
  description             varchar(60) not null default '',
  cover_media_ids         jsonb not null default '[]',   -- 创意 1~3、Hook/风格 1（应用层校验）
  preview_media_id        bigint,                        -- 示例视频，可空
  prompt                  text not null default '',      -- 服务端片段，≤1000 字（应用层校验）
  output_types            jsonb not null default '["video"]',
  input_requirements      jsonb not null default '{}',   -- {"product":"required","character":"optional"}
  is_audio_required       boolean not null default false,
  recommended_models      jsonb not null default '[]',   -- creative 专用
  recommended_params      jsonb not null default '{}',   -- {"aspectRatio":"9:16","durationSec":[10,15]}
  default_skill_code      varchar(64),                   -- creative 专用
  tags                    jsonb not null default '[]',   -- 「口播」标签触发 CheckTalkCopy
  start_at                timestamptz,
  end_at                  timestamptz,
  sort                    integer not null default 0,
  is_enabled              boolean not null default false,
  version                 integer not null default 1,
  created_by_admin_id     bigint,
  updated_by_admin_id     bigint,
  created_at              timestamptz not null default now(),
  updated_at              timestamptz not null default now(),
  constraint ck_marketing_option_kind check (kind in ('creative','hook','style')),
  constraint ck_marketing_option_window check (start_at is null or end_at is null or start_at < end_at),
  constraint ck_marketing_option_version check (version >= 1)
);
create unique index uk_marketing_option_code on marketing_option (code);
-- 前台列表：按 kind + 分类 + 排序取启用的；时段条件在 where 里现算。
create index idx_marketing_option_list on marketing_option (kind, is_enabled, category_code, sort, id);

create table marketing_option_version (
  id                   bigint primary key,
  option_id            bigint not null,
  version              integer not null,
  snapshot             jsonb not null,   -- 该版本的 name/description/prompt/tags/isAudioRequired/outputTypes/inputRequirements
  created_by_admin_id  bigint,
  created_at           timestamptz not null default now()
);
create unique index uk_marketing_option_version on marketing_option_version (option_id, version);

-- +goose Down
drop table if exists marketing_option_version;
drop table if exists marketing_option;
drop table if exists marketing_category;
