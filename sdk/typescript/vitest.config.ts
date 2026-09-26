import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    include: ["test/**/*.test.ts"],
    // Builds and starts the real fleet-server for the client integration tests.
    globalSetup: ["test/support/clientCoreServer.globalSetup.ts"],
  },
});
