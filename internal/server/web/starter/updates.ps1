# What is waiting to be installed, and whether a reboot is owed.
# Read-only: counts and lists, never installs.
$ErrorActionPreference = 'Continue'

Write-Output "=== pending Windows updates ==="
try {
    # The COM searcher is present on every Windows install; the PSWindowsUpdate
    # module usually is not.
    $searcher = (New-Object -ComObject Microsoft.Update.Session).CreateUpdateSearcher()
    $result = $searcher.Search("IsInstalled=0 and Type='Software'")
    Write-Output ("  {0} update(s) available" -f $result.Updates.Count)
    $result.Updates | Select-Object -First 15 | ForEach-Object {
        Write-Output ("    {0}" -f $_.Title)
    }
} catch {
    Write-Output "  could not query Windows Update: $($_.Exception.Message)"
}

Write-Output ""
Write-Output "=== reboot pending ==="
$pending = @(
    "HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Component Based Servicing\RebootPending",
    "HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\WindowsUpdate\Auto Update\RebootRequired"
) | Where-Object { Test-Path $_ }

if ($pending) {
    Write-Output "  YES"
    $pending | ForEach-Object { Write-Output ("    flagged by: {0}" -f ($_ -split '\\')[-1]) }
} else {
    Write-Output "  no"
}

Write-Output ""
Write-Output "=== last boot ==="
Write-Output ("  {0}" -f (Get-CimInstance Win32_OperatingSystem).LastBootUpTime)
