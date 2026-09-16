[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^\d+\.\d+\.\d+$')]
    [string]$Version
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
$utf8NoBom = New-Object System.Text.UTF8Encoding($false)

function Set-FirstMatch {
    param(
        [Parameter(Mandatory = $true)][string]$RelativePath,
        [Parameter(Mandatory = $true)][string]$Pattern,
        [Parameter(Mandatory = $true)][string]$Replacement
    )

    $path = Join-Path $repoRoot $RelativePath
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        throw "Required release metadata file was not found: $RelativePath"
    }

    $text = [IO.File]::ReadAllText($path)
    $regex = New-Object System.Text.RegularExpressions.Regex(
        $Pattern,
        [Text.RegularExpressions.RegexOptions]::Multiline
    )
    if (-not $regex.IsMatch($text)) {
        throw "Version field was not found in $RelativePath"
    }

    $updated = $regex.Replace($text, $Replacement, 1)
    [IO.File]::WriteAllText($path, $updated, $utf8NoBom)
}

Set-FirstMatch `
    -RelativePath 'main.go' `
    -Pattern '^const pluginVersion = "[^"]+"' `
    -Replacement ('const pluginVersion = "' + $Version + '"')

$jsonReplacement = '${1}"' + $Version + '"'
Set-FirstMatch `
    -RelativePath 'registry-entry.json' `
    -Pattern '^(\s*"version"\s*:\s*)"[^"]+"' `
    -Replacement $jsonReplacement
Set-FirstMatch `
    -RelativePath 'registry.json' `
    -Pattern '^(\s*"version"\s*:\s*)"[^"]+"' `
    -Replacement $jsonReplacement

$entry = Get-Content -Raw -LiteralPath (Join-Path $repoRoot 'registry-entry.json') | ConvertFrom-Json
$registry = Get-Content -Raw -LiteralPath (Join-Path $repoRoot 'registry.json') | ConvertFrom-Json
if ($entry.version -ne $Version -or $registry.plugins[0].version -ne $Version) {
    throw 'Release metadata validation failed after updating the version.'
}

Write-Host "Version metadata synchronized to $Version."
