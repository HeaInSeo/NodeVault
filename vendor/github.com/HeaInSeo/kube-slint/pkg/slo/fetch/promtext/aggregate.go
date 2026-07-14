package promtext

import (
	"io"

	"github.com/HeaInSeo/kube-slint/pkg/slo/common/promkey"
)

// Aggregate adds a bare (unlabeled) key for each base metric name, summing all
// of its labeled series — e.g. reconcile_total{controller="a"}=2 and
// reconcile_total{controller="b"}=3 produce reconcile_total=5. This lets
// policy/spec metric IDs reference the base name without enumerating every
// label combination, regardless of which fetcher produced the sample.
//
// Two aggregation traps are avoided:
//   - Double counting: if a real unlabeled series already exists for a name
//     (some exporters emit both a plain total and per-label breakdowns), that
//     name is left untouched rather than overwritten by a synthesized sum.
//   - Histogram buckets / summary quantiles: series carrying a "le" or
//     "quantile" label are cumulative/positional, not additive — summing them
//     produces a meaningless value, so they are excluded from aggregation.
func Aggregate(m map[string]float64) map[string]float64 {
	hasBare := make(map[string]bool, len(m))
	for key := range m {
		name, labels, err := promkey.Parse(key)
		if err == nil && len(labels) == 0 {
			hasBare[name] = true
		}
	}

	out := make(map[string]float64, len(m))
	sums := map[string]float64{}
	for key, val := range m {
		out[key] = val

		name, labels, err := promkey.Parse(key)
		if err != nil || len(labels) == 0 || hasBare[name] {
			continue
		}
		if _, ok := labels["le"]; ok {
			continue
		}
		if _, ok := labels["quantile"]; ok {
			continue
		}
		sums[name] += val
	}
	for name, sum := range sums {
		out[name] = sum
	}
	return out
}

// ParseTextToMapWithAggregates parses Prometheus exposition text into a flat
// map and applies Aggregate, so callers can reference either the exact
// labeled key or the summed base metric name. Fetchers should prefer this
// over ParseTextToMap so that metric semantics are identical regardless of
// which fetcher (curlpod, portforward, ...) produced the sample.
func ParseTextToMapWithAggregates(r io.Reader) (map[string]float64, error) {
	base, err := ParseTextToMap(r)
	if err != nil {
		return nil, err
	}
	return Aggregate(base), nil
}
