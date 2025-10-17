package ray

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"

	"github.com/go-logr/logr"
	routev1 "github.com/openshift/api/route/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	"github.com/ray-project/kuberay/ray-operator/controllers/ray/utils"
)

// Backoff and retry constants for OIDC rollout coordination
const (
	// MediumRequeueDelay for ongoing rollouts
	MediumRequeueDelay = 10 * time.Second

	// OAuth proxy constants
	oauthProxyContainerName = "oauth-proxy"
	oauthProxyVolumeName    = "proxy-tls-secret"
	authProxyPort           = 8443
	oauthProxyPortName      = "oauth-proxy"

	oauthConfigVolumeName = "oauth-config"

	oidcProxyContainerName  = "kube-rbac-proxy"
	oidcProxyPortName       = "https"
	oauthProxyImage         = "registry.redhat.io/openshift4/ose-oauth-proxy:latest"
	oidcProxyContainerImage = "registry.redhat.io/openshift4/ose-kube-rbac-proxy-rhel9@sha256:11828cdb31cd9c1e15bc9e31c7e4669daf71c84c028cad2df5dbab68150da273"
)

// AuthenticationController is a completely independent controller that watches authentication-related
// resources (ConfigMaps, ServiceAccounts, OpenShift Routes) and manages authentication configurations for Ray clusters on Openshift.
type AuthenticationController struct {
	client.Client
	Recorder record.EventRecorder
	// configClient configclient.Interface
	Scheme *runtime.Scheme
	// routeClient  *routev1client.RouteV1Client
	options RayClusterReconcilerOptions
}

// NewAuthenticationController creates a new authentication controller
func NewAuthenticationController(mgr manager.Manager, options RayClusterReconcilerOptions) *AuthenticationController {
	return &AuthenticationController{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("authentication-controller"),
		options:  options,
	}
}

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=get;list;watch
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=operator.openshift.io,resources=kubeapiservers,verbs=get;list;watch
// +kubebuilder:rbac:groups=operator.openshift.io,resources=kubeapiservers/status,verbs=get;list;watch
// +kubebuilder:rbac:groups=config.openshift.io,resources=authentications,verbs=get;list;watch
// +kubebuilder:rbac:groups=config.openshift.io,resources=authentications/status,verbs=get;list;watch
// +kubebuilder:rbac:groups=config.openshift.io,resources=oauths,verbs=get;list;watch
// +kubebuilder:rbac:groups=config.openshift.io,resources=oauths/status,verbs=get;list;watch
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create
// +kubebuilder:rbac:groups=ray.io,resources=rayclusters,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete

// Reconcile handles authentication-related resources and manages OAuth sidecar injection
func (r *AuthenticationController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx).WithName("authentication-controller")
	logger.Info("Reconciling Authentication", "namespacedName", req.NamespacedName)

	// Get the RayCluster - this should always be a RayCluster now due to proper mapping
	rayCluster := &rayv1.RayCluster{}
	if err := r.Get(ctx, req.NamespacedName, rayCluster); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("RayCluster not found, likely deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get RayCluster")
		return ctrl.Result{RequeueAfter: MediumRequeueDelay}, err
	}

	// Skip if managed by external controller
	if manager := utils.ManagedByExternalController(rayCluster.Spec.ManagedBy); manager != nil {
		logger.Info("Skipping RayCluster managed by external controller", "managed-by", manager)
		return ctrl.Result{}, nil
	}

	// Detect the authentication mode configured in the cluster
	authMode := utils.DetectAuthenticationMode(r.options.IsOpenShift)
	logger.Info("Detected authentication mode", "mode", authMode, "cluster", rayCluster.Name)

	// Handle authentication based on detected mode
	// Both OAuth and OIDC modes use the same OIDC configuration (kube-rbac-proxy + HTTPRoute)
	var err error
	switch authMode {
	case utils.ModeIntegratedOAuth:
		logger.Info("Handling Integrated OAuth with OIDC configuration", "cluster", rayCluster.Name)
		err = r.handleOIDCConfiguration(ctx, req, authMode, logger)

	case utils.ModeOIDC:
		logger.Info("Handling OIDC", "cluster", rayCluster.Name)
		err = r.handleOIDCConfiguration(ctx, req, authMode, logger)
	}

	if err != nil {
		logger.Error(err, "Failed to handle authentication configuration")
		return ctrl.Result{RequeueAfter: MediumRequeueDelay}, err
	}

	logger.Info("Successfully reconciled authentication", "cluster", rayCluster.Name)
	// Don't requeue on success - let watches trigger next reconciliation
	return ctrl.Result{}, nil
}

