package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func i32(v int32) *int32 { return &v }

func deployment(ns, name, svcSlug string, ready, desired int32) *appsv1.Deployment {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       appsv1.DeploymentSpec{Replicas: i32(desired)},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: ready},
	}
	if svcSlug != "" {
		d.ObjectMeta.Annotations = map[string]string{AnnotationService: svcSlug}
	}
	return d
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
		deployment("ns1", "db", "", 1, 1), // unannotated, ignored
		deployment("ns2", "auth-api-2", "auth", 2, 2), // duplicate slug, deduped
	)
	a := New(Config{
		ProductID:  "prod",
		Namespaces: []string{"ns1", "ns2"},
		Interval:   30 * time.Second,
	}, NewClientsetLister(client), &stubReporter{}, nil)

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
	}, NewClientsetLister(client), rep, nil)

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
	a := New(Config{Namespaces: []string{"default"}}, NewClientsetLister(fake.NewSimpleClientset()), rep, nil)
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
	a := New(Config{ProductID: "p", Namespaces: []string{"default"}, Interval: time.Second}, NewClientsetLister(client), rep, nil)
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
