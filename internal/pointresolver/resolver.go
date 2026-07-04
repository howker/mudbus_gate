package pointresolver

import (
    "fmt"
    "strconv"
    "strings"
)

// Resolve evaluates a simple arithmetic address formula like
// "2000+(pipe-1)*100+8", substituting varName with varValue everywhere it
// appears as a standalone identifier. Supports +, -, *, parentheses, and
// integer literals - sufficient for VZLET-style parametric addressing
// (CONTRACTS.md pointresolver requirements). No division, no multiple
// variables in one formula (each profile point references exactly one
// instance).
func Resolve(formula string, varName string, varValue int) (int, error) {
    substituted := strings.ReplaceAll(formula, varName, strconv.Itoa(varValue))
    p := &parser{input: substituted}
    val, err := p.parseExpr()
    if err != nil {
        return 0, fmt.Errorf("resolve formula %q (var %s=%d): %w", formula, varName, varValue, err)
    }
    p.skipSpaces()
    if p.pos < len(p.input) {
        return 0, fmt.Errorf("resolve formula %q: unexpected trailing input at %d: %q", formula, p.pos, p.input[p.pos:])
    }
    return val, nil
}

type parser struct {
    input string
    pos   int
}

func (p *parser) skipSpaces() {
    for p.pos < len(p.input) && p.input[p.pos] == ' ' {
        p.pos++
    }
}

// parseExpr handles + and - at the lowest precedence.
func (p *parser) parseExpr() (int, error) {
    val, err := p.parseTerm()
    if err != nil {
        return 0, err
    }
    for {
        p.skipSpaces()
        if p.pos >= len(p.input) {
            break
        }
        switch p.input[p.pos] {
        case '+':
            p.pos++
            rhs, err := p.parseTerm()
            if err != nil {
                return 0, err
            }
            val += rhs
        case '-':
            p.pos++
            rhs, err := p.parseTerm()
            if err != nil {
                return 0, err
            }
            val -= rhs
        default:
            return val, nil
        }
    }
    return val, nil
}

// parseTerm handles * at higher precedence than + and -.
func (p *parser) parseTerm() (int, error) {
    val, err := p.parseFactor()
    if err != nil {
        return 0, err
    }
    for {
        p.skipSpaces()
        if p.pos >= len(p.input) || p.input[p.pos] != '*' {
            return val, nil
        }
        p.pos++
        rhs, err := p.parseFactor()
        if err != nil {
            return 0, err
        }
        val *= rhs
    }
}

// parseFactor handles parenthesized sub-expressions, unary minus, and
// integer literals.
func (p *parser) parseFactor() (int, error) {
    p.skipSpaces()
    if p.pos >= len(p.input) {
        return 0, fmt.Errorf("unexpected end of input")
    }

    if p.input[p.pos] == '-' {
        p.pos++
        v, err := p.parseFactor()
        if err != nil {
            return 0, err
        }
        return -v, nil
    }

    if p.input[p.pos] == '(' {
        p.pos++
        val, err := p.parseExpr()
        if err != nil {
            return 0, err
        }
        p.skipSpaces()
        if p.pos >= len(p.input) || p.input[p.pos] != ')' {
            return 0, fmt.Errorf("expected closing paren at %d", p.pos)
        }
        p.pos++
        return val, nil
    }

    start := p.pos
    for p.pos < len(p.input) && p.input[p.pos] >= '0' && p.input[p.pos] <= '9' {
        p.pos++
    }
    if start == p.pos {
        return 0, fmt.Errorf("expected number at %d, got %q", p.pos, p.input[p.pos:])
    }
    return strconv.Atoi(p.input[start:p.pos])
}