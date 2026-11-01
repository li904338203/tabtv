import type { components } from '~/types/legacy';
import { createTokenRefresher, sendWithRefresh, type StoredTokens, type components as ApiComponents } from "@tabtv/api-client";

type LoginResponse = components["schemas"]["LoginResp"];
type MeResponse = components["schemas"]["MeResp"];
/**
 * ⚠️ 续期响应用**新契约**的 `TokenPair`（access + refresh 两张）。老契约的 `RefreshResp`
 *    只有 accessToken —— 老后端不轮换 refresh；新后端每续一次就作废旧 refresh，
 *    照老类型只存 access 的话，第二次续期必然 40103，用户约 30 分钟就被踢回登录页。
 */
type RefreshResponse = ApiComponents["schemas"]["TokenPair"];
type SendCodeResponse = components["schemas"]["SendCodeResp"];
type PublicAuthSettings = components["schemas"]["PublicAuthSettingResp"];
/**
 * ⚠️ 老契约的 `UserResp` 只有 id/nickname/avatar；新后端 `/auth/me`（契约 `CurrentUser`）
 *    还给**已脱敏**的 phone/email 与水印开关。账号面板与账号中心要用，补在这里。
 */
type UserSummary = components["schemas"]["UserResp"] & {
  phone?: string | null;
  email?: string | null;
  isAiWatermarkOff?: boolean;
};
/** `/credits` 的批次（新契约），账号面板按来源拆分余额用。 */
export type CreditGrantSummary = { id?: string; source?: string; amount?: number; remaining?: number; expireAt?: string | null };
/** `/membership` 历史里的一条（新契约 `Membership`），用于「已过期 N 天」。 */
export type MembershipHistoryItem = { levelCode?: string; levelName?: string; expireAt?: string; status?: string };
type MembershipSummary = NonNullable<MeResponse["membership"]>;

function required(value: string | undefined, field: string) {
  if (!value) throw new Error(`接口响应缺少 ${field}`);
  return value;
}


/**
 * 把一个账号串拆成新后端要的字段。
 *
 * ⚠️ 老后端收 `{account}` 或 `{channel:"email"|"sms", target}` 的二元组，
 * 新后端**刻意不用二元组**，要显式的 `phone` / `email`（契约里写明了理由：
 * 二元组要求调用方先判断自己填的是什么，而服务端仍要再判一次，
 * 两处判断迟早不一致）。
 *
 * ⚠️ 判据放**一处**：发码、登录、注册、重置密码四处必须用同一个判断 ——
 * 各写一份的话会出现"验证码发到了邮箱、登录却按手机号提交"这种
 * 两边都'没错'的错配，而错配的表现是「验证码错误」，指向完全错误的方向。
 */
function accountRef(v: string): { phone: string } | { email: string } {
  const t = v.trim()
  return t.includes("@") ? { email: t } : { phone: t }
}

