import { VisibleError } from "../../error.js";

/**
 * A code package that already exists in S3. The function is pointed at it
 * instead of SST zipping and uploading a package of its own.
 */
export interface PrebuiltCode {
  s3Bucket: string;
  s3Key: string;
  s3ObjectVersion?: string;
  sourceCodeHash?: string;
}

export type FunctionCodeMode =
  /** `sst dev`: the Live bridge is deployed; `bundle` runs locally. */
  | { mode: "dev-bridge" }
  /** SST builds, zips and uploads the handler. */
  | { mode: "build" }
  /** The Lambda points at an existing S3 object; nothing is built or uploaded. */
  | ({ mode: "prebuilt" } & PrebuiltCode);

export interface ResolveFunctionCodeInput {
  name: string;
  dev: boolean;
  code?: PrebuiltCode;
  bundle?: string;
  nodejs?: unknown;
  python?: unknown;
  injections: string[];
  copyFiles: unknown[];
  hasPostbuild: boolean;
}

/**
 * Decides how a function gets its code. Pure, so the rules are testable
 * without a Pulumi engine.
 */
export function resolveFunctionCode(
  input: ResolveFunctionCodeInput,
): FunctionCodeMode {
  if (input.code) {
    const conflicts = [
      input.nodejs !== undefined && "nodejs",
      input.python !== undefined && "python",
      input.injections.length > 0 && "injections",
      input.copyFiles.length > 0 && "copyFiles",
      input.hasPostbuild && "hook.postbuild",
    ].filter((item): item is string => typeof item === "string");
    if (conflicts.length > 0) {
      throw new VisibleError(
        `Function "${input.name}" sets \`code\`, which cannot be combined with ${conflicts
          .map((item) => `\`${item}\``)
          .join(", ")}: the package is not built by SST.`,
      );
    }
    if (input.dev && !input.bundle) {
      throw new VisibleError(
        `Function "${input.name}" sets \`code\` without \`bundle\`; \`sst dev\` needs a local directory to run.`,
      );
    }
    if (!input.code.s3Bucket || !input.code.s3Key) {
      throw new VisibleError(
        `Function "${input.name}" sets \`code\` without an \`s3Bucket\` and \`s3Key\`.`,
      );
    }
  }
  if (input.dev) return { mode: "dev-bridge" };
  if (input.code) return { mode: "prebuilt", ...input.code };
  return { mode: "build" };
}
