param(
    [string]$Url = "http://127.0.0.1:53166",
    [string]$ProjectRoot = "$PSScriptRoot\..",
    [int]$IdleTimeoutSec = 1800,
    [switch]$Force
)

$ErrorActionPreference = "Stop"

function Resolve-FullPath([string]$Path) {
    if ([System.IO.Path]::IsPathRooted($Path)) {
        return [System.IO.Path]::GetFullPath($Path)
    }
    return [System.IO.Path]::GetFullPath((Join-Path (Get-Location).Path $Path))
}

function Get-AgentProcess() {
    Get-CimInstance Win32_Process |
        Where-Object {
            $_.CommandLine -and
            ($_.CommandLine -like "*scripts\chatgpt_agent.py*" -or $_.CommandLine -like "*scripts/chatgpt_agent.py*")
        } |
        Select-Object -First 1
}

function Get-AgentStatus([string]$BaseUrl) {
    try {
        return Invoke-RestMethod -Uri "$BaseUrl/status" -Method Get -TimeoutSec 5
    } catch {
        return $null
    }
}

function Wait-AgentIdle([string]$BaseUrl, [int]$TimeoutSec) {
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    do {
        $status = Get-AgentStatus $BaseUrl
        if ($null -eq $status) {
            return
        }
        $busy = [bool]$status.busy
        $queueLength = 0
        if ($null -ne $status.queue_length) {
            $queueLength = [int]$status.queue_length
        }
        if (-not $busy -and $queueLength -eq 0) {
            return
        }
        Write-Host "Agent busy: request=$($status.running_request_id) queue=$queueLength"
        Start-Sleep -Seconds 5
    } while ((Get-Date) -lt $deadline)

    throw "Refusing to restart chatgpt_agent.py while it is busy. Use -Force to override."
}

function Wait-AgentReady([string]$BaseUrl, [int]$TimeoutSec) {
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    do {
        $status = Get-AgentStatus $BaseUrl
        if ($null -ne $status -and $status.ok) {
            return
        }
        Start-Sleep -Milliseconds 500
    } while ((Get-Date) -lt $deadline)

    throw "Agent did not become ready at $BaseUrl within $TimeoutSec seconds."
}

$ProjectRoot = Resolve-FullPath $ProjectRoot
Set-Location $ProjectRoot

if (-not $Force) {
    Wait-AgentIdle $Url $IdleTimeoutSec
}

$proc = Get-AgentProcess
if ($proc) {
    Write-Host "Stopping chatgpt_agent.py process $($proc.ProcessId)"
    Stop-Process -Id $proc.ProcessId
    try {
        Wait-Process -Id $proc.ProcessId -Timeout 15
    } catch [Microsoft.PowerShell.Commands.ProcessCommandException] {
        Write-Host "Process $($proc.ProcessId) already stopped"
    }
}

$out = Join-Path $ProjectRoot "tmp\chatgpt-agent.out.log"
$err = Join-Path $ProjectRoot "tmp\chatgpt-agent.err.log"
Remove-Item -LiteralPath $out, $err -ErrorAction SilentlyContinue

$python = (Get-Command python).Source
$arguments = @("scripts\chatgpt_agent.py", "serve", "--host", "127.0.0.1", "--port", "53166", "--mode", "always_new")
Write-Host "Starting chatgpt_agent.py"
Start-Process -FilePath $python `
    -ArgumentList $arguments `
    -WorkingDirectory $ProjectRoot `
    -RedirectStandardOutput $out `
    -RedirectStandardError $err `
    -WindowStyle Hidden

Wait-AgentReady $Url 30
Write-Host "Agent restarted and ready: $Url"
