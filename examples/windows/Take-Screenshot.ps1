<# Screenshot the primary display (runs via interactive scheduled task,
   because WinRM shells have no visible desktop). Param: output PNG path. #>
param([string]$Out = 'C:\Temp\screen.png')
# Detach from any console immediately: prevents the brief black conhost
# flash when the scheduled task starts (together with -WindowStyle Hidden).
Add-Type @'
using System;
using System.Runtime.InteropServices;
public class NoWin {
  [DllImport("kernel32.dll")] public static extern bool FreeConsole();
  [DllImport("user32.dll")] public static extern bool ShowWindow(IntPtr h, int n);
  [DllImport("kernel32.dll")] public static extern IntPtr GetConsoleWindow();
}
'@
[void][NoWin]::FreeConsole()
[void][NoWin]::ShowWindow([NoWin]::GetConsoleWindow(), 0)
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$b = [Windows.Forms.Screen]::PrimaryScreen.Bounds
$bmp = New-Object Drawing.Bitmap($b.Width, $b.Height)
$g = [Drawing.Graphics]::FromImage($bmp)
$g.CopyFromScreen($b.Location, [Drawing.Point]::Empty, $b.Size)
$g.Dispose()
$bmp.Save($Out, [Drawing.Imaging.ImageFormat]::Png)
$bmp.Dispose()
'SHOT: ' + $Out + ' ' + $b.Width + 'x' + $b.Height
