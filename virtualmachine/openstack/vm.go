// Copyright 2015 Apcera Inc. All rights reserved.

package openstack

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/apcera/libretto/ssh"
	"github.com/apcera/libretto/util"
	lvm "github.com/apcera/libretto/virtualmachine"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/floatingips"
	ss "github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/startstop"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/networks"
)

// Compiler will complain if openstack.VM doesn't implement VirtualMachine interface.
var _ lvm.VirtualMachine = (*VM)(nil)

var (
	// ErrAuthOptions is returned if the credentials are not set properly as a environment variable
	ErrAuthOptions = errors.New("Openstack credentials (username and password) are not set properly")
	// ErrAuthenticatingClient is returned if the openstack do not return any provider.
	ErrAuthenticatingClient = errors.New("Failed to authenticate the client")
	// ErrInvalidRegion is returned if the region is an invalid.
	ErrInvalidRegion = errors.New("Invalid Openstack region")
	// ErrNoRegion is returned if the region is missing.
	ErrNoRegion = errors.New("Missing Openstack region")
	// ErrNoFlavor is returned querying an flavor, but none is found.
	ErrNoFlavor = errors.New("Requested flavor is not found")
	// ErrNoImage is returned querying an image, but none is found.
	ErrNoImage = errors.New("Requested image is not found")
	// ErrCreatingInstance is returned if a new server/instance is not created successfully.
	ErrCreatingInstance = errors.New("Failed to create instance")
	// ErrNoInstanceID is returned when attempting to perform an operation on an instance, but the ID is missing.
	ErrNoInstanceID = errors.New("Missing instance ID")
	// ErrNoInstance is returned querying an instance, but none is found.
	ErrNoInstance = errors.New("No instance found")
	// ErrActionTimeout is returned when the Openstack instance takes too long to enter waited state.
	ErrActionTimeout = errors.New("Openstack action timeout")
	// ErrNoIPs is returned when no IP addresses are found for an instance.
	ErrNoIPs = errors.New("No IPs found for instance")
)

const (
	// PublicIP is the index of the public IP address that GetIPs returns.
	PublicIP = 0
	// PrivateIP is the index of the private IP address that GetIPs returns.
	PrivateIP = 1

	// ActionTimeout is the maximum seconds to wait before failing to
	// any action on VM, such as Provision, Halt or Destroy.
	ActionTimeout = 900
	// ImageUploadTimeout is the maximum seconds to wait before failing to
	// upload an image.
	ImageUploadTimeout = 900
	// VolumeActionTimeout is the maximum seconds to wait before failing to
	// do an action (create, delete) on the volume.
	VolumeActionTimeout = 900

	// StateActive is the state Openstack reports when the VM is started.
	StateActive = "ACTIVE"
	// StateShutOff is the state Openstack reports when the VM is stopped.
	StateShutOff = "SHUTOFF"
	// StateError is the state Openstack reports when the given action fails on VM.
	StateError = "ERROR"

	// volumeStateAvailable is the state Openstack reports when the volume is created
	volumeStateAvailable = "available"
	// volumeStateInUse is the state Openstack reports when the volume is attached to an instance
	volumeStateInUse = "in-use"
	// volumeStateDeleted is the state Openstack reports when the volume is deleted
	volumeStateDeleted = "nil"
	// volumeStateErrorDeleting is the state Openstack reports when the error happens on deletion
	volumeStateErrorDeleting = "error_deleting"
	// imageQueued is the state Openstack reports when the image is first created
	imageQueued = "queued"
)

// SSHTimeout is the maximum time to wait before failing to GetSSH. This is not
// thread-safe.
var SSHTimeout = 900 * time.Second

// ImageMetadata represents what kind of Image will be loaded to the VM
type ImageMetadata struct {
	// Container Format for the Image, Required
	ContainerFormat string `json:"container_format,omitempty"`
	// Disk Format of the Image, Required
	DiskFormat string `json:"disk_format,omitempty"`
	// Min. amount of disk (GB) required for the image, Optional
	MinDisk int `json:"min_disk,omitempty"`
	// Min. amount of disk (GB) required for the image, Optional
	MinRAM int `json:"min_ram,omitempty"`
	// Name of the image
	Name string `json:"name"`
}

// Volume represents an Openstack disk volume
type Volume struct {
	// ID represents the ID of the volume that attached to this VM
	ID string
	// Device is the device that the volume will attach to the instance as. Omit for "auto"
	Device string
	// Name represents the name of the volume that will be attached to this VM
	Name string
	// Size represents the size of the volume that will be attached to this VM
	Size int
	// Type represents the ID of the volume type that will be attached to this VM
	Type string
}

