# Saves a token from the clipboard into C:\mcp\.env, then clears the clipboard. Never prints it.
#   save_token_from_clipboard.ps1                                  (Cloudflare API token)
#   save_token_from_clipboard.ps1 -Key PLAYWRIGHT_MCP_EXTENSION_TOKEN
param([ValidateSet('CLOUDFLARE_API_TOKEN', 'PLAYWRIGHT_MCP_EXTENSION_TOKEN')][string]$Key = 'CLOUDFLARE_API_TOKEN')
$envFile = 'C:\mcp\.env'
$token = (Get-Clipboard -Raw)
if ($null -eq $token) { 'Clipboard is empty.'; exit 1 }
$token = $token.Trim()
if ($token -notmatch '^[A-Za-z0-9_\-\.=+/]{20,}$') {
    "Clipboard doesn't look like a token ($($token.Length) characters). Nothing saved."
    exit 1
}
$found = $false
$lines = Get-Content $envFile | ForEach-Object {
    if ($_ -match "^#?\s*$Key=") { $found = $true; "$Key=$token" } else { $_ }
}
if (-not $found) { $lines = @($lines) + "$Key=$token" }
$utf8NoBom = New-Object System.Text.UTF8Encoding($false)
[IO.File]::WriteAllLines($envFile, [string[]]$lines, $utf8NoBom)
Set-Clipboard -Value ' '
"Saved $Key to $envFile ($($token.Length) characters) and cleared the clipboard."
