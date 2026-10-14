import { afterEach, describe, expect, it } from "vitest";
import {
  createInvocationEnv,
  MEMORY_TAG,
  tagConsole,
  tagLine,
} from "../../functions/nodejs-runtime/pool-env";

const restores: Array<() => void> = [];
afterEach(() => {
  while (restores.length) restores.pop()!();
});

const tick = () => new Promise((resolve) => setImmediate(resolve));

describe("createInvocationEnv", () => {
  it("resolves process.env against the running invocation", async () => {
    const env = createInvocationEnv({ SHARED: "base", ONLY_BASE: "yes" });
    restores.push(env.install());

    const seen: Record<string, string | undefined> = {};
    await Promise.all([
      env.run({ requestID: "a", env: { SHARED: "from-a" } }, async () => {
        await tick();
        seen.a = process.env.SHARED;
        seen.aBase = process.env.ONLY_BASE;
      }),
      env.run({ requestID: "b", env: { SHARED: "from-b" } }, async () => {
        await tick();
        await tick();
        seen.b = process.env.SHARED;
      }),
    ]);
    expect(seen).toEqual({ a: "from-a", aBase: "yes", b: "from-b" });
    expect(process.env.SHARED).toBe("base");
  });

  it("keeps writes inside their invocation and merges keys", async () => {
    const env = createInvocationEnv({ A: "1" });
    restores.push(env.install());

    await env.run({ requestID: "r", env: { B: "2" } }, async () => {
      process.env.C = "3";
      expect("B" in process.env).toBe(true);
      expect(Object.keys(process.env).sort()).toEqual(["A", "B", "C"]);
      expect({ ...process.env }).toEqual({ A: "1", B: "2", C: "3" });
      delete process.env.B;
      expect(process.env.B).toBeUndefined();
    });
    expect(process.env.C).toBeUndefined();
    expect("C" in process.env).toBe(false);
    expect(process.env.A).toBe("1");
  });

  it("writes outside an invocation land in the base env", () => {
    const env = createInvocationEnv({});
    restores.push(env.install());
    process.env.LATE = "x";
    expect(env.baseEnv.LATE).toBe("x");
    expect(env.current()).toBeUndefined();
  });
});

describe("tagConsole", () => {
  it("tags console lines with the request id only inside an invocation", async () => {
    const env = createInvocationEnv({});
    const lines: string[] = [];
    const original = console.log;
    console.log = (...args: unknown[]) => {
      lines.push(args.map(String).join(" "));
    };
    restores.push(() => {
      console.log = original;
    });
    restores.push(tagConsole(env));

    console.log("outside");
    await env.run({ requestID: "req-1", env: {} }, async () => {
      console.log("one", { two: 2 });
      console.log("multi\nline");
    });
    expect(lines).toEqual([
      "outside",
      tagLine("req-1", 'one {"two":2}'),
      [tagLine("req-1", "multi"), tagLine("req-1", "line")].join("\n"),
    ]);
    expect(tagLine(MEMORY_TAG, "{}")).toBe("\u001esst.memory\u001e{}");
  });
});
