import type { components } from "@tabtv/api-client";
import { QUOTE_VIDEO_SECONDS, modelUnavailableReason, type ModelOption } from "~/utils/composerPrice";
import { mergePrefs, type AgentPrefsValue, type PrefKind } from "~/utils/agentPrefs";
import { modelBadges } from "~/utils/composerUi";
import { LAST_MODE_KEY, writeLastMode, type ComposerMode } from "~/utils/quickGenerate";

/**
 * 用户级 Agent 偏好与模型目录（AGT-27 / AGT-29）。
 *
 * - `prefs`：`GET/PUT /agent/preferences`（只要登录）。会话级覆盖不在这里 —— 走 `agent.patchSession`；
 *   这里的是「设为默认」写的那一份。
 * - `options(kind)`：偏好浮层三页签的模型（视频 / 图片 / 推理）。⚠️ **不可用的照样列出**，原因来自
 *   服务端能力校验（报价接口）的原话；tabtv 不计价，不显示单价（TT-T-14）。
 * - `lastMode`：创造页输入框上次的模式（本地存储，与 AGT-42 同一个键）。
 *
 * ⚠️ 只读不写：加载时只取偏好与报价，不写任何数据；写只发生在用户点「设为默认」时。
 */
type AgentPreferences = components["schemas"]["AgentPreferences"];
type AgentSkill = components["schemas"]["AgentSkill"];
type SkillScene = components["schemas"]["AgentSkillScene"];
type UsableModel = { code: string; name: string; resultType?: string; agentCapable?: boolean; minMemberLevelId?: string; hasNativeAudio?: boolean };

/**
 * 模型目录的种类：偏好浮层的三页签，外加创造页直接生成的音频（直接生成复用同一个浮层与同一份目录，
 * 视频 / 图片两页与 Agent 模式共用缓存）。
 */
export type OptionKind = PrefKind | "audio";

const COMPOSER_MODES: readonly ComposerMode[] = ["agent", "video", "image", "audio"];

/** 每种偏好用哪个操作做一次可用性报价（报价接口与受理同一条能力校验路径）。 */

const QUOTE_OF: Record<OptionKind, { resultType?: string; operation: string; extra: Record<string, unknown> }> = {
  // ⚠️ 视频按 5 秒报价：报价入口会校验时长范围（SCT-39 的 DurationDeclaring），
  //    报 1 秒的话每个视频模型都回「时长必须在 4 到 30 秒之间（当前 1）」，整页模型全被标成不可用（2026-10-30 用户反馈）。
  video: { resultType: "video", operation: "text2video", extra: { durationSec: QUOTE_VIDEO_SECONDS, count: 1 } },
  image: { resultType: "image", operation: "text2img", extra: { count: 1 } },
  reasoning: { operation: "agent_chat", extra: { count: 1 } },
  audio: { resultType: "audio", operation: "text2audio", extra: { count: 1 } }
};

