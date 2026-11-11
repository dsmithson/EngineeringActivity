[CmdletBinding()]
param(
    [string]$Image = 'engineering-activity:test',
    [switch]$SkipBuild,
    [switch]$BrowserTests
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
$containerName = 'activity-phase1-' + [guid]::NewGuid().ToString('N').Substring(0, 12)
$created = $false

function Invoke-Docker {
    param([string[]]$Arguments)
    $output = & docker @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "docker $($Arguments[0]) failed with exit code $LASTEXITCODE"
    }
    return $output
}

Push-Location $repoRoot
try {
    Invoke-Docker -Arguments @('info', '--format', '{{.OSType}}') | Out-Null
    if (-not $SkipBuild) {
        Invoke-Docker -Arguments @('build', '--target', 'web', '-t', $Image, '.') | Out-Host
    }
    Invoke-Docker -Arguments @('run', '-d', '--name', $containerName,
        '-p', '127.0.0.1::8080', '--read-only', '--tmpfs', '/tmp',
        '-e', 'DEMO_MODE=true', '-e', 'ALLOW_UNAUTHENTICATED_DEMO=true', $Image) | Out-Null
    $created = $true
    $binding = (Invoke-Docker -Arguments @('port', $containerName, '8080/tcp') | Select-Object -First 1).Trim()
    if ($binding -notmatch '^127\.0\.0\.1:\d+$') {
        throw "Unexpected port binding: $binding"
    }
    $baseURL = "http://$binding"
    $deadline = [DateTime]::UtcNow.AddSeconds(60)
    $ready = $false
    do {
        try {
            $response = Invoke-WebRequest "$baseURL/healthz" -TimeoutSec 2
            $ready = $response.StatusCode -eq 200
        } catch {
            Start-Sleep -Milliseconds 500
        }
    } until ($ready -or [DateTime]::UtcNow -gt $deadline)
    if (-not $ready) { throw 'Container did not become ready within 60 seconds' }

    $page = Invoke-WebRequest "$baseURL/" -TimeoutSec 5
    if ($page.Content -notmatch 'Development, around the clock') { throw 'Dashboard HTML missing' }
    foreach ($asset in @('/app.js', '/style.css', '/readyz')) {
        $response = Invoke-WebRequest "$baseURL$asset" -TimeoutSec 5
        if ($response.StatusCode -ne 200) { throw "Asset/endpoint failed: $asset" }
    }
    $all = Invoke-RestMethod "$baseURL/api/activity" -TimeoutSec 5
    if (-not $all.demo -or $all.buckets.Count -ne 168 -or $all.identities.Count -lt 2) {
        throw 'Demo must contain explicitly labeled sample data, 168 buckets, and multiple contributors'
    }
    $userID = [Uri]::EscapeDataString($all.identities[0].id)
    $filtered = Invoke-RestMethod "$baseURL/api/activity?user=$userID" -TimeoutSec 5
    $allCommits = ($all.buckets | Measure-Object -Property commits -Sum).Sum
    $userCommits = ($filtered.buckets | Measure-Object -Property commits -Sum).Sum
    if ($userCommits -le 0 -or $userCommits -ge $allCommits) { throw 'Contributor filter did not narrow sample activity' }
    for ($i = 1; $i -lt $all.buckets.Count; $i++) {
        $delta = [DateTimeOffset]::Parse($all.buckets[$i].at) - [DateTimeOffset]::Parse($all.buckets[$i - 1].at)
        if ($delta.TotalHours -ne 1) { throw "Non-consecutive hourly bucket at $i" }
    }
    $runtimeUID = (Invoke-Docker -Arguments @('exec', $containerName, 'id', '-u')).Trim()
    if ($runtimeUID -eq '0') { throw 'Container runs as root' }
    Invoke-Docker -Arguments @('exec', $containerName, 'dashboard', 'healthcheck') | Out-Host

    $health = ''
    $deadline = [DateTime]::UtcNow.AddSeconds(60)
    do {
        $health = (Invoke-Docker -Arguments @('inspect', '--format', '{{.State.Health.Status}}', $containerName)).Trim()
        if ($health -eq 'healthy') { break }
        Start-Sleep -Milliseconds 500
    } until ([DateTime]::UtcNow -gt $deadline)
    if ($health -ne 'healthy') { throw "Docker health status was $health" }

    if ($BrowserTests) {
        if (-not (Test-Path 'node_modules/@playwright/test/cli.js')) {
            throw 'Install browser-test dependencies first: npm ci, then npm run test:e2e:install'
        }
        $previousBaseURL = $env:PLAYWRIGHT_BASE_URL
        try {
            $env:PLAYWRIGHT_BASE_URL = $baseURL
            $npm = if (Get-Command npm.cmd -ErrorAction SilentlyContinue) { 'npm.cmd' } else { 'npm' }
            & $npm run test:e2e
            if ($LASTEXITCODE -ne 0) { throw 'Playwright failed; inspect playwright-report and test-results' }
        } finally {
            $env:PLAYWRIGHT_BASE_URL = $previousBaseURL
        }
    }

    Invoke-Docker -Arguments @('stop', '--time', '15', $containerName) | Out-Null
    $exitCode = (Invoke-Docker -Arguments @('inspect', '--format', '{{.State.ExitCode}}', $containerName)).Trim()
    if ($exitCode -ne '0') { throw "Container did not shut down cleanly: exit $exitCode" }
    Write-Host "PASS: embedded UI/assets, 168 hourly buckets, contributor filter ($allCommits -> $userCommits), UID $runtimeUID, Docker health, and graceful stop."
    if ($BrowserTests) {
        Write-Host 'PASS: Playwright desktop/mobile checks. Review screenshots in playwright-report for visual acceptance.'
    } else {
        Write-Host 'Browser layout and interactive timezone/CSV checks remain separate acceptance checks; add -BrowserTests to run Playwright.'
    }
} catch {
    if ($created) { & docker logs $containerName }
    throw
} finally {
    if ($created) { & docker rm -f $containerName | Out-Null }
    Pop-Location
}
