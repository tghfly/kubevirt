package services

import (
	"context"
	"fmt"
	"strings"

	k8sv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"

	"kubevirt.io/kubevirt/pkg/downwardmetrics"
	"kubevirt.io/kubevirt/pkg/util"
	"kubevirt.io/kubevirt/pkg/util/hardware"
	virtconfig "kubevirt.io/kubevirt/pkg/virt-config"
)

type ResourceRendererOption func(renderer *ResourceRenderer)

type ResourceRenderer struct {
	vmLimits           k8sv1.ResourceList
	vmRequests         k8sv1.ResourceList
	calculatedLimits   k8sv1.ResourceList
	calculatedRequests k8sv1.ResourceList
}

type resourcePredicate func(*v1.VirtualMachineInstance) bool

type VMIResourcePredicates struct {
	resourceRules []VMIResourceRule
	vmi           *v1.VirtualMachineInstance
}

type VMIResourceRule struct {
	predicate resourcePredicate
	option    ResourceRendererOption
}

func not(p resourcePredicate) resourcePredicate {
	return func(vmi *v1.VirtualMachineInstance) bool {
		return !p(vmi)
	}
}
func NewVMIResourceRule(p resourcePredicate, option ResourceRendererOption) VMIResourceRule {
	return VMIResourceRule{predicate: p, option: option}
}

func doesVMIRequireDedicatedCPU(vmi *v1.VirtualMachineInstance) bool {
	return vmi.IsCPUDedicated()
}

func NewResourceRenderer(vmLimits k8sv1.ResourceList, vmRequests k8sv1.ResourceList, options ...ResourceRendererOption) *ResourceRenderer {
	limits := map[k8sv1.ResourceName]resource.Quantity{}
	requests := map[k8sv1.ResourceName]resource.Quantity{}
	copyResources(vmLimits, limits)
	copyResources(vmRequests, requests)

	resourceRenderer := &ResourceRenderer{
		vmLimits:           limits,
		vmRequests:         requests,
		calculatedLimits:   map[k8sv1.ResourceName]resource.Quantity{},
		calculatedRequests: map[k8sv1.ResourceName]resource.Quantity{},
	}

	for _, opt := range options {
		opt(resourceRenderer)
	}
	return resourceRenderer
}

func (rr *ResourceRenderer) Limits() k8sv1.ResourceList {
	podLimits := map[k8sv1.ResourceName]resource.Quantity{}
	copyResources(rr.calculatedLimits, podLimits)
	copyResources(rr.vmLimits, podLimits)
	return podLimits
}

func (rr *ResourceRenderer) Requests() k8sv1.ResourceList {
	podRequests := map[k8sv1.ResourceName]resource.Quantity{}
	copyResources(rr.calculatedRequests, podRequests)
	copyResources(rr.vmRequests, podRequests)
	return podRequests
}

func (rr *ResourceRenderer) ResourceRequirements() k8sv1.ResourceRequirements {
	return k8sv1.ResourceRequirements{
		Limits:   rr.Limits(),
		Requests: rr.Requests(),
	}
}

func WithEphemeralStorageRequest() ResourceRendererOption {
	return func(renderer *ResourceRenderer) {
		// Add ephemeral storage request to container to be used by Kubevirt. This amount of ephemeral storage
		// should be added to the user's request.
		ephemeralStorageOverhead := resource.MustParse(ephemeralStorageOverheadSize)
		ephemeralStorageRequested := renderer.vmRequests[k8sv1.ResourceEphemeralStorage]
		ephemeralStorageRequested.Add(ephemeralStorageOverhead)
		renderer.vmRequests[k8sv1.ResourceEphemeralStorage] = ephemeralStorageRequested

		if ephemeralStorageLimit, ephemeralStorageLimitDefined := renderer.vmLimits[k8sv1.ResourceEphemeralStorage]; ephemeralStorageLimitDefined {
			ephemeralStorageLimit.Add(ephemeralStorageOverhead)
			renderer.vmLimits[k8sv1.ResourceEphemeralStorage] = ephemeralStorageLimit
		}
	}
}

func WithoutDedicatedCPU(cpu *v1.CPU, cpuAllocationRatio int) ResourceRendererOption {
	return func(renderer *ResourceRenderer) {
		vcpus := calcVCPUs(cpu)
		if vcpus != 0 && cpuAllocationRatio > 0 {
			val := float64(vcpus) / float64(cpuAllocationRatio)
			vcpusStr := fmt.Sprintf("%g", val)
			if val < 1 {
				val *= 1000
				vcpusStr = fmt.Sprintf("%gm", val)
			}
			renderer.calculatedRequests[k8sv1.ResourceCPU] = resource.MustParse(vcpusStr)
		}
	}
}

