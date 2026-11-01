-- Agent 技能与用户模型偏好（AGT-21；AG-13/14，docs/agent dev/02 §2.3）
--
-- 起因：技能 = 主说明 + 按需读取的附属文档 + 步骤检查点 + 工具白名单（design/01 §10）。
-- 创作要求要能由运营调优而不发版，所以进表；卡片结构、规划骨架等**格式契约**在代码里
-- （agent/domain/prompt.go），不进这里。
-- 阶段 B 的「加载技能」、偏好接口与首批技能都要用这三张表，所以不等到阶段 E 的管理端。
--
-- ⚠️ agent_skill.tools 是白名单，**只能收窄**状态机允许的工具，不能放宽
--    （由 agent/domain.AllowedTools 取交集保证，表里写了 generate 也进不了规划态）。
-- ⚠️ agent_user_pref 一人一行（主键 user_id）：偏好是用户级的，项目 / 会话级覆盖存在 agent_session.prefs。
-- +goose Up
create table agent_skill (
  id               bigint primary key,
  code             varchar(64) not null,
  name             varchar(64) not null,
  category         varchar(32) not null,
  description      varchar(500) not null default '',
  tags             jsonb not null default '[]',
  cover_media_id   bigint,
  body             text not null,
  steps            jsonb not null default '[]',
  tools            jsonb not null default '[]',
  model_bindings   jsonb not null default '{}',
  is_quick_entry   boolean not null default false,
  sort             integer not null default 0,
  is_enabled       boolean not null default true,
  version          integer not null default 1,
  created_at       timestamptz not null default now(),
  updated_at       timestamptz not null default now()
);
create unique index uk_agent_skill_code on agent_skill (code);

create table agent_skill_reference (
  id         bigint primary key,
  skill_id   bigint not null,
  name       varchar(64) not null,
  body       text not null,
  sort       integer not null default 0,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);
create unique index uk_agent_skill_ref on agent_skill_reference (skill_id, name);

create table agent_user_pref (
  user_id     bigint primary key,
  prefs       jsonb not null default '{}',
  updated_at  timestamptz not null default now()
);

-- +goose Down
drop table agent_user_pref;
drop table agent_skill_reference;
drop table agent_skill;
