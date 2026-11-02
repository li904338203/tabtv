-- 本机设置表（tabtv TT-T-12 / TT-T-22）。
-- 起因：来源系统的配置全部来自环境变量（服务端部署的做法），桌面用户没有 shell 环境，
--   并发上限、日志保留期、可选 S3、局域网上游开关这些要能在「设置」页里改。
--   一个键一行、值存 jsonb：键是代码里写死的白名单（platform/settings），不是任意配置项。
-- +goose Up
create table app_setting (
    key        varchar(100) primary key,
    value      jsonb        not null,
    updated_at timestamptz  not null default now()
);

-- +goose Down
drop table if exists app_setting;
