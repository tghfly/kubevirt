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
 * Copyright 2017, 2018 Red Hat, Inc.
 *
 */

package virtconfig

/*
 This module is intended for exposing the virtualization configuration that is available at the cluster-level and its default settings.
*/

import (
	"fmt"

	"kubevirt.io/client-go/log"

	k8sv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	v1 "kubevirt.io/api/core/v1"
)

const (
	ParallelOutboundMigrationsPerNodeDefault uint32 = 2
	ParallelMigrationsPerClusterDefault      uint32 = 5
	BandwithPerMigrationDefault                     = "0Mi"
	MigrationAllowAutoConverge               bool   = false
	MigrationAllowPostCopy                   bool   = false
	MigrationProgressTimeout                 int64  = 150
	MigrationCompletionTimeoutPerGiB         int64  = 800
	DefaultAMD64MachineType                         = "q35"
	DefaultPPC64LEMachineType                       = "pseries"
	DefaultAARCH64MachineType                       = "virt"
	DefaultCPURequest                               = "100m"
	DefaultMemoryOvercommit                         = 100
	DefaultAMD64EmulatedMachines                    = "q35*,pc-q35*"
	DefaultPPC64LEEmulatedMachines                  = "pseries*"
	DefaultAARCH64EmulatedMachines                  = "virt*"
	DefaultLessPVCSpaceToleration                   = 10
	DefaultMinimumReservePVCBytes                   = 131072
	DefaultNodeSelectors                            = ""
	DefaultNetworkInterface                         = "bridge"
	DefaultImagePullPolicy                          = k8sv1.PullIfNotPresent
	DefaultAllowEmulation                           = false
	DefaultUnsafeMigrationOverride                  = false
	DefaultPermitSlirpInterface                     = false
	SmbiosConfigDefaultFamily                       = "KubeVirt"
	SmbiosConfigDefaultManufacturer                 = "KubeVirt"
	SmbiosConfigDefaultProduct                      = "None"
	DefaultPermitBridgeInterfaceOnPodNetwork        = true
	DefaultSELinuxLauncherType                      = ""
	SupportedGuestAgentVersions                     = "2.*,3.*,4.*,5.*"
	DefaultARCHOVMFPath                             = "/usr/share/OVMF"
	DefaultAARCH64OVMFPath                          = "/usr/share/AAVMF"
	DefaultMemBalloonStatsPeriod             uint32 = 10
	DefaultCPUAllocationRatio                       = 10
	DefaultDiskVerificationMemoryLimitMBytes        = 1700
	DefaultVirtAPILogVerbosity                      = 2
	DefaultVirtControllerLogVerbosity               = 2
	DefaultVirtHandlerLogVerbosity                  = 2
	DefaultVirtLauncherLogVerbosity                 = 2
	DefaultVirtOperatorLogVerbosity                 = 2

	// Default REST configuration settings
	DefaultVirtHandlerQPS         float32 = 5
	DefaultVirtHandlerBurst               = 10
	DefaultVirtControllerQPS      float32 = 200
	DefaultVirtControllerBurst            = 400
	DefaultVirtAPIQPS             float32 = 5
	DefaultVirtAPIBurst                   = 10
	DefaultVirtWebhookClientQPS           = 200
	DefaultVirtWebhookClientBurst         = 400
)

func IsAMD64(arch string) bool {
	if arch == "amd64" {
		return true
	}
	return false
}

func IsARM64(arch string) bool {
	if arch == "arm64" {
		return true
	}
	return false
}

func IsPPC64(arch string) bool {
	if arch == "ppc64le" {
		return true
	}
	return false
}

func (c *ClusterConfig) GetMemBalloonStatsPeriod() uint32 {
	return *c.GetConfig().MemBalloonStatsPeriod
}

func (c *ClusterConfig) AllowEmulation() bool {
	return c.GetConfig().DeveloperConfiguration.UseEmulation
}

func (c *ClusterConfig) GetMigrationConfiguration() *v1.MigrationConfiguration {
	return c.GetConfig().MigrationConfiguration
}

func (c *ClusterConfig) GetImagePullPolicy() (policy k8sv1.PullPolicy) {
	return c.GetConfig().ImagePullPolicy
}

