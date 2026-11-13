# Locks the screen straight after Windows signs in, for use with automatic sign-in. Without this, anyone at the
# PC after a reboot gets an open desktop. Run ONCE from an elevated PowerShell (Run as administrator):
#   powershell -ExecutionPolicy Bypass -File C:\mcp\services\install-lock-at-signin.ps1
# To undo it:
#   powershell -ExecutionPolicy Bypass -File C:\mcp\services\install-lock-at-signin.ps1 -Remove
param([switch]$Remove, [string]$User = "$env:USERDOMAIN\$env:USERNAME")
$ErrorActionPreference = 'Stop'

$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
if (-not ([Security.Principal.WindowsPrincipal]$identity).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Run this from an elevated PowerShell (Run as administrator).'
}
$name = 'MCP-LockAtSignIn'
if ($Remove) {
    Unregister-ScheduledTask -TaskName $name -Confirm:$false
    "Removed $name."
    return
}
$user = $User
$action = New-ScheduledTaskAction -Execute 'rundll32.exe' -Argument 'user32.dll,LockWorkStation'
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $user
$principal = New-ScheduledTaskPrincipal -UserId $user -LogonType Interactive -RunLevel Limited
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable `
    -ExecutionTimeLimit (New-TimeSpan -Minutes 1) -MultipleInstances IgnoreNew
Register-ScheduledTask -TaskName $name -Action $action -Trigger $trigger -Settings $settings -Principal $principal -Force | Out-Null
"Registered $name. The screen will lock each time you sign in, starting next time. Nothing has been locked now."
