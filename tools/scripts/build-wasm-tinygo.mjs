#!/usr/bin/env node
// Builds tsc into a standalone Wasm module using TinyGo 0.43.0 with the
// repository's runtime/target overlay applied in an isolated TINYGOROOT.
// If TinyGo 0.43.0 is not installed locally, it is automatically downloaded
// from the official GitHub release into /tmp/tinygo-0.43.0-base.

import {
    execFileSync,
    spawnSync,
} from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const TINYGO_VERSION = "0.43.0";
const __dirname = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(__dirname, "../..");
const tscDir = path.join(repoRoot, "tsc");
const overlayDir = path.join(__dirname, "tinygo-overlay");

let outPath = "/tmp/tsgo-tinygo-fast.wasm";
for (let i = 2; i < process.argv.length; i++) {
    const arg = process.argv[i];
    if ((arg === "-o" || arg === "--out") && process.argv[i + 1]) {
        outPath = path.resolve(process.argv[++i]);
    }
    else if (arg.startsWith("--out=")) {
        outPath = path.resolve(arg.slice("--out=".length));
    }
}

function checkTinyGoRoot(candidateRoot) {
    if (!candidateRoot) return null;
    const bin = path.join(candidateRoot, "bin", "tinygo");
    if (!fs.existsSync(bin)) return null;
    const res = spawnSync(bin, ["version"], { encoding: "utf8" });
    if (res.status === 0 && res.stdout.includes(`tinygo version ${TINYGO_VERSION}`)) {
        return candidateRoot;
    }
    return null;
}

function findOrDownloadBaseTinyGo() {
    // 1. Check explicit env override
    const fromEnv = checkTinyGoRoot(process.env.TINYGOROOT_BASE);
    if (fromEnv) return fromEnv;

    // 2. Check common mise install location
    const miseCandidate = path.join(
        os.homedir(),
        `.local/share/mise/installs/tinygo/${TINYGO_VERSION}/tinygo`,
    );
    if (checkTinyGoRoot(miseCandidate)) return miseCandidate;

    // 3. Check tinygo on PATH
    const whichRes = spawnSync("tinygo", ["env", "TINYGOROOT"], { encoding: "utf8" });
    if (whichRes.status === 0) {
        const pathRoot = checkTinyGoRoot(whichRes.stdout.trim());
        if (pathRoot) return pathRoot;
    }

    // 4. Check cached download in /tmp
    const cacheDir = path.join(os.tmpdir(), `tinygo-${TINYGO_VERSION}-base`);
    const cachedRoot = path.join(cacheDir, "tinygo");
    if (checkTinyGoRoot(cachedRoot)) return cachedRoot;

    // 5. Download official TinyGo 0.43.0 release tarball for current OS/arch
    const platformMap = { darwin: "darwin", linux: "linux" };
    const archMap = { arm64: "arm64", x64: "amd64" };
    const plat = platformMap[os.platform()];
    const arch = archMap[os.arch()];
    if (!plat || !arch) {
        throw new Error(`Unsupported platform/arch for TinyGo auto-download: ${os.platform()}/${os.arch()}`);
    }
    const archiveName = `tinygo${TINYGO_VERSION}.${plat}-${arch}.tar.gz`;
    const url = `https://github.com/tinygo-org/tinygo/releases/download/v${TINYGO_VERSION}/${archiveName}`;
    fs.mkdirSync(cacheDir, { recursive: true });
    const tarPath = path.join(cacheDir, archiveName);
    console.log(`Downloading TinyGo ${TINYGO_VERSION} from ${url}...`);
    execFileSync("curl", ["-fSL", "--retry", "3", "-o", tarPath, url], { stdio: "inherit" });
    execFileSync("tar", ["-xzf", tarPath, "-C", cacheDir], { stdio: "inherit" });
    if (!checkTinyGoRoot(cachedRoot)) {
        throw new Error(`Failed to verify TinyGo ${TINYGO_VERSION} in ${cachedRoot}`);
    }
    return cachedRoot;
}

function buildOverlayTinyGoRoot(baseRoot) {
    const workRoot = path.join(os.tmpdir(), `tinygo-${TINYGO_VERSION}-tsgo-overlay`);
    fs.rmSync(workRoot, { recursive: true, force: true });
    fs.mkdirSync(workRoot, { recursive: true });

    // Symlink top-level immutable directories (bin, lib, pkg)
    for (const entry of fs.readdirSync(baseRoot)) {
        if (entry === "src" || entry === "targets") continue;
        fs.symlinkSync(path.join(baseRoot, entry), path.join(workRoot, entry));
    }

    // Copy targets/ and src/ (except the 900MB src/device directory, which is symlinked),
    // then copy the overlay files on top.
    fs.cpSync(path.join(baseRoot, "targets"), path.join(workRoot, "targets"), { recursive: true });
    fs.mkdirSync(path.join(workRoot, "src"), { recursive: true });
    for (const entry of fs.readdirSync(path.join(baseRoot, "src"))) {
        if (entry === "device") {
            fs.symlinkSync(path.join(baseRoot, "src", entry), path.join(workRoot, "src", entry));
        }
        else {
            fs.cpSync(path.join(baseRoot, "src", entry), path.join(workRoot, "src", entry), { recursive: true });
        }
    }
    fs.cpSync(overlayDir, workRoot, { recursive: true, force: true });

    return workRoot;
}

const baseRoot = findOrDownloadBaseTinyGo();
const overlayRoot = buildOverlayTinyGoRoot(baseRoot);
const tinygoBin = path.join(overlayRoot, "bin", "tinygo");

console.log(`Using TinyGo ${TINYGO_VERSION} (base: ${baseRoot}, overlay: ${overlayRoot})`);
console.log(`Building ${outPath}...`);

const t0 = Date.now();
execFileSync(
    tinygoBin,
    [
        "build",
        "-target=wasip1",
        "-buildmode=c-shared",
        "-opt=2",
        "-scheduler=none",
        "-gc=leaking",
        "-no-debug",
        "-o",
        outPath,
        "./cmd/tsc",
    ],
    {
        cwd: tscDir,
        env: { ...process.env, TINYGOROOT: overlayRoot },
        stdio: "inherit",
    },
);

const stat = fs.statSync(outPath);
console.log(`Built ${outPath} (${(stat.size / (1024 * 1024)).toFixed(2)} MiB) in ${((Date.now() - t0) / 1000).toFixed(1)}s`);
