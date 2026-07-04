TEMPORARY STORAGE STATUS

Current state:
- storage contract was refactored to internal/storage/storage.go + models.go
- SQLite implementation folder exists: internal/storage/sqlite/
- real SQLite repo is temporarily disabled in repo.go.disabled
- current repo_stub.go is a build-only stub for offline/vendor mode

Why:
- target backend is modernc.org/sqlite
- dependency is not vendored yet
- project uses vendor/offline build mode, so the real driver cannot be fetched now

Required next step:
- add modernc.org/sqlite to go.mod
- vendor dependency
- restore repo.go
- remove repo_stub.go

Architectural note:
This is a temporary build-stability measure during refactoring.
It is intentionally explicit to avoid fake JSON storage under SQLite naming.
