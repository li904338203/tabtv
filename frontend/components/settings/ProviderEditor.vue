<script setup lang="ts">
/**
 * 厂商新建 / 编辑（TT-T-18 高级模式；字段与校验照来源管理端 `ProviderEditor.vue`，规则在 `utils/settingsModelForm.ts`）。
 *
 * ⚠️ API Key 三态：保留 / 替换 / 清除。服务端不回显密钥（连掩码都不回），输入框永远是空的 ——
 *    回填掩码的话什么都没改点保存，掩码就会被当成新密钥存回去，症状是「什么都没动却开始 401」。
 * ⚠️ 带版本号保存：两处同时改时后保存的收到「已被修改」而不是悄悄覆盖。
 * ⚠️ 局域网地址要「常规 → 允许访问局域网上游」打开才调得通，这里提前说出来。
 */
import {
  ElAlert, ElButton, ElDialog, ElForm, ElFormItem, ElInput, ElInputNumber, ElMessage, ElMessageBox, ElRadioButton, ElRadioGroup,
  ElSwitch
} from "element-plus";
import { Plus, X } from "@lucide/vue";
import { settingsApiError } from "~/utils/settingsApi";
import {
  settingsIsPrivateAddress, settingsProviderDraft, settingsProviderPayload, settingsValidateProvider, type SettingsProviderDraft
} from "~/utils/settingsModelForm";

type Row = Record<string, unknown>;

const props = defineProps<{ open: boolean; row: Row | null; providers: Row[]; allowPrivate: boolean }>();
const emit = defineEmits<{ (e: "update:open", v: boolean): void; (e: "saved"): void }>();

const api = useApi();
const draft = reactive<SettingsProviderDraft>(settingsProviderDraft(null));
const saving = ref(false);
const editing = computed(() => props.row !== null);
const privateWarn = computed(() => !props.allowPrivate && settingsIsPrivateAddress(draft.baseUrl));

watch(() => props.open, open => {
  if (open) Object.assign(draft, settingsProviderDraft(props.row));
  // 关闭时把刚输入的密钥从内存里清掉：弹窗组件不销毁
  else Object.assign(draft, { keyValue: "", keyAk: "", keySk: "" });
});

const keyActions = computed(() => [
  ...(editing.value ? [{ value: "keep", label: draft.hasApiKey ? "保留现有 Key" : "保持未配置" }] : []),
  { value: "replace", label: editing.value ? "替换为新 Key" : "现在填写" },
  ...(editing.value && draft.hasApiKey ? [{ value: "clear", label: "清除 Key" }] : [])
]);

async function save() {
  const err = settingsValidateProvider(draft, props.providers, editing.value);
  if (err) {
    ElMessage.warning(err);
    return;
  }
  if (draft.keyAction === "clear") {
    try {
      await ElMessageBox.confirm("清除后这个厂商立刻调不通：需要认证的请求都会失败，直到重新填入 Key。", "清除 API Key",
        { confirmButtonText: "清除", cancelButtonText: "取消", type: "warning" });
    } catch {
      return;
    }
  }
  saving.value = true;
  try {
    await api("/admin/models/providers", { method: "PUT", body: settingsProviderPayload(draft) });
    Object.assign(draft, { keyValue: "", keyAk: "", keySk: "" });
    ElMessage.success(privateWarn.value ? "已保存。这是局域网地址，记得到「常规」里允许访问局域网上游" : "厂商已保存");
    emit("update:open", false);
    emit("saved");
  } catch (e) {
    ElMessage.error({ message: settingsApiError(e).message, duration: 8000 });
  } finally {
    saving.value = false;
  }
}
</script>

