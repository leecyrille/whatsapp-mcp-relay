# Keeps one MCP service's processes running, restarting any that exit. Run by a scheduled task.
# If the service's OFF flag exists (kill switch), it exits at once and starts nothing.
param([Parameter(Mandatory)][ValidateSet('tunnel', 'whatsapp', 'pc', 'claude', 'sms')][string]$Name)

. "$PSScriptRoot\services.ps1"
$env:PYTHONUTF8 = '1'
$env:PYTHONUNBUFFERED = '1'
$flag = $PauseFlag -f $Name
$logDir = 'C:\mcp\logs'
$maxLog = 10MB

# Boot tasks run in session 0, where a normal (non-admin) shell can't stop or even inspect their
# processes, only stop this wrapper through Task Scheduler. So tie the children to this wrapper
# with a job object: when the wrapper ends, for any reason, Windows ends them too. Not used for the
# PC and Claude logon services, so an app opened through PC control doesn't close when that service
# stops. SMS opens nothing, and an off switch for it should leave nothing behind.
if ($Name -in 'whatsapp', 'tunnel', 'sms') {
    try {
        Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
public static class McpJob {
    [StructLayout(LayoutKind.Sequential)] struct Basic {
        public long UserTime, JobTime; public uint LimitFlags; public UIntPtr MinWs, MaxWs;
        public uint ActiveProcesses; public UIntPtr Affinity; public uint Priority, Scheduling; }
    [StructLayout(LayoutKind.Sequential)] struct Io { public ulong A, B, C, D, E, F; }
    [StructLayout(LayoutKind.Sequential)] struct Extended {
        public Basic BasicInfo; public Io IoInfo; public UIntPtr ProcMem, JobMem, PeakProc, PeakJob; }
    [DllImport("kernel32.dll", SetLastError = true)] static extern IntPtr CreateJobObject(IntPtr a, string name);
    [DllImport("kernel32.dll", SetLastError = true)] static extern bool SetInformationJobObject(IntPtr job, int cls, ref Extended info, int size);
    [DllImport("kernel32.dll", SetLastError = true)] static extern bool AssignProcessToJobObject(IntPtr job, IntPtr proc);
    [DllImport("kernel32.dll")] static extern IntPtr GetCurrentProcess();
    static IntPtr handle;
    public static void KillChildrenWithThisProcess() {
        handle = CreateJobObject(IntPtr.Zero, null);
        if (handle == IntPtr.Zero) throw new Exception("CreateJobObject " + Marshal.GetLastWin32Error());
        Extended info = new Extended();
        info.BasicInfo.LimitFlags = 0x2000; // JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
        if (!SetInformationJobObject(handle, 9, ref info, Marshal.SizeOf(typeof(Extended))))
            throw new Exception("SetInformationJobObject " + Marshal.GetLastWin32Error());
        if (!AssignProcessToJobObject(handle, GetCurrentProcess()))
            throw new Exception("AssignProcessToJobObject " + Marshal.GetLastWin32Error());
    }
}
'@
        [McpJob]::KillChildrenWithThisProcess()
    } catch {
        Add-Content "$logDir\$Name-restarts.log" "$(Get-Date -Format s) job object failed: $($_.Exception.Message)"
    }
}

function Rotate-Log($path) {
    if ((Test-Path $path) -and (Get-Item $path).Length -gt $maxLog) {
        Move-Item -Path $path -Destination "$path.1" -Force
    }
}

function Start-Part($part) {
    $base = Join-Path $logDir "$Name-$($part.Id)"
    Rotate-Log "$base.log"
    Rotate-Log "$base.err.log"
    $options = @{
        FilePath               = $part.Exe
        WorkingDirectory       = $part.Dir
        RedirectStandardOutput = "$base.log"
        RedirectStandardError  = "$base.err.log"
        WindowStyle            = 'Hidden'
        PassThru               = $true
    }
    if ($part.Args.Count -gt 0) { $options.ArgumentList = $part.Args }
    $proc = Start-Process @options
    $null = $proc.Handle  # Without a cached handle, ExitCode comes back empty later.
    $proc
}

$running = @{}
while ($true) {
    if (Test-Path $flag) { exit 0 }
    foreach ($part in $Services[$Name]) {
        $proc = $running[$part.Id]
        if ($null -eq $proc -or $proc.HasExited) {
            if ($proc) {
                Add-Content "$logDir\$Name-restarts.log" "$(Get-Date -Format s) $($part.Id) exited with $($proc.ExitCode)"
            }
            $running[$part.Id] = Start-Part $part
        }
    }
    Start-Sleep -Seconds 10
}
