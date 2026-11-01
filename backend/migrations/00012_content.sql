-- 内容、作品、素材与通知（数据模型 §12）
-- 起因：work 独立建表——成片来自 render_job 不是 generation，
-- 且审核状态属于作品层不属于任务层（CD-06）。
-- +goose Up

create table style (
    id             bigint primary key,
    code           varchar(64) not null,
    name           varchar(64) not null,
    cover_media_id bigint,
    category       varchar(32),
    prompt         text,
    author         varchar(64),
    is_commercial  boolean     not null default false,
    sort           integer     not null default 0,
    is_enabled     boolean     not null default true,
    created_at     timestamptz not null default now(),
    updated_at     timestamptz not null default now()
);
create unique index uk_style_code on style (code);

create table effect (
    id             bigint primary key,
    code           varchar(64) not null,
    name           varchar(64) not null,
    cover_media_id bigint,
    category       varchar(32),
    prompt         text,
    author         varchar(64),
    is_commercial  boolean     not null default false,
    sort           integer     not null default 0,
    is_enabled     boolean     not null default true,
    created_at     timestamptz not null default now(),
    updated_at     timestamptz not null default now()
);
create unique index uk_effect_code on effect (code);

create table voice (
    id             bigint primary key,
    code           varchar(64) not null,
    name           varchar(64) not null,
    icon_media_id  bigint,
    preview_media_id bigint,
    upstream_voice_id varchar(128),
    language       varchar(16),
    gender         varchar(16),
    sort           integer     not null default 0,
    is_enabled     boolean     not null default true,
    created_at     timestamptz not null default now(),
    updated_at     timestamptz not null default now(),
    constraint ck_voice_gender check (gender is null or gender in ('male','female','neutral'))
);
create unique index uk_voice_code on voice (code);

-- 三张 *_favorite 合并为一张：结构完全相同，分表会让收藏列表 union 三次。
create table material_favorite (
    id            bigint primary key,
    user_id       bigint      not null,
    material_type varchar(16) not null,
    material_id   bigint      not null,
    created_at    timestamptz not null default now(),
    constraint ck_favorite_type check (material_type in ('style','effect','voice'))
);
create unique index uk_favorite on material_favorite (user_id, material_type, material_id);

-- 作品。source_type 统一 generation 与 render 两种来源（FULL-03）。
create table work (
    id                 bigint primary key,
    workspace_id       bigint      not null,
    source_type        varchar(16) not null,
    source_id          bigint      not null,
    media_object_id    bigint,
    title              varchar(100),
    review_status      varchar(16) not null default 'pending',
    is_featured        boolean     not null default false,
    is_offline         boolean     not null default false,
    submitted_at       timestamptz,
    reviewed_by_admin_id bigint,
    review_reason      varchar(255),
    created_at         timestamptz not null default now(),
    updated_at         timestamptz not null default now(),
    constraint ck_work_source check (source_type in ('generation','render')),
    constraint ck_work_review check (review_status in ('pending','approved','rejected'))
);
create unique index uk_work_source on work (source_type, source_id);
-- 公开可见性 = approved ∧ featured ∧ ¬offline，独立于任务状态。
create index idx_work_public on work (review_status, is_featured, is_offline, created_at desc);
create index idx_work_ws on work (workspace_id, created_at desc);

create table content_review (
    id                bigint primary key,
    work_id           bigint      not null,
    action            varchar(16) not null,
    operator_admin_id bigint      not null,
    reason            varchar(255),
    created_at        timestamptz not null default now(),
    constraint ck_review_action check (action in ('approved','rejected','offline','featured','unfeatured'))
);
create index idx_review_work on content_review (work_id, created_at desc);

create table banner (
    id              bigint primary key,
    position        varchar(32) not null,
    title           varchar(100) not null,
    media_object_id bigint,
    cover_media_id  bigint,
    link_url        varchar(512),
    sort            integer     not null default 0,
    is_enabled      boolean     not null default false,
    start_at        timestamptz,
    end_at          timestamptz,
    created_at      timestamptz not null default now(),
    updated_at      timestamptz not null default now()
);
create index idx_banner_active on banner (position, is_enabled, start_at, end_at);

-- ⚠️ 公告全站一行，**不按人扇出**：十万用户扇出就是十万行，
-- 而绝大多数人从不点开。代价是消息中心要在 SQL 里 union 两张表。
create table announcement (
    id           bigint primary key,
    title        varchar(100) not null,
    content      text        not null,
    status       varchar(16) not null default 'draft',
    link_url     varchar(512),
    published_at timestamptz,
    created_by_admin_id bigint not null,
    created_at   timestamptz not null default now(),
    updated_at   timestamptz not null default now(),
    constraint ck_announcement_status check (status in ('draft','published','offline'))
);
create index idx_announcement_pub on announcement (status, published_at desc);

create table announcement_read (
    id              bigint primary key,
    announcement_id bigint      not null,
    user_id         bigint      not null,
    read_at         timestamptz not null default now()
);
create unique index uk_ann_read on announcement_read (announcement_id, user_id);

create table notification (
    id         bigint primary key,
    user_id    bigint      not null,
    type       varchar(32) not null,
    title      varchar(100) not null,
    content    text,
    ref_type   varchar(32),
    ref_id     varchar(128),
    link_url   varchar(512),
    read_at    timestamptz,
    created_at timestamptz not null default now()
);
-- ⚠️ 幂等：Worker 重试同一任务不会发第二条（FLOW-10）。
-- 审核类 ref_id 要带动作（如 "217:approved"），否则先驳回后通过只能发出一条。
create unique index uk_notification_ref on notification (user_id, type, ref_type, ref_id)
    where ref_id is not null;
create index idx_notification_user on notification (user_id, created_at desc);
create index idx_notification_unread on notification (user_id) where read_at is null;

-- +goose Down
drop table if exists notification;
drop table if exists announcement_read;
drop table if exists announcement;
drop table if exists banner;
drop table if exists content_review;
drop table if exists work;
drop table if exists material_favorite;
drop table if exists voice;
drop table if exists effect;
drop table if exists style;
