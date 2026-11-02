import { describe, expect, it } from 'vitest'

import {
  settingsAgentCapableAllowed, settingsCapabilityPayload, settingsLooksMasked, settingsModelDraft, settingsModelPayload, settingsProviderDraft, settingsProviderPayload, settingsValidateCapability,
  settingsValidateModel, settingsValidateProvider, SETTINGS_VOLC_REGION_HEADER, SETTINGS_VOLC_SERVICE_HEADER,
  type SettingsProtocol,
  settingsEnableSummary, settingsIsPrivateAddress, settingsParamSupportDraft, settingsParamSupportPayload,
  settingsTemplateAuthHint, settingsTemplateState, settingsValidateParamSupport, type SettingsTemplate
} from './settingsModelForm'

const protocols: SettingsProtocol[] = [
  { code: 'openai_images', name: 'OpenAI 图片', isAsync: false, requiresQueryPath: false },
  { code: 'kling_video', name: '可灵视频', isAsync: true, requiresQueryPath: true },
  { code: 'openai_chat', name: 'OpenAI Chat', isAsync: false, requiresQueryPath: false, chatCapable: true },
  { code: 'generic_sync', name: '通用同步 HTTP', isAsync: false, requiresQueryPath: false, chatCapable: false },
]
const row = {
  code: 'gpt_image_2', name: '图片', providerCode: 'p1', domain: 'image', subType: '', upstreamModelCode: 'gpt-image-2',
  apiProtocol: 'openai_images', submitPath: '/images/generations', queryPath: '', estSeconds: 40, maxInputChars: 4000,
  sort: 20, status: 'enabled', minMemberLevelId: '777', operations: [],
}

