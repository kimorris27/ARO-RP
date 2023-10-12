package e2e

// Copyright (c) Microsoft Corporation.
// Licensed under the Apache License 2.0.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	mgmtnetwork "github.com/Azure/azure-sdk-for-go/services/network/mgmt/2020-08-01/network"

	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/Azure/go-autorest/autorest/to"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/Azure/ARO-RP/pkg/api/admin"
	apisubnet "github.com/Azure/ARO-RP/pkg/api/util/subnet"
	"github.com/Azure/ARO-RP/pkg/util/ready"
	"github.com/Azure/ARO-RP/pkg/util/stringutils"
	"github.com/Azure/ARO-RP/pkg/util/version"
	"github.com/Azure/ARO-RP/test/util/project"
)

const (
	testNamespace = "test-e2e"
)

var _ = Describe("Cluster", func() {
	var p project.Project

	var _ = BeforeEach(func(ctx context.Context) {
		By("creating a test namespace")
		p = project.NewProject(clients.Kubernetes, clients.Project, testNamespace)
		err := p.Create(ctx)
		Expect(err).NotTo(HaveOccurred(), "Failed to create test namespace")

		By("verifying the namespace is ready")
		Eventually(func(ctx context.Context) error {
			return p.Verify(ctx)
		}).WithContext(ctx).Should(BeNil())
	})

	var _ = AfterEach(func(ctx context.Context) {
		By("deleting a test namespace")
		err := p.Delete(ctx)
		Expect(err).NotTo(HaveOccurred(), "Failed to delete test namespace")

		By("verifying the namespace is deleted")
		Eventually(func(ctx context.Context) error {
			return p.VerifyProjectIsDeleted(ctx)
		}).WithContext(ctx).Should(BeNil())
	})

	It("can run a stateful set which is using Azure Disk storage", func(ctx context.Context) {
		By("creating stateful set")
		oc, _ := clients.OpenshiftClusters.Get(ctx, vnetResourceGroup, clusterName)
		installVersion, _ := version.ParseVersion(*oc.ClusterProfile.Version)

		storageClass := "managed-csi"

		if installVersion.Lt(version.NewVersion(4, 11)) {
			storageClass = "managed-premium"
		}

		err := createStatefulSet(ctx, clients.Kubernetes, storageClass)
		Expect(err).NotTo(HaveOccurred())

		By("verifying the stateful set is ready")
		Eventually(func(g Gomega, ctx context.Context) {
			s, err := clients.Kubernetes.AppsV1().StatefulSets(testNamespace).Get(ctx, fmt.Sprintf("busybox-%s", storageClass), metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())

			g.Expect(ready.StatefulSetIsReady(s)).To(BeTrue(), "expect stateful to be ready")
		}).WithContext(ctx).Should(Succeed())
	})

	It("can run a stateful set which is using the default Azure File storage class backed by the cluster storage account", func(ctx context.Context) {
		By("adding the Microsoft.Storage service endpoint to each cluster subnet")
		subnets := clusterSubnets()

		for _, s := range subnets {
			vnetID, subnetName, err := apisubnet.Split(s)
			Expect(err).NotTo(HaveOccurred())

			vnetName, err := azure.ParseResourceID(vnetID)
			Expect(err).NotTo(HaveOccurred())

			mgmtSubnet, err := clients.Subnet.Get(ctx, vnetResourceGroup, vnetName, subnetName, "")
			Expect(err).NotTo(HaveOccurred())

			if mgmtSubnet.SubnetPropertiesFormat == nil {
				mgmtSubnet.SubnetPropertiesFormat = &mgmtnetwork.SubnetPropertiesFormat{}
			}

			if mgmtSubnet.SubnetPropertiesFormat.ServiceEndpoints == nil {
				mgmtSubnet.SubnetPropertiesFormat.ServiceEndpoints = &[]mgmtnetwork.ServiceEndpointsPropertiesFormat{}
			}

			serviceEndpoint := mgmtnetwork.ServiceEndpointPropertiesFormat{
				Service:   to.StringPtr("Microsoft.Storage"),
				Locations: &[]string{"*"},
			}

			*mgmtSubnet.ServiceEndpoints = append(*mgmtSubnet.ServiceEndpoints, serviceEndpoint)

			err = clients.Subnet.CreateOrUpdateAndWait(ctx, vnetResourceGroup, vnetName, subnetName, mgmtSubnet)
			Expect(err).NotTo(HaveOccurred())
		}

		By("PUCM-ing the cluster")
		adminOc := &admin.OpenShiftCluster{}
		resp, err := adminRequest(ctx, http.MethodPatch, clusterResourceID, nil, true, json.RawMessage("{}"), adminOc)

		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		Eventually(func(g Gomega, ctx context.Context) {
			adminOc = adminGetCluster(g, ctx, clusterResourceID)
			g.Expect(adminOc.Properties.ProvisioningState).To(Equal(admin.ProvisioningStateSucceeded))
		}).WithContext(ctx).Should(Succeed())

		Expect(adminOc.Properties.LastAdminUpdateError).To(Equal(""))

		By("creating stateful set")
		storageClass := "azurefile-csi"
		err := createStatefulSet(ctx, clients.Kubernetes, storageClass)
		Expect(err).NotTo(HaveOccurred())

		By("verifying the stateful set is ready")
		Eventually(func(g Gomega, ctx context.Context) {
			s, err := clients.Kubernetes.AppsV1().StatefulSets(testNamespace).Get(ctx, fmt.Sprintf("busybox-%s", storageClass), metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())

			g.Expect(ready.StatefulSetIsReady(s)).To(BeTrue(), "expect stateful to be ready")
		}).WithContext(ctx).Should(Succeed())

		By("checking the storage account vnet rules to verify that they include the cluster subnets")
		storageAccounts := storageAccounts()
		clusterResourceGroup := stringutils.LastTokenByte(*adminOc.OpenShiftClusterProperties.ClusterProfile.ResourceGroupID, '/')

		for _, sa := range storageAccounts {
			account, err := clients.Storage.GetProperties(ctx, clusterResourceGroup, sa, &mgmtstorage.AccountExpand{})
			Expect(err).NotTo(HaveOccurred())
			Expect(account.AccountProperties).NotTo(BeNil())
			Expect(account.NetworkRuleSet).NotTo(BeNil())
			Expect(account.NetworkRuleSet.VirtualNetworkRules).NotTo(BeNil())

			vnetRuleSubnets := make([]string, len(account.NetworkRuleSet.VirtualNetworkRules))

			for i, vnr := range *account.NetworkRuleSet.VirtualNetworkRules {
				Expect(vnr.VirtualNetworkResourceId).NotTo(BeNil())

				vnetRuleSubnets[i] = strings.tolower(*vnr.VirtualNetworkResourceID)
			}

			for _, s := range subnets {
				Expect(vnetRuleSubnets).To(ContainElement(strings.tolower(s)))
			}
		}

		By("cleaning up the cluster subnets (removing service endpoints)")
		for _, s := range subnets {
			vnetID, subnetName, err := apisubnet.Split(s)
			Expect(err).NotTo(HaveOccurred())

			vnetName, err := azure.ParseResourceID(vnetID)
			Expect(err).NotTo(HaveOccurred())

			mgmtSubnet, err := clients.Subnet.Get(ctx, vnetResourceGroup, vnetName, subnetName, "")
			Expect(err).NotTo(HaveOccurred())

			if mgmtSubnet.SubnetPropertiesFormat == nil {
				mgmtSubnet.SubnetPropertiesFormat = &mgmtnetwork.SubnetPropertiesFormat{}
			}

			mgmtSubnet.SubnetPropertiesFormat.ServiceEndpoints = &[]mgmtnetwork.ServiceEndpointsPropertiesFormat{}

			err = clients.Subnet.CreateOrUpdateAndWait(ctx, vnetResourceGroup, vnetName, subnetName, mgmtSubnet)
			Expect(err).NotTo(HaveOccurred())
		}

	})

	It("can create load balancer services", func(ctx context.Context) {
		By("creating an external load balancer service")
		err := createLoadBalancerService(ctx, clients.Kubernetes, "elb", map[string]string{})
		Expect(err).NotTo(HaveOccurred())

		By("creating an internal load balancer service")
		err = createLoadBalancerService(ctx, clients.Kubernetes, "ilb", map[string]string{
			"service.beta.kubernetes.io/azure-load-balancer-internal": "true",
		})
		Expect(err).NotTo(HaveOccurred())

		By("verifying the external load balancer service is ready")
		Eventually(func(ctx context.Context) bool {
			svc, err := clients.Kubernetes.CoreV1().Services(testNamespace).Get(ctx, "elb", metav1.GetOptions{})
			if err != nil {
				return false
			}
			return ready.ServiceIsReady(svc)
		}).WithContext(ctx).Should(BeTrue())

		By("verifying the internal load balancer service is ready")
		Eventually(func(ctx context.Context) bool {
			svc, err := clients.Kubernetes.CoreV1().Services(testNamespace).Get(ctx, "ilb", metav1.GetOptions{})
			if err != nil {
				return false
			}
			return ready.ServiceIsReady(svc)
		}).WithContext(ctx).Should(BeTrue())
	})

	// mainly we want to test the gateway/egress functionality - this request for the image will travel from
	// node > gateway > storage account of the registry.
	It("can access and use the internal container registry", func(ctx context.Context) {
		deployName := "internal-registry-deploy"

		By("creating a test deployment from an internal container registry")
		err := createContainerFromInternalContainerRegistryImage(ctx, clients.Kubernetes, deployName)
		Expect(err).NotTo(HaveOccurred())

		By("verifying the deployment is ready")
		Eventually(func(g Gomega, ctx context.Context) {
			s, err := clients.Kubernetes.AppsV1().Deployments(testNamespace).Get(ctx, deployName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())

			g.Expect(ready.DeploymentIsReady(s)).To(BeTrue(), "expect stateful to be ready")
		}).WithContext(ctx).Should(Succeed())
	})
})

