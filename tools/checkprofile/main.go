package main

import (
    "fmt"
    "os"

    "mbgw/internal/profile"
)

func main() {
    if len(os.Args) < 2 {
        fmt.Println("usage: checkprofile <path>")
        os.Exit(1)
    }
    p, err := profile.Parse(os.Args[1])
    if err != nil {
        fmt.Printf("PARSE/VALIDATE ERROR: %v\n", err)
        os.Exit(1)
    }
    fmt.Printf("OK: vendor=%s model=%s description=%s\n", p.Meta.Vendor, p.Meta.Model, p.Meta.Description)
    fmt.Printf("points=%d archives=%d quality_map=%d\n", len(p.Points), len(p.Archives), len(p.QualityMap))

    for i, pt := range p.Points {
        fmt.Printf("  point[%d]: name=%q space=%q addr=%d type=%q unit=%q\n", i, pt.Name, pt.Space, pt.Addr, pt.Type, pt.Unit)
    }
    for i, a := range p.Archives {
        fmt.Printf("  archive[%d]: id=%q strategy=%q note=%q params_count=%d record_layout_count=%d\n", i, a.ID, a.Strategy, a.Note, len(a.Params), len(a.RecordLayout))
        for j, f := range a.RecordLayout {
            fmt.Printf("    record_layout[%d]: offset=%d name=%q type=%q unit=%q crc=%v\n", j, f.Offset, f.Name, f.Type, f.Unit, f.CRC)
        }
    }
    for i, q := range p.QualityMap {
        fmt.Printf("  quality_map[%d]: source=%q bit=%d meaning=%q quality=%q\n", i, q.Source, q.Bit, q.Meaning, q.Quality)
    }
}