#!/usr/bin/env node

import { existsSync } from "node:fs";
import { spawnSync } from "node:child_process";
import path from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const pkgRoot = path.resolve(__dirname, "..");
const ext = process.platform === "win32" ? ".exe" : "";

function resolveBinary() {
  const override = process.env.ODD_AGY_BIN;
  if (override && override.trim() !== "") {
    if (!existsSync(override)) {
      console.error(`odd-agy: ODD_AGY_BIN points to missing file: ${override}`);
      process.exit(1);
    }
    return override;
  }

  const distBinDir = path.join(pkgRoot, "dist", "bin");
  const platformBin = path.join(distBinDir, `odd-agy-${process.platform}-${process.arch}${ext}`);
  if (existsSync(platformBin)) {
    return platformBin;
  }

  const localBin = path.join(distBinDir, `odd-agy${ext}`);
  if (existsSync(localBin)) {
    return localBin;
  }

  // Dev fallback: if running inside the source checkout and Go is available, build once cleanly.
  if (existsSync(path.join(pkgRoot, "go.mod"))) {
    const goCheck = spawnSync("go", ["version"], { stdio: "ignore" });
    if (goCheck.status === 0) {
      const buildScript = path.join(pkgRoot, "scripts", "build-binaries.mjs");
      const buildRes = spawnSync(process.execPath, [buildScript], {
        cwd: pkgRoot,
        stdio: ["ignore", "pipe", "inherit"],
      });
      if (buildRes.stdout && buildRes.stdout.length > 0) {
        process.stderr.write(buildRes.stdout);
      }
      if (buildRes.status === 0 && existsSync(localBin)) {
        return localBin;
      }
    }
  }

  console.error(
    `odd-agy: binary not found for ${process.platform}-${process.arch}. Run "node scripts/build-binaries.mjs" first.`
  );
  process.exit(1);
}

const binPath = resolveBinary();
const result = spawnSync(binPath, process.argv.slice(2), {
  stdio: "inherit",
});

if (result.error) {
  console.error(`odd-agy: failed to execute ${binPath}: ${result.error.message}`);
  process.exit(1);
}

if (typeof result.status === "number") {
  process.exit(result.status);
}

process.exit(1);