func WithHugePages(vmMemory *v1.Memory, memoryOverhead *resource.Quantity) ResourceRendererOption {
	return func(renderer *ResourceRenderer) {
		hugepageType := k8sv1.ResourceName(k8sv1.ResourceHugePagesPrefix + vmMemory.Hugepages.PageSize)
		hugepagesMemReq := renderer.vmRequests.Memory()

		// If requested, use the guest memory to allocate hugepages
		if vmMemory != nil && vmMemory.Guest != nil {
			requests := hugepagesMemReq.Value()
			guest := vmMemory.Guest.Value()
			if requests > guest {
				hugepagesMemReq = vmMemory.Guest
			}
		}
		renderer.calculatedRequests[hugepageType] = *hugepagesMemReq
		renderer.calculatedLimits[hugepageType] = *hugepagesMemReq

		reqMemDiff := resource.NewScaledQuantity(0, resource.Kilo)
		limMemDiff := resource.NewScaledQuantity(0, resource.Kilo)
		// In case the guest memory and the requested memory are different, add the difference
		// to the overhead
		if vmMemory != nil && vmMemory.Guest != nil {
			requests := renderer.vmRequests.Memory().Value()
			limits := renderer.vmLimits.Memory().Value()
			guest := vmMemory.Guest.Value()
			if requests > guest {
				reqMemDiff.Add(*renderer.vmRequests.Memory())
				reqMemDiff.Sub(*vmMemory.Guest)
			}
			if limits > guest {
				limMemDiff.Add(*renderer.vmLimits.Memory())
				limMemDiff.Sub(*vmMemory.Guest)
			}
		}
		// Set requested memory equals to overhead memory
		reqMemDiff.Add(*memoryOverhead)
		renderer.vmRequests[k8sv1.ResourceMemory] = *reqMemDiff
		if _, ok := renderer.vmLimits[k8sv1.ResourceMemory]; ok {
			limMemDiff.Add(*memoryOverhead)
			renderer.vmLimits[k8sv1.ResourceMemory] = *limMemDiff
		}
	}
}

func WithMemoryOverhead(guestResourceSpec v1.ResourceRequirements, memoryOverhead *resource.Quantity) ResourceRendererOption {
	return func(renderer *ResourceRenderer) {
		memoryRequest := renderer.vmRequests[k8sv1.ResourceMemory]
		if !guestResourceSpec.OvercommitGuestOverhead {
			memoryRequest.Add(*memoryOverhead)
		}
		renderer.vmRequests[k8sv1.ResourceMemory] = memoryRequest

		if memoryLimit, ok := renderer.vmLimits[k8sv1.ResourceMemory]; ok {
			memoryLimit.Add(*memoryOverhead)
			renderer.vmLimits[k8sv1.ResourceMemory] = memoryLimit
		}
	}
}

func WithCPUPinning(cpu *v1.CPU) ResourceRendererOption {
	return func(renderer *ResourceRenderer) {
		vcpus := hardware.GetNumberOfVCPUs(cpu)
		if vcpus != 0 {
			renderer.calculatedLimits[k8sv1.ResourceCPU] = *resource.NewQuantity(vcpus, resource.BinarySI)
		} else {
			if cpuLimit, ok := renderer.vmLimits[k8sv1.ResourceCPU]; ok {
				renderer.vmRequests[k8sv1.ResourceCPU] = cpuLimit
			} else if cpuRequest, ok := renderer.vmRequests[k8sv1.ResourceCPU]; ok {
				renderer.vmLimits[k8sv1.ResourceCPU] = cpuRequest
			}
		}

		// allocate 1 more pcpu if IsolateEmulatorThread request
		if cpu.IsolateEmulatorThread {
			emulatorThreadCPU := resource.NewQuantity(1, resource.BinarySI)
			limits := renderer.calculatedLimits[k8sv1.ResourceCPU]
			limits.Add(*emulatorThreadCPU)
			renderer.calculatedLimits[k8sv1.ResourceCPU] = limits
			if cpuRequest, ok := renderer.vmRequests[k8sv1.ResourceCPU]; ok {
				cpuRequest.Add(*emulatorThreadCPU)
				renderer.vmRequests[k8sv1.ResourceCPU] = cpuRequest
			}
		}

		renderer.vmLimits[k8sv1.ResourceMemory] = *renderer.vmRequests.Memory()
	}
}

