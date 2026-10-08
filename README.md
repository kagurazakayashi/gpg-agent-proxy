# gpg-agent-proxy

[English](README.md) | [简体中文](README.zh.md)

A tiny Windows-only bridge that makes the Windows GnuPG `gpg-agent` reachable
from a remote Linux host through `ssh -R` (`RemoteForward`), so `git` and `gpg`
on that server can sign with the YubiKey (OpenPGP card) plugged into your
Windows machine. Key material never leaves the YubiKey.

## Why this is needed

On Windows, GnuPG 2.5's `gpg-agent` has **no filesystem socket**. It listens on a
random TCP port on `127.0.0.1` and requires every client to send a **16-byte
nonce** right after connecting before it answers. The port number and the nonce
are written to:

```
%LOCALAPPDATA%\gnupg\S.gpg-agent.extra
```

(first line = port, then the 16 raw nonce bytes).

Because there is no socket file to forward, `RemoteForward <remote socket> <local socket>`
cannot work. This program listens on a plain TCP port on `127.0.0.1`, and for
every incoming connection it dials the real agent, replays the nonce, then
shovels bytes in both directions.

## How it works

```
 remote Linux host                          Windows
+-----------------------+              +--------------------------------+
| git commit -S         |              | ssh   RemoteForward:           |
|   -> gpg              |              |   remote /run/user/1000/gnupg/ |
|      -> $XDG_RUNTIME_DIR/gnupg/  <==>|   S.gpg-agent                  |
|         S.gpg-agent   |   tunnel     |            ^                   |
|         (unix socket) |              |            |                   |
+-----------------------+              |   gpg-agent-proxy              |
                                       |   127.0.0.1:16688              |
                                       |            |  TCP + 16B nonce  |
                                       |            v                   |
                                       |   gpg-agent (GnuPG 2.5)        |
                                       |            |                   |
                                       |            v                   |
                                       |   YubiKey (OpenPGP card)       |
                                       +--------------------------------+
```

The server side keeps talking to a normal Unix socket; only the Windows agent
is unusual, and this bridge hides that.

## Requirements

- Windows with GnuPG (Gpg4win) 2.4 or newer; `gpg-agent` running.
- An OpenPGP key with the signing subkey on a YubiKey (or in the agent).
- An OpenSSH client on Windows (built-in `C:\Windows\System32\OpenSSH\ssh.exe`,
  or the one bundled with Git for Windows).
- A remote Linux host with `gnupg`, `git` and a writable `/etc/ssh/sshd_config`.
- Go 1.22 or newer, only if you want to build from source.

## Build

```sh
go build -o gpg-agent-proxy.exe .
```

Optional checks and a smaller binary:

```sh
gofmt -l .                  # must print nothing
go vet ./...                # must print nothing
go build -trimpath -ldflags "-s -w" -o gpg-agent-proxy.exe .
```

The repository layout is intentionally small: `main.go` (everything) and
`go.mod`. There are no third-party dependencies.

## Run

```
gpg-agent-proxy.exe [flags]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `-listen` | `127.0.0.1:16688` | Local TCP address to listen on. The `RemoteForward` target in your ssh config must match it. Keep it on loopback. |
| `-socket` | `%LOCALAPPDATA%\gnupg\S.gpg-agent.extra` | The agent endpoint file to read. Check the real path with `gpgconf --list-dirs agent-extra-socket`. |
| `-log` | empty | Also append the console log to this file. Empty means console only. |

Example:

```bat
gpg-agent-proxy.exe
gpg-agent-proxy.exe -listen 127.0.0.1:16688 -log gpg-agent-proxy.log
```

Expected startup output (English only, timestamps are local):

```
21:04:07 gpg-agent-proxy listening on 127.0.0.1:16688
21:04:07 upstream agent endpoint file: C:\Users\you\AppData\Local\gnupg\S.gpg-agent.extra
21:04:07 gpg-agent endpoint: 127.0.0.1:64457 (16-byte nonce)
21:04:07 press Ctrl+C to stop
```

Keep the window open while you use the tunnel. `Ctrl+C` (or closing the console)
shuts down gracefully: active connections are closed, the port is released, the
process exits with code 0. Exit code 1 means it could not listen (usually
another instance is already running) or could not open the log file; exit code 2
means bad command line usage.

Note that the agent endpoint is re-read on **every** connection, so restarting
`gpg-agent` (new port, new nonce) needs no action from you.

## Local setup on Windows

1. Make sure the agent runs and sees your key:

   ```bat
   gpg-connect-agent /bye
   gpg --card-status
   gpgconf --list-dirs agent-extra-socket
   ```

2. Start the bridge and leave it running:

   ```bat
   gpg-agent-proxy.exe
   ```

   If you want it to start automatically at logon, put a shortcut to the exe
   into `shell:startup`.

3. Add a host entry to `%USERPROFILE%\.ssh\config`:

   ```
   Host myserver
       HostName 203.0.113.10
       User yashi
       IdentityFile ~/.ssh/id_ed25519
       RemoteForward /run/user/1000/gnupg/S.gpg-agent 127.0.0.1:16688
       StreamLocalBindUnlink yes
   ```

   Things to check:

   - The **left** side is a path on the **remote** host, not on Windows. Get it
     from `gpgconf --list-dirs agent-socket` on the server; on many distros it is
     `/run/user/<uid>/gnupg/S.gpg-agent`. Replace `1000` with your uid.
   - The **right** side is a plain TCP address served by this bridge. Do not put
     a Windows path here; that is exactly what cannot work.
   - `StreamLocalBindUnlink yes` lets ssh replace a stale socket left on the
     server by an earlier session.
   - If you use Git for Windows' `ssh`, paths are fine as written; if you use the
     built-in Windows `ssh.exe`, this config works as well.

4. Verify that ssh parses the file without connecting:

   ```bat
   ssh -G myserver | findstr /i remoteforward
   ```

   Expected output:

   ```
   remoteforward /run/user/1000/gnupg/S.gpg-agent [127.0.0.1]:16688
   ```

## Remote setup on Linux

Do this once, on the server you want to sign on.

1. Find the socket path the remote `gpg` will use, and use it on the left side
   of `RemoteForward`:

   ```sh
   echo "$XDG_RUNTIME_DIR"
   gpgconf --list-dirs agent-socket
   ```

2. Import your public key. The private key (the subkey on the YubiKey) must
   **not** be copied; only the public part is needed so the remote `gpg` knows
   which card key to ask for:

   ```sh
   gpg --import public-key.asc
   gpg -K --keyid-format long     # the signing key should show card-no: / #
   ```

3. Get the server's own `gpg-agent` out of the way, otherwise it keeps holding
   the socket and ssh cannot bind the forwarded one:

   ```sh
   gpgconf --kill gpg-agent
   echo no-autostart >> ~/.gnupg/gpg-agent.conf

   # On systemd distributions, also stop it from being socket-activated:
   systemctl --user mask --now gpg-agent.socket gpg-agent-ssh.socket \
       gpg-agent-extra.socket gpg-agent-browser.socket
   ```

4. Let sshd replace a stale socket (as root, once):

   ```sh
   echo 'StreamLocalBindUnlink yes' >> /etc/ssh/sshd_config
   systemctl restart sshd
   ```

5. Reconnect from Windows. During login, ssh creates the remote socket. A
   warning such as `remote port forwarding failed for listen path ...` means the
   remote agent is still holding it: redo step 3 and reconnect.

## Verify

Inside the ssh session, ask the agent that answers which version it is:

```sh
gpg-connect-agent 'GETINFO version' /bye
```

- `D 2.5.18` (your Windows GnuPG version) means the tunnel works.
- The server's own version means the forward did not take effect: check the ssh
  warning, the remote path, and step 4 above.

Other useful checks:

```sh
ls -l "$XDG_RUNTIME_DIR/gnupg/"      # S.gpg-agent should be a socket (srw-------)
gpg -K                               # your signing key, private part on the card
```

In the proxy window on Windows you should see one `connect -> ...` line and one
`closed  up=.. down=..` line per signature operation.

## Sign a commit on the server

```sh
git config --global gpg.program gpg      # never a Windows path such as C:\...
git config --global gpg.format openpgp
git config --global user.signingkey <KEYID>
git config --global commit.gpgsign true