func (c *ClusterConfig) GetResourceVersion() string {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.lastValidConfigResourceVersion
}

func (c *ClusterConfig) GetMachineType() string {
	return c.GetConfig().MachineType
}

func (c *ClusterConfig) GetCPUModel() string {
	return c.GetConfig().CPUModel
}

func (c *ClusterConfig) GetCPURequest() *resource.Quantity {
	return c.GetConfig().CPURequest
}

func (c *ClusterConfig) GetDiskVerification() *v1.DiskVerification {
	return c.GetConfig().DeveloperConfiguration.DiskVerification
}

func (c *ClusterConfig) GetMemoryOvercommit() int {
	return c.GetConfig().DeveloperConfiguration.MemoryOvercommit
}

func (c *ClusterConfig) GetEmulatedMachines() []string {
	return c.GetConfig().EmulatedMachines
}

func (c *ClusterConfig) GetLessPVCSpaceToleration() int {
	return c.GetConfig().DeveloperConfiguration.LessPVCSpaceToleration
}

func (c *ClusterConfig) GetMinimumReservePVCBytes() uint64 {
	return c.GetConfig().DeveloperConfiguration.MinimumReservePVCBytes
}

func (c *ClusterConfig) GetNodeSelectors() map[string]string {
	return c.GetConfig().DeveloperConfiguration.NodeSelectors
}

func (c *ClusterConfig) GetDefaultNetworkInterface() string {
	return c.GetConfig().NetworkConfiguration.NetworkInterface
}

func (c *ClusterConfig) SetVMIDefaultNetworkInterface(vmi *v1.VirtualMachineInstance) error {
	autoAttach := vmi.Spec.Domain.Devices.AutoattachPodInterface
	if autoAttach != nil && *autoAttach == false {
		return nil
	}

	// Override only when nothing is specified
	if len(vmi.Spec.Networks) == 0 && len(vmi.Spec.Domain.Devices.Interfaces) == 0 {
		iface := v1.NetworkInterfaceType(c.GetDefaultNetworkInterface())
		switch iface {
		case v1.BridgeInterface:
			if !c.IsBridgeInterfaceOnPodNetworkEnabled() {
				return fmt.Errorf("Bridge interface is not enabled in kubevirt-config")
			}
			vmi.Spec.Domain.Devices.Interfaces = []v1.Interface{*v1.DefaultBridgeNetworkInterface()}
		case v1.MasqueradeInterface:
			vmi.Spec.Domain.Devices.Interfaces = []v1.Interface{*v1.DefaultMasqueradeNetworkInterface()}
		case v1.SlirpInterface:
			if !c.IsSlirpInterfaceEnabled() {
				return fmt.Errorf("Slirp interface is not enabled in kubevirt-config")
			}
			defaultIface := v1.DefaultSlirpNetworkInterface()
			vmi.Spec.Domain.Devices.Interfaces = []v1.Interface{*defaultIface}
		}

		vmi.Spec.Networks = []v1.Network{*v1.DefaultPodNetwork()}
	}
	return nil
}

func (c *ClusterConfig) IsSlirpInterfaceEnabled() bool {
	return *c.GetConfig().NetworkConfiguration.PermitSlirpInterface
}

func (c *ClusterConfig) GetSMBIOS() *v1.SMBiosConfiguration {
	return c.GetConfig().SMBIOSConfig
}

func (c *ClusterConfig) IsBridgeInterfaceOnPodNetworkEnabled() bool {
	return *c.GetConfig().NetworkConfiguration.PermitBridgeInterfaceOnPodNetwork
}

func (c *ClusterConfig) GetDefaultClusterConfig() *v1.KubeVirtConfiguration {
	return c.defaultConfig
}

func (c *ClusterConfig) GetSELinuxLauncherType() string {
	return c.GetConfig().SELinuxLauncherType
}

func (c *ClusterConfig) GetDefaultRuntimeClass() string {
	return c.GetConfig().DefaultRuntimeClass
}

func (c *ClusterConfig) GetSupportedAgentVersions() []string {
	return c.GetConfig().SupportedGuestAgentVersions
}

