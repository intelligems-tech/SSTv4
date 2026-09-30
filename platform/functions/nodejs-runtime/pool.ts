// Pooled local worker for `sst dev`.
//
// One process serves every function that shares a prebuilt bundle. It runs
// several invocations at once, each under its own function's environment and
// request id (see pool-env.ts), and it never rebuilds: the dev loop retires
// it once the bundle changes. Invocations arrive from the Runtime API as
// `{ event, env }` with the function named in headers.
//
// argv: <bundle>/<file>.<export> <workerID> <concurrency>
import path from "node:path";
import fs from "node:fs";
import url from "node:url";
import os from "node:os";
import type { Context as LambdaContext } from "aws-lambda";
import {
  createInvocationEnv,
  MEMORY_TAG,
  tagConsole,
  tagLine,
  type InvocationStore,
} from "./pool-env";

// Enable the V8 compile cache so later workers load the bundle faster.
try {
  const mod = await import("node:module");
  if (typeof mod.enableCompileCache === "function") {
    mod.enableCompileCache(path.join(os.tmpdir(), "sst-compile-cache"));
  }
} catch {
  // Node.js < 22.8: continue without it.
}

const handler = process.argv[2];
const workerID = process.argv[3] ?? "";
const concurrency = Math.max(1, Number(process.argv[4] ?? 1) || 1);
const debugMemory = process.env.SST_DEBUG_MEMORY === "true";
const AWS_LAMBDA_RUNTIME_API =
  `http://` + process.env.AWS_LAMBDA_RUNTIME_API! + "/2018-06-01";

const env = createInvocationEnv(process.env);
env.install();
tagConsole(env);

const parsed = path.parse(handler);
const file = [".js", ".jsx", ".mjs", ".cjs"]
  .map((ext) => path.join(parsed.dir, parsed.name + ext))
  .find((candidate) => fs.existsSync(candidate));
const handlerName = parsed.ext.substring(1);

const STREAMING_SYMBOL = Symbol.for("aws.lambda.streaming");
(global as any).awslambda = {
  streamifyResponse(fn: any) {
    fn[STREAMING_SYMBOL] = true;
    return fn;
  },
  HttpResponseStream: {
    from() {
      throw new Error("Streaming responses are not supported by pooled workers.");
    },
  },
};

// Memory reports travel on stdout as tagged lines the dev loop consumes.
let lastMemoryReport = 0;
function reportMemory(phase: "loaded" | "response", loadMs?: number) {
  if (!debugMemory) return;
  const now = Date.now();
  if (phase === "response" && now - lastMemoryReport < 5000) return;
  lastMemoryReport = now;
  const usage = process.memoryUsage();
  process.stdout.write(
    tagLine(
      MEMORY_TAG,
      JSON.stringify({
        workerID,
        phase,
        loadMs,
        rss: usage.rss,
        heapUsed: usage.heapUsed,
        heapTotal: usage.heapTotal,
        external: usage.external,
      }),
    ) + "\n",
  );
}

