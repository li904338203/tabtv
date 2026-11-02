/**
 * 本机身份（TT-T-15，dev/03 修订块 R-1）。
 *
 * tabtv 没有登录：后端每次启动给本机用户签一张令牌，并在每个请求上自动补上（desktop/local.go 的 LocalAuth）。
 * 这个 store 因此只是一个「永远已登录」的桩 —— 形状与来源系统一致（user / isAuthenticated / accessToken /
 * withTokenRefresh / loadProfile / setWatermarkOff …），29 个调用方一行不改。
 *
 * 积分、会员、登录、注册、找回密码相关的字段与方法保留空实现，只为让尚未清理的调用点编译通过；
 * 界面上的入口已删除（TT-T-13 / TT-T-14）。
 */
type UserSummary = {
  id?: string;
  nickname?: string;
  avatar?: string | null;
  phone?: string | null;
  email?: string | null;
  isAiWatermarkOff?: boolean;
};
export type CreditGrantSummary = { id?: string; source?: string; amount?: number; remaining?: number; expireAt?: string | null };
export type MembershipHistoryItem = { levelCode?: string; levelName?: string; expireAt?: string; status?: string };

/** 本机令牌的占位值：真正的令牌只在后端，前端只需要一个非空值让「已登录」判断成立。 */
const LOCAL_TOKEN = "local";

export const useAuthStore = defineStore("auth", () => {
  const accessToken = ref<string | null>(LOCAL_TOKEN);
  const refreshToken = ref<string | null>(LOCAL_TOKEN);
  const user = ref<UserSummary | null>(null);
  const membership = ref<null>(null);
  const creditBalance = ref(0);
  const creditGrants = ref<CreditGrantSummary[] | null>(null);
  const creditsLoaded = ref(false);
  const membershipHistory = ref<MembershipHistoryItem[]>([]);
  const membershipLoaded = ref(false);
  const settings = ref<null>(null);
  const isAuthenticated = computed(() => true);

  /** 来源系统按 401 续期后重发；本机令牌不过期，直接发一次。 */
  function withTokenRefresh<R extends { status: number }>(send: (token: string | null) => Promise<R>): Promise<R> {
    return send(null);
  }

  async function refreshAccessToken(_stale?: string | null) {
    return LOCAL_TOKEN;
  }

  async function loadProfile() {
    const me = await useApi()<UserSummary>("/auth/me");
    user.value = {
      ...me,
      // ⚠️ 严格判布尔：字段缺失时保持 undefined（= 不知道），不能当成 false（= 水印开着）
      isAiWatermarkOff: typeof me.isAiWatermarkOff === "boolean" ? me.isAiWatermarkOff : undefined
    };
  }

  /** 改 AI 水印开关（`PUT /auth/profile`，204）。`off=true` 表示**关掉**水印。 */
  async function setWatermarkOff(off: boolean) {
    await useApi()<unknown>("/auth/profile", { method: "PUT", body: { isAiWatermarkOff: off } });
    if (user.value) user.value = { ...user.value, isAiWatermarkOff: off };
  }

  // 本机身份不会失效：清会话只清本地缓存的资料。
  function clearSession() {
    user.value = null;
  }

  // 以下为被排除功能留下的空实现（登录 / 注册 / 找回密码 / 退出），入口已删除。
  const unsupported = async () => {
    throw new Error("tabtv 没有账号体系");
  };

  return {
    accessToken,
    refreshToken,
    user,
    membership,
    creditBalance,
    creditGrants,
    creditsLoaded,
    membershipHistory,
    membershipLoaded,
    settings,
    isAuthenticated,
    login: unsupported,
    loadSettings: async () => undefined,
    sendCode: unsupported,
    register: unsupported,
    resetPassword: unsupported,
    refreshAccessToken,
    withTokenRefresh,
    loadProfile,
    clearSession,
    setWatermarkOff,
    logout: async () => undefined
  };
});
