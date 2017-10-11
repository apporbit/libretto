// Copyright 2016 Apcera Inc. All rights reserved.

package gcp

import (
	"errors"
	"net"
	"time"

	"github.com/apcera/libretto/ssh"
	"github.com/apcera/libretto/util"
	"github.com/apcera/libretto/virtualmachine"
)

const (
	// PublicIP represents the index of the public IP address that GetIPs returns.
	PublicIP = 0

	// PrivateIP represents the private IP address that GetIPs returns.
	PrivateIP = 1

	// OperationTimeout represents Maximum time(Second) to wait for operation ready.
	OperationTimeout = 180

	// DevicePathPrefix represents the prefix of the path given to the
	// disks attached to an instance. If a disk named "disk1" is attached
	// to an instance, it's path on the GCE instance becomes
	// "/dev/disk/by-id/google-disk1". This name can be used to reference
	// the device for mounting, resizing, and so on, from within the
	// instance.
	DevicePathPrefix = "/dev/disk/by-id/google-"
)

// SSHTimeout is the maximum time to wait before failing to GetSSH. This is not
// thread-safe.
var SSHTimeout = 3 * time.Minute

var (
	// Compiler will complain if google.VM doesn't implement VirtualMachine interface.
	_ virtualmachine.VirtualMachine = (*VM)(nil)
)

// VM defines a GCE virtual machine.
type VM struct {
	Name        string
	Description string
	Region      string
	Zone        string
	MachineType string
	Preemptible bool // Preemptible instances will be terminates after they run for 24 hours.

	SourceImage   string   //Required
	ImageProjects []string //Required

	Disks []Disk // At least one disk is required, the first one is booted device

	Network          string
	Subnetwork       string
	UseInternalIP    bool
	PrivateIPAddress string

	Scopes  []string //Access scopes
	Project string   //GCE project
	Tags    []string //Instance Tags

	AccountFile  string
	account      accountFile
	SSHCreds     ssh.Credentials
	SSHPublicKey string

	Firewall  string // required when modifying firewall rules
	Endpoints []Endpoint
}

// Endpoint represents the protocol and ports configured in a firewall
type Endpoint struct {
	// Protocol can be one of the following well known protocol strings
	// (tcp, udp, icmp, esp, ah, sctp) which are supported by GCP
	Protocol string
	Ports    []string
}

// Image represents a GCE image
type Image struct {
	Id                uint64 `json:"id,omitempty,string"`
	CreationTimestamp string `json:"creation_timestamp,omitempty"`
	// DeprecationStatus indicates whether the image is depcrecated. If the
	// image is not deprecated it will have an empty string.
	DeprecationStatus string `json:"deprecation_status,omitempty"`
	// DeprecationReplacement provides the url of the image which is a
	// replacement of this deprecated image. If this image is not deprecated
	// it's value will be an empty string.
	DeprecationReplacement string `json:"deprecation_replacement,omitempty"`
	Description            string `json:"description,omitempty"`
	DiskSizeGb             int64  `json:"disk_size_gb,omitempty,string"`
	Family                 string `json:"family,omitempty"`
	Name                   string `json:"name,omitempty"`
	Status                 string `json:"status,omitempty"`
}

// Disk represents the GCP Disk.
// See https://cloud.google.com/compute/docs/disks/?hl=en_US&_ga=1.115106433.702756738.1463769954
type Disk struct {
	Name        string
	DiskType    string
	DiskSizeGb  int
	AutoDelete  bool // Auto delete disk
	Description string
}

// AttachedDisk represents a disk attached to a GCE instance
type AttachedDisk struct {
	Name       string `json:"name,omitempty"`
	DevicePath string `json:"device_path,omitempty"`
	Boot       *bool  `json:"boot,omitempty"`
	AutoDelete *bool  `json:"auto_delete,omitempty"`
	Interface  string `json:"interface,omitempty"`
}

