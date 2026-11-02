<script setup lang="ts">
/**
 * 设置 → 存储（TT-F-34、TT-D-05）：数据目录占用 + 可选 S3 兼容存储。
 *
 * 可选 S3 只为「只收公网地址作参考媒体」的上游服务：素材平时都在本机，只有发给这类上游的那几张才转存一份。
 * ⚠️ 密钥三态（TT-F-28）：读回来的只有掩码 —— 不动就保留、填了新值才替换、点「清除」才清空；
 *    掩码值绝不能被当成新密钥提交回去（来源系统同一条规则）。
 */
import { ElButton, ElForm, ElFormItem, ElInput, ElInputNumber, ElMessage, ElRadioButton, ElRadioGroup, ElSwitch } from "element-plus";
import { formatBytes } from "~/utils/settingsNav";

type SettingItem = { key: string; kind: string; value: unknown; set: boolean };
type Usage = { dataDir: string; media: number; database: number; logs: number; backups: number };
type TestStep = { step: string; ok: boolean; message?: string };

const usage = ref<Usage | null>(null);
const saving = ref(false);
const testing = ref(false);
const testResult = ref<{ ok: boolean; steps: TestStep[] } | null>(null);
const secretMask = ref("");
const secretAction = ref<"keep" | "replace" | "clear">("keep");
const form = reactive({
  endpoint: "", region: "", bucket: "", accessKey: "", secretKey: "", pathStyle: true, prefix: "relay/", signTTLHours: 6
});
const KEYS: Record<string, keyof typeof form> = {
  "storage.s3.endpoint": "endpoint", "storage.s3.region": "region", "storage.s3.bucket": "bucket",
  "storage.s3.access_key": "accessKey", "storage.s3.path_style": "pathStyle", "storage.s3.prefix": "prefix",
  "storage.s3.sign_ttl_hours": "signTTLHours"
};

async function load() {
  usage.value = await useApi()<Usage>("/settings/storage/usage").catch(() => null);
  const resp = await useApi()<{ items: SettingItem[] }>("/settings");
  for (const it of resp.items) {
    const field = KEYS[it.key];
    if (field) (form as Record<string, unknown>)[field] = it.value;
    if (it.key === "storage.s3.secret_key") secretMask.value = String(it.value ?? "");
  }
  form.secretKey = "";
  secretAction.value = "keep";
}

function payload() {
  const body: Record<string, unknown> = {};
  for (const [key, field] of Object.entries(KEYS)) body[key] = form[field];
  if (secretAction.value === "replace") body["storage.s3.secret_key"] = form.secretKey;
  if (secretAction.value === "clear") body["storage.s3.secret_key"] = "";
  return body;
}

async function save() {
  if (secretAction.value === "replace" && !form.secretKey) {
    ElMessage.warning("填写新的 Secret Key，或选择「保留原值」");
    return;
  }
  saving.value = true;
  try {
    await useApi()("/settings", { method: "PUT", body: payload() });
    ElMessage.success("已保存，重启应用后生效");
    await load();
  } catch (e) {
    ElMessage.error((e as { message?: string }).message || "保存失败");
  } finally {
    saving.value = false;
  }
}

async function test() {
  testing.value = true;
  testResult.value = null;
  try {
    testResult.value = await useApi()("/settings/storage/test", {
      method: "POST",
      body: {
        Endpoint: form.endpoint, Region: form.region, Bucket: form.bucket, AccessKey: form.accessKey,
        SecretKey: secretAction.value === "replace" ? form.secretKey : "", PathStyle: form.pathStyle, Prefix: form.prefix,
        KeepSecret: secretAction.value === "keep"
      }
    });
  } catch (e) {
    ElMessage.error((e as { message?: string }).message || "测试失败");
  } finally {
    testing.value = false;
  }
}

onMounted(() => { void load(); });
</script>

<template>
  <div class="settings-panel">
    <div class="section-head"><div><h2>本机占用</h2><p>{{ usage?.dataDir || "数据目录" }}</p></div></div>
    <div v-if="usage" class="storage-usage">
      <div><strong>{{ formatBytes(usage.media) }}</strong><small>素材与成片</small></div>
      <div><strong>{{ formatBytes(usage.database) }}</strong><small>数据库</small></div>
      <div><strong>{{ formatBytes(usage.backups) }}</strong><small>升级前备份</small></div>
      <div><strong>{{ formatBytes(usage.logs) }}</strong><small>日志</small></div>
    </div>

    <div class="section-head"><div><h2>可选：S3 兼容存储</h2><p>只在模型要求「参考图必须是公网地址」时使用，其余素材始终留在本机</p></div></div>
    <ElForm label-position="top" class="settings-form">
      <ElFormItem label="端点（Endpoint）"><ElInput v-model="form.endpoint" placeholder="https://s3.example.com" clearable /></ElFormItem>
      <ElFormItem label="区域（Region）"><ElInput v-model="form.region" placeholder="us-east-1" /></ElFormItem>
      <ElFormItem label="存储桶"><ElInput v-model="form.bucket" /></ElFormItem>
      <ElFormItem label="Access Key"><ElInput v-model="form.accessKey" /></ElFormItem>
      <ElFormItem label="Secret Key">
        <ElRadioGroup v-model="secretAction" size="small">
          <ElRadioButton value="keep">保留原值{{ secretMask ? `（${secretMask}）` : "" }}</ElRadioButton>
          <ElRadioButton value="replace">替换</ElRadioButton>
          <ElRadioButton value="clear">清除</ElRadioButton>
        </ElRadioGroup>
        <ElInput v-if="secretAction === 'replace'" v-model="form.secretKey" type="password" show-password style="margin-top: 8px" />
      </ElFormItem>
      <ElFormItem label="寻址方式">
        <ElSwitch v-model="form.pathStyle" active-text="路径式（MinIO 等）" inactive-text="虚拟主机式（阿里云 OSS、火山 TOS 等）" />
      </ElFormItem>
      <ElFormItem label="对象前缀"><ElInput v-model="form.prefix" /></ElFormItem>
      <ElFormItem label="签名地址有效期（小时）">
        <ElInputNumber v-model="form.signTTLHours" :min="1" :max="168" />
        <small class="settings-help">上游可能排队一阵才来取图，不要设得太短。</small>
      </ElFormItem>
      <div class="storage-actions">
        <ElButton :loading="testing" @click="test">测试连接</ElButton>
        <ElButton type="primary" :loading="saving" @click="save">保存</ElButton>
      </div>
      <ul v-if="testResult" class="storage-steps">
        <li v-for="s in testResult.steps" :key="s.step" :class="{ bad: !s.ok }">
          {{ s.ok ? "✓" : "✗" }} {{ s.step }}<span v-if="s.message">：{{ s.message }}</span>
        </li>
      </ul>
    </ElForm>
  </div>
</template>

<style scoped>
.storage-usage { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 12px; margin-top: 12px; }
.storage-usage div { display: flex; flex-direction: column; gap: 4px; padding: 14px; border: 1px solid var(--portal-line); border-radius: 10px; background: var(--portal-surface); }
.storage-usage strong { font-size: 20px; }
.storage-usage small { color: var(--portal-faint); }
.settings-form { max-width: 640px; margin-top: 12px; }
.settings-help { display: block; margin-top: 4px; color: var(--portal-faint); font-size: 12px; }
.storage-actions { display: flex; gap: 8px; }
.storage-steps { margin: 12px 0 0; padding: 0; list-style: none; font-size: 13px; color: var(--portal-success); }
.storage-steps li.bad { color: var(--portal-danger); }
</style>
