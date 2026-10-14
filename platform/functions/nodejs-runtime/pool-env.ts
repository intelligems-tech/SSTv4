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

export function createInvocationEnv(seed: NodeJS.ProcessEnv = process.env) {
  const invocation = new AsyncLocalStorage<InvocationStore>();
  const baseEnv: Record<string, string | undefined> = { ...seed };

  const currentEnv = () => invocation.getStore()?.env ?? baseEnv;

  const proxy = new Proxy(baseEnv, {
    get(_, key) {
      if (typeof key !== "string") return undefined;
      const store = invocation.getStore();
      if (store && key in store.env) return store.env[key];
      return baseEnv[key];
    },
    set(_, key, value) {
      if (typeof key !== "string") return false;
      currentEnv()[key] = value === undefined ? undefined : String(value);
      return true;
    },
    has(_, key) {
      if (typeof key !== "string") return false;
      const store = invocation.getStore();
      return (store !== undefined && key in store.env) || key in baseEnv;
    },
    deleteProperty(_, key) {
      if (typeof key !== "string") return false;
      delete currentEnv()[key];
      return true;
    },
    ownKeys() {
      const store = invocation.getStore();
      return [
        ...new Set([
          ...Object.keys(baseEnv),
          ...(store ? Object.keys(store.env) : []),
        ]),
      ];
    },
    getOwnPropertyDescriptor(_, key) {
      if (typeof key !== "string") return undefined;
      const store = invocation.getStore();
      const inStore = store !== undefined && key in store.env;
      if (!inStore && !(key in baseEnv)) return undefined;
      return {
        value: inStore ? store!.env[key] : baseEnv[key],
        writable: true,
        enumerable: true,
        configurable: true,
      };
    },
  }) as NodeJS.ProcessEnv;

  return {
    /** The proxy that stands in for `process.env`. */
    proxy,
    /** The worker-wide environment, read when no invocation is running. */
    baseEnv,
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
