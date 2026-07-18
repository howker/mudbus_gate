package sqlite

// Close releases the underlying database handle. Useful for tests that
// create a Repo on a temp file and need the file unlocked before the temp
// directory is removed (on Windows an open handle blocks deletion), and
// for clean shutdown paths. Safe to call once; the sql.DB is set up with a
// single connection (see New).
func (r *Repo) Close() error {
	return r.db.Close()
}
