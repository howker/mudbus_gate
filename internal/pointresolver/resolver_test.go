package pointresolver

import "testing"

func TestResolve_VKMPipeFormula(t *testing.T) {
    // Massovyj raskhod for pipe 1: 2000+(1-1)*100+8 = 2008
    got, err := Resolve("2000+(pipe-1)*100+8", "pipe", 1)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if got != 2008 {
        t.Fatalf("expected 2008, got %d", got)
    }
}

func TestResolve_VKMPipeFormula_Pipe3(t *testing.T) {
    // Pipe 3: 2000+(3-1)*100+8 = 2208
    got, err := Resolve("2000+(pipe-1)*100+8", "pipe", 3)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if got != 2208 {
        t.Fatalf("expected 2208, got %d", got)
    }
}

func TestResolve_LogicalInputFormula(t *testing.T) {
    // 4000+(logical_input-1)*4+2, input 1: 4000+0*4+2 = 4002
    got, err := Resolve("4000+(logical_input-1)*4+2", "logical_input", 1)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if got != 4002 {
        t.Fatalf("expected 4002, got %d", got)
    }
}

func TestResolve_InvalidFormula(t *testing.T) {
    if _, err := Resolve("2000+(pipe-1", "pipe", 1); err == nil {
        t.Fatal("expected error for malformed formula, got nil")
    }
}