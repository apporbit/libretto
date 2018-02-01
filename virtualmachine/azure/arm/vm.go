// Copyright 2016 Apcera Inc. All rights reserved.

// Package arm provides methods for creating and manipulating VMs on Azure using arm API.
package arm

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/apcera/libretto/ssh"
	"github.com/apcera/libretto/util"
	lvm "github.com/apcera/libretto/virtualmachine"

	"github.com/Azure/azure-sdk-for-go/arm/compute"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/azure"
)

var (
	// ErrActionTimeout is returned when the Azure instance takes too long to enter waited state.
	ErrActionTimeout = errors.New("Azure action timeout")
)

const (
	// PublicIP is the index of the public IP address that GetIPs returns.
	PublicIP = 0

	// PrivateIP is the index of the private IP address that GetIPs returns.
	PrivateIP = 1

	// actionTimeout is the maximum seconds to wait before failing to
	// any action on VM, such as Provision, Halt or Destroy.
	actionTimeout = 90

	// running is the status returned when the VM is running
	running = "VM running"

	// stopped is the status returned when the VM is halted
	stopped = "VM stopped"

	// succeeded is the status returned when a deployment ends successfully
	succeeded = "Succeeded"
)

// SSHTimeout is the maximum time to wait before failing to GetSSH. This is not
// thread-safe.
var SSHTimeout = 180 * time.Second

var _ lvm.VirtualMachine = (*VM)(nil)

// OAuthCredentials is the struct that stors OAUTH credentials
type OAuthCredentials struct {
	ClientID       string
	ClientSecret   string
	TenantID       string
	SubscriptionID string
}

// VM represents an Azure virtual machine.
type VM struct {
	// Credentials to connect Azure
	Creds OAuthCredentials

	// Image Properties
	ImagePublisher string
	ImageOffer     string
	ImageSku       string

	// VM Properties
	Size string
	Name string

	// SSH Properties
	SSHCreds     ssh.Credentials // required
	SSHPublicKey string

	// Deployment Properties
	ResourceGroup    string
	StorageAccount   string
	StorageContainer string

	// VM OS Properties
	OsFile string

	// VM Disk Properties
	DiskFile string
	DiskSize int //GB

	// VM Network Properties
	NetworkSecurityGroup string
	Nic                  string
	PublicIP             string
	Subnet               string
	VirtualNetwork       string

	// deployment
	DeploymentName string
}

// GetName returns the name of the VM.
func (vm *VM) GetName() string {
	return vm.Name
}

// Provision creates a new VM instance on Azure. It returns an error if there
// was a problem during creation.
func (vm *VM) Provision() error {
	// Validate VM
	err := validateVM(vm)
	if err != nil {
		return err
	}

	// Set up private members of the VM
	tempName := randStringRunes(5)
	if vm.OsFile == "" {
		vm.OsFile = tempName + "-os-disk.vhd"
	}
	if vm.DiskFile == "" {
		vm.DiskFile = tempName + "-disk.vhd"
	}
	if vm.PublicIP == "" {
		vm.PublicIP = tempName + "-public-ip"
	}
	if vm.Nic == "" {
		vm.Nic = tempName + "-nic"
	}
	if vm.DeploymentName == "" {
		vm.DeploymentName = tempName + "-deploy"
	}

	err = vm.deploy()
	if err != nil {
		return err
	}

	cli, err := vm.GetSSH(ssh.Options{KeepAlive: 2})
	if err != nil {
		return err
	}

	return cli.WaitForSSH(SSHTimeout)
}

