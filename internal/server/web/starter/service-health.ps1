# Services that should be running and are not.
$ErrorActionPreference = 'Continue'

Write-Output "=== automatic services that are not running ==="
$stopped = Get-Service | Where-Object {
    $_.StartType -eq 'Automatic' -and $_.Status -ne 'Running'
}
if ($stopped) {
    $stopped | ForEach-Object {
        Write-Output ("  {0} ({1}) — {2}" -f $_.Name, $_.DisplayName, $_.Status)
    }
} else {
    Write-Output "  none"
}

Write-Output ""
Write-Output "=== services that failed to start this boot ==="
$boot = (Get-CimInstance Win32_OperatingSystem).LastBootUpTime
Get-WinEvent -FilterHashtable @{ LogName='System'; Level=2; StartTime=$boot } -ErrorAction SilentlyContinue |
    Where-Object { $_.ProviderName -like '*Service Control Manager*' } |
    Select-Object -First 15 |
    ForEach-Object { Write-Output ("  {0}  {1}" -f $_.TimeCreated, ($_.Message -split "`n")[0]) }

Write-Output ""
Write-Output "=== recent system errors ==="
Get-WinEvent -FilterHashtable @{ LogName='System'; Level=1,2 } -MaxEvents 10 -ErrorAction SilentlyContinue |
    ForEach-Object { Write-Output ("  {0}  [{1}] {2}" -f $_.TimeCreated, $_.ProviderName, ($_.Message -split "`n")[0]) }
