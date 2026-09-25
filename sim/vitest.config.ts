import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    include: ["test/**/*.test.ts"],
    // Builds and starts the real fleet-server for the fleet integration tests.
    globalSetup: ["test/support/server.globalSetup.ts"],
    testTimeout: 20_000,
  },
});
