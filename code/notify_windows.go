//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// toastScript shows a Windows 10/11 toast through the WinRT API that Windows
// PowerShell 5.1 can reach (PowerShell 7 can't load WinRT types this way). Title and
// body come from environment variables and are XML-escaped inside the script.
const toastScript = `$ErrorActionPreference='Stop'
[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] > $null
[Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom.XmlDocument, ContentType = WindowsRuntime] > $null
$t = [Security.SecurityElement]::Escape($env:BCH_NOTIFY_TITLE)
$b = [Security.SecurityElement]::Escape($env:BCH_NOTIFY_BODY)
$x = New-Object Windows.Data.Xml.Dom.XmlDocument
$x.LoadXml("<toast><visual><binding template='ToastGeneric'><text>$t</text><text>$b</text></binding></visual></toast>")
$id = '{1AC14E77-02E7-4E5D-B744-2EB1AE5198B7}\WindowsPowerShell\v1.0\powershell.exe'
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier($id).Show([Windows.UI.Notifications.ToastNotification]::new($x))`

func windowsToast(title, body string) *exec.Cmd {
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", toastScript)
	cmd.Env = append(os.Environ(), "BCH_NOTIFY_TITLE="+title, "BCH_NOTIFY_BODY="+body)
	// CREATE_NO_WINDOW: run detached from our console so it can't read our input or
	// flash a window.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000}
	return cmd
}
