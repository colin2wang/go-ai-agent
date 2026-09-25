# ============================================================================
# build.ps1 - One-shot build: produce a single self-contained binary
#             cmd/harness -> dist/harness.exe  (subcommands: serve / mq / cron / CLI)
#
# Pipeline:
#   1. go test                      (optional, -SkipTests)
#   2. swag init                    (regenerate OpenAPI docs into api/)
#   3. pnpm install && pnpm build   (Vue3 + TS frontend, in web/)
#   4. copy web/dist                -> internal/api/webdist (picked up by go:embed)
#   5. go build                     (embeds frontend + docs, injects version)
#   6. copy config/ and assets/     -> dist/ (runtime files, editable after build)
#
# Usage:
#   .\build.ps1                     # full build, version = "dev"
#   .\build.ps1 -SkipTests          # skip `go test`
#   .\build.ps1 -Version v1.0.0     # inject version string (binary version)
# ============================================================================
param(
    [string]$Version = "dev",
    [switch]$SkipTests
)

# Fail fast on any error, and always resolve paths relative to this script.
$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

# Output directory and the entrypoints to build (cmd/<name> -> dist/<name>.exe).
# One binary ships every mode: web/API is the default, mq/cron are subcommands.
$OutputDir = Join-Path $PSScriptRoot "dist"
$Commands = @("harness")

# ---- Step 1: run unit tests (skipped with -SkipTests) ----
if (-not $SkipTests) {
    Write-Host "==> go test ./..." -ForegroundColor Cyan
    go test ./...
    if ($LASTEXITCODE -ne 0) { Write-Error "tests failed, aborting build" }
}

# ---- Step 2: version injection (applied at link time via -X main.version) ----
# -s -w strips the symbol table and DWARF data to shrink the binary.
$LdFlags = "-s -w -X main.version=$Version"

# ---- Step 3: regenerate OpenAPI docs from swaggo annotations ----
# Scans cmd/harness/main.go and internal packages, writes api/ (docs.go + swagger.json/yaml).
Write-Host "==> swag init (OpenAPI docs -> api/)" -ForegroundColor Cyan
go run github.com/swaggo/swag/cmd/swag@latest init -g cmd/harness/main.go -d . --parseInternal -o api
if ($LASTEXITCODE -ne 0) { Write-Error "swag init failed" }

# ---- Step 4: build frontend (Vue3 + TypeScript) and stage it for go:embed ----
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
    Write-Host "==> embedded web/dist -> internal/api/webdist" -ForegroundColor Cyan
}

# ---- Step 5: compile the entrypoint (frontend + docs are embedded) ----
# Drop binaries that this script no longer produces (e.g. the previous api.exe /
# mq.exe / cron.exe layout). A stale exe left in dist/ is easy to launch by
# mistake, and it serves the frontend baked in at ITS build time - which looks
# like "the web build was skipped".
if (Test-Path $OutputDir) {
    Get-ChildItem $OutputDir -Filter *.exe | Where-Object { $Commands -notcontains $_.BaseName } | ForEach-Object {
        $stale = $_ # catch 里 $_ 会变成错误记录，先留住文件名
        try {
            Remove-Item $stale.FullName -Force
            Write-Host "==> removed stale $($stale.Name) (not built by this script anymore)" -ForegroundColor Yellow
        } catch {
            Write-Warning "$($stale.Name) is in use - stop that process, then delete it manually"
        }
    }
}

Write-Host "==> go build -o dist/ (harness)" -ForegroundColor Cyan
New-Item -ItemType Directory -Force -Path $OutputDir | Out-Null
foreach ($cmd in $Commands) {
    $binPath = Join-Path $OutputDir "$cmd.exe"
    go build -ldflags $LdFlags -o $binPath ("./cmd/" + $cmd)
    if ($LASTEXITCODE -ne 0) { Write-Error "build failed: $cmd" }
}

# ---- Step 6: copy runtime files next to the binary ----
# config/   : config.yml + ai.yml + features/*.yml (users edit these without rebuilding)
# assets/   : optional extra assets (e.g. external templates), copied only if present
# Contents are copied (not the folder itself) so repeated runs never nest dist/config/config.
foreach ($dir in @("config", "assets")) {
    $src = Join-Path $PSScriptRoot $dir
    if (Test-Path $src) {
        $dst = Join-Path $OutputDir $dir
        New-Item -ItemType Directory -Force -Path $dst | Out-Null
        Copy-Item (Join-Path $src "*") -Destination $dst -Recurse -Force
        Write-Host "==> copied $dir/ -> dist/$dir/" -ForegroundColor Cyan
    }
}

# ---- Result summary ----
foreach ($cmd in $Commands) {
    $bin = Get-Item (Join-Path $OutputDir "$cmd.exe")
    $kb = [math]::Round($bin.Length / 1KB, 1)
    Write-Host "==> Done: $($bin.FullName) (${kb} KB, version=$Version)" -ForegroundColor Green
}
