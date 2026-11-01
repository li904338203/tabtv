<script setup lang="ts">
import {
  ArrowRight,
  CheckCircle2,
  Eye,
  EyeOff,
  KeyRound,
  LockKeyhole,
  Mail,
  ShieldCheck,
  Smartphone
} from "@lucide/vue";
import {
  channelCodeAvailable,
  channelEnabled,
  isValidAuthAccount,
  normalizeAuthAccount,
  passwordIssue,
  registrationNeedsCode,
  type AuthChannel
} from "~/utils/auth";
import { safeRedirect } from "~/utils/safeRedirect";

type AuthMode = "login" | "register" | "reset";

const auth = useAuthStore();
const route = useRoute();
/** 登录 / 注册成功后去哪：`?redirect=` 只认站内路径（防开放重定向，见 utils/safeRedirect）。 */
const redirectTarget = computed(() => safeRedirect(route.query.redirect));
const mode = ref<AuthMode>("login");
const channel = ref<AuthChannel>("email");
const account = ref("");
const password = ref("");
const confirmation = ref("");
const code = ref("");
const error = ref("");
const notice = ref("");
const submitting = ref(false);
const sending = ref(false);
const countdown = ref(0);
const showPassword = ref(false);
const settingsLoading = ref(true);
let countdownTimer: ReturnType<typeof setInterval> | null = null;

const policy = computed(() => auth.settings);
const availableChannels = computed<AuthChannel[]>(() => (["email", "phone"] as AuthChannel[])
  .filter((item) => channelEnabled(policy.value, item)));
const needsCode = computed(() => mode.value === "reset" || (
  mode.value === "register" && registrationNeedsCode(policy.value, channel.value)
));
const canSendCode = computed(() => (
  countdown.value === 0
  && !sending.value
  && isValidAuthAccount(account.value, channel.value)
  && channelCodeAvailable(policy.value, channel.value)
));
const channelLabel = computed(() => channel.value === "email" ? "邮箱" : "手机号");
const accountPlaceholder = computed(() => channel.value === "email" ? "name@example.com" : "请输入 11 位手机号");

function resetCountdown() {
  if (countdownTimer) clearInterval(countdownTimer);
  countdownTimer = null;
  countdown.value = 0;
}

function switchMode(nextMode: AuthMode) {
  resetCountdown();
  mode.value = nextMode;
  error.value = "";
  notice.value = "";
  password.value = "";
  confirmation.value = "";
  code.value = "";
  if (nextMode !== "login" && !availableChannels.value.includes(channel.value)) {
    channel.value = availableChannels.value[0] ?? "email";
  }
}

function switchChannel(nextChannel: AuthChannel) {
  resetCountdown();
  channel.value = nextChannel;
  account.value = "";
  code.value = "";
  error.value = "";
  notice.value = "";
}

function startCountdown() {
  resetCountdown();
  countdown.value = 60;
  countdownTimer = setInterval(() => {
    countdown.value -= 1;
    if (countdown.value <= 0 && countdownTimer) {
      clearInterval(countdownTimer);
      countdownTimer = null;
    }
  }, 1000);
}

async function sendVerificationCode() {
  error.value = "";
  notice.value = "";
  if (!isValidAuthAccount(account.value, channel.value)) {
    error.value = `请输入有效的${channelLabel.value}`;
    return;
  }
  sending.value = true;
  try {
    const normalized = normalizeAuthAccount(account.value, channel.value);
    await auth.sendCode(normalized, channel.value, mode.value === "reset" ? "reset" : "register");
    account.value = normalized;
    notice.value = "验证码已发送";
    startCountdown();
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : "验证码发送失败";
  } finally {
    sending.value = false;
  }
}

async function submit() {
  error.value = "";
  notice.value = "";
  submitting.value = true;
  try {
    if (mode.value === "login") {
      await auth.login(account.value.trim(), password.value);
      await navigateTo(redirectTarget.value);
      return;
    }

    if (!isValidAuthAccount(account.value, channel.value)) throw new Error(`请输入有效的${channelLabel.value}`);
    const normalized = normalizeAuthAccount(account.value, channel.value);
    const passwordError = passwordIssue(password.value, confirmation.value);
    if (passwordError) throw new Error(passwordError);
    if (needsCode.value && !/^\d{6}$/.test(code.value)) throw new Error("请输入 6 位验证码");

    if (mode.value === "register") {
      await auth.register(normalized, password.value, needsCode.value ? code.value : undefined);
      await navigateTo(redirectTarget.value);
      return;
    }

    await auth.resetPassword(normalized, code.value, password.value);
    account.value = normalized;
    password.value = "";
    confirmation.value = "";
    code.value = "";
    mode.value = "login";
    notice.value = "密码已更新，请重新登录";
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : "操作失败";
  } finally {
    submitting.value = false;
  }
}

