-- 我的资产（资产库）：保存的生成结果、单层文件夹、自定义标签、生成结果评级（2026-10-30 用户需求）。
-- 起因：
--   ① 画布节点右键菜单的「评级」「保存到我的资产」此前只是灰着的占位；创作资产页（/works）此前列出工作区里**全部**成功的生成，
--      用户要的是「只有保存过的才展示」，并能按文件夹、标签整理，在画布左侧「资产」页签里挑出来再添加到画布。
--   ② 资产引用的是**一次生成的某一项产物**（generation + output_idx），不复制字节：媒体随 generation_output 已受删除保护
--      （media/app.ReferenceChecker 的 generation_output 一类），文本另存保存那一刻卡片上显示的内容（可能是用户改过的结果）。
--   ③ 同一项产物在一个工作区里只算一份（部分唯一索引，移出资产＝软删后可以再保存）。
--   ④ 评级挂在**生成结果**上、整个工作区共用（团队看到同一个星级），与是否保存无关；清除评级＝删行。
--   ⑤ 文件夹只有一层、同一工作区内不重名（不分大小写）；删文件夹时里面的资产退回顶层（应用层先挪再删）。
--   ⑥ 标签：「角色 / 场景 / 物品 / 风格 / 音效」是预置的（写在代码里），library_tag 只存「新建标签」建出来的；
--      资产上的标签存名字数组（GIN 索引，按标签筛选走 @>）。
-- +goose Up
create table library_folder (
    id                 bigint primary key,
    workspace_id       bigint      not null,
    name               varchar(60) not null,
    created_by_user_id bigint,
    created_at         timestamptz not null default now(),
    updated_at         timestamptz not null default now()
);
create unique index uk_library_folder_name on library_folder (workspace_id, lower(name));

create table library_item (
    id                 bigint primary key,
    workspace_id       bigint       not null,
    folder_id          bigint,
    generation_id      bigint       not null,
    output_idx         integer      not null default 0,
    kind               varchar(16)  not null,
    title              varchar(120) not null,
    media_object_id    bigint,
    text_content       text,
    project_id         bigint,
    canvas_node_id     varchar(64),
    tags               text[]       not null default '{}',
    created_by_user_id bigint,
    created_at         timestamptz  not null default now(),
    updated_at         timestamptz  not null default now(),
    deleted_at         timestamptz,
    constraint ck_library_item_kind check (kind in ('text', 'image', 'video', 'audio'))
);
create unique index uk_library_item_output on library_item (workspace_id, generation_id, output_idx) where deleted_at is null;
-- 列表按 id 倒序翻页（游标分页要求排序键唯一，同 00029 的教训）
create index idx_library_item_ws on library_item (workspace_id, id desc) where deleted_at is null;
create index idx_library_item_folder on library_item (folder_id) where deleted_at is null;
create index idx_library_item_tags on library_item using gin (tags);

create table library_tag (
    id           bigint primary key,
    workspace_id bigint      not null,
    name         varchar(20) not null,
    created_at   timestamptz not null default now()
);
create unique index uk_library_tag_name on library_tag (workspace_id, name);

create table generation_rating (
    generation_id    bigint primary key,
    workspace_id     bigint      not null,
    rating           smallint    not null,
    rated_by_user_id bigint,
    updated_at       timestamptz not null default now(),
    constraint ck_generation_rating check (rating between 1 and 5)
);
create index idx_generation_rating_ws on generation_rating (workspace_id);

-- +goose Down
drop table generation_rating;
drop table library_tag;
drop table library_item;
drop table library_folder;
