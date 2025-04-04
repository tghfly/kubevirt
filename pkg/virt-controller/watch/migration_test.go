/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2017 Red Hat, Inc.
 *
 */

package watch

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	virtcontroller "kubevirt.io/kubevirt/pkg/controller"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/pointer"

	migrationsv1 "kubevirt.io/api/migrations/v1alpha1"

	virtconfig "kubevirt.io/kubevirt/pkg/virt-config"
	"kubevirt.io/kubevirt/tests"

	kubevirtfake "kubevirt.io/client-go/generated/kubevirt/clientset/versioned/fake"

	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	k8sv1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	framework "k8s.io/client-go/tools/cache/testing"
	"k8s.io/client-go/tools/record"

	"kubevirt.io/client-go/api"

	v1 "kubevirt.io/api/core/v1"
	virtv1 "kubevirt.io/api/core/v1"
	fakenetworkclient "kubevirt.io/client-go/generated/network-attachment-definition-client/clientset/versioned/fake"
	"kubevirt.io/client-go/kubecli"

	"kubevirt.io/kubevirt/pkg/testutils"
	utiltype "kubevirt.io/kubevirt/pkg/util/types"
	"kubevirt.io/kubevirt/pkg/virt-controller/services"
)

var _ = Describe("Migration watcher", func() {

	var ctrl *gomock.Controller
	var vmiInterface *kubecli.MockVirtualMachineInstanceInterface
	var migrationInterface *kubecli.MockVirtualMachineInstanceMigrationInterface
	var migrationSource *framework.FakeControllerSource
	var vmiSource *framework.FakeControllerSource
	var podSource *framework.FakeControllerSource
	var vmiInformer cache.SharedIndexInformer
	var podInformer cache.SharedIndexInformer
	var migrationInformer cache.SharedIndexInformer
	var nodeInformer cache.SharedIndexInformer
	var pdbInformer cache.SharedIndexInformer
	var migrationPolicyInformer cache.SharedIndexInformer
	var namespaceStore cache.Store
	var stop chan struct{}
	var controller *MigrationController
	var recorder *record.FakeRecorder
	var mockQueue *testutils.MockWorkQueue
	var podFeeder *testutils.PodFeeder
	var virtClient *kubecli.MockKubevirtClient
	var kubeClient *fake.Clientset
	var networkClient *fakenetworkclient.Clientset
	var pvcInformer cache.SharedIndexInformer
	var qemuGid int64 = 107
	var migrationsClient *kubevirtfake.Clientset
	var namespace k8sv1.Namespace

	shouldExpectMigrationFinalizerRemoval := func(migration *virtv1.VirtualMachineInstanceMigration) {
		migrationInterface.EXPECT().Update(gomock.Any()).Do(func(arg interface{}) (interface{}, interface{}) {
			Expect(arg.(*virtv1.VirtualMachineInstanceMigration).Finalizers).To(BeEmpty())
			return arg, nil
		})
	}

	shouldExpectPodCreation := func(uid types.UID, migrationUid types.UID, expectedAntiAffinityCount int, expectedAffinityCount int, expectedNodeAffinityCount int) {
		// Expect pod creation
		kubeClient.Fake.PrependReactor("create", "pods", func(action testing.Action) (handled bool, obj k8sruntime.Object, err error) {
			update, ok := action.(testing.CreateAction)
			Expect(ok).To(BeTrue())
			Expect(update.GetObject().(*k8sv1.Pod).Labels[virtv1.CreatedByLabel]).To(Equal(string(uid)))
			Expect(update.GetObject().(*k8sv1.Pod).Labels[virtv1.MigrationJobLabel]).To(Equal(string(migrationUid)))

			Expect(update.GetObject().(*k8sv1.Pod).Spec.Affinity).ToNot(BeNil())
			Expect(update.GetObject().(*k8sv1.Pod).Spec.Affinity.PodAntiAffinity).ToNot(BeNil())
			Expect(update.GetObject().(*k8sv1.Pod).Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution).To(HaveLen(expectedAntiAffinityCount))

			if expectedAffinityCount > 0 {
				Expect(update.GetObject().(*k8sv1.Pod).Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution).To(HaveLen(expectedAffinityCount))
			}
			if expectedNodeAffinityCount > 0 {
				Expect(update.GetObject().(*k8sv1.Pod).Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms).To(HaveLen(expectedNodeAffinityCount))
			}

			return true, update.GetObject(), nil
		})
	}

	shouldExpectPodDeletion := func() {
		// Expect pod deletion
		kubeClient.Fake.PrependReactor("delete", "pods", func(action testing.Action) (handled bool, obj k8sruntime.Object, err error) {
			_, ok := action.(testing.DeleteAction)
			Expect(ok).To(BeTrue())
			return true, nil, nil
		})
	}

	shouldExpectAttachmentPodCreation := func(uid types.UID, migrationUid types.UID) {
		// Expect pod creation
		kubeClient.Fake.PrependReactor("create", "pods", func(action testing.Action) (handled bool, obj k8sruntime.Object, err error) {
			update, ok := action.(testing.CreateAction)
			Expect(ok).To(BeTrue())
			Expect(update.GetObject().(*k8sv1.Pod).Labels[virtv1.MigrationJobLabel]).To(Equal(string(migrationUid)))

			Expect(update.GetObject().(*k8sv1.Pod).Spec.Affinity).ToNot(BeNil())
			Expect(update.GetObject().(*k8sv1.Pod).Spec.Affinity.NodeAffinity).ToNot(BeNil())
			Expect(update.GetObject().(*k8sv1.Pod).Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms).To(HaveLen(1))

			return true, update.GetObject(), nil
		})
	}

	shouldExpectPDBPatch := func(vmi *virtv1.VirtualMachineInstance, vmim *virtv1.VirtualMachineInstanceMigration) {
		kubeClient.Fake.PrependReactor("patch", "poddisruptionbudgets", func(action testing.Action) (handled bool, obj k8sruntime.Object, err error) {
			patch, ok := action.(testing.PatchAction)
			Expect(ok).To(BeTrue())
			Expect(patch.GetPatchType()).To(Equal(types.StrategicMergePatchType))

			expectedPatch := fmt.Sprintf(`{"spec":{"minAvailable": 2},"metadata":{"labels":{"%s": "%s"}}}`, virtv1.MigrationNameLabel, vmim.Name)
			Expect(string(patch.GetPatch())).To(Equal(expectedPatch))

			pdb := newPDB(patch.GetName(), vmi, 2)
			pdb.Labels = map[string]string{
				virtv1.MigrationNameLabel: vmim.Name,
			}

			return true, pdb, nil
		})
	}

	shouldExpectGenericMigrationUpdate := func() {
		migrationInterface.EXPECT().UpdateStatus(gomock.Any()).DoAndReturn(func(arg interface{}) (interface{}, interface{}) {
			return arg, nil
		})
	}

	shouldExpectMigrationSchedulingState := func(migration *virtv1.VirtualMachineInstanceMigration) {
		migrationInterface.EXPECT().UpdateStatus(gomock.Any()).DoAndReturn(func(arg interface{}) (interface{}, interface{}) {
			Expect(arg.(*virtv1.VirtualMachineInstanceMigration).Status.Phase).To(Equal(virtv1.MigrationScheduling))
			return arg, nil
		})
	}

	shouldExpectMigrationPreparingTargetState := func(migration *virtv1.VirtualMachineInstanceMigration) {
		migrationInterface.EXPECT().UpdateStatus(gomock.Any()).DoAndReturn(func(arg interface{}) (interface{}, interface{}) {
			Expect(arg.(*virtv1.VirtualMachineInstanceMigration).Status.Phase).To(Equal(virtv1.MigrationPreparingTarget))
			return arg, nil
		})
	}

	shouldExpectMigrationTargetReadyState := func(migration *virtv1.VirtualMachineInstanceMigration) {
		migrationInterface.EXPECT().UpdateStatus(gomock.Any()).DoAndReturn(func(arg interface{}) (interface{}, interface{}) {
			Expect(arg.(*virtv1.VirtualMachineInstanceMigration).Status.Phase).To(Equal(virtv1.MigrationTargetReady))
			return arg, nil
		})
	}

	shouldExpectMigrationRunningState := func(migration *virtv1.VirtualMachineInstanceMigration) {
		migrationInterface.EXPECT().UpdateStatus(gomock.Any()).DoAndReturn(func(arg interface{}) (interface{}, interface{}) {
			Expect(arg.(*virtv1.VirtualMachineInstanceMigration).Status.Phase).To(Equal(virtv1.MigrationRunning))
			return arg, nil
		})
	}

	shouldExpectMigrationCompletedState := func(migration *virtv1.VirtualMachineInstanceMigration) {
		migrationInterface.EXPECT().UpdateStatus(gomock.Any()).Do(func(arg interface{}) (interface{}, interface{}) {
			Expect(arg.(*virtv1.VirtualMachineInstanceMigration).Status.Phase).To(Equal(virtv1.MigrationSucceeded))
			return arg, nil
		})
	}

	shouldExpectMigrationFailedState := func(migration *virtv1.VirtualMachineInstanceMigration) {
		migrationInterface.EXPECT().UpdateStatus(gomock.Any()).Do(func(arg interface{}) (interface{}, interface{}) {
			Expect(arg.(*virtv1.VirtualMachineInstanceMigration).Status.Phase).To(Equal(virtv1.MigrationFailed))
			return arg, nil
		})
	}

	shouldExpectMigrationDeletion := func(namePrefix string, times int) {

		migrationInterface.EXPECT().Delete(gomock.Any(), gomock.Any()).Times(times).Do(func(arg1 interface{}, arg2 interface{}) interface{} {
			Expect(arg1.(string)).To(ContainSubstring(namePrefix))
			return nil
		})
	}

	shouldExpectVirtualMachineInstancePatch := func(vmi *virtv1.VirtualMachineInstance, patch string) {
		vmiInterface.EXPECT().Patch(vmi.Name, types.JSONPatchType, []byte(patch), &metav1.PatchOptions{}).Return(vmi, nil)
	}

	shouldExpectMigrationCondition := func(migration *virtv1.VirtualMachineInstanceMigration, conditionType virtv1.VirtualMachineInstanceMigrationConditionType) {
		migrationInterface.EXPECT().UpdateStatus(gomock.Any()).Do(func(arg interface{}) (interface{}, interface{}) {
			vmim := arg.(*virtv1.VirtualMachineInstanceMigration)
			ExpectWithOffset(1, vmim.Name).To(Equal(migration.Name))

			foundConditionType := false
			for _, cond := range vmim.Status.Conditions {
				if cond.Type == conditionType {
					foundConditionType = true
					break
				}
			}
			ExpectWithOffset(1, foundConditionType).To(BeTrue(), fmt.Sprintf("condition of type %s is expected but cannot be found in migration %s", string(conditionType), migration.Name))

			return arg, nil
		})
	}

	syncCaches := func(stop chan struct{}) {
		go vmiInformer.Run(stop)
		go podInformer.Run(stop)
		go migrationInformer.Run(stop)
		go nodeInformer.Run(stop)
		go pdbInformer.Run(stop)
		go migrationPolicyInformer.Run(stop)

		Expect(cache.WaitForCacheSync(stop,
			vmiInformer.HasSynced,
			podInformer.HasSynced,
			migrationInformer.HasSynced,
			nodeInformer.HasSynced,
			pdbInformer.HasSynced,
			migrationPolicyInformer.HasSynced)).To(BeTrue())
	}

	initController := func(kvConfig *virtv1.KubeVirtConfiguration) {
		config, _, _ := testutils.NewFakeClusterConfigUsingKVConfig(kvConfig)

		controller = NewMigrationController(
			services.NewTemplateService("a", 240, "b", "c", "d", "e", "f", "g", pvcInformer.GetStore(), virtClient, config, qemuGid, "h"),
			vmiInformer,
			podInformer,
			migrationInformer,
			nodeInformer,
			pvcInformer,
			pdbInformer,
			migrationPolicyInformer,
			recorder,
			virtClient,
			config,
			namespaceStore,
			false,
		)
		// Wrap our workqueue to have a way to detect when we are done processing updates
		mockQueue = testutils.NewMockWorkQueue(controller.Queue)
		controller.Queue = mockQueue
		podFeeder = testutils.NewPodFeeder(mockQueue, podSource)
	}

	BeforeEach(func() {
		stop = make(chan struct{})
		ctrl = gomock.NewController(GinkgoT())
		virtClient = kubecli.NewMockKubevirtClient(ctrl)
		migrationInterface = kubecli.NewMockVirtualMachineInstanceMigrationInterface(ctrl)
		vmiInterface = kubecli.NewMockVirtualMachineInstanceInterface(ctrl)

		vmiInformer, vmiSource = testutils.NewFakeInformerFor(&virtv1.VirtualMachineInstance{})
		migrationInformer, migrationSource = testutils.NewFakeInformerFor(&virtv1.VirtualMachineInstanceMigration{})
		podInformer, podSource = testutils.NewFakeInformerFor(&k8sv1.Pod{})
		pdbInformer, _ = testutils.NewFakeInformerFor(&policyv1.PodDisruptionBudget{})
		migrationPolicyInformer, _ = testutils.NewFakeInformerFor(&migrationsv1.MigrationPolicy{})
		namespaceStore = cache.NewStore(cache.DeletionHandlingMetaNamespaceKeyFunc)
		recorder = record.NewFakeRecorder(100)
		recorder.IncludeObject = true
		nodeInformer, _ = testutils.NewFakeInformerFor(&k8sv1.Node{})

		pvcInformer, _ = testutils.NewFakeInformerFor(&k8sv1.PersistentVolumeClaim{})

		initController(&virtv1.KubeVirtConfiguration{})

		// Set up mock client
		kubeClient = fake.NewSimpleClientset()
		virtClient.EXPECT().VirtualMachineInstanceMigration(k8sv1.NamespaceDefault).Return(migrationInterface).AnyTimes()
		virtClient.EXPECT().VirtualMachineInstance(k8sv1.NamespaceDefault).Return(vmiInterface).AnyTimes()
		virtClient.EXPECT().CoreV1().Return(kubeClient.CoreV1()).AnyTimes()
		virtClient.EXPECT().PolicyV1().Return(kubeClient.PolicyV1()).AnyTimes()
		networkClient = fakenetworkclient.NewSimpleClientset()
		virtClient.EXPECT().NetworkClient().Return(networkClient).AnyTimes()
		migrationsClient = kubevirtfake.NewSimpleClientset()
		virtClient.EXPECT().MigrationPolicy().Return(migrationsClient.MigrationsV1alpha1().MigrationPolicies()).AnyTimes()

		namespace = k8sv1.Namespace{
			TypeMeta:   metav1.TypeMeta{Kind: "Namespace"},
			ObjectMeta: metav1.ObjectMeta{Name: metav1.NamespaceDefault},
		}

		// Make sure that all unexpected calls to kubeClient will fail
		kubeClient.Fake.PrependReactor("*", "*", func(action testing.Action) (handled bool, obj k8sruntime.Object, err error) {
			if action.GetVerb() == "get" && action.GetResource().Resource == "namespaces" {
				return true, &namespace, nil
			}

			Expect(action).To(BeNil())
			return true, nil, nil
		})

		syncCaches(stop)
	})

	AfterEach(func() {
		close(stop)
		// Ensure that we add checks for expected events to every test
		Expect(recorder.Events).To(BeEmpty())
	})

	addVirtualMachineInstance := func(vmi *virtv1.VirtualMachineInstance) {
		sourcePod := newSourcePodForVirtualMachine(vmi)
		ExpectWithOffset(1, podInformer.GetStore().Add(sourcePod)).To(Succeed())
		mockQueue.ExpectAdds(1)
		vmiSource.Add(vmi)
		mockQueue.Wait()
	}

	addMigration := func(migration *virtv1.VirtualMachineInstanceMigration) {
		mockQueue.ExpectAdds(1)
		migrationSource.Add(migration)
		mockQueue.Wait()
	}

	addNode := func(node *k8sv1.Node) {
		err := nodeInformer.GetIndexer().Add(node)
		Expect(err).ShouldNot(HaveOccurred())
	}

	addPDB := func(pdb *policyv1.PodDisruptionBudget) {
		err := pdbInformer.GetIndexer().Add(pdb)
		Expect(err).ShouldNot(HaveOccurred())
	}

	addMigrationPolicy := func(policy *migrationsv1.MigrationPolicy) {
		err := migrationPolicyInformer.GetIndexer().Add(policy)
		Expect(err).ShouldNot(HaveOccurred())
	}

	addMigrationPolicies := func(policies ...migrationsv1.MigrationPolicy) {
		for _, policy := range policies {
			addMigrationPolicy(&policy)
		}
	}

	getMigrationConfigPatch := func(customConfigs ...*virtv1.MigrationConfiguration) string {
		Expect(customConfigs).To(Or(BeEmpty(), HaveLen(1)))

		var migrationConfiguration *virtv1.MigrationConfiguration

		if len(customConfigs) > 0 && customConfigs[0] != nil {
			migrationConfiguration = customConfigs[0]
		} else {
			migrationConfiguration = controller.clusterConfig.GetMigrationConfiguration()
			Expect(migrationConfiguration).ToNot(BeNil())
		}

		marshalledConfigs, err := json.Marshal(migrationConfiguration)
		Expect(err).ToNot(HaveOccurred())

		return fmt.Sprintf(`"migrationConfiguration":%s`, string(marshalledConfigs))
	}

	Context("Migration with hotplug volumes", func() {
		var (
			vmi           *virtv1.VirtualMachineInstance
			migration     *virtv1.VirtualMachineInstanceMigration
			targetPod     *k8sv1.Pod
			attachmentPod *k8sv1.Pod
		)

		BeforeEach(func() {
			vmi = newVirtualMachineWithHotplugVolume("testvmi", virtv1.Running)
			migration = newMigration("testmigration", vmi.Name, virtv1.MigrationPending)
			targetPod = newTargetPodForVirtualMachine(vmi, migration, k8sv1.PodRunning)
			attachmentPod = newAttachmentPodForVirtualMachine(targetPod, migration, k8sv1.PodRunning)
		})

		It("should create target attachment pod", func() {
			addMigration(migration)
			addVirtualMachineInstance(vmi)
			podFeeder.Add(targetPod)
			shouldExpectAttachmentPodCreation(vmi.UID, migration.UID)

			controller.Execute()

			testutils.ExpectEvent(recorder, SuccessfulCreatePodReason)
		})

		It("should set migration state to scheduling if attachment pod exists", func() {
			addMigration(migration)
			addVirtualMachineInstance(vmi)
			podFeeder.Add(targetPod)
			podFeeder.Add(attachmentPod)

			shouldExpectMigrationSchedulingState(migration)
			controller.Execute()
		})

		It("should hand pod over to target virt-handler if attachment pod is ready and running", func() {
			vmi.Status.NodeName = "node02"
			migration.Status.Phase = virtv1.MigrationScheduled
			targetPod.Spec.NodeName = "node01"
			targetPod.Status.ContainerStatuses = []k8sv1.ContainerStatus{{
				Name: "compute", State: k8sv1.ContainerState{Running: &k8sv1.ContainerStateRunning{}},
			}}

			addMigration(migration)
			addVirtualMachineInstance(vmi)
			podFeeder.Add(targetPod)
			podFeeder.Add(attachmentPod)

			patch := fmt.Sprintf(`[{ "op": "add", "path": "/status/migrationState", "value": {"targetNode":"node01","targetPod":"%s","targetAttachmentPodUID":"%s","sourceNode":"node02","migrationUid":"testmigration",%s} }, { "op": "test", "path": "/metadata/labels", "value": {} }, { "op": "replace", "path": "/metadata/labels", "value": {"kubevirt.io/migrationTargetNodeName":"node01"} }]`, targetPod.Name, attachmentPod.UID, getMigrationConfigPatch())

			shouldExpectVirtualMachineInstancePatch(vmi, patch)

			controller.Execute()
			testutils.ExpectEvent(recorder, SuccessfulHandOverPodReason)
		})

		It("should fail the migration if the attachment pod goes to final state", func() {
			attachmentPod.Status.Phase = k8sv1.PodFailed

			addMigration(migration)
			addVirtualMachineInstance(vmi)
			podFeeder.Add(targetPod)
			podFeeder.Add(attachmentPod)

			shouldExpectMigrationFailedState(migration)

			controller.Execute()

			testutils.ExpectEvent(recorder, FailedMigrationReason)
		})
	})

	Context("Migration object in pending state", func() {

		It("should patch VMI with nonroot user", func() {
			vmi := newVirtualMachine("testvmi", virtv1.Running)
			delete(vmi.Annotations, virtv1.DeprecatedNonRootVMIAnnotation)
			vmi.Status.RuntimeUser = 0
			migration := newMigration("testmigration", vmi.Name, virtv1.MigrationPending)

			addMigration(migration)
			addVirtualMachineInstance(vmi)

			vmiInterface.EXPECT().Patch(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
				DoAndReturn(func(arg0, arg1, arg2, arg3 interface{}, arg4 ...interface{}) (result *v1.VirtualMachineInstance, err error) {
					Expect(arg0).To(Equal(vmi.Name))
					bytes := arg2.([]byte)
					patch := string(bytes)
					Expect(patch).To(ContainSubstring("/status/runtimeUser"))
					vmiReturn := vmi.DeepCopy()
					vmiReturn.Status.RuntimeUser = 107
					if vmiReturn.Annotations == nil {
						vmiReturn.Annotations = map[string]string{}
					}
					vmi.Annotations[virtv1.DeprecatedNonRootVMIAnnotation] = "true"
					return vmiReturn, nil
				})

			shouldExpectPodCreation(vmi.UID, migration.UID, 1, 0, 0)
			controller.Execute()
			testutils.ExpectEvents(recorder, SuccessfulCreatePodReason)

		})
		It("should create target pod", func() {
			vmi := newVirtualMachine("testvmi", virtv1.Running)
			migration := newMigration("testmigration", vmi.Name, virtv1.MigrationPending)

			addMigration(migration)
			addVirtualMachineInstance(vmi)
			shouldExpectPodCreation(vmi.UID, migration.UID, 1, 0, 0)

			controller.Execute()
			testutils.ExpectEvents(recorder, SuccessfulCreatePodReason)
		})
		It("should not create target pod if multiple pods exist in a non finalized state for VMI", func() {
			vmi := newVirtualMachine("testvmi", virtv1.Running)
			migration := newMigration("testmigration", vmi.Name, virtv1.MigrationPending)

			pod1 := newTargetPodForVirtualMachine(vmi, migration, k8sv1.PodPending)
			pod1.Labels[virtv1.MigrationJobLabel] = "some other job"
			pod2 := newTargetPodForVirtualMachine(vmi, migration, k8sv1.PodRunning)
			pod2.Labels[virtv1.MigrationJobLabel] = "some other job"
			Expect(podInformer.GetStore().Add(pod1)).To(Succeed())
			Expect(podInformer.GetStore().Add(pod2)).To(Succeed())

			addMigration(migration)
			addVirtualMachineInstance(vmi)

			controller.Execute()
		})

		It("should create another target pods if only 4 migrations are in progress", func() {
			// It should create a pod for this one
			vmi := newVirtualMachine("testvmi", virtv1.Running)
			migration := newMigration("testmigration", vmi.Name, virtv1.MigrationPending)

			addMigration(migration)
			addVirtualMachineInstance(vmi)

			// Ensure that 4 migrations are there which are in non-final state
			for i := 0; i < 4; i++ {
				vmi := newVirtualMachine(fmt.Sprintf("testvmi%v", i), virtv1.Running)
				vmi.Status.NodeName = fmt.Sprintf("node%v", i)
				migration := newMigration(fmt.Sprintf("testmigration%v", i), vmi.Name, virtv1.MigrationScheduling)

				addMigration(migration)
				addVirtualMachineInstance(vmi)
			}

			// Add two pending migrations without a target pod to see that tye get ignored
			for i := 0; i < 2; i++ {
				vmi := newVirtualMachine(fmt.Sprintf("xtestvmi%v", i), virtv1.Running)
				migration := newMigration(fmt.Sprintf("xtestmigration%v", i), vmi.Name, virtv1.MigrationPending)
				vmi.Status.NodeName = fmt.Sprintf("node%v", i)

				addMigration(migration)
				addVirtualMachineInstance(vmi)
			}

			shouldExpectPodCreation(vmi.UID, migration.UID, 1, 0, 0)
			controller.Execute()
			testutils.ExpectEvent(recorder, SuccessfulCreatePodReason)
		})

		It("should not overload the cluster and only run 5 migrations in parallel", func() {
			// It should create a pod for this one if we would not limit migrations
			vmi := newVirtualMachine("testvmi", virtv1.Running)
			migration := newMigration("testmigration", vmi.Name, virtv1.MigrationPending)

			addMigration(migration)
			addVirtualMachineInstance(vmi)

			// Ensure that 5 migrations are there which are in non-final state
			for i := 0; i < 5; i++ {
				vmi := newVirtualMachine(fmt.Sprintf("testvmi%v", i), virtv1.Running)
				migration := newMigration(fmt.Sprintf("testmigration%v", i), vmi.Name, virtv1.MigrationScheduling)
				vmi.Status.NodeName = fmt.Sprintf("node%v", i)

				addMigration(migration)
				addVirtualMachineInstance(vmi)
			}

			controller.Execute()
		})

		It("should not overload the cluster and detect pending migrations as running if they have a target pod", func() {
			// It should create a pod for this one if we would not limit migrations
			vmi := newVirtualMachine("testvmi", virtv1.Running)
			migration := newMigration("testmigration", vmi.Name, virtv1.MigrationPending)

			addMigration(migration)
			addVirtualMachineInstance(vmi)

			// Ensure that 3 migrations are there which are running
			for i := 0; i < 3; i++ {
				vmi := newVirtualMachine(fmt.Sprintf("testvmi%v", i), virtv1.Running)
				migration := newMigration(fmt.Sprintf("testmigration%v", i), vmi.Name, virtv1.MigrationScheduling)
				vmi.Status.NodeName = fmt.Sprintf("node%v", i)

				addMigration(migration)
				addVirtualMachineInstance(vmi)
			}

			// Ensure that 2 migrations are pending but have a target pod
			for i := 0; i < 2; i++ {
				vmi := newVirtualMachine(fmt.Sprintf("xtestvmi%v", i), virtv1.Running)
				migration := newMigration(fmt.Sprintf("xtestmigration%v", i), vmi.Name, virtv1.MigrationPending)
				pod := newTargetPodForVirtualMachine(vmi, migration, k8sv1.PodPending)
				vmi.Status.NodeName = fmt.Sprintf("node%v", i)

				addMigration(migration)
				addVirtualMachineInstance(vmi)
				Expect(podInformer.GetStore().Add(pod)).To(Succeed())
			}

			controller.Execute()
		})

		It("should create another target pods if there is only one outbound migration on the node", func() {
			// It should create a pod for this one
			vmi := newVirtualMachine("testvmi", virtv1.Running)
			migration := newMigration("testmigration", vmi.Name, virtv1.MigrationPending)

			addMigration(migration)
			addVirtualMachineInstance(vmi)

			// Ensure that 4 migrations are there which are in non-final state
			for i := 0; i < 1; i++ {
				vmi := newVirtualMachine(fmt.Sprintf("testvmi%v", i), virtv1.Running)
				migration := newMigration(fmt.Sprintf("testmigration%v", i), vmi.Name, virtv1.MigrationScheduling)

				addMigration(migration)
				addVirtualMachineInstance(vmi)
			}

			shouldExpectPodCreation(vmi.UID, migration.UID, 1, 0, 0)
			controller.Execute()
			testutils.ExpectEvent(recorder, SuccessfulCreatePodReason)
		})

		It("should not overload the node and only run 2 outbound migrations in parallel", func() {
			// It should create a pod for this one if we would not limit migrations
			vmi := newVirtualMachine("testvmi", virtv1.Running)
			migration := newMigration("testmigration", vmi.Name, virtv1.MigrationPending)

			addMigration(migration)
			addVirtualMachineInstance(vmi)

			// Ensure that 5 migrations are there which are in non-final state
			for i := 0; i < 2; i++ {
				vmi := newVirtualMachine(fmt.Sprintf("testvmi%v", i), virtv1.Running)
				migration := newMigration(fmt.Sprintf("testmigration%v", i), vmi.Name, virtv1.MigrationScheduling)

				addMigration(migration)
				addVirtualMachineInstance(vmi)
			}

			controller.Execute()
		})

		It("should create target pod and not override existing affinity rules", func() {
			vmi := newVirtualMachine("testvmi", virtv1.Running)
			antiAffinityTerm := k8sv1.PodAffinityTerm{
				LabelSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"somelabel": "somekey",
					},
				},
				TopologyKey: "kubernetes.io/hostname",
			}
			affinityTerm := k8sv1.PodAffinityTerm{
				LabelSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"someotherlabel": "someotherkey",
					},
				},
				TopologyKey: "kubernetes.io/hostname",
			}
			antiAffinityRule := &k8sv1.PodAntiAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []k8sv1.PodAffinityTerm{antiAffinityTerm},
			}
			affinityRule := &k8sv1.PodAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []k8sv1.PodAffinityTerm{affinityTerm},
			}

			nodeAffinityRule := &k8sv1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &k8sv1.NodeSelector{
					NodeSelectorTerms: []k8sv1.NodeSelectorTerm{
						{
							MatchExpressions: []k8sv1.NodeSelectorRequirement{
								{
									Key:      "kubernetes.io/hostname",
									Operator: k8sv1.NodeSelectorOpIn,
									Values:   []string{"somenode"},
								},
							},
						},
					},
				},
			}

			vmi.Spec.Affinity = &k8sv1.Affinity{
				NodeAffinity:    nodeAffinityRule,
				PodAntiAffinity: antiAffinityRule,
				PodAffinity:     affinityRule,
			}

			migration := newMigration("testmigration", vmi.Name, virtv1.MigrationPending)

			addMigration(migration)
			addVirtualMachineInstance(vmi)
			shouldExpectPodCreation(vmi.UID, migration.UID, 2, 1, 1)

			controller.Execute()

			testutils.ExpectEvent(recorder, SuccessfulCreatePodReason)
		})

		It("should place migration in scheduling state if pod exists", func() {
			vmi := newVirtualMachine("testvmi", virtv1.Running)
			migration := newMigration("testmigration", vmi.Name, virtv1.MigrationPending)
			pod := newTargetPodForVirtualMachine(vmi, migration, k8sv1.PodPending)

			addMigration(migration)
			addVirtualMachineInstance(vmi)
			podFeeder.Add(pod)

			shouldExpectMigrationSchedulingState(migration)
			controller.Execute()
		})

		DescribeTable("should handle pod stuck in unschedulable state", func(phase virtv1.VirtualMachineInstanceMigrationPhase, shouldTimeout bool, timeLapse int64, annotationVal string) {
			vmi := newVirtualMachine("testvmi", virtv1.Running)
			migration := newMigration("testmigration", vmi.Name, phase)

			if annotationVal != "" {
				migration.Annotations[virtv1.MigrationUnschedulablePodTimeoutSecondsAnnotation] = annotationVal
			}

			pod := newTargetPodForVirtualMachine(vmi, migration, k8sv1.PodPending)

			pod.Status.Conditions = append(pod.Status.Conditions, k8sv1.PodCondition{
				Type:   k8sv1.PodScheduled,
				Status: k8sv1.ConditionFalse,
				Reason: k8sv1.PodReasonUnschedulable,
			})
			now := now()
			pod.CreationTimestamp = metav1.NewTime(now.Time.Add(time.Duration(-timeLapse) * time.Second))

			addMigration(migration)
			addVirtualMachineInstance(vmi)
			podFeeder.Add(pod)

			if shouldTimeout {
				shouldExpectPodDeletion()
			}

			if phase == virtv1.MigrationPending {
				shouldExpectGenericMigrationUpdate()
			}
			controller.Execute()

			if phase != virtv1.MigrationScheduled {
				testutils.ExpectEvent(recorder, MigrationTargetPodUnschedulable)
			}

			if shouldTimeout {
				testutils.ExpectEvent(recorder, SuccessfulDeletePodReason)
			}
		},
			Entry("in pending state", virtv1.MigrationPending, true, defaultUnschedulablePendingTimeoutSeconds, ""),
			Entry("in scheduling state", virtv1.MigrationScheduling, true, defaultUnschedulablePendingTimeoutSeconds, ""),
			Entry("in scheduled state", virtv1.MigrationScheduled, false, defaultUnschedulablePendingTimeoutSeconds, ""),
			Entry("in pending state but timeout not hit", virtv1.MigrationPending, false, defaultUnschedulablePendingTimeoutSeconds-1, ""),
			Entry("in pending state with custom timeout", virtv1.MigrationPending, true, int64(10), "10"),
			Entry("in pending state with custom timeout not hit", virtv1.MigrationPending, false, int64(10), "11"),
			Entry("in scheduling state but timeout not hit", virtv1.MigrationScheduling, false, defaultUnschedulablePendingTimeoutSeconds-1, ""),
		)

		DescribeTable("should handle pod stuck in pending phase for extended period of time", func(phase virtv1.VirtualMachineInstanceMigrationPhase, shouldTimeout bool, timeLapse int64, annotationVal string) {
			vmi := newVirtualMachine("testvmi", virtv1.Running)
			migration := newMigration("testmigration", vmi.Name, phase)
			pod := newTargetPodForVirtualMachine(vmi, migration, k8sv1.PodPending)

			now := now()
			pod.CreationTimestamp = metav1.NewTime(now.Time.Add(time.Duration(-timeLapse) * time.Second))

			if annotationVal != "" {
				migration.Annotations[virtv1.MigrationPendingPodTimeoutSecondsAnnotation] = annotationVal
			}

			addMigration(migration)
			addVirtualMachineInstance(vmi)
			podFeeder.Add(pod)

			if shouldTimeout {
				shouldExpectPodDeletion()
			}

			if phase == virtv1.MigrationPending {
				shouldExpectGenericMigrationUpdate()
			}
			controller.Execute()

			if shouldTimeout {
				testutils.ExpectEvent(recorder, SuccessfulDeletePodReason)
			}
		},
			Entry("in pending state", virtv1.MigrationPending, true, defaultCatchAllPendingTimeoutSeconds, ""),
			Entry("in scheduling state", virtv1.MigrationScheduling, true, defaultCatchAllPendingTimeoutSeconds, ""),
			Entry("in scheduled state", virtv1.MigrationScheduled, false, defaultCatchAllPendingTimeoutSeconds, ""),
			Entry("in pending state but timeout not hit", virtv1.MigrationPending, false, defaultCatchAllPendingTimeoutSeconds-1, ""),
			Entry("in pending state with custom timeout", virtv1.MigrationPending, true, int64(10), "10"),
			Entry("in pending state with custom timeout not hit", virtv1.MigrationPending, false, int64(10), "11"),
			Entry("in scheduling state but timeout not hit", virtv1.MigrationScheduling, false, defaultCatchAllPendingTimeoutSeconds-1, ""),
		)
	})

	Context("Migration garbage collection", func() {
		DescribeTable("should garbage old finalized migration objects", func(phase virtv1.VirtualMachineInstanceMigrationPhase) {
			vmi := newVirtualMachine("testvmi", virtv1.Running)

			phasesToGarbageCollect := []virtv1.VirtualMachineInstanceMigrationPhase{
				virtv1.MigrationFailed,
				virtv1.MigrationSucceeded,
			}

			phasesToKeep := []virtv1.VirtualMachineInstanceMigrationPhase{
				virtv1.MigrationPhaseUnset,
				virtv1.MigrationPending,
				virtv1.MigrationScheduling,
				virtv1.MigrationPreparingTarget,
				virtv1.MigrationTargetReady,
				virtv1.MigrationRunning,
			}

			for _, curPhase := range phasesToKeep {
				for i := 0; i < 100; i++ {
					mCopy := newMigration(fmt.Sprintf("should-keep-%s-%d", curPhase, i), vmi.Name, curPhase)
					mCopy.Finalizers = []string{}

					mCopy.CreationTimestamp = metav1.Unix(int64(rand.Intn(100)), int64(0))

					Expect(migrationInformer.GetStore().Add(mCopy)).To(Succeed())
				}
			}

			finalizedMigrations := 0
			for _, curPhase := range phasesToGarbageCollect {
				for i := 0; i < 100; i++ {
					mCopy := newMigration(fmt.Sprintf("should-delete-%s-%d", curPhase, i), vmi.Name, curPhase)

					mCopy.CreationTimestamp = metav1.Unix(int64(rand.Intn(100)), int64(0))

					Expect(migrationInformer.GetStore().Add(mCopy)).To(Succeed())
					finalizedMigrations++
				}
			}

			keyMigration := newMigration("should-keep-key-migration", vmi.Name, phase)
			keyMigration.Finalizers = []string{}
			keyMigration.CreationTimestamp = metav1.Unix(int64(101), int64(0))
			addMigration(keyMigration)

			sourcePod := newSourcePodForVirtualMachine(vmi)
			Expect(podInformer.GetStore().Add(sourcePod)).To(Succeed())
			Expect(vmiInformer.GetStore().Add(vmi)).To(Succeed())

			if keyMigration.IsFinal() {
				finalizedMigrations++
				shouldExpectMigrationDeletion("should-delete", finalizedMigrations-defaultFinalizedMigrationGarbageCollectionBuffer)
			} else {
				migrationInterface.EXPECT().UpdateStatus(gomock.Any()).AnyTimes().DoAndReturn(func(arg interface{}) (interface{}, interface{}) {
					return arg, nil
				})
			}

			controller.Execute()
			testutils.IgnoreEvents(recorder)
		},
			Entry("in failed phase", virtv1.MigrationFailed),
			Entry("in succeeded phase", virtv1.MigrationSucceeded),
			Entry("in unset phase", virtv1.MigrationPhaseUnset),
			Entry("in pending phase", virtv1.MigrationPending),
			Entry("in scheduling phase", virtv1.MigrationScheduling),
			Entry("in preparing target phase", virtv1.MigrationPreparingTarget),
			Entry("in target ready phase", virtv1.MigrationTargetReady),
			Entry("in running phase", virtv1.MigrationRunning),
		)

	})

	Context("Migration should immediately fail if", func() {

		DescribeTable("vmi moves to final state", func(phase virtv1.VirtualMachineInstanceMigrationPhase) {
			vmi := newVirtualMachine("testvmi", virtv1.Succeeded)
			vmi.DeletionTimestamp = now()
			migration := newMigration("testmigration", vmi.Name, phase)
			vmi.Status.MigrationState = &virtv1.VirtualMachineInstanceMigrationState{
				MigrationUID: migration.UID,
			}
			pod := newTargetPodForVirtualMachine(vmi, migration, k8sv1.PodRunning)

			addMigration(migration)
			addVirtualMachineInstance(vmi)
			podFeeder.Add(pod)

			shouldExpectMigrationFailedState(migration)

			controller.Execute()

			testutils.ExpectEvent(recorder, FailedMigrationReason)
		},
			Entry("in running state", virtv1.MigrationRunning),
			Entry("in unset state", virtv1.MigrationPhaseUnset),
			Entry("in pending state", virtv1.MigrationPending),
			Entry("in scheduled state", virtv1.MigrationScheduled),
			Entry("in scheduling state", virtv1.MigrationScheduling),
			Entry("in target ready state", virtv1.MigrationTargetReady),
		)
		DescribeTable("Pod moves to final state", func(phase virtv1.VirtualMachineInstanceMigrationPhase) {
			vmi := newVirtualMachine("testvmi", virtv1.Running)
			migration := newMigration("testmigration", vmi.Name, phase)
			vmi.Status.MigrationState = &virtv1.VirtualMachineInstanceMigrationState{
				MigrationUID: migration.UID,
			}
			if phase == virtv1.MigrationTargetReady {
				vmi.Status.MigrationState.StartTimestamp = now()
			}
			pod := newTargetPodForVirtualMachine(vmi, migration, k8sv1.PodSucceeded)
			pod.Spec.NodeName = "node01"

			addMigration(migration)
			addVirtualMachineInstance(vmi)
			podFeeder.Add(pod)

			shouldExpectMigrationFailedState(migration)

			controller.Execute()

			testutils.ExpectEvent(recorder, FailedMigrationReason)
		},
			Entry("in running state", virtv1.MigrationRunning),
			Entry("in unset state", virtv1.MigrationPhaseUnset),
			Entry("in pending state", virtv1.MigrationPending),
			Entry("in scheduled state", virtv1.MigrationScheduled),
			Entry("in scheduling state", virtv1.MigrationScheduling),
			Entry("in target ready state", virtv1.MigrationTargetReady),
		)
		DescribeTable("VMI's migrate state moves to final state", func(phase virtv1.VirtualMachineInstanceMigrationPhase) {
			vmi := newVirtualMachine("testvmi", virtv1.Running)
			migration := newMigration("testmigration", vmi.Name, phase)
			vmi.Status.MigrationState = &virtv1.VirtualMachineInstanceMigrationState{
				MigrationUID:   migration.UID,
				Failed:         true,
				Completed:      true,
				StartTimestamp: now(),
				EndTimestamp:   now(),
			}
			pod := newTargetPodForVirtualMachine(vmi, migration, k8sv1.PodRunning)
			pod.Spec.NodeName = "node01"

			addMigration(migration)
			addVirtualMachineInstance(vmi)
			podFeeder.Add(pod)

			shouldExpectMigrationFailedState(migration)

			controller.Execute()

			testutils.ExpectEvent(recorder, FailedMigrationReason)
		},
			Entry("in running state", virtv1.MigrationRunning),
			Entry("in unset state", virtv1.MigrationPhaseUnset),
			Entry("in pending state", virtv1.MigrationPending),
			Entry("in scheduled state", virtv1.MigrationScheduled),
			Entry("in scheduling state", virtv1.MigrationScheduling),
			Entry("in target ready state", virtv1.MigrationTargetReady),
		)
	})
	Context("Migration object ", func() {

		DescribeTable("should hand pod over to target virt-handler if pod is ready and running", func(containerStatus []k8sv1.ContainerStatus) {
			vmi := newVirtualMachine("testvmi", virtv1.Running)
			vmi.Status.NodeName = "node02"
			migration := newMigration("testmigration", vmi.Name, virtv1.MigrationScheduled)
			pod := newTargetPodForVirtualMachine(vmi, migration, k8sv1.PodRunning)
			pod.Spec.NodeName = "node01"
			pod.Status.ContainerStatuses = containerStatus

			addMigration(migration)
			addVirtualMachineInstance(vmi)
			podFeeder.Add(pod)

			patch := fmt.Sprintf(`[{ "op": "add", "path": "/status/migrationState", "value": {"targetNode":"node01","targetPod":"%s","sourceNode":"node02","migrationUid":"testmigration",%s} }, { "op": "test", "path": "/metadata/labels", "value": {} }, { "op": "replace", "path": "/metadata/labels", "value": {"kubevirt.io/migrationTargetNodeName":"node01"} }]`, pod.Name, getMigrationConfigPatch())

			shouldExpectVirtualMachineInstancePatch(vmi, patch)

			controller.Execute()
			testutils.ExpectEvent(recorder, SuccessfulHandOverPodReason)
		},
			Entry("with running compute container and no infra container",
				[]k8sv1.ContainerStatus{{
					Name: "compute", State: k8sv1.ContainerState{Running: &k8sv1.ContainerStateRunning{}},
				}},
			),
			Entry("with running compute container and no ready istio-proxy container",
				[]k8sv1.ContainerStatus{{
					Name: "compute", State: k8sv1.ContainerState{Running: &k8sv1.ContainerStateRunning{}},
				}, {Name: "istio-proxy", State: k8sv1.ContainerState{Running: &k8sv1.ContainerStateRunning{}}, Ready: false}},
			),
		)

		DescribeTable("should not hand pod over to target virt-handler if pod is not ready and running", func(containerStatus []k8sv1.ContainerStatus) {
			vmi := newVirtualMachine("testvmi", virtv1.Running)
			vmi.Status.NodeName = "node02"
			migration := newMigration("testmigration", vmi.Name, virtv1.MigrationScheduled)
			pod := newTargetPodForVirtualMachine(vmi, migration, k8sv1.PodRunning)
			pod.Spec.NodeName = "node01"
			pod.Status.ContainerStatuses = containerStatus

			addMigration(migration)
			addVirtualMachineInstance(vmi)
			podFeeder.Add(pod)

			controller.Execute()
		},
			Entry("with not ready infra container and not ready compute container",
				[]k8sv1.ContainerStatus{{Name: "compute", Ready: false}, {Name: "kubevirt-infra", Ready: false}},
			),
			Entry("with not ready compute container and no infra container",
				[]k8sv1.ContainerStatus{{Name: "compute", Ready: false}},
			),
		)

		It("should hand pod over to target virt-handler with migration config", func() {
			vmi := newVirtualMachine("testvmi", virtv1.Running)
			vmi.Status.NodeName = "node02"
			migration := newMigration("testmigration", vmi.Name, virtv1.MigrationScheduled)

			pod := newTargetPodForVirtualMachine(vmi, migration, k8sv1.PodRunning)
			pod.Spec.NodeName = "node01"
			pod.Status.ContainerStatuses = []k8sv1.ContainerStatus{{
				Name: "compute", State: k8sv1.ContainerState{Running: &k8sv1.ContainerStateRunning{}},
			}}

			addMigration(migration)
			addVirtualMachineInstance(vmi)
			podFeeder.Add(pod)

			patch := fmt.Sprintf(`[{ "op": "add", "path": "/status/migrationState", "value": {"targetNode":"node01","targetPod":"%s","sourceNode":"node02","migrationUid":"testmigration",%s} }, { "op": "test", "path": "/metadata/labels", "value": {} }, { "op": "replace", "path": "/metadata/labels", "value": {"kubevirt.io/migrationTargetNodeName":"node01"} }]`, pod.Name, getMigrationConfigPatch())
			shouldExpectVirtualMachineInstancePatch(vmi, patch)

			controller.Execute()
			testutils.ExpectEvent(recorder, SuccessfulHandOverPodReason)
		})

		It("should hand pod over to target virt-handler overriding previous state", func() {
			vmi := newVirtualMachine("testvmi", virtv1.Running)
			vmi.Status.NodeName = "node02"
			vmi.Status.MigrationState = &virtv1.VirtualMachineInstanceMigrationState{
				MigrationUID: "1111-2222-3333-4444",
			}
			migration := newMigration("testmigration", vmi.Name, virtv1.MigrationScheduled)
			pod := newTargetPodForVirtualMachine(vmi, migration, k8sv1.PodRunning)
			pod.Spec.NodeName = "node01"
			pod.Status.ContainerStatuses = []k8sv1.ContainerStatus{{
				Name: "compute", State: k8sv1.ContainerState{Running: &k8sv1.ContainerStateRunning{}},
			}}

			addMigration(migration)
			addVirtualMachineInstance(vmi)
			podFeeder.Add(pod)

			patch := fmt.Sprintf(`[{ "op": "test", "path": "/status/migrationState", "value": {"migrationUid":"1111-2222-3333-4444"} }, { "op": "replace", "path": "/status/migrationState", "value": {"targetNode":"node01","targetPod":"%s","sourceNode":"node02","migrationUid":"testmigration",%s} }, { "op": "test", "path": "/metadata/labels", "value": {} }, { "op": "replace", "path": "/metadata/labels", "value": {"kubevirt.io/migrationTargetNodeName":"node01"} }]`, pod.Name, getMigrationConfigPatch())

			shouldExpectVirtualMachineInstancePatch(vmi, patch)

			controller.Execute()
			testutils.ExpectEvent(recorder, SuccessfulHandOverPodReason)
		})

		It("should not hand pod over target pod that's already handed over", func() {
			vmi := newVirtualMachine("testvmi", virtv1.Running)
			vmi.Status.NodeName = "node02"
			migration := newMigration("testmigration", vmi.Name, virtv1.MigrationScheduled)
			vmi.Status.MigrationState = &virtv1.VirtualMachineInstanceMigrationState{
				MigrationUID: migration.UID,
			}
			pod := newTargetPodForVirtualMachine(vmi, migration, k8sv1.PodPending)
			pod.Spec.NodeName = "node01"

			addMigration(migration)
			addVirtualMachineInstance(vmi)
			podFeeder.Add(pod)

			controller.Execute()

			// expect nothing to occur
		})

		It("should not transition to PreparingTarget if VMI MigrationState is outdated", func() {
			vmi := newVirtualMachine("testvmi", virtv1.Running)
			vmi.Status.NodeName = "node02"
			migration := newMigration("testmigration", vmi.Name, virtv1.MigrationScheduled)
			pod := newTargetPodForVirtualMachine(vmi, migration, k8sv1.PodRunning)
			pod.Spec.NodeName = "node01"

			const oldMigrationUID = "oldmigrationuid"
			vmi.Status.MigrationState = &virtv1.VirtualMachineInstanceMigrationState{
				MigrationUID: types.UID(oldMigrationUID),
			}
			addMigration(migration)
			addVirtualMachineInstance(vmi)
			podFeeder.Add(pod)

			patch := fmt.Sprintf(`[{ "op": "test", "path": "/status/migrationState", "value": {"migrationUid":"%s"} }, { "op": "replace", "path": "/status/migrationState", "value": {"targetNode":"node01","targetPod":"%s","sourceNode":"node02","migrationUid":"testmigration",%s} }, { "op": "test", "path": "/metadata/labels", "value": {} }, { "op": "replace", "path": "/metadata/labels", "value": {"kubevirt.io/migrationTargetNodeName":"node01"} }]`, oldMigrationUID, pod.Name, getMigrationConfigPatch())

			shouldExpectVirtualMachineInstancePatch(vmi, patch)

			controller.Execute()
			testutils.ExpectEvent(recorder, SuccessfulHandOverPodReason)
		})
		It("should transition to preparing target phase", func() {
			vmi := newVirtualMachine("testvmi", virtv1.Running)
			vmi.Status.NodeName = "node02"
			migration := newMigration("testmigration", vmi.Name, virtv1.MigrationScheduled)
			pod := newTargetPodForVirtualMachine(vmi, migration, k8sv1.PodRunning)
			pod.Spec.NodeName = "node01"

			vmi.Status.MigrationState = &virtv1.VirtualMachineInstanceMigrationState{
				MigrationUID: migration.UID,
				TargetNode:   "node01",
				SourceNode:   "node02",
				TargetPod:    pod.Name,
			}
			vmi.Labels[virtv1.MigrationTargetNodeNameLabel] = "node01"
			addMigration(migration)
			addVirtualMachineInstance(vmi)
			podFeeder.Add(pod)

			shouldExpectMigrationPreparingTargetState(migration)

			controller.Execute()
		})
		It("should transition to target prepared phase", func() {
			vmi := newVirtualMachine("testvmi", virtv1.Running)
			vmi.Status.NodeName = "node02"
			migration := newMigration("testmigration", vmi.Name, virtv1.MigrationPreparingTarget)
			pod := newTargetPodForVirtualMachine(vmi, migration, k8sv1.PodPending)
			pod.Spec.NodeName = "node01"

			vmi.Status.MigrationState = &virtv1.VirtualMachineInstanceMigrationState{
				MigrationUID:      migration.UID,
				TargetNode:        "node01",
				SourceNode:        "node02",
				TargetNodeAddress: "10.10.10.10:1234",
			}
			addMigration(migration)
			addVirtualMachineInstance(vmi)
			podFeeder.Add(pod)

			shouldExpectMigrationTargetReadyState(migration)

			controller.Execute()
		})
		It("should transition to running phase", func() {
			vmi := newVirtualMachine("testvmi", virtv1.Running)
			vmi.Status.NodeName = "node02"
			migration := newMigration("testmigration", vmi.Name, virtv1.MigrationTargetReady)
			pod := newTargetPodForVirtualMachine(vmi, migration, k8sv1.PodPending)
			pod.Spec.NodeName = "node01"

			vmi.Status.MigrationState = &virtv1.VirtualMachineInstanceMigrationState{
				MigrationUID:      migration.UID,
				TargetNode:        "node01",
				SourceNode:        "node02",
				TargetNodeAddress: "10.10.10.10:1234",
				StartTimestamp:    now(),
			}
			addMigration(migration)
			addVirtualMachineInstance(vmi)
			podFeeder.Add(pod)

			shouldExpectMigrationRunningState(migration)

			controller.Execute()
		})
		It("should transition to completed phase", func() {
			vmi := newVirtualMachine("testvmi", virtv1.Running)
			vmi.Status.NodeName = "node02"
			migration := newMigration("testmigration", vmi.Name, virtv1.MigrationRunning)
			pod := newTargetPodForVirtualMachine(vmi, migration, k8sv1.PodPending)
			pod.Spec.NodeName = "node01"

			vmi.Status.MigrationState = &virtv1.VirtualMachineInstanceMigrationState{
				MigrationUID:      migration.UID,
				TargetNode:        "node01",
				SourceNode:        "node02",
				TargetNodeAddress: "10.10.10.10:1234",
				StartTimestamp:    now(),
				EndTimestamp:      now(),
				Failed:            false,
				Completed:         true,
			}
			addMigration(migration)
			addVirtualMachineInstance(vmi)
			podFeeder.Add(pod)

			shouldExpectMigrationCompletedState(migration)

			controller.Execute()
			testutils.ExpectEvent(recorder, SuccessfulMigrationReason)
		})
		It("should delete itself if VMI no longer exists", func() {
			migration := newMigration("testmigration", "somevmi", virtv1.MigrationRunning)
			addMigration(migration)

			migrationInterface.EXPECT().Delete(gomock.Any(), gomock.Any()).Times(1).Return(nil)

			controller.Execute()
		})
		It("should abort the migration", func() {
			vmi := newVirtualMachine("testvmi", virtv1.Running)
			vmi.Status.NodeName = "node02"
			migration := newMigration("testmigration", vmi.Name, virtv1.MigrationRunning)
			condition := virtv1.VirtualMachineInstanceMigrationCondition{
				Type:          virtv1.VirtualMachineInstanceMigrationAbortRequested,
				Status:        k8sv1.ConditionTrue,
				LastProbeTime: *now(),
			}
			migration.Status.Conditions = append(migration.Status.Conditions, condition)
			pod := newTargetPodForVirtualMachine(vmi, migration, k8sv1.PodPending)
			pod.Spec.NodeName = "node01"
			migration.DeletionTimestamp = now()
			vmi.Status.MigrationState = &virtv1.VirtualMachineInstanceMigrationState{
				MigrationUID:      migration.UID,
				TargetNode:        "node01",
				SourceNode:        "node02",
				TargetNodeAddress: "10.10.10.10:1234",
				StartTimestamp:    now(),
			}
			controller.addHandOffKey(virtcontroller.MigrationKey(migration))
			addMigration(migration)
			addVirtualMachineInstance(vmi)
			podFeeder.Add(pod)

			vmiInterface.EXPECT().Patch(vmi.Name, types.JSONPatchType, gomock.Any(), &metav1.PatchOptions{}).Return(vmi, nil)
			controller.Execute()
			testutils.ExpectEvent(recorder, SuccessfulAbortMigrationReason)
		})
		DescribeTable("should finalize migration on VMI if target pod fails before migration starts", func(phase virtv1.VirtualMachineInstanceMigrationPhase, hasPod bool, podPhase k8sv1.PodPhase, initializeMigrationState bool) {
			vmi := newVirtualMachine("testvmi", virtv1.Running)
			vmi.Status.NodeName = "node02"
			migration := newMigration("testmigration", vmi.Name, phase)

			vmi.Status.MigrationState = nil
			if initializeMigrationState {
				vmi.Status.MigrationState = &virtv1.VirtualMachineInstanceMigrationState{
					MigrationUID: migration.UID,
					TargetNode:   "node01",
					SourceNode:   "node02",
				}
			}
			addMigration(migration)
			addVirtualMachineInstance(vmi)
			if hasPod {
				pod := newTargetPodForVirtualMachine(vmi, migration, podPhase)
				pod.Spec.NodeName = "node01"
				podFeeder.Add(pod)
			}

			if phase == virtv1.MigrationFailed {
				// This finalizer is added by the mutation webhook during creation
				migration.Finalizers = append(migration.Finalizers, virtv1.VirtualMachineInstanceMigrationFinalizer)
				shouldExpectMigrationFinalizerRemoval(migration)
			} else {
				shouldExpectMigrationFailedState(migration)
			}

			if initializeMigrationState {
				patch := `[{ "op": "test", "path": "/status/migrationState", "value": {"targetNode":"node01","sourceNode":"node02","migrationUid":"testmigration"} }, { "op": "replace", "path": "/status/migrationState", "value": {"startTimestamp":"%s","endTimestamp":"%s","targetNode":"node01","sourceNode":"node02","completed":true,"failed":true,"migrationUid":"testmigration"} }]`

				vmiInterface.EXPECT().Patch(vmi.Name, types.JSONPatchType, gomock.Any(), &metav1.PatchOptions{}).DoAndReturn(func(name interface{}, ptype interface{}, vmiStatusPatch []byte, options interface{}) (*virtv1.VirtualMachineInstance, error) {

					vmiSP, err := utiltype.UnmarshalPatch(vmiStatusPatch)
					Expect(err).ToNot(HaveOccurred())
					Expect(vmiSP).To(HaveLen(2))

					b, err := json.Marshal(vmiSP[1].Value)
					Expect(err).ToNot(HaveOccurred())

					newMS := virtv1.VirtualMachineInstanceMigrationState{}
					err = json.Unmarshal(b, &newMS)
					Expect(err).ToNot(HaveOccurred())
					Expect(newMS.StartTimestamp).ToNot(BeNil())
					Expect(newMS.EndTimestamp).ToNot(BeNil())

					expected := fmt.Sprintf(patch, newMS.StartTimestamp.UTC().Format(time.RFC3339), newMS.EndTimestamp.UTC().Format(time.RFC3339))
					Expect(expected).To(Equal(string(vmiStatusPatch)))

					return vmi, nil
				})
			}

			controller.Execute()

			// in this case, we have two failed events. one for the VMI and one on the Migration object.
			if initializeMigrationState {
				testutils.ExpectEvent(recorder, FailedMigrationReason)
			}
			if phase != virtv1.MigrationFailed {
				testutils.ExpectEvent(recorder, FailedMigrationReason)
			}
		},
			Entry("in preparing target state", virtv1.MigrationPreparingTarget, true, k8sv1.PodFailed, true),
			Entry("in target ready state", virtv1.MigrationTargetReady, true, k8sv1.PodFailed, true),
			Entry("in failed state", virtv1.MigrationFailed, true, k8sv1.PodFailed, true),
			Entry("in failed state before pod is created", virtv1.MigrationFailed, false, k8sv1.PodFailed, false),
			Entry("in failed state and pod does not exist", virtv1.MigrationFailed, false, k8sv1.PodFailed, false),
		)
		DescribeTable("with CPU mode which is", func(toDefineHostModelCPU bool) {
			const nodeName = "testNode"

			vmi := newVirtualMachine("testvmi", virtv1.Running)
			vmi.Status.NodeName = nodeName
			if toDefineHostModelCPU {
				vmi.Spec.Domain.CPU = &virtv1.CPU{Model: virtv1.CPUModeHostModel}
			}

			migration := newMigration("testmigration", vmi.Name, virtv1.MigrationPending)

			node := newNode(nodeName)
			if toDefineHostModelCPU {
				node.ObjectMeta.Labels = map[string]string{
					virtv1.HostModelCPULabel + "fake":              "true",
					virtv1.SupportedHostModelMigrationCPU + "fake": "true",
					virtv1.HostModelRequiredFeaturesLabel + "fake": "true",
				}
			}

			addMigration(migration)
			addVirtualMachineInstance(vmi)
			addNode(node)

			expectPodToHaveProperNodeSelector := func(pod *k8sv1.Pod) {
				podHasCpuModeLabelSelector := false
				for key, _ := range pod.Spec.NodeSelector {
					if strings.Contains(key, virtv1.SupportedHostModelMigrationCPU) {
						podHasCpuModeLabelSelector = true
						break
					}
				}

				Expect(podHasCpuModeLabelSelector).To(Equal(toDefineHostModelCPU))
			}
			kubeClient.Fake.PrependReactor("create", "pods", func(action testing.Action) (handled bool, obj k8sruntime.Object, err error) {
				creation, ok := action.(testing.CreateAction)
				Expect(ok).To(BeTrue())
				pod := creation.GetObject().(*k8sv1.Pod)
				expectPodToHaveProperNodeSelector(pod)
				return true, creation.GetObject(), nil
			})
			controller.Execute()

			testutils.ExpectEvent(recorder, SuccessfulCreatePodReason)
		},
			Entry("host-model should be targeted only to nodes which support the model", true),
			Entry("non-host-model should not be targeted to nodes which support the model", false),
		)
	})

	Context("Migration with protected VMI (PDB)", func() {
		It("should update PDB before starting the migration", func() {
			vmi := newVirtualMachine("testvmi", virtv1.Running)
			evictionStrategy := virtv1.EvictionStrategyLiveMigrate
			vmi.Spec.EvictionStrategy = &evictionStrategy
			migration := newMigration("testmigration", vmi.Name, virtv1.MigrationPending)
			pdb := newPDB("pdb-test", vmi, 1)

			addMigration(migration)
			addVirtualMachineInstance(vmi)
			addPDB(pdb)

			shouldExpectPDBPatch(vmi, migration)
			controller.Execute()

			testutils.ExpectEvents(recorder, successfulUpdatePodDisruptionBudgetReason)
		})
		It("should create the target Pod after the k8s PDB controller processed the PDB mutation", func() {
			vmi := newVirtualMachine("testvmi", virtv1.Running)
			evictionStrategy := virtv1.EvictionStrategyLiveMigrate
			vmi.Spec.EvictionStrategy = &evictionStrategy
			migration := newMigration("testmigration", vmi.Name, virtv1.MigrationPending)
			pdb := newPDB("pdb-test", vmi, 2)
			pdb.Generation = 42
			pdb.Status.DesiredHealthy = int32(pdb.Spec.MinAvailable.IntValue())
			pdb.Status.ObservedGeneration = pdb.Generation
			pdb.Labels = map[string]string{
				virtv1.MigrationNameLabel: migration.Name,
			}

			addMigration(migration)
			addVirtualMachineInstance(vmi)
			addPDB(pdb)

			shouldExpectPodCreation(vmi.UID, migration.UID, 1, 0, 0)

			controller.Execute()

			testutils.ExpectEvents(recorder, SuccessfulCreatePodReason)
		})

		Context("when cluster EvictionStrategy is set to 'LiveMigrate'", func() {
			BeforeEach(func() {
				evictionStrategy := virtv1.EvictionStrategyLiveMigrate
				initController(&virtv1.KubeVirtConfiguration{EvictionStrategy: &evictionStrategy})
			})

			It("should update PDB", func() {
				vmi := newVirtualMachine("testvmi", virtv1.Running)
				migration := newMigration("testmigration", vmi.Name, virtv1.MigrationPending)
				pdb := newPDB("pdb-test", vmi, 1)

				addMigration(migration)
				addVirtualMachineInstance(vmi)
				addPDB(pdb)

				shouldExpectPDBPatch(vmi, migration)
				controller.Execute()

				testutils.ExpectEvents(recorder, successfulUpdatePodDisruptionBudgetReason)
			})
		})
	})

	Context("Migration policy", func() {

		var vmi *virtv1.VirtualMachineInstance
		var stubNumber int64
		var stubResourceQuantity resource.Quantity
		var pod *k8sv1.Pod

		getExpectedVmiPatch := func(expectConfigUpdate bool, expectedConfigs *virtv1.MigrationConfiguration, migrationPolicy *migrationsv1.MigrationPolicy) string {
			var migrationPolicyNamePatch string

			if expectConfigUpdate {
				migrationPolicyNamePatch = fmt.Sprintf(`,"migrationPolicyName":"%s"`, migrationPolicy.Name)
			}

			patch := fmt.Sprintf(`[{ "op": "add", "path": "/status/migrationState", `+
				`"value": {"targetNode":"node01","targetPod":"%s","sourceNode":"tefwegwrerg","migrationUid":"testmigration"%s,%s} }, `+
				`{ "op": "test", "path": "/metadata/labels", "value": {"mp-key-0":"mp-value-0"} }, `+
				`{ "op": "replace", "path": "/metadata/labels", "value": {"kubevirt.io/migrationTargetNodeName":"node01","mp-key-0":"mp-value-0"} }]`, pod.Name, migrationPolicyNamePatch, getMigrationConfigPatch(expectedConfigs))

			return patch
		}

		BeforeEach(func() {
			stubNumber = 33425
			stubResourceQuantity = resource.MustParse("25Mi")

			By("Initialize VMI and migration")
			vmi = newVirtualMachine("testvmi", virtv1.Running)
			migration := newMigration("testmigration", vmi.Name, virtv1.MigrationScheduled)

			pod = newTargetPodForVirtualMachine(vmi, migration, k8sv1.PodRunning)
			pod.Spec.NodeName = "node01"
			pod.Status.ContainerStatuses = []k8sv1.ContainerStatus{{
				Name: "compute", State: k8sv1.ContainerState{Running: &k8sv1.ContainerStateRunning{}},
			}}

			addMigration(migration)
			addVirtualMachineInstance(vmi)
			podFeeder.Add(pod)
		})

		Context("matching and precedence", func() {

			type policyInfo struct {
				name                    string
				vmiMatchingLabels       int
				namespaceMatchingLabels int
			}

			DescribeTable("must be done correctly", func(expectedMatchedPolicyName string, policiesToDefine ...policyInfo) {
				policies := make([]migrationsv1.MigrationPolicy, 0)

				for _, info := range policiesToDefine {
					policy := tests.GetPolicyMatchedToVmi(info.name, vmi, &namespace, info.vmiMatchingLabels, info.namespaceMatchingLabels)
					policies = append(policies, *policy)
				}

				policyList := kubecli.NewMinimalMigrationPolicyList(policies...)
				actualMatchedPolicy := MatchPolicy(policyList, vmi, &namespace)

				Expect(actualMatchedPolicy.Name).To(Equal(expectedMatchedPolicyName))
			},
				Entry("only one policy should be matched", "one", policyInfo{"one", 1, 4}),
				Entry("most detail policy should be matched", "two",
					policyInfo{"one", 1, 4}, policyInfo{"two", 4, 2}),
				Entry("if two policies are detailed at the same level, matching policy should be the first name in lexicographic order (1)", "aa",
					policyInfo{"aa", 2, 2}, policyInfo{"zz", 2, 2}),
				Entry("if two policies are detailed at the same level, matching policy should be the first name in lexicographic order (2)", "aa",
					policyInfo{"zz", 2, 2}, policyInfo{"aa", 2, 2}),
			)

			It("policy with one non-fitting label should not match", func() {
				const labelKey = "mp-key-0"
				const labelValue = "mp-value-0"

				policy := tests.GetPolicyMatchedToVmi("testpolicy", vmi, &namespace, 4, 3)
				_, exists := policy.Spec.Selectors.VirtualMachineInstanceSelector[labelKey]
				Expect(exists).To(BeTrue())

				By("Changing one of the policy's labels to it won't match to VMI")
				policy.Spec.Selectors.VirtualMachineInstanceSelector[labelKey] = labelValue + "XYZ"
				policyList := kubecli.NewMinimalMigrationPolicyList(*policy)

				matchedPolicy := MatchPolicy(policyList, vmi, &namespace)
				Expect(matchedPolicy).To(BeNil())
			})

			It("when no policies exist, MatchPolicy() should return nil", func() {
				policyList := kubecli.NewMinimalMigrationPolicyList()
				matchedPolicy := MatchPolicy(policyList, vmi, &namespace)
				Expect(matchedPolicy).To(BeNil())
			})

			It("VMI labels should have precedence over namespace labels", func() {
				numberOfLabels := rand.Intn(5) + 1

				By(fmt.Sprintf("Defining two policies with %d labels, one with VMI labels and one with NS labels", numberOfLabels))
				policyWithNSLabels := tests.GetPolicyMatchedToVmi("aa-policy-with-ns-labels", vmi, &namespace, 0, numberOfLabels)
				policyWithVmiLabels := tests.GetPolicyMatchedToVmi("zz-policy-with-vmi-labels", vmi, &namespace, numberOfLabels, 0)

				policyList := kubecli.NewMinimalMigrationPolicyList(*policyWithNSLabels, *policyWithVmiLabels)

				By("Expecting VMI labels policy to be matched")
				matchedPolicy := MatchPolicy(policyList, vmi, &namespace)
				Expect(matchedPolicy.Name).To(Equal(policyWithVmiLabels.Name), "policy with VMI labels should match")
			})
		})

		DescribeTable("should override cluster-wide migration configurations when", func(defineMigrationPolicy func(*migrationsv1.MigrationPolicySpec), testMigrationConfigs func(configuration *virtv1.MigrationConfiguration), expectConfigUpdate bool) {
			By("Defining migration policy, matching it to vmi to posting it into the cluster")
			migrationPolicy := tests.GetPolicyMatchedToVmi("testpolicy", vmi, &namespace, 1, 0)
			defineMigrationPolicy(&migrationPolicy.Spec)
			addMigrationPolicies(*migrationPolicy)

			By("Calculating new migration config and validating it")
			expectedConfigs := getDefaultMigrationConfiguration()
			isConfigUpdated, err := migrationPolicy.GetMigrationConfByPolicy(expectedConfigs)
			Expect(err).ToNot(HaveOccurred())
			Expect(isConfigUpdated).To(Equal(expectConfigUpdate))
			testMigrationConfigs(expectedConfigs)

			By("Expecting right patch to occur")
			patch := getExpectedVmiPatch(expectConfigUpdate, expectedConfigs, migrationPolicy)
			shouldExpectVirtualMachineInstancePatch(vmi, patch)

			By("Running the controller")
			controller.Execute()
			testutils.ExpectEvent(recorder, SuccessfulHandOverPodReason)
		},
			Entry("allow auto coverage",
				func(p *migrationsv1.MigrationPolicySpec) { p.AllowAutoConverge = pointer.BoolPtr(true) },
				func(c *virtv1.MigrationConfiguration) {
					Expect(c.AllowAutoConverge).ToNot(BeNil())
					Expect(*c.AllowAutoConverge).To(BeTrue())
				},
				true,
			),
			Entry("deny auto coverage",
				func(p *migrationsv1.MigrationPolicySpec) { p.AllowAutoConverge = pointer.BoolPtr(false) },
				func(c *virtv1.MigrationConfiguration) {
					Expect(c.AllowAutoConverge).ToNot(BeNil())
					Expect(*c.AllowAutoConverge).To(BeFalse())
				},
				true,
			),
			Entry("set bandwidth per migration",
				func(p *migrationsv1.MigrationPolicySpec) { p.BandwidthPerMigration = &stubResourceQuantity },
				func(c *virtv1.MigrationConfiguration) {
					Expect(c.BandwidthPerMigration).ToNot(BeNil())
					Expect(c.BandwidthPerMigration.Equal(stubResourceQuantity)).To(BeTrue())
				},
				true,
			),
			Entry("set completion time per GiB",
				func(p *migrationsv1.MigrationPolicySpec) { p.CompletionTimeoutPerGiB = &stubNumber },
				func(c *virtv1.MigrationConfiguration) {
					Expect(c.CompletionTimeoutPerGiB).ToNot(BeNil())
					Expect(*c.CompletionTimeoutPerGiB).To(Equal(stubNumber))
				},
				true,
			),
			Entry("allow post copy",
				func(p *migrationsv1.MigrationPolicySpec) { p.AllowPostCopy = pointer.BoolPtr(true) },
				func(c *virtv1.MigrationConfiguration) {
					Expect(c.AllowPostCopy).ToNot(BeNil())
					Expect(*c.AllowPostCopy).To(BeTrue())
				},
				true,
			),
			Entry("deny post copy",
				func(p *migrationsv1.MigrationPolicySpec) { p.AllowPostCopy = pointer.BoolPtr(false) },
				func(c *virtv1.MigrationConfiguration) {
					Expect(c.AllowPostCopy).ToNot(BeNil())
					Expect(*c.AllowPostCopy).To(BeFalse())
				},
				true,
			),
			Entry("nothing is changed",
				func(p *migrationsv1.MigrationPolicySpec) {},
				func(c *virtv1.MigrationConfiguration) {},
				false,
			),
		)

	})

	Context("Migration of host-model VMI", func() {

		It("should trigger alert when no node supports host-model", func() {
			const nodeName = "testNode"

			By("Defining node (that does not support host model)")
			node := newNode(nodeName)

			By("Defining VMI")
			vmi := newVirtualMachine("testvmi", virtv1.Running)
			vmi.Status.NodeName = nodeName
			vmi.Spec.Domain.CPU = &virtv1.CPU{Model: virtv1.CPUModeHostModel}

			By("Defining migration")
			migration := newMigration("testmigration", vmi.Name, virtv1.MigrationScheduling)
			migration.Annotations[virtv1.MigrationUnschedulablePodTimeoutSecondsAnnotation] = "1"

			By("Defining target pod")
			targetPod := newTargetPodForVirtualMachine(vmi, migration, k8sv1.PodPending)
			if targetPod.Spec.NodeSelector == nil {
				targetPod.Spec.NodeSelector = make(map[string]string)
			}
			targetPod.Spec.NodeSelector[virtv1.HostModelCPULabel+"fake-model"] = "true"
			if node.Labels == nil {
				node.Labels = make(map[string]string)
			}
			node.Labels[virtv1.HostModelCPULabel+"other-fake-model"] = "true"
			targetPod.CreationTimestamp = metav1.NewTime(now().Time.Add(time.Duration(-defaultUnschedulablePendingTimeoutSeconds) * time.Second))
			targetPod.Status.Conditions = append(targetPod.Status.Conditions, k8sv1.PodCondition{
				Type:   k8sv1.PodScheduled,
				Status: k8sv1.ConditionFalse,
				Reason: k8sv1.PodReasonUnschedulable,
			})

			By("Adding objects to mocked cluster")
			addNode(node)
			addMigration(migration)
			addVirtualMachineInstance(vmi)
			podFeeder.Add(targetPod)

			By("Running controller and setting expectations")
			shouldExpectPodDeletion()
			controller.Execute()
			testutils.ExpectEvent(recorder, NoSuitableNodesForHostModelMigration)
			testutils.ExpectEvent(recorder, MigrationTargetPodUnschedulable)
			testutils.ExpectEvent(recorder, SuccessfulDeletePodReason)
		})

	})

	Context("Migration abortion before hand-off to virt-handler", func() {

		var vmi *virtv1.VirtualMachineInstance
		var migration *virtv1.VirtualMachineInstanceMigration

		BeforeEach(func() {
			vmi = newVirtualMachine("testvmi", virtv1.Running)
			migration = newMigration("testmigration", vmi.Name, virtv1.MigrationPending)
			migration.DeletionTimestamp = now()

			Expect(controller.isMigrationHandedOff(migration, vmi)).To(BeFalse(), "this test assumes migration was not handed off yet")
			addMigration(migration)
			addVirtualMachineInstance(vmi)
		})

		AfterEach(func() {
			controller.Execute()
			testutils.ExpectEvent(recorder, FailedMigrationReason)
		})

		It("expect abort condition", func() {
			shouldExpectMigrationCondition(migration, virtv1.VirtualMachineInstanceMigrationAbortRequested)
		})

		It("expect failure phase", func() {
			shouldExpectMigrationFailedState(migration)
		})
	})
})

