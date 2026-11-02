-- 去掉 bblabu_video（中转视频）与 uniart_video（UniArt 视频）两个协议（tabtv，2026-11-03）。
-- 起因：开源版不再提供这两个协议，适配器代码一并删除（用户要求）。启动时的协议同步只会把它们
--   标成 is_registered=false、不会删行；设置页下拉读的是代码里的注册表，本来就不会再出现。
--   这里把留在库里的两行也清掉。
-- ⚠️ 只删没有模型在用的（同 00077）：model.api_protocol 外键指向这张表，用户自己配过这两个协议的
--   模型时保留那一行 —— 模型照样能在设置里看到并改成别的协议，而不是因为删行让迁移失败、应用起不来。
-- +goose Up
delete from api_protocol a
where a.code in ('bblabu_video', 'uniart_video')
  and not exists (select 1 from model m where m.api_protocol = a.code);

-- +goose Down
-- 协议行由启动时的同步按代码注册表维护；回退到含这两个协议的版本时会自动补回，这里不需要恢复。
select 1;
