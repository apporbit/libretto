// Copyright 2016 Apcera Inc. All rights reserved.

// Package management provides methods for creating and manipulating VMs on Azure using management API.
package management

import (
	"fmt"
	"net"
	"time"

	"github.com/Azure/azure-sdk-for-go/management/virtualmachine"
	"github.com/Azure/azure-sdk-for-go/management/vmutils"
	"github.com/apcera/libretto/util"

	"github.com/apcera/libretto/ssh"
	lvm "github.com/apcera/libretto/virtualmachine"
)

const (
	// PublicIP is the index of the public IP address that GetIPs returns.
	PublicIP = 0

	// PrivateIP is the index of the private IP address that GetIPs returns.
	PrivateIP = 1

	errGetClient      = "Error to retrieve Azure client %s"
	errGetListService = "Error to list hosted services %s"
	errProvisionVM    = "Error to provision Azure VM %s"
)

// SSHTimeout is the maximum time to wait before failing to GetSSH. This is not
// thread-safe.
var SSHTimeout = 800 * time.Second

var _ lvm.VirtualMachine = (*VM)(nil)

// VM represents an Azure virtual machine.
type VM struct {
	PublishSettings  string            // publishsettings file path of current account
	ServiceName      string            // Azure hosted service name
	Label            string            // Label for hosted service
	Name             string            // VM name
	Size             string            // virtual machine size
	SourceImage      string            // source vhd file name
	StorageAccount   string            // Storage account name
	StorageContainer string            // the container belongs to storage account
	Location         string            // region on Azure
	SSHCreds         ssh.Credentials   // required
	DeployOptions    DeploymentOptions // optional
	ConfigureHTTP    bool              // Flag to configure HTTP endpoint for the VM
	Cert             Certificated
}

// DeploymentOptions contains the names of some Azure networking options.
type DeploymentOptions struct {
	VirtualNetworkName string
	ReservedIPName     string
}

// Certificated data for azure hosted services
// https://azure.microsoft.com/en-us/documentation/articles/virtual-machines-linux-use-ssh-key/
type Certificated struct {
	Data   []byte
	Format string
}

// GetName returns the name of the VM.
func (vm *VM) GetName() string {
	return vm.Name
}

// Provision creates a new VM instance on Azure. It returns an error if there
// was a problem during creation.
func (vm *VM) Provision() error {
	services, err := vm.listHostedServices()
	if err != nil {
		return fmt.Errorf(errGetListService, err)
	}

	// Always try to reuse the existing hosted service. Create a new one if it
	// doesn't exist
	if !vm.serviceExist(services) {
		err = vm.createHostedService()
		if err != nil {
			return err
		}
	}

	// Create the VM
	role := vmutils.NewVMConfiguration(vm.Name, vm.Size)
	imageURL := fmt.Sprintf("https://%s.blob.core.windows.net/%s/%s.vhd", vm.StorageAccount, vm.StorageContainer, vm.Name)
	vmutils.ConfigureDeploymentFromPlatformImage(
		&role,
		vm.SourceImage,
		imageURL,
		"")

	// Assign the first subnet in virtual network to the VM
	subnet, err := vm.getFirstSubnet()
	if err != nil {
		return fmt.Errorf(errProvisionVM, err)
	}

	err = vmutils.ConfigureWithSubnet(&role, subnet)
	if err != nil {
		return fmt.Errorf(errProvisionVM, err)
	}

	vmclient, err := vm.getVMClient()
	if err != nil {
		return fmt.Errorf(errGetClient, err)
	}

	err = vmutils.ConfigureForLinux(&role, vm.Name, vm.SSHCreds.SSHUser, vm.SSHCreds.SSHPassword)
	if err != nil {
		return fmt.Errorf(errProvisionVM, err)
	}

	err = vmutils.ConfigureWithPublicSSH(&role)
	if err != nil {
		return fmt.Errorf(errProvisionVM, err)
	}

	if vm.ConfigureHTTP {
		err = vmutils.ConfigureWithExternalPort(&role, "HTTP", 80, 80, virtualmachine.InputEndpointProtocolTCP)
		if err != nil {
			return fmt.Errorf(errProvisionVM, err)
		}
	}

	operationID, err := vmclient.CreateDeployment(role, vm.ServiceName, vm.getDeploymentOptions())
	if err != nil {
		return fmt.Errorf(errProvisionVM, err)
	}

	if err := client.WaitForOperation(operationID, nil); err != nil {
		return fmt.Errorf(errProvisionVM, err)
	}

	// Use GetSSH to pull the VM status now
	cli, err := vm.GetSSH(ssh.Options{KeepAlive: 2})
	if err != nil {
		return err
	}

	return cli.WaitForSSH(SSHTimeout)
}

