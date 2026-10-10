<# Screenshot the primary display (runs via interactive scheduled task,
   because WinRM shells have no visible desktop). Param: output PNG path. #>
param([string]$Out = 'C:\Temp\screen.png')
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