git commit -S -m "test"
```

The PIN and touch prompts appear **on Windows**, because the agent that owns the
key runs there. If a commit appears to hang, look at the proxy window and at the
YubiKey LED.

## Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| `cannot listen on 127.0.0.1:16688: ... Only one usage of each socket address ...` | Another instance is already running. Stop it (`Ctrl+C` in its window) or use a different `-listen` and update the ssh config to match. |
| `warning: cannot read that file yet: open ...S.gpg-agent.extra: The system cannot find the file specified` | `gpg-agent` is not running, or its endpoint file lives elsewhere. Start Kleopatra or run `gpg-connect-agent /bye`, then compare with `gpgconf --list-dirs agent-extra-socket`. |
| `remote port forwarding failed for listen path /run/user/1000/gnupg/S.gpg-agent` | The remote agent still owns the socket: `gpgconf --kill gpg-agent`, add `no-autostart`, mask the systemd units, then reconnect. |
| Remote `gpg-connect-agent` reports the server's own GnuPG version | The forward is not in effect: look for the ssh warning above, check the remote path against `gpgconf --list-dirs agent-socket`, and make sure `StreamLocalBindUnlink yes` is set in `sshd_config`. |
| On Linux: `gpg: cannot run C:\Program Files\GnuPG\bin\gpg.exe: No such file or directory` | The server's git has `gpg.program` pointing at a Windows path (often copied from a Windows `.gitconfig`). Fix it: `git config --global gpg.program gpg`. |
| `gpg: skipped "KEYID": No secret key` | The public key was not imported on the server, or `user.signingkey` names another key. See step 2. |
| Nothing in the proxy window when signing | ssh is not forwarding: confirm the session has no forward warning, and that the proxy listens on the address used in `RemoteForward`. |
| Signing worked, then stopped working after a while | If `gpg-agent` was restarted, the proxy already follows the new port and nonce. Check the YubiKey is still inserted and the PIN cache expired (`default-cache-ttl`). |
| Windows firewall prompt after starting the exe | Not needed: the listener is bound to `127.0.0.1` only. You can safely deny public network access. |

## Security notes

- The bridge listens on `127.0.0.1` only. Do not change `-listen` to `0.0.0.0`:
  the agent endpoint it fronts is meant to stay local.
- It replays the agent's nonce on behalf of local callers, which is the same
  access any process on this machine already has by reading
  `S.gpg-agent.extra`. Treat the port as sensitive as your agent.
- While you are connected, the remote host can ask your agent to sign. That is
  the point of the tool; use it only with hosts you trust.
- Private keys never leave the YubiKey. Only sign requests and their results
  travel over the tunnel.

## Notes and limitations

- Windows only by design: it parses the GnuPG Windows endpoint file format.
- All console output is English, including error messages.
- The program has no third-party dependencies; `main.go` is the whole tool.
- If a future GnuPG version exposes a real Unix socket on Windows, this bridge
  becomes unnecessary and `RemoteForward <remote socket> <local socket>` can be
  used directly.
