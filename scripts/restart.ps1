param(
    [string]$Url = "http://127.0.0.1:8765",
    [string]$ExePath = "$PSScriptRoot\..\bin\codex-canvas-local.exe",
    [string]$ProjectRoot = "$PSScriptRoot\..",
    [int]$HealthTimeoutSec = 20,
    [switch]$Force
)

$ErrorActionPreference = "Stop"

function Resolve-FullPath([string]$Path) {
    if ([System.IO.Path]::IsPathRooted($Path)) {
        return [System.IO.Path]::GetFullPath($Path)
    }
    return [System.IO.Path]::GetFullPath((Join-Path (Get-Location).Path $Path))
}

function Get-CanvasProcess([string]$ResolvedExePath) {
    Get-CimInstance Win32_Process |
        Where-Object {
            $_.ExecutablePath -and
            ([System.IO.Path]::GetFullPath($_.ExecutablePath) -ieq $ResolvedExePath)
        } |
        Select-Object -First 1
}

function Get-ActiveJobs([string]$BaseUrl) {
    try {
        $jobs = Invoke-RestMethod -Uri "$BaseUrl/api/jobs" -Method Get -TimeoutSec 5
    } catch {
        Write-Warning "Could not read $BaseUrl/api/jobs: $($_.Exception.Message)"
        return @()
    }

    if ($null -eq $jobs) {
        return @()
    }

    return @($jobs | Where-Object { $_.status -eq "queued" -or $_.status -eq "running" })
}

function Wait-Healthy([string]$BaseUrl, [int]$TimeoutSec) {
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    do {
        try {
            $response = Invoke-WebRequest -UseBasicParsing -Uri "$BaseUrl/" -TimeoutSec 3
            if ($response.StatusCode -eq 200) {
                return
            }
        } catch {
            Start-Sleep -Milliseconds 500
        }
    } while ((Get-Date) -lt $deadline)

    throw "Service did not become healthy at $BaseUrl within $TimeoutSec seconds."
}

$ProjectRoot = Resolve-FullPath $ProjectRoot
$ExePath = Resolve-FullPath $ExePath
$tmpExe = Join-Path $ProjectRoot "bin\codex-canvas-local.new.exe"

Set-Location $ProjectRoot

$proc = Get-CanvasProcess $ExePath
$activeJobs = Get-ActiveJobs $Url
if ($activeJobs.Count -gt 0 -and -not $Force) {
    $summary = $activeJobs | Select-Object id,status,createdAt | Format-Table -AutoSize | Out-String
    throw "Refusing to restart while jobs are active. Use -Force to override.`n$summary"
}

Write-Host "Building $tmpExe"
go build -o $tmpExe .

if ($proc) {
    Write-Host "Stopping process $($proc.ProcessId)"
    Stop-Process -Id $proc.ProcessId
    try {
        Wait-Process -Id $proc.ProcessId -Timeout 15
    } catch [Microsoft.PowerShell.Commands.ProcessCommandException] {
        Write-Host "Process $($proc.ProcessId) already stopped"
    }
}

Move-Item -Force -LiteralPath $tmpExe -Destination $ExePath

$arguments = @("--port", "8765")
if ($proc -and $proc.CommandLine -match 'codex-canvas-local\.exe"\s+(?<args>.+)$') {
    $arguments = $Matches.args
}

Write-Host "Starting $ExePath $arguments"
Start-Process -FilePath $ExePath -ArgumentList $arguments -WorkingDirectory $ProjectRoot -WindowStyle Hidden

Wait-Healthy $Url $HealthTimeoutSec
Write-Host "Restarted and healthy: $Url"
