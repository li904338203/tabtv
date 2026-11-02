<script setup lang="ts">
/**
 * 设置 → 常规（TT-F-33、TT-F-18）：并发上限、日志保留期、局域网上游开关、ffmpeg 路径、水印、数据目录。
 *
 * ⚠️ 每一项都标「立即生效 / 重启后生效」（后端按键给出）：出网策略与 ffmpeg 路径在启动时装配，
 *    改了不重启的话用户会以为已经生效。
 * ⚠️ 水印开关的字段是「关闭标志」（isAiWatermarkOff），界面上的开关是「开启水印」，中间取反一次；
 *    关掉之后成片不再带 AI 生成标识，且只影响之后的渲染（与来源系统账号页同一句说明）。
 */
import { ElMessage } from "element-plus";
import { hostApp } from "~/utils/desktopBridge";

type SettingItem = { key: string; kind: string; value: unknown; default: unknown; set: boolean; restart: boolean; min?: number; max?: number };
type SettingsResp = { items: SettingItem[]; dataDir: string; restartRequired?: string[] };
type RenderSetting = { watermarkMediaId: string; watermarkMissing?: boolean; position: string; opacity: number; scale: number; version?: number };

const auth = useAuthStore();
const items = ref<SettingItem[]>([]);
const dataDir = ref("");
const form = reactive<Record<string, unknown>>({});
const saving = ref(false);
const restartNotice = ref<string[]>([]);
const loadError = ref("");

const render = reactive<RenderSetting>({ watermarkMediaId: "", position: "bottom_right", opacity: 80, scale: 12 });
const watermarkOn = ref(true);

const LABELS: Record<string, { label: string; help: string }> = {
  "generation.max_running": { label: "同时运行的生成任务", help: "超出的任务排队等待，不会失败。" },
  "render.max_concurrent": { label: "同时渲染的成片", help: "渲染很吃 CPU，一般保持 1。" },
  "request_log.retention_days": { label: "调用日志保留天数", help: "发给模型上游的请求记录，到期自动清理。" },
  "provider.allow_private_network": { label: "允许访问局域网上游", help: "只在你自己在局域网里部署了模型网关时打开；打开后应用可以访问内网地址。" },
  "ffmpeg.path": { label: "ffmpeg 路径", help: "留空使用随应用附带的 ffmpeg。" },
  "ffprobe.path": { label: "ffprobe 路径", help: "留空使用随应用附带的 ffprobe。" }
};
const SHOWN = Object.keys(LABELS);
const shownItems = computed(() => items.value.filter(i => SHOWN.includes(i.key)));

async function load() {
  loadError.value = "";
  try {
    const resp = await useApi()<SettingsResp>("/settings");
    items.value = resp.items;
    dataDir.value = resp.dataDir;
    for (const it of resp.items) form[it.key] = it.value;
    const rs = await useApi()<RenderSetting>("/admin/render/settings");
    Object.assign(render, rs);
    await auth.loadProfile();
    watermarkOn.value = auth.user?.isAiWatermarkOff !== true;
  } catch (e) {
    loadError.value = (e as { message?: string }).message || "设置加载失败";
  }
}

async function save() {
  // 只提交改过的键：整份回写会把没碰过的项也标成「已设置」，默认值以后就跟不上了。
  const changed: Record<string, unknown> = {};
  for (const it of items.value) {
    if (SHOWN.includes(it.key) && form[it.key] !== it.value) changed[it.key] = form[it.key];
  }
  saving.value = true;
  try {
    if (Object.keys(changed).length) {
      const resp = await useApi()<SettingsResp>("/settings", { method: "PUT", body: changed });
      items.value = resp.items;
      restartNotice.value = resp.restartRequired ?? [];
    }
    await useApi()("/admin/render/settings", {
      method: "PUT",
      body: { watermarkMediaId: render.watermarkMediaId, position: render.position, opacity: render.opacity, scale: render.scale }
    });
    if (watermarkOn.value !== (auth.user?.isAiWatermarkOff !== true)) await auth.setWatermarkOff(!watermarkOn.value);
    ElMessage.success(restartNotice.value.length ? "已保存，部分设置重启后生效" : "已保存");
  } catch (e) {
    ElMessage.error((e as { message?: string }).message || "保存失败");
  } finally {
    saving.value = false;
  }
}