func WithNetworkResources(networkToResourceMap map[string]string) ResourceRendererOption {
	return func(renderer *ResourceRenderer) {
		resources := renderer.ResourceRequirements()
		for _, resourceName := range networkToResourceMap {
			if resourceName != "" {
				requestResource(&resources, resourceName)
			}
		}
		copyResources(resources.Limits, renderer.calculatedLimits)
		copyResources(resources.Requests, renderer.calculatedRequests)
	}
}

func WithGPUs(gpus []v1.GPU) ResourceRendererOption {
	return func(renderer *ResourceRenderer) {
		resources := renderer.ResourceRequirements()
		for _, gpu := range gpus {
			requestResource(&resources, gpu.DeviceName)
		}
		copyResources(resources.Limits, renderer.calculatedLimits)
		copyResources(resources.Requests, renderer.calculatedRequests)
	}
}

func WithHostDevices(hostDevices []v1.HostDevice) ResourceRendererOption {
	return func(renderer *ResourceRenderer) {
		resources := renderer.ResourceRequirements()
		for _, hostDev := range hostDevices {
			requestResource(&resources, hostDev.DeviceName)
		}
		copyResources(resources.Limits, renderer.calculatedLimits)
		copyResources(resources.Requests, renderer.calculatedRequests)
	}
}

func WithSEV() ResourceRendererOption {
	return func(renderer *ResourceRenderer) {
		resources := renderer.ResourceRequirements()
		requestResource(&resources, SevDevice)
		copyResources(resources.Limits, renderer.calculatedLimits)
		copyResources(resources.Requests, renderer.calculatedRequests)
	}
}

func copyResources(srcResources, dstResources k8sv1.ResourceList) {
	for key, value := range srcResources {
		dstResources[key] = value
	}
}

// GetMemoryOverhead computes the estimation of total
// memory needed for the domain to operate properly.
// This includes the memory needed for the guest and memory
// for Qemu and OS overhead.
// The return value is overhead memory quantity
//
// Note: This is the best estimation we were able to come up with
//
//	and is still not 100% accurate
func GetMemoryOverhead(vmi *v1.VirtualMachineInstance, cpuArch string) *resource.Quantity {
	domain := vmi.Spec.Domain
	vmiMemoryReq := domain.Resources.Requests.Memory()

	overhead := resource.NewScaledQuantity(0, resource.Kilo)

	// Add the memory needed for pagetables (one bit for every 512b of RAM size)
	pagetableMemory := resource.NewScaledQuantity(vmiMemoryReq.ScaledValue(resource.Kilo), resource.Kilo)
	pagetableMemory.Set(pagetableMemory.Value() / 512)
	overhead.Add(*pagetableMemory)

	// Add fixed overhead for KubeVirt components, as seen in a random run, rounded up to the nearest MiB
	// Note: shared libraries are included in the size, so every library is counted (wrongly) as many times as there are
	//   processes using it. However, the extra memory is only in the order of 10MiB and makes for a nice safety margin.
	overhead.Add(resource.MustParse(VirtLauncherMonitorOverhead))
	overhead.Add(resource.MustParse(VirtLauncherOverhead))
	overhead.Add(resource.MustParse(VirtlogdOverhead))
	overhead.Add(resource.MustParse(LibvirtdOverhead))
	overhead.Add(resource.MustParse(QemuOverhead))

	// Add CPU table overhead (8 MiB per vCPU and 8 MiB per IO thread)
	// overhead per vcpu in MiB
	coresMemory := resource.MustParse("8Mi")
	var vcpus int64
	if domain.CPU != nil {
		vcpus = hardware.GetNumberOfVCPUs(domain.CPU)
	} else {
		// Currently, a default guest CPU topology is set by the API webhook mutator, if not set by a user.
		// However, this wasn't always the case.
		// In case when the guest topology isn't set, take value from resources request or limits.
		resources := vmi.Spec.Domain.Resources
		if cpuLimit, ok := resources.Limits[k8sv1.ResourceCPU]; ok {
			vcpus = cpuLimit.Value()
		} else if cpuRequests, ok := resources.Requests[k8sv1.ResourceCPU]; ok {
			vcpus = cpuRequests.Value()
		}
	}

	// if neither CPU topology nor request or limits provided, set vcpus to 1
	if vcpus < 1 {
		vcpus = 1
	}
	value := coresMemory.Value() * vcpus
	coresMemory = *resource.NewQuantity(value, coresMemory.Format)
	overhead.Add(coresMemory)

	// static overhead for IOThread
	overhead.Add(resource.MustParse("8Mi"))

	// Add video RAM overhead
	if domain.Devices.AutoattachGraphicsDevice == nil || *domain.Devices.AutoattachGraphicsDevice == true {
		overhead.Add(resource.MustParse("16Mi"))
	}

	// When use uefi boot on aarch64 with edk2 package, qemu will create 2 pflash(64Mi each, 128Mi in total)
	// it should be considered for memory overhead
	// Additional information can be found here: https://github.com/qemu/qemu/blob/master/hw/arm/virt.c#L120
	if cpuArch == "arm64" {
		overhead.Add(resource.MustParse("128Mi"))
	}

	// Additional overhead of 1G for VFIO devices. VFIO requires all guest RAM to be locked
	// in addition to MMIO memory space to allow DMA. 1G is often the size of reserved MMIO space on x86 systems.
	// Additial information can be found here: https://www.redhat.com/archives/libvir-list/2015-November/msg00329.html
	if util.IsVFIOVMI(vmi) {
		overhead.Add(resource.MustParse("1Gi"))
	}

	// DownardMetrics volumes are using emptyDirs backed by memory.
	// the max. disk size is only 256Ki.
	if downwardmetrics.HasDownwardMetricDisk(vmi) {
		overhead.Add(resource.MustParse("1Mi"))
	}

	addProbeOverheads(vmi, overhead)

	// Consider memory overhead for SEV guests.
	// Additional information can be found here: https://libvirt.org/kbase/launch_security_sev.html#memory
	if util.IsSEVVMI(vmi) {
		overhead.Add(resource.MustParse("256Mi"))
	}

	// Having a TPM device will spawn a swtpm process
	// In `ps`, swtpm has VSZ of 53808 and RSS of 3496, so 53Mi should do
	if vmi.Spec.Domain.Devices.TPM != nil {
		overhead.Add(resource.MustParse("53Mi"))
	}

	return overhead
}

