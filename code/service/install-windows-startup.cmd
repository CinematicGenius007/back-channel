@echo off
rem Opens backchannel in Windows Terminal at every logon.
rem Assumes bch.exe is on PATH (e.g. C:\Tools\bch.exe with C:\Tools in PATH).
set SC=%APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup\backchannel.lnk
powershell -NoProfile -Command ^
  "$s=(New-Object -ComObject WScript.Shell).CreateShortcut('%SC%');" ^
  "$s.TargetPath='wt.exe'; $s.Arguments='-w 0 nt --title backchannel bch';" ^
  "$s.Save()"
echo Created %SC%
