-- 脚本、镜头、资产与组（数据模型 §10）
-- 起因：script_asset 改名（旧表叫 asset，与素材库和 media_object 混淆）；
-- group_item 拆表（07 §6 明确建议，JSONB 整体读改写在并发下必丢更新）。
-- +goose Up

create table script_instance (
    id             bigint primary key,
    workspace_id   bigint      not null,
    project_id     bigint      not null,
    canvas_node_id varchar(64) not null,
    source_type    varchar(16) not null,
    current_step   integer     not null default 1,
    status         varchar(16) not null default 'editing',
    raw_input      text,
    global_style   varchar(512),
    asset_profiles jsonb       not null default '{}'::jsonb,
    version        integer     not null default 1,
    created_at     timestamptz not null default now(),
    updated_at     timestamptz not null default now(),
    constraint ck_script_source check (source_type in ('script','character','manual')),
    constraint ck_script_step   check (current_step between 1 and 3)
);
-- ⚠️ 脚本与画布节点 1:1。这条约束让「限定脚本」等价于「限定节点」，
-- 是资产按节点隔离的基础：旧系统按用户级同名匹配，
-- 导致新节点里的 @角色 命中数周前的记录，连 failed 状态一起继承过来。
create unique index uk_script_canvas_node on script_instance (project_id, canvas_node_id);

create table shot (
    id                  bigint primary key,
    script_id           bigint      not null,
    shot_no             integer     not null,
    duration_sec        integer     not null default 5,
    description         text,
    shot_size           varchar(32),
    lighting            varchar(64),
    dialogue            text,
    sound_effect        varchar(255),
    camera_move         varchar(64),
    image_prompt        text,
    image_prompt_status varchar(16) not null default 'pending',
    image_prompt_source varchar(16),
    video_prompt        text,
    video_prompt_status varchar(16) not null default 'pending',
    video_prompt_source varchar(16),
    entities            jsonb       not null default '[]'::jsonb,
    start_state         text,
    end_state           text,
    version             integer     not null default 1,
    created_at          timestamptz not null default now(),
    updated_at          timestamptz not null default now(),
    constraint ck_shot_duration check (duration_sec between 1 and 60),
    constraint ck_shot_img_status check (image_prompt_status in ('pending','ready','stale','failed')),
    constraint ck_shot_vid_status check (video_prompt_status in ('pending','ready','stale','failed')),
    constraint ck_shot_img_source check (image_prompt_source is null or image_prompt_source in ('ai','template','manual')),
    constraint ck_shot_vid_source check (video_prompt_source is null or video_prompt_source in ('ai','template','manual'))
);
create unique index uk_shot_no on shot (script_id, shot_no);

create table script_asset (
    id              bigint primary key,
    workspace_id    bigint      not null,
    asset_type      varchar(16) not null,
    name            varchar(64) not null,
    description     text,
    profile         jsonb,
    media_object_id bigint,
    source          varchar(16) not null default 'ai',
    status          varchar(16) not null default 'pending',
    created_at      timestamptz not null default now(),
    updated_at      timestamptz not null default now(),
    constraint ck_asset_type   check (asset_type in ('character','scene','prop')),
    constraint ck_asset_source check (source in ('ai','upload','template')),
    constraint ck_asset_status check (status in ('pending','generating','ready','failed'))
);
-- 工作区级资产库，配合 resource_share 支撑跨脚本复用（NEW-16）。
create index idx_asset_ws on script_asset (workspace_id, asset_type, name);

create table shot_asset_ref (
    id              bigint primary key,
    shot_id         bigint  not null,
    script_asset_id bigint  not null,
    -- ⚠️ sort 决定参考图锚定顺序，与提示词里的编号一一对应，不可丢失。
    sort            integer not null default 0
);
create unique index uk_shot_asset on shot_asset_ref (shot_id, script_asset_id);
create index idx_shot_asset_by_asset on shot_asset_ref (script_asset_id);