const { upload } = useReferenceUpload();
async function pickWatermark(event: Event) {
  const file = (event.target as HTMLInputElement).files?.[0];
  if (!file) return;
  try {
    render.watermarkMediaId = await upload(file, { mediaType: "image" });
    ElMessage.success("水印图已上传，保存后生效");
  } catch {
    ElMessage.error("水印图上传失败");
  }
}

function openDataDir() {
  void hostApp()?.OpenDataDir?.();
}

onMounted(() => { void load(); });
</script>

<template>
  <div class="settings-panel">
    <div class="section-head"><div><h2>常规</h2><p>并发、日志、网络与水印</p></div></div>
    <p v-if="loadError" class="settings-error">{{ loadError }} <button type="button" class="portal-link-button" @click="load">重试</button></p>

    <el-form label-position="top" class="settings-form">
      <el-form-item v-for="it in shownItems" :key="it.key">
        <template #label>
          <span>{{ LABELS[it.key]?.label }}</span>
          <el-tag v-if="it.restart" size="small" type="warning" effect="plain" class="settings-tag">重启后生效</el-tag>
        </template>
        <el-input-number v-if="it.kind === 'int'" v-model="form[it.key] as number" :min="it.min" :max="it.max" />
        <el-switch v-else-if="it.kind === 'bool'" v-model="form[it.key] as boolean" />
        <el-input v-else v-model="form[it.key] as string" :placeholder="'使用随应用附带的版本'" clearable />
        <small class="settings-help" :class="{ danger: it.key === 'provider.allow_private_network' && form[it.key] }">{{ LABELS[it.key]?.help }}</small>
      </el-form-item>

      <div class="section-head"><div><h2>水印</h2><p>成片右下角的 AI 生成标识</p></div></div>
      <el-form-item label="在成片上加 AI 生成标识">
        <el-switch v-model="watermarkOn" />
        <small class="settings-help">关掉之后成片不带 AI 生成标识；只影响之后的渲染，已经渲好的不变。</small>
      </el-form-item>
      <el-form-item label="水印图片">
        <input type="file" accept="image/png,image/webp" @change="pickWatermark">
        <small class="settings-help">{{ render.watermarkMediaId && render.watermarkMediaId !== "0" ? "已设置自定义水印图" : "未设置时使用文字标识" }}</small>
      </el-form-item>
      <el-form-item label="位置">
        <el-select v-model="render.position" style="width: 200px">
          <el-option label="右下" value="bottom_right" />
          <el-option label="左下" value="bottom_left" />
          <el-option label="右上" value="top_right" />
          <el-option label="左上" value="top_left" />
        </el-select>
      </el-form-item>
      <el-form-item label="不透明度（%）"><el-slider v-model="render.opacity" :min="10" :max="100" style="width: 320px" /></el-form-item>
      <el-form-item label="大小（画面宽度的 %）"><el-slider v-model="render.scale" :min="4" :max="30" style="width: 320px" /></el-form-item>

      <div class="section-head"><div><h2>数据目录</h2><p>数据库、素材与日志都在这里</p></div></div>
      <el-form-item>
        <code class="settings-path">{{ dataDir || "—" }}</code>
        <el-button size="small" @click="openDataDir">在文件管理器中打开</el-button>
      </el-form-item>

      <el-button type="primary" :loading="saving" @click="save">保存</el-button>
    </el-form>
  </div>
</template>

<style scoped>
.settings-form { max-width: 640px; margin-top: 12px; }
.settings-help { display: block; width: 100%; margin-top: 4px; color: var(--portal-faint); font-size: 12px; line-height: 1.5; }
.settings-help.danger { color: var(--portal-danger); }
.settings-tag { margin-left: 8px; }
.settings-error { color: var(--portal-danger); font-size: 13px; }
.settings-path { margin-right: 12px; padding: 4px 8px; border-radius: 6px; background: var(--portal-code-bg); font-size: 12px; word-break: break-all; }
</style>
