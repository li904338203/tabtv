<script setup lang="ts">
import type { components } from '~/types/legacy';
import type { components as ApiComponents } from "@tabtv/api-client";
import { ArrowUp, Image as ImageIcon, LoaderCircle, Save, X } from "@lucide/vue";
import { ElMessage } from "element-plus";
import DynamicControl from "./DynamicControl.vue";
import GenerationSizeControl, { type FormatParam } from "./GenerationSizeControl.vue";
import ImagePresetChip from "./ImagePresetChip.vue";
import ImagePresetControl from "./ImagePresetControl.vue";
import VideoModeControl from "./VideoModeControl.vue";
import VideoTopTools from "./VideoTopTools.vue";
import type { GroupDetail, GroupItem, ScriptAsset, ScriptShot } from "~/types/script";
import { groupItemReferences, type GroupItemReference, type GroupResp } from "~/utils/scriptContract";
import type { JsonObject } from "~/types/canvas";
import { applyImagePreset, asObject, clearImagePreset, liftLegacySize, mergeObjects, modelReconcileRules, reconcileParams, unwrapOptionObjects } from "~/utils/canvas";
import { isVideoModeCode, nodeParamLayout, paramKey } from "~/utils/paramLayout";

// ⚠️ 同 ParamPanel：参数分区在新契约里是 `group` 不是 `scope`。
type ParamConfig = ApiComponents["schemas"]["NodeParamConfig"];
type PresetItem = components["schemas"]["PresetResp"];

const props = defineProps<{
  group: GroupDetail;
  item: GroupItem;
  title: string;
}>();

const media = useAuthenticatedMedia();
const referencePreviews = ref<Record<string, string>>({});
/** 本脚本的资产（载入后用来给参考图标上名字；也是资产组子项的提示词来源）。⚠️ 要声明在 referenceInputs 之前：下面的 watch 立即求值 */
const assets = ref<ScriptAsset[]>([]);

/**
 * 本镜实际发给出图模型的参考图 —— 和单独的图片节点一样，一镜带着它要用的那几张资产设定图（CD-82）。
 *
 * <p>一镜常关联五到八个资产，到底哪几张进了画面、谁是「图1」，只有 inputRefs 说了算。
 * 不展示的话用户无从判断「为什么这一镜的角色不像」，更会以为这一镜根本没有关联资源。
 * ⚠️ 解析统一走 `groupItemReferences`：新后端的输入只带 ID（`kind` + `referenceId`/`generationId`），
 *    此前这里只认老形状 `type:"image"`，一张都解析不出来，参考图条一直是空的。
 */
const referenceInputs = computed<GroupItemReference[]>(() => groupItemReferences(props.item.inputRefs, assets.value));

const ASSET_TYPE_LABELS: Record<string, string> = { character: "角色", scene: "场景", prop: "道具" };

/** 「图2 · 白骨洞（场景）」：与提示词里的写法一致，用户拿它对照提示词核对谁是谁。 */
function referenceTitle(input: GroupItemReference) {
  const type = ASSET_TYPE_LABELS[input.assetType];
  if (input.name) return `图${input.index} · ${input.name}${type ? `（${type}）` : ""}`;
  return input.generationId ? `图${input.index} · 上游产物` : `图${input.index}`;
}

/** 取到的 blob 打不开时丢掉它，让占位图标顶上，而不是把 alt 文字铺成一大块。 */
function dropReferencePreview(key: string) {
  const url = referencePreviews.value[key];
  if (url?.startsWith("blob:")) URL.revokeObjectURL(url);
  referencePreviews.value = Object.fromEntries(
    Object.entries(referencePreviews.value).filter(([entryKey]) => entryKey !== key)
  );
}

function revokeReferencePreviews() {
  Object.values(referencePreviews.value).forEach((url) => {
    if (url.startsWith("blob:")) URL.revokeObjectURL(url);
  });
  referencePreviews.value = {};
}

/** 参考图走鉴权接口，img 标签直接取会 401，只能先取 blob 再挂上去。 */
async function syncReferencePreviews() {
  if (!import.meta.client) return;
  const next: Record<string, string> = {};
  for (const input of referenceInputs.value) {
    try {
      // ⚠️ 新后端的输入只带 ID：媒体直接取受保护端点，产物要先问地址（`/outputs/0` 回 JSON）
      const url = input.url || (input.generationId ? await outputUrlOf(input.generationId) : "");
      if (!url) continue;
      const response = await media.fetchMedia(url);
      if (!response.ok) continue;
      next[input.key] = URL.createObjectURL(await response.blob());
    } catch {
      // 单张参考图取不到不影响面板其余部分
    }
  }
  revokeReferencePreviews();
  referencePreviews.value = next;
}

