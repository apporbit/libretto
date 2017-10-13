// Copyright 2016 Apcera Inc. All rights reserved.

package gcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"golang.org/x/oauth2/jwt"

	googlecloud "google.golang.org/api/compute/v1"
)

var (
	// OAuth token url.
	tokenURL = "https://accounts.google.com/o/oauth2/token"
)

type googleService struct {
	vm      *VM
	service *googlecloud.Service
}

// accountFile represents the structure of the account file JSON file.
type accountFile struct {
	PrivateKey  string `json:"private_key"`
	ClientEmail string `json:"client_email"`
	ClientId    string `json:"client_id"`
}

func (vm *VM) getService() (*googleService, error) {
	var err error
	var client *http.Client

	if err = parseAccountFile(&vm.account, vm.AccountFile); err != nil {
		return nil, err
	}

	// Auth with AccountFile first if provided
	if vm.account.PrivateKey != "" {
		config := jwt.Config{
			Email:      vm.account.ClientEmail,
			PrivateKey: []byte(vm.account.PrivateKey),
			Scopes:     vm.Scopes,
			TokenURL:   tokenURL,
		}
		client = config.Client(oauth2.NoContext)
	} else {
		client = &http.Client{
			Timeout: time.Duration(30 * time.Second),
			Transport: &oauth2.Transport{
				Source: google.ComputeTokenSource(""),
			},
		}
	}

	svc, err := googlecloud.New(client)
	if err != nil {
		return nil, err
	}

	return &googleService{vm, svc}, nil
}

// get instance from current VM definition.
func (svc *googleService) getInstance() (*googlecloud.Instance, error) {
	return svc.service.Instances.Get(svc.vm.Project, svc.vm.Zone, svc.vm.Name).Do()
}

// waitForOperation pulls to wait for the operation to finish.
func waitForOperation(timeout int, funcOperation func() (*googlecloud.Operation, error)) error {
	var op *googlecloud.Operation
	var err error

	for i := 0; i < timeout; i++ {
		op, err = funcOperation()
		if err != nil {
			return err
		}

		if op.Status == "DONE" {
			if op.Error != nil {
				return fmt.Errorf("operation error: %v", *op.Error.Errors[0])
			}
			return nil
		}
		time.Sleep(1 * time.Second)
	}

	return fmt.Errorf("operation timeout, operations status: %v", op.Status)
}

// waitForOperationReady waits for the regional operation to finish.
func (svc *googleService) waitForOperationReady(operation string) error {
	return waitForOperation(OperationTimeout, func() (*googlecloud.Operation, error) {
		return svc.service.ZoneOperations.Get(svc.vm.Project, svc.vm.Zone, operation).Do()
	})
}

// waitForGlobalOperationReady waits for a global operation to finish.
func (svc *googleService) waitForGlobalOperationReady(operation string) error {
	return waitForOperation(OperationTimeout, func() (*googlecloud.Operation, error) {
		return svc.service.GlobalOperations.Get(svc.vm.Project, operation).Do()
	})
}

func (svc *googleService) getImage() (*googlecloud.Image, error) {
	for _, img := range svc.vm.ImageProjects {
		image, err := svc.service.Images.Get(img, svc.vm.SourceImage).Do()
		if err == nil && image != nil && image.SelfLink != "" {
			return image, nil
		}
		image = nil
	}

	err := fmt.Errorf("could not find image %s in these projects: %s", svc.vm.SourceImage, svc.vm.ImageProjects)
	return nil, err
}

