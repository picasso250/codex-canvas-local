param(
    [string]$Url = "http://127.0.0.1:8765",
    [string]$AgentUrl = "http://127.0.0.1:53166",
    [string]$ExePath = "$PSScriptRoot\..\bin\codex-canvas-local.exe",
    [string]$ProjectRoot = "$PSScriptRoot\..",
    [int]$HealthTimeoutSec = 20,
    [int]$WaitPollSec = 5,
    [int]$WaitTimeoutSec = 3600,
    [switch]$Wait,
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
    $allEndpoint = "$BaseUrl/api/jobs"
    try {
        $jobs = Invoke-RestMethod -Uri $allEndpoint -Method Get -TimeoutSec 5
        if ($null -ne $jobs) {
            return @($jobs | Where-Object { $_.status -eq "queued" -or $_.status -eq "running" })
        }
        return @()
    } catch {
        Write-Warning "Could not read ${allEndpoint}: $($_.Exception.Message). Falling back to per-mode job lists."
    }

    $active = @()
    foreach ($mode in @("work", "pic")) {
        $endpoint = "$BaseUrl/api/$mode/jobs"
        try {
            $jobs = Invoke-RestMethod -Uri $endpoint -Method Get -TimeoutSec 5
        } catch {
            Write-Warning "Could not read ${endpoint}: $($_.Exception.Message)"
            continue
        }

        if ($null -eq $jobs) {
            continue
        }

        $active += @($jobs |
            Where-Object { $_.status -eq "queued" -or $_.status -eq "running" } |
            Select-Object @{Name = "mode"; Expression = { $mode } }, id, status, createdAt)
    }

    return $active
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

function Get-ActiveAgentRequest([string]$BaseUrl) {
    try {
        $status = Invoke-RestMethod -Uri "$BaseUrl/status" -Method Get -TimeoutSec 5
    } catch {
        return $null
    }

    $queueLength = 0
    if ($null -ne $status.queue_length) {
        $queueLength = [int]$status.queue_length
    }
    if ([bool]$status.busy -or $queueLength -gt 0) {
        return [pscustomobject]@{
            mode = "agent"
            id = $status.running_request_id
            status = "running"
            createdAt = ""
        }
    }
    return $null
}

$ProjectRoot = Resolve-FullPath $ProjectRoot
$ExePath = Resolve-FullPath $ExePath
$tmpExe = Join-Path $ProjectRoot "bin\codex-canvas-local.new.exe"

Set-Location $ProjectRoot

$proc = Get-CanvasProcess $ExePath
$waitDeadline = (Get-Date).AddSeconds($WaitTimeoutSec)
do {
    $activeJobs = Get-ActiveJobs $Url
    $activeAgent = Get-ActiveAgentRequest $AgentUrl
    if ($null -ne $activeAgent) {
        $activeJobs = @($activeJobs) + $activeAgent
    }
    if ($activeJobs.Count -eq 0 -or $Force -or -not $Wait) {
        break
    }
    $summary = $activeJobs | Select-Object mode,id,status,createdAt | Format-Table -AutoSize | Out-String
    Write-Host "Waiting for active jobs before restart:`n$summary"
    Start-Sleep -Seconds $WaitPollSec
} while ((Get-Date) -lt $waitDeadline)

if ($activeJobs.Count -gt 0 -and -not $Force) {
    $summary = $activeJobs | Select-Object mode,id,status,createdAt | Format-Table -AutoSize | Out-String
    $hint = if ($Wait) { "Timed out waiting for jobs to finish. Use -Force to override." } else { "Use -Wait to wait, or -Force to override." }
    throw "Refusing to restart while jobs are active. $hint`n$summary"
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
