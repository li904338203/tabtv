-- 营销商品库与角色库（docs/agent/marketing 02 MK-22~27，dev/05 §2；MKT-T-33）
--
-- 起因：营销输入框原先让用户每次临时上传商品图（MK-2），同一个商品每次创作都要重传、
-- 重写卖点；角色只能直接上传人物图或挑脚本里的角色资产。2026-10-23 改为工作区级
-- 商品库与非真人角色库（MK-22/23），另有运营维护的平台预置「营销角色」。
-- 三张表同属营销域、同一批合入，一个迁移建齐：拆开的话表数断言要按合入顺序改两次。
--
-- ⚠️ 角色**不复用 script_asset**（dev/05 MKD-23）：脚本重绑定按名字命中工作区里
--    未被镜头引用的资产，营销角色存进去会被任意脚本的 @同名 自动绑上（AV-20）。
-- ⚠️ 会话不引用这些表的行：建会话时冻结快照进 agent_session.scene_input（MKD-25），
--    之后编辑 / 删除商品与角色不改变已建会话。
-- ⚠️ 删除是软删（deleted_at）：媒体引用检查器只认未删除的行（MKD-31）。
-- ⚠️ 预置角色单独一张表、没有 workspace 列（MKD-30）：用户「添加」时复制字节进
--    本工作区，预置的媒体永远不直接作为生成输入。
-- ⚠️ 张数、图片格式、媒体归属、名称禁用字符在应用层（marketing/domain）校验，不进 CHECK：
--    上限是配置值（SEEAITV_MARKETING_MAX_PRODUCT_IMAGES），写进 CHECK 等于改配置要改迁移。

-- +goose Up
create table marketing_product (
  id                  bigint primary key,
  workspace_id        bigint not null,
  name                varchar(30) not null,
  selling_points      varchar(300) not null default '',
  description         varchar(300) not null default '',
  image_media_ids     jsonb not null default '[]',   -- 有序 mediaId（string），[0] 为主图；1~10（应用层校验）
  version             integer not null default 1,
  created_by_user_id  bigint,
  updated_by_user_id  bigint,
  created_at          timestamptz not null default now(),
  updated_at          timestamptz not null default now(),
  deleted_at          timestamptz,
  constraint ck_marketing_product_images check (jsonb_typeof(image_media_ids) = 'array'),
  constraint ck_marketing_product_version check (version >= 1)
);
create index idx_marketing_product_list on marketing_product (workspace_id, updated_at desc, id desc)
  where deleted_at is null;
-- 媒体引用保护（ReferenceChecker）按 image_media_ids @> '["<id>"]' 查。
create index idx_marketing_product_media on marketing_product using gin (image_media_ids jsonb_path_ops)
  where deleted_at is null;

create table marketing_character (
  id                     bigint primary key,
  workspace_id           bigint not null,
  name                   varchar(50) not null,
  image_media_ids        jsonb not null default '[]',   -- 1~9，[0] 为主图
  appearance             varchar(500) not null default '',
  style_code             varchar(64),                   -- 最近一次生成用的画风（风格素材 code），可空
  voice_text             varchar(500) not null default '',
  voice_media_id         bigint,                        -- 参考音频，只存不发（MKD-33）
  source                 varchar(16) not null default 'user',
  source_preset_id       bigint,
  source_preset_version  integer,
  version                integer not null default 1,
  created_by_user_id     bigint,
  updated_by_user_id     bigint,
  created_at             timestamptz not null default now(),
  updated_at             timestamptz not null default now(),
  deleted_at             timestamptz,
  constraint ck_marketing_character_source check (source in ('user','preset')),
  constraint ck_marketing_character_preset check (
    (source = 'preset') = (source_preset_id is not null and source_preset_version is not null)),
  constraint ck_marketing_character_images check (jsonb_typeof(image_media_ids) = 'array'),
  constraint ck_marketing_character_version check (version >= 1)
);
-- 同工作区未删除的角色不重名（MKD-36）。
create unique index uk_marketing_character_name on marketing_character (workspace_id, lower(name))
  where deleted_at is null;
-- 同一预置角色同一版本在一个工作区只复制一份（MKD-30）。
create unique index uk_marketing_character_preset
  on marketing_character (workspace_id, source_preset_id, source_preset_version)
  where deleted_at is null and source_preset_id is not null;
create index idx_marketing_character_list on marketing_character (workspace_id, updated_at desc, id desc)
  where deleted_at is null;
create index idx_marketing_character_media on marketing_character using gin (image_media_ids jsonb_path_ops)
  where deleted_at is null;
create index idx_marketing_character_voice on marketing_character (voice_media_id)
  where deleted_at is null and voice_media_id is not null;

create table marketing_preset_character (
  id                   bigint primary key,
  name                 varchar(50) not null,
  summary              varchar(60) not null default '',
  image_media_ids      jsonb not null default '[]',    -- 管理端上传（workspace_id=0, purpose=material_cover）
  appearance           varchar(500) not null default '',
  voice_text           varchar(500) not null default '',
  voice_media_id       bigint,
  sort                 integer not null default 0,
  is_enabled           boolean not null default false,  -- 新建默认停用：上线是一次显式操作
  version              integer not null default 1,
  created_by_admin_id  bigint,
  updated_by_admin_id  bigint,
  created_at           timestamptz not null default now(),
  updated_at           timestamptz not null default now(),
  constraint ck_marketing_preset_character_images check (jsonb_typeof(image_media_ids) = 'array'),
  constraint ck_marketing_preset_character_version check (version >= 1)
);
create unique index uk_marketing_preset_character_name on marketing_preset_character (lower(name));
create index idx_marketing_preset_character_list on marketing_preset_character (is_enabled, sort, id);

-- +goose Down
drop table if exists marketing_preset_character;
drop table if exists marketing_character;
drop table if exists marketing_product;
