/// <reference path="./.sst/platform/config.d.ts" />

// Smoke fixture for the pooled dev worker. Three functions share one prebuilt
// bundle; in `sst dev` they run on one pool of local workers and each sees its
// own HANDLER_FN. See smoke.sh.
export default $config({
  app(input) {
    return {
      name: "ig-pool-smoke",
      removal: "remove",
      protect: false,
      home: "aws",
    };
  },
  async run() {
    const functions = Object.fromEntries(
      ["a", "b", "c"].map((id) => [
        id,
        new sst.aws.Function(`Fn${id.toUpperCase()}`, {
          handler: "index.handler",
          bundle: ".bundle",
          pool: "bundle",
          runtime: "nodejs22.x",
          timeout: "30 seconds",
          memory: "256 MB",
          environment: { HANDLER_FN: id, RESOURCE_ENV: "deployed" },
        }),
      ]),
    );
    return {
      a: functions.a.name,
      b: functions.b.name,
      c: functions.c.name,
    };
  },
});
