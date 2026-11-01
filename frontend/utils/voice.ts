import type { components } from '~/types/legacy';
import type { JsonObject } from "~/types/canvas";

type VoiceItem = components["schemas"]["VoiceResp"];

const LANGUAGE_NAMES: Record<string, string> = {
  "zh-CN": "中文（普通话）",
  "zh-TW": "中文（台湾）",
  "en-US": "英语（美国）",
  "en-GB": "英语（英国）",
  "ja-JP": "日语"
};

export function voiceLanguageLabel(language?: string) {
  return language ? LANGUAGE_NAMES[language] ?? language : "未设置";
}

export function voiceGenderLabel(gender?: string) {
  return gender === "female" ? "女声" : gender === "male" ? "男声" : "未设置";
}

export function applyVoiceSelection(params: JsonObject, voice: VoiceItem): JsonObject {
  if (!voice.id) return { ...params };
  return {
    ...params,
    voice: voice.id,
    voiceId: voice.id,
    voiceName: voice.name,
    voiceSampleUrl: voice.sampleUrl,
    voiceLanguage: voice.language,
    voiceGender: voice.gender
  };
}
