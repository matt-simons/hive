package remotemachineset

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/golang/mock/gomock"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/pointer"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"

	machineapi "github.com/openshift/cluster-api/pkg/apis/machine/v1beta1"
	"github.com/openshift/hive/pkg/apis"
	hivev1 "github.com/openshift/hive/pkg/apis/hive/v1"
	hivev1aws "github.com/openshift/hive/pkg/apis/hive/v1/aws"
	"github.com/openshift/hive/pkg/awsclient"
	mockaws "github.com/openshift/hive/pkg/awsclient/mock"
	"github.com/openshift/hive/pkg/constants"
	controllerutils "github.com/openshift/hive/pkg/controller/utils"
	awsprovider "sigs.k8s.io/cluster-api-provider-aws/pkg/apis/awsproviderconfig/v1beta1"
)

const (
	testName                 = "foo"
	testNamespace            = "default"
	testClusterID            = "foo-12345-uuid"
	testInfraID              = "foo-12345"
	machineAPINamespace      = "openshift-machine-api"
	adminKubeconfigSecret    = "foo-admin-kubeconfig"
	adminKubeconfigSecretKey = "kubeconfig"
	adminPasswordSecret      = "foo-admin-creds"
	adminPasswordSecretKey   = "password"
	sshKeySecret             = "foo-ssh-key"
	sshKeySecretKey          = "ssh-publickey"
	testAMI                  = "ami-totallyfake"
)

func init() {
	log.SetLevel(log.DebugLevel)
}

