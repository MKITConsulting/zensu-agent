package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
)

func i32(v int32) *int32 { return &v }

func deployment(ns, name, svcSlug string, ready, desired int32) *appsv1.Deployment {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.DeploymentSpec{
			Replicas: i32(desired),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: ready},
	}
	if svcSlug != "" {
		d.ObjectMeta.Annotations = map[string]string{AnnotationService: svcSlug}
	}
	return d
}

func pod(ns, name string, lbls map[string]string, restarts ...int32) *corev1.Pod {
	var cs []corev1.ContainerStatus
	for _, r := range restarts {
		cs = append(cs, corev1.ContainerStatus{RestartCount: r})
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: lbls},
		Status:     corev1.PodStatus{ContainerStatuses: cs},
	}
}

// container builds one ContainerMetrics with the given CPU (millicores) and
// memory (bytes), used to assemble fake PodMetrics in tests.
func container(name string, cpuMilli, memBytes int64) metricsv1beta1.ContainerMetrics {
	return metricsv1beta1.ContainerMetrics{
		Name: name,
		Usage: corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewMilliQuantity(cpuMilli, resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(memBytes, resource.BinarySI),
		},
	}
}

func podMetrics(ns, name string, containers ...metricsv1beta1.ContainerMetrics) metricsv1beta1.PodMetrics {
	return metricsv1beta1.PodMetrics{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Containers: containers,
	}
}

// fakeReader is a controllable ClusterReader: deployments/pods come from a fake
// clientset, while PodMetricsForSelector returns scripted CPU/memory (or a
// scripted error such as ErrMetricsAPIUnavailable) so metrics paths can be
// exercised deterministically.
type fakeReader struct {
	inner       ClusterReader
	metricsCPU  int64
	metricsMem  int64
	available   bool
	metricsErr  error
	metricsHits int
}

func (f *fakeReader) ListDeployments(ctx context.Context, ns string) ([]appsv1.Deployment, error) {
	return f.inner.ListDeployments(ctx, ns)
}

func (f *fakeReader) ListPods(ctx context.Context, ns, sel string) ([]corev1.Pod, error) {
	return f.inner.ListPods(ctx, ns, sel)
}

func (f *fakeReader) PodMetricsForSelector(_ context.Context, _, _ string) (int64, int64, bool, error) {
	f.metricsHits++
	if f.metricsErr != nil {
		return 0, 0, false, f.metricsErr
	}
	return f.metricsCPU, f.metricsMem, f.available, nil
}

func TestDeriveStatus(t *testing.T) {
	cases := []struct {
		ready, desired int32
		want           string
	}{
		{3, 3, StatusUp},
		{5, 3, StatusUp},
		{1, 2, StatusDegraded},
		{0, 2, StatusDown},
		{0, 0, StatusDown},
		{2, 0, StatusDown},
	}
	for _, c := range cases {
		if got := DeriveStatus(c.ready, c.desired); got != c.want {
			t.Errorf("DeriveStatus(%d,%d)=%s want %s", c.ready, c.desired, got, c.want)
		}
	}
}

func TestMapDeployment(t *testing.T) {
	entry, ok := MapDeployment(*deployment("default", "auth-api", "auth", 3, 3))
	if !ok {
		t.Fatal("annotated deployment should map")
	}
	if entry.Slug != "auth" || entry.Name != "auth-api" || entry.Status != StatusUp {
		t.Errorf("unexpected entry: %+v", entry)
	}
	if entry.ReadyReplicas == nil || *entry.ReadyReplicas != 3 ||
		entry.DesiredReplicas == nil || *entry.DesiredReplicas != 3 {
		t.Errorf("replica counts wrong: %+v", entry)
	}

	if _, ok := MapDeployment(*deployment("default", "db", "", 1, 1)); ok {
		t.Error("unannotated deployment must be skipped")
	}
}

