export type AuthChannel = "email" | "phone";

export type PublicAuthPolicy = {
  emailEnabled?: boolean;
  phoneEnabled?: boolean;
  emailVerificationRequired?: boolean;
  phoneVerificationRequired?: boolean;
  emailCodeAvailable?: boolean;
  phoneCodeAvailable?: boolean;
};

const EMAIL_PATTERN = /^[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,63}$/i;
const PHONE_PATTERN = /^1[3-9]\d{9}$/;

export function normalizeAuthAccount(account: string, channel: AuthChannel) {
  const value = account.trim();
  if (channel === "email") return value.toLowerCase();
  return value.replace(/[\s-]/g, "").replace(/^\+86/, "");
}

export function isValidAuthAccount(account: string, channel: AuthChannel) {
  const normalized = normalizeAuthAccount(account, channel);
  return channel === "email" ? EMAIL_PATTERN.test(normalized) : PHONE_PATTERN.test(normalized);
}

export function channelEnabled(policy: PublicAuthPolicy | null, channel: AuthChannel) {
  if (!policy) return true;
  return channel === "email" ? policy.emailEnabled !== false : policy.phoneEnabled !== false;
}

export function registrationNeedsCode(policy: PublicAuthPolicy | null, channel: AuthChannel) {
  if (!policy) return true;
  return channel === "email"
    ? policy.emailVerificationRequired !== false
    : policy.phoneVerificationRequired !== false;
}

export function channelCodeAvailable(policy: PublicAuthPolicy | null, channel: AuthChannel) {
  if (!policy) return false;
  return channel === "email" ? policy.emailCodeAvailable === true : policy.phoneCodeAvailable === true;
}

export function passwordIssue(password: string, confirmation: string) {
  if (password.length < 8) return "密码至少需要 8 个字符";
  if (password.length > 72) return "密码不能超过 72 个字符";
  if (password !== confirmation) return "两次输入的密码不一致";
  return "";
}