// VM represents an Openstack EC2 virtual machine.
type VM struct {
	// IdentityEndpoint represents the Openstack Endpoint to use for creating this VM.
	IdentityEndpoint string
	// Username represents the username to use for connecting to the sdk.
	Username string
	// Password represents the password to use for connecting to the sdk.
	Password string
	// Region represents the Openstack region that this VM belongs to.
	Region string
	// TenantName represents the Openstack tenant name that this VM belnogs to
	TenantName string

	// FlavorName represents the flavor that will be used by th VM.
	FlavorName string

	// ImageID represents the image that will be used (or being used) by the VM
	ImageID string
	// Metadata contains the necessary image upload information (metadata and path)
	// about the image that will be uploaded by the VM to the Openstack.
	ImageMetadata ImageMetadata
	// ImagePath is the path that Image will be read from
	ImagePath string

	// Volume represents the volume that will be attached to this VM on provision.
	Volume Volume

	// UUID of this instance (server). Set after provisioning
	InstanceID string

	// Instance Name of the VM (optional)
	Name string

	// List of network UUIDs that this VM will be attached to
	Networks []string

	// Pool to choose a floating IP for this VM, it is required to assign an external IP
	// to the VM.
	FloatingIPPool string
	// FloatingIP is the object that stores the necessary floating ip information for this VM
	FloatingIP *floatingips.FloatingIP

	// SecurityGroup represents the name of the security group to which this VM should belong
	SecurityGroup string

	// UserData [optional] contains configuration information or scripts to use upon launch,
	// known as cloud-init scripts.
	UserData []byte

	// AdminPassword [optional] sets the root user password. If not set, a randomly-generated password
	// will be created by OpenStack API.
	AdminPassword string

	// Credentials are the credentials to use when connecting to the VM over SSH
	Credentials ssh.Credentials

	// computeClient represents the client to access to gophercloud compute api. It is set within Provision
	// and set to nil in destroy.
	computeClient *gophercloud.ServiceClient
}

// MarshalJSON serializes the VM object to JSON. It includes the FloatingIP.ID
// field which would otherwise be omitted. We can't delete the floating IPs
// during a Destroy() call without knowing the floating IP ID.
func (vm *VM) MarshalJSON() ([]byte, error) {
	// Make an alias of the VM type to avoid infinite recursion. This works
	// because the alias does not have a MarshalJSON() method.
	type (
		// credsAlias prevents a mutex in ssh.Credentials from being copied.
		credsAlias struct {
			SSHUser       string
			SSHPassword   string
			SSHPrivateKey string
		}
		vmAlias struct {
			IdentityEndpoint string
			Username         string
			Password         string
			Region           string
			TenantName       string
			FlavorName       string
			ImageID          string
			ImageMetadata    ImageMetadata
			ImagePath        string
			Volume           Volume
			InstanceID       string
			Name             string
			Networks         []string
			FloatingIPPool   string
			FloatingIP       *floatingips.FloatingIP
			SecurityGroup    string
			UserData         []byte
			AdminPassword    string
			Credentials      credsAlias
		}
	)

	// Creating the alias in this way avoids copying the mutex in
	// ssh.Credentials, which go vet doesn't like.
	alias := vmAlias{
		IdentityEndpoint: vm.IdentityEndpoint,
		Username:         vm.Username,
		Password:         vm.Password,
		Region:           vm.Region,
		TenantName:       vm.TenantName,
		FlavorName:       vm.FlavorName,
		ImageID:          vm.ImageID,
		ImageMetadata:    vm.ImageMetadata,
		ImagePath:        vm.ImagePath,
		Volume:           vm.Volume,
		InstanceID:       vm.InstanceID,
		Name:             vm.Name,
		Networks:         vm.Networks,
		FloatingIPPool:   vm.FloatingIPPool,
		FloatingIP:       vm.FloatingIP,
		SecurityGroup:    vm.SecurityGroup,
		UserData:         vm.UserData,
		AdminPassword:    vm.AdminPassword,
		Credentials: credsAlias{
			SSHUser:       vm.Credentials.SSHUser,
			SSHPassword:   vm.Credentials.SSHPassword,
			SSHPrivateKey: vm.Credentials.SSHPrivateKey,
		},
	}

	b, err := json.Marshal(alias)
	if err != nil {
		return nil, err
	}

	// We have vm serialized to JSON, but it's missing the floating IP ID.
	// Unmarshal it into a map[string]interface{}, add the floating IP ID back
	// in, then serialize it again.
	var temp interface{}
	err = json.Unmarshal(b, &temp)
	if err != nil {
		return nil, err
	}

	m := temp.(map[string]interface{})
	fip := m["FloatingIP"].(map[string]interface{})
	fip["id"] = vm.FloatingIP.ID

	return json.Marshal(m)
}