// Request a resource by name. This function bumps the number of resources,
// both its limits and requests attributes.
//
// If we were operating with a regular resource (CPU, memory, network
// bandwidth), we would need to take care of QoS. For example,
// https://kubernetes.io/docs/tasks/configure-pod-container/quality-service-pod/#create-a-pod-that-gets-assigned-a-qos-class-of-guaranteed
// explains that when Limits are set but Requests are not then scheduler
// assumes that Requests are the same as Limits for a particular resource.
//
// But this function is not called for this standard resources but for
// resources managed by device plugins. The device plugin design document says
// the following on the matter:
// https://github.com/kubernetes/community/blob/master/contributors/design-proposals/resource-management/device-plugin.md#end-user-story
//
// ```
// Devices can be selected using the same process as for OIRs in the pod spec.
// Devices have no impact on QOS. However, for the alpha, we expect the request
// to have limits == requests.
// ```
//
// Which suggests that, for resources managed by device plugins, 1) limits
// should be equal to requests; and 2) QoS rules do not apVFIO//
// Hence we don't copy Limits value to Requests if the latter is missing.
func requestResource(resources *k8sv1.ResourceRequirements, resourceName string) {
	name := k8sv1.ResourceName(resourceName)
	bumpResources(resources.Limits, name)
	bumpResources(resources.Requests, name)
}

func bumpResources(resources k8sv1.ResourceList, name k8sv1.ResourceName) {
	unitQuantity := *resource.NewQuantity(1, resource.DecimalSI)

	val, ok := resources[name]
	if ok {
		val.Add(unitQuantity)
		resources[name] = val
	} else {
		resources[name] = unitQuantity
	}
}

func calcVCPUs(cpu *v1.CPU) int64 {
	if cpu != nil {
		return hardware.GetNumberOfVCPUs(cpu)
	}
	return int64(1)
}

func getRequiredResources(vmi *v1.VirtualMachineInstance, allowEmulation bool) k8sv1.ResourceList {
	res := k8sv1.ResourceList{}
	if util.NeedTunDevice(vmi) {
		res[TunDevice] = resource.MustParse("1")
	}
	if util.NeedVirtioNetDevice(vmi, allowEmulation) {
		// Note that about network interface, allowEmulation does not make
		// any difference on eventual Domain xml, but uniformly making
		// /dev/vhost-net unavailable and libvirt implicitly fallback
		// to use QEMU userland NIC emulation.
		res[VhostNetDevice] = resource.MustParse("1")
	}
	if !allowEmulation {
		res[KvmDevice] = resource.MustParse("1")
	}
	if util.IsAutoAttachVSOCK(vmi) {
		res[VhostVsockDevice] = resource.MustParse("1")
	}
	return res
}

