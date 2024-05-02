/*
Copyright 2020 The Kubernetes Authors.

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

package azuremodel

import (
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	compute "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute"
	"k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/pkg/model"
	"k8s.io/kops/pkg/model/defaults"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/azure"
	"k8s.io/kops/upup/pkg/fi/cloudup/azuretasks"
)

// VMScaleSetModelBuilder configures VMScaleSet objects
type VMScaleSetModelBuilder struct {
	*AzureModelContext
	BootstrapScriptBuilder *model.BootstrapScriptBuilder
	Lifecycle              fi.Lifecycle
}

var _ fi.CloudupModelBuilder = &VMScaleSetModelBuilder{}

// Build is responsible for constructing the VM ScaleSet from the kops spec.
func (b *VMScaleSetModelBuilder) Build(c *fi.CloudupModelBuilderContext) error {
	c.AddTask(&azuretasks.ApplicationSecurityGroup{
		Name:          fi.PtrTo(b.NameForApplicationSecurityGroupControlPlane()),
		Lifecycle:     b.Lifecycle,
		ResourceGroup: b.LinkToResourceGroup(),
		Tags:          map[string]*string{},
	})
	c.AddTask(&azuretasks.ApplicationSecurityGroup{
		Name:          fi.PtrTo(b.NameForApplicationSecurityGroupNodes()),
		Lifecycle:     b.Lifecycle,
		ResourceGroup: b.LinkToResourceGroup(),
		Tags:          map[string]*string{},
	})

	for _, ig := range b.InstanceGroups {
		name := b.AutoscalingGroupName(ig)
		vmss, err := b.buildVMScaleSetTask(c, name, ig)
		if err != nil {
			return err
		}
		c.AddTask(vmss)

		if ig.IsControlPlane() || b.Cluster.UsesLegacyGossip() {
			// Create tasks for assigning built-in roles to VM Scale Sets.
			// See https://docs.microsoft.com/en-us/azure/role-based-access-control/built-in-roles
			// for the ID definitions.
			roleDefIDs := map[string]string{
				// Owner
				"owner": "8e3af657-a8ff-443c-a75c-2fe8c4bcb635",
				// Storage Blob Data Contributor
				"blob": "ba92f5b4-2d11-453d-a403-e96b0029c9fe",
			}
			for k, roleDefID := range roleDefIDs {
				c.AddTask(b.buildRoleAssignmentTask(vmss, k, roleDefID))
			}
		}
	}

	return nil
}

func (b *VMScaleSetModelBuilder) buildVMScaleSetTask(
	c *fi.CloudupModelBuilderContext,
	name string,
	ig *kops.InstanceGroup,
) (*azuretasks.VMScaleSet, error) {
	var azNumbers []*string
	for _, zone := range ig.Spec.Zones {
		az, err := azure.ZoneToAvailabilityZoneNumber(zone)
		if err != nil {
			return nil, err
		}
		azNumbers = append(azNumbers, &az)
	}
	t := &azuretasks.VMScaleSet{
		Name:               fi.PtrTo(name),
		Lifecycle:          b.Lifecycle,
		ResourceGroup:      b.LinkToResourceGroup(),
		VirtualNetwork:     b.LinkToVirtualNetwork(),
		SKUName:            fi.PtrTo(ig.Spec.MachineType),
		ComputerNamePrefix: fi.PtrTo(ig.Name),
		AdminUser:          fi.PtrTo(b.Cluster.Spec.CloudProvider.Azure.AdminUser),
		Zones:              azNumbers,
	}

	switch ig.Spec.Role {
	case kops.InstanceGroupRoleControlPlane:
		t.ApplicationSecurityGroups = append(t.ApplicationSecurityGroups, b.LinkToApplicationSecurityGroupControlPlane())
	case kops.InstanceGroupRoleNode:
		t.ApplicationSecurityGroups = append(t.ApplicationSecurityGroups, b.LinkToApplicationSecurityGroupNodes())
	default:
		return nil, fmt.Errorf("unexpected instance group role for instance group: %q, %q", ig.Name, ig.Spec.Role)
	}

	var err error
	if t.Capacity, err = getCapacity(&ig.Spec); err != nil {
		return nil, err
	}

	sp, err := getStorageProfile(&ig.Spec)
	if err != nil {
		return nil, err
	}
	t.StorageProfile = &azuretasks.VMScaleSetStorageProfile{
		VirtualMachineScaleSetStorageProfile: sp,
	}

	if n := len(b.SSHPublicKeys); n > 0 {
		if n > 1 {
			return nil, fmt.Errorf("expected at most one SSH public key; found %d keys", n)
		}
		t.SSHPublicKey = fi.PtrTo(string(b.SSHPublicKeys[0]))
	}

	if t.UserData, err = b.BootstrapScriptBuilder.ResourceNodeUp(c, ig); err != nil {
		return nil, err
	}

	subnets, err := b.GatherSubnets(ig)
	if err != nil {
		return nil, err
	}
	if len(subnets) != 1 {
		return nil, fmt.Errorf("expected exactly one subnet for InstanceGroup %q; subnets was %s", ig.Name, ig.Spec.Subnets)
	}
	subnet := subnets[0]
	t.Subnet = b.LinkToAzureSubnet(subnet)

	switch subnet.Type {
	case kops.SubnetTypePublic, kops.SubnetTypeUtility:
		t.RequirePublicIP = fi.PtrTo(true)
		if ig.Spec.AssociatePublicIP != nil {
			t.RequirePublicIP = ig.Spec.AssociatePublicIP
		}
	case kops.SubnetTypeDualStack, kops.SubnetTypePrivate:
		t.RequirePublicIP = fi.PtrTo(false)
	default:
		return nil, fmt.Errorf("unexpected subnet type: for InstanceGroup %q; type was %s", ig.Name, subnet.Type)
	}

	if ig.Spec.Role == kops.InstanceGroupRoleControlPlane && b.Cluster.Spec.API.LoadBalancer != nil {
		t.LoadBalancer = &azuretasks.LoadBalancer{
			Name: to.Ptr(b.NameForLoadBalancer()),
		}
	}

	t.Tags = b.CloudTagsForInstanceGroup(ig)

	return t, nil
}

func getCapacity(spec *kops.InstanceGroupSpec) (*int64, error) {
	// Follow the convention that all other CSPs have.
	minSize := int32(1)
	maxSize := int32(1)
	if spec.MinSize != nil {
		minSize = fi.ValueOf(spec.MinSize)
	} else if spec.Role == kops.InstanceGroupRoleNode {
		minSize = 2
	}
	if spec.MaxSize != nil {
		maxSize = *spec.MaxSize
	} else if spec.Role == kops.InstanceGroupRoleNode {
		maxSize = 2
	}
	if minSize != maxSize {
		return nil, fmt.Errorf("instance group must have the same min and max size in Azure, but got %d and %d", minSize, maxSize)
	}
	return fi.PtrTo(int64(minSize)), nil
}

func getStorageProfile(spec *kops.InstanceGroupSpec) (*compute.VirtualMachineScaleSetStorageProfile, error) {
	var volumeSize int32
	if spec.RootVolume != nil && spec.RootVolume.Size != nil {
		volumeSize = *spec.RootVolume.Size
	} else {
		var err error
		volumeSize, err = defaults.DefaultInstanceGroupVolumeSize(spec.Role)
		if err != nil {
			return nil, err
		}
	}

	imageReference, err := parseImage(spec.Image)
	if err != nil {
		return nil, err
	}

	// Hack
	if spec.RootVolume != nil && spec.RootVolume.Type != nil {
		if *spec.RootVolume.Type == "EphemeralOnOSCache" {
			return &compute.VirtualMachineScaleSetStorageProfile{
				ImageReference:     imageReference,
				OSDisk: &compute.VirtualMachineScaleSetOSDisk{
					OSType:       to.Ptr(compute.OperatingSystemTypesLinux),
					CreateOption: to.Ptr(compute.DiskCreateOptionTypesFromImage),
					DiskSizeGB:   to.Ptr(volumeSize),
					DiffDiskSettings: &compute.DiffDiskSettings{
						Option: to.Ptr(compute.DiffDiskOptionsLocal),
						Placement: to.Ptr(compute.DiffDiskPlacementCacheDisk),
					},
					// With terraform, caching must be read only when using Ephemeral Disk
					Caching: to.Ptr(compute.CachingTypesReadOnly),
				},
			}, nil
		}
	}

	return nil, fmt.Errorf("Root Volume type of %s does not exist", spec.RootVolume.Type)
}

func parseImage(image string) (*compute.ImageReference, error) {
	if strings.HasPrefix(image, "/subscriptions/") {
		return &compute.ImageReference{
			ID: to.Ptr(image),
		}, nil
	}

	l := strings.Split(image, ":")
	if len(l) != 4 {
		return nil, fmt.Errorf("malformed format of image urn: %s", image)
	}
	return &compute.ImageReference{
		Publisher: to.Ptr(l[0]),
		Offer:     to.Ptr(l[1]),
		SKU:       to.Ptr(l[2]),
		Version:   to.Ptr(l[3]),
	}, nil
}

func (b *VMScaleSetModelBuilder) buildRoleAssignmentTask(vmss *azuretasks.VMScaleSet, roleKey, roleDefID string) *azuretasks.RoleAssignment {
	name := fmt.Sprintf("%s-%s", *vmss.Name, roleKey)
	return &azuretasks.RoleAssignment{
		Name:          to.Ptr(name),
		Lifecycle:     b.Lifecycle,
		ResourceGroup: b.LinkToResourceGroup(),
		VMScaleSet:    vmss,
		RoleDefID:     to.Ptr(roleDefID),
	}
}
