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

  it("does not leak an earlier invocation's keys into a later one", async () => {
    const env = createInvocationEnv({ SST_POOL: "bundle" });
    restores.push(env.install());

    const a = { HANDLER_FN: "x" };
    env.setLatest(a);
    await env.run({ requestID: "a", env: a }, async () => {
      expect(process.env.HANDLER_FN).toBe("x");
    });

    const b = {};
    env.setLatest(b);
    await env.run({ requestID: "b", env: b }, async () => {
      expect(process.env.HANDLER_FN).toBeUndefined();
      expect("HANDLER_FN" in process.env).toBe(false);
      expect(Object.keys(process.env)).not.toContain("HANDLER_FN");
      expect({ ...process.env }).toEqual({ SST_POOL: "bundle" });
    });
    expect(process.env.HANDLER_FN).toBeUndefined();
    expect("HANDLER_FN" in process.env).toBe(false);
  });

  it("hides the seed function's keys from invocations", async () => {
    const env = createInvocationEnv({
      HANDLER_FN: "seed",
      SST_POOL: "bundle",
      SST_POOL_SEED_KEYS: "HANDLER_FN",
    });
    restores.push(env.install());

    // Module scope still sees the seed before the first request.
    expect(process.env.HANDLER_FN).toBe("seed");
    await env.run({ requestID: "r", env: { IG_POOL: "1gb" } }, async () => {
      expect(process.env.HANDLER_FN).toBeUndefined();
      expect("HANDLER_FN" in process.env).toBe(false);
      expect(process.env.SST_POOL).toBe("bundle");
      expect(process.env.SST_POOL_SEED_KEYS).toBeUndefined();
      expect(Object.keys(process.env).sort()).toEqual(["IG_POOL", "SST_POOL"]);
    });
    env.setLatest({ IG_POOL: "1gb" });
    expect(process.env.HANDLER_FN).toBeUndefined();
  });

  it("keeps module-scope writes visible to invocations", async () => {
    const env = createInvocationEnv({ SST_POOL_SEED_KEYS: "" });
    restores.push(env.install());

    process.env.LATE = "x";
    await env.run({ requestID: "r", env: {} }, async () => {
      expect(process.env.LATE).toBe("x");
    });
    env.setLatest({});
    expect(process.env.LATE).toBe("x");

    delete process.env.LATE;
    await env.run({ requestID: "s", env: {} }, async () => {
      expect("LATE" in process.env).toBe(false);
    });
  });

  it("replaces the latest env outside an invocation", () => {
    const env = createInvocationEnv({ W: "w" });
    restores.push(env.install());

    env.setLatest({ A: "1" });
    expect(process.env.A).toBe("1");
    env.setLatest({ B: "2" });
    expect(process.env.A).toBeUndefined();
    expect({ ...process.env }).toEqual({ W: "w", B: "2" });
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
