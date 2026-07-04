package archive
var registry = map[string]ArchiveReader{}
func Register(r ArchiveReader) {
    if r == nil {
        return
    }
    registry[r.Strategy()] = r
}
func Get(name string) (ArchiveReader, bool) {
    r, ok := registry[name]
    return r, ok
}