func TestCollect_DedupAcrossNamespaces(t *testing.T) {
	client := fake.NewSimpleClientset(
		deployment("ns1", "auth-api", "auth", 3, 3),
		deployment("ns1", "worker", "worker", 1, 2),
		deployment("ns1", "db", "", 1, 1),             // unannotated, ignored
		deployment("ns2", "auth-api-2", "auth", 2, 2), // duplicate slug, deduped
	)
	a := New(Config{
		ProductID:  "prod",
		Namespaces: []string{"ns1", "ns2"},
		Interval:   30 * time.Second,
	}, NewClientsetLister(client, nil), &stubReporter{}, nil)

	got, err := a.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 services, got %d: %+v", len(got), got)
	}
	bySlug := map[string]ServiceHeartbeat{}
	for _, s := range got {
		bySlug[s.Slug] = s
	}
	if bySlug["auth"].Status != StatusUp {
		t.Errorf("auth status = %s", bySlug["auth"].Status)
	}
	if bySlug["worker"].Status != StatusDegraded {
		t.Errorf("worker status = %s", bySlug["worker"].Status)
	}
	if bySlug["auth"].IntervalSeconds != 30 {
		t.Errorf("interval not propagated: %d", bySlug["auth"].IntervalSeconds)
	}
}

func TestCollect_SumsRestartCounts(t *testing.T) {
	client := fake.NewSimpleClientset(
		deployment("default", "api", "api", 2, 2),
		pod("default", "api-1", map[string]string{"app": "api"}, 3),
		pod("default", "api-2", map[string]string{"app": "api"}, 1, 2),
		pod("default", "stray", map[string]string{"app": "other"}, 9),
	)
	a := New(Config{
		ProductID:  "prod",
		Namespaces: []string{"default"},
	}, NewClientsetLister(client, nil), &stubReporter{}, nil)

	got, err := a.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 service, got %d: %+v", len(got), got)
	}
	if got[0].RestartCount == nil {
		t.Fatal("RestartCount should be set when the deployment has a selector")
	}
	if *got[0].RestartCount != 6 {
		t.Errorf("RestartCount = %d, want 6 (3 + 1 + 2; stray pod with app=other excluded)", *got[0].RestartCount)
	}
}

// TestSumPodMetrics covers the raw summation across multiple containers and
// multiple pods, independent of the agent wiring.
func TestSumPodMetrics(t *testing.T) {
	items := []metricsv1beta1.PodMetrics{
		podMetrics("default", "api-1",
			container("app", 100, 200_000_000),
			container("sidecar", 50, 30_000_000),
		),
		podMetrics("default", "api-2",
			container("app", 250, 300_000_000),
		),
	}
	cpu, mem := sumPodMetrics(items)
	if cpu != 400 {
		t.Errorf("cpu = %d, want 400 (100 + 50 + 250)", cpu)
	}
	if mem != 530_000_000 {
		t.Errorf("mem = %d, want 530000000 (200M + 30M + 300M)", mem)
	}
}

// TestCollect_AttachesMetrics verifies CPU/memory are summed across multiple
// containers and multiple pods, then attached as exactly two MetricSamples with
// the correct keys and values.
func TestCollect_AttachesMetrics(t *testing.T) {
	client := fake.NewSimpleClientset(deployment("default", "api", "api", 2, 2))
	reader := &fakeReader{
		inner:      NewClientsetLister(client, nil),
		metricsCPU: 400,         // e.g. 100 + 50 + 250 across pods/containers
		metricsMem: 530_000_000, // e.g. 200M + 30M + 300M
		available:  true,
	}
	a := New(Config{ProductID: "prod", Namespaces: []string{"default"}}, reader, &stubReporter{}, nil)

	got, err := a.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 service, got %d: %+v", len(got), got)
	}
	if reader.metricsHits != 1 {
		t.Errorf("PodMetricsForSelector called %d times, want 1", reader.metricsHits)
	}
	ms := got[0].Metrics
	if len(ms) != 2 {
		t.Fatalf("want exactly 2 metric samples, got %d: %+v", len(ms), ms)
	}
	byKey := map[string]float64{}
	for _, m := range ms {
		byKey[m.Key] = m.Value
	}
	if v, ok := byKey[MetricCPUMillicores]; !ok || v != 400 {
		t.Errorf("%s = %v (present=%v), want 400", MetricCPUMillicores, v, ok)
	}
	if v, ok := byKey[MetricMemoryBytes]; !ok || v != 530_000_000 {
		t.Errorf("%s = %v (present=%v), want 530000000", MetricMemoryBytes, v, ok)
	}
}

