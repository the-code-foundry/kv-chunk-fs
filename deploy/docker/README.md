# kv-chunk-fs in Docker (example)

The recommended deployment is the host service (`../kv-chunk-fs.service`): a user FUSE
mount needs no privileges there, and systemd orders it before llama-swap. This folder
shows the container alternative. The image builds (checked 2026-10-16) but is not what
we run.

What a container needs that the host service does not:

| | why |
|---|---|
| `/dev/fuse`, `CAP_SYS_ADMIN` | to mount at all |
| `apparmor:unconfined` (or a custom profile) | Docker's default AppArmor profile denies `mount` |
| bind of `/srv/kv` with propagation **`rshared`** | so the mount made inside the container appears on the host (and, through the models' `rslave` binds, in the model containers) |
| a shared host mount at `/srv` | `rshared` requires it; systemd makes `/` shared by default |
| start before the models, stop gracefully | a mount that disappears leaves the models with `Transport endpoint is not connected` until it is back |

```sh
docker build -f deploy/docker/Dockerfile --build-arg VERSION=$(git describe --always) -t kv-chunk-fs .
docker compose -f deploy/docker/compose.example.yml up -d      # instead of kv-chunk-fs.service
docker exec <container> head -1 /kv/cache/status
```

Do not run it next to the host service: both would mount `/srv/kv/cache`.