describe('模型表单', () => {
  it('从列表行还原、原样保存不丢字段（保存是整条替换）', () => {
    expect(settingsModelPayload(settingsModelDraft(row))).toEqual({
      code: 'gpt_image_2', name: '图片', providerCode: 'p1', domain: 'image', subType: '', upstreamModelCode: 'gpt-image-2',
      apiProtocol: 'openai_images', submitPath: '/images/generations', queryPath: '', estSeconds: 40, maxInputChars: 4000,
      sort: 20, status: 'enabled', agentCapable: false,
    })
  })
  it('异步协议必须有查询路径，同步协议不能有', () => {
    const d = { ...settingsModelDraft(null), code: 'k3', name: '可灵', providerCode: 'p1', domain: 'video', upstreamModelCode: 'kling-v3', submitPath: '/v1/videos' }
    expect(settingsValidateModel({ ...d, apiProtocol: 'kling_video' }, [], protocols, false)).toMatch(/查询路径/)
    expect(settingsValidateModel({ ...d, apiProtocol: 'kling_video', queryPath: '/v1/videos/{taskId}' }, [], protocols, false)).toBe('')
    expect(settingsValidateModel({ ...d, apiProtocol: 'openai_images', queryPath: '/x' }, [], protocols, false)).toMatch(/同步协议/)
  })
  it('编码格式、重复、预计耗时范围', () => {
    const d = { ...settingsModelDraft(row), code: 'new_one' }
    expect(settingsValidateModel({ ...d, code: 'Bad' }, [row], protocols, false)).toMatch(/小写字母/)
    expect(settingsValidateModel({ ...d, code: 'gpt_image_2' }, [row], protocols, false)).toMatch(/已存在/)
    expect(settingsValidateModel({ ...d, estSeconds: 0 }, [row], protocols, false)).toMatch(/预计耗时/)
    expect(settingsValidateModel({ ...d, maxInputChars: 0 }, [row], protocols, false)).toMatch(/正整数/)
    expect(settingsValidateModel(d, [row], protocols, false)).toBe('')
  })

  // 视频能力三项（AGT-05）
  const vrow = {
    ...row, code: 'kling_3', domain: 'video', apiProtocol: 'kling_video', queryPath: '/v1/videos/{taskId}',
    maxDurationSec: 10, isMultiShot: true, hasNativeAudio: true,
  }
  it('视频能力字段回填并原样保存', () => {
    const d = settingsModelDraft(vrow)
    expect([d.maxDurationSec, d.isMultiShot, d.hasNativeAudio]).toEqual([10, true, true])
    expect(settingsModelPayload(d)).toMatchObject({ maxDurationSec: 10, isMultiShot: true, hasNativeAudio: true })
    // 老数据没有这几项：回填成未配置 / false
    const old = settingsModelDraft({ ...vrow, maxDurationSec: null, isMultiShot: undefined, hasNativeAudio: undefined })
    expect([old.maxDurationSec, old.isMultiShot, old.hasNativeAudio]).toEqual([null, false, false])
  })
  // ⚠️ 清空时长要显式带 null（= 清除）；不带的话服务端按「保留」处理，用户清空输入框也清不掉
  it('视频模型清空时长时显式提交 null', () => {
    const body = settingsModelPayload({ ...settingsModelDraft(vrow), maxDurationSec: null })
    expect(body).toHaveProperty('maxDurationSec', null)
  })
  it('切到非视频域时不提交视频能力字段', () => {
    const body = settingsModelPayload({ ...settingsModelDraft(vrow), domain: 'image', apiProtocol: 'openai_images', queryPath: '' })
    for (const k of ['maxDurationSec', 'isMultiShot', 'hasNativeAudio']) expect(body).not.toHaveProperty(k)
    expect(settingsModelPayload(settingsModelDraft(row))).not.toHaveProperty('maxDurationSec')
  })
  it('单次最长时长：1~600 的整数或留空', () => {
    const d = settingsModelDraft(vrow)
    expect(settingsValidateModel(d, [], protocols, true)).toBe('')
    expect(settingsValidateModel({ ...d, maxDurationSec: null }, [], protocols, true)).toBe('')
    expect(settingsValidateModel({ ...d, maxDurationSec: 600 }, [], protocols, true)).toBe('')
    for (const bad of [0, -1, 601, 2.5]) expect(settingsValidateModel({ ...d, maxDurationSec: bad }, [], protocols, true)).toMatch(/单次最长时长/)
    // 非视频模型不校验（不会提交）
    expect(settingsValidateModel({ ...settingsModelDraft(row), maxDurationSec: 0 }, [], protocols, true)).toBe('')
  })

  // 支持 Agent（AGT-04）
  const trow = {
    ...row, code: 'gpt_5', domain: 'text', apiProtocol: 'openai_chat', submitPath: '/chat/completions', agentCapable: true,
  }
  // ⚠️ 保存是整条替换：回填丢了这一项的话，改个名字保存一次推理模型就从 Agent 下拉里消失
  it('从列表行还原、原样保存不丢 agentCapable', () => {
    const d = settingsModelDraft(trow)
    expect(d.agentCapable).toBe(true)
    expect(settingsModelPayload(d)).toHaveProperty('agentCapable', true)
    expect(settingsModelDraft({ ...trow, agentCapable: undefined }).agentCapable).toBe(false)
  })
  it('非文本模型一律提交 false（改类型时连带取消标记）', () => {
    const body = settingsModelPayload({ ...settingsModelDraft(trow), domain: 'image', apiProtocol: 'openai_images' })
    expect(body).toHaveProperty('agentCapable', false)
  })
  it('只有文本模型且协议能多轮 + 工具调用时可勾', () => {
    const d = settingsModelDraft(trow)
    expect(settingsAgentCapableAllowed(d, protocols)).toBe(true)
    expect(settingsAgentCapableAllowed({ ...d, apiProtocol: 'generic_sync' }, protocols)).toBe(false)
    expect(settingsAgentCapableAllowed({ ...d, domain: 'image' }, protocols)).toBe(false)
    expect(settingsValidateModel(d, [], protocols, true)).toBe('')
    expect(settingsValidateModel({ ...d, apiProtocol: 'generic_sync' }, [], protocols, true)).toMatch(/不能作推理模型/)
  })
})

const prow = {
  code: 'kling_official', name: '可灵', adapterType: 'configurable_http', baseUrl: 'https://api.klingai.com', authType: 'bearer',
  hasApiKey: true, headers: { 'X-Trace': 'on' }, testPath: '', connectTimeoutMs: 10000, requestTimeoutMs: 120000,
  idleTimeoutMs: 60000, maxRetries: 2, maxConcurrent: 0, status: 'enabled', isReady: true, version: 3,
}

