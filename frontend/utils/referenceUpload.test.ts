import { describe, expect, it } from "vitest";
import { referenceMediaType, referenceUploadForm } from "./referenceUpload";

describe("referenceUpload", () => {
  it("按 MIME 判类型", () => {
    expect(referenceMediaType({ type: "image/png" })).toBe("image");
    expect(referenceMediaType({ type: "video/mp4" })).toBe("video");
    expect(referenceMediaType({ type: "audio/mpeg" })).toBe("audio");
    expect(referenceMediaType({ type: "application/pdf" })).toBeUndefined();
  });

  it("只有 file 必带；mediaType / projectId 有才带", () => {
    const file = new Blob(["x"], { type: "image/png" });
    expect([...referenceUploadForm(file).keys()]).toEqual(["file"]);
    expect([...referenceUploadForm(file, { mediaType: "image", projectId: "9" }).keys()]).toEqual(["file", "mediaType", "projectId"]);
  });
});
