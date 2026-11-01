-- 时间线与渲染（数据模型 §11.2，NEW-07~NEW-11）
-- 起因：旧系统只有逐镜头视频，没有拼接成片。
-- render_job 同时存 timeline_version + snapshot + ffmpeg_version：
-- 只记时间线版本的话，升级 ffmpeg 后同一版本渲染出不同结果就无法解释。
-- +goose Up

create table timeline (
    id           bigint primary key,
    workspace_id bigint      not null,
    project_id   bigint      not null,
    script_id    bigint,
    name         varchar(64) not null,
    version      integer     not null default 1,
    aspect_ratio varchar(16) not null default '16:9',
    fps          integer     not null default 30,
    width        integer     not null default 1920,
    height       integer     not null default 1080,
    status       varchar(16) not null default 'editing',
    created_at   timestamptz not null default now(),
    updated_at   timestamptz not null default now(),
    constraint ck_timeline_status check (status in ('editing','archived')),
    constraint ck_timeline_fps    check (fps between 1 and 120)
);
create index idx_timeline_project on timeline (project_id, updated_at desc);

create table timeline_track (
    id          bigint primary key,
    timeline_id bigint      not null,
    kind        varchar(16) not null,
    idx         integer     not null,
    name        varchar(64),
    is_muted    boolean     not null default false,
    volume      integer     not null default 100,
    created_at  timestamptz not null default now(),
    constraint ck_track_kind   check (kind in ('video','audio','subtitle')),
    constraint ck_track_volume check (volume between 0 and 200)
);
create unique index uk_track_idx on timeline_track (timeline_id, kind, idx);

create table timeline_clip (
    id              bigint primary key,
    track_id        bigint  not null,
    idx             integer not null,
    media_object_id bigint  not null,
    source_in_ms    integer not null default 0,
    source_out_ms   integer not null,
    start_ms        integer not null,
    duration_ms     integer not null,
    transition_in   jsonb,
    transition_out  jsonb,
    speed           integer not null default 100,
    volume          integer not null default 100,
    fade_in_ms      integer not null default 0,
    fade_out_ms     integer not null default 0,
    created_at      timestamptz not null default now(),
    constraint ck_clip_range    check (source_out_ms > source_in_ms),
    constraint ck_clip_duration check (duration_ms > 0),
    constraint ck_clip_start    check (start_ms >= 0)
);
create unique index uk_clip_idx on timeline_clip (track_id, idx);

create table timeline_subtitle (
    id          bigint primary key,
    timeline_id bigint  not null,
    start_ms    integer not null,
    end_ms      integer not null,
    text        text    not null,
    style       jsonb,
    created_at  timestamptz not null default now(),
    constraint ck_subtitle_range check (end_ms > start_ms)
);
create index idx_subtitle_time on timeline_subtitle (timeline_id, start_ms);

create table render_job (
    id               bigint primary key,
    workspace_id     bigint      not null,
    timeline_id      bigint      not null,
    -- 固定版本渲染：编辑时间线不影响进行中的导出（NEW-10）。
    timeline_version integer     not null,
    snapshot         jsonb       not null,
    -- ⚠️ 必须记 ffmpeg 版本：8.x 的 filter 行为跨版本有差异。
    ffmpeg_version   varchar(64),
    payer_account_id bigint      not null,
    created_by_user_id bigint    not null,
    status           varchar(16) not null default 'queued',
    progress         integer     not null default 0,
    stage            varchar(16),
    output_media_id  bigint,
    credits_cost     integer     not null default 0,
    error_code       varchar(32),
    error_message    varchar(1024),
    deadline_at      timestamptz,
    started_at       timestamptz,
    finished_at      timestamptz,
    created_at       timestamptz not null default now(),
    updated_at       timestamptz not null default now(),
    constraint ck_render_status check (status in ('queued','running','success','failed','cancelled')),
    constraint ck_render_stage  check (stage is null or stage in
        ('probing','assembling','encoding','archiving')),
    constraint ck_render_progress check (progress between 0 and 100)
);
create index idx_render_timeline on render_job (timeline_id, created_at desc);
create index idx_render_recovery on render_job (status, deadline_at) where status in ('queued','running');

-- +goose Down
drop table if exists render_job;
drop table if exists timeline_subtitle;
drop table if exists timeline_clip;
drop table if exists timeline_track;
drop table if exists timeline;
