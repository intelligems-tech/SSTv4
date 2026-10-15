import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    // Pulumi's runtime uses trace events, which worker threads lack
    // (ERR_TRACE_EVENTS_UNAVAILABLE), so each test file runs in a process.
    pool: "forks",
  },
});