func (c *ClusterConfig) GetOVMFPath() string {
	return c.GetConfig().OVMFPath
}

func (c *ClusterConfig) GetCPUAllocationRatio() int {
	return c.GetConfig().DeveloperConfiguration.CPUAllocationRatio
}

func (c *ClusterConfig) GetMinimumClusterTSCFrequency() *int64 {
	return c.GetConfig().DeveloperConfiguration.MinimumClusterTSCFrequency
}

func (c *ClusterConfig) GetPermittedHostDevices() *v1.PermittedHostDevices {
	return c.GetConfig().PermittedHostDevices
}

func canSelectNode(nodeSelector map[string]string, node *k8sv1.Node) bool {
	for key, val := range nodeSelector {
		labelValue, exist := node.Labels[key]
		if !exist || val != labelValue {
			return false
		}
	}
	return true
}

func (c *ClusterConfig) GetDesiredMDEVTypes(node *k8sv1.Node) []string {
	mdevTypesConf := c.GetConfig().MediatedDevicesConfiguration
	if mdevTypesConf == nil {
		return []string{}
	}
	nodeMdevConf := mdevTypesConf.NodeMediatedDeviceTypes
	if nodeMdevConf != nil {
		mdevTypesMap := make(map[string]struct{})
		for _, nodeConfig := range nodeMdevConf {
			if canSelectNode(nodeConfig.NodeSelector, node) {
				for _, mdevType := range nodeConfig.MediatedDevicesTypes {
					mdevTypesMap[mdevType] = struct{}{}
				}
			}
		}
		if len(mdevTypesMap) != 0 {
			mdevTypesList := []string{}
			for mdevType, _ := range mdevTypesMap {
				mdevTypesList = append(mdevTypesList, mdevType)
			}
			return mdevTypesList
		}
	}
	return mdevTypesConf.MediatedDevicesTypes
}

type virtComponent int

const (
	virtHandler virtComponent = iota
	virtApi
	virtController
	virtOperator
	virtLauncher
)

// Gets the component verbosity. nodeName can be empty, then it's ignored.
func (c *ClusterConfig) getComponentVerbosity(component virtComponent, nodeName string) uint {
	logConf := c.GetConfig().DeveloperConfiguration.LogVerbosity

	if nodeName != "" {
		if level := logConf.NodeVerbosity[nodeName]; level != 0 {
			return level
		}
	}

	switch component {
	case virtHandler:
		return logConf.VirtHandler
	case virtApi:
		return logConf.VirtAPI
	case virtController:
		return logConf.VirtController
	case virtOperator:
		return logConf.VirtOperator
	case virtLauncher:
		return logConf.VirtLauncher
	default:
		log.Log.Errorf("getComponentVerbosity called with an unknown virtComponent: %v", component)
		return 0
	}
}

func (c *ClusterConfig) GetVirtHandlerVerbosity(nodeName string) uint {
	return c.getComponentVerbosity(virtHandler, nodeName)
}

func (c *ClusterConfig) GetVirtAPIVerbosity(nodeName string) uint {
	return c.getComponentVerbosity(virtApi, nodeName)
}

func (c *ClusterConfig) GetVirtControllerVerbosity(nodeName string) uint {
	return c.getComponentVerbosity(virtController, nodeName)
}

func (c *ClusterConfig) GetVirtOperatorVerbosity(nodeName string) uint {
	return c.getComponentVerbosity(virtOperator, nodeName)
}

func (c *ClusterConfig) GetVirtLauncherVerbosity() uint {
	return c.getComponentVerbosity(virtLauncher, "")
}

// GetMinCPUModel return minimal cpu which is used in node-labeller
func (c *ClusterConfig) GetMinCPUModel() string {
	return c.GetConfig().MinCPUModel
}

// GetObsoleteCPUModels return slice of obsolete cpus which are used in node-labeller
func (c *ClusterConfig) GetObsoleteCPUModels() map[string]bool {
	return c.GetConfig().ObsoleteCPUModels
}

// GetClusterCPUArch return the CPU architecture in ClusterConfig
func (c *ClusterConfig) GetClusterCPUArch() string {
	return c.cpuArch
}
