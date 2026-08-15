# The processes actually consuming this machine.
$ErrorActionPreference = 'Continue'

Write-Output "=== top 10 by CPU time ==="
Get-Process | Sort-Object CPU -Descending | Select-Object -First 10 |
    ForEach-Object {
        Write-Output ("  {0,-28} pid {1,-7} cpu {2,8:N1}s  mem {3,7:N0} MB" -f
            $_.ProcessName, $_.Id, $_.CPU, ($_.WorkingSet64 / 1MB))
    }

Write-Output ""
Write-Output "=== top 10 by memory ==="
Get-Process | Sort-Object WorkingSet64 -Descending | Select-Object -First 10 |
    ForEach-Object {
        Write-Output ("  {0,-28} pid {1,-7} mem {2,7:N0} MB" -f
            $_.ProcessName, $_.Id, ($_.WorkingSet64 / 1MB))
    }

Write-Output ""
Write-Output "=== memory summary ==="
$os = Get-CimInstance Win32_OperatingSystem
$totalGB = [math]::Round($os.TotalVisibleMemorySize / 1MB, 1)
$freeGB  = [math]::Round($os.FreePhysicalMemory / 1MB, 1)
Write-Output ("  {0} GB free of {1} GB ({2}% used)" -f $freeGB, $totalGB,
    [math]::Round((($totalGB - $freeGB) / $totalGB) * 100, 1))

Write-Output ""
Write-Output "=== processor load ==="
Write-Output ("  {0}% in use" -f (Get-CimInstance Win32_Processor |
    Measure-Object -Property LoadPercentage -Average).Average)