export const useAgentPrefsStore = defineStore("agentPrefs", () => {
  const prefs = ref<AgentPrefsValue>({});
  const loaded = ref(false);
  const saving = ref(false);
  const optionsByKind = ref<Partial<Record<OptionKind, ModelOption[]>>>({});
  const optionErrors = ref<Partial<Record<OptionKind, string>>>({});
  const lastMode = ref<ComposerMode>(readMode());
  let loading: Promise<void> | null = null;
  const optionLoads = new Map<OptionKind, Promise<ModelOption[]>>();

  function readMode(): ComposerMode {
    if (typeof localStorage === "undefined") return "agent";
    try {
      const value = localStorage.getItem(LAST_MODE_KEY);
      return COMPOSER_MODES.includes(value as ComposerMode) ? value as ComposerMode : "agent";
    } catch {
      return "agent";
    }
  }

  function setLastMode(mode: ComposerMode) {
    lastMode.value = mode;
    if (typeof localStorage !== "undefined") writeLastMode(localStorage, mode);
  }

  /** 取用户偏好（单飞；已取到就不再取，`force` 重取）。失败时保留已有值、不抛。 */
  async function load(force = false): Promise<void> {
    if (loaded.value && !force) return;
    if (loading) return loading;
    loading = (async () => {
      try {
        const response = await useApi()<AgentPreferences>("/agent/preferences");
        prefs.value = response.prefs ?? {};
        loaded.value = true;
      } catch {
        // 取不到偏好：按「全部智能匹配」处理，不让输入框报错。
      } finally {
        loading = null;
      }
    })();
    return loading;
  }

  /**
   * 设为默认：把改动**按节合并**进用户偏好后整份写回（保留不认识的键）。
   * 失败抛出，由调用处提示。
   */
  async function saveDefault(patch: AgentPrefsValue): Promise<AgentPrefsValue> {
    await load();
    const next = mergePrefs(prefs.value, patch);
    saving.value = true;
    try {
      const response = await useApi()<AgentPreferences>("/agent/preferences", { method: "PUT", body: { prefs: next } });
      prefs.value = response.prefs ?? next;
      return prefs.value;
    } finally {
      saving.value = false;
    }
  }

  async function quoteOne(kind: OptionKind, model: UsableModel): Promise<ModelOption> {
    const spec = QUOTE_OF[kind];
    try {
      await useApi()<unknown>("/generations/estimate", {
        method: "POST",
        body: { modelCode: model.code, operation: spec.operation, ...spec.extra }
      });
      return { code: model.code, name: model.name || model.code, available: true, badges: modelBadges(model), hasNativeAudio: model.hasNativeAudio };
    } catch (cause) {
      return { code: model.code, name: model.name || model.code, available: false, reason: modelUnavailableReason(cause), badges: modelBadges(model), hasNativeAudio: model.hasNativeAudio };
    }
  }

  /**
   * 某一页签的模型（含不可用的）。单飞并缓存；`force` 重取（改了模型配置回来后）。
   * ⚠️ 推理模型只列 `agentCapable`（服务端按「支持 Agent」且绑定了 `agent_chat` 过滤）。
   */
  async function loadOptions(kind: OptionKind, force = false): Promise<ModelOption[]> {
    // ⚠️ 只缓存「至少有一个可用」的结果：空列表或全不可用多半是后台还没配好，
    // 缓存住的话运营配完之后输入框仍一直说「还没有可用的推理模型」，只能刷新页面（2026-10-16 实测）。
    const cached = optionsByKind.value[kind];
    if (!force && cached?.some(option => option.available)) return cached;
    const inflight = optionLoads.get(kind);
    if (inflight) return inflight;
    const run = (async () => {
      try {
        const spec = QUOTE_OF[kind];
        const query = kind === "reasoning" ? { agentCapable: true } : { resultType: spec.resultType };
        const models = await useApi()<UsableModel[]>("/models", { query });
        const list = (models ?? []).filter(model => kind !== "reasoning" || model.agentCapable !== false);
        const options = await Promise.all(list.map(model => quoteOne(kind, model)));
        optionsByKind.value = { ...optionsByKind.value, [kind]: options };
        optionErrors.value = { ...optionErrors.value, [kind]: undefined };
        return options;
      } catch (cause) {
        optionErrors.value = { ...optionErrors.value, [kind]: modelUnavailableReason(cause) };
        return optionsByKind.value[kind] ?? [];
      } finally {
        optionLoads.delete(kind);
      }
    })();
    optionLoads.set(kind, run);
    return run;
  }

  const skills = ref<AgentSkill[] | null>(null);
  let skillsLoading: Promise<AgentSkill[]> | null = null;

  const sceneSkills = ref<Partial<Record<SkillScene, AgentSkill[]>>>({});
  const sceneLoads = new Map<SkillScene, Promise<AgentSkill[]>>();

  /**
   * 技能目录（`GET /agent/skills`，单飞并缓存；失败抛出、下次重取）。
   *
   * `scene` 不传 = 原来那条请求（不带参数，服务端按 general），创造页走这里、一个字节都不变；
   * 传了（营销页传 `marketing`）走按场景分开的缓存 —— 共用一份缓存的话，先开过营销页再回创造页，
   * 技能弹层会列出仅属营销的技能（MKT-AC-15）。
   */
  async function loadSkills(force = false, scene?: SkillScene): Promise<AgentSkill[]> {
    if (scene) return loadSceneSkills(scene, force);
    if (skills.value && !force) return skills.value;
    skillsLoading ??= useApi()<{ items: AgentSkill[] }>("/agent/skills")
      .then(response => {
        skills.value = response.items ?? [];
        return skills.value;
      })
      .finally(() => {
        skillsLoading = null;
      });
    return skillsLoading;
  }

  async function loadSceneSkills(scene: SkillScene, force: boolean): Promise<AgentSkill[]> {
    const cached = sceneSkills.value[scene];
    if (cached && !force) return cached;
    const inflight = sceneLoads.get(scene);
    if (inflight) return inflight;
    const run = useApi()<{ items: AgentSkill[] }>("/agent/skills", { query: { scene } })
      .then((response) => {
        const items = response.items ?? [];
        sceneSkills.value = { ...sceneSkills.value, [scene]: items };
        return items;
      })
      .finally(() => {
        sceneLoads.delete(scene);
      });
    sceneLoads.set(scene, run);
    return run;
  }

  function options(kind: OptionKind): ModelOption[] {
    return optionsByKind.value[kind] ?? [];
  }

  function option(kind: OptionKind, code: string | undefined): ModelOption | undefined {
    return code ? options(kind).find(item => item.code === code) : undefined;
  }

  return {
    prefs,
    loaded,
    saving,
    optionsByKind,
    optionErrors,
    lastMode,
    load,
    saveDefault,
    loadOptions,
    options,
    option,
    skills,
    /** 按场景缓存的技能目录（只读；营销输入框据此读 `isBatch`，MKT-T-32）。 */
    sceneSkills,
    loadSkills,
    setLastMode
  };
});
