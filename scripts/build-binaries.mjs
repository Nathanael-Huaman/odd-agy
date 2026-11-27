#!/usr/bin/env node

import { mkdirSync } from "node:fs";
import { spawnSync } from "node:child_process";
import path from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const pkgRoot = path.resolve(__dirname, "..");
const outDir = path.join(pkgRoot, "dist", "bin");

const allTargets = [
  { goos: "darwin", goarch: "arm64", name: "odd-agy-darwin-arm64" },
  { goos: "darwin", goarch: "amd64", name: "odd-agy-darwin-x64" },
  { goos: "linux", goarch: "amd64", name: "odd-agy-linux-x64" },
  { goos: "linux", goarch: "arm64", name: "odd-agy-linux-arm64" },
  { goos: "windows", goarch: "amd64", name: "odd-agy-win32-x64.exe" },
];

const buildAll = process.argv.includes("--all");
mkdirSync(outDir, { recursive: true });

function buildOne(outName, envOverrides = {}) {
  const outPath = path.join(outDir, outName);
  const res = spawnSync("go", ["build", "-o", outPath, "./cmd/odd-agy"], {
    cwd: pkgRoot,
    env: { ...process.env, CGO_ENABLED: "0", ...envOverrides },
    stdio: "inherit",
  });
  if (res.status !== 0) {
    process.exit(res.status ?? 1);
  }
}

if (buildAll) {
  for (const t of allTargets) {
    buildOne(t.name, { GOOS: t.goos, GOARCH: t.goarch });
  }
} else {
  const ext = process.platform === "win32" ? ".exe" : "";
  buildOne(`odd-agy${ext}`);
}
