-- 生成任务的上游原始报错与用户可见文案分开存（2026-10-31 用户要求：上游返回的错误不能直接返回给前端用户）。
-- 起因：error_message 此前直接存上游 / Provider 层的原话（如整段 `{"error":{"code":"upstream_text_reply",...}}` JSON、
--   中转的「预扣费额度失败, 用户剩余额度」—— 那是**我们自己的上游账户**余额），任务状态、积分流水原因、站内通知、
--   Agent / MCP、脚本 / 智能剪辑 / 导演台的调用记录都原样带给了用户。现在 error_message 只存 platform/usererr 翻好的
--   中文文案，原话进 generation.error_detail，只给管理端排障与日志用。
--   历史行一并处理（读取侧就不用再为老行兜底，那样每个读取的地方都要记得做一遍，漏一处就泄漏）：
--   1. generation：上游失败（provider_rejected / provider_failed）的原话挪进 error_detail，error_message 换成文案；
--   2. 从它复制出去的副本（退款流水原因、组子项、Agent 步骤）改成同一句文案；
--   3. 同步调用的记录（脚本 / 智能剪辑 / 导演台、Agent 轮次与步骤、提示词优化、整图节点）里**带上游痕迹**的行
--      改成文案。这些表没有 detail 列，原话不再保留在库里（provider_request_log 仍有近 30 天的原始请求）。
--   ⚠️ 分类口径与 platform/usererr.FromUpstream 一致，改一边要改另一边。
-- +goose Up
alter table generation add column error_detail text;

-- +goose StatementBegin
create function pg_temp.usererr(raw text) returns text language sql immutable as $$
  select case
    when raw is null or raw = '' then '生成失败，请稍后重试'
    when lower(raw) ~ '(内容政策|内容安全|内容审核|content_policy|content policy|safety system|safety_violation|moderation|nsfw|违反|敏感)'
      then '内容审核未通过，请调整描述、换一个选项或换一张图后重试'
    when lower(raw) ~ '(timeout|deadline|超时)' then '模型响应超时，请稍后重试'
    when lower(raw) ~ '(connection refused|no such host|connection reset|eof|连接)' then '连接模型服务失败，请稍后重试'
    when raw ~ '(没有返回|未返回|未解析出产物)' or lower(raw) ~ '(no output|no image)' then '模型没有返回结果，请重试'
    when raw ~* 'http[^0-9]{0,6}429' then '模型服务繁忙，请稍后重试'
    when raw ~* 'http[^0-9]{0,6}(401|402|403|404)' then '模型服务暂不可用，请联系运营处理'
    when raw ~* 'http[^0-9]{0,6}5[0-9][0-9]' then '模型服务暂时不可用，请稍后重试'
    when raw ~* 'http[^0-9]{0,6}4[0-9][0-9]' then '模型未能处理这次请求，请调整描述或参数后重试'
    else '生成失败，请稍后重试'
  end
$$;
-- +goose StatementEnd

-- 1. generation
update generation
set error_detail = error_message, error_message = pg_temp.usererr(error_message)
where error_code in ('provider_rejected', 'provider_failed') and error_detail is null and error_message is not null;

-- 2. 从 generation 复制出去的副本
update credit_transaction ct set reason = g.error_message
from generation g
where ct.ref_type = 'generation' and ct.ref_id = g.id::text and ct.type = 'refund'
  and g.error_detail is not null and ct.reason is distinct from g.error_message;

update group_item gi set error_message = g.error_message
from generation g
where gi.generation_id = g.id and g.error_detail is not null and gi.error_message is not null
  and gi.error_message is distinct from g.error_message;

update agent_step s set error_message = g.error_message
from generation g
where s.generation_id = g.id and g.error_detail is not null and s.error_message is not null
  and s.error_message is distinct from g.error_message;

-- 3. 带上游痕迹的同步调用记录（上游报文、HTTP 状态、请求地址、Go 网络层原话）
update script_llm_call set error_message = pg_temp.usererr(error_message)
where error_message ~* '(上游|http|\{|context canceled|deadline exceeded|dial tcp|取模型配置|密钥|provider )';
update smartcut_llm_call set error_message = pg_temp.usererr(error_message)
where error_message ~* '(上游|http|\{|context canceled|deadline exceeded|dial tcp|取模型配置|密钥|provider )';
update director_llm_call set error_message = pg_temp.usererr(error_message)
where error_message ~* '(上游|http|\{|context canceled|deadline exceeded|dial tcp|取模型配置|密钥|provider )';
update agent_turn set error_message = pg_temp.usererr(error_message)
where error_message ~* '(上游|http|\{|context canceled|deadline exceeded|dial tcp|取模型配置|密钥|provider )';
update agent_step set error_message = pg_temp.usererr(error_message)
where error_message ~* '(上游|http|\{|context canceled|deadline exceeded|dial tcp|取模型配置|密钥|provider )';
update agent_prompt_optimize set error_message = pg_temp.usererr(error_message)
where error_message ~* '(上游|http|\{|context canceled|deadline exceeded|dial tcp|取模型配置|密钥|provider )';
update workflow_node_run set error_message = pg_temp.usererr(error_message)
where error_message ~* '(上游|http|\{|context canceled|deadline exceeded|dial tcp|取模型配置|密钥|provider )';
-- 同步调用失败的退款流水（脚本 / Agent 轮次等，ref_type 不是 generation）
update credit_transaction set reason = pg_temp.usererr(reason)
where type = 'refund' and reason ~* '(上游|http|\{|context canceled|deadline exceeded|dial tcp|取模型配置|密钥|provider )';

-- +goose Down
-- ⚠️ 只还原 generation（原话存在 error_detail 里）；副本与同步调用记录的原话没有保留，回退不了 ——
-- 它们只是给用户看的说明文字，回退后显示文案而不是原话不影响任何逻辑。
update generation set error_message = error_detail where error_detail is not null;
alter table generation drop column if exists error_detail;
