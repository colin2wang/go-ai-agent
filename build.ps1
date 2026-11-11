# ============================================================================
# build.ps1 - One-shot build: produce a single self-contained desktop binary
#             root main package -> build/bin/harness.exe  (Wails desktop app + mq / cron / CLI subcommands)
#
# Pipeline:
#   1. go test                      (opt-in with -Test)
#   2. pnpm install && pnpm build   (Vue3 + TS frontend, in web/)
#   3. copy web/dist                -> internal/api/webdist (picked up by go:embed, served by Wails)
#   4. wails build (或回退 go build)  -> build/bin/harness.exe
#   5. copy config/ and assets/     -> build/bin/ (runtime files, editable after build)
#
# Usage:
#   .\build.ps1                     # full build (no tests), version = "dev"
#   .\build.ps1 -Test               # run `go test` before building
#   .\build.ps1 -Version v1.0.0     # inject version string (binary version)
#   .\build.ps1 -Help               # show this help and exit
# ============================================================================
param(
    [string]$Version = "dev",
    [switch]$Test,
    [switch]$Help
)

if ($Help) {
    Write-Host @"
build.ps1 - one-shot build: root main package -> build/bin/harness.exe (Wails desktop app)

Usage:
  .\build.ps1 [-Version <ver>] [-Test] [-Help]

Options:
  -Version <ver>  inject version string into the binary (default: "dev")
  -Test           run 'go test ./...' before building (skipped by default)
  -Help           show this help and exit

Pipeline:
  1. go test                      (opt-in with -Test)
  2. pnpm install && pnpm build   (Vue3 + TS frontend, in web/)
  3. copy web/dist                -> internal/api/webdist (go:embed, Wails asset server)
  4. wails build (or fallback go build) -> build/bin/harness.exe
  5. copy config/ and assets/     -> build/bin/ (runtime files, editable after build)
"@ -ForegroundColor Cyan
    exit 0
}

# Fail fast on any error, and always resolve paths relative to this script.
$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

# Output directory (Wails standard layout: build/bin).
$OutputDir = Join-Path $PSScriptRoot "build/bin"

# ---- Step 1: run unit tests (only with -Test) ----
if ($Test) {
    Write-Host "==> go test ./..." -ForegroundColor Cyan
    go test ./...
    if ($LASTEXITCODE -ne 0) { Write-Error "tests failed, aborting build" }
}

# ---- Step 2: version injection (applied at link time via -X main.version) ----
# -s -w strips the symbol table and DWARF data to shrink the binary;
# -H windowsgui keeps the GUI binary from allocating a console window.
# desktop,production tags switch Wails into embedded-asset production mode.
$LdFlags = "-s -w -X main.version=$Version -H windowsgui"
$GoTags = "desktop,production"

# wails CLI 可用时走 wails build（自动嵌入 build/appicon.png、icon.ico 与 manifest），
# 否则回退 go build（无图标/清单嵌入，但同样无控制台）。
$wailsCli = Get-Command wails -ErrorAction SilentlyContinue