watch(() => referenceInputs.value.map(input => input.url).join("|"), () => void syncReferencePreviews(), { immediate: true });
onBeforeUnmount(revokeReferencePreviews);

// 三种组共用这个面板：模型域与文案不同，其余交互一致
const assetGroup = computed(() => props.group.groupType === "asset");
const videoGroup = computed(() => props.group.groupType === "video_generator");
/**
 * 视频子项的操作码必须与后端 GroupNodeService.videoPlan 判定一致：
 * 只有首帧一张图走 img2video，带上本镜资产就必须走 ref2video ——
 * img2video 在上游只取第一张，其余参考图会被静默丢弃。
 * 模型选择器按 operation 拉取，不支持 ref2video 的模型因此自动不可选。
 */
/**
 * 视频子项默认走全能参考：分镜图当首帧、资产图当参考图，两者一起送出。
 * 用户可以在生成模式里改，改了就以 draft.mode 为准 —— 与画布视频节点同一套语义。
 */
const operation = computed(() => {
  if (assetGroup.value) return "asset_gen";
  if (!videoGroup.value) {
    // 分镜图带着参考图就是图生图（服务端规划时写进了快照）：一律按文生图选模型、报价的话，
    // 列出来的模型可能根本不收图，报的价也不是真正要扣的（CD-82）
    const planned = String(asObject(props.item.paramsSnapshot).operation ?? "").trim();
    return planned || (referenceInputs.value.length ? "img2img" : "text2img");
  }
  const mode = String(draft.value.mode ?? "").trim();
  if (mode) return mode;
  return referenceInputs.value.length ? "ref2video" : "text2video";
});
const subjectLabel = computed(() => {
  if (assetGroup.value) return "资产设定图";
  return videoGroup.value ? "分镜视频" : "分镜图";
});
const selectedPreset = computed(() => {
  if (videoGroup.value) return undefined;
  const code = String(draft.value.preset ?? "");
  if (!code) return undefined;
  return configStore.imagePresets.find(item => presetCodeOf(item) === code) as PresetItem | undefined;
});
const presetOptions = computed(() => asObject(draft.value.presetOptions));
const promptPlaceholder = computed(() => {
  if (selectedPreset.value?.description) return selectedPreset.value.description;
  if (assetGroup.value) return "描述该角色/场景/道具的外观特征";
  return videoGroup.value
    ? "描述这一镜的运动：起止状态、主体动作、运镜与音效"
    : "描述你想生成的分镜画面";
});
const emit = defineEmits<{
  update: [group: GroupDetail];
  close: [];
}>();

const configStore = useCanvasConfigStore();
/** 组 / 脚本接口只走统一入口（新契约，见 utils/scriptContract）。 */
const scripts = useScriptApi();
/**
 * 子项的提示词**不在子项上**：新后端执行时取镜头的提示词（资产取资产描述）。
 * 所以面板打开时把镜头 / 资产取来显示，改提示词就是改镜头的提示词 —— 否则在这里改的词不会生效。
 */
const shot = ref<ScriptShot>();
const promptKind = computed<"image" | "video">(() => (videoGroup.value ? "video" : "image"));
const draft = ref<JsonObject>({});
const savedFingerprint = ref("");
const modelOptions = ref<components["schemas"]["ModelSummaryResp"][]>([]);
const modelOverrides = ref<JsonObject>({});
const busy = ref<"save" | "generate" | "">("");
let syncVersion = 0;
/** 用户在面板里动过没有（提示词单独记）：载入镜头之前他就打了字的话，不能拿镜头上的词盖掉。 */
let userTouched = false;
let promptTouched = false;

const nodeConfig = computed(() => configStore.nodeType(videoGroup.value ? "video" : "image"));
const params = computed(() => nodeConfig.value?.params ?? []);
/**
 * 子项就是一个画布节点，版式与画布上的同类节点**同一份**（`utils/paramLayout.ts`）：
 * 视频子项顶部是参考/标记/特效/角色库/运镜，底部「模型 → 生成模式 → 尺寸」；
 * 图片子项底部「模型 → 尺寸 → 图片预设」。
 *
 * ⚠️ 数量不进子项的尺寸弹窗：一个子项就是一张图/一段视频，逐项计费逐项重试（AV-22）。
 * ⚠️ 图片子项**不渲染**顶部的「参考」上传：子项的参考图来自脚本流水线（inputRefs，
 *    下面那一排缩略图），此前这里落到通用 UploadControl —— 只 emit 文件名、一个字节都没传。
 */
