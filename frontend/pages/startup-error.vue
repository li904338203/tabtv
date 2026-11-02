<script setup lang="ts">
/**
 * 启动错误页（TT-F-02）：说清原因、数据目录与日志位置，并能重试。
 * 不套平台布局 —— 启动没成功时侧栏里的入口一个都用不了。
 */
const failure = ref<StartupError | null>(null);
const retrying = ref(false);

onMounted(async () => {
  failure.value = (await hostApp()?.GetStartupError().catch(() => null)) ?? null;
});

async function retry() {
  const host = hostApp();
  if (!host) return;
  retrying.value = true;
  try {
    failure.value = await host.RetryStartup();
    if (!failure.value) await navigateTo("/", { replace: true });
  } finally {
    retrying.value = false;
  }
}
</script>

<template>
  <main class="startup-error">
    <h1>tabtv 没能启动</h1>
    <template v-if="failure">
      <p class="reason">{{ failure.reason }}</p>
      <pre class="detail" data-testid="startup-detail">{{ failure.detail }}</pre>
      <dl>
        <dt>数据目录</dt>
        <dd data-testid="startup-data-dir">{{ failure.dataDir || "（未确定）" }}</dd>
        <dt>日志目录</dt>
        <dd data-testid="startup-log-dir">{{ failure.logDir || "（未确定）" }}</dd>
      </dl>
    </template>
    <p v-else>没有读到错误详情。</p>
    <button type="button" @click="hostApp()?.OpenLogDir?.()">打开日志目录</button>
    <button type="button" :disabled="retrying" data-testid="startup-retry" @click="retry">
      {{ retrying ? "正在重试…" : "重试" }}
    </button>
  </main>
</template>

<style scoped>
.startup-error {
  max-width: 720px;
  margin: 80px auto;
  padding: 0 24px;
  color: var(--text-primary, #e8e8e8);
  font: 14px/1.7 system-ui, sans-serif;
}
.reason { font-size: 16px; }
.detail {
  white-space: pre-wrap;
  padding: 12px;
  border-radius: 8px;
  background: rgba(127, 127, 127, 0.12);
}
dl { display: grid; grid-template-columns: 6em 1fr; gap: 4px 12px; }
dt { opacity: 0.7; }
dd { margin: 0; word-break: break-all; }
button { margin: 16px 12px 0 0; padding: 6px 20px; }
</style>