# ---- Step 3: build frontend (Vue3 + TypeScript) and stage it for go:embed ----
# Skipped entirely when the web/ directory does not exist.
if (Test-Path (Join-Path $PSScriptRoot "web")) {
    Write-Host "==> pnpm install && pnpm build (web)" -ForegroundColor Cyan
    Push-Location (Join-Path $PSScriptRoot "web")
    try {
        # Always sync dependencies first: package.json may gain new deps while
        # node_modules already exists, and skipping install then breaks vue-tsc.
        # pnpm refuses to run dependency install scripts unless they are approved
        # in web/pnpm-workspace.yaml (allowBuilds), and exits non-zero otherwise.
        pnpm install
        if ($LASTEXITCODE -ne 0) {
            Write-Error @"
pnpm install failed.
  If the output contains 'ERR_PNPM_IGNORED_BUILDS' / 'Ignored build scripts: <pkg>',
  approve that package in web/pnpm-workspace.yaml, e.g.
      allowBuilds:
        <pkg>: true
  (or run 'pnpm approve-builds' inside web/), then rerun .\build.ps1
"@
        }
        pnpm run build
        if ($LASTEXITCODE -ne 0) { Write-Error "frontend build failed" }
    } finally { Pop-Location }

    # Mirror the build output into internal/api/webdist, the directory baked into
    # the binary by `//go:embed all:webdist` (see internal/api/static.go).
    $feDist = Join-Path $PSScriptRoot "web/dist"
    $embedDir = Join-Path $PSScriptRoot "internal/api/webdist"
    # Clear previous assets but keep index.html so the embed dir is never empty
    # (go:embed fails to compile when the pattern matches no files).
    New-Item -ItemType Directory -Force -Path $embedDir | Out-Null
    Get-ChildItem -Force -Path $embedDir -Exclude index.html | Remove-Item -Recurse -Force
    Copy-Item (Join-Path $feDist "*") -Destination $embedDir -Recurse -Force
    # Ensure the site icon is baked in: vite copies web/public/favicon.svg to
    # web/dist root, but if that step was ever skipped, fall back to the source
    # so the embedded index.html's <link rel="icon" href="/favicon.svg"> resolves.
    if (-not (Test-Path (Join-Path $embedDir "favicon.svg"))) {
        $iconSrc = Join-Path $PSScriptRoot "web/public/favicon.svg"
        if (Test-Path $iconSrc) { Copy-Item $iconSrc -Destination (Join-Path $embedDir "favicon.svg") -Force }
    }
    Write-Host "==> embedded web/dist -> internal/api/webdist" -ForegroundColor Cyan
}

# ---- Step 4: compile the entrypoint (frontend assets are embedded) ----
# Drop stale exes from previous layouts (dist/, old build outputs). A stale exe
# is easy to launch by mistake and serves assets baked in at ITS build time.
$repoRoot = $PSScriptRoot
foreach ($staleDir in @((Join-Path $repoRoot "dist"), $OutputDir)) {
    if (Test-Path $staleDir) {
        Get-ChildItem $staleDir -Filter *.exe | ForEach-Object {
            $stale = $_ # catch 里 $_ 会变成错误记录，先留住文件名
            try {
                Remove-Item $stale.FullName -Force
                Write-Host "==> removed stale $($stale.FullName)" -ForegroundColor Yellow
            } catch {
                Write-Warning "$($stale.Name) is in use - stop that process, then delete it manually"
            }
        }
    }
}

if ($wailsCli) {
    Write-Host "==> wails build (icon/manifest embedded, output: build/bin)" -ForegroundColor Cyan
    wails build -clean -tags $GoTags -ldflags $LdFlags
    if ($LASTEXITCODE -ne 0) { Write-Error "wails build failed" }
    $binPath = Join-Path $PSScriptRoot "build/bin/harness.exe"
} else {
    Write-Host "==> wails CLI not found, falling back to go build (install wails to embed icon & manifest)" -ForegroundColor Yellow
    Write-Host "==> go build -tags $GoTags -o build/bin/ (harness)" -ForegroundColor Cyan
    New-Item -ItemType Directory -Force -Path (Join-Path $PSScriptRoot "build/bin") | Out-Null
    $binPath = Join-Path $PSScriptRoot "build/bin/harness.exe"
    go build -tags $GoTags -ldflags $LdFlags -o $binPath .
    if ($LASTEXITCODE -ne 0) { Write-Error "build failed: harness" }
}

# ---- Step 5: copy runtime files next to the binary ----
# config/   : config.yml + ai.yml + features/*.yml (users edit these without rebuilding)
# assets/   : optional extra assets (e.g. external templates), copied only if present
# Contents are copied (not the folder itself) so repeated runs never nest build/bin/config/config.
foreach ($dir in @("config", "assets")) {
    $src = Join-Path $PSScriptRoot $dir
    if (Test-Path $src) {
        $dst = Join-Path $OutputDir $dir
        New-Item -ItemType Directory -Force -Path $dst | Out-Null
        Copy-Item (Join-Path $src "*") -Destination $dst -Recurse -Force
        Write-Host "==> copied $dir/ -> build/bin/$dir/" -ForegroundColor Cyan
    }
}

# ---- Result summary ----
$bin = Get-Item $binPath
$kb = [math]::Round($bin.Length / 1KB, 1)
Write-Host "==> Done: $($bin.FullName) (${kb} KB, version=$Version)" -ForegroundColor Green