// clusterSubnets returns a slice containing all of the cluster subnets' resource IDs.
func clusterSubnets() []string {
	oc, err := clients.OpenshiftClusters.Get(ctx, vnetResourceGroup, clusterName)
	Expect(err).NotTo(HaveOccurred())

	subnetMap := map[string]struct{}{}
	subnetMap[*oc.OpenShiftClusterProperties.MasterProfile.SubnetID] = struct{}{}

	for _, p := range *oc.OpenShiftClusterProperties.WorkerProfiles {
		subnetMap[*p.SubnetID] = struct{}{}
	}

	subnets := []string{}

	for k, _ := range subnetMap {
		subnets := append(subnets, k)
	}

	return subnets
}

// storageAccounts returns a slice containing the names of the
// two ARO storage accounts.
func storageAccounts() []string {
	oc := adminGetCluster(Default, ctx, clusterResourceID)

	clusterStorageAccountName := "cluster" + oc.Properties.StorageSuffix

	return []string{
		clusterStorageAccountName,
		oc.Properties.ImageRegistryStorageAccountName,
	}
}

func createStatefulSet(ctx context.Context, cli kubernetes.Interface, storageClass string) error {
	pvcStorage, err := resource.ParseQuantity("2Gi")
	if err != nil {
		return err
	}

	_, err = cli.AppsV1().StatefulSets(testNamespace).Create(ctx, &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("busybox-%s", storageClass),
		},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "busybox"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "busybox"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "busybox",
							Image: "busybox",
							Command: []string{
								"/bin/sh",
								"-c",
								"while true; do sleep 1; done",
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "busybox",
									MountPath: "/data",
									ReadOnly:  false,
								},
							},
						},
					},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "busybox",
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{
							corev1.ReadWriteOnce,
						},
						StorageClassName: to.StringPtr(storageClass),
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: pvcStorage,
							},
						},
					},
				},
			},
		},
	}, metav1.CreateOptions{})
	return err
}

func createLoadBalancerService(ctx context.Context, cli kubernetes.Interface, name string, annotations map[string]string) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   testNamespace,
			Annotations: annotations,
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name: "port",
					Port: 8080,
				},
			},
			Type: corev1.ServiceTypeLoadBalancer,
		},
	}
	_, err := cli.CoreV1().Services(testNamespace).Create(ctx, svc, metav1.CreateOptions{})
	return err
}

func createContainerFromInternalContainerRegistryImage(ctx context.Context, cli kubernetes.Interface, name string) error {
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: to.Int32Ptr(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": name},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "cli",
							Image: "image-registry.openshift-image-registry.svc:5000/openshift/cli",
							Command: []string{
								"/bin/sh",
								"-c",
								"while true; do sleep 1; done",
							},
						},
					},
				},
			},
		},
	}
	_, err := cli.AppsV1().Deployments(testNamespace).Create(ctx, deploy, metav1.CreateOptions{})
	return err
}
