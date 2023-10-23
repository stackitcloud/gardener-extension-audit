package controller

import (
	"context"
	"fmt"
	"path"
	"time"

	"github.com/gardener/gardener/extensions/pkg/controller/extension"

	"github.com/gardener/gardener/extensions/pkg/controller"
	extensionssecretsmanager "github.com/gardener/gardener/extensions/pkg/util/secret/manager"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	"github.com/gardener/gardener/pkg/client/kubernetes"
	"github.com/gardener/gardener/pkg/extensions"
	gutil "github.com/gardener/gardener/pkg/utils/gardener"
	kutil "github.com/gardener/gardener/pkg/utils/kubernetes"
	"github.com/gardener/gardener/pkg/utils/managedresources"
	"github.com/gardener/gardener/pkg/utils/secrets"
	secretsmanager "github.com/gardener/gardener/pkg/utils/secrets/manager"
	"github.com/go-logr/logr"
	"github.com/metal-stack/gardener-extension-audit/pkg/apis/audit/v1alpha1"
	"github.com/metal-stack/gardener-extension-audit/pkg/apis/config"
	"github.com/metal-stack/gardener-extension-audit/pkg/imagevector"
	"github.com/metal-stack/metal-lib/pkg/pointer"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configlatest "k8s.io/client-go/tools/clientcmd/api/latest"
	configv1 "k8s.io/client-go/tools/clientcmd/api/v1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	defaultAuditPolicy = `
apiVersion: audit.k8s.io/v1
kind: Policy
rules:
  # The following requests were manually identified as high-volume and low-risk,
  # so drop them.
  - level: None
    resources:
      - group: ""
        resources:
          - endpoints
          - services
          - services/status
    users:
      - 'system:kube-proxy'
    verbs:
      - watch
  - level: None
    resources:
      - group: ""
        resources:
          - nodes
          - nodes/status
    userGroups:
      - 'system:nodes'
    verbs:
      - get
  - level: None
    namespaces:
      - kube-system
    resources:
      - group: ""
        resources:
          - endpoints
    users:
      - 'system:kube-controller-manager'
      - 'system:kube-scheduler'
      - 'system:serviceaccount:kube-system:endpoint-controller'
    verbs:
      - get
      - update
  - level: None
    resources:
      - group: ""
        resources:
          - namespaces
          - namespaces/status
          - namespaces/finalize
    users:
      - 'system:apiserver'
    verbs:
      - get
  # Don't log HPA fetching metrics.
  - level: None
    resources:
      - group: metrics.k8s.io
    users:
      - 'system:kube-controller-manager'
    verbs:
      - get
      - list
  # Don't log these read-only URLs.
  - level: None
    nonResourceURLs:
      - '/healthz*'
      - /version
      - '/swagger*'
  # Don't log events requests.
  - level: None
    resources:
      - group: ""
        resources:
          - events
  # node and pod status calls from nodes are high-volume and can be large, don't log responses for expected updates from nodes
  - level: Request
    omitStages:
      - RequestReceived
    resources:
      - group: ""
        resources:
          - nodes/status
          - pods/status
    users:
      - kubelet
      - 'system:node-problem-detector'
      - 'system:serviceaccount:kube-system:node-problem-detector'
    verbs:
      - update
      - patch
  - level: Request
    omitStages:
      - RequestReceived
    resources:
      - group: ""
        resources:
          - nodes/status
          - pods/status
    userGroups:
      - 'system:nodes'
    verbs:
      - update
      - patch
  # deletecollection calls can be large, don't log responses for expected namespace deletions
  - level: Request
    omitStages:
      - RequestReceived
    users:
      - 'system:serviceaccount:kube-system:namespace-controller'
    verbs:
      - deletecollection
  # Secrets, ConfigMaps, and TokenReviews can contain sensitive & binary data,
  # so only log at the Metadata level.
  - level: Metadata
    omitStages:
      - RequestReceived
    resources:
      - group: ""
        resources:
          - secrets
          - configmaps
      - group: authentication.k8s.io
        resources:
          - tokenreviews
  # Get repsonses can be large; skip them.
  - level: Request
    omitStages:
      - RequestReceived
    resources:
      - group: ""
      - group: admissionregistration.k8s.io
      - group: apiextensions.k8s.io
      - group: apiregistration.k8s.io
      - group: apps
      - group: authentication.k8s.io
      - group: authorization.k8s.io
      - group: autoscaling
      - group: batch
      - group: certificates.k8s.io
      - group: extensions
      - group: metrics.k8s.io
      - group: networking.k8s.io
      - group: policy
      - group: rbac.authorization.k8s.io
      - group: scheduling.k8s.io
      - group: settings.k8s.io
      - group: storage.k8s.io
    verbs:
      - get
      - list
      - watch
  # Default level for known APIs
  - level: RequestResponse
    omitStages:
      - RequestReceived
    resources:
      - group: ""
      - group: admissionregistration.k8s.io
      - group: apiextensions.k8s.io
      - group: apiregistration.k8s.io
      - group: apps
      - group: authentication.k8s.io
      - group: authorization.k8s.io
      - group: autoscaling
      - group: batch
      - group: certificates.k8s.io
      - group: extensions
      - group: metrics.k8s.io
      - group: networking.k8s.io
      - group: policy
      - group: rbac.authorization.k8s.io
      - group: scheduling.k8s.io
      - group: settings.k8s.io
      - group: storage.k8s.io
  # Default level for all other requests.
  - level: Metadata
    omitStages:
      - RequestReceived
`
)

// NewActuator returns an actuator responsible for Extension resources.
func NewActuator(config config.ControllerConfiguration) extension.Actuator {
	return &actuator{
		config: config,
	}
}

type actuator struct {
	client  client.Client
	decoder runtime.Decoder
	config  config.ControllerConfiguration
}

// InjectClient injects the controller runtime client into the reconciler.
func (a *actuator) InjectClient(client client.Client) error {
	a.client = client
	return nil
}

// InjectScheme injects the given scheme into the reconciler.
func (a *actuator) InjectScheme(scheme *runtime.Scheme) error {
	a.decoder = serializer.NewCodecFactory(scheme, serializer.EnableStrict).UniversalDecoder()
	return nil
}

// Reconcile the Extension resource.
func (a *actuator) Reconcile(ctx context.Context, log logr.Logger, ex *extensionsv1alpha1.Extension) error {
	namespace := ex.GetNamespace()

	cluster, err := controller.GetCluster(ctx, a.client, namespace)
	if err != nil {
		return err
	}

	auditConfig := &v1alpha1.AuditConfig{}
	if ex.Spec.ProviderConfig != nil {
		if _, _, err := a.decoder.Decode(ex.Spec.ProviderConfig.Raw, nil, auditConfig); err != nil {
			return fmt.Errorf("failed to decode provider config: %w", err)
		}
	}

	if auditConfig.Persistence == nil {
		auditConfig.Persistence = &v1alpha1.AuditPersistence{}
	}

	if auditConfig.Persistence.Size == nil {
		auditConfig.Persistence.Size = pointer.Pointer("1Gi")
	}

	if auditConfig.AuditPolicy == nil {
		auditConfig.AuditPolicy = pointer.Pointer(defaultAuditPolicy)
	}

	if auditConfig.Backends == nil {
		auditConfig.Backends = &v1alpha1.AuditBackends{
			Log: &v1alpha1.AuditBackendLog{
				Enabled: true,
			},
		}
	}

	if err := a.createResources(ctx, log, auditConfig, cluster, namespace); err != nil {
		return err
	}

	return nil
}

// Delete the Extension resource.
func (a *actuator) Delete(ctx context.Context, log logr.Logger, ex *extensionsv1alpha1.Extension) error {
	return a.deleteResources(ctx, log, ex.GetNamespace())
}

// Restore the Extension resource.
func (a *actuator) Restore(ctx context.Context, log logr.Logger, ex *extensionsv1alpha1.Extension) error {
	return a.Reconcile(ctx, log, ex)
}

// Migrate the Extension resource.
func (a *actuator) Migrate(ctx context.Context, log logr.Logger, ex *extensionsv1alpha1.Extension) error {
	return nil
}

func (a *actuator) createResources(ctx context.Context, log logr.Logger, auditConfig *v1alpha1.AuditConfig, cluster *extensions.Cluster, namespace string) error {
	const (
		caName                        = "ca-audittailer"
		auditForwaderAccessSecretName = gutil.SecretNamePrefixShootAccess + "audit-cluster-forwarding-vpn-gateway"
	)

	shootAccessSecret := gutil.NewShootAccessSecret(auditForwaderAccessSecretName, namespace)
	if err := shootAccessSecret.Reconcile(ctx, a.client); err != nil {
		return err
	}

	secretConfigs := []extensionssecretsmanager.SecretConfigWithOptions{
		{
			Config: &secrets.CertificateSecretConfig{
				Name:       caName,
				CommonName: caName,
				CertType:   secrets.CACert,
			},
			Options: []secretsmanager.GenerateOption{secretsmanager.Persist()},
		},
		{
			Config: &secrets.CertificateSecretConfig{
				Name:       "audittailer-server",
				CommonName: "audittailer",
				DNSNames:   kutil.DNSNamesForService("audittailer", "audit"),
				CertType:   secrets.ServerCert,
			},
			Options: []secretsmanager.GenerateOption{secretsmanager.SignedByCA(caName, secretsmanager.UseCurrentCA)},
		},
		{
			Config: &secrets.CertificateSecretConfig{
				Name:       "audittailer-client",
				CommonName: "audittailer",
				DNSNames:   kutil.DNSNamesForService("audittailer", "audit"),
				CertType:   secrets.ClientCert,
			},
			Options: []secretsmanager.GenerateOption{secretsmanager.SignedByCA(caName, secretsmanager.UseCurrentCA)},
		},
	}

	sm, err := extensionssecretsmanager.SecretsManagerForCluster(ctx, log, clock.RealClock{}, a.client, cluster, "audit", secretConfigs)
	if err != nil {
		return err
	}

	secrets, err := extensionssecretsmanager.GenerateAllSecrets(ctx, sm, secretConfigs)
	if err != nil {
		return err
	}

	shootObjects, err := shootObjects()
	if err != nil {
		return err
	}

	seedObjects, err := seedObjects(auditConfig, secrets, cluster, shootAccessSecret.Secret.Name, namespace)
	if err != nil {
		return err
	}

	shootResources, err := managedresources.NewRegistry(kubernetes.ShootScheme, kubernetes.ShootCodec, kubernetes.ShootSerializer).AddAllAndSerialize(shootObjects...)
	if err != nil {
		return err
	}

	seedResources, err := managedresources.NewRegistry(kubernetes.SeedScheme, kubernetes.SeedCodec, kubernetes.SeedSerializer).AddAllAndSerialize(seedObjects...)
	if err != nil {
		return err
	}

	if err := managedresources.CreateForShoot(ctx, a.client, namespace, v1alpha1.ShootAuditResourceName, "audit-extension", false, shootResources); err != nil {
		return err
	}

	log.Info("managed resource created successfully", "name", v1alpha1.ShootAuditResourceName)

	if err := managedresources.CreateForSeed(ctx, a.client, namespace, v1alpha1.SeedAuditResourceName, false, seedResources); err != nil {
		return err
	}

	log.Info("managed resource created successfully", "name", v1alpha1.SeedAuditResourceName)

	return nil
}

func (a *actuator) deleteResources(ctx context.Context, log logr.Logger, namespace string) error {
	log.Info("deleting managed resource for registry cache")

	if err := managedresources.Delete(ctx, a.client, namespace, v1alpha1.ShootAuditResourceName, false); err != nil {
		return err
	}

	if err := managedresources.Delete(ctx, a.client, namespace, v1alpha1.SeedAuditResourceName, false); err != nil {
		return err
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	if err := managedresources.WaitUntilDeleted(timeoutCtx, a.client, namespace, v1alpha1.ShootAuditResourceName); err != nil {
		return err
	}

	if err := managedresources.WaitUntilDeleted(timeoutCtx, a.client, namespace, v1alpha1.SeedAuditResourceName); err != nil {
		return err
	}

	return nil
}

func seedObjects(auditConfig *v1alpha1.AuditConfig, secrets map[string]*corev1.Secret, cluster *extensions.Cluster, shootAccessSecretName, namespace string) ([]client.Object, error) {
	// grcImage, err := imagevector.ImageVector().FindImage("group-rolebinding-controller")
	// if err != nil {
	// 	return nil, fmt.Errorf("failed to find group-rolebinding-controller image: %w", err)
	// }

	kubeconfig, err := webhookKubeconfig(namespace)
	if err != nil {
		return nil, fmt.Errorf("unable to generate webhook kubeconfig: %w", err)
	}

	size, err := resource.ParseQuantity(pointer.SafeDeref(auditConfig.Persistence.Size))
	if err != nil {
		return nil, fmt.Errorf("unable to parse persistence size as kubernetes quantity: %w", err)
	}

	fluentbitConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fluent-bit-config",
			Namespace: namespace,
		},
		Data: map[string]string{
			"fluent-bit.conf": `[INPUT]
    Name            http

@INCLUDE *.backend.conf
`,
		},
	}

	objects := []client.Object{
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "audit-webhook-config",
				Namespace: namespace,
			},
			StringData: map[string]string{
				"audit-webhook-config.yaml": string(kubeconfig),
			},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "audit-policy",
				Namespace: namespace,
			},
			Data: map[string]string{
				"audit-policy.yaml": *auditConfig.AuditPolicy,
			},
		},
		fluentbitConfigMap,
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "audit-webhook-backend",
				Namespace: namespace,
			},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{
					"app": "audit-webhook-backend",
				},
				Ports: []corev1.ServicePort{
					{
						Name:     "http",
						Port:     9880,
						Protocol: corev1.ProtocolTCP,
					},
				},
			},
		},
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "audit-webhook-backend",
				Namespace: namespace,
			},
			Spec: appsv1.StatefulSetSpec{
				Replicas:    pointer.Pointer(int32(2)),
				ServiceName: "audit-webhook-backend",
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app": "audit-webhook-backend",
					},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app": "audit-webhook-backend",
							"networking.gardener.cloud/from-prometheus":    "allowed",
							"networking.gardener.cloud/to-dns":             "allowed",
							"networking.gardener.cloud/to-public-networks": "allowed",
						},
						Annotations: map[string]string{
							"scheduler.alpha.kubernetes.io/critical-pod": "",
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "fluent-bit",
								Image: "fluent/fluent-bit:2.1.10", // TODO: factor out to image vector
								Args: []string{
									"--storage_path=/data",
									"--config=/config/fluent-bit.conf",
								},
								VolumeMounts: []corev1.VolumeMount{
									{
										Name:      "config",
										MountPath: "/config",
									},
									{
										Name:      "audit-data",
										MountPath: "/data",
									},
								},
							},
						},
						Volumes: []corev1.Volume{
							{
								Name: "config",
								VolumeSource: corev1.VolumeSource{
									ConfigMap: &corev1.ConfigMapVolumeSource{
										LocalObjectReference: corev1.LocalObjectReference{
											Name: "fluent-bit-config",
										},
									},
								},
							},
						},
					},
				},
				VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "audit-data",
						},
						Spec: corev1.PersistentVolumeClaimSpec{
							AccessModes: []corev1.PersistentVolumeAccessMode{
								corev1.ReadWriteOnce,
							},
							StorageClassName: auditConfig.Persistence.StorageClassName,
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceStorage: size,
								},
							},
						},
					},
				},
			},
		},
		&networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "allow-to-audit-webhook-backend-from-kube-apiserver",
				Namespace: namespace,
			},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app": "audit-webhook-backend",
					},
				},
				Ingress: []networkingv1.NetworkPolicyIngressRule{
					{
						From: []networkingv1.NetworkPolicyPeer{
							{
								PodSelector: &metav1.LabelSelector{
									MatchLabels: map[string]string{
										"app":  "kubernetes",
										"role": "apiserver",
									},
								},
							},
						},
					},
				},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			},
		},
		&networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "allow-from-kube-apiserver-to-audit-webhook-backend",
				Namespace: namespace,
			},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app":  "kubernetes",
						"role": "apiserver",
					},
				},
				Egress: []networkingv1.NetworkPolicyEgressRule{
					{
						To: []networkingv1.NetworkPolicyPeer{
							{
								PodSelector: &metav1.LabelSelector{
									MatchLabels: map[string]string{
										"app": "audit-webhook-backend",
									},
								},
							},
						},
					},
				},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			},
		},
	}

	if pointer.SafeDeref(auditConfig.Backends.Log).Enabled {
		fluentbitConfigMap.Data["log.backend.conf"] = `[OUTPUT]
    Name                  stdout
    Match                 audit
`
	}

	if pointer.SafeDeref(auditConfig.Backends.ClusterForwarding).Enabled {
		fluentbitConfigMap.Data["log.backend.conf"] = `[OUTPUT]
    Name                  forward
    Match                 audit
    Host                  audit-cluster-forwarding-vpn-gateway
    Port                  9090
    Require_ack_response  True
    Compress              gzip
    tls                   On
    tls.verify            On
    tls.debug             2
    tls.ca_file           /certs/cluster-forwarding/ca.crt
    tls.crt_file          /certs/cluster-forwarding/tls.crt
    tls.key_file          /certs/cluster-forwarding/tls.key
    tls.vhost             audittailer
`
		auditForwarder := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "audit-cluster-forwarding-vpn-gateway",
				Namespace: namespace,
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: pointer.Pointer(int32(1)),
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app": "audit-cluster-forwarding-vpn-gateway",
					},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app":                              "audit-cluster-forwarding-vpn-gateway",
							"networking.gardener.cloud/to-dns": "allowed",
							"networking.gardener.cloud/to-shoot-apiserver": "allowed",
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "audit-forwarder",
								Image: "ghcr.io/metal-stack/audit-forwarder:latest", // TODO: use from image vector
								Env: []corev1.EnvVar{
									{
										Name:  "AUDIT_KUBECFG",
										Value: path.Join(gutil.VolumeMountPathGenericKubeconfig, "kubeconfig"),
									},
									{
										Name:  "AUDIT_NAMESPACE",
										Value: "audit",
									},
									{
										Name:  "AUDIT_SERVICE_NAME",
										Value: "audittailer",
									},
									{
										Name:  "AUDIT_SECRET_NAME",
										Value: secrets["audittailer-client"].Name,
									},
									{
										Name:  "AUDIT_TLS_CA_FILE",
										Value: "ca.crt",
									},
									{
										Name:  "AUDIT_TLS_CRT_FILE",
										Value: "tls.crt",
									},
									{
										Name:  "AUDIT_TLS_KEY_FILE",
										Value: "tls.key",
									},
									{
										Name:  "AUDIT_TLS_VHOST",
										Value: "audittailer",
									},
								},
							},
						},
					},
				},
			},
		}

		if err := gutil.InjectGenericKubeconfig(auditForwarder, extensions.GenericTokenKubeconfigSecretNameFromCluster(cluster), shootAccessSecretName); err != nil {
			return nil, err
		}

		objects = append(objects, auditForwarder)
	}

	return objects, nil

	// func (vp *valuesProvider) audittailerSecretConfigs() []extensionssecretsmanager.SecretConfigWithOptions {
	// 	if !vp.controllerConfig.ClusterAudit.Enabled {
	// 		return nil
	// 	}

	// 	const auditTailerCAName = "ca-provider-metal-audittailer"
	// 	return []extensionssecretsmanager.SecretConfigWithOptions{
	// 		{
	// 			Config: &secrets.CertificateSecretConfig{
	// 				Name:       auditTailerCAName,
	// 				CommonName: auditTailerCAName,
	// 				CertType:   secrets.CACert,
	// 			},
	// 			Options: []secretsmanager.GenerateOption{secretsmanager.Persist()},
	// 		},
	// 		{
	// 			Config: &secrets.CertificateSecretConfig{
	// 				Name:                        metal.AudittailerClientSecretName,
	// 				CommonName:                  "audittailer",
	// 				DNSNames:                    []string{"audittailer"},
	// 				Organization:                []string{"audittailer-client"},
	// 				CertType:                    secrets.ClientCert,
	// 				SkipPublishingCACertificate: false,
	// 			},
	// 			Options: []secretsmanager.GenerateOption{secretsmanager.SignedByCA(auditTailerCAName, secretsmanager.UseCurrentCA)},
	// 		},
	// 		{
	// 			Config: &secrets.CertificateSecretConfig{
	// 				Name:                        metal.AudittailerServerSecretName,
	// 				CommonName:                  "audittailer",
	// 				DNSNames:                    []string{"audittailer"},
	// 				Organization:                []string{"audittailer-server"},
	// 				CertType:                    secrets.ServerCert,
	// 				SkipPublishingCACertificate: false,
	// 			},
	// 			Options: []secretsmanager.GenerateOption{secretsmanager.SignedByCA(auditTailerCAName, secretsmanager.UseCurrentCA)},
	// 		},
	// 	}
	// }

	// // AuditPolicyName is the name of the configmap containing the audit policy.
	// AuditPolicyName = "audit-policy-override"
	// // AudittailerNamespace is the namespace where the audit tailer will get deployed.
	// AudittailerNamespace = "audit"
	// // AudittailerClientSecretName is the name of the secret containing the certificates for the audittailer client.
	// AudittailerClientSecretName = "audittailer-client" // nolint:gosec
	// // AudittailerServerSecretName is the name of the secret containing the certificates for the audittailer server.
	// AudittailerServerSecretName = "audittailer-server" // nolint:gosec
	// // AuditForwarderSplunkConfigName is the name of the configmap containing the splunk configuration for the auditforwarder.
	// AuditForwarderSplunkConfigName = "audit-to-splunk-config"
	// // AuditForwarderSplunkSecretName is the name of the secret containing the splunk hec token and, if required, the ca certificate.
	// AuditForwarderSplunkSecretName = "audit-to-splunk-secret" // nolint:gosec
	// return nil, nil
}

func webhookKubeconfig(namespace string) ([]byte, error) {
	var (
		contextName = "audit-webhook"
		url         = fmt.Sprintf("http://audit-webhook-backend.%s.svc.cluster.local:9880/audit", namespace)
	)

	config := &configv1.Config{
		CurrentContext: contextName,
		Clusters: []configv1.NamedCluster{
			{
				Name: contextName,
				Cluster: configv1.Cluster{
					Server: url,
				},
			},
		},
		Contexts: []configv1.NamedContext{
			{
				Name: contextName,
				Context: configv1.Context{
					Cluster:  contextName,
					AuthInfo: contextName,
				},
			},
		},
		AuthInfos: []configv1.NamedAuthInfo{
			{
				Name:     contextName,
				AuthInfo: configv1.AuthInfo{},
			},
		},
	}

	kubeconfig, err := runtime.Encode(configlatest.Codec, config)
	if err != nil {
		return nil, fmt.Errorf("unable to encode webhook kubeconfig: %w", err)
	}

	return kubeconfig, nil
}

func shootObjects() ([]client.Object, error) {
	audittailerImage, err := imagevector.ImageVector().FindImage("audittailer")
	if err != nil {
		return nil, fmt.Errorf("failed to find audittailer image: %w", err)
	}

	return []client.Object{
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "audit",
				Labels: map[string]string{
					"k8s-app": "audittailer",
				},
			},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "audittailer-config",
				Namespace: "audit",
				Labels: map[string]string{
					"app.kubernetes.io/name": "audittailer",
				},
			},
			Data: map[string]string{
				"fluent.conf": `
<source>
	@type forward
	port 24224
	bind 0.0.0.0
	<transport tls>
	ca_path                   /fluentd/etc/ssl/ca.crt
	cert_path                 /fluentd/etc/ssl/tls.crt
	private_key_path          /fluentd/etc/ssl/tls.key
	client_cert_auth          true
	</transport>
</source>
<match **>
	@type stdout
	<buffer>
	@type file
	path /fluentbuffer/auditlog-*
	chunk_limit_size          256Mb
	</buffer>
	<format>
	@type json
	</format>
</match>
`,
			},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "audittailer",
				Namespace: "audit",
				Labels: map[string]string{
					"k8s-app": "audittailer",
				},
			},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"k8s-app": "audittailer",
					},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"k8s-app": "audittailer",
							"app":     "audittailer",
						},
					},
					Spec: corev1.PodSpec{
						AutomountServiceAccountToken: pointer.Pointer(false),
						Containers: []corev1.Container{
							{
								Name:            "audittailer",
								Image:           audittailerImage.String(),
								ImagePullPolicy: corev1.PullIfNotPresent,
								Env: []corev1.EnvVar{
									{
										// this is supposed to limit fluentd memory usage. See https://docs.fluentd.org/deployment/performance-tuning-single-process#reduce-memory-usage.
										Name:  "RUBY_GC_HEAP_OLDOBJECT_LIMIT_FACTOR",
										Value: "1.2",
									},
								},
								Ports: []corev1.ContainerPort{
									{
										ContainerPort: int32(24224),
										Protocol:      corev1.ProtocolTCP,
									},
								},
								VolumeMounts: []corev1.VolumeMount{
									{
										Name:      "fluentd-config",
										MountPath: "/fluentd/etc",
									},
									{
										Name:      "fluentd-certs",
										MountPath: "/fluentd/etc/ssl",
									},
									{
										Name:      "fluentbuffer",
										MountPath: "/fluentbuffer",
									},
								},
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("100m"),
										corev1.ResourceMemory: resource.MustParse("200Mi"),
									},
									Limits: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("150m"),
										corev1.ResourceMemory: resource.MustParse("512Mi"),
									},
								},
								SecurityContext: &corev1.SecurityContext{
									RunAsUser:                pointer.Pointer(int64(65534)),
									AllowPrivilegeEscalation: pointer.Pointer(false),
									SeccompProfile: &corev1.SeccompProfile{
										Type: corev1.SeccompProfileTypeRuntimeDefault,
									},
									Capabilities: &corev1.Capabilities{
										Drop: []corev1.Capability{
											"ALL",
										},
									},
								},
							},
						},
						RestartPolicy: corev1.RestartPolicyAlways,
						Volumes: []corev1.Volume{
							{
								Name: "fluentd-config",
								VolumeSource: corev1.VolumeSource{
									ConfigMap: &corev1.ConfigMapVolumeSource{
										LocalObjectReference: corev1.LocalObjectReference{
											Name: "audittailer-config",
										},
									},
								},
							},
							{
								Name: "fluentd-certs",
								VolumeSource: corev1.VolumeSource{
									Secret: &corev1.SecretVolumeSource{
										SecretName: "", // FIXME: server secret self-signed
									},
								},
							},
							{
								Name: "fluentbuffer",
								VolumeSource: corev1.VolumeSource{
									EmptyDir: &corev1.EmptyDirVolumeSource{},
								},
							},
						},
					},
				},
			},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "audittailer",
				Namespace: "audit",
				Labels: map[string]string{
					"app": "audittailer",
				},
			},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{
					"app": "audittailer",
				},
				Ports: []corev1.ServicePort{
					{
						Port:       24224,
						TargetPort: intstr.FromInt(24224),
					},
				},
			},
		},
		&rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "audittailer",
				Namespace: "audit",
			},
			Rules: []rbacv1.PolicyRule{
				{
					APIGroups: []string{""},
					Resources: []string{
						"services",
						"secrets",
					},
					Verbs: []string{
						"get",
						"list",
					},
				},
			},
		},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "audittailer",
				Namespace: "audit",
			},
			Subjects: []rbacv1.Subject{
				{
					Kind:      "ServiceAccount",
					Name:      "audittailer-client",
					Namespace: "kube-system",
				},
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "Role",
				Name:     "audittailer",
			},
		},
	}, nil
}