func newPDB(name string, vmi *virtv1.VirtualMachineInstance, pods int) *policyv1.PodDisruptionBudget {
	minAvailable := intstr.FromInt(pods)

	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(vmi, virtv1.VirtualMachineInstanceGroupVersionKind),
			},
			Name:      name,
			Namespace: vmi.Namespace,
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvailable,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					virtv1.CreatedByLabel: string(vmi.UID),
				},
			},
		},
	}
}

func newMigration(name string, vmiName string, phase virtv1.VirtualMachineInstanceMigrationPhase) *virtv1.VirtualMachineInstanceMigration {

	migration := &virtv1.VirtualMachineInstanceMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: k8sv1.NamespaceDefault,
			Annotations: map[string]string{
				virtv1.ControllerAPILatestVersionObservedAnnotation:  virtv1.ApiLatestVersion,
				virtv1.ControllerAPIStorageVersionObservedAnnotation: virtv1.ApiStorageVersion,
			},
		},
		Spec: virtv1.VirtualMachineInstanceMigrationSpec{
			VMIName: vmiName,
		},
	}
	migration.TypeMeta = metav1.TypeMeta{
		APIVersion: virtv1.GroupVersion.String(),
		Kind:       "VirtualMachineInstanceMigration",
	}
	migration.UID = types.UID(name)
	migration.Status.Phase = phase
	return migration
}

