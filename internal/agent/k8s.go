package agent

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// AnnotationService is the annotation a workload must carry to be reported.
// Opt-in by design: only annotated Deployments are observed.
const AnnotationService = "zensu.dev/service"

// ClusterReader is the minimal read-only Kubernetes surface the agent needs:
// list Deployments, and list the Pods behind a Deployment to sum restarts. The
// agent only ever reads — it never mutates anything.
type ClusterReader interface {
	ListDeployments(ctx context.Context, namespace string) ([]appsv1.Deployment, error)
	ListPods(ctx context.Context, namespace, selector string) ([]corev1.Pod, error)
}

type clientsetLister struct {
	client kubernetes.Interface
}

func (l clientsetLister) ListDeployments(ctx context.Context, namespace string) ([]appsv1.Deployment, error) {
	list, err := l.client.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (l clientsetLister) ListPods(ctx context.Context, namespace, selector string) ([]corev1.Pod, error) {
	list, err := l.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// NewClientsetLister wraps a Kubernetes clientset as a ClusterReader.
func NewClientsetLister(client kubernetes.Interface) ClusterReader {
	return clientsetLister{client: client}
}

// NewInClusterLister builds a ClusterReader from in-cluster config (the
// ServiceAccount token mounted into the pod).
func NewInClusterLister() (ClusterReader, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	return clientsetLister{client: client}, nil
}

// MapDeployment converts a Deployment to a ServiceHeartbeat when it carries the
// zensu.dev/service annotation. The bool is false for unannotated workloads,
// which the agent ignores. RestartCount is filled in separately from the
// Deployment's Pods (see Agent.Collect).
func MapDeployment(d appsv1.Deployment) (ServiceHeartbeat, bool) {
	slug := d.Annotations[AnnotationService]
	if slug == "" {
		return ServiceHeartbeat{}, false
	}
	ready := d.Status.ReadyReplicas
	var desired int32
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	r, de := ready, desired
	return ServiceHeartbeat{
		Slug:            slug,
		Name:            d.Name,
		Status:          DeriveStatus(ready, desired),
		ReadyReplicas:   &r,
		DesiredReplicas: &de,
	}, true
}

// deploymentSelector renders a Deployment's pod selector (MatchLabels) as a
// label-selector string. Returns "" when the Deployment has no MatchLabels, so
// callers skip pod listing rather than accidentally matching every pod.
func deploymentSelector(d appsv1.Deployment) string {
	if d.Spec.Selector == nil || len(d.Spec.Selector.MatchLabels) == 0 {
		return ""
	}
	return labels.Set(d.Spec.Selector.MatchLabels).AsSelector().String()
}

// sumRestarts totals the container restart counts across the given Pods.
func sumRestarts(pods []corev1.Pod) int32 {
	var total int32
	for _, p := range pods {
		for _, cs := range p.Status.ContainerStatuses {
			total += cs.RestartCount
		}
	}
	return total
}
