// Copies console/dist into server/internal/web/dist so a local `go build`
// embeds the console (the Docker build does the same copy itself). The
// committed .gitkeep there is left alone; everything else is replaced.
import { cpSync, readdirSync, rmSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const src = join(here, "..", "dist");
const dst = join(here, "..", "..", "server", "internal", "web", "dist");

for (const name of readdirSync(dst)) {
  if (name !== ".gitkeep") rmSync(join(dst, name), { recursive: true, force: true });
}
cpSync(src, dst, { recursive: true });
console.log(`embedded ${src} -> ${dst}`);
