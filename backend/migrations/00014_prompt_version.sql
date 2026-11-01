-- 提示词版本历史（P03-05）
--
-- 起因：数据模型 §10 把「后台发布/失效」列为建议新增，而 P03-05 要求
-- **版本发布与回滚**。回滚必须有不可变的历史快照，单行结构做不到。
--
-- 设计：prompt_template 保存「草稿」与「当前生效内容」两份；
-- prompt_template_version 保存每次发布的不可变快照。
-- 运行时只读 content（且 status='published'），永远不读草稿。
-- +goose Up

-- 草稿与生效内容分开：编辑草稿不影响线上，避免"边改边生效"。
alter table prompt_template add column draft_content text;
alter table prompt_template add column published_at timestamptz;
alter table prompt_template add column note varchar(255);

create table prompt_template_version (
    id                  bigint primary key,
    template_id         bigint      not null,
    version             integer     not null,
    content             text        not null,
    note                varchar(255),
    published_by_admin_id bigint,
    published_at        timestamptz not null default now(),
    -- rollback_of_version 非空表示这次发布是回滚到某个历史版本。
    -- 记下来才能回答"线上这版是怎么来的"。
    rollback_of_version integer
);
-- 同一模板的版本号唯一。
create unique index uk_prompt_version on prompt_template_version (template_id, version);
create index idx_prompt_version_tpl on prompt_template_version (template_id, version desc);

-- +goose Down
drop table if exists prompt_template_version;
alter table prompt_template drop column if exists note;
alter table prompt_template drop column if exists published_at;
alter table prompt_template drop column if exists draft_content;