func newVirtualMachine(name string, phase virtv1.VirtualMachineInstancePhase) *virtv1.VirtualMachineInstance {
	vmi := api.NewMinimalVMI(name)
	vmi.UID = types.UID(name)
	vmi.Status.Phase = phase
	vmi.Status.NodeName = "tefwegwrerg"
	vmi.ObjectMeta.Labels = make(map[string]string)
	// This would be set by mutation webhook
	vmi.Status.RuntimeUser = 107
	vmi.ObjectMeta.Annotations = map[string]string{
		virtv1.DeprecatedNonRootVMIAnnotation: "true",
	}
	return vmi
}

func newVirtualMachineWithHotplugVolume(name string, phase virtv1.VirtualMachineInstancePhase) *virtv1.VirtualMachineInstance {
	vmi := newVirtualMachine(name, phase)
	vmi.Status.VolumeStatus = []virtv1.VolumeStatus{
		{
			HotplugVolume: &virtv1.HotplugVolumeStatus{},
		},
	}
	return vmi
}

func newSourcePodForVirtualMachine(vmi *virtv1.VirtualMachineInstance) *k8sv1.Pod {
	return &k8sv1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rand.String(10),
			Namespace: vmi.Namespace,
			Labels: map[string]string{
				virtv1.AppLabel:       "virt-launcher",
				virtv1.CreatedByLabel: string(vmi.UID),
			},
			Annotations: map[string]string{
				virtv1.DomainAnnotation: vmi.Name,
			},
		},
		Status: k8sv1.PodStatus{
			Phase: k8sv1.PodRunning,
			ContainerStatuses: []k8sv1.ContainerStatus{
				{Ready: true, Name: "test"},
			},
		},
		Spec: k8sv1.PodSpec{
			NodeName: vmi.Status.NodeName,
			Volumes:  []k8sv1.Volume{},
		},
	}
}

