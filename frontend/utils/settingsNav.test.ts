import { describe, expect, it } from "vitest";
import { SETTINGS_SECTIONS, activeSettingsSection, formatBytes } from "./settingsNav";

describe("设置页分区", () => {
  it("路径与子路径都归到对应分区", () => {
    expect(activeSettingsSection("/settings/models")).toBe("models");
    expect(activeSettingsSection("/settings/models/advanced")).toBe("models");
    expect(activeSettingsSection("/settings/modelsx")).toBe("");
  });
  it("分区 key 与路径唯一", () => {
    expect(new Set(SETTINGS_SECTIONS.map(s => s.key)).size).toBe(SETTINGS_SECTIONS.length);
    expect(new Set(SETTINGS_SECTIONS.map(s => s.path)).size).toBe(SETTINGS_SECTIONS.length);
  });
});

describe("formatBytes", () => {
  it("按 1024 进位，保留一位小数", () => {
    expect(formatBytes(0)).toBe("0 B");
    expect(formatBytes(512)).toBe("512 B");
    expect(formatBytes(1536)).toBe("1.5 KB");
    expect(formatBytes(5 * 1024 * 1024 * 1024)).toBe("5.0 GB");
    expect(formatBytes(null)).toBe("0 B");
  });
});