// TestCollect_GracefulDegradeNoMetricsServer verifies that when metrics-server
// is absent (sentinel error), the heartbeat is still produced with no Metrics
// and Collect returns no error — the tick must not fail.
func TestCollect_GracefulDegradeNoMetricsServer(t *testing.T) {
	client := fake.NewSimpleClientset(deployment("default", "api", "api", 2, 2))
	reader := &fakeReader{
		inner:      NewClientsetLister(client, nil),
		metricsErr: ErrMetricsAPIUnavailable,
	}
	a := New(Config{ProductID: "prod", Namespaces: []string{"default"}}, reader, &stubReporter{}, nil)

	got, err := a.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect must not error on missing metrics-server: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 service, got %d: %+v", len(got), got)
	}
	if len(got[0].Metrics) != 0 {
		t.Errorf("Metrics must be empty when metrics-server is unavailable, got %+v", got[0].Metrics)
	}
	// Status/restart path still works.
	if got[0].Status != StatusUp {
		t.Errorf("status = %s, want up", got[0].Status)
	}
}

// TestTick_GracefulDegradeNoMetricsServer is the Tick-level counterpart: a
// heartbeat is still sent (no error) when metrics-server is missing.
func TestTick_GracefulDegradeNoMetricsServer(t *testing.T) {
	client := fake.NewSimpleClientset(deployment("default", "api", "api", 2, 2))
	reader := &fakeReader{
		inner:      NewClientsetLister(client, nil),
		metricsErr: ErrMetricsAPIUnavailable,
	}
	rep := &stubReporter{}
	a := New(Config{ProductID: "prod", Source: "test", Namespaces: []string{"default"}}, reader, rep, nil)

	if err := a.Tick(context.Background()); err != nil {
		t.Fatalf("Tick must not fail when metrics-server is missing: %v", err)
	}
	if rep.calls != 1 {
		t.Fatalf("heartbeat should still be sent once, got %d calls", rep.calls)
	}
	if len(rep.last.Services) != 1 || len(rep.last.Services[0].Metrics) != 0 {
		t.Errorf("sent batch should carry the service without metrics: %+v", rep.last.Services)
	}
}

// TestCollect_GracefulDegradeTransientError verifies a transient (non-sentinel)
// metrics error does not crash the tick and yields no metrics for that tick.
func TestCollect_GracefulDegradeTransientError(t *testing.T) {
	client := fake.NewSimpleClientset(deployment("default", "api", "api", 2, 2))
	reader := &fakeReader{
		inner:      NewClientsetLister(client, nil),
		metricsErr: errors.New("metrics-server temporarily unavailable"),
	}
	a := New(Config{ProductID: "prod", Namespaces: []string{"default"}}, reader, &stubReporter{}, nil)

	got, err := a.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect must not error on transient metrics error: %v", err)
	}
	if len(got) != 1 || len(got[0].Metrics) != 0 {
		t.Errorf("transient metrics error should yield no metrics: %+v", got)
	}
}

// TestMetricsJSONMarshal verifies a batch carrying metrics marshals to the
// agreed contract shape: metrics is an array of {key,value} objects (camelCase).
func TestMetricsJSONMarshal(t *testing.T) {
	batch := HeartbeatBatch{
		ProductID: "p",
		Services: []ServiceHeartbeat{{
			Slug:   "api",
			Status: StatusUp,
			Metrics: []MetricSample{
				{Key: MetricCPUMillicores, Value: 1234},
				{Key: MetricMemoryBytes, Value: 530000000},
			},
		}},
	}
	b, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		`"metrics":[`,
		`"key":"cpu_millicores"`,
		`"value":1234`,
		`"key":"memory_bytes"`,
		`"value":530000000`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("marshalled batch missing %q\n got: %s", want, s)
		}
	}

	// Round-trip back to confirm exact values survive.
	var rt HeartbeatBatch
	if err := json.Unmarshal(b, &rt); err != nil {
		t.Fatal(err)
	}
	rms := rt.Services[0].Metrics
	if len(rms) != 2 || rms[0].Key != MetricCPUMillicores || rms[0].Value != 1234 ||
		rms[1].Key != MetricMemoryBytes || rms[1].Value != 530000000 {
		t.Errorf("round-tripped metrics wrong: %+v", rms)
	}
}

