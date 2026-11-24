package kube

import "k8s.io/apimachinery/pkg/runtime/schema"

// cert-manager resources.
var (
	GVRCertificate        = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}
	GVRCertificateRequest = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificaterequests"}
	GVRIssuer             = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "issuers"}
	GVRClusterIssuer      = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "clusterissuers"}
	GVROrder              = schema.GroupVersionResource{Group: "acme.cert-manager.io", Version: "v1", Resource: "orders"}
	GVRChallenge          = schema.GroupVersionResource{Group: "acme.cert-manager.io", Version: "v1", Resource: "challenges"}
)

// OLM resources, used on OpenShift.
var (
	GVRSubscription    = schema.GroupVersionResource{Group: "operators.coreos.com", Version: "v1alpha1", Resource: "subscriptions"}
	GVRInstallPlan     = schema.GroupVersionResource{Group: "operators.coreos.com", Version: "v1alpha1", Resource: "installplans"}
	GVRCSV             = schema.GroupVersionResource{Group: "operators.coreos.com", Version: "v1alpha1", Resource: "clusterserviceversions"}
	GVRCatalogSource   = schema.GroupVersionResource{Group: "operators.coreos.com", Version: "v1alpha1", Resource: "catalogsources"}
	GVROperatorGroup   = schema.GroupVersionResource{Group: "operators.coreos.com", Version: "v1", Resource: "operatorgroups"}
	GVRPackageManifest = schema.GroupVersionResource{Group: "packages.operators.coreos.com", Version: "v1", Resource: "packagemanifests"}
)

// Built-in resources cmo deletes through the dynamic client.
var (
	GVRCRD                = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	GVRNamespace          = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	GVRValidatingWH       = schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingwebhookconfigurations"}
	GVRMutatingWH         = schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "mutatingwebhookconfigurations"}
	GVRClusterRole        = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}
	GVRClusterRoleBinding = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}
	GVRSecret             = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	GVRService            = schema.GroupVersionResource{Version: "v1", Resource: "services"}
	GVRServiceAccount     = schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}
	GVRDeployment         = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	GVRRole               = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}
	GVRRoleBinding        = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}
)

// OpenShift resources.
var (
	GVRClusterVersion = schema.GroupVersionResource{Group: "config.openshift.io", Version: "v1", Resource: "clusterversions"}
	// GVRCertManagerOperator is the Red Hat operator's CertManager config (cluster-scoped, named cluster).
	GVRCertManagerOperator = schema.GroupVersionResource{Group: "operator.openshift.io", Version: "v1alpha1", Resource: "certmanagers"}
)

// ListKinds maps every resource above to its list kind, for fake dynamic
// clients in tests.
func ListKinds() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{
		GVRCertificate:         "CertificateList",
		GVRCertificateRequest:  "CertificateRequestList",
		GVRIssuer:              "IssuerList",
		GVRClusterIssuer:       "ClusterIssuerList",
		GVROrder:               "OrderList",
		GVRChallenge:           "ChallengeList",
		GVRSubscription:        "SubscriptionList",
		GVRInstallPlan:         "InstallPlanList",
		GVRCSV:                 "ClusterServiceVersionList",
		GVRCatalogSource:       "CatalogSourceList",
		GVROperatorGroup:       "OperatorGroupList",
		GVRPackageManifest:     "PackageManifestList",
		GVRCRD:                 "CustomResourceDefinitionList",
		GVRNamespace:           "NamespaceList",
		GVRValidatingWH:        "ValidatingWebhookConfigurationList",
		GVRMutatingWH:          "MutatingWebhookConfigurationList",
		GVRClusterRole:         "ClusterRoleList",
		GVRClusterRoleBinding:  "ClusterRoleBindingList",
		GVRSecret:              "SecretList",
		GVRService:             "ServiceList",
		GVRServiceAccount:      "ServiceAccountList",
		GVRDeployment:          "DeploymentList",
		GVRRole:                "RoleList",
		GVRRoleBinding:         "RoleBindingList",
		GVRClusterVersion:      "ClusterVersionList",
		GVRCertManagerOperator: "CertManagerList",
	}
}

// ManagedByLabel marks every object cmo creates, so it can find and clean them up.
const (
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedByValue = "cmo-cli"
	// ManagedSelector selects the objects cmo created.
	ManagedSelector = ManagedByLabel + "=" + ManagedByValue
)
