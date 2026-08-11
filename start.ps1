[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
$exe = Join-Path $root 'bin\cli-proxy-api.exe'
$config = Join-Path $root 'config.yaml'
$pidFile = Join-Path $root 'run\cli-proxy-api.pid'
$stdoutLog = Join-Path $root 'run\stdout.log'
$stderrLog = Join-Path $root 'run\stderr.log'

if (-not (Test-Path -LiteralPath $exe)) {
    throw "Executable not found: $exe"
}
if (-not (Test-Path -LiteralPath $config)) {
    throw "Configuration not found: $config"
}

if (Test-Path -LiteralPath $pidFile) {
    $existingPid = [int](Get-Content -LiteralPath $pidFile -Raw)
    $existing = Get-CimInstance Win32_Process -Filter "ProcessId = $existingPid" -ErrorAction SilentlyContinue
    if ($existing -and $existing.ExecutablePath -eq $exe) {
        Write-Host "CLIProxyAPI is already running (PID $existingPid)."
        exit 0
    }
    Remove-Item -LiteralPath $pidFile -Force
}

New-Item -ItemType Directory -Force (Join-Path $root 'run'), (Join-Path $root 'logs'), (Join-Path $root 'auths') | Out-Null
$process = Start-Process -FilePath $exe `
    -ArgumentList @('-config', $config) `
    -WorkingDirectory $root `
    -RedirectStandardOutput $stdoutLog `
    -RedirectStandardError $stderrLog `
    -WindowStyle Hidden `
    -PassThru
Set-Content -LiteralPath $pidFile -Value $process.Id -Encoding ascii -NoNewline

$healthUrl = 'http://127.0.0.1:8317/healthz'
for ($attempt = 0; $attempt -lt 30; $attempt++) {
    Start-Sleep -Milliseconds 500
    try {
        $response = Invoke-WebRequest -UseBasicParsing -Uri $healthUrl -TimeoutSec 2
        if ($response.StatusCode -eq 200) {
            Write-Host "CLIProxyAPI started (PID $($process.Id)): $healthUrl"
            exit 0
        }
    } catch {
        if ($process.HasExited) {
            $stderr = if (Test-Path -LiteralPath $stderrLog) { Get-Content -LiteralPath $stderrLog -Tail 30 | Out-String } else { '' }
            throw "CLIProxyAPI exited during startup.`n$stderr"
        }
    }
}

Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue
Remove-Item -LiteralPath $pidFile -Force -ErrorAction SilentlyContinue
throw "CLIProxyAPI did not become healthy within 15 seconds. See $stderrLog"

