# keybase-client-webdav

Mount KBFS (`/keybase`) on macOS without Keybase's FUSE kernel extension.

Keybase's `kbfuse` kext lags behind macOS releases (see
[keybase/client#29887](https://github.com/keybase/client/issues/29887)). This
tool serves KBFS over WebDAV on localhost, backed by the `keybase fs` CLI, and
you mount it with macOS's built-in `mount_webdav`. Nothing is installed in the
kernel and the Keybase app is left untouched.

## Use

```sh
go build -o keybase-client-webdav .
./keybase-client-webdav                 # serves http://127.0.0.1:8484
scripts/mount.sh                        # mounts at /Volumes/Keybase
scripts/unmount.sh
```

Requires the Keybase app/service to be running and logged in. The `keybase`
binary is taken from `-keybase` (default
`/Applications/Keybase.app/Contents/SharedSupport/bin/keybase`) and must be an
absolute path. It is never looked up on `PATH`, and keybase child processes get
a clean `PATH`: if `PATH` contains a directory that lives on this mount, the
lookup would call back into the server and deadlock it.

**Do not put directories from this mount on your `PATH`** (or symlink
`~/.ssh`, `~/.claude`, ... into it) unless you accept that every shell command
lookup blocks while the mount is unavailable.

To start the server and keep it mounted, `scripts/install-launchd.sh <path-to-binary>`
installs two per-user launch agents:

- `com.github.miguelaferreira.keybase-client-webdav` runs the server at login.
- `com.github.miguelaferreira.keybase-client-webdav-mount` runs `scripts/automount.sh`
  at login and every 30 seconds. It does nothing while `~/Keybase` is mounted;
  otherwise it waits until the server answers and Keybase is logged in, then
  mounts. It never reads files under the mountpoint (a WebDAV mount with no
  server behind it can block readers uninterruptibly). Log:
  `~/Library/Logs/keybase-client-webdav-mount.log`.

The mountpoint defaults to `~/Keybase`; `/Volumes` is root-owned and cleared at
boot, so mounting there needs `sudo` each time.

Flags: `-addr`, `-keybase`, `-readonly`, `-token-file`, `-no-auth`, `-max-procs`.

## Security

- The server only listens on loopback and rejects non-loopback `Host` headers
  (DNS rebinding).
- macOS's WebDAV client will not do Basic auth over plain HTTP, so access is
  gated by a secret URL prefix (`http://127.0.0.1:8484/<token>/`). The token is
  generated on first run into `~/.config/keybase-client-webdav/token` (mode
  0600) and is visible in the `mount_webdav` argument list while it runs. Anyone
  who can read that file or the URL can read and write your KBFS.
- `-no-auth` serves at `/` with no secret. Any local process could then use
  your KBFS.
- `-readonly` rejects every write.

## Behavior and limits

- `/` shows `private`, `public` and `team`; the service cannot list `/keybase`
  itself.
- Reads stream through `keybase fs read`. A seek restarts the stream and skips
  forward, so random access into large files costs time proportional to the
  offset.
- Writes are buffered in a temp file and uploaded on close, so a large write is
  not visible in KBFS until the client closes the file.
- Modification times come from `keybase fs ls -l`: day resolution for files
  older than about six months, and no time zone.
- Finder metadata (`._*`, `.DS_Store`) is held in memory and never stored in
  KBFS.
- No symlink, permission, xattr or change-notification support.
- Every operation spawns a `keybase` process (at most `-max-procs` at once), so
  large directory trees are slow compared to a FUSE mount.
