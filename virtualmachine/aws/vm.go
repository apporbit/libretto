// Copyright 2015 Apcera Inc. All rights reserved.

// Package aws provides a standard way to create a virtual machine on AWS.
package aws

import (
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/apcera/libretto/ssh"
	"github.com/apcera/libretto/util"
	"github.com/apcera/libretto/virtualmachine"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
)

const (
	// PublicIP is the index of the public IP address that GetIPs returns.
	PublicIP = 0
	// PrivateIP is the index of the private IP address that GetIPs returns.
	PrivateIP = 1

	// StateStarted is the state AWS reports when the VM is started.
	StateStarted = "running"
	// StateHalted is the state AWS reports when the VM is halted.
	StateHalted = "stopped"
	// StateDestroyed is the state AWS reports when the VM is destroyed.
	StateDestroyed = "terminated"
	// StatePending is the state AWS reports when the VM is pending.
	StatePending = "pending"
)

// SSHTimeout is the maximum time to wait before failing to GetSSH. This is not
// thread-safe.
var SSHTimeout = 5 * time.Minute

var (
	// This ensures that aws.VM implements the virtualmachine.VirtualMachine
	// interface at compile time.
	_ virtualmachine.VirtualMachine = (*VM)(nil)

	// limiter rate limits channel to prevent saturating AWS API limits.
	limiter = time.Tick(time.Millisecond * 500)
)

var (
	// ErrNoCreds is returned when no credentials are found in environment or
	// home directory.
	ErrNoCreds = errors.New("Missing AWS credentials")
	// ErrNoRegion is returned when a request was sent without a region.
	ErrNoRegion = errors.New("Missing AWS region")
	// ErrNoInstance is returned querying an instance, but none is found.
	ErrNoInstance = errors.New("Missing VM instance")
	// ErrNoInstanceID is returned when attempting to perform an operation on
	// an instance, but the ID is missing.
	ErrNoInstanceID = errors.New("Missing instance ID")
	// ErrProvisionTimeout is returned when the EC2 instance takes too long to
	// enter "running" state.
	ErrProvisionTimeout = errors.New("AWS provision timeout")
	// ErrNoIPs is returned when no IP addresses are found for an instance.
	ErrNoIPs = errors.New("Missing IPs for instance")
	// ErrNoSupportSuspend is returned when vm.Suspend() is called.
	ErrNoSupportSuspend = errors.New("Suspend action not supported by AWS")
	// ErrNoSupportResume is returned when vm.Resume() is called.
	ErrNoSupportResume = errors.New("Resume action not supported by AWS")
)

// VM represents an AWS EC2 virtual machine.
type VM struct {
	Name                   string
	Region                 string // required
	AMI                    string
	InstanceType           string
	InstanceID             string // required when adding volume
	KeyPair                string // required
	IamInstanceProfileName string
	PrivateIPAddress       string

	// required when addding or deleting volume
	Volumes                      []EbsBlockVolume
	KeepRootVolumeOnDestroy      bool
	DeleteNonRootVolumeOnDestroy bool

	VPC    string
	Subnet string
	// required when modifying security group rules
	// all other parameters except this one and Region
	// is ingnored while security group modification
	SecurityGroups []SecurityGroup

	SSHCreds            ssh.Credentials // required
	DeleteKeysOnDestroy bool
}

// Region represents a AWS Region
type Region struct {
	Name           string `json:"name,omitempty"`
	RegionEndpoint string `json:"region_endpoint,omitempty"`
}

// Zone represents a AWS availability zone
type Zone struct {
	Name   string `json:"name,omitempty"`
	State  string `json:"state,omitempty"`
	Region string `json:"region,omitempty"`
}

// VPC represents a AWS VPC
type VPC struct {
	Id         string   `json:"id,omitempty"`
	State      string   `json:"state,omitempty"`
	IsDefault  *bool    `json:"is_default,omitempty"`
	IPv4Blocks []string `json:"ipv4_blocks,omitempty"`
	IPv6Blocks []string `json:"ipv6_blocks,omitempty"`
	// ID of DHCP options associated with VPC
	DhcpOptionsId string `json:"dhcp_options_id,omitempty"`
	// Allowed tenancy of instances launched into the VPC
	InstanceTenancy string `json:"instance_tenancy,omitempty"`
}