func TestRemoteMachineSetReconcile(t *testing.T) {
	apis.AddToScheme(scheme.Scheme)
	machineapi.SchemeBuilder.AddToScheme(scheme.Scheme)
	awsprovider.SchemeBuilder.AddToScheme(scheme.Scheme)

	getPool := func(c client.Client, poolName string) *hivev1.MachinePool {
		pool := &hivev1.MachinePool{}
		if err := c.Get(context.TODO(), client.ObjectKey{Namespace: testNamespace, Name: fmt.Sprintf("%s-%s", testName, poolName)}, pool); err != nil {
			return nil
		}
		return pool
	}

	// Utility function to list test MachineSets from the fake client
	getRMSL := func(rc client.Client) (*machineapi.MachineSetList, error) {
		rMSL := &machineapi.MachineSetList{}
		tm := metav1.TypeMeta{}
		tm.SetGroupVersionKind(machineapi.SchemeGroupVersion.WithKind("MachineSet"))
		err := rc.List(context.TODO(), rMSL, client.UseListOptions(&client.ListOptions{
			Raw: &metav1.ListOptions{TypeMeta: tm},
		}))
		if err == nil {
			return rMSL, err
		}
		return nil, err
	}

	tests := []struct {
		name                      string
		localExisting             []runtime.Object
		remoteExisting            []runtime.Object
		expectErr                 bool
		expectNoFinalizer         bool
		expectedRemoteMachineSets *machineapi.MachineSetList
	}{
		{
			name: "Kubeconfig doesn't exist yet",
			localExisting: []runtime.Object{
				testClusterDeployment(),
				testMachinePool("worker", 3, []string{}),
			},
			expectErr: true,
		},
		{
			name: "No-op",
			localExisting: []runtime.Object{
				testClusterDeployment(),
				testMachinePool("worker", 3, []string{}),
				testSecret(adminKubeconfigSecret, adminKubeconfigSecretKey, testName),
				testSecret(adminPasswordSecret, adminPasswordSecretKey, testName),
				testSecret(sshKeySecret, sshKeySecretKey, testName),
			},
			remoteExisting: []runtime.Object{
				testMachineSet("foo-12345-worker-us-east-1a", "worker", true, 1, 0),
				testMachineSet("foo-12345-worker-us-east-1b", "worker", true, 1, 0),
				testMachineSet("foo-12345-worker-us-east-1c", "worker", true, 1, 0),
			},
			expectedRemoteMachineSets: func() *machineapi.MachineSetList {
				return &machineapi.MachineSetList{
					Items: []machineapi.MachineSet{
						*testMachineSet("foo-12345-worker-us-east-1a", "worker", true, 1, 0),
						*testMachineSet("foo-12345-worker-us-east-1b", "worker", true, 1, 0),
						*testMachineSet("foo-12345-worker-us-east-1c", "worker", true, 1, 0),
					},
				}
			}(),
		},
		{
			name: "Update machine set replicas",
			localExisting: []runtime.Object{
				testClusterDeployment(),
				testMachinePool("worker", 3, []string{}),
				testSecret(adminKubeconfigSecret, adminKubeconfigSecretKey, testName),
				testSecret(adminPasswordSecret, adminPasswordSecretKey, testName),
				testSecret(sshKeySecret, sshKeySecretKey, testName),
			},
			remoteExisting: []runtime.Object{
				testMachineSet("foo-12345-worker-us-east-1a", "worker", true, 1, 0),
				testMachineSet("foo-12345-worker-us-east-1b", "worker", true, 1, 0),
				testMachineSet("foo-12345-worker-us-east-1c", "worker", true, 0, 0),
			},
			expectedRemoteMachineSets: func() *machineapi.MachineSetList {
				return &machineapi.MachineSetList{
					Items: []machineapi.MachineSet{
						*testMachineSet("foo-12345-worker-us-east-1a", "worker", true, 1, 0),
						*testMachineSet("foo-12345-worker-us-east-1b", "worker", true, 1, 0),
						*testMachineSet("foo-12345-worker-us-east-1c", "worker", true, 1, 1),
					},
				}
			}(),
		},
		{
			name: "Create missing machine set",
			localExisting: []runtime.Object{
				testClusterDeployment(),
				testMachinePool("worker", 3, []string{"us-east-1a", "us-east-1b", "us-east-1c"}),
				testSecret(adminKubeconfigSecret, adminKubeconfigSecretKey, testName),
				testSecret(adminPasswordSecret, adminPasswordSecretKey, testName),
				testSecret(sshKeySecret, sshKeySecretKey, testName),
			},
			remoteExisting: []runtime.Object{
				testMachineSet("foo-12345-worker-us-east-1a", "worker", true, 1, 0),
				testMachineSet("foo-12345-worker-us-east-1b", "worker", true, 1, 0),
			},
			expectedRemoteMachineSets: func() *machineapi.MachineSetList {
				return &machineapi.MachineSetList{
					Items: []machineapi.MachineSet{
						*testMachineSet("foo-12345-worker-us-east-1a", "worker", true, 1, 0),
						*testMachineSet("foo-12345-worker-us-east-1b", "worker", true, 1, 0),
						*testMachineSet("foo-12345-worker-us-east-1c", "worker", false, 1, 0),
					},
				}
			}(),
		},
		{
			name: "Skip create missing machine set when clusterDeployment has annotation hive.openshift.io/syncset-pause: true ",
			localExisting: []runtime.Object{
				func() *hivev1.ClusterDeployment {
					cd := testClusterDeployment()
					cd.Annotations = map[string]string{}
					cd.Annotations[constants.SyncsetPauseAnnotation] = "true"
					return cd
				}(),
				testMachinePool("worker", 3, []string{"us-east-1a", "us-east-1b", "us-east-1c"}),
				testSecret(adminKubeconfigSecret, adminKubeconfigSecretKey, testName),
				testSecret(adminPasswordSecret, adminPasswordSecretKey, testName),
				testSecret(sshKeySecret, sshKeySecretKey, testName),
			},
			remoteExisting: []runtime.Object{},
			expectedRemoteMachineSets: func() *machineapi.MachineSetList {
				return &machineapi.MachineSetList{}
			}(),
		},
		{
			name: "Skip create missing machine set when cluster has unreachable condition",
			localExisting: []runtime.Object{
				func() *hivev1.ClusterDeployment {
					cd := testClusterDeployment()
					cd.Status.Conditions = controllerutils.SetClusterDeploymentCondition(cd.Status.Conditions,
						hivev1.UnreachableCondition, corev1.ConditionTrue, "", "", controllerutils.UpdateConditionAlways)
					return cd
				}(),
				testMachinePool("worker", 3, []string{"us-east-1a", "us-east-1b", "us-east-1c"}),
				testSecret(adminKubeconfigSecret, adminKubeconfigSecretKey, testName),
				testSecret(adminPasswordSecret, adminPasswordSecretKey, testName),
				testSecret(sshKeySecret, sshKeySecretKey, testName),
			},
			remoteExisting: []runtime.Object{},
			expectedRemoteMachineSets: func() *machineapi.MachineSetList {
				return &machineapi.MachineSetList{}
			}(),
		},
		{
			name: "Delete extra machine set",
			localExisting: []runtime.Object{
				testClusterDeployment(),
				testMachinePool("worker", 3, []string{}),
				testSecret(adminKubeconfigSecret, adminKubeconfigSecretKey, testName),
				testSecret(adminPasswordSecret, adminPasswordSecretKey, testName),
				testSecret(sshKeySecret, sshKeySecretKey, testName),
			},
			remoteExisting: []runtime.Object{
				testMachineSet("foo-12345-worker-us-east-1a", "worker", true, 1, 0),
				testMachineSet("foo-12345-worker-us-east-1b", "worker", true, 1, 0),
				testMachineSet("foo-12345-worker-us-east-1c", "worker", true, 1, 0),
				testMachineSet("foo-12345-worker-us-east-1d", "worker", true, 1, 0),
			},
			expectedRemoteMachineSets: func() *machineapi.MachineSetList {
				return &machineapi.MachineSetList{
					Items: []machineapi.MachineSet{
						*testMachineSet("foo-12345-worker-us-east-1a", "worker", true, 1, 0),
						*testMachineSet("foo-12345-worker-us-east-1b", "worker", true, 1, 0),
						*testMachineSet("foo-12345-worker-us-east-1c", "worker", true, 1, 0),
					},
				}
			}(),
		},
		{
			name: "Other machinesets ignored",
			localExisting: []runtime.Object{
				testClusterDeployment(),
				testMachinePool("worker", 3, []string{"us-east-1a"}),
				testMachinePool("other", 3, []string{"us-east-1b"}),
				testSecret(adminKubeconfigSecret, adminKubeconfigSecretKey, testName),
				testSecret(adminPasswordSecret, adminPasswordSecretKey, testName),
				testSecret(sshKeySecret, sshKeySecretKey, testName),
			},
			remoteExisting: []runtime.Object{
				testMachineSet("foo-12345-worker-us-east-1a", "worker", true, 3, 0),
				testMachineSet("foo-12345-other-us-east-1b", "other", true, 3, 0),
			},
			expectedRemoteMachineSets: func() *machineapi.MachineSetList {
				return &machineapi.MachineSetList{
					Items: []machineapi.MachineSet{
						*testMachineSet("foo-12345-worker-us-east-1a", "worker", true, 3, 0),
						*testMachineSet("foo-12345-other-us-east-1b", "other", true, 3, 0),
					},
				}
			}(),
		},
		{
			name: "Create additional machinepool machinesets",
			localExisting: []runtime.Object{
				testClusterDeployment(),
				testMachinePool("worker", 3, []string{}),
				testMachinePool("other", 3, []string{}),
				testSecret(adminKubeconfigSecret, adminKubeconfigSecretKey, testName),
				testSecret(adminPasswordSecret, adminPasswordSecretKey, testName),
				testSecret(sshKeySecret, sshKeySecretKey, testName),
			},
			remoteExisting: []runtime.Object{
				testMachineSetWithAMI("foo-12345-other-us-east-1a", "other", "ami2", true, 1, 0),
				testMachineSetWithAMI("foo-12345-other-us-east-1b", "other", "ami2", true, 1, 0),
				testMachineSetWithAMI("foo-12345-other-us-east-1c", "other", "ami2", true, 1, 0),
			},
			expectedRemoteMachineSets: func() *machineapi.MachineSetList {
				return &machineapi.MachineSetList{
					Items: []machineapi.MachineSet{
						*testMachineSetWithAMI("foo-12345-worker-us-east-1a", "worker", "ami2", false, 1, 0),
						*testMachineSetWithAMI("foo-12345-worker-us-east-1b", "worker", "ami2", false, 1, 0),
						*testMachineSetWithAMI("foo-12345-worker-us-east-1c", "worker", "ami2", false, 1, 0),
						*testMachineSetWithAMI("foo-12345-other-us-east-1a", "other", "ami2", true, 1, 0),
						*testMachineSetWithAMI("foo-12345-other-us-east-1b", "other", "ami2", true, 1, 0),
						*testMachineSetWithAMI("foo-12345-other-us-east-1c", "other", "ami2", true, 1, 0),
					},
				}
			}(),
		},
		{
			name: "Delete machinepool machinesets",
			localExisting: []runtime.Object{
				testClusterDeployment(),
				func() runtime.Object {
					mp := testMachinePool("worker", 3, []string{})
					now := metav1.Now()
					mp.DeletionTimestamp = &now
					return mp
				}(),
				testMachinePool("other", 3, []string{}),
				testSecret(adminKubeconfigSecret, adminKubeconfigSecretKey, testName),
				testSecret(adminPasswordSecret, adminPasswordSecretKey, testName),
				testSecret(sshKeySecret, sshKeySecretKey, testName),
			},
			remoteExisting: []runtime.Object{
				testMachineSet("foo-12345-other-us-east-1a", "other", true, 1, 0),
				testMachineSet("foo-12345-other-us-east-1b", "other", true, 1, 0),
				testMachineSet("foo-12345-other-us-east-1c", "other", true, 1, 0),
				testMachineSet("foo-12345-worker-us-east-1a", "worker", true, 1, 0),
				testMachineSet("foo-12345-worker-us-east-1b", "worker", true, 1, 0),
				testMachineSet("foo-12345-worker-us-east-1c", "worker", true, 1, 0),
			},
			expectNoFinalizer: true,
			expectedRemoteMachineSets: func() *machineapi.MachineSetList {
				return &machineapi.MachineSetList{
					Items: []machineapi.MachineSet{
						*testMachineSet("foo-12345-other-us-east-1a", "other", true, 1, 0),
						*testMachineSet("foo-12345-other-us-east-1b", "other", true, 1, 0),
						*testMachineSet("foo-12345-other-us-east-1c", "other", true, 1, 0),
					},
				}
			}(),
		},
		{
			// Can't control what a user might do in their cluster.
			name: "Nil ProviderSpec",
			localExisting: []runtime.Object{
				testClusterDeployment(),
				testMachinePool("worker", 3, []string{}),
				testSecret(adminKubeconfigSecret, adminKubeconfigSecretKey, testName),
				testSecret(adminPasswordSecret, adminPasswordSecretKey, testName),
				testSecret(sshKeySecret, sshKeySecretKey, testName),
			},
			remoteExisting: []runtime.Object{
				func() *machineapi.MachineSet {
					ms := testMachineSet("foo-12345-worker-us-east-1c", "worker", true, 1, 0)
					ms.Spec.Template.Spec.ProviderSpec.Value = nil
					return ms
				}(),
			},
			expectErr: true,
		},
		{
			name: "Nil worker pool platform",
			localExisting: []runtime.Object{
				testClusterDeployment(),
				func() runtime.Object {
					mp := testMachinePool("worker", 3, []string{})
					mp.Spec.Platform.AWS = nil
					return mp
				}(),
				testSecret(adminKubeconfigSecret, adminKubeconfigSecretKey, testName),
				testSecret(adminPasswordSecret, adminPasswordSecretKey, testName),
				testSecret(sshKeySecret, sshKeySecretKey, testName),
			},
			remoteExisting: []runtime.Object{
				testMachineSet("foo-12345-worker-us-east-1a", "worker", true, 1, 0),
				testMachineSet("foo-12345-worker-us-east-1b", "worker", true, 1, 0),
				testMachineSet("foo-12345-worker-us-east-1c", "worker", true, 1, 0),
			},
		},
		{
			name: "No cluster deployment",
			localExisting: []runtime.Object{
				testMachinePool("worker", 3, []string{}),
				testSecret(adminKubeconfigSecret, adminKubeconfigSecretKey, testName),
				testSecret(adminPasswordSecret, adminPasswordSecretKey, testName),
				testSecret(sshKeySecret, sshKeySecretKey, testName),
			},
			remoteExisting: []runtime.Object{
				testMachineSet("foo-12345-worker-us-east-1a", "worker", true, 1, 0),
				testMachineSet("foo-12345-worker-us-east-1b", "worker", true, 1, 0),
				testMachineSet("foo-12345-worker-us-east-1c", "worker", true, 1, 0),
			},
			expectNoFinalizer: true,
			expectedRemoteMachineSets: func() *machineapi.MachineSetList {
				return &machineapi.MachineSetList{
					Items: []machineapi.MachineSet{
						*testMachineSet("foo-12345-worker-us-east-1a", "worker", true, 1, 0),
						*testMachineSet("foo-12345-worker-us-east-1b", "worker", true, 1, 0),
						*testMachineSet("foo-12345-worker-us-east-1c", "worker", true, 1, 0),
					},
				}
			}(),
		},
		{
			name: "Deleted cluster deployment",
			localExisting: []runtime.Object{
				func() runtime.Object {
					cd := testClusterDeployment()
					now := metav1.Now()
					cd.DeletionTimestamp = &now
					return cd
				}(),
				testMachinePool("worker", 3, []string{}),
				testSecret(adminKubeconfigSecret, adminKubeconfigSecretKey, testName),
				testSecret(adminPasswordSecret, adminPasswordSecretKey, testName),
				testSecret(sshKeySecret, sshKeySecretKey, testName),
			},
			remoteExisting: []runtime.Object{
				testMachineSet("foo-12345-worker-us-east-1a", "worker", true, 1, 0),
				testMachineSet("foo-12345-worker-us-east-1b", "worker", true, 1, 0),
				testMachineSet("foo-12345-worker-us-east-1c", "worker", true, 1, 0),
			},
			expectNoFinalizer: true,
			expectedRemoteMachineSets: func() *machineapi.MachineSetList {
				return &machineapi.MachineSetList{
					Items: []machineapi.MachineSet{
						*testMachineSet("foo-12345-worker-us-east-1a", "worker", true, 1, 0),
						*testMachineSet("foo-12345-worker-us-east-1b", "worker", true, 1, 0),
						*testMachineSet("foo-12345-worker-us-east-1c", "worker", true, 1, 0),
					},
				}
			}(),
		},
	}

	for _, test := range tests {
		apis.AddToScheme(scheme.Scheme)
		machineapi.SchemeBuilder.AddToScheme(scheme.Scheme)
		t.Run(test.name, func(t *testing.T) {
			fakeClient := fake.NewFakeClient(test.localExisting...)
			remoteFakeClient := fake.NewFakeClient(test.remoteExisting...)

			mockCtrl := gomock.NewController(t)
			mockAWSClient := mockaws.NewMockClient(mockCtrl)
			// Test availability zone retrieval when zones have not been set for machine pool
			mockTestAvailabilityZones(mockAWSClient, "us-east-1", []string{"us-east-1a", "us-east-1b", "us-east-1c"})

			rcd := &ReconcileRemoteMachineSet{
				Client: fakeClient,
				scheme: scheme.Scheme,
				logger: log.WithField("controller", "remotemachineset"),
				remoteClusterAPIClientBuilder: func(string, string) (client.Client, error) {
					return remoteFakeClient, nil
				},
				awsClientBuilder: func(client.Client, string, string, string) (awsclient.Client, error) {
					return mockAWSClient, nil
				},
			}
			_, err := rcd.Reconcile(reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      fmt.Sprintf("%s-worker", testName),
					Namespace: testNamespace,
				},
			})

			if test.expectErr {
				if err == nil {
					t.Errorf("expected error but got none")
				}
				// Should not proceed with test validations if we expected an error from the reconcile.
				return
			}
			if err != nil && !test.expectErr {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if pool := getPool(fakeClient, "worker"); assert.NotNil(t, pool, "missing machinepool") {
				if test.expectNoFinalizer {
					assert.NotContains(t, pool.Finalizers, finalizer, "unexpected finalizer")
				} else {
					assert.Contains(t, pool.Finalizers, finalizer, "missing finalizer")
				}
			}

			if test.expectedRemoteMachineSets != nil {
				rMSL, err := getRMSL(remoteFakeClient)
				if assert.NoError(t, err) {
					for _, eMS := range test.expectedRemoteMachineSets.Items {
						found := false
						for _, rMS := range rMSL.Items {
							if eMS.Name == rMS.Name {
								found = true
								assert.Equal(t, *eMS.Spec.Replicas, *rMS.Spec.Replicas)
								assert.Equal(t, eMS.Generation, rMS.Generation)
								if !reflect.DeepEqual(eMS.ObjectMeta.Labels, rMS.ObjectMeta.Labels) {
									t.Errorf("machineset %v has unexpected labels:\nexpected: %v\nactual: %v", eMS.Name, eMS.Labels, rMS.Labels)
								}
								if !reflect.DeepEqual(eMS.ObjectMeta.Annotations, rMS.ObjectMeta.Annotations) {
									t.Errorf("machineset %v has unexpected annotations:\nexpected: %v\nactual: %v", eMS.Name, eMS.Labels, rMS.Labels)
								}
								if !reflect.DeepEqual(eMS.Spec.Template.Spec.Labels, rMS.Spec.Template.Spec.Labels) {
									t.Errorf("machineset %v machinespec has unexpected labels:\nexpected: %v\nactual: %v", eMS.Name, eMS.Spec.Template.Spec.Labels, rMS.Spec.Template.Spec.Labels)
								}
								if !reflect.DeepEqual(eMS.Spec.Template.Spec.Taints, rMS.Spec.Template.Spec.Taints) {
									t.Errorf("machineset %v has unexpected taints:\nexpected: %v\nactual: %v", eMS.Name, eMS.Spec.Template.Spec.Taints, rMS.Spec.Template.Spec.Taints)
								}

								rAWSProviderSpec, _ := decodeAWSMachineProviderSpec(
									rMS.Spec.Template.Spec.ProviderSpec.Value, scheme.Scheme)
								log.Debugf("remote AWS: %v", printAWSMachineProviderConfig(rAWSProviderSpec))
								assert.NotNil(t, rAWSProviderSpec)

								eAWSProviderSpec, _ := decodeAWSMachineProviderSpec(
									eMS.Spec.Template.Spec.ProviderSpec.Value, scheme.Scheme)
								log.Debugf("expected AWS: %v", printAWSMachineProviderConfig(eAWSProviderSpec))
								assert.NotNil(t, eAWSProviderSpec)
								assert.Equal(t, eAWSProviderSpec.AMI, rAWSProviderSpec.AMI, "%s AMI does not match", eMS.Name)

							}
						}
						if !found {
							t.Errorf("did not find expected remote machineset: %v", eMS.Name)
						}
					}
					for _, rMS := range rMSL.Items {
						found := false
						for _, eMS := range test.expectedRemoteMachineSets.Items {
							if rMS.Name == eMS.Name {
								found = true
							}
						}
						if !found {
							t.Errorf("found unexpected remote machineset: %v", rMS.Name)
						}
					}
				}
			}
		})
	}
}

