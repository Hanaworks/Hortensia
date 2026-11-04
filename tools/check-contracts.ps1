#requires -Version 7.4
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
$schemaPath = Join-Path $repoRoot 'contracts/agent-v1.schema.json'
$schema = Get-Content -LiteralPath $schemaPath -Raw | ConvertFrom-Json -AsHashtable
$valid = @(Get-Content -LiteralPath (Join-Path $repoRoot 'contracts/examples/agent-v1.valid.json') -Raw | ConvertFrom-Json)
$invalid = @(Get-Content -LiteralPath (Join-Path $repoRoot 'contracts/examples/agent-v1.invalid.json') -Raw | ConvertFrom-Json)
$errorsFound = [Collections.Generic.List[string]]::new()
$covered = [Collections.Generic.HashSet[string]]::new()

foreach ($sample in $valid) {
    $validationErrors = @()
    $passes = Test-Json -Json ($sample | ConvertTo-Json -Depth 100 -Compress) -SchemaFile $schemaPath -ErrorAction SilentlyContinue -ErrorVariable validationErrors
    if (-not $passes) { $errorsFound.Add("Valid sample rejected: $($sample.type): $validationErrors") }
    [void]$covered.Add($sample.type)
}
foreach ($sample in $invalid) {
    $passes = Test-Json -Json ($sample.message | ConvertTo-Json -Depth 100 -Compress) -SchemaFile $schemaPath -ErrorAction SilentlyContinue
    if ($passes) { $errorsFound.Add("Invalid sample accepted: $($sample.reason)") }
}
$declaredTypes = @($schema.oneOf | ForEach-Object { $_.properties.type.const })
foreach ($messageType in $declaredTypes) {
    if (-not $covered.Contains($messageType)) { $errorsFound.Add("No positive sample: $messageType") }
}
if ($covered.Count -ne $declaredTypes.Count) { $errorsFound.Add('Sample/schema message type coverage differs') }
if ($valid.Count -eq 0 -or $invalid.Count -eq 0) { $errorsFound.Add('Both positive and negative samples are required') }

# A payload is Base64 on the wire. Validate the decoded object separately: schema
# contentEncoding is an annotation, not a substitute for this application check.
$payloadChecks = 0
foreach ($sample in $valid | Where-Object type -eq 'prepare') {
    $payloadBytes = [Convert]::FromBase64String($sample.body.payload_b64)
    $payloadText = [Text.UTF8Encoding]::new($false, $true).GetString($payloadBytes)
    $payload = $payloadText | ConvertFrom-Json
    $payloadSchema = [ordered]@{
        '$schema' = $schema['$schema']
        '$defs' = $schema['$defs']
        '$ref' = '#/$defs/FrozenPayload'
    } | ConvertTo-Json -Depth 100
    if (-not (Test-Json -Json $payloadText -Schema $payloadSchema -ErrorAction SilentlyContinue)) {
        $errorsFound.Add('Decoded prepare payload rejected')
    }
    foreach ($field in @('node_id','stack_id','version_id')) {
        if ($payload.$field -ne $sample.body.$field) { $errorsFound.Add("Prepare target mismatch: $field") }
    }
    $payloadChecks++
}

if ($errorsFound.Count) { throw ($errorsFound -join "`n") }
[pscustomobject]@{
    Validator = "PowerShell $($PSVersionTable.PSVersion) Test-Json"
    Schema = $schema['$schema']
    MessageTypes = $declaredTypes.Count
    ValidSamples = $valid.Count
    InvalidSamples = $invalid.Count
    DecodedPayloads = $payloadChecks
    Result = 'passed (structure and sample consistency only; no network or Docker execution)'
} | ConvertTo-Json
