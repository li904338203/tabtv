import type { components } from '~/types/legacy';
import type { components as ApiComponents } from "@tabtv/api-client";
import { asObject } from "~/utils/canvas";

/**
 * ⚠️ `/canvas/config` 的类型取自**生成的新契约**，不是 `types/legacy`（老 Java 契约的抄本）。
 *
 * 两份类型的差异不是命名风格，是**实打实的字段名**：
 *   参数分区 `scope` → **`group`**（`main`/`format`/`advanced`）
 *   预设列表 `presets` → **`imagePresets`**
 *   视频模式 `[{label,value}]` → **纯字符串数组**
 *   节点种类 `generate`/`group` → **`generator`/`composite`**
 * 而读错字段**一律不报错**，只是渲染成空 —— 这正是画布参数面板"界面错乱"的成因。
 */
type CanvasConfig = ApiComponents["schemas"]["CanvasConfig"];
type NodeType = ApiComponents["schemas"]["NodeTypeConfig"];
type ModelSummary = components["schemas"]["ModelSummaryResp"];
type ModelParams = ApiComponents["schemas"]["ModelParams"];

export const useCanvasConfigStore = defineStore("canvas-config", () => {
  const config = ref<CanvasConfig | null>(null);
  const modelLists = ref<Record<string, ModelSummary[]>>({});
  const modelOverrides = ref<Record<string, Record<string, unknown>>>({});
  const loading = ref(false);

  async function load() {
    if (config.value || loading.value) return;
    loading.value = true;
    try {
      config.value = await useApi()<CanvasConfig>("/canvas/config");
      // ⚠️ 新后端的 `/canvas/config` **不内嵌模型清单**（老后端有 `models`）——
      //    模型走独立的 `/models?resultType=`，按产出类型分别取（大列表懒加载，
      //    见 param.config.domainFrom）。这里留空，由 `models()` 按需填。
      //    照老结构读不会报错，只是 `modelLists` 恒为空 —— 而那表现为
      //    "模型下拉一直转圈"，看起来像接口慢。
      modelLists.value = {};
    } finally {
      loading.value = false;
    }
  }

  function nodeType(code: string): NodeType | undefined {
    return config.value?.nodeTypes?.find(item => item.code === code);
  }

  async function models(domain: string, operation?: string) {
    const key = operation ? `${domain}:${operation}` : domain;
    if (!modelLists.value[key]) {
      // ⚠️ 新后端的参数是 **`resultType`** 不是 `domain`（老后端用 domain）。
      //    参数名不对**不会报错** —— 服务端忽略它，返回**全量模型**，
      //    于是图片节点的下拉里会列出视频与文本模型，用户选了才发现不对。
      //    （新后端刻意用 resultType：筛的是"产出什么"，而一个模型可以绑
      //    多个操作、跨域产出 —— 按 domain 筛会把它从该出现的地方漏掉。）
      const query = new URLSearchParams({ resultType: domain });
      if (operation) query.set("operation", operation);
      modelLists.value[key] = await useApi()<ModelSummary[]>(`/models?${query.toString()}`);
    }
    return modelLists.value[key] ?? [];
  }

  /**
   * 模型级参数覆盖（参数合并的第三层：
   * `param_def.config < node_param.config_override < model_param_support.override`）。
   *
   * 清晰度 / 时长 / 画幅能选什么由**型号**决定（Seedance 2.5 是 4~30 秒、480p~1080p，
   * 2.0 Fast 只有 720p），不是节点通用的那一套 —— 选定模型后才取，按模型缓存一次。
   *
   * ⚠️ 这条端点（`GET /models/{code}/params`）直到 P15-01 才补上：此前一直 404，下面的 catch
   *    把它当成「没有覆盖」吞掉，面板永远只有通用选项，选了型号不支持的值要到上游才报错
   *    （那时已经扣过费）。现在取不到仍**退回空覆盖**而不报错 —— 参数面板打不开的话这个节点
   *    就完全没法用了 —— 但这应当只是网络抖动，所以**失败不缓存**，下次选模型时再取。
   * ⚠️ 成功的结果只缓存在本页：后台改了型号覆盖，已打开的画布要刷新一次才能拿到。
   */
  async function overrides(modelCode: string) {
    const cached = modelOverrides.value[modelCode];
    if (cached) return cached;
    try {
      const response = await useApi()<ModelParams>(`/models/${encodeURIComponent(modelCode)}/params`);
      const value = Object.fromEntries(
        Object.entries(response.override ?? {}).map(([key, item]) => [key, asObject(item)])
      );
      modelOverrides.value[modelCode] = value;
      return value;
    } catch {
      return {};
    }
  }

  /**
   * 素材目录（风格 / 特效）—— `param_def.config.domainFrom === "materials"` 的懒加载源。
   *
   * ⚠️ 端点回的是**裸数组**（`/canvas/styles`、`/canvas/effects`），不是分页对象。
   * ⚠️ 取不到时缓存空数组**并且不抛** —— 素材目录为空是正常状态（还没配），
   *    让参数面板因此打不开的话，整个节点就没法用了。
   */
  const materialLists = ref<Record<string, { code?: string; name?: string }[]>>({});
  async function materials(type: string) {
    const key = type === "effect" ? "effect" : "style";
    if (!materialLists.value[key]) {
      try {
        materialLists.value[key] = await useApi()<{ code?: string; name?: string }[]>(`/canvas/${key}s`);
      } catch {
        materialLists.value[key] = [];
      }
    }
    return materialLists.value[key] ?? [];
  }

  /**
   * ⚠️ 新后端的字段名是 **`imagePresets`**（老后端是 `presets`），
   * 而 `videoModes` 是**纯字符串数组**（老后端是 `[{label,value}]`）。
   * 读错名字不报错，只是下拉永远是空的 —— 看起来像"还没配预设"。
   */
  const imagePresets = computed(() => config.value?.imagePresets ?? []);
  const videoModes = computed<string[]>(() => config.value?.videoModes ?? []);

  return {
    config, loading, modelLists, materialLists, imagePresets, videoModes,
    load, nodeType, models, overrides, materials
  };
});
