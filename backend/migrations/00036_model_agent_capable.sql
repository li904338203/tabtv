-- 模型目录：「支持 Agent」标记（AGT-04，AG-P1 / AG-14，决策 D-4）
--
-- 起因：画布 Agent 的推理模型要做「多轮 + 工具 + 流式」调用，而能说这件事的协议
-- 只有 openai_chat 与 anthropic_messages（`protocol.IsChatCapable`）。只看 domain='text'
-- 去选推理模型的话，配成 generic_sync 的文本模型、只会单轮润色的中转模型都会出现在
-- 推理模型下拉里 —— 选了之后第一轮就报「协议不支持对话」，而配置页看不出任何问题。
-- 反过来也不能把「凡是 openai_chat 的文本模型」都当推理模型：能不能稳定做工具调用
-- 是**型号**的事（小模型会把工具参数写坏），只能由运营显式勾选。
--
-- ⚠️ 保存时校验 `domain='text' ∧ 协议可聊天`（目录域 SaveModel，42203），不在库上加 CHECK：
-- 协议能力来自代码里的注册表，写进约束等于在 SQL 里再维护一份协议名单（GAP-09）。
-- ⚠️ 管理端保存是整条替换：这一列在接口上是三态（不传 = 保留），否则老调用方
-- （配置脚本、联调工具）一保存就把标记清掉。
-- +goose Up
alter table model add column is_agent_capable boolean not null default false;

-- +goose Down
alter table model drop column if exists is_agent_capable;
