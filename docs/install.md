# Install

## Platform

**Linux only.** kv-chunk-fs relies on the kernel's FUSE (via go-fuse and `fusermount3`),
Linux `getxattr`/`setxattr` calls, kernel features negotiated at mount (atomic `O_TRUNC`,
1 MiB requests, `allow_other` + `default_permissions`), systemd `Type=notify`, and
Docker's `rslave` bind propagation. macOS (macFUSE) would be a port: other xattr calls,
no Linux-only mount options, launchd instead of systemd; Windows has no FUSE (WinFsp
would need a different library). The KV-cache proxy and llama.cpp run anywhere; without
kv-chunk-fs they save plain files.

`build.sh` produces a static Linux x86-64 binary with no dependencies; FUSE mounting
needs `fusermount3` (package `fuse3`).

## Build and install

```sh
./build.sh            # vet, unit tests, static build in the golang Docker container
sudo deploy/install.sh
```

`build.sh` needs Docker, not a Go toolchain. `install.sh` installs the binary to
`/opt/kv-chunk-fs/`, enables `user_allow_other` in `/etc/fuse.conf`, installs
`/etc/kv-chunk-fs.env` (settings; never overwrites an existing copy), the systemd unit
and a drop-in ordering llama-swap after it, then restarts the service and waits for
`healthy`.

`install.sh` takes its layout from environment variables, with these defaults:

| variable | default | |
|---|---|---|
| `KVCFS_USER` | `$SUDO_USER` | the user the service runs as (owns the files) |
| `KVCFS_PARENT` | `/srv/kv` | plain parent directory; the mount is `$KVCFS_PARENT/cache` |
| `KVCFS_BACKING` | `/srv/kv-cache.backing` | backing directory (`tree/`, `store/`, `staging/`) |

## Mount propagation

Mount inside a plain parent directory (`/srv/kv/cache` inside `/srv/kv`) and have
containers bind the parent with propagation `rslave`: a bind of the mount itself would
keep the dead mount after a service restart, while a mount made under a bound directory
propagates into it. Restarting the service while models run is then fine — the
containers see the new mount at once.

The mount point must be empty or hold nothing worth keeping: the mount hides it. To move
from a plain snapshot directory, stop the llama.cpp models first and move it aside (it
is a cache; nothing is imported).

## Running in a container instead

`deploy/docker/` holds an example image and compose file (needs `/dev/fuse`,
`CAP_SYS_ADMIN`, `apparmor:unconfined` and an `rshared` bind). The host service is the
simpler choice; see `deploy/docker/README.md`.