// Network defines a VPC network in GCP
type Network struct {
	Name                  string   `json:"name,omitempty"`
	Description           string   `json:"description,omitempty"`
	Id                    uint64   `json:"id,omitempty,string"`
	AutoCreateSubnetworks *bool    `json:"auto_create_subnetworks,omitempty"`
	IPv4Range             string   `json:"ipv4_range,omitempty"`
	CreationTimestamp     string   `json:"creation_timestamp,omitempty"`
	Subnetworks           []string `json:"subnetworks,omitempty"`
}

// Subnetwork represents a GCP subnetwork in a region
type Subnetwork struct {
	Name              string `json:"name,omitempty"`
	Description       string `json:"description,omitempty"`
	Id                uint64 `json:"id,omitempty,string"`
	CreationTimestamp string `json:"creation_timestamp,omitempty"`
	GatewayAddress    string `json:"gateway_address,omitempty"`
	Network           string `json:"network,omitempty"`
	Region            string `json:"region,omitempty"`
	IpCidrRange       string `json:"ipv4_range,omitempty"`
}

// MachinType defines GCP machine type (aka flavors)
type MachineType struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	GuestCpus   int64  `json:"cpus,omitempty"`
	MemoryMb    int64  `json:"memory_mb,omitempty"`
	IsSharedCpu *bool  `json:"is_shared_cpu,omitempty"`
	// Maximum persistent disks allowed
	MaximumPersistentDisks int64 `json:"max_persistent_disks,omitempty"`
	// Maximum total persistent disks size (GB) allowed
	MaximumPersistentDisksSizeGb int64 `json:"max_persistent_disks_size_gb,omitempty,string"`
}

// DiskType defines GCP disk type
type DiskType struct {
	Name              string `json:"name,omitempty"`
	Description       string `json:"description,omitempty"`
	ValidDiskSize     string `json:"valid_size,omitempty"`
	DefaultDiskSizeGb int64  `json:"default_disk_size_gb,omitempty,string"`
}

// Zone represents a GCP zone
type Zone struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Region      string `json:"region,omitempty"`
	Status      string `json:"status,omitempty"`
}

// Region represents a GCP region
type Region struct {
	Name        string   `json:"name,omitempty"`
	Description string   `json:"description,omitempty"`
	Zones       []string `json:"zones,omitempty"`
	Status      string   `json:"status,omitempty"`
}

// InstanceData represents the details of a launched GCE instance
type InstanceData struct {
	Name              string         `json:"name,omitempty"`
	Id                uint64         `json:"id,omitempty,string"`
	Status            string         `json:"status,omitempty"`
	CreationTimestamp string         `json:"creation_timestamp,omitempty"`
	PrivateIpv4       string         `json:"private_ipv4,omitempty"`
	PublicIpv4        string         `json:"public_ipv4,omitempty"`
	Volumes           []AttachedDisk `json:"volumes,omitempty"`
}

// GetName returns the name of the virtual machine.
func (vm *VM) GetName() string {
	return vm.Name
}

// Provision creates a virtual machine on GCE. It returns an error if
// there was a problem during creation.
func (vm *VM) Provision() error {
	s, err := vm.getService()
	if err != nil {
		return err
	}

	return s.provision()
}

// GetIPs returns a slice of IP addresses assigned to the VM.
func (vm *VM) GetIPs() ([]net.IP, error) {
	s, err := vm.getService()
	if err != nil {
		return nil, err
	}

	return s.getIPs()
}

// Destroy deletes the VM on GCE.
func (vm *VM) Destroy() error {
	s, err := vm.getService()
	if err != nil {
		return err
	}

	return s.delete()
}

// GetState retrieve the instance status.
func (vm *VM) GetState() (string, error) {
	s, err := vm.getService()
	if err != nil {
		return "", err
	}

	instance, err := s.getInstance()
	if err != nil {
		return "", err
	}

	switch instance.Status {
	case "PROVISIONING", "STAGING":
		return virtualmachine.VMStarting, nil
	case "RUNNING":
		return virtualmachine.VMRunning, nil
	case "STOPPING", "STOPPED", "TERMINATED":
		return virtualmachine.VMHalted, nil
	default:
		return virtualmachine.VMUnknown, nil
	}
}

