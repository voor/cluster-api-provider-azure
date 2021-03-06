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

package managedclusters

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/Azure/azure-sdk-for-go/services/containerservice/mgmt/2020-02-01/containerservice"
	"github.com/pkg/errors"
	"k8s.io/klog"
	azure "sigs.k8s.io/cluster-api-provider-azure/cloud"
)

var (
	defaultUser     string = "azureuser"
	managedIdentity string = "msi"
)

// Spec contains properties to create a managed cluster.
type Spec struct {
	// Name is the name of this AKS Cluster.
	Name string

	// ResourceGroup is the name of the Azure resource group for this AKS Cluster.
	ResourceGroup string

	// Location is a string matching one of the canonical Azure region names. Examples: "westus2", "eastus".
	Location string

	// Tags is a set of tags to add to this cluster.
	Tags map[string]string

	// Version defines the desired Kubernetes version.
	Version string

	// LoadBalancerSKU for the managed cluster. Possible values include: 'Standard', 'Basic'. Defaults to standard.
	LoadBalancerSKU *string

	// NetworkPlugin used for building Kubernetes network. Possible values include: 'Azure', 'Kubenet'. Defaults to Azure.
	NetworkPlugin *string

	// NetworkPolicy used for building Kubernetes network. Possible values include: 'Calico', 'Azure'. Defaults to Azure.
	NetworkPolicy *string

	// SSHPublicKey is a string literal containing an ssh public key. Will autogenerate and discard if not provided.
	SSHPublicKey string

	// AgentPools is the list of agent pool specifications in this cluster.
	AgentPools []PoolSpec

	// PodCIDR is the CIDR block for IP addresses distributed to pods
	PodCIDR string

	// ServiceCIDR is the CIDR block for IP addresses distributed to services
	ServiceCIDR string
}

type PoolSpec struct {
	Name         string
	SKU          string
	Replicas     int32
	OSDiskSizeGB int32
}

// Get fetches a managed cluster from Azure.
func (s *Service) Get(ctx context.Context, spec interface{}) (interface{}, error) {
	managedClusterSpec, ok := spec.(*Spec)
	if !ok {
		return nil, errors.New("expected managed cluster specification")
	}
	return s.Client.Get(ctx, managedClusterSpec.ResourceGroup, managedClusterSpec.Name)
}

// Get fetches a managed cluster kubeconfig from Azure.
func (s *Service) GetCredentials(ctx context.Context, group, name string) ([]byte, error) {
	return s.Client.GetCredentials(ctx, group, name)
}

// Reconcile idempotently creates or updates a managed cluster, if possible.
func (s *Service) Reconcile(ctx context.Context, spec interface{}) error {
	managedClusterSpec, ok := spec.(*Spec)
	if !ok {
		return errors.New("expected managed cluster specification")
	}

	properties := containerservice.ManagedCluster{
		Identity: &containerservice.ManagedClusterIdentity{
			Type: containerservice.SystemAssigned,
		},
		Location: &managedClusterSpec.Location,
		ManagedClusterProperties: &containerservice.ManagedClusterProperties{
			DNSPrefix:         &managedClusterSpec.Name,
			KubernetesVersion: &managedClusterSpec.Version,
			LinuxProfile: &containerservice.LinuxProfile{
				AdminUsername: &defaultUser,
				SSH: &containerservice.SSHConfiguration{
					PublicKeys: &[]containerservice.SSHPublicKey{
						{
							KeyData: &managedClusterSpec.SSHPublicKey,
						},
					},
				},
			},
			ServicePrincipalProfile: &containerservice.ManagedClusterServicePrincipalProfile{
				ClientID: &managedIdentity,
			},
			AgentPoolProfiles: &[]containerservice.ManagedClusterAgentPoolProfile{},
			NetworkProfile: &containerservice.NetworkProfileType{
				NetworkPlugin:   containerservice.Azure,
				LoadBalancerSku: containerservice.Standard,
			},
		},
	}

	if managedClusterSpec.NetworkPlugin != nil {
		properties.NetworkProfile.NetworkPlugin = containerservice.NetworkPlugin(*managedClusterSpec.NetworkPlugin)
	}

	if managedClusterSpec.PodCIDR != "" {
		properties.NetworkProfile.PodCidr = &managedClusterSpec.PodCIDR
	}

	if managedClusterSpec.ServiceCIDR != "" {
		properties.NetworkProfile.ServiceCidr = &managedClusterSpec.ServiceCIDR
		ip, _, err := net.ParseCIDR(managedClusterSpec.ServiceCIDR)
		if err != nil {
			return fmt.Errorf("failed to parse service cidr: %w", err)
		}
		// HACK: set the last octet of the IP to .10
		// This ensures the dns IP is valid in the service cidr without forcing the user
		// to specify it in both the Capi cluster and the Azure control plane.
		// https://golang.org/src/net/ip.go#L48
		ip[15] = byte(10)
		dnsIP := ip.String()
		properties.NetworkProfile.DNSServiceIP = &dnsIP

	}

	if managedClusterSpec.NetworkPolicy != nil {
		if strings.EqualFold(*managedClusterSpec.NetworkPolicy, "Azure") {
			properties.NetworkProfile.NetworkPolicy = containerservice.NetworkPolicyAzure
		} else if strings.EqualFold(*managedClusterSpec.NetworkPolicy, "Calico") {
			properties.NetworkProfile.NetworkPolicy = containerservice.NetworkPolicyCalico
		} else {
			return fmt.Errorf("invalid network policy: '%s'. Allowed options are 'calico' and 'azure'", *managedClusterSpec.NetworkPolicy)
		}
	}

	if managedClusterSpec.LoadBalancerSKU != nil {
		properties.NetworkProfile.LoadBalancerSku = containerservice.LoadBalancerSku(*managedClusterSpec.LoadBalancerSKU)
	}

	for _, pool := range managedClusterSpec.AgentPools {
		profile := containerservice.ManagedClusterAgentPoolProfile{
			Name:         &pool.Name,
			VMSize:       containerservice.VMSizeTypes(pool.SKU),
			OsDiskSizeGB: &pool.OSDiskSizeGB,
			Count:        &pool.Replicas,
			Type:         containerservice.VirtualMachineScaleSets,
		}
		*properties.AgentPoolProfiles = append(*properties.AgentPoolProfiles, profile)
	}

	err := s.Client.CreateOrUpdate(ctx, managedClusterSpec.ResourceGroup, managedClusterSpec.Name, properties)
	if err != nil {
		return fmt.Errorf("failed to create or update managed cluster, %#+v", err)
	}

	return nil
}

// Delete deletes the virtual network with the provided name.
func (s *Service) Delete(ctx context.Context, spec interface{}) error {
	managedClusterSpec, ok := spec.(*Spec)
	if !ok {
		return errors.New("expected managed cluster specification")
	}

	klog.V(2).Infof("Deleting managed cluster  %s ", managedClusterSpec.Name)
	err := s.Client.Delete(ctx, managedClusterSpec.ResourceGroup, managedClusterSpec.Name)
	if err != nil {
		if azure.ResourceNotFound(err) {
			// already deleted
			return nil
		}
		return errors.Wrapf(err, "failed to delete managed cluster %s in resource group %s", managedClusterSpec.Name, managedClusterSpec.ResourceGroup)
	}

	klog.V(2).Infof("successfully deleted managed cluster %s ", managedClusterSpec.Name)
	return nil
}