// GetIPs returns the IP addresses of the Azure VM instance.
func (vm *VM) GetIPs() ([]net.IP, error) {
	vmclient, err := vm.getVMClient()
	if err != nil {
		return nil, err
	}

	resp, err := vmclient.GetDeployment(vm.ServiceName, vm.Name)
	if err != nil {
		return nil, err
	}

	ips := make([]net.IP, 2)
	if ip := resp.VirtualIPs[0].Address; ip != "" {
		ips[PublicIP] = net.ParseIP(ip)
	}
	if ip := resp.RoleInstanceList[0].IPAddress; ip != "" {
		ips[PrivateIP] = net.ParseIP(ip)
	}

	return ips, nil
}

// GetSSH returns an SSH client that can be used to connect to the VM. An error
// is returned if the VM has no IPs.
func (vm *VM) GetSSH(options ssh.Options) (ssh.Client, error) {
	ips, err := util.GetVMIPs(vm, options)
	if err != nil {
		return nil, err
	}

	client := ssh.SSHClient{
		Creds:   &vm.SSHCreds,
		IP:      ips[PublicIP],
		Options: options,
		Port:    22,
	}
	return &client, nil
}

// GetState returns the status of the Azure VM. The status will be one of the
// following:
//     "Running"
//     "Suspended"
//     "RunningTransitioning"
//     "SuspendedTransitioning"
//     "Starting"
//     "Suspending"
//     "Deploying"
//     "Deleting"
func (vm *VM) GetState() (string, error) {
	vmclient, err := vm.getVMClient()
	if err != nil {
		return "", fmt.Errorf(errGetClient, err)
	}

	resp, err := vmclient.GetDeployment(vm.ServiceName, vm.Name)
	if err != nil {
		return "", lvm.ErrVMInfoFailed
	}

	return vm.translateState(string(resp.Status)), nil
}

// Destroy deletes the VM on Azure.
func (vm *VM) Destroy() error {
	vmclient, err := vm.getVMClient()
	if err != nil {
		return fmt.Errorf(errGetClient, err)
	}

	reqID, err := vmclient.DeleteDeployment(vm.ServiceName, vm.Name)
	if err != nil {
		return err
	}

	// and wait for the deletion:
	if err := client.WaitForOperation(reqID, nil); err != nil {
		return fmt.Errorf("Error waiting for instance %s to be deleted off the hosted service %s: %s",
			vm.Name, vm.Name, err)
	}

	return vm.deleteHostedService()
}

// Halt shuts down the VM.
func (vm *VM) Halt() error {
	vmclient, err := vm.getVMClient()
	if err != nil {
		return fmt.Errorf(errGetClient, err)
	}

	reqID, err := vmclient.ShutdownRole(vm.ServiceName, vm.Name, vm.Name, virtualmachine.PostShutdownActionStopped)

	if err != nil {
		return err
	}

	// Wait for the shutdown
	if err := client.WaitForOperation(reqID, nil); err != nil {
		return fmt.Errorf("Error waiting for instance %s to be shutting down the hosted service %s: %s",
			vm.Name, vm.Name, err)
	}
	return nil
}

// Start boots a stopped VM.
func (vm *VM) Start() error {
	vmclient, err := vm.getVMClient()
	if err != nil {
		return fmt.Errorf(errGetClient, err)
	}

	reqID, err := vmclient.StartRole(vm.ServiceName, vm.Name, vm.Name)
	if err != nil {
		return err
	}

	// Wait for the shutdown
	if err := client.WaitForOperation(reqID, nil); err != nil {
		return fmt.Errorf("Error waiting for instance %s to be starting the hosted service %s: %s",
			vm.Name, vm.Name, err)
	}
	return nil
}

// Suspend returns an error because it is not supported on Azure.
func (vm *VM) Suspend() error {
	return lvm.ErrSuspendNotSupported
}

// Resume returns an error because it is not supported on Azure.
func (vm *VM) Resume() error {
	return lvm.ErrResumeNotSupported
}

func (vm *VM) AddDisk() error {
        return errors.New("TO-DO")
}

func (vm *VM) RemoveDisk(diskName string) error {
        return errors.New("TO-DO")
}