func newTargetPodForVirtualMachine(vmi *virtv1.VirtualMachineInstance, migration *virtv1.VirtualMachineInstanceMigration, phase k8sv1.PodPhase) *k8sv1.Pod {
	return &k8sv1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rand.String(10),
			Namespace: vmi.Namespace,
			Labels: map[string]string{
				virtv1.AppLabel:          "virt-launcher",
				virtv1.CreatedByLabel:    string(vmi.UID),
				virtv1.MigrationJobLabel: string(migration.UID),
			},
			Annotations: map[string]string{
				virtv1.DomainAnnotation:           vmi.Name,
				virtv1.MigrationJobNameAnnotation: migration.Name,
			},
		},
		Status: k8sv1.PodStatus{
			Phase: phase,
			ContainerStatuses: []k8sv1.ContainerStatus{
				{Ready: true, Name: "test"},
			},
		},
	}
}

func newAttachmentPodForVirtualMachine(ownerPod *k8sv1.Pod, migration *virtv1.VirtualMachineInstanceMigration, phase k8sv1.PodPhase) *k8sv1.Pod {
	return &k8sv1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rand.String(10),
			Namespace: ownerPod.Namespace,
			UID:       "test-uid",
			Labels: map[string]string{
				virtv1.AppLabel:          "hotplug-disk",
				virtv1.MigrationJobLabel: string(migration.UID),
			},
			Annotations: map[string]string{
				virtv1.MigrationJobNameAnnotation: migration.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(ownerPod, schema.GroupVersionKind{
					Group:   k8sv1.SchemeGroupVersion.Group,
					Version: k8sv1.SchemeGroupVersion.Version,
					Kind:    "Pod",
				}),
			},
		},
		Status: k8sv1.PodStatus{
			Phase: phase,
			ContainerStatuses: []k8sv1.ContainerStatus{
				{Ready: true, Name: "test"},
			},
		},
	}
}

