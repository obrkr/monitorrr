# Is this machine's network actually working, and where does it break.
$ErrorActionPreference = 'Continue'

Write-Output "=== interfaces ==="
Get-NetIPAddress -AddressFamily IPv4 | Where-Object { $_.IPAddress -ne '127.0.0.1' } |
    Sort-Object InterfaceAlias |
    ForEach-Object { Write-Output ("  {0}: {1}/{2}" -f $_.InterfaceAlias, $_.IPAddress, $_.PrefixLength) }

$route = Get-NetRoute -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue |
    Sort-Object RouteMetric | Select-Object -First 1
$gw = if ($route) { $route.NextHop } else { $null }
Write-Output ("  default gateway: {0}" -f $(if ($gw) { $gw } else { "none found" }))

Write-Output ""
Write-Output "=== gateway reachable ==="
if ($gw -and (Test-Connection -ComputerName $gw -Count 2 -Quiet -ErrorAction SilentlyContinue)) {
    Write-Output "  yes ($gw)"
} else {
    Write-Output "  NO -- $(if ($gw) { $gw } else { 'no gateway configured' })"
}

Write-Output ""
Write-Output "=== DNS resolution ==="
try {
    Resolve-DnsName example.com -ErrorAction Stop | Out-Null
    Write-Output "  working"
} catch {
    Write-Output "  FAILED -- name resolution is not working"
}
Get-DnsClientServerAddress -AddressFamily IPv4 -ErrorAction SilentlyContinue |
    Where-Object { $_.ServerAddresses } |
    ForEach-Object { Write-Output ("  {0}: {1}" -f $_.InterfaceAlias, ($_.ServerAddresses -join ", ")) }

Write-Output ""
Write-Output "=== internet reachable ==="
if (Test-Connection -ComputerName 1.1.1.1 -Count 2 -Quiet -ErrorAction SilentlyContinue) {
    Write-Output "  yes (1.1.1.1 responds)"
} else {
    Write-Output "  NO -- cannot reach 1.1.1.1"
}

Write-Output ""
Write-Output "=== listening ports ==="
Get-NetTCPConnection -State Listen -ErrorAction SilentlyContinue |
    Select-Object -First 15 LocalAddress, LocalPort, OwningProcess |
    ForEach-Object {
        $name = (Get-Process -Id $_.OwningProcess -ErrorAction SilentlyContinue).ProcessName
        Write-Output ("  {0}:{1}  {2}" -f $_.LocalAddress, $_.LocalPort, $name)
    }
