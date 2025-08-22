/*
Copyright 2025 The CloudPilot AI Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package instancetype

import (
	"context"
	"fmt"
	"strings"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/operator/options"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
)

func NewInstanceType(ctx context.Context, mt *computepb.MachineType,
	region string, offerings cloudprovider.Offerings, nodeClass *v1alpha1.GCENodeClass) *cloudprovider.InstanceType {
	if offerings == nil {
		return nil
	}

	reservedCPU, reservedMemory, evictionMemory := utils.ResolveReservedResource(aws.StringValue(mt.Name), int64(mt.GetGuestCpus()*1000), int64(mt.GetMemoryMb()))
	overhead := cloudprovider.InstanceTypeOverhead{
		KubeReserved: corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewMilliQuantity(reservedCPU, resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(reservedMemory*1024*1024, resource.BinarySI),
		},
		EvictionThreshold: corev1.ResourceList{
			corev1.ResourceMemory: *resource.NewQuantity(evictionMemory*1024*1024, resource.BinarySI),
		},
		SystemReserved: corev1.ResourceList{},
	}

	it := &cloudprovider.InstanceType{
		Name:         aws.StringValue(mt.Name),
		Requirements: computeRequirements(mt, offerings, region),
		Offerings:    offerings,
		Capacity:     computeCapacity(ctx, mt, nodeClass),
		Overhead:     &overhead,
	}

	return it
}

func extractCategory(part string) string {
	i := 0
	for ; i < len(part); i++ {
		if part[i] >= '0' && part[i] <= '9' {
			break
		}
	}
	return part[:i]
}

//nolint:gocyclo
func computeRequirements(mt *computepb.MachineType, offerings cloudprovider.Offerings, region string) scheduling.Requirements {
	requirements := scheduling.NewRequirements(
		// Well Known Upstream
		scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, aws.StringValue(mt.Name)),
		scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpDoesNotExist),
		scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, string(corev1.Linux)),
		scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, lo.Map(offerings.Available(), func(o *cloudprovider.Offering, _ int) string {
			return o.Requirements.Get(corev1.LabelTopologyZone).Any()
		})...),
		scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, region),
		scheduling.NewRequirement(corev1.LabelWindowsBuild, corev1.NodeSelectorOpDoesNotExist),

		// Well Known to Karpenter
		scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, lo.Map(offerings.Available(), func(o *cloudprovider.Offering, _ int) string {
			return o.Requirements.Get(karpv1.CapacityTypeLabelKey).Any()
		})...),

		// Well Known to Google Cloud
		scheduling.NewRequirement(v1alpha1.LabelInstanceCPU, corev1.NodeSelectorOpIn, fmt.Sprintf("%d", mt.GetGuestCpus())),
		scheduling.NewRequirement(v1alpha1.LabelInstanceCPUModel, corev1.NodeSelectorOpDoesNotExist),
		scheduling.NewRequirement(v1alpha1.LabelInstanceMemory, corev1.NodeSelectorOpIn, fmt.Sprintf("%d", mt.GetMemoryMb())),
		scheduling.NewRequirement(v1alpha1.LabelInstanceCategory, corev1.NodeSelectorOpDoesNotExist),
		scheduling.NewRequirement(v1alpha1.LabelInstanceFamily, corev1.NodeSelectorOpDoesNotExist),
		scheduling.NewRequirement(v1alpha1.LabelInstanceGeneration, corev1.NodeSelectorOpDoesNotExist),
		scheduling.NewRequirement(v1alpha1.LabelInstanceSize, corev1.NodeSelectorOpDoesNotExist),
		scheduling.NewRequirement(v1alpha1.LabelInstanceGPUName, corev1.NodeSelectorOpDoesNotExist),
		scheduling.NewRequirement(v1alpha1.LabelInstanceGPUManufacturer, corev1.NodeSelectorOpDoesNotExist),
		scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, corev1.NodeSelectorOpDoesNotExist),
		scheduling.NewRequirement(v1alpha1.LabelInstanceGPUMemory, corev1.NodeSelectorOpDoesNotExist),
	)
	// Only add zone-id label when available in offerings. It may not be available if a user has upgraded from a
	// previous version of Karpenter w/o zone-id support and the nodeclass vswitch status has not yet updated.
	if zoneIDs := lo.FilterMap(offerings.Available(), func(o *cloudprovider.Offering, _ int) (string, bool) {
		zoneID := o.Requirements.Get(v1alpha1.LabelTopologyZoneID).Any()
		return zoneID, zoneID != ""
	}); len(zoneIDs) != 0 {
		requirements.Add(scheduling.NewRequirement(v1alpha1.LabelTopologyZoneID, corev1.NodeSelectorOpIn, zoneIDs...))
	}

	// GPU labels
	if len(mt.GetAccelerators()) > 0 {
		requirements.Get(v1alpha1.LabelInstanceGPUName).Insert(extractGPUName(mt))
		requirements.Get(v1alpha1.LabelInstanceGPUCount).Insert(fmt.Sprintf("%d", len(mt.GetAccelerators())))
	}

	// The format looks like: n1-standard-1, the family is n1-standard, the category is n, the instance size is 1
	// Also, there is something like e2-medium, the family is e2, the category is e, the instance size is medium
	instanceTypeParts := strings.Split(aws.StringValue(mt.Name), "-")
	if len(instanceTypeParts) >= 2 {
		requirements.Get(v1alpha1.LabelInstanceCategory).Insert(extractCategory(instanceTypeParts[0]))
		requirements.Get(v1alpha1.LabelInstanceSize).Insert(instanceTypeParts[len(instanceTypeParts)-1])
		// The laster number of the first part is the generation
		requirements.Get(v1alpha1.LabelInstanceGeneration).Insert(extractGeneration(instanceTypeParts[0]))

		if len(instanceTypeParts) == 2 {
			requirements.Get(v1alpha1.LabelInstanceFamily).Insert(instanceTypeParts[0])
		}
		// If there are three parts, also insert the first two joined as family
		if len(instanceTypeParts) == 3 {
			requirements.Get(v1alpha1.LabelInstanceFamily).Insert(instanceTypeParts[0] + "-" + instanceTypeParts[1])
		}

		requirements.Get(corev1.LabelArchStable).Insert(extractArch(instanceTypeParts[0]))
	}

	return requirements
}

func extractGPUName(mt *computepb.MachineType) string {
	if len(mt.GetAccelerators()) > 0 {
		return mt.GetAccelerators()[0].GetGuestAcceleratorType()
	}
	return ""
}

func extractGeneration(instanceTypePrefix string) string {
	// The laster number of the first part is the generation
	return string(instanceTypePrefix[len(instanceTypePrefix)-1])
}

func extractArch(instanceTypePrefix string) string {
	// referring to https://cloud.google.com/compute/docs/instances/arm-on-compute
	if instanceTypePrefix == "a4x" || instanceTypePrefix == "c4a" || instanceTypePrefix == "t2a" {
		return "arm64"
	}
	return "amd64"
}

func computeCapacity(ctx context.Context, mt *computepb.MachineType, nodeClass *v1alpha1.GCENodeClass) corev1.ResourceList {
	resourceList := corev1.ResourceList{
		corev1.ResourceCPU:              *cpu(mt),
		corev1.ResourceMemory:           *memory(ctx, mt),
		corev1.ResourceEphemeralStorage: *ephemeralStorage(ctx, mt, nodeClass),
		corev1.ResourcePods:             *maxPods(nodeClass),
	}
	return resourceList
}

func cpu(mt *computepb.MachineType) *resource.Quantity {
	return resource.NewQuantity(int64(mt.GetGuestCpus()), resource.DecimalSI)
}

func memory(ctx context.Context, mt *computepb.MachineType) *resource.Quantity {
	osReservedPercent := options.FromContext(ctx).VMMemoryOverheadPercent
	totalQuantity := int64(mt.GetMemoryMb()) * 1024 * 1024
	return resource.NewQuantity(totalQuantity-int64(float64(totalQuantity)*osReservedPercent), resource.DecimalSI)
}

// ephemeralStorage calculates the available ephemeral storage for the instance type
// This includes boot disk space minus OS and system reserved space
//
// CURRENT SCOPE: Boot disk ephemeral storage only
// - Calculates based on boot disk size (explicit config or 100GB GCP default)
// - Applies unified 50GB system reservation for all OS types
// - Local SSD support is planned for future enhancement
func ephemeralStorage(ctx context.Context, mt *computepb.MachineType, nodeClass *v1alpha1.GCENodeClass) *resource.Quantity {
	// Get boot disk size using layered default strategy
	bootDiskSizeGiB := getBootDiskSize(nodeClass)
	
	// Add Local SSD storage if the instance type supports it
	// TODO: Currently always returns 0 - see getLocalSSDStorage() comments
	instanceTypeName := aws.StringValue(mt.Name)
	localSSDSizeGiB := getLocalSSDStorage(instanceTypeName)
	totalStorageGiB := bootDiskSizeGiB + localSSDSizeGiB
	
	// Calculate system reserved space
	// NOTE: Currently uses unified reservation logic for all OS types, but we still
	// pass nodeClass for potential future OS-specific optimizations if real-world
	// data suggests different reservation needs
	osReservedGiB := calculateSystemReserved(nodeClass, totalStorageGiB)
	
	// Calculate available ephemeral storage
	availableStorageGiB := totalStorageGiB - osReservedGiB
	
	// Ensure we have at least 1GB available
	if availableStorageGiB < 1 {
		availableStorageGiB = 1
	}
	
	// Convert to bytes
	availableStorageBytes := availableStorageGiB * 1024 * 1024 * 1024
	return resource.NewQuantity(availableStorageBytes, resource.BinarySI)
}

// getBootDiskSize returns the boot disk size using a layered default strategy
// Priority: explicit boot disk > first disk > global default (100GB as per GCP specification)
func getBootDiskSize(nodeClass *v1alpha1.GCENodeClass) int64 {
	// Global default boot disk size - matches GCP/GKE official default for all image families
	// Reference: https://cloud.google.com/kubernetes-engine/docs/concepts/node-images
	// "If unspecified, the default disk size is 100GB"
	defaultBootDiskSizeGiB := int64(100)
	
	if nodeClass == nil {
		return defaultBootDiskSizeGiB
	}
	
	// Priority 1: Look for explicitly marked boot disk
	if len(nodeClass.Spec.Disks) > 0 {
		for _, disk := range nodeClass.Spec.Disks {
			if disk.Boot {
				return int64(disk.SizeGiB)
			}
		}
		
		// Priority 2: If no boot disk is explicitly marked, use the first disk
		return int64(nodeClass.Spec.Disks[0].SizeGiB)
	}
	
	// Priority 3: Use GCP's unified default (100GB) for all image families
	// GCP has already sized this to accommodate all supported operating systems
	return defaultBootDiskSizeGiB
}

// getLocalSSDStorage returns the Local SSD storage size in GiB for instance types that support it
// Currently returns 0 (no Local SSD support) to focus on core boot disk functionality.
func getLocalSSDStorage(instanceTypeName string) int64 {
	return 0
}

// calculateSystemReserved calculates the amount of storage to reserve for OS and Kubernetes components
func calculateSystemReserved(nodeClass *v1alpha1.GCENodeClass, totalStorageGiB int64) int64 {
	// Priority 1: Check if user explicitly configured ephemeral-storage reservation
	if nodeClass != nil && nodeClass.Spec.KubeletConfiguration != nil {
		// Check SystemReserved first (OS system daemons and kernel memory)
		if systemReserved, exists := nodeClass.Spec.KubeletConfiguration.SystemReserved["ephemeral-storage"]; exists {
			if reservedGiB, err := parseStorageQuantity(systemReserved); err == nil {
				// Convert to GiB and ensure it doesn't exceed total storage
				if reservedGiB <= totalStorageGiB {
					return reservedGiB
				}
				// If configured value exceeds total, cap it at total storage
				return totalStorageGiB
			}
		}
		
		// Check KubeReserved (Kubernetes system components)
		if kubeReserved, exists := nodeClass.Spec.KubeletConfiguration.KubeReserved["ephemeral-storage"]; exists {
			if reservedGiB, err := parseStorageQuantity(kubeReserved); err == nil {
				// Convert to GiB and ensure it doesn't exceed total storage
				if reservedGiB <= totalStorageGiB {
					return reservedGiB
				}
				// If configured value exceeds total, cap it at total storage
				return totalStorageGiB
			}
		}
	}
	
	// Priority 2: Fall back to unified 50GB reservation strategy
	// This is our default approach when no explicit configuration is provided
	// Based on real GKE observations showing ~52GB reservation on 100GB disks
	defaultReservedGiB := int64(50)
	
	// For very small disks, ensure we don't reserve more than available
	if totalStorageGiB <= defaultReservedGiB {
		// Reserve at most 80% of small disks to ensure some usable space
		reservedGiB := totalStorageGiB * 80 / 100
		// But ensure minimum 5GB reservation for system stability
		if reservedGiB < 5 {
			reservedGiB = 5
		}
		return reservedGiB
	}
	
	return defaultReservedGiB
}

// parseStorageQuantity parses a Kubernetes resource quantity string and returns the value in GiB
// Supports formats like "50Gi", "51200Mi", "100G", etc.
func parseStorageQuantity(quantityStr string) (int64, error) {
	quantity, err := resource.ParseQuantity(quantityStr)
	if err != nil {
		return 0, err
	}
	
	// Convert to bytes first, then to GiB
	bytes := quantity.Value()
	giB := bytes / (1024 * 1024 * 1024)
	
	return giB, nil
}

// maxPods returns the maximum number of pods that can run on the node
// Priority: nodeclass MaxPods configuration > default KubeletMaxPods value
func maxPods(nodeClass *v1alpha1.GCENodeClass) *resource.Quantity {
	// Priority 1: Check if user explicitly configured MaxPods in KubeletConfiguration
	if nodeClass != nil && nodeClass.Spec.KubeletConfiguration != nil && nodeClass.Spec.KubeletConfiguration.MaxPods != nil {
		return resource.NewQuantity(int64(*nodeClass.Spec.KubeletConfiguration.MaxPods), resource.DecimalSI)
	}
	
	// Priority 2: Fall back to default maximum pods value
	// This is the standard maximum pods per node for GCP instances
	return resource.NewQuantity(int64(v1alpha1.KubeletMaxPods), resource.DecimalSI)
}
