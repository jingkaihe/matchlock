import { build } from "esbuild";
import { copyFile, rm } from "node:fs/promises";

await rm("dist", { recursive: true, force: true });

await build({
  entryPoints: ["src/main.tsx"],
  bundle: true,
  minify: true,
  sourcemap: false,
  outfile: "dist/app.js",
  format: "esm",
  target: ["es2020"],
  logLevel: "info"
});

await copyFile("index.html", "dist/index.html");
