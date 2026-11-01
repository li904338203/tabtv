-- +goose Up
-- 起因：生成任务全部终态后，扫描循环会唤醒会话跑一个不计费的 system 轮，并写一条
-- 「所等任务各自的结局」说明。它原先以 system_note 落库，而 system_note 的语义是
-- 「只给用户看、不进模型上下文」—— 于是唤醒轮里模型只看得到当初 generate 返回的
-- 「已受理」，不知道成败与产出，续跑出的总结是盲写的（AGT-25 发现）。
--
-- 用一个独立角色区分，而不是按文案前缀判断（文案改一个字判断就失效，且没有报错）：
--   system_note  —— 只给用户看（截断提示、卡片作废说明…），不进模型上下文
--   system_event —— 系统事件（任务结果…），用户看得到，**也进模型上下文**（以 user 角色 + 前缀交给模型）
alter table agent_message drop constraint ck_agent_message_role;
alter table agent_message add constraint ck_agent_message_role
  check (role in ('user','assistant','tool','system_note','system_event'));

-- +goose Down
update agent_message set role = 'system_note' where role = 'system_event';
alter table agent_message drop constraint ck_agent_message_role;
alter table agent_message add constraint ck_agent_message_role
  check (role in ('user','assistant','tool','system_note'));
