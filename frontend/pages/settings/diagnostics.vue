<script setup lang="ts">
/**
 * 设置 → 环境自检（TT-F-35）：每一项给状态、现值与处置建议 —— 只说「不可用」的话用户不知道下一步做什么。
 */
import { CircleAlert, CircleCheck, CircleX } from "@lucide/vue";
import { hostApp } from "~/utils/desktopBridge";

type Check = { key: string; label: string; status: "ok" | "warn" | "fail"; value: string; advice?: string };
const checks = ref<Check[]>([]);
const version = ref("");
const loading = ref(false);
const loadError = ref("");

async function run() {
  loading.value = true;
  loadError.value = "";
  try {
    const resp = await useApi()<{ version: string; checks: Check[] }>("/settings/diagnostics");
    checks.value = resp.checks;
    version.value = resp.version;
  } catch (e) {
    loadError.value = (e as { message?: string }).message || "自检失败";
  } finally {
    loading.value = false;
  }
}

onMounted(() => { void run(); });
</script>

<template>
  <div class="settings-panel">
    <div class="section-head">
      <div><h2>环境自检</h2><p>tabtv {{ version || "—" }}</p></div>
      <div class="diag-actions">
        <el-button size="small" @click="hostApp()?.OpenLogDir?.()">打开日志目录</el-button>
        <el-button size="small" :loading="loading" @click="run">重新检查</el-button>
      </div>
    </div>
    <p v-if="loadError" class="settings-error">{{ loadError }}</p>
    <ul class="diag-list">
      <li v-for="c in checks" :key="c.key" :class="c.status">
        <CircleCheck v-if="c.status === 'ok'" :size="18" />
        <CircleAlert v-else-if="c.status === 'warn'" :size="18" />
        <CircleX v-else :size="18" />
        <div>
          <strong>{{ c.label }}</strong>
          <p class="diag-value">{{ c.value }}</p>
          <p v-if="c.advice" class="diag-advice">{{ c.advice }}</p>
        </div>
      </li>
    </ul>
  </div>
</template>

<style scoped>
.diag-actions { display: flex; gap: 8px; }
.diag-list { margin: 12px 0 0; padding: 0; list-style: none; display: flex; flex-direction: column; gap: 8px; }
.diag-list li { display: flex; gap: 12px; padding: 12px 14px; border: 1px solid var(--portal-line); border-radius: 10px; background: var(--portal-surface); }
.diag-list li.ok > svg { color: var(--portal-success); }
.diag-list li.warn > svg { color: var(--portal-gold); }
.diag-list li.fail > svg { color: var(--portal-danger); }
.diag-value { margin: 4px 0 0; color: var(--portal-muted); font-size: 12px; word-break: break-all; }
.diag-advice { margin: 4px 0 0; color: var(--portal-text-soft); font-size: 12px; }
.settings-error { color: var(--portal-danger); font-size: 13px; }
</style>
