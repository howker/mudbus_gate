package quality

import (
    "testing"

    "mbgw/internal/profile"
)

func intPtr(v int) *int { return &v }

func TestEvaluate_BitMatch(t *testing.T) {
    rules := []profile.QualityRule{
        {Source: "status", Bit: 3, Meaning: "sensor_break", Quality: "INVALID"},
    }
    sv := StatusValues{"status": {"": 1 << 3}}

    tag, reason := Evaluate("flow", "", rules, sv)
    if tag != Invalid {
        t.Fatalf("want Invalid, got %v (reason=%q)", tag, reason)
    }
    if reason != "sensor_break" {
        t.Fatalf("want reason sensor_break, got %q", reason)
    }
}

func TestEvaluate_CodeMatch(t *testing.T) {
    rules := []profile.QualityRule{
        {Source: "req_status", Code: intPtr(7), Meaning: "too_large", Quality: "NO_DATA"},
    }
    sv := StatusValues{"req_status": {"": 7}}

    tag, _ := Evaluate("archive_field", "", rules, sv)
    if tag != NoData {
        t.Fatalf("want NoData, got %v", tag)
    }
}

func TestEvaluate_CodeTakesPrecedenceOverBit(t *testing.T) {
    // Bit 0 of value 7 (0b111) is set, which would match a Bit:0 rule,
    // but Code is set on this rule so bit-matching must not be used.
    rules := []profile.QualityRule{
        {Source: "req_status", Bit: 0, Code: intPtr(2), Meaning: "wrong", Quality: "INVALID"},
    }
    sv := StatusValues{"req_status": {"": 7}}

    tag, _ := Evaluate("x", "", rules, sv)
    if tag != Valid {
        t.Fatalf("want Valid (code 2 != 7, bit must be ignored since Code is set), got %v", tag)
    }
}

func TestEvaluate_NoMatchDefaultsValid(t *testing.T) {
    rules := []profile.QualityRule{
        {Source: "status", Bit: 3, Meaning: "sensor_break", Quality: "INVALID"},
    }
    sv := StatusValues{"status": {"": 0}}

    tag, reason := Evaluate("flow", "", rules, sv)
    if tag != Valid {
        t.Fatalf("want Valid, got %v", tag)
    }
    if reason != "" {
        t.Fatalf("want empty reason, got %q", reason)
    }
}

func TestEvaluate_SourcePointNeverEvaluatesAgainstItself(t *testing.T) {
    rules := []profile.QualityRule{
        {Source: "status", Bit: 0, Meaning: "self", Quality: "INVALID"},
    }
    sv := StatusValues{"status": {"": 1}}

    tag, _ := Evaluate("status", "", rules, sv)
    if tag != Valid {
        t.Fatalf("a status source point must not evaluate quality against itself, got %v", tag)
    }
}

func TestEvaluate_PerInstanceLookup(t *testing.T) {
    rules := []profile.QualityRule{
        {Source: "pipe_status", Bit: 0, Meaning: "sensor_break", Quality: "INVALID"},
    }
    sv := StatusValues{
        "pipe_status": {"1": 1, "2": 0},
    }

    tag1, _ := Evaluate("flow", "1", rules, sv)
    if tag1 != Invalid {
        t.Fatalf("instance 1: want Invalid, got %v", tag1)
    }

    tag2, _ := Evaluate("flow", "2", rules, sv)
    if tag2 != Valid {
        t.Fatalf("instance 2: want Valid, got %v", tag2)
    }
}

func TestEvaluate_FallsBackToDeviceWideWhenSourceHasNoPerInstanceValue(t *testing.T) {
    // Device-wide status source (only "" entry), point being evaluated has
    // an instance - the device-wide value must still apply.
    rules := []profile.QualityRule{
        {Source: "status", Bit: 0, Meaning: "sensor_break", Quality: "INVALID"},
    }
    sv := StatusValues{"status": {"": 1}}

    tag, _ := Evaluate("flow", "3", rules, sv)
    if tag != Invalid {
        t.Fatalf("want Invalid via device-wide fallback, got %v", tag)
    }
}

func TestEvaluate_FirstMatchingRuleWins(t *testing.T) {
    rules := []profile.QualityRule{
        {Source: "status", Bit: 0, Meaning: "first", Quality: "ESTIMATED"},
        {Source: "status", Bit: 1, Meaning: "second", Quality: "INVALID"},
    }
    sv := StatusValues{"status": {"": 0b11}} // both bits set

    tag, reason := Evaluate("flow", "", rules, sv)
    if tag != Estimated || reason != "first" {
        t.Fatalf("want first rule (Estimated/first), got %v/%q", tag, reason)
    }
}

func TestEvaluate_UnknownSourceIsIgnored(t *testing.T) {
    rules := []profile.QualityRule{
        {Source: "missing", Bit: 0, Meaning: "x", Quality: "INVALID"},
    }
    sv := StatusValues{}

    tag, _ := Evaluate("flow", "", rules, sv)
    if tag != Valid {
        t.Fatalf("want Valid when source was never collected, got %v", tag)
    }
}