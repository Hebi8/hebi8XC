# Running a node all the time

`h8c up` runs in the foreground. To have a node start with the machine, give it to the system's own service manager. The three setups below are the ones in use on real machines since 2026-10-13. `h8c` has no command that installs them yet.

On every platform, first:

1. Put `h8c` somewhere stable and run `h8c name <name>` if the host name should not be the node's name (the name ends up in a public key title), then `h8c login`.
2. To share this machine's sessions, put Wake's `wake-cli` next to it. Where it is in Wake's release files (v0.9.2):
   - Linux: `wake-<version>-linux-<arch>.tar.gz`, file `wake-cli`.
   - macOS: `Wake-<version>-macos.zip`, file `Wake.app/Contents/MacOS/wake-cli`. It runs on its own outside the app.
   - Windows: `wake-<version>-windows-x86_64.zip`, file `wake-cli.exe`.

   Only that one file is needed. Installing the Wake app as well does no harm: while the app runs it keeps the index current and `h8c` only reads it.

A service does not get your shell's `PATH`, so the examples pass `--wake-cli` with the full path.

## Linux: systemd user service

`~/.config/systemd/user/h8c.service`:

```ini
[Unit]
Description=hebi8XC node (h8c)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%h/.local/bin/h8c up --wake-cli %h/.local/bin/wake-cli
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
```

```sh
systemctl --user daemon-reload
systemctl --user enable --now h8c.service
journalctl --user -u h8c.service -f     # the log
```

For the node to run while you are not logged in, lingering must be on: `loginctl enable-linger "$USER"`.

## macOS: launch agent

`~/Library/LaunchAgents/io.github.hebi8.h8c.plist`, with `/Users/alice` replaced by your home directory (launchd does not expand `~`):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>io.github.hebi8.h8c</string>
  <key>ProgramArguments</key>
  <array>
    <string>/Users/alice/.local/bin/h8c</string>
    <string>up</string>
    <string>--wake-cli</string>
    <string>/Users/alice/.local/bin/wake-cli</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict><key>PATH</key><string>/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin</string></dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>StandardOutPath</key><string>/Users/alice/Library/Logs/h8c.log</string>
  <key>StandardErrorPath</key><string>/Users/alice/Library/Logs/h8c.log</string>
</dict>
</plist>
```

```sh
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/io.github.hebi8.h8c.plist
launchctl kickstart -k gui/$(id -u)/io.github.hebi8.h8c    # restart, for example after replacing h8c
launchctl bootout gui/$(id -u)/io.github.hebi8.h8c         # stop and unload
```

The `PATH` entry is there so the node finds the `tailscale` command. The node runs while you are logged in. mDNS worked from a launch agent without any prompt on the machine it was tried on.

## Windows: task at logon

In PowerShell, as yourself (no administrator needed for the task itself):

```powershell
$h   = "$env:USERPROFILE\bin\h8c.exe"
$w   = "$env:USERPROFILE\bin\wake-cli.exe"
$log = "$env:APPDATA\hebi8xc\h8c.log"

$arg = "--headless cmd.exe /d /c `"`"$h`" up --wake-cli `"$w`" >> `"$log`" 2>&1`""
$action   = New-ScheduledTaskAction -Execute "conhost.exe" -Argument $arg
$trigger  = New-ScheduledTaskTrigger -AtLogOn -User "$env:USERDOMAIN\$env:USERNAME"
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
    -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) `
    -MultipleInstances IgnoreNew
Register-ScheduledTask -TaskName "hebi8XC node" -Action $action -Trigger $trigger -Settings $settings -Force
Start-ScheduledTask -TaskName "hebi8XC node"
```

`conhost.exe --headless` keeps a console window from appearing; the log goes to the file. The node runs only while you are logged in, and is off while the machine sleeps. The first time a program listens, Windows asks whether to allow it; see the README's Windows section for what happens when nobody answers.

To check that the node answers on Windows, ask it (`h8c peers`, or `curl.exe http://127.0.0.1:4893/`, which answers 401 without the cookie). `Get-NetTCPConnection -State Listen` did not list the node's ports although they were answering.

## Replacing the binary

Machines do not have to be updated together. On each one: put the new file in place, then restart the service.

- Linux: replace the file, `systemctl --user restart h8c.service`.
- macOS: replace the file, `launchctl kickstart -k gui/$(id -u)/io.github.hebi8.h8c`.
- Windows: the running file cannot be replaced. `Stop-ScheduledTask -TaskName "hebi8XC node"`, `Stop-Process -Name h8c`, replace the file, `Start-ScheduledTask -TaskName "hebi8XC node"`.

`h8c version` says what a machine runs, and `h8c peers` what the others run.
