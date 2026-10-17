param([ValidateSet('workspace','dev')][string]$Service = 'workspace')
$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
Push-Location -LiteralPath $repoRoot
try {
    $networkDriver = docker network inspect --format '{{.Driver}}' kind 2>$null
    if ($LASTEXITCODE -eq 0) {
        if ($networkDriver.Trim() -ne 'bridge') { throw 'The existing kind network is not a bridge. Inspect Docker networking before starting.' }
    } else {
        docker network create --driver bridge --label io.x-k8s.kind.role=network kind | Out-Null
        if ($LASTEXITCODE -ne 0) { throw 'Could not create the kind bridge. Check Docker Desktop.' }
    }
    docker compose up -d --build $Service
    if ($LASTEXITCODE -ne 0) { throw 'Could not start the Linux workspace.' }
} finally {
    Pop-Location
}
