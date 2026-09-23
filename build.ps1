# ============================================================================
# build.ps1 - Build harness executable into dist/
# Usage: .\build.ps1 [-Version "v1.0.0"] [-SkipTests]
# ============================================================================
param(
    [string]$Version = "dev",
    [switch]$SkipTests
)

$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

$OutputDir = Join-Path $PSScriptRoot "dist"
$BinaryName = "harness.exe"

# ---- Tests (optional skip) ----
if (-not $SkipTests) {
    Write-Host "==> go test ./..." -ForegroundColor Cyan
    go test ./...
    if ($LASTEXITCODE -ne 0) { Write-Error "tests failed, aborting build" }
}

# ---- Version injection (takes effect via ldflags) ----
$LdFlags = "-s -w -X main.version=$Version"

# ---- Regenerate OpenAPI docs (swag) ----
Write-Host "==> swag init (OpenAPI docs)" -ForegroundColor Cyan
go run github.com/swaggo/swag/cmd/swag@latest init -g cmd/harness/main.go -d . --parseInternal -o docs
if ($LASTEXITCODE -ne 0) { Write-Error "swag init failed" }

# ---- Build frontend (vue3+ts) and embed into internal/api/webdist ----
if (Test-Path (Join-Path $PSScriptRoot "frontend")) {
    Write-Host "==> npm ci && npm run build (frontend)" -ForegroundColor Cyan
    Push-Location (Join-Path $PSScriptRoot "frontend")
    try {
        if (Test-Path node_modules) { npm run build } else { npm install --no-audit --no-fund; npm run build }
        if ($LASTEXITCODE -ne 0) { Write-Error "frontend build failed" }
    } finally { Pop-Location }

    $feDist = Join-Path $PSScriptRoot "frontend/dist"
    $embedDir = Join-Path $PSScriptRoot "internal/api/webdist"
    Remove-Item (Join-Path $embedDir "*") -Recurse -Force -Exclude index.html
    Copy-Item (Join-Path $feDist "*") -Destination $embedDir -Recurse -Force
    Write-Host "==> embedded frontend/dist -> internal/api/webdist" -ForegroundColor Cyan
}

# ---- Build ----
Write-Host "==> go build -o dist/" -ForegroundColor Cyan
New-Item -ItemType Directory -Force -Path $OutputDir | Out-Null
go build -ldflags $LdFlags -o (Join-Path $OutputDir $BinaryName) ./cmd/harness
if ($LASTEXITCODE -ne 0) { Write-Error "build failed" }

# ---- Copy runtime assets: config/ and assets/ ----
foreach ($dir in @("config", "assets")) {
    $src = Join-Path $PSScriptRoot $dir
    if (Test-Path $src) {
        Copy-Item $src -Destination (Join-Path $OutputDir $dir) -Recurse -Force
        Write-Host "==> copied $dir/ -> dist/$dir/" -ForegroundColor Cyan
    }
}

# ---- Result ----
$bin = Get-Item (Join-Path $OutputDir $BinaryName)
$kb = [math]::Round($bin.Length / 1KB, 1)
Write-Host "==> Done: $($bin.FullName) (${kb} KB, version=$Version)" -ForegroundColor Green