// Suspend is not supported, return the error.
func (vm *VM) Suspend() error {
	return errors.New("Suspend action not supported by GCE")
}

// Resume is not supported, return the error.
func (vm *VM) Resume() error {
	return errors.New("Resume action not supported by GCE")
}

// Halt stops a GCE instance.
func (vm *VM) Halt() error {
	s, err := vm.getService()
	if err != nil {
		return err
	}

	return s.stop()
}

// Start a stopped GCE instance.
func (vm *VM) Start() error {
	s, err := vm.getService()
	if err != nil {
		return err
	}

	return s.start()
}

// Reset a GCE instance.
func (vm *VM) Reset() error {
	s, err := vm.getService()
	if err != nil {
		return err
	}

	return s.reset()
}

// GetSSH returns an SSH client connected to the instance.
func (vm *VM) GetSSH(options ssh.Options) (ssh.Client, error) {
	ips, err := vm.GetIPs()
	if err != nil {
		return nil, err
	}

	client := &ssh.SSHClient{
		Creds:   &vm.SSHCreds,
		IP:      ips[PublicIP],
		Options: options,
		Port:    22,
	}

	if err := client.WaitForSSH(SSHTimeout); err != nil {
		return nil, err
	}

	return client, nil
}

// InsertSSHKey uploads new ssh key into the GCE instance.
func (vm *VM) InsertSSHKey(publicKey string) error {
	s, err := vm.getService()
	if err != nil {
		return err
	}

	return s.insertSSHKey()
}

// DeleteDisks cleans up all the disks attached to the GCE instance.
func (vm *VM) DeleteDisks() error {
	s, err := vm.getService()
	if err != nil {
		return err
	}

	errs := s.deleteDisks()
	if len(errs) > 0 {
		err = util.CombineErrors(": ", errs...)
		return err
	}

	return nil
}

// This function cannot be used in GCP as defined in the interface. The
// function's signature will be different for vsphere, gcp and aws. This can be
// cleaned up when the VirtualMachine interface is corrected. Please refer to
// AddNewDisks function to add new disks to an instance.
func (vm *VM) AddDisk() ([]string, error) {
	return nil, nil
}

// This function cannot be used in GCP as defined in the interface. The
// function's signature will be different for vsphere, gcp and aws. This can be
// cleaned up when the VirtualMachine interface is corrected. Please refer to
// DeleteVMDisks function to detach and delete disks from a VM.
func (vm *VM) RemoveDisk(diskName []string) error {
	return errors.New("TO-DO")
}

// GetNetworkList gets the list of VPC networks
func (vm *VM) GetNetworkList() ([]Network, error) {
	var response []Network
	s, err := vm.getService()
	if err != nil {
		return nil, err
	}

	networkList, err := s.getNetworkList()
	if err != nil {
		return nil, err
	}

	subnetworks := make([]string, 0)
	for _, network := range networkList {
		subnetworks = nil
		for _, subnetworkURL := range network.Subnetworks {
			subnetworks = append(subnetworks,
				convResURLToName(subnetworkURL))
		}
		response = append(response, Network{
			Name:                  network.Name,
			Description:           network.Description,
			Id:                    network.Id,
			AutoCreateSubnetworks: &network.AutoCreateSubnetworks,
			CreationTimestamp:     network.CreationTimestamp,
			IPv4Range:             network.IPv4Range,
			Subnetworks:           subnetworks})
	}

	return response, nil
}