/**
 * ⚠️ 视频子项不出顶部的「上传 · 角色库 · 运镜 · 特效」：子项的输入由服务端按镜头规划（videoPlan），
 *    参数只收白名单里的那几项（`GroupParamKeys`）—— 摆出来就是点了没用的按钮。
 */
const GROUP_ITEM_EXCLUDED = ["count", "reference", "roleLib", "camera", "effectLib", "mark"];
const layout = computed(() => nodeParamLayout(videoGroup.value ? "video" : "image", params.value, { exclude: GROUP_ITEM_EXCLUDED }));
const videoTopParams = computed(() => (videoGroup.value ? layout.value.top : []));
const bottomParams = computed(() => layout.value.bottom);
const sizeDialogBefore = computed(() => layout.value.sizeBefore);
const formatParams = computed<FormatParam[]>(() => layout.value.size.map(item => ({
  code: item.code ?? "",
  name: item.name ?? item.code ?? "",
  config: controlConfig(item),
  value: draft.value[paramKey(item.code)],
  defaultValue: item.defaultValue
})));
const running = computed(() => ["queued", "running"].includes(props.item.status));
const dirty = computed(() => fingerprint(draft.value) !== savedFingerprint.value);
const canGenerate = computed(() => Boolean(String(draft.value.prompt ?? "").trim())
  && Boolean(String(draft.value.model ?? "").trim()));

function fingerprint(value: JsonObject) {
  return JSON.stringify(value);
}

function normalizedParams(value?: Record<string, unknown>): JsonObject {
  // ⚠️ 新后端读平铺的 `aspect_ratio` / `quality`。此前这里反过来把平铺键**收进**老的
  //    `size` 对象 —— 用户在子项里选的画幅写进了一个没有人读的对象，出图照旧按默认画幅。
  const next = unwrapOptionObjects(liftLegacySize(asObject(value)));
  next.count = 1;
  // 资产组子项存的是 name/description（reconcile 时写入），没有 prompt；
  // 首次打开时用描述兜底，用户改动后即以 prompt 为准。
  if (assetGroup.value && !String(next.prompt ?? "").trim()) {
    next.prompt = String(next.description ?? "").trim();
  }
  return next;
}

function assetById(id: string) {
  return assets.value.find(asset => asset.id === id);
}

async function outputUrlOf(generationId: string) {
  const resolved = await scripts.resolveGroupOutputs({
    ...props.group,
    items: [{ ...props.item, status: "success", generationId, outputUrl: null }]
  });
  return resolved.items[0]?.outputUrl ?? "";
}

/** 这一项真正会用的提示词：镜头上的（分镜图 / 视频），或资产描述。 */
function sourcePrompt() {
  if (assetGroup.value) return assetById(String(props.item.assetId ?? ""))?.description ?? "";
  const text = promptKind.value === "video" ? shot.value?.videoPrompt : shot.value?.imagePrompt;
  return String(text ?? "");
}

async function loadSource() {
  try {
    const detail = await scripts.loadWorkbench(props.group.scriptId);
    shot.value = detail.shots.find(item => item.id === props.item.shotId);
    assets.value = detail.assets;
    // ⚠️ 不能按「草稿脏不脏」决定填不填词：载入镜头之前 syncModels / applyModel 已经往草稿里写了模型与默认参数，
    //    草稿早就「脏」了 —— 于是镜头上的提示词永远进不来，面板里提示词是空的、「生成」按钮是灰的（CD-81）。
    if (!promptTouched) draft.value = { ...draft.value, prompt: sourcePrompt() || String(draft.value.prompt ?? "") };
    // 打开时程序填进来的模型、默认参数、提示词都不算改动：否则一打开「保存」就是亮的
    if (!userTouched) savedFingerprint.value = fingerprint(draft.value);
    void syncReferencePreviews();
  } catch {
    // 取不到镜头也不挡住面板：只是提示词显示为空
  }
}

function resetDraft() {
  draft.value = { ...normalizedParams(props.item.paramsSnapshot), prompt: sourcePrompt() || String(asObject(props.item.paramsSnapshot).prompt ?? "") };
  if (!draft.value.model && modelOptions.value[0]?.code) draft.value.model = modelOptions.value[0].code;
  savedFingerprint.value = fingerprint(draft.value);
}

/** 刷新整组并交给画布（子项的保存只回一个子项；画布上要的是整组）。 */
async function refreshGroup(resp?: GroupResp) {
  emit("update", resp ? await scripts.adoptGroup(resp, props.group) : await scripts.refreshGroup(props.group));
}

