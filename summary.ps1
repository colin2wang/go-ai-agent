# ============================================================================
# summary.ps1 - Generate docs/SUMMARY.md, a single summary document containing:
#   1. README.md                  (root, verbatim)
#   2. Go files                   (per-file: declared type names + line count)
#   3. go.mod                     (module info + dependency table)
#   4. web/README.md              (verbatim, if present)
#   5. web source files           (file names + line counts, excl. node_modules/dist)
#   6. web/package.json           (scripts + dependencies table)
#
# Usage:
#   .\summary.ps1                 # write docs/SUMMARY.md
# ============================================================================
param(
    [string]$OutFile = (Join-Path $PSScriptRoot "docs/SUMMARY.md")
)

$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

# Read a file as UTF-8 without BOM handling surprises (5.1-safe).
function Read-Utf8([string]$path) {
    [System.IO.File]::ReadAllText($path)
}

# Count lines in a file (trailing newline does not add a phantom line).
function Count-Lines([string]$path) {
    $text = Read-Utf8 $path
    ($text -split "`r`n|`n|`r").Count - 1
}

# Relative path from repo root, with forward slashes (markdown friendly).
function Get-RelPath([string]$path) {
    $full = [System.IO.Path]::GetFullPath($path)
    $root = [System.IO.Path]::GetFullPath($PSScriptRoot).TrimEnd('\')
    if ($full.StartsWith($root, [StringComparison]::OrdinalIgnoreCase)) {
        $full = $full.Substring($root.Length).TrimStart('\')
    }
    $full -replace '\\', '/'
}

$sb = New-Object System.Text.StringBuilder
function W([string]$line = "") { [void]$sb.AppendLine($line) }

W "# 项目摘要 (SUMMARY)"
W
W "> 本文件由 ``summary.ps1`` 自动生成，生成时间：$(Get-Date -Format 'yyyy-MM-dd HH:mm:ss')。请勿手工编辑。"
W

# ----------------------------------------------------------------------------
# 1. 根 README.md
# ----------------------------------------------------------------------------
W "## 1. README.md（根目录）"
W
$rootReadme = Join-Path $PSScriptRoot "README.md"
if (Test-Path $rootReadme) {
    W (Read-Utf8 $rootReadme).TrimEnd()
} else {
    W "_未找到 README.md_"
}
W

# ----------------------------------------------------------------------------
# 2. Go 文件：类名（type 声明）与行数
# ----------------------------------------------------------------------------
W "## 2. Go 代码（类名与行数）"
W
W "| 文件 | 声明的类型 | 类型数 | 行数 |"
W "| --- | --- | ---: | ---: |"

$goFiles = Get-ChildItem -Path $PSScriptRoot -Recurse -Filter *.go |
    Where-Object { $_.FullName -notmatch '\\(node_modules|dist|web|\.idea)\\' } |
    Sort-Object FullName

$goTotal = 0
foreach ($f in $goFiles) {
    $text = Read-Utf8 $f.FullName
    $lines = ($text -split "`r`n|`n|`r").Count - 1
    $goTotal += $lines
    # 类型声明：`type Xxx struct|interface|...`，跳过注释行
    $types = @()
    foreach ($m in [regex]::Matches($text, '(?m)^[ \t]*type\s+([A-Za-z_]\w*)')) {
        $types += $m.Groups[1].Value
    }
    $typeText = if ($types.Count) { $types -join ', ' } else { '-' }
    W "| $($f.Name) | $typeText | $($types.Count) | $lines |"
}
W
W "共 $($goFiles.Count) 个 .go 文件，合计 $goTotal 行。"
W
W "文件路径一览："
W
foreach ($f in $goFiles) {
    W "- ``$(Get-RelPath $f.FullName)``"
}
W

# ----------------------------------------------------------------------------
# 3. go.mod
# ----------------------------------------------------------------------------
W "## 3. go.mod（依赖）"
W
$goMod = Join-Path $PSScriptRoot "go.mod"
$modName = ""
$goVersion = ""
$goDeps = @()
if (Test-Path $goMod) {
    $inBlock = $false
    foreach ($ln in (Read-Utf8 $goMod) -split "`r`n|`n") {
        $t = $ln.Trim()
        if ($t.StartsWith("module ")) { $modName = $t.Substring(7).Trim(); continue }
        if ($t.StartsWith("go "))     { $goVersion = $t.Substring(3).Trim(); continue }
        if ($t -match '^require\s*\(') { $inBlock = $true; continue }
        if ($inBlock -and $t -eq ')') { $inBlock = $false; continue }
        # require 块内条目 与 单行 require xxx v1.2.3
        $line = if ($inBlock) { $t } elseif ($t.StartsWith("require ")) { $t.Substring(8).Trim() } else { continue }
        if (-not $line) { continue }
        $indirect = $line -match '//\s*indirect'
        $parts = ($line -replace '//.*$', '').Trim() -split '\s+'
        if ($parts.Count -ge 2) {
            $goDeps += [pscustomobject]@{ Path = $parts[0]; Version = $parts[1]; Indirect = $indirect }
        }
    }
    W "模块名：``$modName`` ，Go 版本：``$goVersion``"
    W
    W "| 依赖模块 | 版本 | 类型 |"
    W "| --- | --- | --- |"
    foreach ($d in ($goDeps | Sort-Object Path)) {
        $kind = if ($d.Indirect) { "indirect" } else { "direct" }
        W "| ``$($d.Path)`` | $($d.Version) | $kind |"
    }
    $direct = @($goDeps | Where-Object { -not $_.Indirect }).Count
    W
    W "共 $($goDeps.Count) 项依赖（直接 $direct，间接 $($goDeps.Count - $direct)）。"
} else {
    W "_未找到 go.mod_"
}
W

# ----------------------------------------------------------------------------
# 4. web/README.md
# ----------------------------------------------------------------------------
W "## 4. web/README.md"
W
$webReadme = Join-Path $PSScriptRoot "web/README.md"
if (Test-Path $webReadme) {
    W (Read-Utf8 $webReadme).TrimEnd()
} else {
    W "_未找到 web/README.md_"
}
W

# ----------------------------------------------------------------------------
# 5. web 源码文件与行数（排除 node_modules / dist）
# ----------------------------------------------------------------------------
W "## 5. 前端代码（文件名与行数）"
W
W "| 文件 | 行数 |"
W "| --- | ---: |"

$webFiles = @()
$webDir = Join-Path $PSScriptRoot "web"
if (Test-Path $webDir) {
    $webFiles = Get-ChildItem -Path $webDir -Recurse -File |
        Where-Object { $_.FullName -notmatch '\\(node_modules|dist)\\' -and $_.Name -ne 'pnpm-lock.yaml' -and $_.Extension -ne '.map' } |
        Sort-Object FullName
    $webTotal = 0
    foreach ($f in $webFiles) {
        $lines = Count-Lines $f.FullName
        $webTotal += $lines
        W "| $(Get-RelPath $f.FullName) | $lines |"
    }
    W
    W "共 $($webFiles.Count) 个前端文件（不含 node_modules、dist、锁文件），合计 $webTotal 行。"
} else {
    W "_未找到 web/ 目录_"
}
W

# ----------------------------------------------------------------------------
# 6. web/package.json
# ----------------------------------------------------------------------------
W "## 6. web/package.json（依赖）"
W
$pkgPath = Join-Path $webDir "package.json"
if (Test-Path $pkgPath) {
    $pkg = Read-Utf8 $pkgPath | ConvertFrom-Json
    W "包名：``$($pkg.name)`` ，版本：``$($pkg.version)``"
    W
    if ($pkg.scripts) {
        W "构建脚本："
        W
        foreach ($p in $pkg.scripts.PSObject.Properties) {
            W "- ``$($p.Name)``：``$($p.Value)``"
        }
        W
    }
    W "| 依赖包 | 版本 | 类型 |"
    W "| --- | --- | --- |"
    foreach ($section in @(@{ Obj = $pkg.dependencies; Kind = "dependencies" }, @{ Obj = $pkg.devDependencies; Kind = "devDependencies" })) {
        if (-not $section.Obj) { continue }
        foreach ($p in $section.Obj.PSObject.Properties) {
            W "| ``$($p.Name)`` | $($p.Value) | $($section.Kind) |"
        }
    }
} else {
    W "_未找到 web/package.json_"
}

# ----------------------------------------------------------------------------
# 写出 docs/SUMMARY.md（UTF-8 无 BOM）
# ----------------------------------------------------------------------------
$outDir = Split-Path $OutFile -Parent
if (-not (Test-Path $outDir)) { New-Item -ItemType Directory -Force -Path $outDir | Out-Null }
[System.IO.File]::WriteAllText($OutFile, $sb.ToString(), (New-Object System.Text.UTF8Encoding($false)))
Write-Host "==> Summary written: $(Get-RelPath $OutFile)" -ForegroundColor Green
