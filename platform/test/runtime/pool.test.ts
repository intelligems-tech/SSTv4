import { afterAll, beforeAll, describe, expect, it } from "vitest";
import http from "node:http";
import { spawn, type ChildProcess } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

// Drives the pooled worker end to end against a fake Runtime API: two
// invocations for two different functions run on one worker, each sees its
// own environment, and console lines come back tagged with their request.

const here = path.dirname(fileURLToPath(import.meta.url));
const runtime = path.resolve(here, "../../functions/nodejs-runtime/pool.ts");

interface Invocation {
  requestID: string;
  functionID: string;
  env: Record<string, string>;
  event: unknown;
}

let server: http.Server;
let port: number;
let child: ChildProcess;
let bundle: string;
const queue: Invocation[] = [];
const waiting: http.ServerResponse[] = [];
const responses = new Map<string, unknown>();
const resolvers = new Map<string, (value: unknown) => void>();
let stdout = "";

function serveNext(res: http.ServerResponse, inv: Invocation) {
  res.writeHead(200, {
    "content-type": "application/json",
    "lambda-runtime-aws-request-id": inv.requestID,
    "lambda-runtime-deadline-ms": String(Date.now() + 10_000),
    "lambda-runtime-invoked-function-arn": `arn:aws:lambda:us-east-1:1:function:${inv.functionID}`,
    "lambda-runtime-sst-function-id": inv.functionID,
    "lambda-runtime-function-name": inv.functionID,
  });
  res.end(JSON.stringify({ event: inv.event, env: inv.env }));
}

function invoke(inv: Invocation) {
  const done = new Promise<unknown>((resolve) =>
    resolvers.set(inv.requestID, resolve),
  );
  const res = waiting.shift();
  if (res) serveNext(res, inv);
  else queue.push(inv);
  return done;
}

beforeAll(async () => {
  bundle = fs.mkdtempSync(path.join(os.tmpdir(), "sst-pool-test-"));
  fs.writeFileSync(
    path.join(bundle, "index.mjs"),
    [
      `export const handler = async (event, context) => {`,
      `  if (event === "boom") throw new Error("boom for " + process.env.HANDLER_FN);`,
      `  console.log("hello " + process.env.HANDLER_FN);`,
      `  await new Promise((r) => setTimeout(r, 50));`,
      `  return { fn: process.env.HANDLER_FN, id: process.env.SST_FUNCTION_ID, name: context.functionName, event };`,
      `};`,
    ].join("\n"),
  );
  server = http.createServer((req, res) => {
    const url = req.url ?? "";
    if (req.method === "GET" && url.endsWith("/runtime/invocation/next")) {
      const inv = queue.shift();
      if (inv) serveNext(res, inv);
      else waiting.push(res);
      return;
    }
    const match = url.match(/\/runtime\/invocation\/([^/]+)\/(response|error)$/);
    if (req.method === "POST" && match) {
      let body = "";
      req.on("data", (chunk) => (body += chunk));
      req.on("end", () => {
        const value = { kind: match[2], body: JSON.parse(body) };
        responses.set(match[1], value);
        resolvers.get(match[1])?.(value);
        res.writeHead(202);
        res.end();
      });
      return;
    }
    res.writeHead(404);
    res.end();
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  port = (server.address() as { port: number }).port;
  child = spawn(
    process.env.SST_BUN_PATH ?? "bun",
    [runtime, path.join(bundle, "index.handler"), "pool-1", "2"],
    {
      env: {
        ...process.env,
        AWS_LAMBDA_RUNTIME_API: `127.0.0.1:${port}`,
        SST_DEBUG_MEMORY: "true",
      },
      stdio: ["ignore", "pipe", "pipe"],
    },
  );
  child.stdout!.on("data", (chunk) => (stdout += chunk.toString()));
  child.stderr!.on("data", (chunk) => (stdout += chunk.toString()));
}, 30_000);

afterAll(() => {
  child?.kill();
  server?.close();
  fs.rmSync(bundle, { recursive: true, force: true });
});

describe("pooled worker", () => {
  it("runs invocations of different functions with their own env on one worker", async () => {
    const [a, b] = await Promise.all([
      invoke({
        requestID: "req-a",
        functionID: "FnA",
        env: { HANDLER_FN: "a", AWS_LAMBDA_FUNCTION_NAME: "fn-a" },
        event: { n: 1 },
      }),
      invoke({
        requestID: "req-b",
        functionID: "FnB",
        env: { HANDLER_FN: "b", AWS_LAMBDA_FUNCTION_NAME: "fn-b" },
        event: { n: 2 },
      }),
    ]);
    expect(a).toEqual({
      kind: "response",
      body: { fn: "a", id: "FnA", name: "FnA", event: { n: 1 } },
    });
    expect(b).toEqual({
      kind: "response",
      body: { fn: "b", id: "FnB", name: "FnB", event: { n: 2 } },
    });
    await new Promise((resolve) => setTimeout(resolve, 100));
    expect(stdout).toContain("\u001ereq-a\u001ehello a");
    expect(stdout).toContain("\u001ereq-b\u001ehello b");
    expect(stdout).toContain("\u001esst.memory\u001e");
  }, 30_000);

  it("reports a thrown error under its request and keeps serving", async () => {
    const failed = await invoke({
      requestID: "req-c",
      functionID: "FnC",
      env: { HANDLER_FN: "c" },
      event: "boom",
    });
    expect(failed).toMatchObject({
      kind: "error",
      body: { errorType: "Error", errorMessage: "boom for c" },
    });
    const after = await invoke({
      requestID: "req-d",
      functionID: "FnA",
      env: { HANDLER_FN: "a" },
      event: { n: 4 },
    });
    expect(after).toMatchObject({ kind: "response", body: { fn: "a" } });
  }, 30_000);
});