/** 要随子项存下的参数：提示词不在这里（在镜头 / 资产上），模型执行时单独给。 */
function itemParams() {
  const { prompt: _prompt, model: _model, ...rest } = draft.value;
  return rest;
}

function controlConfig(param: ParamConfig) {
  let value = mergeObjects(asObject(param.config), asObject(modelOverrides.value[param.code ?? ""]));
  if (param.code === "model") {
    value = {
      ...value,
      options: modelOptions.value.map(model => ({
        label: `${model.name ?? model.code}${model.isVip ? " · VIP" : ""}`,
        value: model.code
      }))
    };
  }
  return value;
}

function controlValue(param: ParamConfig) {
  return draft.value[paramKey(param.code)];
}

function updateParam(code: string, value: unknown) {
  userTouched = true;
  if (code === "prompt") promptTouched = true;
  draft.value = { ...draft.value, [code]: value };
  if (code === "model" && typeof value === "string") void applyModel(value);
}

async function applyModel(modelCode: string) {
  const version = ++syncVersion;
  const overrides = await configStore.overrides(modelCode) as JsonObject;
  if (version !== syncVersion) return;
  modelOverrides.value = overrides;
  draft.value = reconcileParams(draft.value, modelReconcileRules(params.value, overrides));
  draft.value.count = 1;
}

async function syncModels() {
  // domain 必须跟着组类型走：视频子项的 operation 是 img2video/text2video，
  // 拿 image 域去查一个模型都取不到，选择器会变成空壳
  modelOptions.value = await configStore.models(videoGroup.value ? "video" : "image", operation.value);
  const selected = modelOptions.value.find(model => model.code === draft.value.model) ?? modelOptions.value[0];
  if (!selected?.code) return;
  if (!draft.value.model) draft.value = { ...draft.value, model: selected.code };
  await applyModel(selected.code);
}

function selectImagePreset(preset: PresetItem) {
  userTouched = true;
  draft.value = applyImagePreset(draft.value, preset);
}

function clearPreset() {
  userTouched = true;
  draft.value = clearImagePreset(draft.value);
}

/**
 * 保存：提示词存回镜头（或资产描述），参数存在子项上。
 * ⚠️ 改了镜头的提示词，镜头版本 +1，这一项就成了「建组之后镜头改过」—— 执行时会被拦下（FLOW-05）。
 *    存子项（`PUT .../items/{id}`）会把它对齐到镜头的当前版本并重算输入，所以两步要一起做。
 */
async function persist() {
  const prompt = String(draft.value.prompt ?? "").trim();
  if (assetGroup.value) {
    const asset = assetById(String(props.item.assetId ?? ""));
    if (asset && prompt && prompt !== (asset.description ?? "").trim()) await scripts.saveAsset(asset, { description: prompt });
    return;
  }
  if (shot.value && prompt && prompt !== sourcePrompt().trim()) {
    shot.value = await scripts.savePrompt(props.group.scriptId, shot.value, promptKind.value, prompt);
  }
  await scripts.saveGroupItem(props.group.groupId, props.item.itemId, { params: itemParams() });
}

async function save() {
  if (busy.value || !dirty.value) return;
  busy.value = "save";
  try {
    await persist();
    savedFingerprint.value = fingerprint(draft.value);
    await refreshGroup();
    ElMessage.success(`${subjectLabel.value}参数已保存`);
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : `${subjectLabel.value}参数保存失败`);
  } finally {
    busy.value = "";
  }
}

async function generate() {
  if (busy.value || running.value || !canGenerate.value) return;
  busy.value = "generate";
  try {
    if (dirty.value) await persist();
    savedFingerprint.value = fingerprint(draft.value);
    const modelCode = String(draft.value.model ?? "");
    if (assetGroup.value) {
      // 资产图的重跑入口在资产本身（资产组不支持整组执行，AV-23）
      const [result] = await scripts.generateAssets(props.group.scriptId, {
        modelCode,
        assetIds: [String(props.item.assetId ?? "")],
        params: liftLegacySize(itemParams())
      });
      await refreshGroup();
      if (result?.skipped) {
        ElMessage.warning(result.reason || `当前${subjectLabel.value}没有提交`);
        return;
      }
    } else {
      // 新后端没有逐项生成的路径：执行时点名这一项（会是一次新任务、重新计费）。版本取最新的组
      const latest = await scripts.getGroup(props.group.groupId);
      await refreshGroup(await scripts.executeGroup(props.group.groupId, {
        modelCode, version: latest.version, itemIds: [props.item.itemId], force: true
      }));
    }
    ElMessage.success(`当前${subjectLabel.value}已重新提交`);
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : `${subjectLabel.value}生成失败`);
  } finally {
    busy.value = "";
  }
}