func newNode(name string) *k8sv1.Node {
	node := &k8sv1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		TypeMeta: metav1.TypeMeta{
			Kind:       "Node",
			APIVersion: virtv1.GroupVersion.String(),
		},
	}

	node.Status.Phase = k8sv1.NodeRunning

	return node
}

func getDefaultMigrationConfiguration() *virtv1.MigrationConfiguration {
	nodeTaintKey := "kubevirt.io/drain"
	parallelOutboundMigrationsPerNode := virtconfig.ParallelOutboundMigrationsPerNodeDefault
	parallelMigrationsPerCluster := virtconfig.ParallelMigrationsPerClusterDefault
	allowAutoConverge := virtconfig.MigrationAllowAutoConverge
	bandwidthPerMigration := resource.MustParse(virtconfig.BandwithPerMigrationDefault)
	completionTimeoutPerGiB := virtconfig.MigrationCompletionTimeoutPerGiB
	progressTimeout := virtconfig.MigrationProgressTimeout
	unsafeMigrationOverride := virtconfig.DefaultUnsafeMigrationOverride
	allowPostCopy := virtconfig.MigrationAllowPostCopy

	return &virtv1.MigrationConfiguration{
		NodeDrainTaintKey:                 &nodeTaintKey,
		ParallelOutboundMigrationsPerNode: &parallelOutboundMigrationsPerNode,
		ParallelMigrationsPerCluster:      &parallelMigrationsPerCluster,
		AllowAutoConverge:                 &allowAutoConverge,
		BandwidthPerMigration:             &bandwidthPerMigration,
		CompletionTimeoutPerGiB:           &completionTimeoutPerGiB,
		ProgressTimeout:                   &progressTimeout,
		UnsafeMigrationOverride:           &unsafeMigrationOverride,
		AllowPostCopy:                     &allowPostCopy,
	}
}
