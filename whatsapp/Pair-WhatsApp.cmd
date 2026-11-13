@echo off
rem Shows the WhatsApp QR code. Use this for the first link and for any re-scan.
rem On your phone: WhatsApp > Settings > Linked devices > Link a device, then scan.
rem Once it says it's connected, wait a minute for history to sync, then press Ctrl+C.
rem Afterwards, restart the service: mcpctl.ps1 start whatsapp
powershell -NoProfile -ExecutionPolicy Bypass -File C:\mcp\services\mcpctl.ps1 stop whatsapp
if errorlevel 1 (
    echo.
    echo The WhatsApp service is still running, so a second copy can't be started safely.
    pause
    exit /b 1
)
cd /d C:\mcp\whatsapp\src\whatsapp-bridge
whatsapp-bridge.exe
