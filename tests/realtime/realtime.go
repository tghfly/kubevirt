package realtime

import (
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/resource"

	k8smetav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"

	virtconfig "kubevirt.io/kubevirt/pkg/virt-config"
	"kubevirt.io/kubevirt/tests"
	"kubevirt.io/kubevirt/tests/console"
	cd "kubevirt.io/kubevirt/tests/containerdisk"
	"kubevirt.io/kubevirt/tests/exec"
	"kubevirt.io/kubevirt/tests/framework/checks"
	"kubevirt.io/kubevirt/tests/libvmi"
	"kubevirt.io/kubevirt/tests/util"
)

const (
	memory                         = "512Mi"
	tuneAdminRealtimeCloudInitData = `#cloud-config
password: fedora
chpasswd: { expire: False }
bootcmd:
   - sudo tuned-adm profile realtime
`
)

func newFedoraRealtime(realtimeMask string) *v1.VirtualMachineInstance {
	return libvmi.New(
		libvmi.WithRng(),
		libvmi.WithContainerImage(cd.ContainerDiskFor(cd.ContainerDiskFedoraRealtime)),
		libvmi.WithCloudInitNoCloudUserData(tuneAdminRealtimeCloudInitData, true),
		libvmi.WithLimitMemory(memory),
		libvmi.WithLimitCPU("2"),
		libvmi.WithResourceMemory(memory),
		libvmi.WithResourceCPU("2"),
		libvmi.WithCPUModel(v1.CPUModeHostPassthrough),
		libvmi.WithDedicatedCPUPlacement(),
		libvmi.WithRealtimeMask(realtimeMask),
		libvmi.WithNUMAGuestMappingPassthrough(),
		libvmi.WithHugepages("2Mi"),
		libvmi.WithGuestMemory(memory),
	)
}

func byStartingTheVMI(vmi *v1.VirtualMachineInstance, virtClient kubecli.KubevirtClient) {
	By("Starting a VirtualMachineInstance")
	var err error
	vmi, err = virtClient.VirtualMachineInstance(util.NamespaceTestDefault).Create(vmi)
	Expect(err).ToNot(HaveOccurred())
	tests.WaitForSuccessfulVMIStart(vmi)
}

var _ = Describe("[sig-compute-realtime][Serial]Realtime", func() {

	var virtClient kubecli.KubevirtClient

	BeforeEach(func() {
		var err error
		virtClient, err = kubecli.GetKubevirtClient()
		Expect(err).ToNot(HaveOccurred())
	})

	Context("should start the realtime VM", func() {
		BeforeEach(func() {
			checks.SkipTestIfNoFeatureGate(virtconfig.NUMAFeatureGate)
			checks.SkipTestIfNoFeatureGate(virtconfig.CPUManager)
			checks.SkipTestIfNotRealtimeCapable()
		})

		It("when no mask is specified", func() {
			const noMask = ""
			vmi := newFedoraRealtime(noMask)
			byStartingTheVMI(vmi, virtClient)
			By("Validating VCPU scheduler placement information")
			pod := tests.GetRunningPodByVirtualMachineInstance(vmi, util.NamespaceTestDefault)
			psOutput, err := exec.ExecuteCommandOnPod(
				virtClient,
				pod,
				"compute",
				[]string{tests.BinBash, "-c", "ps -u qemu -L -o policy,rtprio,psr|grep FF| awk '{print $2}'"},
			)
			Expect(err).ToNot(HaveOccurred())
			slice := strings.Split(strings.TrimSpace(psOutput), "\n")
			Expect(slice).To(HaveLen(2))
			for _, l := range slice {
				Expect(parsePriority(l)).To(BeEquivalentTo(1))
			}
			By("Validating that the memory lock limits are higher than the memory requested")
			psOutput, err = exec.ExecuteCommandOnPod(
				virtClient,
				pod,
				"compute",
				[]string{tests.BinBash, "-c", "grep 'locked memory' /proc/$(ps -u qemu -o pid --noheader|xargs)/limits |tr -s ' '| awk '{print $4\" \"$5}'"},
			)
			Expect(err).ToNot(HaveOccurred())
			limits := strings.Split(strings.TrimSpace(psOutput), " ")
			softLimit, err := strconv.ParseInt(limits[0], 10, 64)
			Expect(err).ToNot(HaveOccurred())
			hardLimit, err := strconv.ParseInt(limits[1], 10, 64)
			Expect(err).ToNot(HaveOccurred())
			Expect(softLimit).To(Equal(hardLimit))
			mustParse := resource.MustParse(memory)
			requested, canConvert := mustParse.AsInt64()
			Expect(canConvert).To(BeTrue())
			Expect(hardLimit).To(BeNumerically(">", requested))
			By("checking if the guest is still running")
			vmi, err = virtClient.VirtualMachineInstance(util.NamespaceTestDefault).Get(vmi.Name, &k8smetav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(vmi.Status.Phase).To(Equal(v1.Running))
			Expect(console.LoginToFedora(vmi)).To(Succeed())
		})

		It("when realtime mask is specified", func() {
			vmi := newFedoraRealtime("0-1,^1")
			byStartingTheVMI(vmi, virtClient)
			pod := tests.GetRunningPodByVirtualMachineInstance(vmi, util.NamespaceTestDefault)
			By("Validating VCPU scheduler placement information")
			psOutput, err := exec.ExecuteCommandOnPod(
				virtClient,
				pod,
				"compute",
				[]string{tests.BinBash, "-c", "ps -u qemu -L -o policy,rtprio,psr|grep FF| awk '{print $2}'"},
			)
			Expect(err).ToNot(HaveOccurred())
			slice := strings.Split(strings.TrimSpace(psOutput), "\n")
			Expect(slice).To(HaveLen(1))
			Expect(parsePriority(slice[0])).To(BeEquivalentTo(1))

			By("Validating the VCPU mask matches the scheduler profile for all cores")
			psOutput, err = exec.ExecuteCommandOnPod(
				virtClient,
				pod,
				"compute",
				[]string{tests.BinBash, "-c", "ps -cT -u qemu  |grep -i cpu |awk '{print $3\" \" $8}'"},
			)
			Expect(err).ToNot(HaveOccurred())
			slice = strings.Split(strings.TrimSpace(psOutput), "\n")
			Expect(slice).To(HaveLen(2))
			Expect(slice[0]).To(Equal("FF 0/KVM"))
			Expect(slice[1]).To(Equal("TS 1/KVM"))

			By("checking if the guest is still running")
			vmi, err = virtClient.VirtualMachineInstance(util.NamespaceTestDefault).Get(vmi.Name, &k8smetav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(vmi.Status.Phase).To(Equal(v1.Running))
			Expect(console.LoginToFedora(vmi)).To(Succeed())
		})
	})
	Context("with NonRoot", func() {
		BeforeEach(func() {
			if checks.HasFeature(virtconfig.Root) {
				Skip("Root feature gate is enabled")
			}
		})

		It("should deny the realtime VM", func() {
			vmi := newFedoraRealtime("0-1,^1")
			_, err := virtClient.VirtualMachineInstance(util.NamespaceTestDefault).Create(vmi)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("Root feature gate is not used"))
		})
	})

})

func parsePriority(psLine string) int64 {
	s := strings.Split(psLine, " ")
	Expect(s).To(HaveLen(1))
	prio, err := strconv.ParseInt(s[0], 10, 8)
	Expect(err).NotTo(HaveOccurred())
	return prio
}