describe('供应商表单', () => {
  it('编辑时密钥默认「保留」且不带进请求体（带 keep 会被审计记成改过密钥）', () => {
    const d = settingsProviderDraft(prow)
    expect(d.keyAction).toBe('keep')
    expect(d.keyValue).toBe('')
    const body = settingsProviderPayload(d)
    expect(body).not.toHaveProperty('apiKey')
    expect(body).toMatchObject({ version: 3, connectTimeoutMs: 10000, requestTimeoutMs: 120000, idleTimeoutMs: 60000, headers: { 'X-Trace': 'on' } })
  })
  it('替换要填真实值，掩码被拒；清除只带 action', () => {
    const d = settingsProviderDraft(prow)
    expect(settingsValidateProvider({ ...d, keyAction: 'replace' }, [prow], true)).toMatch(/新的 API Key/)
    expect(settingsValidateProvider({ ...d, keyAction: 'replace', keyValue: 'sk-****abcd' }, [prow], true)).toMatch(/掩码/)
    expect(settingsProviderPayload({ ...d, keyAction: 'replace', keyValue: ' sk-real ' }).apiKey).toEqual({ action: 'replace', value: 'sk-real' })
    expect(settingsProviderPayload({ ...d, keyAction: 'clear', status: 'disabled' }).apiKey).toEqual({ action: 'clear' })
  })
  it('需要认证却没有密钥时不能启用', () => {
    const d = settingsProviderDraft(prow)
    expect(settingsValidateProvider({ ...d, keyAction: 'clear' }, [prow], true)).toMatch(/启用前必须配置 API Key/)
    expect(settingsValidateProvider({ ...d, keyAction: 'clear', authType: 'none' }, [prow], true)).toBe('')
  })
  it('超时：都要大于 0，空闲不大于整体', () => {
    const d = settingsProviderDraft(prow)
    expect(settingsValidateProvider({ ...d, idleTimeoutSec: 200 }, [prow], true)).toMatch(/空闲超时/)
    expect(settingsValidateProvider({ ...d, connectTimeoutSec: 0 }, [prow], true)).toMatch(/大于 0/)
  })
  it('请求头重名（不区分大小写）与空名拒掉；地址必须是 http(s)', () => {
    const d = settingsProviderDraft(prow)
    expect(settingsValidateProvider({ ...d, headers: [{ name: 'X-A', value: '1' }, { name: 'x-a', value: '2' }] }, [prow], true)).toMatch(/重名/)
    expect(settingsValidateProvider({ ...d, headers: [{ name: ' ', value: '1' }] }, [prow], true)).toMatch(/名称不能为空/)
    expect(settingsValidateProvider({ ...d, baseUrl: 'api.x.com' }, [prow], true)).toMatch(/http/)
  })
  it('新建时可以先不配密钥（停用状态）', () => {
    const d = { ...settingsProviderDraft(null), code: 'new_p', name: '新', baseUrl: 'https://x.example.com' }
    expect(settingsValidateProvider(d, [prow], false)).toBe('')
    expect(settingsProviderPayload(d)).not.toHaveProperty('apiKey')
  })
  it('掩码判定', () => {
    expect(settingsLooksMasked('sk-****1234')).toBe(true)
    expect(settingsLooksMasked('sk-live-abc')).toBe(false)
  })
})

