# Start, stop or check the MCP services.
#   mcpctl.ps1 status
#   mcpctl.ps1 stop whatsapp        (stays off until "start")
#   mcpctl.ps1 start whatsapp
#
# "start all" leaves SMS alone: it reads bank and login texts, so it only starts when named.
# "stop all" and "status" do include it.
param(
    [Parameter(Mandatory)][ValidateSet('start', 'stop', 'status')][string]$Action,
    [ValidateSet('all', 'tunnel', 'whatsapp', 'pc', 'claude', 'sms')][string]$Name = 'all'
)
. "$PSScriptRoot\services.ps1"
$ErrorActionPreference = 'SilentlyContinue'
$names = if ($Name -ne 'all') { @($Name) }
         elseif ($Action -eq 'start') { 'tunnel', 'whatsapp', 'pc', 'claude' }
         else { 'tunnel', 'whatsapp', 'pc', 'claude', 'sms' }

# Only exact executable + argument matches, so anything that merely mentions a name is safe.
function Find-Parts($svc) {
    $all = Get-CimInstance Win32_Process
    foreach ($p in $all) {
        $exe = [string]$p.ExecutablePath
        $cmd = [string]$p.CommandLine
        if (-not $exe -or -not $cmd) { continue }
        if ($exe -like '*\powershell.exe' -and $cmd -like "*-File C:\mcp\services\run-service.ps1 -Name $svc*") { $p; continue }
        foreach ($part in $Services[$svc]) {
            $argText = ($part.Args -join ' ')
            $isVenv = $part.Exe -eq $Py -and ($exe -like 'C:\mcp\venv\*' -or $exe -like 'C:\mcp\toolchain\uv-python\*')
            if (($exe -eq $part.Exe -or $isVenv) -and ($argText -eq '' -or $cmd -like "*$argText*")) { $p; break }
        }
        if ($svc -eq 'pc') {
            if ($p.Name -eq 'node.exe' -and $cmd -like '*\mcp\pc\playwright\*') { $p }
            elseif ($exe -like 'C:\mcp\*' -and $cmd -like '*windows-mcp*') { $p }
        }
    }
}

# Boot tasks run in session 0, where this shell can't read a process's path or command line. Names
# and listening ports are still visible, so those are what count for them.
$BootProbes = @{
    whatsapp = @{ Names = @('whatsapp-bridge', 'wa-relay'); Ports = @(8701, 8704, 18700, 18701) }
    tunnel   = @{ Names = @('cloudflared'); Ports = @() }
}

function Count-Running($svc) {
    $ids = @(Find-Parts $svc | ForEach-Object { $_.ProcessId })
    if ($BootProbes.ContainsKey($svc)) {
        $probe = $BootProbes[$svc]
        $ids += @(Get-Process -Name $probe.Names -ErrorAction SilentlyContinue | ForEach-Object { $_.Id })
        if ($probe.Ports.Count -gt 0) {
            $ids += @(Get-NetTCPConnection -State Listen | Where-Object { $_.LocalPort -in $probe.Ports } |
                ForEach-Object { [int]$_.OwningProcess })
        }
    }
    @($ids | Sort-Object -Unique).Count
}

function Stop-One($svc) {
    New-Item -ItemType File -Path ($PauseFlag -f $svc) -Force | Out-Null
    Stop-ScheduledTask -TaskName $Tasks[$svc].Task
    foreach ($try in 1..20) {
        foreach ($p in @(Find-Parts $svc)) { Stop-Process -Id $p.ProcessId -Force -Confirm:$false }
        if ((Count-Running $svc) -eq 0) { break }
        Start-Sleep -Milliseconds 500
    }
    $left = Count-Running $svc
    if ($left -gt 0) {
        "$svc`: NOT fully stopped. $left process(es) still running; stop them from an elevated shell."
        $script:stopFailed = $true
    } else {
        "$svc`: stopped, stays off until 'mcpctl start $svc'. Processes left: 0"
    }
}

function Start-One($svc) {
    Remove-Item -Path ($PauseFlag -f $svc) -Force
    $task = $Tasks[$svc].Task
    Enable-ScheduledTask -TaskName $task | Out-Null
    Start-ScheduledTask -TaskName $task
    if ($?) { "$svc`: started through task $task." ; return }
    # Fallback for when this account isn't allowed to start the task directly.
    Start-Process powershell.exe -WindowStyle Hidden -ArgumentList @(
        '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', 'C:\mcp\services\run-service.ps1', '-Name', $svc)
    "$svc`: started directly."
}

foreach ($svc in $names) {
    switch ($Action) {
        'stop'  { Stop-One $svc }
        'start' { Start-One $svc }
        'status' {
            $task = Get-ScheduledTask -TaskName $Tasks[$svc].Task
            $off = Test-Path ($PauseFlag -f $svc)
            $count = Count-Running $svc
            "{0,-9} task={1,-8} switched-off={2,-5} processes={3}" -f $svc, $(if ($task) { $task.State } else { 'missing' }), $off, $count
        }
    }
}
if ($Action -eq 'status') {
    'Ports: ' + ((Get-NetTCPConnection -State Listen | Where-Object { $_.LocalPort -in 8701, 8702, 8703, 8704, 18700, 18701, 18702, 18703, 18705, 18710 } |
        Sort-Object LocalPort | ForEach-Object { "$($_.LocalAddress):$($_.LocalPort)" }) -join '  ')
}
if ($script:stopFailed) { exit 1 }
