package agent

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Annotations a workload must carry to be reported. Opt-in by design: only
// annotated Deployments are observed.
const (
	AnnotationService = "zensu.dev/service"
	AnnotationProduct = "zensu.dev/product"
)

// DeploymentLister is the minimal Kubernetes surface the agent needs. The agent
// only ever lists Deployments — it never mutates anything.
type DeploymentLister interface {
	ListDeployments(ctx context.Context, namespace string) ([]appsv1.Deployment, error)
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

// NewClientsetLister wraps a Kubernetes clientset as a DeploymentLister.
func NewClientsetLister(client kubernetes.Interface) DeploymentLister {
	return clientsetLister{client: client}
}

// NewInClusterLister builds a DeploymentLister from in-cluster config (the
// ServiceAccount token mounted into the pod).
func NewInClusterLister() (DeploymentLister, error) {
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
// which the agent ignores.
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
