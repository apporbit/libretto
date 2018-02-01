// Copyright 2016 Apcera Inc. All rights reserved.

package management

import (
	"encoding/base64"
	"fmt"
	"sync"

	lvm "github.com/apcera/libretto/virtualmachine"

	"github.com/Azure/azure-sdk-for-go/management"
	"github.com/Azure/azure-sdk-for-go/management/hostedservice"
	"github.com/Azure/azure-sdk-for-go/management/virtualmachine"
	"github.com/Azure/azure-sdk-for-go/management/virtualnetwork"
)

// Cache the Azure client object.
var (
	mu     sync.Mutex
	client management.Client
)

// getClient instantiates an Azure client if necessary and returns a copy of the
// client. It returns an error if there is a problem reading or unmarshaling the
// .publishSettings file.
func (vm *VM) getClient() (management.Client, error) {
	mu.Lock()
	defer mu.Unlock()
	if client != nil {
		return client, nil
	}

	var err error
	client, err = management.ClientFromPublishSettingsFile(vm.PublishSettings, "")
	if err != nil {
		return nil, err
	}

	return client, nil
}

// getVMClient returns a new Azure virtual machine client.
func (vm *VM) getVMClient() (virtualmachine.VirtualMachineClient, error) {
	c, err := vm.getClient()
	if err != nil {
		return virtualmachine.VirtualMachineClient{}, err
	}

	return virtualmachine.NewClient(c), nil
}

// getServiceClient returns a new Azure hosted service client.
func (vm *VM) getServiceClient() (hostedservice.HostedServiceClient, error) {
	c, err := vm.getClient()
	if err != nil {
		return hostedservice.HostedServiceClient{}, err
	}

	return hostedservice.NewClient(c), nil
}

// getVirtualNetworkClient returns a new virtual network client.
func (vm *VM) getVirtualNetworkClient() (virtualnetwork.VirtualNetworkClient, error) {
	c, err := vm.getClient()
	if err != nil {
		return virtualnetwork.VirtualNetworkClient{}, err
	}

	return virtualnetwork.NewClient(c), nil
}

// createHostedService creates a hosted service on Azure.
func (vm *VM) createHostedService() error {
	sc, err := vm.getServiceClient()
	if err != nil {
		return fmt.Errorf(errGetClient, err)
	}

	// create hosted service
	if err := sc.CreateHostedService(hostedservice.CreateHostedServiceParameters{
		ServiceName: vm.ServiceName,
		Location:    vm.Location,
		Label:       base64.StdEncoding.EncodeToString([]byte(vm.Label))}); err != nil {
		return err
	}

	if vm.Cert.Data != nil {
		err = vm.addAddCertificate()
		if err != nil {
			return err
		}

	}

	return nil
}

// deleteHostedService deletes a hosted service on Azure.
func (vm *VM) deleteHostedService() error {
	sc, err := vm.getServiceClient()
	if err != nil {
		return fmt.Errorf(errGetClient, err)
	}

	// Delete hosted service
	_, err = sc.DeleteHostedService(vm.ServiceName, true)
	return err
}

// listHostedServices lists all the hosted services under the current account.
func (vm *VM) listHostedServices() ([]hostedservice.HostedService, error) {
	sc, err := vm.getServiceClient()
	if err != nil {
		return nil, fmt.Errorf(errGetClient, err)
	}

	resp, err := sc.ListHostedServices()
	if err != nil {
		return nil, err
	}

	return resp.HostedServices, nil
}

// addAddCertificate adds certs to a hosted service.
func (vm *VM) addAddCertificate() error {
	sc, err := vm.getServiceClient()
	if err != nil {
		return fmt.Errorf(errGetClient, err)
	}

	_, err = sc.AddCertificate(vm.Name, vm.Cert.Data, hostedservice.CertificateFormat(vm.Cert.Format), vm.SSHCreds.SSHPassword)
	return err
}

// serviceExist checks if the desired service name already exists.
func (vm *VM) serviceExist(services []hostedservice.HostedService) bool {
	for _, srv := range services {
		if srv.ServiceName == vm.ServiceName {
			return true
		}
	}

	return false
}

func (vm *VM) getDeploymentOptions() virtualmachine.CreateDeploymentOptions {
	vnn := vm.DeployOptions.VirtualNetworkName
	if vnn == "" {
		return virtualmachine.CreateDeploymentOptions{}
	}
	return virtualmachine.CreateDeploymentOptions{VirtualNetworkName: vm.DeployOptions.VirtualNetworkName}
}

// getFirstSubnet gets the name of the first subnet within the VM's virtual
// network.
func (vm *VM) getFirstSubnet() (string, error) {
	vc, err := vm.getVirtualNetworkClient()
	if err != nil {
		return "", fmt.Errorf(errGetClient, err)
	}

	nc, err := vc.GetVirtualNetworkConfiguration()
	if err != nil {
		return "", fmt.Errorf("Error to get VirtualNetwork Configuration : %s", err)
	}

	for _, vns := range nc.Configuration.VirtualNetworkSites {
		if vns.Name == vm.DeployOptions.VirtualNetworkName {
			if len(vns.Subnets) == 0 {
				return "", fmt.Errorf("No subnet in the virtual network")
			}

			return vns.Subnets[0].Name, nil
		}
	}

	return "", fmt.Errorf("VirtualNetwork %s is not found", vm.DeployOptions.VirtualNetworkName)
}

// translateState converts an Azure state to a libretto state.
func (vm *VM) translateState(azureState string) string {
	switch azureState {
	case "Starting", "Deploying":
		return lvm.VMStarting
	case "Running":
		return lvm.VMRunning
	case "Suspended":
		return lvm.VMHalted
	case "Deleting", "Suspending", "RunningTransitioning":
		return lvm.VMPending
	default:
		return lvm.VMUnknown
	}
}