onMounted(async () => {
  try {
    await auth.loadSettings();
    channel.value = availableChannels.value[0] ?? "email";
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : "认证策略加载失败";
  } finally {
    settingsLoading.value = false;
  }
});

onBeforeUnmount(() => {
  resetCountdown();
});
</script>

<template>
  <main class="auth-page">
    <div class="auth-backdrop" aria-hidden="true"><img src="/media/show-green.jpg" alt=""></div>
    <section class="auth-panel">
      <header class="auth-brand-row">
        <NuxtLink to="/" class="auth-brand" aria-label="tabtv 首页"><img class="brand-mark" src="/brand/logo-mark.png" alt="" draggable="false"><strong><img class="brand-word" src="/brand/logo-word.png" alt="tabtv" draggable="false"></strong></NuxtLink>
        <span class="auth-security"><ShieldCheck :size="13" />安全认证</span>
      </header>

      <nav class="auth-mode-tabs" aria-label="认证方式">
        <button type="button" :class="{ active: mode === 'login' }" @click="switchMode('login')">登录</button>
        <button type="button" :class="{ active: mode === 'register' }" @click="switchMode('register')">注册</button>
        <button type="button" :class="{ active: mode === 'reset' }" @click="switchMode('reset')">找回密码</button>
      </nav>

      <form @submit.prevent="submit">
        <div v-if="mode !== 'login' && availableChannels.length > 1" class="auth-channel-switch">
          <button type="button" :class="{ active: channel === 'email' }" @click="switchChannel('email')"><Mail :size="14" />邮箱</button>
          <button type="button" :class="{ active: channel === 'phone' }" @click="switchChannel('phone')"><Smartphone :size="14" />手机</button>
        </div>

        <label class="auth-field">
          <span>{{ mode === 'login' ? '邮箱或手机号' : channelLabel }}</span>
          <div><Mail v-if="mode === 'login' || channel === 'email'" :size="16" /><Smartphone v-else :size="16" /><input v-model="account" :type="mode !== 'login' && channel === 'email' ? 'email' : 'text'" :inputmode="mode !== 'login' && channel === 'phone' ? 'numeric' : 'text'" :placeholder="mode === 'login' ? '邮箱或手机号' : accountPlaceholder" autocomplete="username" required></div>
        </label>

        <label v-if="mode !== 'login' && needsCode" class="auth-field">
          <span>验证码</span>
          <div class="auth-code-field"><KeyRound :size="16" /><input v-model="code" inputmode="numeric" maxlength="6" placeholder="6 位验证码" autocomplete="one-time-code" required><button type="button" :disabled="!canSendCode" @click="sendVerificationCode">{{ countdown > 0 ? `${countdown}s` : sending ? '发送中' : '发送验证码' }}</button></div>
        </label>
        <p v-if="mode !== 'login' && needsCode && !settingsLoading && !channelCodeAvailable(policy, channel)" class="auth-message error">当前{{ channelLabel }}验证码服务暂不可用</p>

        <label class="auth-field">
          <span>{{ mode === 'reset' ? '新密码' : '密码' }}</span>
          <div><LockKeyhole :size="16" /><input v-model="password" :type="showPassword ? 'text' : 'password'" :autocomplete="mode === 'login' ? 'current-password' : 'new-password'" :placeholder="mode === 'login' ? '请输入密码' : '8-72 个字符'" required><button type="button" class="auth-password-toggle" :title="showPassword ? '隐藏密码' : '显示密码'" @click="showPassword = !showPassword"><EyeOff v-if="showPassword" :size="16" /><Eye v-else :size="16" /></button></div>
        </label>

        <label v-if="mode !== 'login'" class="auth-field">
          <span>确认密码</span>
          <div><CheckCircle2 :size="16" /><input v-model="confirmation" :type="showPassword ? 'text' : 'password'" autocomplete="new-password" placeholder="再次输入密码" required></div>
        </label>

        <p v-if="error" class="auth-message error">{{ error }}</p>
        <p v-if="notice" class="auth-message success">{{ notice }}</p>

        <button class="auth-submit" type="submit" :disabled="submitting || settingsLoading">
          <span>{{ submitting ? '处理中' : mode === 'login' ? '进入创作平台' : mode === 'register' ? '创建账号' : '更新密码' }}</span><ArrowRight :size="16" />
        </button>
      </form>

      <footer class="auth-panel-footer">
        <span><ShieldCheck :size="13" />验证码 5 分钟内有效</span>
        <button v-if="mode === 'login'" type="button" @click="switchMode('reset')">忘记密码</button>
        <button v-else type="button" @click="switchMode('login')">返回登录</button>
      </footer>
    </section>
  </main>
</template>