// handleOIDCConfiguration configures OIDC for RayClusters (supports both OIDC and OAuth modes)
func (r *AuthenticationController) handleOIDCConfiguration(ctx context.Context, req ctrl.Request, authMode utils.AuthenticationMode, logger logr.Logger) error {
	// Try to get RayCluster
	rayCluster := &rayv1.RayCluster{}
	if err := r.Get(ctx, req.NamespacedName, rayCluster); err != nil {
		// Not a RayCluster or doesn't exist, skip
		return client.IgnoreNotFound(err)
	}

	// Check if authentication should be enabled for this cluster (supports both OAuth and OIDC modes)
	shouldEnable := utils.ShouldEnableOIDC(rayCluster, authMode) || utils.ShouldEnableOAuth(rayCluster, authMode)
	if !shouldEnable {
		logger.Info("Authentication not requested for this cluster", "cluster", rayCluster.Name, "mode", authMode)
		return r.cleanupOIDCResources(ctx, rayCluster, authMode, logger)
	}

	// Ensure ingress is enabled for httproute creation
	if err := r.ensureIngressEnabled(ctx, rayCluster, logger); err != nil {
		return fmt.Errorf("failed to ensure ingress enabled: %w", err)
	}

	// Ensure OIDC resources exist
	if err := r.ensureOIDCResources(ctx, rayCluster, authMode, logger); err != nil {
		return fmt.Errorf("failed to ensure OIDC resources: %w", err)
	}

	eventMsg := fmt.Sprintf("Authentication configured for RayCluster (mode: %s)", authMode)
	r.Recorder.Event(rayCluster, "Normal", "AuthenticationConfigured", eventMsg)
	return nil
}

// ensureIngressEnabled ensures that ingress is enabled for the RayCluster
func (r *AuthenticationController) ensureIngressEnabled(ctx context.Context, cluster *rayv1.RayCluster, logger logr.Logger) error {
	// Check if ingress is already enabled
	if cluster.Spec.HeadGroupSpec.EnableIngress != nil && *cluster.Spec.HeadGroupSpec.EnableIngress {
		logger.Info("Ingress already enabled for cluster", "cluster", cluster.Name)
		return nil
	}

	logger.Info("Enabling ingress for Auth-secured cluster", "cluster", cluster.Name)

	// Create a copy and enable ingress
	updatedCluster := cluster.DeepCopy()
	trueValue := true
	updatedCluster.Spec.HeadGroupSpec.EnableIngress = &trueValue

	// Update the cluster
	if err := r.Update(ctx, updatedCluster); err != nil {
		return fmt.Errorf("failed to enable ingress: %w", err)
	}

	logger.Info("Successfully enabled ingress for cluster", "cluster", cluster.Name)
	r.Recorder.Event(cluster, "Normal", "IngressEnabled", "Automatically enabled ingress for Auth configuration")
	return nil
}

func (r *AuthenticationController) ensureOIDCResources(ctx context.Context, cluster *rayv1.RayCluster, authMode utils.AuthenticationMode, logger logr.Logger) error {
	if err := r.ensureServiceAccount(ctx, cluster, authMode, logger); err != nil {
		return fmt.Errorf("failed to ensure service account: %w", err)
	}

	// Create HttpRoute
	if err := r.ensureHttpRoute(ctx, cluster, logger); err != nil {
		return fmt.Errorf("failed to ensure HttpRoute: %w", err)
	}

	// Create ConfigMap
	if err := r.ensureOIDCConfigMap(ctx, cluster, logger); err != nil {
		return fmt.Errorf("failed to ensure ConfigMap: %w", err)
	}

	return nil
}

