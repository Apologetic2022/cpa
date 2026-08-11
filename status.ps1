[CmdletBinding()]
param()

$exe = Join-Path $PSScriptRoot 'bin\cli-proxy-api.exe'
$pidFile = Join-Path $PSScriptRoot 'run\cli-proxy-api.pid'
$healthUrl = 'http://127.0.0.1:8317/healthz'

if (-not (Test-Path -LiteralPath $pidFile)) {
    Write-Host 'Status: stopped'
    exit 1
}

$servicePid = [int](Get-Content -LiteralPath $pidFile -Raw)
$process = Get-CimInstance Win32_Process -Filter "ProcessId = $servicePid" -ErrorAction SilentlyContinue
if (-not $process -or $process.ExecutablePath -ne $exe) {
    Write-Host 'Status: stopped (stale PID file)'
    exit 1
}

try {
    $response = Invoke-WebRequest -UseBasicParsing -Uri $healthUrl -TimeoutSec 3
    if ($response.StatusCode -eq 200) {
        Write-Host "Status: healthy (PID $servicePid, $healthUrl)"
        exit 0
    }
} catch {
    Write-Host "Status: running but unhealthy (PID $servicePid)"
    exit 2
}

Write-Host "Status: running but unhealthy (PID $servicePid)"
exit 2

