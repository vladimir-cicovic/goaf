<#
.SYNOPSIS
  Prepares a Windows 10/11 host for goaf WinRM management.
.DESCRIPTION
  Run ELEVATED (right-click PowerShell -> Run as administrator):
    powershell -ExecutionPolicy Bypass -File Enable-GoafWinRM.ps1
  What it does:
    1. Enables WinRM service + HTTP listener (port 5985), skipping the
       Public-network-profile check that blocks Set-WSManQuickConfig.
    2. Enables Basic authentication (goaf can use Basic or NTLM).
    3. Enables the "Windows Remote Management" firewall rules.
    4. Sets LocalAccountTokenFilterPolicy=1 so non-builtin local admins
       (anything except Administrator) can log on remotely - without it
       WinRM returns HTTP 401 even with the right password.
    5. Prints listener status + IPv4 for the inventory file.
#>
#Requires -RunAsAdministrator
$ErrorActionPreference = "Stop"

Write-Host "== 1/5 WinRM service + listener =="
Enable-PSRemoting -Force -SkipNetworkProfileCheck

Write-Host "== 2/5 Basic auth =="
Set-Item WSMan:\localhost\Service\Auth\Basic $true

Write-Host "== 3/5 Firewall rules =="
Enable-NetFirewallRule -DisplayGroup "Windows Remote Management"

Write-Host "== 4/5 Remote UAC fix (non-builtin admins) =="
Set-ItemProperty -Path "HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System" `
  -Name LocalAccountTokenFilterPolicy -Value 1 -Type DWord

Write-Host "== 5/5 Restart + verify =="
Restart-Service WinRM
winrm enumerate winrm/config/listener
$ip = (Get-NetIPAddress -AddressFamily IPv4 -ErrorAction SilentlyContinue |
  Where-Object { $_.IPAddress -notlike '127.*' -and $_.AddressState -eq 'Preferred' } |
  Select-Object -First 1 -ExpandProperty IPAddress)
Write-Host ""
Write-Host "Done. Use in inventory:"
Write-Host "  hosts:"
Write-Host "    ${ip}:"
Write-Host "      user: <admin-user>"
Write-Host "      connection: winrm"
Write-Host "      # password: <secret>   # or GOAF_WINRM_PASSWORD / --ask-winrm-pass"
Write-Host "Test: Test-WSMan -ComputerName ${ip} -Credential (Get-Credential) -Authentication Negotiate"