func (r *AuthenticationController) cleanupOIDCResources(ctx context.Context, cluster *rayv1.RayCluster, authMode utils.AuthenticationMode, logger logr.Logger) error {
	namer := utils.NewResourceNamer(cluster)

	// Remove ConfigMap
	configMap := &corev1.ConfigMap{}
	configMapName := namer.ConfigMapName()
	if err := r.Get(ctx, client.ObjectKey{Name: configMapName, Namespace: cluster.Namespace}, configMap); err == nil {
		if err := r.Delete(ctx, configMap); err != nil {
			logger.Info("Failed to delete ConfigMap", "error", err)
		} else {
			logger.Info("Deleted ConfigMap", "configMap", configMapName)
		}
	}

	// Remove HTTPRoute
	httpRoute := &gatewayv1.HTTPRoute{}
	httpRouteName := cluster.Name
	if err := r.Get(ctx, client.ObjectKey{Name: httpRouteName, Namespace: cluster.Namespace}, httpRoute); err == nil {
		if err := r.Delete(ctx, httpRoute); err != nil {
			logger.Info("Failed to delete HTTPRoute", "error", err)
		} else {
			logger.Info("Deleted HTTPRoute", "httpRoute", httpRouteName)
		}
	}

	// Remove service account
	sa := &corev1.ServiceAccount{}
	saName := namer.ServiceAccountName(authMode)
	if err := r.Get(ctx, client.ObjectKey{Name: saName, Namespace: cluster.Namespace}, sa); err == nil {
		if err := r.Delete(ctx, sa); err != nil {
			logger.Info("Failed to delete service account", "error", err)
		} else {
			logger.Info("Deleted service account", "serviceAccount", saName, "mode", authMode)
		}
	}

	return nil
}

func (r *AuthenticationController) ensureHttpRoute(ctx context.Context, cluster *rayv1.RayCluster, logger logr.Logger) error {
	serviceName, err := utils.GenerateHeadServiceName("RayCluster", cluster.Spec, cluster.Name)
	if err != nil {
		return err
	}

	httpRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
		},
	}

	opResult, err := controllerutil.CreateOrUpdate(ctx, r.Client, httpRoute, func() error {
		// Set controller reference
		if err := controllerutil.SetControllerReference(cluster, httpRoute, r.Scheme); err != nil {
			return err
		}

		// Helper variables for pointer fields
		group := gatewayv1.Group("gateway.networking.k8s.io")
		kind := gatewayv1.Kind("Gateway")
		gatewayName := gatewayv1.ObjectName("data-science-gateway")
		namespace := gatewayv1.Namespace("openshift-ingress")
		serviceGroup := gatewayv1.Group("")
		serviceKind := gatewayv1.Kind("Service")
		weight := int32(1)
		pathExact := gatewayv1.PathMatchExact
		pathPrefix := gatewayv1.PathMatchPathPrefix
		port := gatewayv1.PortNumber(8265)
		pathValue := "/"
		prefixValue := fmt.Sprintf("/ray/%s/%s", cluster.Namespace, cluster.Name)

		// Update the HTTPRoute spec
		httpRoute.Spec = gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{
						Group:     &group,
						Kind:      &kind,
						Name:      gatewayName,
						Namespace: &namespace,
					},
				},
			},
			Rules: []gatewayv1.HTTPRouteRule{
				// Rule 1: Exact match for root path - redirect to #/
				// This handles the case when users access /ray/{namespace}/{cluster} without the trailing hash
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{
							Path: &gatewayv1.HTTPPathMatch{
								Type:  &pathExact,
								Value: &prefixValue,
							},
						},
					},
					Filters: []gatewayv1.HTTPRouteFilter{
						{
							Type: gatewayv1.HTTPRouteFilterRequestRedirect,
							RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
								Path: &gatewayv1.HTTPPathModifier{
									Type:            gatewayv1.FullPathHTTPPathModifier,
									ReplaceFullPath: ptr.To(prefixValue + "/#/"),
								},
								StatusCode: ptr.To(302), // Temporary redirect
							},
						},
					},
				},
				// Rule 2: Prefix match for all other paths (including #/)
				// This handles all sub-paths and rewrites them to the backend
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{
							Path: &gatewayv1.HTTPPathMatch{
								Type:  &pathPrefix,
								Value: &prefixValue,
							},
						},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Group: &serviceGroup,
									Kind:  &serviceKind,
									Name:  gatewayv1.ObjectName(serviceName),
									Port:  &port,
								},
								Weight: &weight,
							},
						},
					},
					Filters: []gatewayv1.HTTPRouteFilter{
						{
							Type: gatewayv1.HTTPRouteFilterURLRewrite,
							URLRewrite: &gatewayv1.HTTPURLRewriteFilter{
								Path: &gatewayv1.HTTPPathModifier{
									Type:               gatewayv1.PrefixMatchHTTPPathModifier,
									ReplacePrefixMatch: &pathValue,
								},
							},
						},
					},
				},
			},
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to create or update HTTPRoute: %w", err)
	}

	if opResult != controllerutil.OperationResultNone {
		logger.Info("HTTPRoute reconciled", "name", httpRoute.Name, "operation", opResult)
	}

	return nil
}

