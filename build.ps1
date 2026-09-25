# ============================================================================
# build.ps1 - One-shot build: produce a self-contained harness.exe into dist/
#
# Pipeline:
#   1. go test                      (optional, -SkipTests)
#   2. swag init                    (regenerate OpenAPI docs from annotations)
#   3. npm run build                (Vue3 + TS frontend)
#   4. copy frontend/dist           -> internal/api/webdist (picked up by go:embed)
#   5. go build                     (embed frontend, inject version via ldflags)
#   6. copy config/ and assets/     -> dist/ (runtime files, editable after build)
#
# Usage:
#   .\build.ps1                     # full build, version = "dev"
#   .\build.ps1 -SkipTests          # skip `go test`
#   .\build.ps1 -Version v1.0.0     # inject version string (harness version)
# ============================================================================
param(
    [string]$Version = "dev",
    [switch]$SkipTests
)

# Fail fast on any error, and always resolve paths relative to this script.
$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

# Output directory and final binary name.
$OutputDir = Join-Path $PSScriptRoot "dist"
$BinaryName = "harness.exe"

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
# Scans cmd/harness/main.go and internal packages, writes docs/ (docs.go + swagger.json/yaml).
Write-Host "==> swag init (OpenAPI docs)" -ForegroundColor Cyan
go run github.com/swaggo/swag/cmd/swag@latest init -g cmd/harness/main.go -d . --parseInternal -o docs
if ($LASTEXITCODE -ne 0) { Write-Error "swag init failed" }

# ---- Step 4: build frontend (Vue3 + TypeScript) and stage it for go:embed ----
# Skipped entirely when the frontend/ directory does not exist.
if (Test-Path (Join-Path $PSScriptRoot "frontend")) {
    Write-Host "==> npm ci && npm run build (frontend)" -ForegroundColor Cyan
    Push-Location (Join-Path $PSScriptRoot "frontend")
    try {
        # Install dependencies only when node_modules is absent, then run vue-tsc + vite build.
        if (Test-Path node_modules) { npm run build } else { npm install --no-audit --no-fund; npm run build }
        if ($LASTEXITCODE -ne 0) { Write-Error "frontend build failed" }
    } finally { Pop-Location }

    # Mirror the build output into internal/api/webdist, the directory baked into
    # the binary by `//go:embed all:webdist` (see internal/api/static.go).
    $feDist = Join-Path $PSScriptRoot "frontend/dist"
    $embedDir = Join-Path $PSScriptRoot "internal/api/webdist"
    # Clear previous assets but keep index.html so the embed dir is never empty
    # (go:embed fails to compile when the pattern matches no files).
    New-Item -ItemType Directory -Force -Path $embedDir | Out-Null
    Get-ChildItem -Force -Path $embedDir -Exclude index.html | Remove-Item -Recurse -Force
    Copy-Item (Join-Path $feDist "*") -Destination $embedDir -Recurse -Force
    Write-Host "==> embedded frontend/dist -> internal/api/webdist" -ForegroundColor Cyan
}

# ---- Step 5: compile the Go binary (frontend + docs are embedded) ----
Write-Host "==> go build -o dist/" -ForegroundColor Cyan
New-Item -ItemType Directory -Force -Path $OutputDir | Out-Null
go build -ldflags $LdFlags -o (Join-Path $OutputDir $BinaryName) ./cmd/harness
if ($LASTEXITCODE -ne 0) { Write-Error "build failed" }

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
$bin = Get-Item (Join-Path $OutputDir $BinaryName)
$kb = [math]::Round($bin.Length / 1KB, 1)
Write-Host "==> Done: $($bin.FullName) (${kb} KB, version=$Version)" -ForegroundColor Green
