package v1

import (
	"context"

	routev1 "github.com/openshift/api/route/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	"github.com/ray-project/kuberay/ray-operator/controllers/ray/utils"
)

// RayClusterDefaulter mutates RayClusters
type RayClusterDefaulter struct {
	RESTMapper meta.RESTMapper
}

//+kubebuilder:webhook:path=/mutate-ray-io-v1-raycluster,mutating=true,failurePolicy=fail,sideEffects=None,groups=ray.io,resources=rayclusters,verbs=create;update,versions=v1,name=mraycluster.kb.io,admissionReviewVersions=v1

var _ webhook.CustomDefaulter = &RayClusterDefaulter{}

// Default implements webhook.CustomDefaulter
func (d *RayClusterDefaulter) Default(_ context.Context, obj runtime.Object) error {
	rayCluster := obj.(*rayv1.RayCluster)

	rayclusterlog.Info("default", "name", rayCluster.Name)

	// Initialize annotations map if nil
	if rayCluster.Annotations == nil {
		rayCluster.Annotations = make(map[string]string)
	}

	// Only set the secure network annotation on OpenShift
	if d.isOpenShift() {
		rayCluster.Annotations[utils.EnableSecureTrustedNetworkAnnotationKey] = "true"
		rayclusterlog.Info("enforcing secure trusted network on OpenShift", "name", rayCluster.Name, "namespace", rayCluster.Namespace)
	}

	return nil
}

// isOpenShift checks if the cluster is running on OpenShift
func (d *RayClusterDefaulter) isOpenShift() bool {
	gvk := routev1.GroupVersion.WithKind("Route")
	_, err := d.RESTMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	return err == nil
}

// SetupRayClusterDefaulterWithManager registers the defaulting webhook for RayCluster
func SetupRayClusterDefaulterWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&rayv1.RayCluster{}).
		WithDefaulter(&RayClusterDefaulter{
			RESTMapper: mgr.GetRESTMapper(),
		}).
		Complete()
}