// Subnet represents a AWS Subnet
type Subnet struct {
	Id                    string   `json:"id,omitempty"`
	State                 string   `json:"state,omitempty"`
	VpcId                 string   `json:"vpc_id,omitempty"`
	IPv4Block             string   `json:"ipv4block,omitempty"`
	IPv6Blocks            []string `json:"ipv6blocks,omitempty"`
	AvailableAddressCount *int64   `json:"available_address_count,omitempty"`
	// Availability Zone of the subnet
	AvailabilityZone string `json:"availability_zone,omitempty"`
	// Indicates if this is default for Availability Zone
	DefaultForAz        bool `json:"default_for_az,omitempty"`
	MapPublicIpOnLaunch bool `json:"map_public_ip_on_launch,omitempty"`
}

// IpPermission in AWS is used to represent inbound or outbound rules
// associated with SecurityGroup
type IpPermission struct {
	FromPort   *int64   `json:"from_port,omitempty"`
	ToPort     *int64   `json:"to_port,omitempty"`
	IpProtocol string   `json:"ip_protocol,omitempty"`
	Ipv4Ranges []string `json:"ipv4_ranges,omitempty"`
	Ipv6Ranges []string `json:"ipv6_ranges,omitempty"`
}

// SecurityGroup represents a AWS SecurityGroup
type SecurityGroup struct {
	Id                  string         `json:"id,omitempty"`
	Name                string         `json:"name,omitempty"`
	Description         string         `json:"description,omitempty"`
	OwnerId             string         `json:"owner_id,omitempty"`
	VpcId               string         `json:"vpc_id,omitempty"`
	IpPermissionsEgress []IpPermission `json:"ip_permissions_egress,omitempty"`
	IpPermissions       []IpPermission `json:"ip_permissions,omitempty"`
}

// InstanceStatus represents AWS InstanceStatus
type InstanceStatus struct {
	AvailabilityZone string `json:"availability_zone,omitempty"`
	InstanceId       string `json:"instance_id,omitempty"`
	State            string `json:"state,omitempty"`
}

// EbsBlockVolume represents a AWS EbsBlockDevice
type EbsBlockVolume struct {
	DeviceName       string `json:"device_name,omitempty"`
	VolumeSize       *int64 `json:"volume_size,omitempty"`
	VolumeType       string `json:"volume_type,omitempty"`
	AvailabilityZone string `json:"availability_zone,omitempty"`
	VolumeId         string `json:"volume_id,omitempty"`
	SnapshotId       string `json:"snapshot_id,omitempty"`
}

// Image represents a AWS Image
type Image struct {
	Id                 *string           `json:"id,omitempty"`
	Name               *string           `json:"name,omitempty"`
	Description        *string           `json:"description,omitempty"`
	State              *string           `json:"state,omitempty"`
	OwnerId            *string           `json:"owner_id,omitempty"`
	OwnerAlias         *string           `json:"owner_alias,omitempty"`
	CreationDate       *string           `json:"creation_date,omitempty"`
	Architecture       *string           `json:"architecture,omitempty"`
	Platform           *string           `json:"platform,omitempty"`
	Hypervisor         *string           `json:"hypervisor,omitempty"`
	VirtualizationType *string           `json:"virtualization_type,omitempty"`
	ImageType          *string           `json:"image_type,omitempty"`
	KernelId           *string           `json:"kernel_id,omitemtpy"`
	RootDeviceName     *string           `json:"root_device_name,omitempty"`
	RootDeviceType     *string           `json:"root_device_type,omitempty"`
	Public             *bool             `json:"public,omitempty"`
	EbsVolumes         []*EbsBlockVolume `json:"ebs_volumes,omitempty"`
}

// GetName returns the name of the virtual machine
func (vm *VM) GetName() string {
	return vm.Name
}

