import { AsyncLocalStorage } from "node:async_hooks";

/**
 * Per-invocation context for a pooled worker.
 *
 * One worker runs several invocations at once, each for a possibly different
 * function. Every invocation carries its own environment (the deployed
 * function's env vars) and its own request id. Both live in AsyncLocalStorage,
 * and `process.env` is replaced with a proxy that resolves against the current
 * invocation first, so handler code that reads `process.env.X` keeps working
 * unchanged while two functions with different env run side by side.
 */
export interface InvocationStore {
  requestID: string;
  env: Record<string, string | undefined>;
}

/**
 * Console lines are prefixed with the request they belong to, using the ASCII
 * record separator, which never appears in normal logs. The dev loop splits
 * the worker's stdout back out by this tag.
 */
export const TAG = "\u001e";
export const MEMORY_TAG = "sst.memory";

export function tagLine(requestID: string, line: string): string {
  return `${TAG}${requestID}${TAG}${line}`;
}

/**
 * The keys of the worker's process environment that came from one of the
 * functions it serves (the seed), listed by the dev loop.
 */
export const SEED_KEYS_VAR = "SST_POOL_SEED_KEYS";

/**
 * Builds the `process.env` stand-in for a pooled worker from three layers:
 *
 * - `workerEnv`: the worker's own environment, the process env minus the seed
 *   function's keys, plus writes made outside any invocation (module scope).
 * - `baseEnv`: what code outside any invocation reads. It starts as the full
 *   process env, then holds `workerEnv` plus the latest invocation's env,
 *   replaced on every request by `setLatest`, never merged.
 * - the invocation's own env, read first while an invocation runs.
 *
 * An invocation falls back to `workerEnv` only, so it never sees keys that
 * belong to another function: neither the seed's nor an earlier request's.
 */
export function createInvocationEnv(
  seed: NodeJS.ProcessEnv = process.env,
  seedKeys: string[] = (seed[SEED_KEYS_VAR] ?? "").split(",").filter(Boolean),
) {
  const invocation = new AsyncLocalStorage<InvocationStore>();
  const workerEnv: Record<string, string | undefined> = { ...seed };
  for (const key of [...seedKeys, SEED_KEYS_VAR]) delete workerEnv[key];
  const baseEnv: Record<string, string | undefined> = { ...seed };

  const fallback = () => (invocation.getStore() ? workerEnv : baseEnv);

  const proxy = new Proxy(baseEnv, {
    get(_, key) {
      if (typeof key !== "string") return undefined;
      const store = invocation.getStore();
      if (store && key in store.env) return store.env[key];
      return fallback()[key];
    },
    set(_, key, value) {
      if (typeof key !== "string") return false;
      const next = value === undefined ? undefined : String(value);
      const store = invocation.getStore();
      if (store) {
        store.env[key] = next;
        return true;
      }
      workerEnv[key] = next;
      baseEnv[key] = next;
      return true;
    },
    has(_, key) {
      if (typeof key !== "string") return false;
      const store = invocation.getStore();
      return (store !== undefined && key in store.env) || key in fallback();
    },
    deleteProperty(_, key) {
      if (typeof key !== "string") return false;
      const store = invocation.getStore();
      if (store) {
        delete store.env[key];
        return true;
      }
      delete workerEnv[key];
      delete baseEnv[key];
      return true;
    },
    ownKeys() {
      const store = invocation.getStore();
      return [
        ...new Set([
          ...Object.keys(fallback()),
          ...(store ? Object.keys(store.env) : []),
        ]),
      ];
    },
    getOwnPropertyDescriptor(_, key) {
      if (typeof key !== "string") return undefined;
      const store = invocation.getStore();
      const inStore = store !== undefined && key in store.env;
      const base = fallback();
      if (!inStore && !(key in base)) return undefined;
      return {
        value: inStore ? store!.env[key] : base[key],
        writable: true,
        enumerable: true,
        configurable: true,
      };
    },
  }) as NodeJS.ProcessEnv;

  return {
    /** The proxy that stands in for `process.env`. */
    proxy,
    /** The environment read when no invocation is running. */
    baseEnv,
    /** Makes `invocationEnv` the latest one outside any invocation. */
    setLatest(invocationEnv: Record<string, string | undefined>) {
      for (const key of Object.keys(baseEnv)) delete baseEnv[key];
      Object.assign(baseEnv, workerEnv, invocationEnv);
    },
    /** Runs `fn` with `store` as the current invocation. */
    run<T>(store: InvocationStore, fn: () => T): T {
      return invocation.run(store, fn);
    },
    /** The invocation running on the current async path, if any. */
    current(): InvocationStore | undefined {
      return invocation.getStore();
    },
    /** Replaces `process.env` with the proxy and returns a restore function. */
    install(): () => void {
      const previous = process.env;
      process.env = proxy;
      return () => {
        process.env = previous;
      };
    },
  };
}

export type InvocationEnv = ReturnType<typeof createInvocationEnv>;

/**
 * Tags console output with the current invocation so interleaved logs of a
 * shared worker can be attributed. Returns a restore function.
 */
export function tagConsole(env: InvocationEnv): () => void {
  const methods = ["log", "info", "warn", "error", "debug", "trace"] as const;
  const originals = Object.fromEntries(
    methods.map((method) => [method, console[method].bind(console)]),
  ) as Record<(typeof methods)[number], (...args: unknown[]) => void>;
  for (const method of methods) {
    console[method] = (...args: unknown[]) => {
      const store = env.current();
      if (!store) return originals[method](...args);
      const text = args
        .map((a) => (typeof a === "string" ? a : safeFormat(a)))
        .join(" ");
      return originals[method](
        text
          .split("\n")
          .map((line) => tagLine(store.requestID, line))
          .join("\n"),
      );
    };
  }
  return () => {
    for (const method of methods) console[method] = originals[method];
  };
}

function safeFormat(value: unknown): string {
  if (value instanceof Error) return value.stack ?? value.message;
  if (typeof value === "object" && value !== null) {
    try {
      return JSON.stringify(value);
    } catch {
      return String(value);
    }
  }
  return String(value);
}
