-- +goose Up
-- 起因（MKT-T-17 / MKD-2、MKD-8、MKD-10）：营销 Agent = 同一引擎 + 场景档案。场景落在**会话**上
-- 而不是项目上：它决定「这段对话怎么解释」，落项目的话同一项目里随后开的通用会话也会被拼上营销片段。
--
-- ⚠️ scene 建后不可改：改了历史消息的解释会变。没有任何更新路径写它（PATCH /agent/sessions/{id}
--    严格解码，带 scene 键一律 400）。
-- ⚠️ scene_input 只存 ID、名称、卖点与选项的 (code,version)，不存 URL、不存片段原文
--    （片段按 (code,version) 从 marketing_option_version 取，MKD-5）。
-- ⚠️ agent_step.plan_item_key / error_code：批量「余额不足停后续」「部分成功」按它们**现算**，
--    不打标记（漏标一处不报错，症状是余额不足后又提交了两个变体）。error_code 不加 CHECK：
--    受理侧的错误码会增加，CHECK 会让新错误码写不进来而整步失败。
-- ⚠️ agent_message.refs / agent_session.scene_input 的 GIN 索引给媒体引用保护用（MKT-T-23）：
--    不加的话「这张图还有没有会话在用」是全表扫，回收扫描会随会话量线性变慢。
-- 已有会话取默认值 scene='general'、scene_input='{}'，请求体与行为不变。只加列 / 索引，不动表数。
alter table agent_session add column scene       varchar(16) not null default 'general';
alter table agent_session add column scene_input jsonb not null default '{}';
alter table agent_session add constraint ck_agent_session_scene check (scene in ('general','marketing'));

alter table agent_step add column plan_item_key varchar(32);
alter table agent_step add column error_code    varchar(64);
create index idx_agent_step_plan on agent_step (session_id, plan_item_key) where plan_item_key is not null;

create index idx_agent_message_refs on agent_message using gin (refs jsonb_path_ops);
create index idx_agent_session_scene_input on agent_session using gin (scene_input jsonb_path_ops)
  where scene = 'marketing';

-- +goose Down
drop index if exists idx_agent_session_scene_input;
drop index if exists idx_agent_message_refs;
drop index if exists idx_agent_step_plan;
alter table agent_step drop column if exists error_code;
alter table agent_step drop column if exists plan_item_key;
alter table agent_session drop constraint if exists ck_agent_session_scene;
alter table agent_session drop column if exists scene_input;
alter table agent_session drop column if exists scene;