// createDisks creates non-booted disk.
func (svc *googleService) createDisks() (disks []*googlecloud.AttachedDisk, err error) {
	if len(svc.vm.Disks) == 0 {
		return nil, errors.New("no disks were found")
	}

	image, err := svc.getImage()
	if err != nil {
		return nil, err
	}

	for i, disk := range svc.vm.Disks {
		if i == 0 {
			// First one is booted device, it will created in VM provision stage
			disks = append(disks, &googlecloud.AttachedDisk{
				Type:       "PERSISTENT",
				Mode:       "READ_WRITE",
				Kind:       "compute#attachedDisk",
				Boot:       true,
				AutoDelete: disk.AutoDelete,
				InitializeParams: &googlecloud.AttachedDiskInitializeParams{
					SourceImage: image.SelfLink,
					DiskSizeGb:  int64(disk.DiskSizeGb),
					DiskType:    fmt.Sprintf("zones/%s/diskTypes/%s", svc.vm.Zone, disk.DiskType),
				},
			})
			continue
		}

		// Reuse the existing disk, create non-booted devices if it does not exist
		searchDisk, _ := svc.getDisk(disk.Name)
		if searchDisk == nil {
			d := &googlecloud.Disk{
				Name:   disk.Name,
				SizeGb: int64(disk.DiskSizeGb),
				Type:   fmt.Sprintf("zones/%s/diskTypes/%s", svc.vm.Zone, disk.DiskType),
			}

			op, err := svc.service.Disks.Insert(svc.vm.Project, svc.vm.Zone, d).Do()
			if err != nil {
				return disks, fmt.Errorf("error while creating disk %s: %v", disk.Name, err)
			}

			err = svc.waitForOperationReady(op.Name)
			if err != nil {
				return disks, fmt.Errorf("error while waiting for the disk %s ready, error: %v", disk.Name, err)
			}
		}

		disks = append(disks, &googlecloud.AttachedDisk{
			DeviceName: disk.Name,
			Type:       "PERSISTENT",
			Mode:       "READ_WRITE",
			Boot:       false,
			AutoDelete: disk.AutoDelete,
			Source:     fmt.Sprintf("projects/%s/zones/%s/disks/%s", svc.vm.Project, svc.vm.Zone, disk.Name),
		})
	}

	return disks, nil
}

// getDisk retrieves the Disk object.
func (svc *googleService) getDisk(name string) (*googlecloud.Disk, error) {
	return svc.service.Disks.Get(svc.vm.Project, svc.vm.Zone, name).Do()
}

// deleteDisk deletes the persistent disk.
func (svc *googleService) deleteDisk(name string) error {
	op, err := svc.service.Disks.Delete(svc.vm.Project, svc.vm.Zone, name).Do()
	if err != nil {
		return err
	}

	return svc.waitForOperationReady(op.Name)
}

