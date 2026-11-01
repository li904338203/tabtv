-- 派生媒体与异步导出（NEW-14、NEW-15、NEW-16、GAP-12、GAP-18）
--
-- 起因一：宫格切分要产出**独立素材**，但"这块是从哪张图的第几格切来的"
-- 必须留下来 —— 不留的话用户面对九张一模一样构图的图分不清谁是谁，
-- 而切分的全部价值就是这九块能各自独立地继续参与生成。
--
-- 起因二：派生关系是**逻辑外键且不级联**。源图删了，切块仍然有效
-- （NEW-16 明确要求"源节点删除不破坏仍有效引用"）：切块是一份
-- 独立的字节，它的存在不依赖源图还在不在。source_media_id 只是溯源线索，
-- 查不到源图时按"源已删除"显示，不是错误。
--
-- 起因三：批量下载在旧系统是前端逐个 a.href 直链（GAP-18），既绕过了
-- 鉴权、又在几十项时把浏览器打满。改成服务端异步打包：任务化之后
-- 进度、失败、过期清理才有地方表达。
-- +goose Up

-- ---- 派生关系：记在 media_object 上，不另立一张关联表 ----
-- 一份派生媒体**只有一个**来源（切块来自一张图），一对一的关系放关联表
-- 等于每次查都要 join 一次，而 join 回来的永远是 0 或 1 行。
alter table media_object add column source_media_id bigint;
alter table media_object add column derive_kind    varchar(24);
-- derive_key 是**同一种派生下的变体**，宫格切分填 '3x3'。
-- ⚠️ 没有它的话唯一约束只能落在 (源图, 种类, 编号) 上，等于一张图
-- 一辈子只能切一种格数 —— 用户先切 2×2 再想切 3×3 会撞唯一索引，
-- 而那是个完全正当的操作。
alter table media_object add column derive_key     varchar(64);
-- derive_index 从 1 起，直接显示给用户（"第 3 格"）。
alter table media_object add column derive_index   integer;
-- derive_meta 存位置信息（行/列/像素矩形）。用 jsonb 而不是拆成四列：
-- 将来"按区域自由切分"的矩形不是行列能表达的，那时不该再加一次列。
alter table media_object add column derive_meta    jsonb;

alter table media_object add constraint ck_media_derive_kind
    check (derive_kind is null or derive_kind in ('grid_tile'));
-- 同一张源图、同一种格数下的同一格只该有一份：重复点"切分"不该越切越多。
-- ⚠️ 用 coalesce 而不是直接用列：PG 的唯一索引里 null 彼此不相等，
-- 留着 null 就等于这条约束对没填 derive_key 的行完全失效。
create unique index uk_media_derive on media_object
    (source_media_id, derive_kind, coalesce(derive_key,''), derive_index)
    where source_media_id is not null;
create index idx_media_source on media_object (source_media_id) where source_media_id is not null;

-- 导出压缩包是一种新的媒体用途。
alter table media_object drop constraint ck_media_purpose;
alter table media_object add constraint ck_media_purpose check (purpose in
    ('upload_reference','generation_output','render_output','script_asset',
     'banner','avatar','material_cover','export_archive'));

-- ---- 异步导出任务 ----
create table export_task (
    id              bigint       primary key,
    workspace_id    bigint       not null,
    user_id         bigint       not null,
    kind            varchar(16)  not null default 'zip',
    status          varchar(16)  not null default 'queued',
    -- items 是**受理时固定下来的清单快照**：之后源产物被删、被改名都不影响
    -- 这一次导出的内容解释（与 generation 的参数快照同一条规则）。
    items           jsonb        not null,
    item_count      integer      not null default 0,
    -- 打好的包本身也是一个 media_object，走同一套受保护读取（GAP-18 的修法）。
    media_object_id bigint,
    size_bytes      bigint,
    error_code      varchar(64),
    error_message   varchar(1024),
    -- ⚠️ 过期是**算出来的**（now() > expires_at），不是清理作业标出来的：
    -- 靠作业标记的话，作业停了一天，界面上就有一整天的下载按钮点下去 404。
    expires_at      timestamptz  not null,
    created_at      timestamptz  not null default now(),
    updated_at      timestamptz  not null default now(),
    constraint ck_export_kind   check (kind in ('zip')),
    constraint ck_export_status check (status in ('queued','running','success','failed')),
    constraint ck_export_count  check (item_count >= 0)
);
-- 列表按工作区倒序翻页；id 是雪花，天然按时间倒序。
create index idx_export_ws on export_task (workspace_id, id desc);
-- 清理作业只关心已经出包、且已过期的那些。
create index idx_export_expired on export_task (expires_at)
    where status = 'success' and media_object_id is not null;

-- +goose Down
drop table if exists export_task;
drop index if exists idx_media_source;
drop index if exists uk_media_derive;
alter table media_object drop constraint if exists ck_media_derive_kind;
alter table media_object drop column if exists derive_meta;
alter table media_object drop column if exists derive_index;
alter table media_object drop column if exists derive_key;
alter table media_object drop column if exists derive_kind;
alter table media_object drop column if exists source_media_id;
alter table media_object drop constraint ck_media_purpose;
alter table media_object add constraint ck_media_purpose check (purpose in
    ('upload_reference','generation_output','render_output','script_asset',
     'banner','avatar','material_cover'));
