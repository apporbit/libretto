// Package exoscale provides a standard way to create a virtual machine on exoscale.ch.
package exoscale

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/apcera/libretto/ssh"
	"github.com/apcera/libretto/util"
	"github.com/apcera/libretto/virtualmachine"
	"github.com/pyr/egoscale"
)

// VM represents an Exoscale virtual machine.
type VM struct {
	Config Config // Exoscale client configuration

	Name            string          // virtual machine name
	Template        Template        // template identification
	ServiceOffering ServiceOffering // Service offering
	SecurityGroups  []SecurityGroup // list of security groups associated with the virtual machine
	KeypairName     string          // SSH Keypair identifier to use
	Userdata        string          // User data sent to the virtual machine
	Zone            Zone            // Zone identifier

	ID    string // Virtual machine ID.
	JobID string // virtual machine creation job ID

	SSHCreds ssh.Credentials // SSH credentials required to connect to machine

	ips   []net.IP // IP addresses
	state string   // machine state
}

// Config is the new droplet payload
type Config struct {
	Endpoint  string `json:"endpoint,omitempty"`  // required
	APIKey    string `json:"apikey,omitempty"`    // required
	APISecret string `json:"apisecret,omitempty"` // required
}

// Template is the base image for Exoscale virtual machines
type Template struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	ZoneName  string `json:"zonename,omitempty"`
	StorageGB int    `json:"storagegb,omitempty"`
}

// ServiceOffering is a Exoscale machine type offering
type ServiceOffering struct {
	ID   string              `json:"id,omitempty"`
	Name ServiceOfferingType `json:"name,omitempty"`
}

// SecurityGroup is a Exoscale security group
type SecurityGroup struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// Zone is a Exoscale zone
type Zone struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// ServiceOfferingType is a Exoscale instance type offering
type ServiceOfferingType string

// Exoscal offerings
const (
	Micro      ServiceOfferingType = "Micro"
	Tiny       ServiceOfferingType = "Tiny"
	Small      ServiceOfferingType = "Small"
	Medium     ServiceOfferingType = "Medium"
	Large      ServiceOfferingType = "Large"
	ExtraLarge ServiceOfferingType = "Extra-large"
	Huge       ServiceOfferingType = "Huge"
)

// SSHTimeout is the maximum time to wait before failing to GetSSH. This is not
// thread-safe.
var SSHTimeout = 30 * time.Second

// This ensures that exoscale.VM implements the virtualmachine.VirtualMachine
// interface at compile time.
var _ virtualmachine.VirtualMachine = (*VM)(nil)

// GetName returns the name of the virtual machine
// If an error occurs, an empty string is returned
func (vm *VM) GetName() string {

	if err := vm.updateInfo(); err != nil {
		return ""
	}

	return vm.Name
}

// Provision creates a virtual machine on exoscale.
// A JobID is informed that can be used to poll the VM creation process (see WaitVMCreation)
func (vm *VM) Provision() error {

	if vm.Template.ID == "" {
		if err := vm.fillTemplateID(); err != nil {
			return err
		}
	}

	if vm.ServiceOffering.ID == "" {
		if err := vm.fillServiceOfferingID(); err != nil {
			return err
		}
	}

	for _, sg := range vm.SecurityGroups {
		if sg.ID == "" {
			vm.fillSecurityGroupsID()
			break
		}
	}

	if vm.Zone.ID == "" {
		if err := vm.fillZoneID(); err != nil {
			return err
		}
	}

	securityGroups := make([]string, len(vm.SecurityGroups))
	for i := range vm.SecurityGroups {
		securityGroups[i] = vm.SecurityGroups[i].ID
	}

	profile := egoscale.MachineProfile{
		Template:        vm.Template.ID,
		ServiceOffering: vm.ServiceOffering.ID,
		SecurityGroups:  securityGroups,
		Keypair:         vm.KeypairName,
		Userdata:        vm.Userdata,
		Zone:            vm.Zone.ID,
		Name:            vm.Name,
	}

	client := vm.getExoClient()
	jobID, err := client.CreateVirtualMachine(profile)
	if err != nil {
		return err
	}

	vm.JobID = jobID

	return nil
}