watch(() => props.item.itemId, resetDraft, { immediate: true });
watch(() => JSON.stringify(props.item.paramsSnapshot ?? {}), () => {
  if (!dirty.value) resetDraft();
});

onMounted(async () => {
  await configStore.load();
  await syncModels();
  await loadSource();
});

</script>

<template>
  <section class="node-param-panel group-image-node-panel nodrag nowheel" @mousedown.stop>
    <header class="group-image-panel-heading">
      <span><ImageIcon :size="15" />{{ title }}</span>
      <button type="button" title="关闭图片参数" @click="emit('close')"><X :size="15" /></button>
    </header>
    <div v-if="videoTopParams.length" class="node-top-controls">
      <VideoTopTools
        :controls="videoTopParams"
        :params="draft"
        :project-id="group.projectId"
        :node-id="item.canvasNodeId"
        @update="(code, value) => updateParam(code, value)"
      />
    </div>
    <!-- 参考图：编号与提示词里的「图N」同一套，名字说清是哪个资产（CD-82） -->
    <div v-if="referenceInputs.length" class="group-image-reference-strip" :aria-label="`参考图 ${referenceInputs.length} 张`">
      <figure v-for="input in referenceInputs" :key="input.key" :title="referenceTitle(input)">
        <img
          v-if="referencePreviews[input.key]"
          :src="referencePreviews[input.key]"
          :alt="referenceTitle(input)"
          @error="dropReferencePreview(input.key)"
        >
        <ImageIcon v-else :size="14" />
        <figcaption>{{ input.index }}</figcaption>
        <span v-if="input.name" class="group-image-reference-name">{{ input.name }}</span>
      </figure>
    </div>
    <ImagePresetChip
      v-if="selectedPreset"
      :preset="selectedPreset"
      :options="presetOptions"
      @update="value => updateParam('presetOptions', value)"
      @clear="clearPreset"
    />
    <textarea :value="String(draft.prompt ?? '')" :placeholder="promptPlaceholder" @input="updateParam('prompt', ($event.target as HTMLTextAreaElement).value)" />
    <div class="node-bottom-controls">
      <div class="node-control-scroll">
        <template v-for="param in bottomParams" :key="param.code">
          <GenerationSizeControl
            v-if="param.code === sizeDialogBefore"
            :node-type="videoGroup ? 'video' : 'image'"
            :params="formatParams"
            @update="updateParam"
          />
          <!-- ⚠️ 新后端的参数代号是 video_mode，而子项与画布节点生效的键都是 `mode` -->
          <VideoModeControl
            v-if="videoGroup && isVideoModeCode(param.code)"
            :model-value="draft.mode ?? operation"
            :config="controlConfig(param)"
            @update:model-value="updateParam('mode', $event)"
          />
          <ImagePresetControl
            v-else-if="param.code === 'preset'"
            :presets="configStore.imagePresets"
            :selected="String(draft.preset ?? '')"
            @select="selectImagePreset"
          />
          <DynamicControl
            v-else
            :control="param.control ?? ''"
            :name="param.name ?? param.code ?? ''"
            :config="controlConfig(param)"
            :model-value="controlValue(param)"
            @update:model-value="updateParam(paramKey(param.code), $event)"
          />
        </template>
        <GenerationSizeControl
          v-if="!sizeDialogBefore"
          :node-type="videoGroup ? 'video' : 'image'"
          :params="formatParams"
          @update="updateParam"
        />
      </div>
      <button class="group-image-save" type="button" :disabled="Boolean(busy) || !dirty" title="保存图片参数" @click="save">
        <LoaderCircle v-if="busy === 'save'" :size="16" class="spin" />
        <Save v-else :size="16" />
      </button>
      <!-- 原「预计积分」角标的位置（TT-T-14 删除金额）：保留空占位把发送键推到右侧，布局不变 -->
      <span class="node-estimate" aria-hidden="true" />
      <button class="node-generate" type="button" :disabled="Boolean(busy) || running || !canGenerate" :title="`重新生成当前${subjectLabel}`" @click="generate">
        <LoaderCircle v-if="busy === 'generate' || running" :size="17" class="spin" />
        <ArrowUp v-else :size="18" />
      </button>
    </div>
  </section>
</template>
