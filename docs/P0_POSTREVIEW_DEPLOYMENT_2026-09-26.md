# P0 post-review deployment / rollback

This P0 package changes the SQLite schema used by `es_vkm_channels`.
The migration is forward-only for the database file: old executables that expect
the historical `tag` column or the D1 four-slot table must not be started against
a database after the new migration has run.

## Before first start of the new executable

1. Stop the MBGW Windows service and confirm that it is actually stopped.
2. Make an offline copy of the SQLite database **together with its WAL sidecar
   files if they still exist**. A stopped service may still leave
   `mbgw_server.db-wal` and `mbgw_server.db-shm`; the `.db` file alone is not
   guaranteed to contain the latest committed transactions.
3. Keep the previous `mbgw.exe` together with that complete database backup
   until the new version has passed startup, archive polling and ES
   synchronization checks.

Example (adjust service name and DB path to the object):

```powershell
Stop-Service mbgw; while((Get-Service mbgw).Status -ne 'Stopped'){ Start-Sleep -Milliseconds 200 }; New-Item C:\mbgw\backup\pre_p0 -ItemType Directory -Force | Out-Null; Copy-Item C:\mbgw\mbgw_server.db* C:\mbgw\backup\pre_p0\ -Force
```

Do not copy a live SQLite database while the service is writing to it. If
`-wal`/`-shm` are absent after a clean stop, copying the `.db` alone is fine.
If they are present, keep them in the same backup set as the `.db`.

## Rollback

Rollback is **the previous executable plus the complete pre-P0 database
backup set**. Never leave WAL/SHM files created by the new database next to the
restored old `.db`: SQLite may try to apply an unrelated WAL to it.

```powershell
Stop-Service mbgw; while((Get-Service mbgw).Status -ne 'Stopped'){ Start-Sleep -Milliseconds 200 }; Remove-Item C:\mbgw\mbgw_server.db-wal,C:\mbgw\mbgw_server.db-shm -Force -ErrorAction SilentlyContinue; Copy-Item C:\mbgw\backup\pre_p0\mbgw_server.db* C:\mbgw\ -Force; Copy-Item C:\mbgw\backup\mbgw.exe C:\mbgw\mbgw.exe -Force; Start-Service mbgw
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