// GetIPs returns the IP addresses of the Azure VM instance.
func (vm *VM) GetIPs() ([]net.IP, error) {
	ips := make([]net.IP, 2)

	// Set up the auth token.
	tok, err := getServicePrincipalToken(&vm.Creds, azure.PublicCloud.ResourceManagerEndpoint)
	if err != nil {
		return nil, err
	}
	authorizer := autorest.NewBearerAuthorizer(tok)

	// Get the Public IP
	ip, err := vm.getPublicIP(authorizer)
	if err != nil {
		return nil, err
	}
	ips[PublicIP] = ip

	// Get the Private IP
	ip, err = vm.getPrivateIP(authorizer)
	if err != nil {
		return nil, err
	}
	ips[PrivateIP] = ip

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
//     "running"
//     "stopped"
func (vm *VM) GetState() (string, error) {
	// Set up the authorizer
	tok, err := getServicePrincipalToken(&vm.Creds, azure.PublicCloud.ResourceManagerEndpoint)
	if err != nil {
		return "", err
	}
	authorizer := autorest.NewBearerAuthorizer(tok)

	virtualMachinesClient := compute.NewVirtualMachinesClient(vm.Creds.SubscriptionID)
	virtualMachinesClient.Authorizer = authorizer

	r, e := virtualMachinesClient.Get(vm.ResourceGroup, vm.Name, "InstanceView")
	if e != nil {
		return "", e
	}

	if r.VirtualMachineProperties != nil && r.VirtualMachineProperties.InstanceView != nil && len(*r.VirtualMachineProperties.InstanceView.Statuses) > 1 {
		state := *(*r.VirtualMachineProperties.InstanceView.Statuses)[1].DisplayStatus
		return translateState(state), e
	}

	return "", errors.New("failed to get VM status")
}

// Destroy deletes the VM on Azure.
func (vm *VM) Destroy() error {
	// Set up the authorizer
	tok, err := getServicePrincipalToken(&vm.Creds, azure.PublicCloud.ResourceManagerEndpoint)
	if err != nil {
		return err
	}
	authorizer := autorest.NewBearerAuthorizer(tok)

	// Delete the VM
	virtualMachinesClient := compute.NewVirtualMachinesClient(vm.Creds.SubscriptionID)
	virtualMachinesClient.Authorizer = authorizer

	_, errc := virtualMachinesClient.Delete(vm.ResourceGroup, vm.Name, nil)
	if err := <-errc; err != nil {
		return err
	}

	// Make sure VM is deleted
	deleted := false
	for i := 0; i < actionTimeout; i++ {
		_, err := vm.GetState()
		if err != nil {
			if strings.Contains(err.Error(), `Code="ResourceNotFound"`) ||
				strings.Contains(err.Error(), `Code="NotFound"`) {
				deleted = true
				break
			}
			return err
		}

		time.Sleep(1 * time.Second)
	}

	if !deleted {
		return ErrActionTimeout
	}

	var errors []error
	// Delete the files of this VM
	err = vm.deleteVMFiles(authorizer)
	if err != nil {
		errors = append(errors, err)
	}

	// Delete the network interface of this VM
	err = vm.deleteNic(authorizer)
	if err != nil {
		errors = append(errors, err)
	} else {
		// Delete the public IP of this VM
		err = vm.deletePublicIP(authorizer)
		if err != nil {
			errors = append(errors, err)
		}
	}

	// Delete the deployed arm template
	err = vm.deleteDeployment(authorizer)
	if err != nil {
		errors = append(errors, err)
	}

	// Return all the errors
	var returnedErr error
	if len(errors) > 0 {
		for i, err := range errors {
			if i == 0 {
				returnedErr = err
				continue
			}

			returnedErr = fmt.Errorf("%s, %s", returnedErr, err)
		}
	}

	return returnedErr
}

// Halt shuts down the VM.
func (vm *VM) Halt() error {
	// Set up the authorizer
	tok, err := getServicePrincipalToken(&vm.Creds, azure.PublicCloud.ResourceManagerEndpoint)
	if err != nil {
		return err
	}
	authorizer := autorest.NewBearerAuthorizer(tok)

	// Poweroff the VM
	virtualMachinesClient := compute.NewVirtualMachinesClient(vm.Creds.SubscriptionID)
	virtualMachinesClient.Authorizer = authorizer

	_, errc := virtualMachinesClient.PowerOff(vm.ResourceGroup, vm.Name, nil)
	if err := <-errc; err != nil {
		return err
	}

	// Make sure the VM is stopped
	for i := 0; i < actionTimeout; i++ {
		state, err := vm.GetState()
		if err != nil {
			return err
		}
		if state == lvm.VMHalted {
			return nil
		}

		time.Sleep(1 * time.Second)
	}
	return ErrActionTimeout
}

// Start boots a stopped VM.
func (vm *VM) Start() error {
	// Set up the authorizer
	tok, err := getServicePrincipalToken(&vm.Creds, azure.PublicCloud.ResourceManagerEndpoint)
	if err != nil {
		return err
	}
	authorizer := autorest.NewBearerAuthorizer(tok)

	// Start the VM
	virtualMachinesClient := compute.NewVirtualMachinesClient(vm.Creds.SubscriptionID)
	virtualMachinesClient.Authorizer = authorizer

	_, errc := virtualMachinesClient.Start(vm.ResourceGroup, vm.Name, nil)
	if err := <-errc; err != nil {
		return err
	}

	// Make sure the VM is running
	for i := 0; i < actionTimeout; i++ {
		state, err := vm.GetState()
		if err != nil {
			return err
		}
		if state == lvm.VMRunning {
			return nil
		}

		time.Sleep(1 * time.Second)
	}
	return ErrActionTimeout
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
