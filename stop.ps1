[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$exe = Join-Path $PSScriptRoot 'bin\cli-proxy-api.exe'
$pidFile = Join-Path $PSScriptRoot 'run\cli-proxy-api.pid'

if (-not (Test-Path -LiteralPath $pidFile)) {
    Write-Host 'CLIProxyAPI is not running (no PID file).'
    exit 0
}

$servicePid = [int](Get-Content -LiteralPath $pidFile -Raw)
$process = Get-CimInstance Win32_Process -Filter "ProcessId = $servicePid" -ErrorAction SilentlyContinue
if ($process -and $process.ExecutablePath -eq $exe) {
    Stop-Process -Id $servicePid -Force
    Wait-Process -Id $servicePid -Timeout 10 -ErrorAction SilentlyContinue
    Write-Host "CLIProxyAPI stopped (PID $servicePid)."
} else {
    Write-Host 'CLIProxyAPI process was not found; removing stale PID file.'
}

Remove-Item -LiteralPath $pidFile -Force -ErrorAction SilentlyContinue

