[CmdletBinding()]
param([switch]$SkipBuild, [switch]$BrowserTests)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
# Everything this script creates is disposable and scoped to this unique project.
$project = 'activity-acceptance-' + [guid]::NewGuid().ToString('N').Substring(0, 12)
$work = Join-Path $repoRoot ".cache/$project"
$composeArgs = @('compose', '--project-name', $project, '--env-file', "$work/test.env", '-f', "$repoRoot/compose.yaml", '-f', "$work/override.yaml")
$savedEnv = @{}
$started = $false

function Invoke-Compose {
    param([string[]]$Arguments, [switch]$AllowFailure)
    $output = & docker @composeArgs @Arguments 2>&1
    $code = $LASTEXITCODE
    if ($AllowFailure) { return @{ Code = $code; Output = $output } }
    if ($code -ne 0) {
        # Do not dump resolved config or environment: both can contain credentials.
        $output | Out-Host
        throw "Compose $($Arguments[0]) failed (exit $code)"
    }
    return $output
}
function Get-SQL {
    param([string]$SQL, [string]$Database = 'activity')
    $result = Invoke-Compose -Arguments @('exec', '-T', 'postgres', 'psql', '-U', 'activity', '-d', $Database, '-v', 'ON_ERROR_STOP=1', '-qAtc', $SQL)
    return ($result -join "`n").Trim()
}
function Assert-SQL {
    param([string]$SQL, [string]$Expected, [string]$Database = 'activity')
    $actual = Get-SQL -SQL $SQL -Database $Database
    if ($actual -ne $Expected) { throw "SQL assertion failed (expected '$Expected', got '$actual'): $SQL" }
}
function Get-FixtureCounts {
    Get-SQL "SELECT (SELECT count(*) FROM commits) || '/' || (SELECT count(*) FROM commit_files) || '/' || (SELECT count(*) FROM pull_requests) || '/' || (SELECT count(*) FROM task_events) || '/' || (SELECT count(*) FROM identities WHERE kind = 'fixture')"
}
function Set-TestRoot {
    param([string[]]$Includes)
    $entries = @('<include file="production/db.changelog-master.xml" relativeToChangelogFile="true"/>')
    foreach ($file in $Includes) { $entries += "<include file=`"$file`" relativeToChangelogFile=`"true`"/>" }
    @"
<?xml version="1.0" encoding="UTF-8"?>
<databaseChangeLog xmlns="http://www.liquibase.org/xml/ns/dbchangelog"
 xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
 xsi:schemaLocation="http://www.liquibase.org/xml/ns/dbchangelog https://www.liquibase.org/xml/ns/dbchangelog/dbchangelog-4.33.xsd">
$($entries -join "`n")
</databaseChangeLog>
"@ | Set-Content "$work/changelog/db.changelog-master.xml" -Encoding utf8
}
function Start-Stack {
    Invoke-Compose -Arguments @('up', '-d', '--wait', '--wait-timeout', '120', 'web') | Out-Null
}

Push-Location $repoRoot
try {
    New-Item -ItemType Directory -Force "$work/changelog/production" | Out-Null
    # Shell values override --env-file in Compose. Temporarily isolate all interpolated inputs.
    $variables = [regex]::Matches((Get-Content compose.yaml -Raw), '\$\{([A-Za-z_][A-Za-z0-9_]*)') | ForEach-Object { $_.Groups[1].Value } | Select-Object -Unique
    foreach ($name in $variables) {
        $savedEnv[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
        Remove-Item "Env:$name" -ErrorAction SilentlyContinue
    }
    @"
POSTGRES_PASSWORD=$([guid]::NewGuid().ToString('N'))
WEB_PORT=0
METRICS_PORT=0
LIQUIBASE_CONTEXTS=dev-seed
"@ | Set-Content "$work/test.env" -Encoding utf8
    $mount = "$work/changelog".Replace('\', '/')
    @"
services:
  liquibase:
    volumes:
      - type: bind
        source: '$mount'
        target: /liquibase/changelog
        read_only: true
"@ | Set-Content "$work/override.yaml" -Encoding utf8
    Copy-Item db/changelog/* "$work/changelog/production" -Recurse
    Copy-Item tests/fixtures/liquibase/*.sql "$work/changelog"
    # Expected changeset counts come from the changelog itself, not hard-coded numbers.
    $allChangesets = (Get-ChildItem db/changelog/changes/*.sql | Select-String -Pattern '^--changeset ').Count
    $seedChangesets = (Get-ChildItem db/changelog/changes/*.sql | Select-String -Pattern '^--changeset .*context:@dev-seed').Count
    if ($seedChangesets -lt 1) { throw 'No explicit-only dev-seed changesets found' }
    # Kubernetes init containers wait for `liquibase status` to report "up to date"; a runAlways
    # changeset that applies outside the dev-seed context would block every web/collector Pod.
    $alwaysOutsideSeed = Get-ChildItem db/changelog/changes/*.sql | Select-String -Pattern '^--changeset .*runAlways:true' | Where-Object { $_.Line -notmatch 'context:@dev-seed' }
    if ($alwaysOutsideSeed) { throw "runAlways changesets outside the dev-seed context: $($alwaysOutsideSeed.Line -join '; ')" }
    $schemaChangesets = $allChangesets - $seedChangesets
    Set-TestRoot @()
    if (-not $SkipBuild) { Invoke-Compose -Arguments @('build', 'web') | Out-Host }
    $started = $true
    Start-Stack
    Assert-SQL "SELECT count(*) FROM databasechangelog" "$allChangesets"
    Assert-SQL "SELECT count(*) FROM information_schema.tables WHERE table_name IN ('app_metadata', 'commits', 'pull_requests', 'task_events', 'sync_checkpoints')" '5'
    Assert-SQL "SELECT value FROM app_metadata WHERE key = 'dataset'" 'fixture'
    Assert-SQL "SELECT count(*) > 1000 FROM commits WHERE repository_id = '00000000-0000-4000-8000-00000000f1c5'" 't'
    Assert-SQL "SELECT count(DISTINCT tool) FROM rpt_commits, unnest(ai_tools) AS tool" '3'
    Assert-SQL "SELECT count(*) FROM task_events WHERE work_item_id = 399999 AND event_type = 'completed'" '2'
    Assert-SQL "INSERT INTO app_metadata (key,value) VALUES ('acceptance-sentinel','preserved') RETURNING value" 'preserved'
    Write-Host "PASS: fresh PostgreSQL, $allChangesets changesets ($seedChangesets dev-seed), fixture seed, and healthy frontend."

    $fixtureCounts = Get-FixtureCounts
    Invoke-Compose -Arguments @('down') | Out-Null
    Start-Stack
    Assert-SQL "SELECT value FROM app_metadata WHERE key='acceptance-sentinel'" 'preserved'
    Assert-SQL "SELECT count(*) FROM databasechangelog" "$allChangesets"
    Invoke-Compose -Arguments @('run', '--rm', 'liquibase') | Out-Null
    Assert-SQL "SELECT count(*) FROM databasechangelog" "$allChangesets"
    $after = Get-FixtureCounts
    if ($after -ne $fixtureCounts) { throw "Reseed changed fixture row counts: $fixtureCounts -> $after" }
    Assert-SQL "SELECT max(committed_at) > now() - interval '2 hours' FROM commits" 't'
    Write-Host "PASS: restart preserves data; repeated update re-anchors fixtures ($fixtureCounts) without duplicating rows or changesets."

    # Context protection: an empty filter and the live context must never load fixtures.
    foreach ($case in @(@{ Name = 'activity_nofilter'; Filter = '' }, @{ Name = 'activity_live'; Filter = 'live' })) {
        Get-SQL "CREATE DATABASE $($case.Name)" | Out-Null
        Invoke-Compose -Arguments @('run', '--rm', '-e', "LIQUIBASE_COMMAND_URL=jdbc:postgresql://postgres:5432/$($case.Name)", '-e', "LIQUIBASE_COMMAND_CONTEXT_FILTER=$($case.Filter)", 'liquibase') | Out-Null
        Assert-SQL "SELECT count(*) FROM databasechangelog" "$schemaChangesets" -Database $case.Name
        Assert-SQL "SELECT (SELECT count(*) FROM commits) + (SELECT count(*) FROM identities WHERE kind = 'fixture') + (SELECT count(*) FROM app_metadata WHERE key = 'dataset')" '0' -Database $case.Name
    }
    Write-Host "PASS: no dev-seed rows without the explicit dev-seed context (empty filter and 'live')."

    Set-TestRoot @('incremental.sql')
    Invoke-Compose -Arguments @('run', '--rm', 'liquibase') | Out-Null
    Assert-SQL "SELECT count(*) FROM databasechangelog" "$($allChangesets + 1)"
    Assert-SQL "SELECT count(*) FROM information_schema.tables WHERE table_name='compose_acceptance'" '1'
    Write-Host 'PASS: new changeset applies to an existing database.'

    # Remove web before testing dependency failure: Compose does not stop an already-running web.
    Invoke-Compose -Arguments @('down') | Out-Null
    Set-TestRoot @('incremental.sql', 'failure.sql')
    $failed = Invoke-Compose -Arguments @('up', '-d', '--wait', '--wait-timeout', '120', 'web') -AllowFailure
    if ($failed.Code -eq 0) { throw 'Broken migration unexpectedly allowed startup' }
    $runningWeb = Invoke-Compose -Arguments @('ps', '--status', 'running', '-q', 'web')
    if ($runningWeb) { throw 'Web ran despite migration failure' }
    $migrationID = (Invoke-Compose -Arguments @('ps', '-aq', 'liquibase') | Select-Object -First 1)
    if (-not $migrationID) { throw 'Liquibase container missing after failure' }
    $migrationExit = & docker inspect --format '{{.State.ExitCode}}' $migrationID
    if ($LASTEXITCODE -ne 0 -or [int]$migrationExit -eq 0) { throw 'Liquibase must exit nonzero on invalid SQL' }
    Assert-SQL "SELECT count(*) FROM databasechangelog" "$($allChangesets + 1)"
    Assert-SQL "SELECT locked FROM databasechangeloglock" 'f'
    Write-Host 'PASS: deliberate migration failure exits nonzero, releases lock, and blocks frontend startup.'

    Set-TestRoot @('incremental.sql')
    Start-Stack
    Assert-SQL "SELECT value FROM app_metadata WHERE key='acceptance-sentinel'" 'preserved'
    Assert-SQL "SELECT count(*) FROM databasechangelog" "$($allChangesets + 1)"
    $binding = (Invoke-Compose -Arguments @('port', 'web', '8080') | Select-Object -First 1).Trim()
    if ($binding -notmatch '^127\.0\.0\.1:\d+$') { throw "Unexpected web port binding: $binding" }
    $baseURL = "http://$binding"
    Invoke-WebRequest "$baseURL/readyz" -TimeoutSec 10 | Out-Null
    $activity = Invoke-RestMethod "$baseURL/api/v1/activity" -TimeoutSec 20
    $commits = ($activity.buckets | Measure-Object -Property commits -Sum).Sum
    if ($activity.demo -or $activity.dataset -ne 'fixture' -or $activity.buckets.Count -ne 168 -or $commits -le 0 -or $activity.identities.Count -lt 4 -or -not $activity.collectedThrough) {
        throw 'Expected PostgreSQL-backed fixture activity with 168 hours, commits, identities, and a collection watermark'
    }
    $filtered = Invoke-RestMethod "$baseURL/api/v1/activity?user=fixture:ada" -TimeoutSec 20
    $userCommits = ($filtered.buckets | Measure-Object -Property commits -Sum).Sum
    if ($userCommits -le 0 -or $userCommits -ge $commits) { throw 'Contributor filter did not narrow database activity' }
    $from = [DateTime]::UtcNow.AddMonths(-24).ToString('yyyy-MM-dd')
    $monthly = Invoke-RestMethod "$baseURL/api/v1/activity?bucket=month&from=$from&group=fpga&tz=America/Phoenix" -TimeoutSec 20
    if ($monthly.buckets.Count -lt 24 -or ($monthly.buckets | Measure-Object -Property aiMarkedCommits -Sum).Sum -le 0) { throw 'Long-range group query failed' }
    $metricsBinding = (Invoke-Compose -Arguments @('port', 'web', '9090') | Select-Object -First 1).Trim()
    if ($metricsBinding -notmatch '^127\.0\.0\.1:\d+$') { throw "Unexpected metrics port binding: $metricsBinding" }
    $metrics = (Invoke-WebRequest "http://$metricsBinding/metrics" -TimeoutSec 10).Content
    foreach ($needle in @('activity_exporter_up 1', 'activity_sync_watermark_timestamp_seconds{source="commits",scope="FixtureRepo"}', 'activity_collection_runs_total{outcome="success"} 1')) {
        if (-not $metrics.Contains($needle)) { throw "Metrics missing: $needle" }
    }
    Write-Host "PASS: database-backed API (commits $commits -> $userCommits for one contributor; 24-month group view) and /metrics with durable collector status."

    # The live collector must refuse to write into a seeded development database.
    if (-not $SkipBuild) { Invoke-Compose -Arguments @('--profile', 'collector', 'build', 'collector') | Out-Host }
    $refused = Invoke-Compose -Arguments @('--profile', 'collector', 'run', '--rm', '-e', 'AZURE_DEVOPS_PAT=not-a-real-token',
        '-e', 'AZURE_DEVOPS_ORG=acme', '-e', 'AZURE_DEVOPS_PROJECT=Platform', '-e', 'AZURE_DEVOPS_REPOSITORIES=Widgets', 'collector') -AllowFailure
    if ($refused.Code -eq 0 -or -not (($refused.Output -join "`n") -match 'fixture')) { throw 'Collector did not refuse the fixture database' }
    Write-Host 'PASS: collector container refuses to collect live data into the dev-seed database.'
    if ($BrowserTests) {
        if (-not (Test-Path node_modules/@playwright/test/cli.js)) { throw 'Run npm ci and npm run test:e2e:install first' }
        $previousURL = $env:PLAYWRIGHT_BASE_URL
        try {
            $env:PLAYWRIGHT_BASE_URL = $baseURL
            & node node_modules/@playwright/test/cli.js test
            if ($LASTEXITCODE -ne 0) { throw 'Playwright failed; inspect playwright-report' }
        } finally { $env:PLAYWRIGHT_BASE_URL = $previousURL }
        Write-Host 'PASS: Playwright desktop/mobile checks against the PostgreSQL-backed Compose frontend.'
    }
} finally {
    try {
        if ($started) {
            if ($project -notmatch '^activity-acceptance-[0-9a-f]{12}$') { throw 'Unsafe cleanup project name' }
            # This unique acceptance project's volumes are intentionally disposable. Never use this for development restart.
            Invoke-Compose -Arguments @('down', '--volumes', '--remove-orphans') | Out-Null
        }
    } finally {
        foreach ($name in $savedEnv.Keys) { if ($null -eq $savedEnv[$name]) { Remove-Item "Env:$name" -ErrorAction SilentlyContinue } else { [Environment]::SetEnvironmentVariable($name, $savedEnv[$name], 'Process') } }
        $resolvedWork = [IO.Path]::GetFullPath($work)
        $expectedWork = [IO.Path]::GetFullPath((Join-Path $repoRoot ".cache/$project"))
        if ($resolvedWork -eq $expectedWork -and $project -match '^activity-acceptance-[0-9a-f]{12}$' -and (Test-Path -LiteralPath $resolvedWork)) {
            Remove-Item -LiteralPath $resolvedWork -Recurse -Force
        }
        Pop-Location
    }
}
