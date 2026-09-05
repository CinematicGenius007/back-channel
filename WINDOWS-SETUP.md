# backchannel — Windows Client Setup

One-time setup to connect a Windows PC to a `bch` hub (for example the one running on your Mac).

---

## 1. Install

1. Create a folder for it:
   ```powershell
   mkdir C:\Tools
   ```
2. Copy the binary in and rename it exactly `bch.exe`:
   ```powershell
   copy C:\Users\you\Downloads\bch-windows-amd64.exe C:\Tools\bch.exe
   ```
3. First run may trigger SmartScreen (unsigned binary) — click **More info → Run anyway**. Only happens once.

## 2. Add to PATH

1. Press **Win**, type `env`, open **"Edit environment variables for your account"**.
2. Select **Path** → **Edit** → **New** → type `C:\Tools` → **OK** on every dialog.
3. **Fully close** all terminal windows (not just the tab) and reopen — PATH is only re-read by a fresh terminal process.
4. Verify:
   ```powershell
   bch -v
   ```
   If you get `'bch' is not recognized`: check `dir C:\Tools` shows `bch.exe`, that `$env:Path -split ';' | Select-String Tools` shows the entry, and restart the terminal app fully.

## 3. Connect (first run)

You need the hub's address and **your account name**. If you don't have an account yet, the
hub owner gives you an invite code.

**With an account:**
```powershell
bch -hub 192.168.1.20 -user ayush -name work
password for ayush@192.168.1.20: ********
```
**With an invite code (creates your account):**
```powershell
bch -hub 192.168.1.20 -invite k7Qz-9mPd2 -user ayush -name work
choose a password (8+ characters): ********
```
**Public hub** (started with `-public`): use the `tls://` address and the fingerprint the operator sent you:
```powershell
bch -hub tls://203.0.113.10:7777 -user ayush -fingerprint DC:CB:53:... -name work
```
**Guest** (only if the hub allows it): `bch -hub 192.168.1.20 -token TOKEN -name work`

`-name` is just a label for this device ("work", "laptop") — it shows up in `/sessions` and in
clipboard-sync notes. The login is saved as a *session* in `config.json` (never the password), so
**every run after this, just**:
```powershell
bch
```

## 4. Using it

| Action | How |
|---|---|
| Send text | type it, Enter |
| Send a file | drag the file into the terminal window, Enter — or `/send C:\path\file.png` |
| Switch channel | `/1` `/2` … or `/c design` or `/design` |
| Send clipboard as a message | `/paste` |
| Copy a message to clipboard | `/copy 12` or `/copy` for the latest |
| Sync clipboard with your Mac | `/clip on` (here and on the Mac) — see CLIPBOARD.md |
| Download a large file | `/get 13` (files under 50 MB save automatically in the channel you're viewing) |
| Open this channel's inbox folder | `/open` |
| See who's online / all members | `/who` / `/members` |
| Leave | `/quit` or Ctrl-C |

Files you receive land in `C:\Users\you\backchannel\inbox\<channel>\`.
Drop files into `C:\Users\you\backchannel\outbox\` (even outside the app) to send them to the active
channel, or into `outbox\<channel>\` for a specific one.

## 5. Config file locations

| File | Purpose |
|---|---|
| `%USERPROFILE%\backchannel\config.json` | hub address, account, session token, device name, preferences |
| `%USERPROFILE%\backchannel\inbox\<channel>\` | files received |
| `%USERPROFILE%\backchannel\outbox\` | drop files here to auto-send |
| `%USERPROFILE%\backchannel\outbox\sent\` | moved here after upload |

Change the location for any run with `-dir "D:\backchannel"`, or permanently with
`setx BCH_DIR "D:\backchannel"` (open a new terminal after `setx`).

## 6. One-shot commands (no chat window)

```powershell
bch send file.png                 # send a file to your last active channel
bch send -ch ops "deploy is done" # send text to #ops
bch discover                      # list hubs found on this LAN
```

## 7. Auto-start at login (optional)

`service\install-windows-startup.cmd` in the repo creates a Startup shortcut that opens `bch` in
Windows Terminal. Or by hand:
```powershell
schtasks /create /tn "backchannel" /tr "wt.exe bch" /sc onlogon /rl highest
schtasks /delete /tn "backchannel" /f     # remove it later
```

## Troubleshooting

| Symptom | Fix |
|---|---|
| `'bch' is not recognized` | PATH not applied — restart terminal fully, recheck §2 |
| Hangs on "connecting…" | Wrong `-hub` IP/port, hub isn't running, or a firewall blocks TCP 7777/7778 |
| "bad username or password" | Case doesn't matter for the name; the password does. An admin can set a new one with `/admin mod NAME pass=TEMPORARY`; for the owner account, `bch serve -reset-owner` on the hub machine |
| "session expired or revoked" | You were logged out from another device or the hub rotated. Run `bch -user NAME` again |
| "hub closed the connection before hello" | Hub runs with TLS — use `-hub tls://HOST` |
| "HUB CERTIFICATE CHANGED" | Either the hub was reinstalled (get the new fingerprint, re-run with `-fingerprint`) or someone is intercepting — ask the operator before trusting it |
| Password prompt shows what I type | The terminal isn't a real console (e.g. some IDE terminals). Use Windows Terminal or PowerShell |
| SmartScreen blocks every launch | Right-click `bch.exe` → Properties → check "Unblock" at the bottom → OK |