func WithVirtualizationResources(virtResources k8sv1.ResourceList) ResourceRendererOption {
	return func(renderer *ResourceRenderer) {
		copyResources(virtResources, renderer.vmLimits)
	}
}

func getNetworkToResourceMap(virtClient kubecli.KubevirtClient, vmi *v1.VirtualMachineInstance) (networkToResourceMap map[string]string, err error) {
	networkToResourceMap = make(map[string]string)
	for _, network := range vmi.Spec.Networks {
		if network.Multus != nil {
			namespace, networkName := getNamespaceAndNetworkName(vmi, network.Multus.NetworkName)
			crd, err := virtClient.NetworkClient().K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespace).Get(context.Background(), networkName, metav1.GetOptions{})
			if err != nil {
				return map[string]string{}, fmt.Errorf("Failed to locate network attachment definition %s/%s", namespace, networkName)
			}
			networkToResourceMap[network.Name] = getResourceNameForNetwork(crd)
		}
	}
	return
}

func validatePermittedHostDevices(spec *v1.VirtualMachineInstanceSpec, config *virtconfig.ClusterConfig) error {
	errors := make([]string, 0)

	if hostDevs := config.GetPermittedHostDevices(); hostDevs != nil {
		// build a map of all permitted host devices
		supportedHostDevicesMap := make(map[string]bool)
		for _, dev := range hostDevs.PciHostDevices {
			supportedHostDevicesMap[dev.ResourceName] = true
		}
		for _, dev := range hostDevs.MediatedDevices {
			supportedHostDevicesMap[dev.ResourceName] = true
		}
		for _, hostDev := range spec.Domain.Devices.GPUs {
			if _, exist := supportedHostDevicesMap[hostDev.DeviceName]; !exist {
				errors = append(errors, fmt.Sprintf("GPU %s is not permitted in permittedHostDevices configuration", hostDev.DeviceName))
			}
		}
		for _, hostDev := range spec.Domain.Devices.HostDevices {
			if _, exist := supportedHostDevicesMap[hostDev.DeviceName]; !exist {
				errors = append(errors, fmt.Sprintf("HostDevice %s is not permitted in permittedHostDevices configuration", hostDev.DeviceName))
			}
		}
	}

	if len(errors) != 0 {
		return fmt.Errorf(strings.Join(errors, " "))
	}

	return nil
}

func sidecarResources(vmi *v1.VirtualMachineInstance) k8sv1.ResourceRequirements {
	resources := k8sv1.ResourceRequirements{}
	// add default cpu and memory limits to enable cpu pinning if requested
	// TODO(vladikr): make the hookSidecar express resources
	if vmi.IsCPUDedicated() || vmi.WantsToHaveQOSGuaranteed() {
		resources.Limits = make(k8sv1.ResourceList)
		resources.Limits[k8sv1.ResourceCPU] = resource.MustParse("200m")
		resources.Limits[k8sv1.ResourceMemory] = resource.MustParse("64M")
	}
	return resources
}

func initContainerResourceRequirementsForVMI(vmi *v1.VirtualMachineInstance) k8sv1.ResourceRequirements {
	if vmi.IsCPUDedicated() || vmi.WantsToHaveQOSGuaranteed() {
		return k8sv1.ResourceRequirements{
			Limits:   initContainerDedicatedCPURequiredResources(),
			Requests: initContainerDedicatedCPURequiredResources(),
		}
	} else {
		return k8sv1.ResourceRequirements{
			Limits:   initContainerMinimalLimits(),
			Requests: initContainerMinimalRequests(),
		}
	}
}

func initContainerDedicatedCPURequiredResources() k8sv1.ResourceList {
	return k8sv1.ResourceList{
		k8sv1.ResourceCPU:    resource.MustParse("10m"),
		k8sv1.ResourceMemory: resource.MustParse("40M"),
	}
}

func initContainerMinimalLimits() k8sv1.ResourceList {
	return k8sv1.ResourceList{
		k8sv1.ResourceCPU:    resource.MustParse("100m"),
		k8sv1.ResourceMemory: resource.MustParse("40M"),
	}
}

func initContainerMinimalRequests() k8sv1.ResourceList {
	return k8sv1.ResourceList{
		k8sv1.ResourceCPU:    resource.MustParse("10m"),
		k8sv1.ResourceMemory: resource.MustParse("1M"),
	}
}