func (r *AuthenticationController) ensureOIDCConfigMap(ctx context.Context, cluster *rayv1.RayCluster, logger logr.Logger) error {
	namer := utils.NewResourceNamer(cluster)
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      namer.ConfigMapName(),
			Namespace: cluster.Namespace,
		},
	}

	opResult, err := controllerutil.CreateOrUpdate(ctx, r.Client, configMap, func() error {
		// Set controller reference
		if err := controllerutil.SetControllerReference(cluster, configMap, r.Scheme); err != nil {
			return err
		}

		// Build ConfigMap data dynamically
		configYAML := fmt.Sprintf(`
authorization:
  resourceAttributes:
    # For an incoming request, the proxy will check if the user
    # has the "get" verb on the "services" resource.
    verb: "get"
    resource: "services"
    # The API group and resource name should match the target Service.
    apiGroup: ""
    resourceName: "%s"
`, cluster.Name+"-head-svc")

		// Set labels and data
		if configMap.Labels == nil {
			configMap.Labels = make(map[string]string)
		}
		configMap.Labels["app"] = "kube-rbac-proxy"

		if configMap.Data == nil {
			configMap.Data = make(map[string]string)
		}
		configMap.Data["config.yaml"] = configYAML

		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to create or update OIDC ConfigMap: %w", err)
	}

	if opResult != controllerutil.OperationResultNone {
		logger.Info("OIDC ConfigMap reconciled", "name", configMap.Name, "operation", opResult)
	}

	return nil
}

// ensureServiceAccount creates or updates the service account for authentication (used for both OIDC and OAuth modes)
func (r *AuthenticationController) ensureServiceAccount(ctx context.Context, cluster *rayv1.RayCluster, authMode utils.AuthenticationMode, logger logr.Logger) error {
	namer := utils.NewResourceNamer(cluster)
	saName := namer.ServiceAccountName(authMode)
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: cluster.Namespace,
		},
	}

	opResult, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		// Set controller reference
		if err := controllerutil.SetControllerReference(cluster, sa, r.Scheme); err != nil {
			return err
		}

		// Service account doesn't need special annotations for kube-rbac-proxy
		// It uses standard Kubernetes RBAC

		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to create or update service account: %w", err)
	}

	if opResult != controllerutil.OperationResultNone {
		logger.Info("Service account reconciled", "name", saName, "operation", opResult, "mode", authMode)
	}

	return nil
}

// ensureOAuthServiceAccount creates or updates the OAuth service account
func (r *AuthenticationController) ensureOAuthServiceAccount(ctx context.Context, cluster *rayv1.RayCluster, logger logr.Logger) error {
	namer := utils.NewResourceNamer(cluster)
	saName := namer.ServiceAccountName(utils.ModeIntegratedOAuth)
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: cluster.Namespace,
		},
	}

	opResult, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		// Set controller reference
		if err := controllerutil.SetControllerReference(cluster, sa, r.Scheme); err != nil {
			return err
		}

		// Add service account annotation for OAuth
		if sa.Annotations == nil {
			sa.Annotations = make(map[string]string)
		}
		sa.Annotations["serviceaccounts.openshift.io/oauth-redirectreference.first"] = fmt.Sprintf(
			`{"kind":"OAuthRedirectReference","apiVersion":"v1","reference":{"kind":"Route","name":"%s"}}`,
			utils.GenerateRouteName(cluster.Name),
		)

		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to create or update OAuth service account: %w", err)
	}

	if opResult != controllerutil.OperationResultNone {
		logger.Info("OAuth service account reconciled", "name", saName, "operation", opResult)
	}

	return nil
}