func testMachinePool(name string, replicas int, zones []string) *hivev1.MachinePool {
	return &hivev1.MachinePool{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  testNamespace,
			Name:       fmt.Sprintf("%s-%s", testName, name),
			Finalizers: []string{finalizer},
		},
		Spec: hivev1.MachinePoolSpec{
			ClusterDeploymentRef: corev1.LocalObjectReference{
				Name: testName,
			},
			Name:     name,
			Replicas: pointer.Int64Ptr(int64(replicas)),
			Platform: hivev1.MachinePoolPlatform{
				AWS: &hivev1aws.MachinePoolPlatform{
					InstanceType: "m4.large",
					Zones:        zones,
				},
			},
			Labels: map[string]string{
				"machine.openshift.io/cluster-api-cluster":      testInfraID,
				"machine.openshift.io/cluster-api-machine-role": name,
				"machine.openshift.io/cluster-api-machine-type": name,
			},
			Taints: []corev1.Taint{
				{
					Key:    "foo",
					Value:  "bar",
					Effect: corev1.TaintEffectNoSchedule,
				},
			},
		},
	}
}

func testMachineSet(name string, machineType string, unstompedAnnotation bool, replicas int, generation int) *machineapi.MachineSet {
	return testMachineSetWithAMI(name, machineType, testAMI, unstompedAnnotation, replicas, generation)
}