// SetTag adds a tag to the VM and its attached volumes.
func (vm *VM) SetTag(key, value string) error {
	svc, err := getService(vm.Region)
	if err != nil {
		return fmt.Errorf("failed to get AWS service: %v", err)
	}

	if vm.InstanceID == "" {
		return ErrNoInstanceID
	}

	volIDs, err := getInstanceVolumeIDs(svc, vm.InstanceID)
	if err != nil {
		return fmt.Errorf("Failed to get instance's volumes IDs: %s", err)
	}

	ids := make([]*string, 0, len(volIDs)+1)
	ids = append(ids, aws.String(vm.InstanceID))
	for _, v := range volIDs {
		ids = append(ids, aws.String(v))
	}

	_, err = svc.CreateTags(&ec2.CreateTagsInput{
		Resources: ids,
		Tags: []*ec2.Tag{
			{Key: aws.String(key),
				Value: aws.String(value)},
		},
	})
	if err != nil {
		return fmt.Errorf("Failed to create tag on VM: %v", err)
	}

	return nil
}

// SetTags takes in a map of tags to set to the provisioned instance. This is
// essentially a shorter way than calling SetTag many times.
func (vm *VM) SetTags(tags map[string]string) error {
	for k, v := range tags {
		if err := vm.SetTag(k, v); err != nil {
			return err
		}
	}
	return nil
}

// Provision creates a virtual machine on AWS. It returns an error if
// there was a problem during creation, if there was a problem adding a tag, or
// if the VM takes too long to enter "running" state.
func (vm *VM) Provision() error {
	<-limiter
	svc, err := getService(vm.Region)
	if err != nil {
		return fmt.Errorf("failed to get AWS service: %v", err)
	}

	resp, err := svc.RunInstances(instanceInfo(vm))
	if err != nil {
		return fmt.Errorf("Failed to create instance: %v", err)
	}

	if hasInstanceID(resp.Instances[0]) {
		vm.InstanceID = *resp.Instances[0].InstanceId
	} else {
		return ErrNoInstanceID
	}

	if err := waitUntilReady(svc, vm.InstanceID); err != nil {
		return err
	}

	if vm.DeleteNonRootVolumeOnDestroy {
		return setNonRootDeleteOnDestroy(svc, vm.InstanceID, true)
	}

	if vm.Name != "" {
		if err := vm.SetTag("Name", vm.GetName()); err != nil {
			return err
		}
	}

	return nil
}

// GetIPs returns a slice of IP addresses assigned to the VM. The PublicIP or
// PrivateIP consts can be used to retrieve respective IP address type. It
// returns nil if there was an error obtaining the IPs.
func (vm *VM) GetIPs() ([]net.IP, error) {
	svc, err := getService(vm.Region)
	if err != nil {
		return nil, fmt.Errorf("failed to get AWS service: %v", err)
	}

	if vm.InstanceID == "" {
		// Probably need to call Provision first.
		return nil, ErrNoInstanceID
	}

	inst, err := svc.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{
			aws.String(vm.InstanceID),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("Failed to describe instance: %s", err)
	}

	if len(inst.Reservations) < 1 {
		return nil, errors.New("Missing instance reservation")
	}
	if len(inst.Reservations[0].Instances) < 1 {
		return nil, ErrNoInstance
	}

	ips := make([]net.IP, 2)
	if ip := inst.Reservations[0].Instances[0].PublicIpAddress; ip != nil {
		ips[PublicIP] = net.ParseIP(*ip)
	}
	if ip := inst.Reservations[0].Instances[0].PrivateIpAddress; ip != nil {
		ips[PrivateIP] = net.ParseIP(*ip)
	}

	return ips, nil
}

