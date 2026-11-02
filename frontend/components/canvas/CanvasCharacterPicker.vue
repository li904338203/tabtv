<script setup lang="ts">
/**
 * 画布视频节点的「角色库」—— 与 Agent / 营销输入框**同一个角色库弹窗**（`CharacterPickerDialog`）：
 * 「我的」角色 + 只读的脚本角色资产 + 平台营销角色（添加时复制一份进「我的」，MKD-30）。
 *
 * 选中的角色以**主图**作为参考图进这个节点的生成输入（ParamPanel 的 requestInputs）。
 * 此前这里是老系统的角色选择器，列的是**画布上的图片节点** —— 那其实是「画布参考」，而连线本来就能做到；
 * 真正的角色库（建好、存在工作区里、跨项目复用的角色）在画布上没有入口。
 *
 * 弹窗里的「新增 / 编辑」要用到推理模型（一键优化）与图片模型（生成角色图）：这里按用户的默认偏好取，
 * 与输入框的取法一致（会话之外没有会话级偏好）。
 */
import CharacterPickerDialog from "~/components/agent/composer/marketing/CharacterPickerDialog.vue";
import { effectivePrefs as resolvePrefs, prefModel, resolveReasoningModel } from "~/utils/agentPrefs";
import { reasoningUnavailable } from "~/utils/composerPrice";
import { marketingCharacterLimits, type MarketingPickedCharacter } from "~/utils/marketingLibrary";

const props = defineProps<{
  selected: MarketingPickedCharacter[];
  /** 这个节点最多挂几个角色（`roleLib` 的配置 maxCount）。 */
  maxRefs: number;
}>();
const emit = defineEmits<{
  close: [];
  pick: [characters: MarketingPickedCharacter[]];
  deleted: [id: string];
  updated: [character: MarketingPickedCharacter];
}>();

const marketing = useMarketingConfig();
const prefsStore = useAgentPrefsStore();
const workspace = useWorkspaceStore();

onMounted(() => {
  void marketing.load();
  void prefsStore.load();
  void prefsStore.loadOptions("reasoning");
});

const prefs = computed(() => resolvePrefs(prefsStore.prefs, {}));
const reasoningOptions = computed(() => prefsStore.options("reasoning"));
const reasoningCode = computed(() => resolveReasoningModel(prefs.value, reasoningOptions.value));
const optimizeUnavailable = computed(() => reasoningUnavailable(
  prefsStore.option("reasoning", reasoningCode.value) ?? reasoningOptions.value[0]
));
const limits = computed(() => ({ ...marketingCharacterLimits(marketing.config.value), maxRefs: props.maxRefs }));
const readOnly = computed(() => workspace.current?.role === "viewer");
</script>

<template>
  <CharacterPickerDialog
    :selected="selected"
    :limits="limits"
    :read-only="readOnly"
    :optimize-unavailable="optimizeUnavailable"
    :optimize-model-code="reasoningCode"
    :default-model-code="prefModel(prefs, 'image') ?? ''"
    @close="emit('close')"
    @pick="emit('pick', $event)"
    @deleted="emit('deleted', $event)"
    @updated="emit('updated', $event)"
  />
</template>
