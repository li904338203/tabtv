import { describe, expect, it } from "vitest";
import {
  channelCodeAvailable,
  isValidAuthAccount,
  normalizeAuthAccount,
  passwordIssue,
  registrationNeedsCode
} from "./auth";

describe("user authentication helpers", () => {
  it("normalizes email and mainland phone accounts", () => {
    expect(normalizeAuthAccount(" User@Example.COM ", "email")).toBe("user@example.com");
    expect(normalizeAuthAccount("+86 138-0013-8000", "phone")).toBe("13800138000");
  });

  it("validates the selected account channel", () => {
    expect(isValidAuthAccount("user@example.com", "email")).toBe(true);
    expect(isValidAuthAccount("13800138000", "phone")).toBe(true);
    expect(isValidAuthAccount("user@example.com", "phone")).toBe(false);
  });

  it("uses the server registration verification policy", () => {
    const policy = { emailVerificationRequired: false, phoneVerificationRequired: true };
    expect(registrationNeedsCode(policy, "email")).toBe(false);
    expect(registrationNeedsCode(policy, "phone")).toBe(true);
  });

  it("does not claim a code channel is available until settings load", () => {
    expect(channelCodeAvailable(null, "email")).toBe(false);
    expect(channelCodeAvailable({ emailCodeAvailable: true }, "email")).toBe(true);
  });

  it("checks password length and confirmation", () => {
    expect(passwordIssue("short", "short")).toContain("8");
    expect(passwordIssue("password-1", "password-2")).toContain("不一致");
    expect(passwordIssue("password-1", "password-1")).toBe("");
  });
});
