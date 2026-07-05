package quality

import "mbgw/internal/profile"

// Evaluate determines the quality tag for a point named pointName at the
// given instance ("" for non-parametric points), against qualityMap rules
// and already-decoded status source values.
//
// Matching: a rule matches if statusVal (looked up for the SAME instance
// as pointName, falling back to the device-wide "" entry when the source
// has no per-instance value) satisfies rule.Code (exact match, if set) or
// rule.Bit (bitmask test, otherwise). The first matching rule wins, in
// profile order. A status source point never evaluates quality against
// itself. No match -> Valid with no reason.
func Evaluate(pointName string, instance string, qualityMap []profile.QualityRule, sv StatusValues) (Tag, string) {
    for _, rule := range qualityMap {
        if rule.Source == pointName {
            continue
        }

        instValues, ok := sv[rule.Source]
        if !ok {
            continue
        }

        statusVal, ok := instValues[instance]
        if !ok {
            statusVal, ok = instValues[""]
            if !ok {
                continue
            }
        }

        var matched bool
        if rule.Code != nil {
            matched = statusVal == int64(*rule.Code)
        } else {
            matched = statusVal&(1<<uint(rule.Bit)) != 0
        }

        if matched {
            return Tag(rule.Quality), rule.Meaning
        }
    }
    return Valid, ""
}