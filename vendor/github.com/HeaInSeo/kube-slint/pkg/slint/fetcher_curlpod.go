package slint

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/HeaInSeo/kube-slint/pkg/slo/fetch"
	"github.com/HeaInSeo/kube-slint/pkg/slo/fetch/curlpod"
	"github.com/HeaInSeo/kube-slint/pkg/slo/fetch/promtext"
)

// ---- Default Fetcher: curlpod + promtext ----

// curlPodFetcher is the fetcher implementation that runs a curl pod to
// collect /metrics. It implements fetch.SnapshotFetcher, so Session.Start()
// pre-captures a start snapshot, closing Gap G (where both of
// engine.Execute()'s two Fetch() calls would otherwise return post-workload
// state).
type curlPodFetcher struct {
	impl       *sessionImpl
	pod        *curlpod.CurlPod
	startCache *fetch.Sample // set on PreFetch() success, returned by the first Fetch()
	startErr   error         // set on PreFetch() failure, propagated as unreliable by the first Fetch()
	fetchCount int           // tracks the number of Fetch() calls
}

func newCurlPodFetcher(impl *sessionImpl) fetch.MetricsFetcher {
	client := curlpod.New(nil, nil)
	// Add the required safety label.
	client.LabelSelector = fmt.Sprintf("app.kubernetes.io/managed-by=kube-slint,slint-run-id=%s", SanitizeKubernetesLabelValue(impl.RunID))
	// Apply TLS integration knob and dangerous security-boundary opt-ins.
	//nolint:staticcheck // SA1019: intentional bridge for the deprecated legacy field, kept for backward compatibility.
	client.TLSInsecureSkipVerify = impl.TLSInsecureSkipVerify
	client.DangerouslySkipTLSVerify = impl.DangerouslySkipTLSVerify
	client.DangerouslyAllowExternalMetricsURL = impl.DangerouslyAllowExternalMetricsURL
	client.DangerouslyAllowKubeSystemNamespace = impl.DangerouslyAllowKubeSystemNamespace

	return &curlPodFetcher{
		impl: impl,
		pod: &curlpod.CurlPod{
			Client:             client,
			Namespace:          impl.Config.Namespace,
			MetricsServiceName: impl.Config.MetricsServiceName,
			ServiceAccountName: impl.Config.ServiceAccountName,
			Token:              impl.Config.Token,
			Image:              impl.CurlImage,
			ServiceURLFormat:   impl.ServiceURLFormat,
		},
	}
}

// PreFetch pre-captures a snapshot at the start of the measurement window.
// Called from Session.Start(); implements the fetch.SnapshotFetcher
// interface. On failure it leaves startCache unset and propagates the
// failure from the first Fetch() instead.
func (f *curlPodFetcher) PreFetch(ctx context.Context) error {
	raw, err := f.pod.Run(ctx, f.impl.WaitPodDoneTimeout, f.impl.LogsTimeout)
	if err != nil {
		f.startErr = err
		return err
	}
	values, err := parsePrometheusText(raw)
	if err != nil {
		f.startErr = err
		return err
	}
	s := fetch.Sample{At: time.Now(), Values: values}
	f.startCache = &s
	f.startErr = nil
	return nil
}

// Fetch retrieves a metric sample. On the first call, if PreFetch() cached
// a start snapshot, it's returned as-is; subsequent calls run the curl pod.
func (f *curlPodFetcher) Fetch(ctx context.Context, at time.Time) (fetch.Sample, error) {
	f.fetchCount++
	if f.fetchCount == 1 && f.startErr != nil {
		return fetch.Sample{}, fmt.Errorf("prefetch start snapshot failed: %w", f.startErr)
	}
	// If this is the first Fetch() call and startCache is set, return the
	// cached start snapshot — so engine.Execute()'s first Fetch(startedAt)
	// call returns pre-workload state.
	if f.fetchCount == 1 && f.startCache != nil {
		cached := *f.startCache
		cached.At = at
		return cached, nil
	}

	podCtx, cancel := context.WithTimeout(ctx, f.impl.podRunTimeout())
	defer cancel()

	raw, err := f.pod.Run(podCtx, f.impl.WaitPodDoneTimeout, f.impl.LogsTimeout)
	if err != nil {
		return fetch.Sample{}, err
	}

	values, err := parsePrometheusText(raw)
	if err != nil {
		return fetch.Sample{}, err
	}

	return fetch.Sample{
		At:     at,
		Values: values,
	}, nil
}

func parsePrometheusText(raw string) (map[string]float64, error) {
	return promtext.ParseTextToMapWithAggregates(strings.NewReader(raw))
}
