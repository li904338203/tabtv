import { describe, expect, it } from "vitest";
import { applyVoiceSelection, voiceGenderLabel, voiceLanguageLabel } from "./voice";

describe("voice selection helpers", () => {
  it("formats language and gender values", () => {
    expect(voiceLanguageLabel("zh-CN")).toBe("中文（普通话）");
    expect(voiceLanguageLabel("fr-FR")).toBe("fr-FR");
    expect(voiceGenderLabel("male")).toBe("男声");
  });

  it("stores the stable voice id without losing existing generation params", () => {
    expect(applyVoiceSelection(
      { model: "dev_audio", text: "hello" },
      { id: "43001", name: "青涩青年", language: "zh-CN", gender: "male" }
    )).toEqual({
      model: "dev_audio",
      text: "hello",
      voice: "43001",
      voiceId: "43001",
      voiceName: "青涩青年",
      voiceSampleUrl: undefined,
      voiceLanguage: "zh-CN",
      voiceGender: "male"
    });
  });

  it("ignores a response without an id", () => {
    expect(applyVoiceSelection({ model: "dev_audio" }, { name: "unknown" }))
      .toEqual({ model: "dev_audio" });
  });
});