// GetName returns the name of the virtual machine
func (vm *VM) GetName() string {
	return vm.Name
}

// Provision creates a virtual machine on Openstack. It returns an error if
// there was a problem during creation, if there was a problem adding a tag, or
// if the VM takes too long to enter "running" state.
func (vm *VM) Provision() error {
	client, err := getComputeClient(vm)
	if err != nil {
		return fmt.Errorf("compute client is not set for the VM: %s", err)
	}

	// Get back an flavor ID string
	flavorID, err := flavors.IDFromName(client, vm.FlavorName)
	if err != nil {
		return ErrNoFlavor
	}

	// Fetch an image ID string
	var imageID string
	if vm.ImageID == "" {
		imageID, err = findImageIDByName(client, vm.ImageMetadata.Name)
		if err != nil {
			return fmt.Errorf("error on searching image: %s", err)
		}

		if imageID == "" {
			// Create an image ID and return the image ID
			imageID, err = createImage(vm)
			if err != nil {
				return err
			}
		}
		vm.ImageID = imageID
	} else {
		imageID = vm.ImageID
	}

	// Set the security group for this vm
	securityGroup := vm.SecurityGroup
	if securityGroup == "" {
		securityGroup = "default"
	}

	var listOfNetworks []servers.Network
	for _, networkID := range vm.Networks {
		listOfNetworks = append(listOfNetworks, servers.Network{UUID: networkID})
	}

	createOpts := servers.CreateOpts{
		Name:           vm.Name,
		FlavorRef:      flavorID,
		ImageRef:       imageID,
		Networks:       listOfNetworks,
		SecurityGroups: []string{securityGroup},
		UserData:       vm.UserData,
		AdminPass:      vm.AdminPassword,
	}

	server, err := servers.Create(client, createOpts).Extract()
	if err != nil {
		return err
	}

	// Cleanup VM if something goes wrong
	var cleanup = func(err error) error {
		if errDestroy := vm.Destroy(); errDestroy != nil {
			return fmt.Errorf("%s %s", err, errDestroy)
		}
		return err
	}

	// Set the server ID to VM ID
	vm.InstanceID = server.ID

	// Wait until VM runs
	err = waitUntil(vm, lvm.VMRunning)
	if err != nil {
		return cleanup(err)
	}

	// Create and associate an floating IP for this VM
	if vm.FloatingIPPool == "" {
		return cleanup(fmt.Errorf("empty floating IP pool"))
	}

	fip, err := floatingips.Create(client, &floatingips.CreateOpts{
		Pool: vm.FloatingIPPool,
	}).Extract()

	if err != nil {
		return cleanup(fmt.Errorf("unable to create a floating ip: %s", err))
	}

	err = floatingips.AssociateInstance(client, server.ID, floatingips.AssociateOpts{FloatingIP: fip.IP}).ExtractErr()
	if err != nil {
		errFipDelete := floatingips.Delete(client, fip.ID).ExtractErr()
		err = fmt.Errorf("%s %s", err, errFipDelete)
		return cleanup(fmt.Errorf("unable to associate a floating ip: %s", err))
	}
	vm.FloatingIP = fip

	// Wait until the VM gets ready for SSH
	err = waitUntilSSHReady(vm)
	if err != nil {
		return cleanup(err)
	}

	// Create and attach a volume to this VM, if the volume size is > 0
	if vm.Volume.Size > 0 {
		err = createAndAttachVolume(vm)
		if err != nil {
			return cleanup(err)
		}
	}

	return nil
}

// GetIPs returns a slice of IP addresses assigned to the VM. The PublicIP or
// PrivateIP consts can be used to retrieve respective IP address type. It
// returns nil if there was an error obtaining the IPs.
func (vm *VM) GetIPs() ([]net.IP, error) {
	server, err := getServer(vm)
	if server == nil || err != nil {
		// Probably need to call Provision first.
		return nil, err
	}

	client, err := getNetworkClient(vm)
	if client == nil || err != nil {
		// Probably need to create some network first.
		return nil, err
	}
	ips := make([]net.IP, 2)
	for _, networkID := range vm.Networks {
		network, err := networks.Get(client, networkID).Extract()
		if err != nil {
			return nil, err
		}

		addressSlice := server.Addresses[network.Name].([]interface{})
		for _, addressElement := range addressSlice {
			addressBlock := addressElement.(map[string]interface{})
			ipType := addressBlock["OS-EXT-IPS:type"].(string)
			ip := addressBlock["addr"].(string)
			if ipType == "floating" {
				ips[PublicIP] = net.ParseIP(ip)
			}
			if ipType == "fixed" {
				ips[PrivateIP] = net.ParseIP(ip)
			}
		}
	}

	return ips, nil
}