// GetSubnetworkList gets the list of subnetworks
func (vm *VM) GetSubnetworkList() ([]Subnetwork, error) {
	s, err := vm.getService()
	if err != nil {
		return nil, err
	}

	subnetworkList, err := s.getSubnetworkList()
	if err != nil {
		return nil, err
	}

	response := make([]Subnetwork, 0)

	for _, subnetwork := range subnetworkList {
		networkName := convResURLToName(subnetwork.Network)
		regionName := convResURLToName(subnetwork.Region)
		response = append(response, Subnetwork{
			Name:              subnetwork.Name,
			Description:       subnetwork.Description,
			Id:                subnetwork.Id,
			CreationTimestamp: subnetwork.CreationTimestamp,
			Network:           networkName,
			Region:            regionName,
			GatewayAddress:    subnetwork.GatewayAddress,
			IpCidrRange:       subnetwork.IpCidrRange,
		})
	}

	return response, nil
}

// GetMachineTypeList gets the list of available machine types (aka flavors)
func (vm *VM) GetMachineTypeList() ([]MachineType, error) {
	s, err := vm.getService()
	if err != nil {
		return nil, err
	}

	machineTypeList, err := s.getMachineTypeList()
	if err != nil {
		return nil, err
	}

	response := make([]MachineType, 0)

	for _, machineType := range machineTypeList {
		response = append(response, MachineType{
			Name:                         machineType.Name,
			Description:                  machineType.Description,
			GuestCpus:                    machineType.GuestCpus,
			MemoryMb:                     machineType.MemoryMb,
			IsSharedCpu:                  &machineType.IsSharedCpu,
			MaximumPersistentDisks:       machineType.MaximumPersistentDisks,
			MaximumPersistentDisksSizeGb: machineType.MaximumPersistentDisksSizeGb,
		})
	}

	return response, nil
}

// GetDiskTypeList gets the list of available disk types
func (vm *VM) GetDiskTypeList() ([]DiskType, error) {
	s, err := vm.getService()
	if err != nil {
		return nil, err
	}

	diskTypeList, err := s.getDiskTypeList()
	if err != nil {
		return nil, err
	}

	response := make([]DiskType, 0)

	for _, diskType := range diskTypeList {
		response = append(response, DiskType{
			Name:              diskType.Name,
			Description:       diskType.Description,
			ValidDiskSize:     diskType.ValidDiskSize,
			DefaultDiskSizeGb: diskType.DefaultDiskSizeGb,
		})
	}

	return response, nil
}

// GetZoneList gets the list of available zones
func (vm *VM) GetZoneList() ([]Zone, error) {
	s, err := vm.getService()
	if err != nil {
		return nil, err
	}

	zoneList, err := s.getZoneList()
	if err != nil {
		return nil, err
	}

	response := make([]Zone, 0)

	for _, zone := range zoneList {
		regionName := convResURLToName(zone.Region)
		response = append(response, Zone{
			Name:        zone.Name,
			Description: zone.Description,
			Region:      regionName,
			Status:      zone.Status,
		})
	}

	return response, nil
}

// GetRegionList gets the list of available regions
func (vm *VM) GetRegionList() ([]Region, error) {
	s, err := vm.getService()
	if err != nil {
		return nil, err
	}

	regionList, err := s.getRegionList()
	if err != nil {
		return nil, err
	}

	response := make([]Region, 0)

	zones := make([]string, 0)
	for _, region := range regionList {
		zones = nil
		for _, zoneUrl := range region.Zones {
			zones = append(zones, convResURLToName(zoneUrl))
		}
		response = append(response, Region{
			Name:        region.Name,
			Description: region.Description,
			Zones:       zones,
			Status:      region.Status,
		})
	}

	return response, nil
}

