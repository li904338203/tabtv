/**
 * 创造页的未登录草稿（AGT-43，设计 02 §8）：未登录时点输入框 / 发送 → 把已输入的东西存进
 * `sessionStorage('create-draft')` → 去登录 → 回到 `/create` 恢复一次、立即清掉。
 *
 * - 存正文、模式、技能、附件的 **mediaId**（不存文件本身，也不存任何 URL —— GO-27）；
 *   恢复时附件要再按工作区校验一次归属，校验不过的丢弃并提示（调用方做）。
 * - 24 小时过期：隔天再打开，一段早就不想要的旧草稿突然出现在输入框里比没有更糟。
 * - 损坏 / 形状不对的一律丢弃，不抛错（读不出草稿不该让创造页打不开）。
 */
import type { ComposerMode } from "~/utils/quickGenerate";

export const CREATE_DRAFT_KEY = "create-draft";
export const CREATE_DRAFT_TTL_MS = 24 * 60 * 60 * 1000;
/** 正文上限与消息上限一致（服务端 8000 字）。 */
const MAX_TEXT = 8000;
const MAX_MEDIA = 20;
const MODES: readonly ComposerMode[] = ["agent", "video", "image", "audio"];

export type CreateDraft = {
  mode: ComposerMode;
  text: string;
  skill?: { code: string; name: string };
  mediaIds: string[];
};

type StorageLike = Pick<Storage, "getItem" | "setItem" | "removeItem">;

export function encodeCreateDraft(draft: CreateDraft, now: number = Date.now()): string {
  return JSON.stringify({
    v: 1,
    savedAt: now,
    mode: draft.mode,
    text: draft.text.slice(0, MAX_TEXT),
    ...(draft.skill ? { skill: { code: draft.skill.code, name: draft.skill.name } } : {}),
    mediaIds: draft.mediaIds.filter(id => /^\d+$/.test(id)).slice(0, MAX_MEDIA)
  });
}

export function decodeCreateDraft(raw: string | null | undefined, now: number = Date.now()): CreateDraft | undefined {
  if (!raw) return undefined;
  let value: Record<string, unknown>;
  try {
    const parsed = JSON.parse(raw) as unknown;
    if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) return undefined;
    value = parsed as Record<string, unknown>;
  } catch {
    return undefined;
  }
  const savedAt = Number(value.savedAt);
  if (!Number.isFinite(savedAt) || now - savedAt > CREATE_DRAFT_TTL_MS || savedAt - now > 60_000) return undefined;
  if (!MODES.includes(value.mode as ComposerMode) || typeof value.text !== "string") return undefined;
  const skill = value.skill as { code?: unknown; name?: unknown } | undefined;
  const mediaIds = Array.isArray(value.mediaIds)
    ? value.mediaIds.filter((id): id is string => typeof id === "string" && /^\d+$/.test(id)).slice(0, MAX_MEDIA)
    : [];
  return {
    mode: value.mode as ComposerMode,
    text: value.text.slice(0, MAX_TEXT),
    ...(skill && typeof skill.code === "string" && skill.code && typeof skill.name === "string"
      ? { skill: { code: skill.code, name: skill.name } }
      : {}),
    mediaIds
  };
}

/** 存草稿（跳登录前调用）。写不进去（隐私模式、配额）只是回来时不恢复。 */
export function saveCreateDraft(storage: StorageLike | undefined, draft: CreateDraft, now: number = Date.now()) {
  try {
    storage?.setItem(CREATE_DRAFT_KEY, encodeCreateDraft(draft, now));
  } catch {
    // 忽略。
  }
}

/** 取出并**立即清掉**草稿（只恢复一次；过期 / 损坏的也一并清掉）。 */
export function takeCreateDraft(storage: StorageLike | undefined, now: number = Date.now()): CreateDraft | undefined {
  try {
    const raw = storage?.getItem(CREATE_DRAFT_KEY);
    if (raw === null || raw === undefined) return undefined;
    storage?.removeItem(CREATE_DRAFT_KEY);
    return decodeCreateDraft(raw, now);
  } catch {
    return undefined;
  }
}
