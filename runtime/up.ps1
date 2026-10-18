param([ValidateSet('workspace','dev')][string]$Service = 'workspace')
$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
Push-Location -LiteralPath $repoRoot
try {
    # Windows PowerShell 5.1 treats native stderr as NativeCommandError.
    # Collect it only for this probe, then restore strict failure handling
    # before creation/Compose. Only an absent kind network permits creation.
    $inspectionPreference = $ErrorActionPreference
    try {
        $ErrorActionPreference = 'Continue'
        $inspection = @(docker network inspect --format '{{.Driver}}' kind 2>&1)
        $inspectionExit = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $inspectionPreference
    }
    if ($inspectionExit -eq 0) {
        $networkDriver = ($inspection -join "`n").Trim()
        if ($networkDriver.Trim() -ne 'bridge') { throw 'The existing kind network is not a bridge. Inspect Docker networking before starting.' }
    } else {
        $inspectionText = ($inspection | ForEach-Object { $_.ToString() }) -join "`n"
        $inspectionLines = @($inspectionText -split '\r?\n' | Where-Object { $_.Trim().Length -gt 0 })
        $missingKindPattern = '^(Error response from daemon: network kind not found|(?:Error response from daemon: |Error: )?No such network: kind)\s*$'
        if ($inspectionLines.Count -eq 0 -or @($inspectionLines | Where-Object { $_ -notmatch $missingKindPattern }).Count -gt 0) {
            throw 'Could not inspect the kind bridge. Check Docker Desktop before starting.'
        }
        docker network create --driver bridge --label io.x-k8s.kind.role=network kind | Out-Null
        if ($LASTEXITCODE -ne 0) { throw 'Could not create the kind bridge. Check Docker Desktop.' }
    }
    docker compose up -d --build $Service
    if ($LASTEXITCODE -ne 0) { throw 'Could not start the Linux workspace.' }
} finally {
    Pop-Location
}