// deleteDisks deletes all the persistent disk.
func (svc *googleService) deleteDisks() (errs []error) {
	for _, disk := range svc.vm.Disks {
		err := svc.deleteDisk(disk.Name)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// getIPs returns the IP addresses of the GCE instance.
func (svc *googleService) getIPs() ([]net.IP, error) {
	instance, err := svc.service.Instances.Get(svc.vm.Project, svc.vm.Zone, svc.vm.Name).Do()
	if err != nil {
		return nil, err
	}

	ips := make([]net.IP, 2)
	nic := instance.NetworkInterfaces[0]

	publicIP := nic.AccessConfigs[0].NatIP
	if publicIP == "" {
		return nil, errors.New("error while retrieving public IP")
	}

	privateIP := nic.NetworkIP
	if privateIP == "" {
		return nil, errors.New("error while retrieving private IP")
	}

	ips[PublicIP] = net.ParseIP(publicIP)
	ips[PrivateIP] = net.ParseIP(privateIP)

	return ips, nil
}

// provision a new googlecloud VM instance.
func (svc *googleService) provision() error {
	zone, err := svc.service.Zones.Get(svc.vm.Project, svc.vm.Zone).Do()
	if err != nil {
		return err
	}

	machineType, err := svc.service.MachineTypes.Get(svc.vm.Project, zone.Name, svc.vm.MachineType).Do()
	if err != nil {
		return err
	}

	network, err := svc.service.Networks.Get(svc.vm.Project, svc.vm.Network).Do()
	if err != nil {
		return err
	}

	// validate network
	if !network.AutoCreateSubnetworks && len(network.Subnetworks) > 0 {
		// Network appears to be in "custom" mode, so a subnetwork is required
		// libretto doesn't handle the network creation
		if svc.vm.Subnetwork == "" {
			return fmt.Errorf("a subnetwork must be specified")
		}
	}

	subnetworkSelfLink := ""
	if svc.vm.Subnetwork != "" {
		subnetwork, err := svc.service.Subnetworks.Get(svc.vm.Project, svc.vm.region(), svc.vm.Subnetwork).Do()
		if err != nil {
			return err
		}
		subnetworkSelfLink = subnetwork.SelfLink
	}

	accessconfig := googlecloud.AccessConfig{
		Name: "External NAT for Libretto",
		Type: "ONE_TO_ONE_NAT",
	}

	md := svc.getSSHKey()

	disks, err := svc.createDisks()
	if err != nil {
		return err
	}

	instance := &googlecloud.Instance{
		Name:        svc.vm.Name,
		Description: svc.vm.Description,
		Disks:       disks,
		MachineType: machineType.SelfLink,
		Metadata: &googlecloud.Metadata{
			Items: []*googlecloud.MetadataItems{
				{
					Key:   "sshKeys",
					Value: &md,
				},
			},
		},
		NetworkInterfaces: []*googlecloud.NetworkInterface{
			{
				AccessConfigs: []*googlecloud.AccessConfig{
					&accessconfig,
				},
				Network:    network.SelfLink,
				Subnetwork: subnetworkSelfLink,
				NetworkIP:  svc.vm.PrivateIPAddress,
			},
		},
		Scheduling: &googlecloud.Scheduling{
			Preemptible: svc.vm.Preemptible,
		},
		ServiceAccounts: []*googlecloud.ServiceAccount{
			{
				Email:  "default",
				Scopes: svc.vm.Scopes,
			},
		},
		Tags: &googlecloud.Tags{
			Items: svc.vm.Tags,
		},
	}

	op, err := svc.service.Instances.Insert(svc.vm.Project, zone.Name, instance).Do()
	if err != nil {
		return err
	}

	if err = svc.waitForOperationReady(op.Name); err != nil {
		return err
	}

	_, err = svc.getInstance()
	return err
}

// start starts a stopped GCE instance.
func (svc *googleService) start() error {
	instance, err := svc.getInstance()
	if err != nil {
		if !strings.Contains(err.Error(), "no instance found") {
			return err
		}
	}

	if instance == nil {
		return errors.New("no instance found")
	}

	op, err := svc.service.Instances.Start(svc.vm.Project, svc.vm.Zone, svc.vm.Name).Do()
	if err != nil {
		return err
	}

	return svc.waitForOperationReady(op.Name)
}

// stop halts a GCE instance.
func (svc *googleService) stop() error {
	_, err := svc.getInstance()
	if err != nil {
		if !strings.Contains(err.Error(), "no instance found") {
			return err
		}
		return fmt.Errorf("no instance found, %v", err)
	}

	op, err := svc.service.Instances.Stop(svc.vm.Project, svc.vm.Zone, svc.vm.Name).Do()
	if err != nil {
		return err
	}

	return svc.waitForOperationReady(op.Name)
}

// deletes the GCE instance.
func (svc *googleService) delete() error {
	op, err := svc.service.Instances.Delete(svc.vm.Project, svc.vm.Zone, svc.vm.Name).Do()
	if err != nil {
		return err
	}

	return svc.waitForOperationReady(op.Name)
}

// extract the region from zone name.
func (vm *VM) region() string {
	return vm.Zone[:len(vm.Zone)-2]
}

func parseAccountJSON(result interface{}, jsonText string) error {
	dec := json.NewDecoder(strings.NewReader(jsonText))
	return dec.Decode(result)
}

func parseAccountFile(file *accountFile, account string) error {
	if err := parseAccountJSON(file, account); err != nil {
		if _, err = os.Stat(account); os.IsNotExist(err) {
			return fmt.Errorf("error finding account file: %s", account)
		}

		bytes, err := ioutil.ReadFile(account)
		if err != nil {
			return fmt.Errorf("error reading account file from path '%s': %s", file, err)
		}

		err = parseAccountJSON(file, string(bytes))
		if err != nil {
			return fmt.Errorf("error parsing account file: %s", err)
		}
	}

	return nil
}

func (svc *googleService) getSSHKey() string {
	return fmt.Sprintf("%s:%s\n", svc.vm.SSHCreds.SSHUser, svc.vm.SSHPublicKey)
}

func (svc *googleService) insertSSHKey() error {
	md := svc.getSSHKey()
	instance, err := svc.getInstance()
	if err != nil {
		return err
	}

	op, err := svc.service.Instances.SetMetadata(svc.vm.Project, svc.vm.Zone, svc.vm.Name, &googlecloud.Metadata{
		Fingerprint: instance.Metadata.Fingerprint,
		Items: []*googlecloud.MetadataItems{
			{
				Key:   "sshKeys",
				Value: &md,
			},
		},
	}).Do()
	if err != nil {
		return err
	}

	return svc.waitForOperationReady(op.Name)
}

// getNetworkList gets the list of networks from the given project
func (svc *googleService) getNetworkList() ([]*googlecloud.Network, error) {
	networkList, err := svc.service.Networks.List(svc.vm.Project).Do()
	if err != nil {
		return nil, err
	}

	return networkList.Items, nil
}

// getSubnetworkList gets the list of subnets for the given combination of
// project and region
func (svc *googleService) getSubnetworkList() ([]*googlecloud.Subnetwork, error) {
	subnetworkList, err := svc.service.Subnetworks.List(svc.vm.Project, svc.vm.region()).Do()
	if err != nil {
		return nil, err
	}

	return subnetworkList.Items, nil
}

// getMachineTypeList gets the list of machine types (aka flavors) for the
// given project in the given zone
func (svc *googleService) getMachineTypeList() ([]*googlecloud.MachineType, error) {
	machineTypeList, err := svc.service.MachineTypes.List(svc.vm.Project, svc.vm.Zone).Do()
	if err != nil {
		return nil, err
	}

	return machineTypeList.Items, nil
}

// getZoneList gets the list of zones
func (svc *googleService) getZoneList() ([]*googlecloud.Zone, error) {
	zoneList, err := svc.service.Zones.List(svc.vm.Project).Do()
	if err != nil {
		return nil, err
	}

	return zoneList.Items, nil
}

// getRegionList gets the list of regions
func (svc *googleService) getRegionList() ([]*googlecloud.Region, error) {
	regionList, err := svc.service.Regions.List(svc.vm.Project).Do()
	if err != nil {
		return nil, err
	}

	return regionList.Items, nil
}

// reset resets a GCE instance.
func (svc *googleService) reset() error {
	op, err := svc.service.Instances.Reset(svc.vm.Project, svc.vm.Zone, svc.vm.Name).Do()
	if err != nil {
		return err
	}

	return svc.waitForOperationReady(op.Name)
}

// getDiskTypeList gets the list of disk types for the given project in the
// given zone
func (svc *googleService) getDiskTypeList() ([]*googlecloud.DiskType, error) {
	diskTypeList, err := svc.service.DiskTypes.List(svc.vm.Project, svc.vm.Zone).Do()
	if err != nil {
		return nil, err
	}

	return diskTypeList.Items, nil
}

// getDiskType gets details of a disk type
func (svc *googleService) getDiskType(diskType string) (*googlecloud.DiskType, error) {
	return svc.service.DiskTypes.Get(svc.vm.Project, svc.vm.Zone, diskType).Do()
}

// createDisk creates a new disk in GCE
func (svc *googleService) createDisk(disk *Disk) error {
	diskType, err := svc.getDiskType(disk.DiskType)
	if err != nil {
		return err
	}

	gDisk := &googlecloud.Disk{
		Name:        disk.Name,
		SizeGb:      int64(disk.DiskSizeGb),
		Type:        diskType.SelfLink,
		Description: disk.Description,
	}

	op, err := svc.service.Disks.Insert(svc.vm.Project, svc.vm.Zone, gDisk).Do()
	if err != nil {
		return err
	}
	return svc.waitForOperationReady(op.Name)
}

// attachDisk attaches a disk to an instance
func (svc *googleService) attachDisk(disk *Disk) error {

	gDisk, err := svc.getDisk(disk.Name)
	if err != nil {
		return err
	}

	gAttachedDisk := &googlecloud.AttachedDisk{
		Source:     gDisk.SelfLink,
		DeviceName: disk.Name,
		AutoDelete: disk.AutoDelete,
	}

	op, err := svc.service.Instances.AttachDisk(svc.vm.Project, svc.vm.Zone,
		svc.vm.Name, gAttachedDisk).Do()

	if err != nil {
		return err
	}
	return svc.waitForOperationReady(op.Name)
}

// detachDisk detaches a disk from an instance
func (svc *googleService) detachDisk(disk *Disk) error {

	var deviceName string

	gDisk, err := svc.getDisk(disk.Name)
	if err != nil {
		return err
	}
	instance, err := svc.getInstance()
	if err != nil {
		return err
	}

	for _, attachedDisk := range instance.Disks {
		if attachedDisk.Source == gDisk.SelfLink {
			deviceName = attachedDisk.DeviceName
			break
		}
	}

	op, err := svc.service.Instances.DetachDisk(svc.vm.Project, svc.vm.Zone,
		svc.vm.Name, deviceName).Do()

	if err != nil {
		return err
	}
	return svc.waitForOperationReady(op.Name)

}

// getFirewall gets details of a firewall
func (svc *googleService) getFirewall() (*googlecloud.Firewall, error) {
	return svc.service.Firewalls.Get(svc.vm.Project, svc.vm.Firewall).Do()
}

// addFirewallRules adds new ports to a firewall
func (svc *googleService) addFirewallRules() error {

	currFirewall, err := svc.getFirewall()
	if err != nil {
		return err
	}
	newRules := currFirewall.Allowed
	for _, endpoint := range svc.vm.Endpoints {
		newRules = append(newRules, &googlecloud.FirewallAllowed{
			IPProtocol: endpoint.Protocol,
			Ports:      endpoint.Ports,
		})
	}

	return svc.patchFirewall(newRules)
}

// patchFirewall patches the firewall with the given allowed ports (rules)
func (svc *googleService) patchFirewall(allowed []*googlecloud.FirewallAllowed) error {
	newFirewall := &googlecloud.Firewall{
		Allowed: allowed,
	}
	op, err := svc.service.Firewalls.Patch(svc.vm.Project, svc.vm.Firewall,
		newFirewall).Do()
	if err != nil {
		return err
	}
	return svc.waitForGlobalOperationReady(op.Name)
}

// removeFirewallRules removes ports from a given firewall
func (svc *googleService) removeFirewallRules() error {
	// Define a map to identify ports to be removed. It will record the
	// ports with corresponding protocol as remPorts[Protocol][Port] = true
	// for each protocol and port combination.
	remPorts := make(map[string]map[string]bool)
	// Define a map to deal with elements where the same protocol
	// appears again
	encountered := make(map[string]bool)
	// Identify ports for each protocol to be removed
	for _, endpoint := range svc.vm.Endpoints {
		// Initialize the map only if the given protocol is encountered
		// for the first time
		if ok := encountered[endpoint.Protocol]; !ok {
			remPorts[endpoint.Protocol] = make(map[string]bool)
			encountered[endpoint.Protocol] = true
		}
		for _, port := range endpoint.Ports {
			remPorts[endpoint.Protocol][port] = true
		}
	}

	// Initialize new rules with the current one
	// Refer to this link for structure of firewall resource
	// https://cloud.google.com/compute/docs/reference/latest/firewalls
	firewall, err := svc.getFirewall()
	if err != nil {
		return err
	}
	newRules := firewall.Allowed

	// Remove the ports and protocols that are to be removed from the
	// rules. Iterate through each item in the list of rules.
	for indexEndp := 0; indexEndp < len(newRules); indexEndp++ {
		allowed := newRules[indexEndp]
		// Iterate through each port in the port list
		for indPort := 0; indPort < len(newRules[indexEndp].Ports); indPort++ {
			port := newRules[indexEndp].Ports[indPort]
			// Process removal if the given port is to be removed
			if ok := remPorts[allowed.IPProtocol][port]; ok {
				newRules[indexEndp].Ports[indPort] = ""
				newRules[indexEndp].Ports = append(
					newRules[indexEndp].Ports[:indPort],
					newRules[indexEndp].Ports[indPort+1:]...
				)
				// Set the index back by one because we have
				// shifted elements from indP onwards to left
				// by one
				indPort = indPort - 1
			}
		}
		// At the end of removal of ports for a particular protocol if
		// there are no ports left, then remove that protocol as well.
		if len(newRules[indexEndp].Ports) == 0 {
			newRules[indexEndp] = nil
			newRules = append(newRules[:indexEndp], newRules[indexEndp+1:]...)
			// Set the index back by one because we have shifted
			// elements from indP onwards to left by one
			indexEndp = indexEndp - 1
		}
	}
	return svc.patchFirewall(newRules)
}

// getImageList gets a list of available images in the given list of projects
func (svc *googleService) getImageList() ([]*googlecloud.Image, error) {
	imageListAll := make([]*googlecloud.Image, 0)
	for _, project := range svc.vm.ImageProjects {
		imageList, err := svc.service.Images.List(project).Do()
		if err != nil {
			return nil, err
		}
		imageListAll = append(imageListAll, imageList.Items...)
	}

	return imageListAll, nil
}

// convResURLToName returns resource name from the given resource URL
func convResURLToName(url string) string {
	urlSplit := strings.Split(url, "/")
	return urlSplit[len(urlSplit)-1]
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
