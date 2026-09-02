package e2e

import (
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ray-project/kuberay/ray-operator/controllers/ray/utils"
	rayv1ac "github.com/ray-project/kuberay/ray-operator/pkg/client/applyconfiguration/ray/v1"
	. "github.com/ray-project/kuberay/ray-operator/test/support"
)

const (
	rhoaiRegistryPrefix        = "registry.redhat.io/"
	rhoaiImageDigestPattern    = `@sha256:[a-f0-9]{64}$`
	rhodsOperatorCSVNamePrefix = "rhods-operator"
	rhodsOperatorNamespace     = "redhat-ods-operator"
	kuberayOperatorDeployName  = "kuberay-operator"
	kubeRBACProxyEnvVar        = "RELATED_IMAGE_ODH_KUBE_RBAC_PROXY_IMAGE"
)

func TestRayClusterRHOAIImages(t *testing.T) {
	test := With(t)
	g := NewWithT(t)

	relatedImages := rhoaiRelatedImages(test)
	if len(relatedImages) == 0 {
		t.Skip("no rhods-operator CSV found; skipping RHOAI image validation (standalone / PR ITS)")
	}

	operator := kuberayOperatorDeployment(test, g)
	operatorImage := operator.Spec.Template.Spec.Containers[0].Image
	sidecarPin := envValue(operator.Spec.Template.Spec.Containers[0].Env, kubeRBACProxyEnvVar)
	g.Expect(sidecarPin).NotTo(BeEmpty(), "%s must be set on %s", kubeRBACProxyEnvVar, kuberayOperatorDeployName)

	assertRHOAIImage(g, "kuberay-operator", operatorImage, relatedImages)

	namespace := test.NewTestNamespace()
	rayClusterAC := rayv1ac.RayCluster("raycluster-image-validation", namespace.Name).
		WithAnnotations(map[string]string{
			utils.EnableSecureTrustedNetworkAnnotationKey: "true",
		}).
		WithSpec(NewRayClusterSpec())

	rayCluster, err := test.Client().Ray().RayV1().RayClusters(namespace.Name).Apply(test.Ctx(), rayClusterAC, TestApplyOptions)
	g.Expect(err).NotTo(HaveOccurred())
	LogWithTimestamp(t, "Created RayCluster %s/%s", rayCluster.Namespace, rayCluster.Name)

	var headPod *corev1.Pod
	g.Eventually(func(g Gomega) {
		pod, err := GetHeadPod(test, rayCluster)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(sidecarImage(pod, oidcProxySidecarName)).NotTo(BeEmpty(),
			"head pod should have an injected %s sidecar", oidcProxySidecarName)
		headPod = pod
	}, TestTimeoutMedium).Should(Succeed())

	sidecar := sidecarImage(headPod, oidcProxySidecarName)
	g.Expect(sidecar).To(Equal(sidecarPin),
		"injected %s sidecar should use %s from the operator Deployment", oidcProxySidecarName, kubeRBACProxyEnvVar)
	assertRHOAIImage(g, oidcProxySidecarName+" sidecar", sidecar, relatedImages)
}

func assertRHOAIImage(g Gomega, name, image string, relatedImages map[string]struct{}) {
	g.Expect(image).To(HavePrefix(rhoaiRegistryPrefix), "%s image %s must be hosted on %s", name, image, rhoaiRegistryPrefix)
	g.Expect(image).To(MatchRegexp(rhoaiImageDigestPattern), "%s image %s must use a sha256 digest, not a tag", name, image)
	_, ok := relatedImages[image]
	g.Expect(ok).To(BeTrue(), "%s image %s is not listed in the rhods-operator CSV relatedImages", name, image)
}

func rhoaiRelatedImages(test Test) map[string]struct{} {
	test.T().Helper()
	gvr := schema.GroupVersionResource{
		Group:    "operators.coreos.com",
		Version:  "v1alpha1",
		Resource: "clusterserviceversions",
	}
	list, err := test.Client().Dynamic().Resource(gvr).Namespace(rhodsOperatorNamespace).List(test.Ctx(), metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		test.T().Fatalf("listing CSVs in %s: %v", rhodsOperatorNamespace, err)
	}

	images := map[string]struct{}{}
	for i := range list.Items {
		item := &list.Items[i]
		if !strings.HasPrefix(item.GetName(), rhodsOperatorCSVNamePrefix) {
			continue
		}
		raw, found, err := unstructured.NestedSlice(item.Object, "spec", "relatedImages")
		if err != nil || !found {
			continue
		}
		for _, entry := range raw {
			m, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			image, _, _ := unstructured.NestedString(m, "image")
			if image != "" {
				images[image] = struct{}{}
			}
		}
	}
	return images
}

func kuberayOperatorDeployment(test Test, g Gomega) appsv1.Deployment {
	test.T().Helper()
	deploys, err := test.Client().Core().AppsV1().Deployments(metav1.NamespaceAll).List(test.Ctx(), metav1.ListOptions{
		FieldSelector: "metadata.name=" + kuberayOperatorDeployName,
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(deploys.Items).NotTo(BeEmpty(), "expected a %s Deployment", kuberayOperatorDeployName)
	return deploys.Items[0]
}

func envValue(env []corev1.EnvVar, name string) string {
	for _, e := range env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

func sidecarImage(pod *corev1.Pod, name string) string {
	for _, c := range pod.Spec.Containers {
		if c.Name == name {
			return c.Image
		}
	}
	return ""
}
