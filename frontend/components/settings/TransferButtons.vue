<script setup lang="ts">
/**
 * 设置页的「导出 / 导入」两个按钮（TT-T-19~21）。文件形状校验在 `utils/settingsTransfer.ts`；
 * 这里只负责选文件、校验、确认，然后把通过校验的条目交给页面去逐条保存。
 */
import { ElButton, ElMessage, ElMessageBox } from "element-plus";
import { KIND_LABELS, parseImport, type TransferKind } from "~/utils/settingsTransfer";

const props = defineProps<{ kind: TransferKind; busy?: boolean; note?: string }>();
const emit = defineEmits<{ (e: "export"): void; (e: "import", items: Record<string, unknown>[]): void }>();

const input = ref<HTMLInputElement | null>(null);

async function onPick(event: Event) {
  const file = (event.target as HTMLInputElement).files?.[0];
  (event.target as HTMLInputElement).value = "";
  if (!file) return;
  const r = parseImport(await file.text(), props.kind);
  if (!r.ok) {
    ElMessage.error({ message: `不能导入：${r.error}`, duration: 8000 });
    return;
  }
  try {
    await ElMessageBox.confirm(
      `文件里有 ${r.items.length} 条${KIND_LABELS[props.kind]}。同名（同 code）的已有条目会被覆盖；新建的一律是停用状态。${props.note ?? ""}`,
      `导入${KIND_LABELS[props.kind]}`,
      { confirmButtonText: "导入", cancelButtonText: "取消", type: "warning" }
    );
  } catch {
    return; // 取消
  }
  emit("import", r.items);
}
</script>

<template>
  <div class="transfer-buttons">
    <ElButton :disabled="busy" @click="emit('export')">导出 JSON</ElButton>
    <ElButton :loading="busy" @click="input?.click()">导入 JSON</ElButton>
    <input ref="input" type="file" accept="application/json,.json" hidden @change="onPick">
  </div>
</template>

<style scoped>
.transfer-buttons { display: inline-flex; gap: 8px; }
</style>