// GetIPs returns the list of ip addresses associated with the VM
func (vm *VM) GetIPs() ([]net.IP, error) {

	if err := vm.updateInfo(); err != nil {
		return nil, err
	}

	return vm.ips, nil
}

// Destroy removes virtual machine and all storage associated
func (vm *VM) Destroy() error {

	if vm.ID == "" {
		return fmt.Errorf("Need an ID to destroy the virtual machine")
	}

	params := url.Values{}
	params.Set("id", vm.ID)

	client := vm.getExoClient()
	resp, err := client.Request("destroyVirtualMachine", params)
	if err != nil {
		return fmt.Errorf("Destroying virtual machine %q: %s", vm.ID, err)
	}

	destroy := &egoscale.DestroyVirtualMachineResponse{}
	if err := json.Unmarshal(resp, destroy); err != nil {
		return fmt.Errorf("Destroying virtual machine %q: %s", vm.ID, err)
	}

	vm.JobID = destroy.JobID

	return nil
}

// GetState returns virtual machine state
func (vm *VM) GetState() (string, error) {

	if vm.ID == "" {
		return "", fmt.Errorf("Need an ID to get virtual machine state")
	}

	if err := vm.updateInfo(); err != nil {
		return "", err
	}

	return vm.state, nil
}

// Suspend pauses the virtual machine. Not supported
func (vm *VM) Suspend() error {
	return virtualmachine.ErrSuspendNotSupported
}

// Resume resumes a suspended virtual machine. Not supported
func (vm *VM) Resume() error {
	return virtualmachine.ErrResumeNotSupported
}

// Halt stop a virtual machine
func (vm *VM) Halt() error {

	if vm.ID == "" {
		return fmt.Errorf("Need an ID to stop the virtual machine")
	}

	params := url.Values{}
	params.Set("id", vm.ID)

	client := vm.getExoClient()
	resp, err := client.Request("stopVirtualMachine", params)
	if err != nil {
		return fmt.Errorf("Stopping virtual machine %q: %s", vm.ID, err)
	}

	stop := &egoscale.StopVirtualMachineResponse{}
	if err := json.Unmarshal(resp, stop); err != nil {
		return fmt.Errorf("Stopping virtual machine %q: %s", vm.ID, err)
	}

	vm.JobID = stop.JobID

	return nil

}

// Start starts virtual machine
func (vm *VM) Start() error {

	if vm.ID == "" {
		return fmt.Errorf("Need an ID to start the virtual machine")
	}

	params := url.Values{}
	params.Set("id", vm.ID)

	client := vm.getExoClient()
	resp, err := client.Request("startVirtualMachine", params)
	if err != nil {
		return fmt.Errorf("Starting virtual machine %q: %s", vm.ID, err)
	}

	start := &egoscale.StartVirtualMachineResponse{}
	if err := json.Unmarshal(resp, start); err != nil {
		return fmt.Errorf("Starting virtual machine %q: %s", vm.ID, err)
	}

	vm.JobID = start.JobID

	return nil
}

// GetSSH returns SSH keys to access the virtual machine
func (vm *VM) GetSSH(options ssh.Options) (ssh.Client, error) {

	ips, err := util.GetVMIPs(vm, options)
	if err != nil {
		return nil, err
	}

	client := &ssh.SSHClient{
		Creds:   &vm.SSHCreds,
		IP:      ips[0], // public IP
		Options: options,
		Port:    22,
	}
	if err := client.WaitForSSH(SSHTimeout); err != nil {
		return nil, err
	}

	return client, nil

}

func (vm *VM) AddDisk() error {
        return errors.New("TO-DO")
}

func (vm *VM) RemoveDisk(diskName string) error {
        return errors.New("TO-DO")
}
