-- 起因：提示词的 `stale` 要**可识别**，而不是靠"记得在每个改动点标一下"。
--
-- AV-16 要求「上游内容变更影响下游就绪状态」。做法有两种：
--
--   ① 在每个会改上游的地方主动把下游标 stale（改镜头、改资产、改全片风格、
--      reconcile 重写描述、重新生成分镜…）；
--   ② 合成提示词时记下**上游的指纹**，读的时候比一比。
--
-- 只做 ① 的问题是：漏掉任何一个调用点都不会有编译错误，症状是
-- 用户改了资产描述、提示词却还显示 ready —— 他拿着一段过时的词去出图，
-- 画面里那个角色还是旧的样子，而界面上一切正常。
--
-- 这里选 ②：指纹存在行上，任何路径改了上游都会被比出来。①  作为
-- 即时反馈仍然保留（改完立刻变灰），但正确性不依赖它。

-- +goose Up
alter table shot
    add column image_prompt_fingerprint varchar(16),
    add column video_prompt_fingerprint varchar(16);

comment on column shot.image_prompt_fingerprint is
    '合成图片提示词时上游内容的指纹；与当前上游不一致即为 stale。空表示从未合成过';
comment on column shot.video_prompt_fingerprint is
    '合成视频提示词时上游内容的指纹；与当前上游不一致即为 stale。空表示从未合成过';

-- +goose Down
alter table shot
    drop column if exists video_prompt_fingerprint,
    drop column if exists image_prompt_fingerprint;