// NOTE: Pod recreation logic removed - Kubernetes pods are immutable
// You cannot add containers to running pods, so we don't try to retrofit existing pods
// OAuth sidecar is automatically injected by RayCluster controller during pod creation
// Users must delete and recreate their RayCluster to enable OAuth on existing clusters

// generateSelfSignedCert generates a self-signed certificate for OAuth proxy
func generateSelfSignedCert(cluster *rayv1.RayCluster) ([]byte, []byte, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"KubeRay OAuth Proxy"},
			CommonName:   cluster.Name + "-oauth-proxy",
		},
		NotBefore:   time.Now(),
		NotAfter:    time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:    x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames: []string{
			"localhost",
			cluster.Name,
			fmt.Sprintf("%s.%s", cluster.Name, cluster.Namespace),
			fmt.Sprintf("%s.%s.svc", cluster.Name, cluster.Namespace),
			fmt.Sprintf("%s.%s.svc.cluster.local", cluster.Name, cluster.Namespace),
		},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return nil, nil, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	return certPEM, keyPEM, nil
}

// GetOAuthProxySidecar returns the OAuth proxy sidecar container configuration
// This can be used by the RayCluster controller to inject the sidecar
func GetOAuthProxySidecar(cluster *rayv1.RayCluster) corev1.Container {
	namer := utils.NewResourceNamer(cluster)
	return corev1.Container{
		Name:            oauthProxyContainerName,
		Image:           oauthProxyImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Ports: []corev1.ContainerPort{
			utils.CreateContainerPort(authProxyPort, oauthProxyPortName),
		},
		Args: []string{
			fmt.Sprintf("--https-address=:%d", authProxyPort),
			"--provider=openshift",
			fmt.Sprintf("--openshift-service-account=%s", namer.ServiceAccountName(utils.ModeIntegratedOAuth)),
			"--upstream=http://localhost:8265",
			"--tls-cert=/etc/tls/private/tls.crt",
			"--tls-key=/etc/tls/private/tls.key",
			"--cookie-secret=$(COOKIE_SECRET)",
			fmt.Sprintf("--openshift-delegate-urls=%s", utils.FormatOAuthDelegateURLs(cluster.Namespace)),
			"--skip-provider-button",
		},
		Env: []corev1.EnvVar{
			utils.CreateEnvVarFromSecret("COOKIE_SECRET", namer.SecretName(utils.ModeIntegratedOAuth), "cookie_secret"),
		},
		VolumeMounts: []corev1.VolumeMount{
			utils.CreateVolumeMount(oauthProxyVolumeName, "/etc/tls/private", true),
		},
		// Add resource limits to prevent excessive resource usage
		Resources: utils.StandardProxyResources(),
		// Add liveness probe to detect if OAuth proxy is healthy
		LivenessProbe: utils.CreateProbe(utils.ProbeConfig{
			Path:                "/oauth/healthz",
			Port:                authProxyPort,
			Scheme:              corev1.URISchemeHTTPS,
			InitialDelaySeconds: 30,
			TimeoutSeconds:      1,
			PeriodSeconds:       10,
			SuccessThreshold:    1,
			FailureThreshold:    3,
		}),
		// Add readiness probe to prevent routing traffic before OAuth proxy is ready
		ReadinessProbe: utils.CreateProbe(utils.ProbeConfig{
			Path:                "/oauth/healthz",
			Port:                authProxyPort,
			Scheme:              corev1.URISchemeHTTPS,
			InitialDelaySeconds: 5,
			TimeoutSeconds:      1,
			PeriodSeconds:       5,
			SuccessThreshold:    1,
			FailureThreshold:    3,
		}),
	}
}

