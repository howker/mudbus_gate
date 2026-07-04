package device

import "mbgw/internal/profile"

// evaluateQuality determines the quality tag for a point named pointName,
// given a map of already-decoded status-word values (keyed by the status
// point's name) and the device profile's quality_map rules.
//
// Simplification for this vertical slice: quality_map rules apply
// device-wide - if a rule's source status word has its bit set, the rule's
// quality is applied to every OTHER point on the device (not just points
// sharing the same instance/pipe as the status word). Per-instance quality
// scoping is a separate follow-up (see backlog) once devices have
// per-instance status registers modeled in their profiles.
func evaluateQuality(pointName string, qualityMap []profile.QualityRule, statusValues map[string]int64) (quality string, reason string) {
    for _, rule := range qualityMap {
        if rule.Source == pointName {
            // A status point does not evaluate quality against itself.
            continue
        }
        statusVal, ok := statusValues[rule.Source]
        if !ok {
            continue
        }
        bitSet := statusVal&(1<<uint(rule.Bit)) != 0
        if bitSet {
            return rule.Quality, rule.Meaning
        }
    }
    return "GOOD", ""
}