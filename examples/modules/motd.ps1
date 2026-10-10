<# External module example (WinRM): ensure a file holds the wanted content.
   Params (environment): $env:GOAF_P_path (default C:\Temp\motd-example),
   $env:GOAF_P_content (required).
   Protocol: script <check|apply> prints "needed|changed: true|false",
   optional "output: ..." lines, or "error: ..." on failure. #>
param([string]$Verb)
$file = if ($env:GOAF_P_path) { $env:GOAF_P_path } else { 'C:\Temp\motd-example' }
$want = $env:GOAF_P_content

if (-not $want) { Write-Output 'error: content param is required'; exit 1 }
$cur = ''
if (Test-Path $file) { $cur = [IO.File]::ReadAllText($file) }

switch ($Verb) {
  'check' {
    if ($cur -eq $want) { Write-Output 'needed: false' } else { Write-Output 'needed: true' }
  }
  'apply' {
    [IO.File]::WriteAllText($file, $want)
    Write-Output 'changed: true'
    Write-Output "output: updated $file"
  }
  default { Write-Output "error: want check|apply, got $Verb"; exit 1 }
}