<template>
  <ElDialog :model-value="open" :title="editing ? '编辑厂商' : '新建厂商'" width="min(720px, 96vw)" :close-on-click-modal="false" @update:model-value="emit('update:open', $event)">
    <ElForm label-position="top">
      <div class="form-grid">
        <ElFormItem label="厂商编码" required>
          <ElInput v-model="draft.code" :disabled="editing" :maxlength="32" placeholder="my_gateway" />
          <small class="pv-help">{{ editing ? "编码是业务键，保存后不能改。" : "小写字母、数字、下划线与连字符，最长 32 位。" }}</small>
        </ElFormItem>
        <ElFormItem label="名称" required><ElInput v-model="draft.name" :maxlength="64" placeholder="我的模型网关" /></ElFormItem>
      </div>
      <ElFormItem label="地址" required>
        <ElInput v-model="draft.baseUrl" :maxlength="512" placeholder="https://api.example.com/v1" />
        <small class="pv-help">模型的提交路径拼在它后面。</small>
      </ElFormItem>
      <ElAlert v-if="privateWarn" type="warning" :closable="false" show-icon class="pv-alert">
        这是局域网 / 本机地址。默认不允许访问局域网上游，请先到
        <NuxtLink to="/settings/general" class="portal-link-button">设置 → 常规</NuxtLink>
        打开「允许访问局域网上游」（重启后生效），否则连接测试与生成都会被出网策略拒绝。
      </ElAlert>

      <section class="pv-auth">
        <ElFormItem label="认证方式" required>
          <ElRadioGroup v-model="draft.authType" size="small">
            <ElRadioButton value="bearer">Bearer 令牌</ElRadioButton>
            <ElRadioButton value="header">自定义请求头</ElRadioButton>
            <ElRadioButton value="volc_v4">火山 V4 签名</ElRadioButton>
            <ElRadioButton value="none">不认证</ElRadioButton>
          </ElRadioGroup>
        </ElFormItem>
        <template v-if="draft.authType !== 'none'">
          <ElFormItem :label="draft.authType === 'volc_v4' ? 'AccessKey / SecretKey' : 'API Key'">
            <ElRadioGroup v-model="draft.keyAction" size="small">
              <ElRadioButton v-for="a in keyActions" :key="a.value" :value="a.value">{{ a.label }}</ElRadioButton>
            </ElRadioGroup>
            <small class="pv-help">{{ draft.hasApiKey ? "已配置。Key 只保存在这台电脑的数据库里，这里不回显；不换就选「保留」。" : "尚未配置 Key。" }}</small>
          </ElFormItem>
          <div v-if="draft.keyAction === 'replace' && draft.authType === 'volc_v4'" class="form-grid">
            <ElFormItem label="新 AccessKey"><ElInput v-model="draft.keyAk" type="password" show-password autocomplete="new-password" placeholder="AKLT…" /></ElFormItem>
            <ElFormItem label="新 SecretKey"><ElInput v-model="draft.keySk" type="password" show-password autocomplete="new-password" placeholder="粘贴完整的 SecretKey" /></ElFormItem>
          </div>
          <ElFormItem v-else-if="draft.keyAction === 'replace'" label="新 Key">
            <ElInput v-model="draft.keyValue" type="password" show-password autocomplete="new-password" placeholder="粘贴完整的 API Key" />
          </ElFormItem>
          <div v-if="draft.authType === 'volc_v4'" class="form-grid">
            <ElFormItem label="地域" required><ElInput v-model="draft.volcRegion" :maxlength="64" placeholder="cn-north-1" /></ElFormItem>
            <ElFormItem label="服务" required><ElInput v-model="draft.volcService" :maxlength="64" placeholder="以接口文档为准" /></ElFormItem>
          </div>
          <p v-if="draft.authType === 'header'" class="pv-help">
            Key 默认放在 <code>X-API-Key</code> 头里；要换头名，在下面的自定义请求头里加一条 <code>X-Auth-Header-Name</code>，值写头名。
          </p>
        </template>
      </section>

      <ElFormItem label="自定义请求头">
        <div class="pv-headers">
          <div v-for="(h, i) in draft.headers" :key="i" class="pv-header-row">
            <ElInput v-model="h.name" :maxlength="128" placeholder="名称，例如 anthropic-version" />
            <ElInput v-model="h.value" :maxlength="1024" placeholder="值" />
            <ElButton link title="删除这一条" @click="draft.headers.splice(i, 1)"><X :size="14" /></ElButton>
          </div>
          <ElButton size="small" @click="draft.headers.push({ name: '', value: '' })"><Plus :size="14" />添加请求头</ElButton>
        </div>
        <small class="pv-help">请求头会原样显示在这里，不要把 Key 写在这里。</small>
      </ElFormItem>
      <div class="form-grid three">
        <ElFormItem label="连接超时（秒）" required><ElInputNumber v-model="draft.connectTimeoutSec" :min="1" :max="600" :precision="0" /></ElFormItem>
        <ElFormItem label="请求超时（秒）" required><ElInputNumber v-model="draft.requestTimeoutSec" :min="1" :max="3600" :precision="0" /></ElFormItem>
        <ElFormItem label="空闲超时（秒）" required><ElInputNumber v-model="draft.idleTimeoutSec" :min="1" :max="3600" :precision="0" /></ElFormItem>
        <ElFormItem label="最大重试"><ElInputNumber v-model="draft.maxRetries" :min="0" :max="10" :precision="0" /></ElFormItem>
        <ElFormItem label="并发上限（0 = 不限）"><ElInputNumber v-model="draft.maxConcurrent" :min="0" :precision="0" /></ElFormItem>
        <ElFormItem label="连接测试路径"><ElInput v-model="draft.testPath" :maxlength="256" placeholder="/models（留空用地址本身）" /></ElFormItem>
      </div>
      <ElFormItem label="启用">
        <ElSwitch :model-value="draft.status === 'enabled'" @update:model-value="(v: string | number | boolean) => (draft.status = v ? 'enabled' : 'disabled')" />
        <small class="pv-help">停用前要先停用它下面的全部模型。</small>
      </ElFormItem>
    </ElForm>
    <template #footer>
      <ElButton @click="emit('update:open', false)">取消</ElButton>
      <ElButton type="primary" :loading="saving" @click="save">保存</ElButton>
    </template>
  </ElDialog>
</template>

<style scoped>
.form-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 0 14px; }
.form-grid.three { grid-template-columns: repeat(3, minmax(0, 1fr)); }
.form-grid :deep(.el-input-number) { width: 100%; }
.pv-help { display: block; width: 100%; margin-top: 4px; color: var(--portal-faint); font-size: 12px; line-height: 1.5; }
.pv-alert { margin-bottom: 14px; }
.pv-auth { margin-bottom: 14px; padding: 12px 12px 2px; border: 1px solid var(--portal-line); border-radius: 8px; }
.pv-headers { display: grid; width: 100%; gap: 6px; }
.pv-header-row { display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1.4fr) max-content; gap: 6px; }
.pv-headers > .el-button { justify-self: start; }
</style>