-- 同步 LLM 计费，与普通 generation 分离：产出是结构化文本不是媒体，
-- 走 MQ 只多一层状态同步（06 §4.1）。
create table script_llm_call (
    id               bigint primary key,
    workspace_id     bigint      not null,
    payer_account_id bigint      not null,
    script_id        bigint      not null,
    shot_id          bigint,
    purpose          varchar(32) not null,
    model_id         bigint      not null,
    -- ⚠️ 幂等键随每次重新生成而变，且**失败也要计数**：
    -- 只按产出物算轮次的话，一次失败什么都没改变，重试会算出同一个键、
    -- 命中那条 failed 记录，用户再也点不动「重新生成」。
    idempotency_key  varchar(128) not null,
    status           varchar(16) not null default 'running',
    credits_cost     integer     not null default 0,
    result           jsonb,
    error_message    varchar(1024),
    created_at       timestamptz not null default now(),
    updated_at       timestamptz not null default now(),
    constraint ck_llm_purpose check (purpose in ('storyboard_gen','prompt_compose')),
    constraint ck_llm_status  check (status in ('running','success','failed'))
);
create unique index uk_llm_idem on script_llm_call (script_id, idempotency_key);
create index idx_llm_script on script_llm_call (script_id, created_at desc);

create table prompt_template (
    id         bigint primary key,
    code       varchar(64) not null,
    model_code varchar(64),
    content    text        not null,
    status     varchar(16) not null default 'published',
    version    integer     not null default 1,
    updated_by_admin_id bigint,
    created_at timestamptz not null default now(),
    updated_at timestamptz not null default now(),
    constraint ck_prompt_status check (status in ('draft','published','archived'))
);
-- 读取优先模型定制、再通用；nulls not distinct 让通用行也受唯一约束保护。
create unique index uk_prompt on prompt_template (code, model_code) nulls not distinct;

create table group_node (
    id             bigint primary key,
    workspace_id   bigint      not null,
    project_id     bigint      not null,
    canvas_node_id varchar(64) not null,
    group_type     varchar(32) not null,
    script_id      bigint,
    name           varchar(64),
    status         varchar(16) not null default 'draft',
    version        integer     not null default 1,
    created_at     timestamptz not null default now(),
    updated_at     timestamptz not null default now(),
    constraint ck_group_type   check (group_type in ('asset','storyboard_image','video_generator')),
    constraint ck_group_status check (status in
        ('draft','running','success','failed','cancelled','partial_success','ungrouped'))
);
create unique index uk_group_canvas_node on group_node (project_id, canvas_node_id);

-- 拆表而非 JSONB：逐项重试、逐项计费、部分成功都要求对单个子项做条件更新。
create table group_item (
    id               bigint primary key,
    group_node_id    bigint      not null,
    item_no          integer     not null,
    shot_id          bigint,
    script_asset_id  bigint,
    generation_id    bigint,
    -- ⚠️ stale 检测：镜头改了之后旧 item 不能默默用过时 prompt 生成（FLOW-05）。
    shot_version     integer,
    params_snapshot  jsonb       not null default '{}'::jsonb,
    input_refs       jsonb       not null default '[]'::jsonb,
    status           varchar(16) not null default 'draft',
    progress         integer     not null default 0,
    credits_cost     integer     not null default 0,
    error_message    varchar(1024),
    created_at       timestamptz not null default now(),
    updated_at       timestamptz not null default now(),
    constraint ck_group_item_status check (status in
        ('draft','queued','running','success','failed','cancelled','stale'))
);
create unique index uk_group_item on group_item (group_node_id, item_no);
create index idx_group_item_gen on group_item (generation_id) where generation_id is not null;

-- +goose Down
drop table if exists group_item;
drop table if exists group_node;
drop table if exists prompt_template;
drop table if exists script_llm_call;
drop table if exists shot_asset_ref;
drop table if exists script_asset;
drop table if exists shot;
drop table if exists script_instance;
