# Basic device inventory: OS, addresses, and local user accounts.
# Windows counterpart to inventory.sh.
#
# Continue on error so one unavailable cmdlet does not abandon the report.
$ErrorActionPreference = 'Continue'

Write-Output "=== host ==="
Write-Output "hostname:  $env:COMPUTERNAME"
Write-Output "collected: $(Get-Date -Format 'yyyy-MM-dd HH:mm:ss')"

Write-Output ""
Write-Output "=== os ==="
$os = Get-CimInstance Win32_OperatingSystem
Write-Output "name:      $($os.Caption)"
Write-Output "version:   $($os.Version) (build $($os.BuildNumber))"
Write-Output "arch:      $($os.OSArchitecture)"
Write-Output "booted:    $($os.LastBootUpTime)"

Write-Output ""
Write-Output "=== addresses ==="
Get-NetIPAddress -AddressFamily IPv4 |
    Where-Object { $_.IPAddress -ne '127.0.0.1' } |
    Sort-Object InterfaceAlias |
    ForEach-Object { Write-Output ("  {0}: {1}/{2}" -f $_.InterfaceAlias, $_.IPAddress, $_.PrefixLength) }

$gateway = (Get-NetRoute -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue |
    Sort-Object RouteMetric | Select-Object -First 1)
if ($gateway) { Write-Output "  default via: $($gateway.NextHop) dev $($gateway.InterfaceAlias)" }

Write-Output ""
Write-Output "=== local user accounts ==="
# Get-LocalUser is Windows 10 / Server 2016 and later; fall back for older images.
if (Get-Command Get-LocalUser -ErrorAction SilentlyContinue) {
    Get-LocalUser | ForEach-Object {
        $last = if ($_.LastLogon) { $_.LastLogon.ToString('yyyy-MM-dd') } else { 'never' }
        Write-Output ("  {0} (enabled={1}, last logon={2})" -f $_.Name, $_.Enabled, $last)
    }
} else {
    Get-CimInstance Win32_UserAccount -Filter "LocalAccount=True" |
        ForEach-Object { Write-Output ("  {0} (disabled={1})" -f $_.Name, $_.Disabled) }
}

Write-Output ""
Write-Output "=== accounts with admin rights ==="
if (Get-Command Get-LocalGroupMember -ErrorAction SilentlyContinue) {
    Get-LocalGroupMember -Group 'Administrators' -ErrorAction SilentlyContinue |
        ForEach-Object { Write-Output "  $($_.Name) [$($_.ObjectClass)]" }
} else {
    net localgroup Administrators
}

Write-Output ""
Write-Output "=== currently logged in ==="
$sessions = Get-CimInstance Win32_LoggedOnUser -ErrorAction SilentlyContinue |
    ForEach-Object { "{0}\{1}" -f $_.Antecedent.Domain, $_.Antecedent.Name } |
    Sort-Object -Unique
if ($sessions) { $sessions | ForEach-Object { Write-Output "  $_" } }
else { Write-Output "  (nobody)" }
