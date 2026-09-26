import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// In dev, the console runs on Vite and proxies the server's surface to a local
// fleet-server (FLEET_SERVER, default http://localhost:8080). In production the
// built assets are embedded in fleet-server and served from the same origin.
const server = process.env.FLEET_SERVER ?? "http://localhost:8080";

export default defineConfig({
  plugins: [react()],
  define: {
    __CONSOLE_VERSION__: JSON.stringify(process.env.npm_package_version ?? "dev"),
  },
  server: {
    proxy: {
      "/ws": { target: server, ws: true },
      "/api": { target: server },
      "/healthz": { target: server },
    },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    // MapLibre alone is ~1 MB minified; the console is one embedded bundle.
    chunkSizeWarningLimit: 1600,
  },
});
