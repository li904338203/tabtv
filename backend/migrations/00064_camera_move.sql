-- 运镜目录（视频节点「运镜」）。
-- 起因：视频节点的运镜要像对标产品那样从「运镜广场」里挑（固定镜头、跟随拍摄、盘旋抬升、镜头上摇……
-- 手持拍摄），选中后在输入框里以标签显示，生成时由服务端把这一项的提示词拼进去。
-- 此前试过把运镜做成节点参数里的几组选项（镜头运动 / 景别 / 机位），与对标产品的用法对不上。
--
-- 运镜与风格、特效同属素材目录：有封面、可收藏、后台可维护、**提示词不下发前端**（拼接在服务端），
-- 所以照 effect 建一张同构的表；收藏复用 material_favorite，只放开 material_type 的取值。
-- 只修饰视频节点（catalog/domain.MaterialType.TargetNodeType）。
-- +goose Up

create table camera_move (
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
create unique index uk_camera_move_code on camera_move (code);

alter table material_favorite drop constraint ck_favorite_type;
alter table material_favorite add constraint ck_favorite_type
    check (material_type in ('style','effect','voice','camera'));

-- +goose Down
delete from material_favorite where material_type = 'camera';
alter table material_favorite drop constraint ck_favorite_type;
alter table material_favorite add constraint ck_favorite_type
    check (material_type in ('style','effect','voice'));
drop table if exists camera_move;
