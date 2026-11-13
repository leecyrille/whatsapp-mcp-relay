# What each MCP service runs. Dot-sourced by run-service.ps1 and mcpctl.ps1.
$script:Py = 'C:\mcp\venv\Scripts\python.exe'
$script:Gw = 'C:\mcp\gateway\access_proxy.py'

$script:Services = @{
    tunnel = @(
        @{ Id = 'cloudflared'; Exe = 'C:\mcp\toolchain\cloudflared\cloudflared.exe'
           Args = @('tunnel', '--no-autoupdate', '--config', 'C:\mcp\cloudflared\config.yml', 'run')
           Dir = 'C:\mcp\cloudflared' }
    )
    whatsapp = @(
        @{ Id = 'bridge'; Exe = 'C:\mcp\whatsapp\src\whatsapp-bridge\whatsapp-bridge.exe'; Args = @()
           Dir = 'C:\mcp\whatsapp\src\whatsapp-bridge' }
        # Full paths, as mcpctl finds processes by command line and both of these are a server.py.
        @{ Id = 'server'; Exe = $Py; Args = @('C:\mcp\whatsapp\server\server.py'); Dir = 'C:\mcp\whatsapp\server' }
        @{ Id = 'gateway'; Exe = $Py; Args = @($Gw, '--server', 'whatsapp'); Dir = 'C:\mcp\gateway' }
        @{ Id = 'relay'; Exe = 'C:\mcp\whatsapp-app\relay\bin\wa-relay.exe'; Args = @(); Dir = 'C:\mcp\whatsapp-app\relay\bin' }
    )
    pc = @(
        @{ Id = 'aggregator'; Exe = $Py; Args = @('aggregator.py'); Dir = 'C:\mcp\pc' }
        @{ Id = 'gateway'; Exe = $Py; Args = @($Gw, '--server', 'pc'); Dir = 'C:\mcp\gateway' }
    )
    claude = @(
        @{ Id = 'server'; Exe = $Py; Args = @('C:\mcp\claude\server.py'); Dir = 'C:\mcp\claude' }
        @{ Id = 'gateway'; Exe = $Py; Args = @($Gw, '--server', 'claude'); Dir = 'C:\mcp\gateway' }
        @{ Id = 'runner'; Exe = 'C:\mcp\whatsapp-app\relay\bin\wa-runner.exe'; Args = @(); Dir = 'C:\mcp\whatsapp-app\relay\bin' }
    )
    sms = @(
        @{ Id = 'bridge'; Exe = 'C:\mcp\sms\bin\sms-bridge.exe'; Args = @(); Dir = 'C:\mcp\sms\bin' }
    )
}

# Scheduled task for each service, and when it starts. Desktop control and Claude's own login
# need the logged-in user, so those two wait for logon; the rest start at boot. SMS waits too: its
# saved pairing is encrypted to the user's Windows login, which a boot task with no stored password
# can't unlock.
$script:Tasks = @{
    tunnel   = @{ Task = 'MCP-Tunnel';   Trigger = 'Boot' }
    whatsapp = @{ Task = 'MCP-WhatsApp'; Trigger = 'Boot' }
    pc       = @{ Task = 'MCP-PC';       Trigger = 'Logon' }
    claude   = @{ Task = 'MCP-Claude';   Trigger = 'Logon' }
    sms      = @{ Task = 'MCP-SMS';      Trigger = 'Logon' }
}
$script:PauseFlag = 'C:\mcp\services\OFF-{0}.flag'