// 火山 V4 签名（docs/agent/smartcut SCT-32，SCD-38）：AK / SK 两个输入框拼成一个密钥、仍是三态；地域与服务单独填，
// 存在请求头的约定键里。
describe('供应商表单：火山 V4 签名', () => {
  const vrow = {
    ...prow, code: 'volc_music', authType: 'volc_v4', hasApiKey: true,
    headers: { 'x-volc-region': 'cn-north-1', 'X-Volc-Service': 'imagination', 'X-Trace': 'on' },
  }
  it('回显：地域与服务进各自的输入框、不留在请求头列表里；两段密钥都不回填', () => {
    const d = settingsProviderDraft(vrow)
    expect(d.authType).toBe('volc_v4')
    expect([d.volcRegion, d.volcService]).toEqual(['cn-north-1', 'imagination'])
    expect(d.headers).toEqual([{ name: 'X-Trace', value: 'on' }])
    expect([d.keyAction, d.keyAk, d.keySk, d.keyValue]).toEqual(['keep', '', '', ''])
  })
  it('保留：不带密钥字段；地域与服务写回约定键', () => {
    const body = settingsProviderPayload(settingsProviderDraft(vrow))
    expect(body).not.toHaveProperty('apiKey')
    expect(body.headers).toEqual({ 'X-Trace': 'on', [SETTINGS_VOLC_REGION_HEADER]: 'cn-north-1', [SETTINGS_VOLC_SERVICE_HEADER]: 'imagination' })
  })
  it('替换：两段拼成 AK:SK；只填一半、AK 带冒号、像掩码都拒', () => {
    const d = { ...settingsProviderDraft(vrow), keyAction: 'replace' as const }
    expect(settingsValidateProvider(d, [vrow], true)).toMatch(/AccessKey 与 SecretKey/)
    expect(settingsValidateProvider({ ...d, keyAk: 'AKLT1' }, [vrow], true)).toMatch(/AccessKey 与 SecretKey/)
    expect(settingsValidateProvider({ ...d, keyAk: 'AK:x', keySk: 'SK' }, [vrow], true)).toMatch(/冒号/)
    expect(settingsValidateProvider({ ...d, keyAk: 'AKLT1', keySk: 'ab****cd' }, [vrow], true)).toMatch(/掩码/)
    const ok = { ...d, keyAk: ' AKLT1 ', keySk: ' c2Vj:cmV0 ' }
    expect(settingsValidateProvider(ok, [vrow], true)).toBe('')
    expect(settingsProviderPayload(ok).apiKey).toEqual({ action: 'replace', value: 'AKLT1:c2Vj:cmV0' })
  })
  it('地域与服务必填；不能手写进自定义请求头', () => {
    const d = settingsProviderDraft(vrow)
    expect(settingsValidateProvider({ ...d, volcService: ' ' }, [vrow], true)).toMatch(/地域与服务/)
    expect(settingsValidateProvider({ ...d, headers: [{ name: 'X-VOLC-REGION', value: 'x' }] }, [vrow], true)).toMatch(/别写进自定义请求头/)
  })
  it('换成别的认证方式：不再带地域与服务', () => {
    const body = settingsProviderPayload({ ...settingsProviderDraft(vrow), authType: 'bearer' })
    expect(body.headers).toEqual({ 'X-Trace': 'on' })
  })
  it('清除之后不能启用（需要认证却没有密钥）', () => {
    expect(settingsValidateProvider({ ...settingsProviderDraft(vrow), keyAction: 'clear' }, [vrow], true)).toMatch(/启用前必须配置/)
  })
})

describe('能力矩阵', () => {
  const ok = { role: 'image', minCount: 1, maxCount: 4, allowedMime: ['image/png'] }
  it('合法的一条通过，请求体去掉空白', () => {
    expect(settingsValidateCapability(ok, [], false)).toBe('')
    expect(settingsCapabilityPayload('img2img', { ...ok, allowedMime: [' image/png ', ''] }))
      .toEqual({ operation: 'img2img', role: 'image', minCount: 1, maxCount: 4, allowedMime: ['image/png'] })
  })
  it('最多小于最少、全 0、重复角色、MIME 格式不对都拒', () => {
    expect(settingsValidateCapability({ ...ok, minCount: 3, maxCount: 2 }, [], false)).toMatch(/不能小于/)
    expect(settingsValidateCapability({ ...ok, minCount: 0, maxCount: 0 }, [], false)).toMatch(/没有意义/)
    expect(settingsValidateCapability(ok, [{ role: 'image' }], false)).toMatch(/已经配过/)
    expect(settingsValidateCapability(ok, [{ role: 'image' }], true)).toBe('')
    expect(settingsValidateCapability({ ...ok, allowedMime: ['png'] }, [], false)).toMatch(/MIME/)
  })
})

// ---------------------------------------------------------------- tabtv 新增（TT-T-18）


describe('桌面版：没有会员门槛', () => {
  it('模型请求体不带 minMemberLevelId（服务端按保留处理）', () => {
    expect(settingsModelPayload(settingsModelDraft(row))).not.toHaveProperty('minMemberLevelId')
  })
})

