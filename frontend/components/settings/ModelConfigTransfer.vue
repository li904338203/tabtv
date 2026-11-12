<script setup lang="ts">
/**
 * 模型与厂商：整套配置的「导出配置 / 导入配置」（换电脑、重装时同步厂商、模型、Key 与公网中转存储）。
 * 导入先预览（dryRun）再确认写入；文件的解析与写入都在后端（`/admin/model-config/*`）。
 */
import { ElButton, ElDialog, ElMessage, ElMessageBox } from "element-plus";
import { settingsApiError } from "~/utils/settingsApi";
import {
  modelConfigFileName, modelConfigOutcome, modelConfigPreviewLines, parseModelConfigText,
  type ModelConfigOutcome, type ModelConfigReport
} from "~/utils/settingsModelConfig";
import { downloadJson } from "~/utils/settingsTransfer";

const emit = defineEmits<{ (e: "changed"): void }>();
const api = useApi();
const input = ref<HTMLInputElement | null>(null);
const busy = ref(false);
const pending = ref<Record<string, unknown> | null>(null);
const preview = ref<ModelConfigReport | null>(null);
const result = ref<{ report: ModelConfigReport; outcome: ModelConfigOutcome } | null>(null);

async function exportAll() {
  try {
    await ElMessageBox.confirm(
      "导出的文件包含全部厂商的 API Key 与公网中转存储的密钥（明文）。请只在自己的设备之间传递，不要发到群聊或公开网盘。",
      "导出模型配置",
      { confirmButtonText: "导出", cancelButtonText: "取消", type: "warning" }
    );
  } catch {
    return;
  }
  busy.value = true;
  try {
    downloadJson(modelConfigFileName(), await api<Record<string, unknown>>("/admin/model-config/export"));
  } catch (e) {
    ElMessage.error(settingsApiError(e).message);
  } finally {
    busy.value = false;
  }
}

async function onPick(event: Event) {
  const file = (event.target as HTMLInputElement).files?.[0];
  (event.target as HTMLInputElement).value = "";
  if (!file) return;
  const parsed = parseModelConfigText(await file.text());
  if (!parsed.ok) {
    ElMessage.error({ message: `不能导入：${parsed.error}`, duration: 8000 });
    return;
  }
  busy.value = true;
  try {
    preview.value = await api<ModelConfigReport>("/admin/model-config/import?dryRun=true", { method: "POST", body: parsed.data });
    pending.value = parsed.data;
  } catch (e) {
    ElMessage.error(settingsApiError(e).message);
  } finally {
    busy.value = false;
  }
}

async function confirmImport() {
  if (!pending.value) return;
  busy.value = true;
  try {
    const report = await api<ModelConfigReport>("/admin/model-config/import", { method: "POST", body: pending.value });
    result.value = { report, outcome: modelConfigOutcome(report) };
    preview.value = null;
    pending.value = null;
    emit("changed");
  } catch (e) {
    ElMessage.error(settingsApiError(e).message);
  } finally {
    busy.value = false;
  }
}

function closePreview() {
  preview.value = null;
  pending.value = null;
}
</script>

<template>
  <div class="mc-transfer">
    <ElButton :disabled="busy" @click="exportAll">导出配置</ElButton>
    <ElButton :loading="busy" @click="input?.click()">导入配置</ElButton>
    <input ref="input" type="file" accept="application/json,.json" hidden @change="onPick">

    <ElDialog :model-value="!!preview" title="导入模型配置" width="520px" @close="closePreview">
      <template v-if="preview">
        <div v-if="preview.problems.length" class="mc-problems">
          <p>文件有问题，不能导入：</p>
          <ul><li v-for="p in preview.problems" :key="p">{{ p }}</li></ul>
        </div>
        <template v-else>
          <ul class="mc-lines"><li v-for="l in modelConfigPreviewLines(preview)" :key="l">{{ l }}</li></ul>
          <p class="mc-note">同名（同 code）的厂商与模型以文件为准覆盖，模型的操作绑定、输入规则、参数也会同步成文件里的样子。</p>
        </template>
      </template>
      <template #footer>
        <ElButton @click="closePreview">取消</ElButton>
        <ElButton type="primary" :loading="busy" :disabled="!!preview?.problems.length" @click="confirmImport">导入</ElButton>
      </template>
    </ElDialog>

    <ElDialog :model-value="!!result" title="导入完成" width="560px" @close="result = null">
      <template v-if="result">
        <p>成功 {{ result.outcome.ok }} 项<template v-if="result.outcome.failed.length">，{{ result.outcome.failed.length }} 项未完成</template>。</p>
        <p v-if="result.report.restartRequired" class="mc-warn">公网中转存储已写入，重启应用后生效。</p>
        <div v-if="result.outcome.failed.length" class="mc-problems">
          <p>未完成：</p>
          <ul><li v-for="f in result.outcome.failed" :key="f.label + f.message"><b>{{ f.label }}</b>：{{ f.message }}</li></ul>
        </div>
        <div v-if="result.outcome.warned.length" class="mc-warned">
          <p>提醒：</p>
          <ul><li v-for="w in result.outcome.warned" :key="w.label + w.message"><b>{{ w.label }}</b>：{{ w.message }}</li></ul>
        </div>
        <p v-if="result.outcome.failed.length" class="mc-note">修好原因后可以再导入一次，已成功的会原样覆盖，不会重复。</p>
      </template>
      <template #footer><ElButton type="primary" @click="result = null">知道了</ElButton></template>
    </ElDialog>
  </div>
</template>

<style scoped>
.mc-transfer { display: inline-flex; gap: 8px; }
.mc-lines { margin: 0; padding-left: 18px; line-height: 1.9; }
.mc-note { color: var(--portal-faint); font-size: 12px; margin-top: 10px; }
.mc-warn { color: var(--portal-warning, #b7791f); }
.mc-problems { color: var(--portal-danger); }
.mc-problems ul, .mc-warned ul { margin: 4px 0 0; padding-left: 18px; line-height: 1.8; font-size: 13px; }
.mc-warned { color: var(--portal-faint); margin-top: 8px; }
</style>