// Destroy terminates the VM on Openstack. It returns an error if there is no instance ID.
func (vm *VM) Destroy() error {

	if vm.InstanceID == "" {
		// Probably need to call Provision first.
		return ErrNoInstanceID
	}

	client, err := getComputeClient(vm)
	if err != nil {
		return fmt.Errorf("compute client is not set for the VM, %s", err)
	}

	// Delete the floating IP first before destroying the VM
	var errors []error
	if vm.FloatingIP != nil {
		err = floatingips.DisassociateInstance(client, vm.InstanceID, floatingips.DisassociateOpts{FloatingIP: vm.FloatingIP.IP}).ExtractErr()
		if err != nil {
			errors = append(errors, fmt.Errorf("unable to disassociate floating ip from instance: %s", err))
		} else {
			err = floatingips.Delete(client, vm.FloatingIP.ID).ExtractErr()
			if err != nil {
				errors = append(errors, fmt.Errorf("unable to delete floating ip: %s", err))
			}
		}
	}

	// De-attach and delete the volume, if there is an attached one
	if vm.Volume.ID != "" {
		err = deattachAndDeleteVolume(vm)
		if err != nil {
			errors = append(errors, err)
		}
	}

	// Delete the instance
	err = deleteVM(client, vm.InstanceID)
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

	vm.computeClient = nil
	return returnedErr
}

// GetSSH returns an SSH client that can be used to connect to a VM. An error is
// returned if the VM has no IPs.
func (vm *VM) GetSSH(options ssh.Options) (ssh.Client, error) {
	ips, err := util.GetVMIPs(vm, options)
	if err != nil {
		return nil, err
	}

	client := ssh.SSHClient{Creds: &vm.Credentials, IP: ips[PublicIP], Port: 22, Options: options}
	return &client, nil
}

// GetState returns the state of the VM, such as "ACTIVE". An error is returned
// if the instance ID is missing, if there was a problem querying Openstack, or if
// there are no instances.
func (vm *VM) GetState() (string, error) {
	server, err := getServer(vm)
	if err != nil {
		return "", err
	}

	if server == nil {
		// VM state "unknown"
		return "", lvm.ErrVMInfoFailed
	}

	if server.Status == StateActive {
		return lvm.VMRunning, nil
	} else if server.Status == StateShutOff {
		return lvm.VMHalted, nil
	} else if server.Status == StateError {
		return lvm.VMError, nil
	}
	return lvm.VMUnknown, nil
}

// Halt shuts down the insance on Openstack.
func (vm *VM) Halt() error {
	if vm.InstanceID == "" {
		// Probably need to call Provision first.
		return ErrNoInstanceID
	}

	client, err := getComputeClient(vm)
	if err != nil {
		return fmt.Errorf("compute client is not set for the VM, %s", err)
	}

	// Take a look at the initial state of the VM. Make sure it is in ACTIVE state
	state, err := vm.GetState()
	if err != nil {
		return err
	}

	if state != lvm.VMRunning {
		return fmt.Errorf("the VM is not active, so cannot be halted")
	}

	// Stop the VM (instance)
	err = ss.Stop(client, vm.InstanceID).ExtractErr()
	if err != nil {
		return fmt.Errorf("failed to stop the instance: %s", err)
	}

	// Wait until VM halts
	return waitUntil(vm, lvm.VMHalted)
}

// Start boots a stopped VM.
func (vm *VM) Start() error {
	if vm.InstanceID == "" {
		// Probably need to call Provision first.
		return ErrNoInstanceID
	}

	client, err := getComputeClient(vm)
	if err != nil {
		return fmt.Errorf("compute client is not set for the VM, %s", err)
	}

	// Take a look at the initial state of the VM. Make sure it is in ACTIVE state
	state, err := vm.GetState()
	if err != nil {
		return err
	}

	if state != lvm.VMHalted {
		return fmt.Errorf("VM is not in halted state")
	}

	// Start the VM (instance)
	err = ss.Start(client, vm.InstanceID).ExtractErr()
	if err != nil {
		return fmt.Errorf("failed to start the instance")
	}

	// Wait until the VM gets ready for SSH
	return waitUntilSSHReady(vm)
}

// Suspend always returns an error since we do not support for Openstack for now.
// TODO Remove this error message, when suspend is supported by libretto in the future.
func (vm *VM) Suspend() error {
	return lvm.ErrSuspendNotSupported
}

// Resume always returns an error since we do not support for Openstack for now.
// TODO Remove this error message, when resume is supported by libretto in the future.
func (vm *VM) Resume() error {
	return lvm.ErrResumeNotSupported
}

func (vm *VM) AddDisk() error {
        return errors.New("TO-DO")
}

func (vm *VM) RemoveDisk(diskName string) error {
        return errors.New("TO-DO")
}
