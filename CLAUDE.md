## Layout

- `platform/` — TS Pulumi components embedded via `//go:embed` (`platform/platform.go:16`)
- `examples/` — sample apps
- `cmd/sst/` — Go CLI entry, orchestrates everything
- `cmd/sst/mosaic/` — live dev TUI
- `pkg/server/` — JSON-RPC bridge for custom Pulumi dynamic providers (`rpc/rpc.ts` ↔ `pkg/server`)
- `pkg/bus/` — pub/sub event bus
- `sdk/js/` — runtime SDK for reading linked resources
- `www/` — docs site (auto-generated from JSDoc comments in platform and extracted from the Go CLI)

## Commands

- **Setup**: `bun run setup`
- **Build platform**: `bun run build:platform`
- **Build CLI**: `bun run build:cli`
- **Run CLI locally**: `cd examples/<app> && go run ../../cmd/sst <command>`
- **Go tests**: `bun run test:cli`
- **Typecheck**: `bun run typecheck`
- **Generate docs**: `bun run docs:generate`
- **Run docs locally**: `bun run docs:dev`

## Verification

1. Build the platform
2. `cd examples/<app> && bun install`
3. `go run ../../cmd/sst deploy`
4. Verify with `curl` or AWS CLI
5. Don't clean up unless told to
6. `sst dev --mode=basic` for dev mode

## Releasing `@intelligems/sst-ion`

One release is 9 npm packages: 8 binary packages (`@intelligems/sst-ion-<os>-<cpu>`) plus the SDK, published last. Versions are `<upstream>-ig.<n>` and come from the git tag (`sdk/js/scripts/release.ts`).

Never publish locally with one 2FA code (`npm_config_otp=… bun run release`). A code lasts about 30 seconds and each upload takes about 10, so it expires partway through. That has left half-published releases twice (ig.1 and ig.2).

### Default: CI with trusted publishing (no tokens, no 2FA codes)

1. Commit, then `git tag v<version>-ig.<n>` and `git push intelligems <branch> v<version>-ig.<n>`.
2. The tag push runs `.github/workflows/release.yml`, which builds, runs tests, and publishes all 9 packages over OIDC.
3. Check: `npm view @intelligems/sst-ion dist-tags --prefer-online`, then do a clean install in a temp dir and run `sst version`.

Requirements (one-time setup):
- On npmjs.com, add a trusted publisher to each of the 9 packages: GitHub Actions, repo `intelligems-tech/SSTv4`, workflow `release.yml`.
- The workflow needs `permissions: id-token: write` and npm CLI ≥ 11.5.1 (`npm install -g npm@latest` before publishing). Do not set `NODE_AUTH_TOKEN`.

Use trusted publishing, not an `NPM_TOKEN` secret: npm is removing direct publishing with granular tokens in January 2027.

### Fallback: local publish, one 2FA code per package

Use this only when CI can't run.

1. `bun run build:platform`, then `goreleaser release --skip=publish,validate --clean` (the tag must be on HEAD). Check: `dist/sst_darwin_arm64_v8.0/sst version`, run from a directory with no local SST install.
2. Pack each package to a `.tgz` instead of running `bun run release`:
   - each binary package needs `bin/<sst or sst.exe>` plus a `package.json` with `name`, `version`, `os` and `cpu`;
   - the SDK needs `dist/`, `bin/sst.mjs`, `README.md`, and `optionalDependencies` set to all 8 binary packages at the release version;
   - never ship `sdk/js/bin/native/`; the SDK tarball should be about 30 KB.
3. Have the human publish with a fresh code per file, SDK last, so `latest` only moves once every binary is up:
   ```bash
   for f in <binary tgzs…> intelligems-sst-ion-<version>.tgz; do
     read "otp?$f code: "; npm publish "$f" --access public --tag latest --otp="$otp" || break
   done
   ```
4. npm answers `202` and the version can take minutes to show up. Before retrying a package, check the npm debug log for `PUT 202`; a version cannot be published twice.
