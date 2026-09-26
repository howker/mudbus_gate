# P0 post-review deployment / rollback

This P0 package changes the SQLite schema used by `es_vkm_channels`.
The migration is forward-only for the database file: old executables that expect
the historical `tag` column or the D1 four-slot table must not be started against
a database after the new migration has run.

## Before first start of the new executable

1. Stop the MBGW Windows service.
2. Make an offline copy of the SQLite database file used by the service.
3. Keep the previous `mbgw.exe` together with that database copy until the new
   version has passed startup, archive polling and ES synchronization checks.

Example (adjust service name and DB path to the object):

```powershell
Stop-Service mbgw; Copy-Item C:\mbgw\mbgw_server.db C:\mbgw\backup\mbgw_server_pre_p0.db -Force
```

Do not copy a live SQLite database while the service is writing to it.

## Rollback

Rollback is **the previous executable plus the pre-P0 database copy**:

```powershell
Stop-Service mbgw; Copy-Item C:\mbgw\backup\mbgw_server_pre_p0.db C:\mbgw\mbgw_server.db -Force; Copy-Item C:\mbgw\backup\mbgw.exe C:\mbgw\mbgw.exe -Force; Start-Service mbgw
```

Archive rows collected only after the P0 migration are not present in the
restored database. The normal bounded catch-up/backfill path must recover them
from the meters when communication is available.

## Release checks

Before production deployment, run with the real vendored dependencies and
Go 1.20.14:

- `go test ./...`
- `go vet ./...`
- full Linux `go test -race ./...`
- legacy Windows build and `go version -m` verification
- `git diff --check`

The production executable must not be built from the pre-fix `584a674` tree.
