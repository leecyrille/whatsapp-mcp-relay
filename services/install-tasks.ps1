# Registers the MCP scheduled tasks. Run ONCE from an elevated PowerShell (Run as administrator):
#   powershell -ExecutionPolicy Bypass -File C:\mcp\services\install-tasks.ps1
# To register only some services' tasks, without touching the others, add: -Only tunnel,whatsapp
# The tasks run as the signed-in Windows user unless -User DOMAIN
ame says otherwise.
# "Boot" tasks run without anyone logged in and store no password (S4U). "Logon" tasks run in the
# interactive session. All restart on failure and have no time limit.
param([string[]]$Only, [string]$User = "$env:USERDOMAIN\$env:USERNAME")
. "$PSScriptRoot\services.ps1"
$ErrorActionPreference = 'Stop'

$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
if (-not ([Security.Principal.WindowsPrincipal]$identity).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Run this from an elevated PowerShell (Run as administrator).'
}
$user = $User

foreach ($o in @($Only)) { if ($o -and -not $Tasks.ContainsKey($o)) { throw "No service named $o." } }
foreach ($name in @($Tasks.Keys)) {
    if ($Only -and $Only -notcontains $name) { continue }
    $info = $Tasks[$name]
    $action = New-ScheduledTaskAction -Execute 'powershell.exe' -WorkingDirectory 'C:\mcp\services' -Argument (
        "-NoProfile -NonInteractive -WindowStyle Hidden -ExecutionPolicy Bypass -File C:\mcp\services\run-service.ps1 -Name $name")
    $settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable `
        -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) `
        -MultipleInstances IgnoreNew
    if ($info.Trigger -eq 'Boot') {
        $trigger = New-ScheduledTaskTrigger -AtStartup
        $principal = New-ScheduledTaskPrincipal -UserId $user -LogonType S4U -RunLevel Limited
    } else {
        $trigger = New-ScheduledTaskTrigger -AtLogOn -User $user
        $principal = New-ScheduledTaskPrincipal -UserId $user -LogonType Interactive -RunLevel Limited
    }
    Register-ScheduledTask -TaskName $info.Task -Action $action -Trigger $trigger -Settings $settings `
        -Principal $principal -Force | Out-Null
    "Registered $($info.Task) ($($info.Trigger))"
}
'Done. Nothing has been started; check with: mcpctl.ps1 status'
