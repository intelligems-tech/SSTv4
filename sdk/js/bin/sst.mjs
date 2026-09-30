#!/usr/bin/env node
import { createRequire } from "node:module";
const require = createRequire(import.meta.url);
import path from "path";
import { execFileSync } from "child_process";
import { existsSync } from "fs";

let resolved = process.env.SST_BIN_PATH;

if (!resolved) {
  // Binary packages are published as `<package name>-<platform>-<arch>`, so a
  // renamed fork resolves its own binaries.
  const pkg = require("../package.json");
  const name = `${pkg.name}-${process.platform}-${process.arch}`;
  const binary = process.platform === "win32" ? "sst.exe" : "sst";

  try {
    resolved = require.resolve(path.join(name, "bin", binary));
  } catch (ex) {
    // A linked checkout (`pnpm link`) has no binary package; use the binary
    // built into `bin/native` by `scripts/build-local`.
    const local = path.join(import.meta.dirname, "native", binary);
    if (existsSync(local)) resolved = local;
  }
  if (!resolved) {
    console.error(
      `It seems that your package manager failed to install the right version of the SST CLI for your platform. You can try manually installing the "${name}" package.`,
    );
    process.exit(1);
  }
}

process.on("SIGINT", () => {});

try {
  execFileSync(resolved, process.argv.slice(2), {
    stdio: "inherit",
  });
} catch (ex) {
  process.exit(1);
}