func testMachineSetWithAMI(name, machineType, ami string, unstompedAnnotation bool, replicas int, generation int) *machineapi.MachineSet {
	msReplicas := int32(replicas)
	awsProviderSpec := &awsprovider.AWSMachineProviderConfig{
		TypeMeta: metav1.TypeMeta{
			Kind:       "AWSMachineProviderConfig",
			APIVersion: awsprovider.SchemeGroupVersion.String(),
		},
		AMI: awsprovider.AWSResourceReference{
			ID: aws.String(ami),
		},
	}
	rawAWSProviderSpec, err := encodeAWSMachineProviderSpec(awsProviderSpec, scheme.Scheme)
	if err != nil {
		log.WithError(err).Fatal("error encoding AWS machine provider spec")
	}

	ms := machineapi.MachineSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: machineAPINamespace,
			Labels: map[string]string{
				machinePoolNameLabel:                       machineType,
				"machine.openshift.io/cluster-api-cluster": testInfraID,
			},
			Generation: int64(generation),
		},
		Spec: machineapi.MachineSetSpec{
			Replicas: &msReplicas,
			Template: machineapi.MachineTemplateSpec{
				Spec: machineapi.MachineSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"machine.openshift.io/cluster-api-cluster":      testInfraID,
							"machine.openshift.io/cluster-api-machine-role": machineType,
							"machine.openshift.io/cluster-api-machine-type": machineType,
						},
					},
					ProviderSpec: machineapi.ProviderSpec{
						Value: rawAWSProviderSpec,
					},

					Taints: []corev1.Taint{
						{
							Key:    "foo",
							Value:  "bar",
							Effect: corev1.TaintEffectNoSchedule,
						},
					},
				},
			},
		},
	}
	// Add a pre-existing annotation which we will ensure remains in updated machinesets.
	if unstompedAnnotation {
		ms.Annotations = map[string]string{
			"hive.openshift.io/unstomped": "true",
		}
	}
	return &ms
}

