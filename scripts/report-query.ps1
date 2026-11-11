[CmdletBinding()]
param(
    # live = local live-data stack (compose.live.yaml); dev = dev-seed fixture stack (compose.yaml)
    [ValidateSet('live', 'dev')][string]$Target = 'live',
    # A SQL file containing one SELECT (no trailing semicolon required), or use -Query.
    [string]$SqlFile,
    [string]$Query,
    [ValidateSet('csv', 'json', 'table')][string]$Format = 'csv',
    # Write output here instead of stdout.
    [string]$OutFile
)
# Read-only reporting helper: runs one SELECT against the dashboard database through the
# Compose postgres container (no host psql needed) inside a READ ONLY transaction.
$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
if (($SqlFile -and $Query) -or (-not $SqlFile -and -not $Query)) { throw 'Specify exactly one of -SqlFile or -Query.' }
$sql = if ($SqlFile) { Get-Content -Raw $SqlFile } else { $Query }
$sql = $sql.Trim().TrimEnd(';')
if ($sql -match ';') { throw 'Only a single statement is allowed.' }

$composeArgs = @('compose', '-f', "$repoRoot/compose.yaml")
if ($Target -eq 'live') {
    if (-not (Test-Path "$repoRoot/.env.live")) { throw '.env.live not found; see docs/local-development.md (live-data stack).' }
    $composeArgs += @('--env-file', "$repoRoot/.env.live", '-f', "$repoRoot/compose.live.yaml")
}
switch ($Format) {
    'json'  { $statement = "SELECT coalesce(json_agg(t), '[]'::json) FROM ($sql) AS t"; $flags = @('-At') }
    'csv'   { $statement = $sql; $flags = @('--csv') }
    'table' { $statement = $sql; $flags = @() }
}
$wrapped = "BEGIN TRANSACTION READ ONLY;`n$statement;`nROLLBACK;"
$output = $wrapped | & docker @composeArgs exec -T postgres sh -c 'psql -q -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" "$@"' psql @flags 2>&1
if ($LASTEXITCODE -ne 0) { $output | Out-Host; throw "Query failed (exit $LASTEXITCODE)" }
$text = ($output | Where-Object { $_ -notmatch '^(BEGIN|ROLLBACK)$' }) -join "`n"
if ($OutFile) { Set-Content -Path $OutFile -Value $text -Encoding utf8; Write-Host "Wrote $OutFile" } else { $text }