async function post(pathname: string, body: unknown) {
  return fetch(AWS_LAMBDA_RUNTIME_API + pathname, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
}

function errorBody(ex: any) {
  return {
    errorType: ex instanceof Error ? ex.name : "Error",
    errorMessage: ex instanceof Error ? ex.message : String(ex),
    trace: ex instanceof Error ? ex.stack?.split("\n") : undefined,
  };
}

let lastContext: LambdaContext | undefined;

async function postError(context: LambdaContext | undefined, ex: any) {
  const body = errorBody(ex);
  if (!context) {
    await post(`/runtime/init/error`, body).catch(() => {});
    return;
  }
  await post(`/runtime/invocation/${context.awsRequestId}/error`, body).catch(
    () => {},
  );
}

// Best effort: attribute to the invocation that was running most recently.
process.on("unhandledRejection", (ex) => void postError(lastContext, ex));
process.on("uncaughtException", (ex) => void postError(lastContext, ex));

let fn: any;
const loadStart = Date.now();
try {
  if (!file) throw new Error(`Could not find handler file for "${handler}"`);
  const mod = await import(url.pathToFileURL(file).href);
  fn = mod[handlerName];
  if (!fn) {
    throw new Error(
      `Function "${handlerName}" not found in "${file}". Found ${Object.keys(mod).join(", ")}`,
    );
  }
  if (fn[STREAMING_SYMBOL] === true) {
    throw new Error(
      `Handler "${handlerName}" streams its response, which pooled workers do not support. Remove pool from this function.`,
    );
  }
} catch (ex: any) {
  await postError(undefined, ex);
  process.exit(1);
}
reportMemory("loaded", Date.now() - loadStart);

// Exit if nothing has been asked of this worker for a while. The dev loop
// normally retires idle workers first; this is the backstop.
let idleTimer: NodeJS.Timeout | undefined;
function armIdleExit() {
  if (idleTimer) clearTimeout(idleTimer);
  idleTimer = setTimeout(() => process.exit(0), 1000 * 60 * 15);
}

function header(headers: Headers, name: string) {
  return headers.get(name) ?? undefined;
}

function parseJsonHeader(headers: Headers, name: string) {
  const value = headers.get(name);
  return value ? JSON.parse(value) : undefined;
}

async function runLoop() {
  while (true) {
    armIdleExit();
    let context: LambdaContext;
    let event: unknown;
    let invocationEnv: Record<string, string | undefined>;
    try {
      const result = await fetch(
        AWS_LAMBDA_RUNTIME_API + `/runtime/invocation/next`,
      );
      const body = (await result.json()) as {
        event: unknown;
        env?: Record<string, string>;
      };
      invocationEnv = { ...(body.env ?? {}) };
      event = body.event;
      const functionID = header(result.headers, "lambda-runtime-sst-function-id");
      if (functionID) invocationEnv.SST_FUNCTION_ID = functionID;
      context = {
        awsRequestId: header(result.headers, "lambda-runtime-aws-request-id") ?? "",
        invokedFunctionArn:
          header(result.headers, "lambda-runtime-invoked-function-arn") ?? "",
        getRemainingTimeInMillis: () =>
          Math.max(
            Number(result.headers.get("lambda-runtime-deadline-ms")) -
              Date.now(),
            0,
          ),
        identity: parseJsonHeader(
          result.headers,
          "lambda-runtime-cognito-identity",
        ),
        clientContext: parseJsonHeader(
          result.headers,
          "lambda-runtime-client-context",
        ),
        functionName:
          header(result.headers, "lambda-runtime-function-name") ||
          invocationEnv.AWS_LAMBDA_FUNCTION_NAME!,
        functionVersion:
          header(result.headers, "lambda-runtime-function-version") ||
          invocationEnv.AWS_LAMBDA_FUNCTION_VERSION!,
        memoryLimitInMB:
          header(result.headers, "lambda-runtime-function-memory-size") ||
          invocationEnv.AWS_LAMBDA_FUNCTION_MEMORY_SIZE!,
        logGroupName: header(result.headers, "lambda-runtime-log-group-name") ?? "",
        logStreamName:
          header(result.headers, "lambda-runtime-log-stream-name") ?? "",
        callbackWaitsForEmptyEventLoop: true,
        done() {
          throw new Error(
            "`done` on lambda Context is not implemented by SST Live Lambda Development.",
          );
        },
        fail() {
          throw new Error(
            "`fail` on lambda Context is not implemented by SST Live Lambda Development.",
          );
        },
        succeed() {
          throw new Error(
            "`succeed` on lambda Context is not implemented by SST Live Lambda Development.",
          );
        },
      };
    } catch (ex: any) {
      if (ex?.code === "UND_ERR_HEADERS_TIMEOUT") continue;
      await new Promise((resolve) => setTimeout(resolve, 200));
      continue;
    }

    // Code that snapshots process.env outside any invocation (module scope)
    // sees the environment of the latest invocation.
    Object.assign(env.baseEnv, invocationEnv);
    lastContext = context;
    (global as any)[Symbol.for("aws.lambda.runtime.requestId")] =
      context.awsRequestId;

    const store: InvocationStore = {
      requestID: context.awsRequestId,
      env: invocationEnv,
    };
    let response: unknown;
    try {
      response = await env.run(store, () => fn(event, context));
    } catch (ex: any) {
      await postError(context, ex);
      continue;
    }

    while (true) {
      try {
        await post(
          `/runtime/invocation/${context.awsRequestId}/response`,
          response,
        );
        break;
      } catch (ex) {
        console.error(ex);
        await new Promise((resolve) => setTimeout(resolve, 500));
      }
    }
    reportMemory("response");
  }
}

await Promise.all(Array.from({ length: concurrency }, () => runLoop()));
