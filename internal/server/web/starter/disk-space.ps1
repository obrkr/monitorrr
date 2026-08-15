# Free space per drive, plus the biggest folders worth looking at.
# Read-only: reports, never deletes.
$ErrorActionPreference = 'Continue'

Write-Output "=== drives ==="
Get-CimInstance Win32_LogicalDisk -Filter "DriveType=3" | ForEach-Object {
    $freePct = if ($_.Size) { [math]::Round(($_.FreeSpace / $_.Size) * 100, 1) } else { 0 }
    Write-Output ("  {0} {1} GB free of {2} GB ({3}% free){4}" -f $_.DeviceID,
        [math]::Round($_.FreeSpace / 1GB, 1), [math]::Round($_.Size / 1GB, 1), $freePct,
        $(if ($freePct -lt 20) { "  <-- LOW" } else { "" }))
}

Write-Output ""
Write-Output "=== largest folders under C:\Users ==="
# Measured one level down: a full recursive scan of a big disk is slow and
# rarely more useful than knowing which profile is the problem.
Get-ChildItem C:\Users -Directory -ErrorAction SilentlyContinue | ForEach-Object {
    $size = (Get-ChildItem $_.FullName -Recurse -File -ErrorAction SilentlyContinue |
             Measure-Object -Property Length -Sum).Sum
    [PSCustomObject]@{ Folder = $_.Name; GB = [math]::Round($size / 1GB, 2) }
} | Sort-Object GB -Descending | Select-Object -First 8 |
    ForEach-Object { Write-Output ("  {0}: {1} GB" -f $_.Folder, $_.GB) }

Write-Output ""
Write-Output "=== recycle bin and temp ==="
$temp = (Get-ChildItem $env:TEMP -Recurse -File -ErrorAction SilentlyContinue |
         Measure-Object -Property Length -Sum).Sum
Write-Output ("  user temp: {0} MB" -f [math]::Round($temp / 1MB, 1))
$wintemp = (Get-ChildItem C:\Windows\Temp -Recurse -File -ErrorAction SilentlyContinue |
            Measure-Object -Property Length -Sum).Sum
Write-Output ("  windows temp: {0} MB" -f [math]::Round($wintemp / 1MB, 1))