// Destroy terminates the VM on AWS. It returns an error if AWS credentials are
// missing or if there is no instance ID.
func (vm *VM) Destroy() error {
	svc, err := getService(vm.Region)
	if err != nil {
		return fmt.Errorf("failed to get AWS service: %v", err)
	}

	if vm.InstanceID == "" {
		// Probably need to call Provision first.
		return ErrNoInstanceID
	}
	_, err = svc.TerminateInstances(&ec2.TerminateInstancesInput{
		InstanceIds: []*string{
			aws.String(vm.InstanceID),
		},
	})
	if err != nil {
		return err
	}

	if err := waitUntilTerminated(svc, vm.InstanceID); err != nil {
		return err
	}

	if !vm.DeleteKeysOnDestroy {
		return nil
	}

	vm.ResetKeyPair()
	return nil
}

// GetSSH returns an SSH client that can be used to connect to a VM. An error
// is returned if the VM has no IPs.
func (vm *VM) GetSSH(options ssh.Options) (ssh.Client, error) {
	ips, err := util.GetVMIPs(vm, options)
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

// GetState returns the state of the VM, such as "running". An error is
// returned if the instance ID is missing, if there was a problem querying AWS,
// or if there are no instances.
func (vm *VM) GetState() (string, error) {
	svc, err := getService(vm.Region)
	if err != nil {
		return "", fmt.Errorf("failed to get AWS service: %v", err)
	}

	if vm.InstanceID == "" {
		// Probably need to call Provision first.
		return "", ErrNoInstanceID
	}

	stat, err := svc.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{
			aws.String(vm.InstanceID),
		},
	})
	if err != nil {
		return "", fmt.Errorf("Failed to describe instance: %s", err)
	}

	if n := len(stat.Reservations); n < 1 {
		return "", ErrNoInstance
	}
	if n := len(stat.Reservations[0].Instances); n < 1 {
		return "", ErrNoInstance
	}

	return *stat.Reservations[0].Instances[0].State.Name, nil
}

// Halt shuts down the VM on AWS.
func (vm *VM) Halt() error {
	svc, err := getService(vm.Region)
	if err != nil {
		return fmt.Errorf("failed to get AWS service: %v", err)
	}

	if vm.InstanceID == "" {
		// Probably need to call Provision first.
		return ErrNoInstanceID
	}

	_, err = svc.StopInstances(&ec2.StopInstancesInput{
		InstanceIds: []*string{
			aws.String(vm.InstanceID),
		},
		DryRun: aws.Bool(false),
		Force:  aws.Bool(true),
	})
	if err != nil {
		return fmt.Errorf("Failed to stop instance: %v", err)
	}

	if err := waitUntilStopped(svc, vm.InstanceID); err != nil {
		return err
	}

	return nil
}

// Start boots a stopped VM.
func (vm *VM) Start() error {
	svc, err := getService(vm.Region)
	if err != nil {
		return fmt.Errorf("failed to get AWS service: %v", err)
	}

	if vm.InstanceID == "" {
		// Probably need to call Provision first.
		return ErrNoInstanceID
	}

	_, err = svc.StartInstances(&ec2.StartInstancesInput{
		InstanceIds: []*string{
			aws.String(vm.InstanceID),
		},
		DryRun: aws.Bool(false),
	})
	if err != nil {
		return fmt.Errorf("Failed to start instance: %v", err)
	}

	if err := waitUntilRunning(svc, vm.InstanceID); err != nil {
		return err
	}

	return nil
}

// GetRegionList: returns list of regions
func (vm *VM) GetRegionList() ([]Region, error) {
	svc, err := getService(vm.Region)
	if err != nil {
		return nil, fmt.Errorf("Failed to get AWS service: %v", err)
	}

	regionListOutput, err := svc.DescribeRegions(&ec2.DescribeRegionsInput{})
	if err != nil {
		return nil, fmt.Errorf("Failed to get region list: %v", err)
	}
	response := make([]Region, 0)
	for _, region := range regionListOutput.Regions {
		response = append(response, Region{
			Name:           *region.RegionName,
			RegionEndpoint: *region.Endpoint})
	}

	return response, nil
}