// TestServiceHeartbeat_OmitsEmptyMetrics confirms the metrics field is omitted
// entirely (omitempty) when no samples are present — so degraded heartbeats
// don't carry an empty "metrics" key.
func TestServiceHeartbeat_OmitsEmptyMetrics(t *testing.T) {
	b, err := json.Marshal(ServiceHeartbeat{Slug: "api", Status: StatusUp})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "metrics") {
		t.Errorf("empty metrics must be omitted, got: %s", b)
	}
}

// TestPodMetricsForSelector_NilMetricsClient verifies the real lister reports
// the metrics API as unavailable (graceful sentinel) when constructed without a
// metrics client.
func TestPodMetricsForSelector_NilMetricsClient(t *testing.T) {
	r := NewClientsetLister(fake.NewSimpleClientset(), nil)
	cpu, mem, available, err := r.PodMetricsForSelector(context.Background(), "default", "app=api")
	if !errors.Is(err, ErrMetricsAPIUnavailable) {
		t.Errorf("err = %v, want ErrMetricsAPIUnavailable", err)
	}
	if available || cpu != 0 || mem != 0 {
		t.Errorf("expected unavailable zero metrics, got cpu=%d mem=%d available=%v", cpu, mem, available)
	}
}

type stubReporter struct {
	last  HeartbeatBatch
	err   error
	calls int
}

func (s *stubReporter) Send(_ context.Context, b HeartbeatBatch) error {
	s.calls++
	s.last = b
	return s.err
}

func TestTick_SendsBatch(t *testing.T) {
	client := fake.NewSimpleClientset(deployment("default", "api", "api", 2, 2))
	rep := &stubReporter{}
	a := New(Config{
		ProductID:  "prod-x",
		Source:     "test",
		Namespaces: []string{"default"},
		Interval:   60 * time.Second,
	}, NewClientsetLister(client, nil), rep, nil)

	if err := a.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rep.calls != 1 {
		t.Fatalf("calls = %d, want 1", rep.calls)
	}
	if rep.last.ProductID != "prod-x" || rep.last.Source != "test" || len(rep.last.Services) != 1 {
		t.Errorf("unexpected batch: %+v", rep.last)
	}
}

func TestTick_NoWorkloadsSkipsSend(t *testing.T) {
	rep := &stubReporter{}
	a := New(Config{Namespaces: []string{"default"}}, NewClientsetLister(fake.NewSimpleClientset(), nil), rep, nil)
	if err := a.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rep.calls != 0 {
		t.Error("must not send an empty batch")
	}
}

func TestRun_Once(t *testing.T) {
	client := fake.NewSimpleClientset(deployment("default", "api", "api", 1, 1))
	rep := &stubReporter{}
	a := New(Config{ProductID: "p", Namespaces: []string{"default"}, Interval: time.Second}, NewClientsetLister(client, nil), rep, nil)
	if err := a.Run(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if rep.calls != 1 {
		t.Errorf("once mode should send exactly one batch, got %d", rep.calls)
	}
}

func TestReporterSend(t *testing.T) {
	var gotKey string
	var gotBody HeartbeatBatch
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rep := NewReporter(srv.URL, "zsk_test", 5*time.Second)
	err := rep.Send(context.Background(), HeartbeatBatch{
		ProductID: "p",
		Services:  []ServiceHeartbeat{{Slug: "api", Status: StatusUp}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotKey != "zsk_test" {
		t.Errorf("X-API-Key = %q", gotKey)
	}
	if gotBody.ProductID != "p" || len(gotBody.Services) != 1 || gotBody.Services[0].Slug != "api" {
		t.Errorf("server received unexpected body: %+v", gotBody)
	}
}

func TestReporterSend_RejectsNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":"boom"}`))
	}))
	defer srv.Close()

	rep := NewReporter(srv.URL, "k", 5*time.Second)
	if err := rep.Send(context.Background(), HeartbeatBatch{ProductID: "p"}); err == nil {
		t.Error("expected error on 500 response")
	}
}
