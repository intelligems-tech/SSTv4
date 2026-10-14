import { describe, expect, it } from "vitest";
import { resolveFunctionCode } from "../../src/components/aws/helpers/function-code";

const base = {
  name: "MyFunction",
  dev: false,
  injections: [] as string[],
  copyFiles: [] as unknown[],
  hasPostbuild: false,
};

const code = {
  s3Bucket: "artifacts",
  s3Key: "backend/abc.zip",
  sourceCodeHash: "c2hh",
};

describe("resolveFunctionCode", () => {
  it("builds when nothing is set", () => {
    expect(resolveFunctionCode(base)).toEqual({ mode: "build" });
  });

  it("points at the prebuilt package outside dev", () => {
    expect(resolveFunctionCode({ ...base, code })).toEqual({
      mode: "prebuilt",
      ...code,
    });
  });

  it("keeps the dev bridge in dev when a bundle names the local directory", () => {
    expect(
      resolveFunctionCode({ ...base, dev: true, code, bundle: ".sst/dev" }),
    ).toEqual({ mode: "dev-bridge" });
    expect(resolveFunctionCode({ ...base, dev: true })).toEqual({
      mode: "dev-bridge",
    });
  });

  it("refuses code in dev without a bundle to run locally", () => {
    expect(() => resolveFunctionCode({ ...base, dev: true, code })).toThrow(
      /sets `code` without `bundle`/,
    );
  });

  it("refuses code combined with anything SST would build", () => {
    expect(() =>
      resolveFunctionCode({ ...base, code, nodejs: { minify: true } }),
    ).toThrow(/`nodejs`/);
    expect(() =>
      resolveFunctionCode({ ...base, code, python: { container: true } }),
    ).toThrow(/`python`/);
    expect(() =>
      resolveFunctionCode({ ...base, code, injections: ["console.log(1)"] }),
    ).toThrow(/`injections`/);
    expect(() =>
      resolveFunctionCode({ ...base, code, copyFiles: [{ from: "x" }] }),
    ).toThrow(/`copyFiles`/);
    expect(() =>
      resolveFunctionCode({ ...base, code, hasPostbuild: true }),
    ).toThrow(/`hook.postbuild`/);
  });

  it("refuses a package without a bucket and key", () => {
    expect(() =>
      resolveFunctionCode({ ...base, code: { s3Bucket: "", s3Key: "k" } }),
    ).toThrow(/without an `s3Bucket` and `s3Key`/);
  });
});