// GetAvailabilityZoneList: returns list of availability zones for a region
func (vm *VM) GetAvailabilityZoneList() ([]Zone, error) {
	svc, err := getService(vm.Region)
	if err != nil {
		return nil, fmt.Errorf("Failed to get AWS service: %v", err)
	}

	zoneListOutput, err := svc.DescribeAvailabilityZones(
		&ec2.DescribeAvailabilityZonesInput{})
	if err != nil {
		return nil, fmt.Errorf("Failed to get availabilityZone list: %v", err)
	}
	response := make([]Zone, 0)
	for _, zone := range zoneListOutput.AvailabilityZones {
		response = append(response, Zone{
			Name:   *zone.ZoneName,
			State:  *zone.State,
			Region: *zone.RegionName})
	}
	return response, nil
}

// GetVPCList: returns list of VPCs for given region
func (vm *VM) GetVPCList() ([]VPC, error) {
	svc, err := getService(vm.Region)
	if err != nil {
		return nil, fmt.Errorf("Failed to get AWS service: %v", err)
	}

	vpcListOutput, err := svc.DescribeVpcs(&ec2.DescribeVpcsInput{})
	if err != nil {
		return nil, fmt.Errorf("Failed to get VPC list: %v", err)
	}
	response := make([]VPC, 0)
	for _, vpc := range vpcListOutput.Vpcs {
		ipv4Blocks := make([]string, 0)
		for _, ipv4Block := range vpc.CidrBlockAssociationSet {
			ipv4Blocks = append(ipv4Blocks,
				*ipv4Block.CidrBlock)
		}

		ipv6Blocks := make([]string, 0)
		for _, ipv6Block := range vpc.Ipv6CidrBlockAssociationSet {
			ipv6Blocks = append(ipv6Blocks,
				*ipv6Block.Ipv6CidrBlock)
		}

		response = append(response, VPC{
			Id:              *vpc.VpcId,
			State:           *vpc.State,
			IsDefault:       vpc.IsDefault,
			DhcpOptionsId:   *vpc.DhcpOptionsId,
			InstanceTenancy: *vpc.InstanceTenancy,
			IPv4Blocks:      ipv4Blocks,
			IPv6Blocks:      ipv6Blocks})
	}
	return response, nil
}

// GetSubnetList: returns list of all subnet for given region
// most relevant filter(s) (map-keys): "vpc-id", "subnet-id", "availabilityZone"
// See all available filters at below link
// http://docs.aws.amazon.com/sdk-for-go/api/service/ec2/#DescribeSubnetsInput
func (vm *VM) GetSubnetList(filter map[string][]*string) ([]Subnet, error) {
	svc, err := getService(vm.Region)
	if err != nil {
		return nil, fmt.Errorf("Failed to get AWS service: %v", err)
	}

	filters := GetFilters(filter)

	input := &ec2.DescribeSubnetsInput{
		Filters: filters}
	subnetListOutput, err := svc.DescribeSubnets(input)
	if err != nil {
		return nil, fmt.Errorf("Failed to get Subnet list: %v", err)
	}
	response := make([]Subnet, 0)
	for _, subnet := range subnetListOutput.Subnets {
		ipv6Blocks := make([]string, 0)
		for _, ipv6Block := range subnet.Ipv6CidrBlockAssociationSet {
			ipv6Blocks = append(ipv6Blocks,
				*ipv6Block.Ipv6CidrBlock)
		}

		response = append(response, Subnet{
			Id:                    *subnet.SubnetId,
			State:                 *subnet.State,
			VpcId:                 *subnet.VpcId,
			IPv4Block:             *subnet.CidrBlock,
			AvailableAddressCount: subnet.AvailableIpAddressCount,
			AvailabilityZone:      *subnet.AvailabilityZone,
			DefaultForAz:          *subnet.DefaultForAz,
			MapPublicIpOnLaunch:   *subnet.MapPublicIpOnLaunch,
			IPv6Blocks:            ipv6Blocks})
	}
	return response, nil
}

