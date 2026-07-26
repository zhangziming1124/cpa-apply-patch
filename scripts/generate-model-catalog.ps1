# Generate ~/.codex/model_catalog.json from a CPA server's live catalog.
#
# Fetches the full Codex-client catalog from CPA, then adds
# apply_patch_tool_type: "freeform" to every text model that lacks it
# (media models are skipped). GPT entries are kept byte-identical, so the
# Codex model picker keeps its full list. Optionally adds
# multi_agent_version: "v2" to enable the v2 desktop UI pipeline.
param(
    [Parameter(Mandatory = $true)][string]$CpaBaseUrl,   # e.g. http://1.2.3.4:8317
    [Parameter(Mandatory = $true)][string]$ApiKey,
    [string]$OutFile = (Join-Path $env:USERPROFILE ".codex\model_catalog.json"),
    [switch]$SkipMultiAgentV2
)

$ErrorActionPreference = "Stop"
$url = "$($CpaBaseUrl.TrimEnd('/'))/v1/models?client_version=0.99.0"
$resp = curl.exe -s $url -H "Authorization: Bearer $ApiKey" | ConvertFrom-Json
if (-not $resp.models) { throw "CPA returned no models — check URL and API key" }

$added = @()
foreach ($m in $resp.models) {
    if ($m.slug -match 'image|imagine|video') { continue }   # media models never get apply_patch
    if (-not $m.apply_patch_tool_type) {
        $m | Add-Member -NotePropertyName apply_patch_tool_type -NotePropertyValue "freeform" -Force
        $added += $m.slug
    }
    if (-not $SkipMultiAgentV2 -and -not $m.multi_agent_version) {
        $m | Add-Member -NotePropertyName multi_agent_version -NotePropertyValue "v2" -Force
    }
}

if (Test-Path $OutFile) { Copy-Item $OutFile "$OutFile.bak" -Force }
Set-Content -LiteralPath $OutFile -Value ([PSCustomObject]@{ models = @($resp.models) } | ConvertTo-Json -Depth 30) -Encoding UTF8

"catalog written: $OutFile ($($resp.models.Count) models)"
"apply_patch added to $($added.Count) models: $($added -join ', ')"
"next: add this line to ~/.codex/config.toml ->  model_catalog_json = `"$($OutFile -replace '\\', '\\')`""