// AddNewDisks adds a new disks to an instance
func (vm *VM) AddNewDisks() ([]AttachedDisk, error) {
	disksPresent := make(map[string]bool)
	response := make([]AttachedDisk, 0)

	s, err := vm.getService()
	if err != nil {
		return nil, err
	}

	for _, disk := range vm.Disks {
		if err := s.createDisk(&disk); err != nil {
			return nil, err
		}

		if err := s.attachDisk(&disk); err != nil {
			return nil, err
		}
		disksPresent[disk.Name] = true
	}

	instance, err := s.getInstance()
	if err != nil {
		return nil, err
	}

	for _, attachedDisk := range instance.Disks {
		if disksPresent[attachedDisk.DeviceName] {
			response = append(response, AttachedDisk{
				Name:       attachedDisk.DeviceName,
				Boot:       &attachedDisk.Boot,
				AutoDelete: &attachedDisk.AutoDelete,
				Interface:  attachedDisk.Interface,
				DevicePath: DevicePathPrefix +
					attachedDisk.DeviceName,
			})
		}
	}
	return response, nil
}

// IsInstance checks if the given instance is present in GCP. Returns true if
// instance is available else false.
func (vm *VM) IsInstance() (bool, error) {
	s, err := vm.getService()
	if err != nil {
		return false, err
	}

	_, err = s.getInstance()
	if err != nil {
		return false, err
	} else {
		return true, nil
	}
}

// DeleteVMDisk detaches the given disks from the instance and deletes them
func (vm *VM) DeleteVMDisks() error {
	s, err := vm.getService()
	if err != nil {
		return err
	}

	for _, disk := range vm.Disks {
		if err := s.detachDisk(&disk); err != nil {
			return err
		}

		if err := s.deleteDisk(disk.Name); err != nil {
			return err
		}
	}
	return nil
}

// GetInstance gets instance details
func (vm *VM) GetInstance() (*InstanceData, error) {
	s, err := vm.getService()
	if err != nil {
		return nil, err
	}

	instance, err := s.getInstance()
	if err != nil {
		return nil, err
	}
	ips, err := vm.GetIPs()
	if err != nil {
		return nil, err
	}

	vmResponse := &InstanceData{
		Name:              instance.Name,
		Id:                instance.Id,
		CreationTimestamp: instance.CreationTimestamp,
		Status:            instance.Status,
		PrivateIpv4:       ips[PrivateIP].String(),
		PublicIpv4:        ips[PublicIP].String(),
	}
	for _, attachedDisk := range instance.Disks {
		vmResponse.Volumes = append(vmResponse.Volumes, AttachedDisk{
			Name:       attachedDisk.DeviceName,
			Boot:       &attachedDisk.Boot,
			AutoDelete: &attachedDisk.AutoDelete,
			Interface:  attachedDisk.Interface,
			DevicePath: DevicePathPrefix +
				attachedDisk.DeviceName,
		})
	}

	return vmResponse, nil
}

// AddEndpoints adds new endpoints to the given firewall
func (vm *VM) AddEndpoints() error {
	s, err := vm.getService()
	if err != nil {
		return err
	}
	return s.addFirewallPorts()
}

// RemoveEndpoints removes given endpoints from the given firewall
func (vm *VM) RemoveEndpoints() error {
	s, err := vm.getService()
	if err != nil {
		return err
	}
	return s.removeFirewallPorts()
}

// GetImageList lists images available in given projects
func (vm *VM) GetImageList() ([]Image, error) {
	s, err := vm.getService()
	if err != nil {
		return nil, err
	}

	imageList, err := s.getImageList()
	if err != nil {
		return nil, err
	}

	response := make([]Image, 0)

	for _, image := range imageList {
		depStatus := ""
		depReplacement := ""
		if image.Deprecated != nil {
			depStatus = image.Deprecated.State
			depReplacement = image.Deprecated.Replacement
		}

		response = append(response, Image{
			Name:                   image.Name,
			Description:            image.Description,
			Status:                 image.Status,
			Id:                     image.Id,
			DeprecationStatus:      depStatus,
			DeprecationReplacement: depReplacement,
			CreationTimestamp:      image.CreationTimestamp,
			DiskSizeGb:             image.DiskSizeGb,
			Family:                 image.Family,
		})
	}

	return response, nil
}