// GetSecurityGroupList : returns list of all securityGroup for given region
// most relevant filter(s) (map-keys): "vpc-id", "group-id"
// See all available filters at below link
// http://docs.aws.amazon.com/sdk-for-go/api/service/ec2/#DescribeSecurityGroupsInput
func (vm *VM) GetSecurityGroupList(filter map[string][]*string) ([]SecurityGroup, error) {
	svc, err := getService(vm.Region)
	if err != nil {
		return nil, fmt.Errorf("Failed to get AWS service: %v", err)
	}

	filters := GetFilters(filter)

	input := &ec2.DescribeSecurityGroupsInput{
		Filters: filters}

	secGrpListOutput, err := svc.DescribeSecurityGroups(input)
	if err != nil {
		return nil, fmt.Errorf("Failed to get SecurityGroup list: %v", err)
	}

	response := make([]SecurityGroup, 0)
	for _, securityGroup := range secGrpListOutput.SecurityGroups {
		ipPermissionsEgress := ToVMAWSIpPermissions(securityGroup.IpPermissionsEgress)
		ipPermissions := ToVMAWSIpPermissions(securityGroup.IpPermissions)

		response = append(response, SecurityGroup{
			Id:                  *securityGroup.GroupId,
			Name:                *securityGroup.GroupName,
			Description:         *securityGroup.Description,
			OwnerId:             *securityGroup.OwnerId,
			VpcId:               *securityGroup.VpcId,
			IpPermissionsEgress: ipPermissionsEgress,
			IpPermissions:       ipPermissions})
	}
	return response, nil
}

// GetImageList: returns list of images available for given account
// Includes public,owned private images & private images with explicit permission
func (vm *VM) GetImageList(filter map[string][]*string) ([]Image, error) {
	svc, err := getService(vm.Region)
	if err != nil {
		return nil, fmt.Errorf("Failed to get AWS service: %v", err)
	}

	filters := GetFilters(filter)

	input := &ec2.DescribeImagesInput{
		Filters: filters}

	imageListOutput, err := svc.DescribeImages(input)
	if err != nil {
		return nil, fmt.Errorf("Failed to get Image list: %v", err)
	}

	response := make([]Image, 0)
	for _, image := range imageListOutput.Images {
		img := GetVMAWSImage(image)
		response = append(response, img)
	}
	return response, nil
}

// AuthorizeSecurityGroup: Adds one or more rules to a security group
func (vm *VM) AuthorizeSecurityGroup() error {
	svc, err := getService(vm.Region)
	if err != nil {
		return fmt.Errorf("Failed to get AWS service: %v", err)
	}

	secGrp := vm.SecurityGroups[0]

	ec2IpPermissions := ToEc2IpPermissions(secGrp.IpPermissions)
	input := &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId:       &secGrp.Id,
		IpPermissions: ec2IpPermissions}

	_, err = svc.AuthorizeSecurityGroupIngress(input)
	if err != nil {
		return fmt.Errorf("Failed to authorize security group ingress rules: %v", err)
	}

	ec2IpPermissionsEgress := ToEc2IpPermissions(
		secGrp.IpPermissionsEgress)
	egressInput := &ec2.AuthorizeSecurityGroupEgressInput{
		GroupId:       &secGrp.Id,
		IpPermissions: ec2IpPermissionsEgress}
	_, err = svc.AuthorizeSecurityGroupEgress(egressInput)
	if err != nil {
		return fmt.Errorf("Failed to authorize security group egress rules: %v", err)
	}

	return nil
}

// RevokeSecurityGroup: Removes one or more rules from a security group
func (vm *VM) RevokeSecurityGroup() error {
	svc, err := getService(vm.Region)
	if err != nil {
		return fmt.Errorf("Failed to get AWS service: %v", err)
	}

	secGrp := vm.SecurityGroups[0]

	ec2IpPermissions := ToEc2IpPermissions(secGrp.IpPermissions)
	input := &ec2.RevokeSecurityGroupIngressInput{
		GroupId:       &secGrp.Id,
		IpPermissions: ec2IpPermissions}
	_, err = svc.RevokeSecurityGroupIngress(input)
	if err != nil {
		return fmt.Errorf("Failed to revoke security group ingress rules: %v", err)
	}

	ec2IpPermissionsEgress := ToEc2IpPermissions(
		secGrp.IpPermissionsEgress)
	egressInput := &ec2.RevokeSecurityGroupEgressInput{
		GroupId:       &secGrp.Id,
		IpPermissions: ec2IpPermissionsEgress}
	_, err = svc.RevokeSecurityGroupEgress(egressInput)
	if err != nil {
		return fmt.Errorf("Failed to revoke security group egress rules: %v", err)
	}

	return nil
}

