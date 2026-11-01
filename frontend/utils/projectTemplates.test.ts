import { describe, expect, it } from "vitest";
import { PROJECT_TEMPLATES } from "./projectTemplates";

describe("project template catalog", () => {
  it("keeps the four backend template codes in the canvas launcher order", () => {
    expect(PROJECT_TEMPLATES.map((template) => template.code)).toEqual([
      "story_script",
      "role_three_view",
      "first_frame_video",
      "audio_video"
    ]);
  });

  it("provides a visual asset and unique name for every launcher", () => {
    expect(new Set(PROJECT_TEMPLATES.map((template) => template.name)).size).toBe(4);
    expect(PROJECT_TEMPLATES.every((template) => template.cover.startsWith("/media/"))).toBe(true);
  });
});