// GetOAuthProxyVolumes returns the volumes needed for OAuth proxy sidecar
func GetOAuthProxyVolumes(cluster *rayv1.RayCluster) []corev1.Volume {
	namer := utils.NewResourceNamer(cluster)
	return []corev1.Volume{
		utils.CreateSecretVolume(oauthConfigVolumeName, namer.SecretName(utils.ModeIntegratedOAuth)),
		utils.CreateSecretVolume(oauthProxyVolumeName, namer.TLSSecretName(utils.ModeIntegratedOAuth)),
	}
}

func GetOIDCProxySidecar(cluster *rayv1.RayCluster) corev1.Container {
	namer := utils.NewResourceNamer(cluster)
	configMapName := namer.ConfigMapName()
	return corev1.Container{
		Name:            oidcProxyContainerName,
		Image:           oidcProxyContainerImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Ports: []corev1.ContainerPort{
			utils.CreateContainerPort(authProxyPort, oidcProxyPortName),
		},
		Args: []string{
			fmt.Sprintf("--secure-listen-address=0.0.0.0:%d", authProxyPort),
			"--upstream=http://127.0.0.1:8265/",
			"--config-file=/etc/kube-rbac-proxy/config.yaml",
			"--logtostderr=true",
		},
		VolumeMounts: []corev1.VolumeMount{
			utils.CreateVolumeMount(configMapName, "/etc/kube-rbac-proxy/", true),
		},
		// Add resource limits to prevent excessive resource usage
		Resources: utils.StandardProxyResources(),
	}
}

func GetOIDCProxyVolumes(cluster *rayv1.RayCluster) []corev1.Volume {
	namer := utils.NewResourceNamer(cluster)
	configMapName := namer.ConfigMapName()
	return []corev1.Volume{
		utils.CreateConfigMapVolume(configMapName, configMapName),
	}
}

// SetupWithManager sets up the controller with the Manager
func (r *AuthenticationController) SetupWithManager(mgr ctrl.Manager) error {
	// Predicate to only reconcile RayClusters when relevant changes occur
	rayClusterPredicate := predicate.Or(
		predicate.GenerationChangedPredicate{},
		predicate.AnnotationChangedPredicate{},
	)

	return ctrl.NewControllerManagedBy(mgr).
		// PRIMARY: Watch RayClusters
		For(&rayv1.RayCluster{}, builder.WithPredicates(rayClusterPredicate)).
		// OWNED: Watch resources owned by RayClusters
		Owns(&corev1.ServiceAccount{}).
		Owns(&corev1.Service{}).
		Owns(&routev1.Route{}).
		// // SECONDARY: Watch cluster-wide auth config and map to all RayClusters
		// Watches(
		// 	&configv1.Authentication{},
		// 	handler.EnqueueRequestsFromMapFunc(r.mapAuthResourceToRayClusters),
		// ).
		// Watches(
		// 	&configv1.OAuth{},
		// 	handler.EnqueueRequestsFromMapFunc(r.mapAuthResourceToRayClusters),
		// ).
		Named("authentication").
		Complete(r)
}

// mapAuthResourceToRayClusters maps cluster-wide auth config changes to all RayClusters
// This ensures all clusters are re-evaluated when authentication mode changes
func (r *AuthenticationController) mapAuthResourceToRayClusters(ctx context.Context, obj client.Object) []reconcile.Request {
	logger := ctrl.LoggerFrom(ctx)

	// List all RayClusters in all namespaces
	rayClusterList := &rayv1.RayClusterList{}
	if err := r.List(ctx, rayClusterList); err != nil {
		logger.Error(err, "Failed to list RayClusters for authentication config mapping")
		return []reconcile.Request{}
	}

	// Create reconcile requests for all clusters
	requests := make([]reconcile.Request, 0)
	for _, cluster := range rayClusterList.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      cluster.Name,
				Namespace: cluster.Namespace,
			},
		})
	}

	logger.Info("Mapping authentication config change to RayClusters",
		"authResource", obj.GetName(),
		"clusters", len(requests))

	return requests
}