export const useAuthStore = defineStore("auth", () => {
  const cookieOptions = { sameSite: "lax" as const, path: "/" };
  const accessToken = useCookie<string | null>("aitv_access_token", cookieOptions);
  const refreshToken = useCookie<string | null>("aitv_refresh_token", cookieOptions);
  const user = ref<UserSummary | null>(null);
  const membership = ref<MembershipSummary | null>(null);
  const creditBalance = ref(0);
  /**
   * 下面四个只服务于「拿不到就不显示」：`creditsLoaded=false` 时面板不显示积分卡，
   * 而不是显示一个 0（失败时 creditBalance 会被置 0，单看它分不出「真的没有」与「没取到」）。
   */
  const creditGrants = ref<CreditGrantSummary[] | null>(null);
  const creditsLoaded = ref(false);
  const membershipHistory = ref<MembershipHistoryItem[]>([]);
  const membershipLoaded = ref(false);
  const settings = ref<PublicAuthSettings | null>(null);

  const isAuthenticated = computed(() => Boolean(accessToken.value));

  async function login(account: string, password: string) {
    const api = useApi();
    const response = await api<LoginResponse>("/auth/login", {
      method: "POST",
      body: { ...accountRef(account), password }
    });
    applyLogin(response);
  }

  function applyLogin(response: LoginResponse) {
    // ⚠️ 先清工作区再写 token：新账号的工作区要重新拉。
    //    顺序反过来的话，中间件可能在清除之前就跑了一次 load()。
    useWorkspaceStore().clear();
    accessToken.value = required(response.accessToken, "accessToken");
    refreshToken.value = required(response.refreshToken, "refreshToken");
    // ⚠️ 新后端的登录/注册**不返回 user**（只有 token 三件套），
    //    所以这里恒为 null，由中间件随后调 `/auth/me` 补上。
    user.value = response.user ?? null;
  }

  /**
   * 公开的认证配置（哪些登录方式开着）。
   *
   * ⚠️ 新后端**没有公开的 `/auth/settings`** —— 认证配置只有管理端那条
   * （`/admin/auth-settings`，要 `system:*` 权限）。取不到时**两种方式都放开**，
   * 而不是都关掉：关掉的话登录页会变成一块什么都点不了的空白，
   * 而用户根本不知道发生了什么。真被关掉的那种方式，服务端会在提交时拒绝，
   * 那时的错误是具体的。已记入 P15-01 挂账。
   */
  async function loadSettings() {
    try {
      settings.value = await useApi()<PublicAuthSettings>("/auth/settings");
    } catch {
      settings.value = { emailEnabled: true, phoneEnabled: true } as PublicAuthSettings;
    }
    return settings.value;
  }

  async function sendCode(target: string, _channel: "email" | "phone", scene: "register" | "reset" | "login") {
    // ⚠️ `channel` 参数保留但**不再发给服务端**：渠道由账号本身决定
    //    （带 @ 就是邮箱），服务端也是这么判的。留着形参是为了不动 4 个调用点。
    // ⚠️ `scene` → `purpose`，且取值不同：reset → reset_password。
    const purpose = scene === "reset" ? "reset_password" : scene;
    return useApi()<SendCodeResponse>("/auth/code", {
      method: "POST",
      body: { ...accountRef(target), purpose }
    });
  }

  async function register(account: string, password: string, code?: string) {
    const response = await useApi()<LoginResponse>("/auth/register", {
      method: "POST",
      body: { ...accountRef(account), password, code: code || undefined }
    });
    applyLogin(response);
  }

  async function resetPassword(account: string, code: string, newPassword: string) {
    await useApi()<unknown>("/auth/password/reset", {
      method: "POST",
      // ⚠️ 字段名是 `password` 不是 `newPassword`（新后端契约）。
      body: { ...accountRef(account), code, password: newPassword }
    });
  }

  /**
   * 浏览器里 cookie 的**当前**值（别的标签页可能刚写过）；服务端渲染时就是本次请求带来的那一对。
   * ⚠️ `useCookie` 的 ref 不会跟着别的标签页变，所以这里直接读 `document.cookie`。
   */
  function readStoredTokens(): StoredTokens {
    if (!import.meta.client) return { accessToken: accessToken.value ?? null, refreshToken: refreshToken.value ?? null };
    const jar = new Map(document.cookie.split("; ").filter(Boolean).map((kv) => {
      const i = kv.indexOf("=");
      return [kv.slice(0, i), decodeURIComponent(kv.slice(i + 1))] as const;
    }));
    return { accessToken: jar.get("aitv_access_token") || null, refreshToken: jar.get("aitv_refresh_token") || null };
  }

  // 每个 store 实例一个（服务端每个请求一个）：单飞的进行中请求不能跨用户共享。
  const refresher = createTokenRefresher({
    read: () => ({ accessToken: accessToken.value ?? null, refreshToken: refreshToken.value ?? null }),
    readStored: readStoredTokens,
    write: (pair) => {
      accessToken.value = pair.accessToken;
      refreshToken.value = pair.refreshToken;
    },
    // ⚠️ 直接调，不走 useApi：useApi 遇到 401 会当场清会话，而续期失败时还要先看一眼
    //    别的标签页是不是刚续过（是的话用它的那一对，不掉线）。
    call: async (rt) => {
      const config = useRuntimeConfig();
      const response = await $fetch<RefreshResponse>(`${config.public.apiBase}/auth/refresh`, {
        method: "POST",
        body: { refreshToken: rt }
      });
      return {
        accessToken: required(response.accessToken, "accessToken"),
        refreshToken: required(response.refreshToken, "refreshToken")
      };
    }
  });

  /**
   * 续期并返回新的 access。`stale` 是失败请求当时带的 access：已经被别的请求续过的话直接用新的。
   * 规则（只续一次、两张都存、跨标签页）见 `@tabtv/api-client` 的 session.ts。
   */
  async function refreshAccessToken(stale?: string | null) {
    return refresher(stale);
  }

  /**
   * 不走 useApi 的带鉴权请求（取字节的媒体、工作区列表）也要续期：撞上 401 就续一次、重试一次。
   * `send` 用传进来的 token 发请求，返回带 `status` 的响应（原生 fetch 或 `$fetch.raw`）。
   *
   * ⚠️ 不续的话 access 一过期（15 分钟）这些地方就整片 401：画布上的产出图变成裂图，
   *    别的请求续期成功之后它们也不会自己好；工作区列表取不到则后续请求全部缺工作区头（400）。
   * ⚠️ 续不上时**原样交回那次 401**，不在这里清会话：一张图取不到不该把人踢出去，
   *    会话真的失效时下一个普通请求（useApi）会统一处置。
   */
  function withTokenRefresh<R extends { status: number }>(send: (token: string | null) => Promise<R>): Promise<R> {
    return sendWithRefresh(accessToken.value ?? null, send, refresher);
  }

  async function loadProfile() {
    // ⚠️ 老后端的 `/auth/me` **一次返回 user + membership + creditBalance 三样**，
    //    新后端只返回用户本身 —— 积分走 `/credits`、会员走 `/membership`。
    //    照老结构读的话，`response.creditBalance` 恒为 undefined，
    //    `?? 0` 把它变成 0 —— **顶栏永远显示 0 积分，而且不报错**。
    const me = await useApi()<Record<string, unknown>>("/auth/me");
    // 新后端直接返回用户字段（不再套一层 user）。
    user.value = (me.user as UserSummary | undefined) ?? ({
      id: me.id as string | undefined,
      nickname: me.nickname as string | undefined,
      avatar: me.avatar as string | undefined,
      phone: (me.phone as string | null | undefined) ?? null,
      email: (me.email as string | null | undefined) ?? null,
      // ⚠️ 严格判布尔：字段缺失时保持 undefined（= 不知道），不能当成 false（= 水印开着）
      isAiWatermarkOff: typeof me.isAiWatermarkOff === "boolean" ? me.isAiWatermarkOff : undefined,
    } as UserSummary);

    // ⚠️ 这两个**各自失败不能拖垮登录态**：取不到积分只是顶栏少个数字，
    //    而抛出去会让整个 loadProfile 失败，中间件随后把用户踢回登录页。
    await Promise.all([
      useApi()<{ balance?: number; grants?: CreditGrantSummary[] }>("/credits")
        .then(r => {
          creditBalance.value = r.balance ?? 0;
          creditGrants.value = Array.isArray(r.grants) ? r.grants : null;
          creditsLoaded.value = true;
        })
        .catch(() => { creditBalance.value = 0; creditGrants.value = null; creditsLoaded.value = false; }),
      useApi()<Record<string, unknown>>("/membership")
        .then(r => {
          membership.value = (r.current ?? r.membership ?? null) as MembershipSummary | null;
          membershipHistory.value = Array.isArray(r.history) ? (r.history as MembershipHistoryItem[]) : [];
          membershipLoaded.value = true;
        })
        .catch(() => { membership.value = null; membershipHistory.value = []; membershipLoaded.value = false; }),
    ]);
  }

  function clearSession() {
    accessToken.value = null;
    refreshToken.value = null;
    user.value = null;
    membership.value = null;
    creditBalance.value = 0;
    creditGrants.value = null;
    creditsLoaded.value = false;
    membershipHistory.value = [];
    membershipLoaded.value = false;
    // ⚠️ **工作区必须一起清**。它是 module 级状态，且 `load()` 有
    //    `if (loaded) return` 的早退 —— 不清的话，同一个标签页换账号登录之后
    //    工作区还指着**上一个账号**的，于是每个业务请求都带着一个自己不是成员的
    //    工作区 ID，后端回 **40302 NOT_WORKSPACE_MEMBER**。
    //    而那个错误看起来像"权限问题"，不像"前端状态没清干净"。
    useWorkspaceStore().clear();
  }

  /**
   * 改 AI 水印开关（`PUT /auth/profile`，204）。只发这一个字段 —— 整份回写会让两个标签页互相覆盖。
   * `off=true` 表示**关掉**水印（字段是「关闭标志」）。
   */
  async function setWatermarkOff(off: boolean) {
    await useApi()<unknown>("/auth/profile", { method: "PUT", body: { isAiWatermarkOff: off } });
    if (user.value) user.value = { ...user.value, isAiWatermarkOff: off };
  }

  async function logout() {
    try {
      await useApi()<unknown>("/auth/logout", { method: "POST" });
    } finally {
      clearSession();
    }
  }

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
    login,
    loadSettings,
    sendCode,
    register,
    resetPassword,
    refreshAccessToken,
    withTokenRefresh,
    loadProfile,
    clearSession,
    setWatermarkOff,
    logout
  };
});