describe('局域网地址提示', () => {
  it('私有 / 回环 / .local 判为局域网，公网域名不是', () => {
    for (const u of ['http://192.168.1.5:3000/v1', 'http://10.0.0.2', 'http://172.20.1.1', 'http://127.0.0.1:8080',
      'http://localhost:11434', 'http://gpu-box.local/v1', 'http://[::1]:8000', 'http://100.88.1.2']) {
      expect(settingsIsPrivateAddress(u), u).toBe(true)
    }
    for (const u of ['https://api.openai.com/v1', 'http://172.32.0.1', 'https://8.8.8.8', 'not a url']) {
      expect(settingsIsPrivateAddress(u), u).toBe(false)
    }
  })
})

describe('参数支持', () => {
  it('回填与请求体：可选值逗号分隔、空覆盖给 null', () => {
    const d = settingsParamSupportDraft({ paramCode: 'duration', allowedValues: ['4', '15'], defaultValue: '4', override: null })
    expect(d).toEqual({ paramCode: 'duration', allowedText: '4, 15', defaultValue: '4', overrideText: '' })
    expect(settingsParamSupportPayload({ ...d, allowedText: '5，10\n15' }))
      .toEqual({ paramCode: 'duration', allowedValues: ['5', '10', '15'], defaultValue: '4', override: null })
  })
  it('校验：参数必选、全空、覆盖不是对象、默认值不在可选项里', () => {
    const base = { paramCode: 'duration', allowedText: '5, 10', defaultValue: '5', overrideText: '' }
    expect(settingsValidateParamSupport(base)).toBe('')
    expect(settingsValidateParamSupport({ ...base, paramCode: '' })).toMatch(/请选择参数/)
    expect(settingsValidateParamSupport({ paramCode: 'd', allowedText: '', defaultValue: '', overrideText: '' })).toMatch(/至少填一项/)
    expect(settingsValidateParamSupport({ ...base, overrideText: '[1]' })).toMatch(/JSON 对象/)
    expect(settingsValidateParamSupport({ ...base, overrideText: '{bad' })).toMatch(/不是合法的 JSON/)
    expect(settingsValidateParamSupport({ ...base, defaultValue: '8' })).toMatch(/不在可选项里/)
    // 覆盖里给了 options 时以它为准
    expect(settingsValidateParamSupport({ ...base, allowedText: '', defaultValue: '1080p',
      overrideText: '{"options":[{"label":"1080P","value":"1080p"}]}' })).toBe('')
  })
})

describe('模板卡片', () => {
  const t = (over: Partial<SettingsTemplate>): SettingsTemplate => ({
    code: 'openai', name: 'OpenAI', baseUrl: 'https://api.openai.com/v1', authType: 'bearer', installed: true,
    status: 'disabled', hasApiKey: false, isReady: false, version: 1,
    models: [{ code: 'a', name: 'A', domain: 'text', installed: true, status: 'disabled' },
      { code: 'b', name: 'B', domain: 'image', installed: true, status: 'disabled' }], ...over,
  })
  it('状态：未填 Key / 已填未启用 / 部分启用 / 全部启用 / 未安装', () => {
    expect(settingsTemplateState(t({})).label).toBe('未填 Key')
    expect(settingsTemplateState(t({ hasApiKey: true })).label).toMatch(/已填 Key/)
    const half = t({ status: 'enabled', hasApiKey: true })
    half.models[0].status = 'enabled'
    expect(settingsTemplateState(half).label).toBe('已启用 1/2')
    expect(settingsTemplateState(t({ status: 'enabled', models: [{ code: 'a', name: 'A', domain: 'text', installed: true, status: 'enabled' }] })).label).toBe('已启用')
    expect(settingsTemplateState(t({ installed: false })).type).toBe('danger')
  })
  it('Key 的说明按认证方式给', () => {
    expect(settingsTemplateAuthHint({ authType: 'header', headerName: 'x-api-key' })).toMatch(/x-api-key/)
    expect(settingsTemplateAuthHint({ authType: 'bearer' })).toMatch(/Bearer/)
  })
  it('启用结果：部分失败时点名失败的模型与原因', () => {
    expect(settingsEnableSummary([{ code: 'a', name: 'A', enabled: true }])).toEqual({ text: '已启用 1 个模型', ok: true })
    const r = settingsEnableSummary([{ code: 'a', name: 'A', enabled: true }, { code: 'b', name: 'B', enabled: false, reason: '无法启用：没有价格' }])
    expect(r.ok).toBe(false)
    expect(r.text).toMatch(/B：无法启用：没有价格/)
  })
})