// CreateVolume: Creates a volume with given parameter
func (vm *VM) CreateVolume() error {
	svc, err := getService(vm.Region)
	if err != nil {
		return fmt.Errorf("Failed to get AWS service: %v", err)
	}

	volume := vm.Volumes[0]
	instanceStatus, err := GetInstanceStatus(svc, vm.InstanceID)
	if err != nil {
		return fmt.Errorf("Failed to get availability zone of instance: %v", err)
	}
	volume.AvailabilityZone = instanceStatus.AvailabilityZone

	input := GetVolumeInput(&volume)
	response, err := svc.CreateVolume(input)
	if err != nil {
		return fmt.Errorf("Failed to create volume: %v", err)
	}

	if err := waitForCreate(svc, *response.VolumeId); err != nil {
		return err
	}

	volume.VolumeId = *response.VolumeId
	vm.Volumes[0] = volume

	return nil
}

// AttachVolume: Attaches given volume to given instance
func (vm *VM) AttachVolume() error {
	svc, err := getService(vm.Region)
	if err != nil {
		return fmt.Errorf("Failed to get AWS service: %v", err)
	}

	volume := vm.Volumes[0]
	input := &ec2.AttachVolumeInput{
		Device:     &volume.DeviceName,
		InstanceId: &vm.InstanceID,
		VolumeId:   &volume.VolumeId}
	_, err = svc.AttachVolume(input)
	if err != nil {
		return fmt.Errorf("Failed to attach volume (volumeId %s) to "+
			"instance (instanceId %s): %v", volume.VolumeId, err)
	}

	if err := waitForAttach(svc, volume.VolumeId); err != nil {
		return err
	}

	return nil
}

// DetachVolume: Detaches volume with given Id from instance
func (vm *VM) DetachVolume() error {
	svc, err := getService(vm.Region)
	if err != nil {
		return fmt.Errorf("Failed to get AWS service: %v", err)
	}

	volume := vm.Volumes[0]
	input := &ec2.DetachVolumeInput{
		VolumeId: &volume.VolumeId}
	_, err = svc.DetachVolume(input)
	if err != nil {
		return fmt.Errorf("Failed to detach volume (volumeId %s) from "+
			"instance (instanceId %s): %v", volume.VolumeId, err)
	}

	if err := waitForDetach(svc, volume.VolumeId); err != nil {
		return err
	}

	return nil
}

// DeleteVolume: Deletes volume with given Id
// Disk must not be in-use by any instance
func (vm *VM) DeleteVolume() error {
	svc, err := getService(vm.Region)
	if err != nil {
		return fmt.Errorf("Failed to get AWS service: %v", err)
	}

	volume := vm.Volumes[0]
	input := &ec2.DeleteVolumeInput{
		VolumeId: &volume.VolumeId}
	_, err = svc.DeleteVolume(input)
	if err != nil {
		return fmt.Errorf("Failed to delete volume (volumeId %s): %v",
			volume.VolumeId, err)
	}

	return nil
}

// Suspend always returns an error because this isn't supported by AWS.
func (vm *VM) Suspend() error {
	return ErrNoSupportSuspend
}

// Resume always returns an error because this isn't supported by AWS.
func (vm *VM) Resume() error {
	return ErrNoSupportResume
}

// SetKeyPair sets the given private key and AWS key name for this vm
func (vm *VM) SetKeyPair(privateKey string, name string) {
	vm.SSHCreds.SSHPrivateKey = privateKey
	vm.KeyPair = name
}

// ResetKeyPair resets the key pair for this VM.
func (vm *VM) ResetKeyPair() {
	vm.SSHCreds.SSHPrivateKey = ""
	vm.KeyPair = ""
}