func testClusterDeployment() *hivev1.ClusterDeployment {
	return &hivev1.ClusterDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testName,
			Namespace:  testNamespace,
			Finalizers: []string{hivev1.FinalizerDeprovision},
			UID:        types.UID("1234"),
		},
		Spec: hivev1.ClusterDeploymentSpec{
			ClusterName: testName,
			Platform: hivev1.Platform{
				AWS: &hivev1aws.Platform{
					CredentialsSecretRef: corev1.LocalObjectReference{
						Name: "aws-credentials",
					},
					Region: "us-east-1",
				},
			},
			ClusterMetadata: &hivev1.ClusterMetadata{
				AdminKubeconfigSecretRef: corev1.LocalObjectReference{Name: fmt.Sprintf("%s-admin-kubeconfig", testName)},
				ClusterID:                testClusterID,
				InfraID:                  testInfraID,
			},
			Installed: true,
		},
	}
}

func mockTestAvailabilityZones(mockAWSClient *mockaws.MockClient, region string, zones []string) {
	availabilityZones := []*ec2.AvailabilityZone{}

	for _, zone := range zones {
		availabilityZones = append(availabilityZones, &ec2.AvailabilityZone{
			RegionName: aws.String(region),
			ZoneName:   aws.String(zone),
		})
	}

	mockAWSClient.EXPECT().DescribeAvailabilityZones(gomock.Any()).Return(
		&ec2.DescribeAvailabilityZonesOutput{
			AvailabilityZones: availabilityZones,
		}, nil).AnyTimes()
}

func testSecret(name, key, value string) *corev1.Secret {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Data: map[string][]byte{
			key: []byte(value),
		},
	}
	return s
}

func printAWSMachineProviderConfig(cfg *awsprovider.AWSMachineProviderConfig) string {
	b, err := json.Marshal(cfg)
	if err != nil {
		panic(err.Error())
	}
	return string(b)
}
