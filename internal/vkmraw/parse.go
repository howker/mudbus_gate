package vkmraw

import (
	"regexp"
	"strconv"
	"strings"
)

var numericPrefixRe = regexp.MustCompile(`^[+-]?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?`)

// Field is one numeric parameter decoded from a raw VKM archive string.
// Header is the human-readable description supplied by the meter in {...}
// or <...>, when present. Unit is kept exactly as the meter reported it.
type Field struct {
	Tag    string
	Header string
	Unit   string
	Value  float64
}

// Parse extracts numeric fields in wire order. Non-numeric fields such as
// Time/NSS are ignored. Both observed header layouts are supported:
//
//	tag=<header>valueUnit
//	tag{header}=valueUnit
func Parse(raw string) []Field {
	raw = strings.TrimRight(raw, "\x00")
	entries := strings.Split(raw, ";")
	out := make([]Field, 0, len(entries))
	seen := make(map[string]bool)

	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		eq := strings.Index(entry, "=")
		if eq <= 0 {
			continue
		}

		tagPart := strings.TrimSpace(entry[:eq])
		valuePart := strings.TrimSpace(entry[eq+1:])
		tag := tagPart
		header := ""

		if br := strings.IndexAny(tagPart, "{<"); br >= 0 {
			tag = strings.TrimSpace(tagPart[:br])
			if h, ok := headerBlock(tagPart[br:]); ok {
				header = h
			}
		}
		if tag == "" || tag == "Time" || seen[tag] {
			continue
		}

		if len(valuePart) > 0 && (valuePart[0] == '{' || valuePart[0] == '<') {
			if h, rest, ok := consumeHeader(valuePart); ok {
				if header == "" {
					header = h
				}
				valuePart = strings.TrimSpace(rest)
			}
		}

		num := numericPrefixRe.FindString(valuePart)
		if num == "" {
			continue
		}
		value, err := strconv.ParseFloat(num, 64)
		if err != nil {
			continue
		}
		unit := strings.TrimSpace(valuePart[len(num):])
		seen[tag] = true
		out = append(out, Field{Tag: tag, Header: cleanHeader(header), Unit: unit, Value: value})
	}
	return out
}

// Float returns one numeric tag value from a raw VKM string.
func Float(raw, tag string) (float64, bool) {
	for _, f := range Parse(raw) {
		if f.Tag == tag {
			return f.Value, true
		}
	}
	return 0, false
}

// NumericTags returns numeric tag names in the order supplied by the meter.
func NumericTags(raw string) []string {
	fields := Parse(raw)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, f.Tag)
	}
	return out
}

func consumeHeader(s string) (header, rest string, ok bool) {
	if s == "" {
		return "", s, false
	}
	var close byte
	switch s[0] {
	case '{':
		close = '}'
	case '<':
		close = '>'
	default:
		return "", s, false
	}
	idx := strings.IndexByte(s, close)
	if idx < 0 {
		return "", s, false
	}
	return s[1:idx], s[idx+1:], true
}

func headerBlock(s string) (string, bool) {
	h, _, ok := consumeHeader(s)
	return h, ok
}

func cleanHeader(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimSpace(strings.TrimRight(s, "*"))
	return s
}
