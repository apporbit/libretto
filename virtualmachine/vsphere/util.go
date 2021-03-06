// Copyright 2015 Apcera Inc. All rights reserved.

package vsphere

import (
	"archive/tar"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/apcera/libretto/util"
	lvm "github.com/apcera/libretto/virtualmachine"
)

const (
	RETRY_COUNT                = 20
	GRAY_STATUS_CHECK_TIMEOUT  = 1 * time.Minute
	GREEN_STATUS_CHECK_TIMEOUT = 10 * time.Minute
	IPWAIT_TIMEOUT             = 1 * time.Hour
)

const (
	RESOURCE_POOL_DEPTH = 8
)

/*
 * The guest heartbeat. The heartbeat status is classified as:
 * gray - VMware Tools are not installed or not running.
 * red - No heartbeat. Guest operating system may have stopped responding.
 * yellow - Intermittent heartbeat. May be due to guest load.
 * green - Guest operating system is responding normally.
 */
const (
	GUEST_HEART_BEAT_STATUS = "guestHeartbeatStatus"
	GREEN_HEART_BEAT        = 1 << iota
	YELLOW_HEART_BEAT
	GRAY_HEART_BEAT
	RED_HEART_BEAT
)

func StringInSlice(str string, list []string) bool {
	for _, v := range list {
		if v == str {
			return true
		}
	}
	return false
}

// VMSearchFilter struct encapsulates all relevant search parameters
type VMSearchFilter struct {
	Name         string
	InstanceUuid string
	SearchInDC   bool
}

// getVMSearchFilter: returns VMSearchFilter object for given vm
// by default vm is searched within DC
func getVMSearchFilter(vmName string) VMSearchFilter {
	searchFilter := VMSearchFilter{
		Name:       vmName,
		SearchInDC: true,
	}
	return searchFilter
}

// getTempSearchFilter: returns VMSearchFilter object for given template
// by default template is searched in entire inventory (across dc)
func getTempSearchFilter(template Template) VMSearchFilter {
	searchFilter := VMSearchFilter{
		Name:         template.Name,
		InstanceUuid: template.InstanceUuid,
		SearchInDC:   false,
	}
	return searchFilter
}

// mutex for custom spec creation
var checkCustomSpecMutex sync.Mutex

// Exists checks if the VM already exists.
var Exists = func(vm *VM, searchFilter VMSearchFilter) (bool, error) {
	_, err := findVM(vm, searchFilter)
	if err != nil {
		if _, ok := err.(ErrorObjectNotFound); ok {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

var getURI = func(host string) string {
	return fmt.Sprintf("https://%s/sdk", host)
}

var newClient = func(vm *VM) (*govmomi.Client, error) {
	return govmomi.NewClient(vm.ctx, vm.uri, vm.Insecure)
}

var newFinder = func(c *vim25.Client) finder {
	return vmwareFinder{find.NewFinder(c, true)}
}

var newCollector = func(c *vim25.Client) *property.Collector {
	return property.DefaultCollector(c)
}

// SetupSession is used to setup the session.
var SetupSession = func(vm *VM) error {
	uri := getURI(vm.Host)
	u, err := url.Parse(uri)
	if err != nil || u.String() == "" {
		return NewErrorParsingURL(uri, err)
	}
	u.User = url.UserPassword(vm.Username, vm.Password)
	vm.uri = u
	vm.ctx, vm.cancel = context.WithCancel(context.Background())
	client, err := newClient(vm)
	if err != nil {
		return NewErrorClientFailed(err)
	}

	vm.client = client
	vm.finder = newFinder(vm.client.Client)
	vm.collector = newCollector(vm.client.Client)
	return nil
}

// GetDatacenter retrieves the datacenter that the provisioner was configured
// against.
func GetDatacenter(vm *VM) (*mo.Datacenter, error) {
	dcList, err := vm.finder.DatacenterList(vm.ctx, "*")
	if err != nil {
		return nil, NewErrorObjectNotFound(err, vm.Datacenter)
	}
	for _, dc := range dcList {
		dcMo := mo.Datacenter{}
		ps := []string{"name", "hostFolder", "vmFolder", "datastore", "network"}
		err := vm.collector.RetrieveOne(vm.ctx, dc.Reference(), ps, &dcMo)
		if err != nil {
			return nil, NewErrorPropertyRetrieval(dc.Reference(), ps, err)
		}
		if dcMo.Name == vm.Datacenter {
			return &dcMo, err
		}
	}
	return nil, NewErrorObjectNotFound(err, vm.Datacenter)
}

var open = func(name string) (file *os.File, err error) {
	return os.Open(name)
}

var readAll = func(r io.Reader) ([]byte, error) {
	return ioutil.ReadAll(r)
}

//Extract the tar pointed by 'body' to 'basePath' directory
var extractOva = func(basePath string, body io.Reader) (string, error) {
	tarBallReader := tar.NewReader(body)
	var ovfFileName string

	//iterates through the files in .ova file[tar file]
	for {
		header, err := tarBallReader.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			return "", err
		}
		filename := header.Name
		if filepath.Ext(filename) == ".ovf" {
			ovfFileName = filename
		}
		// writes the content of the file pointed to by tarBallReader to a local file with same name
		err = func() error {
			writer, err := os.Create(filepath.Join(basePath, filename))
			defer writer.Close()
			if err != nil {
				return err
			}
			_, err = io.Copy(writer, tarBallReader)
			if err != nil {
				return err
			}
			return nil
		}()
		if err != nil {
			return "", err
		}
	}
	if ovfFileName == "" {
		return "", errors.New("no ovf file found in the archive")
	}
	return filepath.Join(basePath, ovfFileName), nil
}

// Downloads the ova file from the 'url' (can be local path/remote http server) to 'basePath' directory
// and returns the path to extracted ovf file
var downloadOva = func(basePath, url string) (string, error) {
	var ovaReader io.Reader
	// if url is a remote url
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		resp, err := http.Get(url)
		if err != nil {
			return "", err
		}
		ovaReader = resp.Body
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("can't download ova file from url: %s %s %d", url, "status: ", resp.StatusCode)
		}
		defer resp.Body.Close()
	} else {
		resp, err := os.Open(url)
		if err != nil {
			return "", err
		}
		ovaReader = resp
		defer resp.Close()
	}
	ovfFilePath, err := extractOva(basePath, ovaReader)
	if err != nil {
		return "", err
	}
	return ovfFilePath, nil
}

var parseOvf = func(ovfLocation string) (string, error) {
	ovf, err := open(ovfLocation)
	if err != nil {
		return "", fmt.Errorf("Failed to open the ovf file: %v", err)
	}

	ovfContent, err := readAll(ovf)
	if err != nil {
		return "", fmt.Errorf("Failed to open the ovf file: %v", err)
	}
	return string(ovfContent), nil
}

// findComputeResource takes a data center and finds a compute resource on which the name
// property matches the one passed in. Assumes that the dc has the hostfolder property populated.
var findComputeResource = func(vm *VM, dc *mo.Datacenter, name string) (*mo.ComputeResource, error) {
	mor, err := findMob(vm, dc.HostFolder, name)
	if err != nil {
		return nil, err
	}
	cr := mo.ComputeResource{}
	err = vm.collector.RetrieveOne(vm.ctx, *mor, []string{"name", "host", "resourcePool", "datastore", "network"}, &cr)
	if err != nil {
		return nil, err
	}
	return &cr, nil
}

// findClusterComputeResource takes a data center and finds a compute resource on which the name
// property matches the one passed in. Assumes that the dc has the hostfolder property populated.
var findClusterComputeResource = func(vm *VM, dc *mo.Datacenter, name string) (*mo.ClusterComputeResource, error) {
	mor, err := findMob(vm, dc.HostFolder, name)
	if err != nil {
		return nil, err
	}
	cr := mo.ClusterComputeResource{}
	err = vm.collector.RetrieveOne(vm.ctx, *mor, []string{"name",
		"configuration", "host", "resourcePool", "datastore",
		"network"}, &cr)
	if err != nil {
		return nil, err
	}
	return &cr, nil
}

// findDatastore finds a datastore in the given dc
var findDatastore = func(vm *VM, dc *mo.Datacenter, name string) (*mo.Datastore, error) {
	for _, dsMor := range dc.Datastore {
		dsMo := mo.Datastore{}
		ps := []string{"name"}
		err := vm.collector.RetrieveOne(vm.ctx, dsMor, ps, &dsMo)
		if err != nil {

			return nil, NewErrorPropertyRetrieval(dsMor, ps, err)
		}
		if dsMo.Name == name {
			return &dsMo, nil
		}
	}
	return nil, NewErrorObjectNotFound(errors.New("datastore not found"), name)
}

// findHostSystem finds a host system within a slice of mors to hostsystems
var findHostSystem = func(vm *VM, hsMors []types.ManagedObjectReference, name string) (*mo.HostSystem, error) {
	for _, hsMor := range hsMors {
		hsMo := mo.HostSystem{}
		ps := []string{"name"}
		err := vm.collector.RetrieveOne(vm.ctx, hsMor, ps, &hsMo)
		if err != nil {
			return nil, NewErrorPropertyRetrieval(hsMor, ps, err)
		}
		if hsMo.Name == name {
			return &hsMo, nil
		}
	}
	return nil, NewErrorObjectNotFound(errors.New("host system not found"), name)
}

var findMob func(*VM, types.ManagedObjectReference, string) (*types.ManagedObjectReference, error)

var createNetworkMapping = func(vm *VM, networks []Network,
	networkMors []types.ManagedObjectReference) ([]types.OvfNetworkMapping,
	map[string]types.ManagedObjectReference, error) {
	nwMap := map[string]types.ManagedObjectReference{}
	// Create a map between network name and mor for lookup
	for _, network := range networkMors {
		name, err := getNetworkName(vm, network)
		if err != nil {
			return nil, nil, err
		}
		if name == "" {
			return nil, nil, fmt.Errorf("Network name empty for: %s", network.Value)
		}
		nwMap[name] = network
	}

	var mappings []types.OvfNetworkMapping
	for _, mapping := range networks {
		nwName := mapping.Name
		mor, ok := nwMap[nwName]
		if !ok {
			return nil, nwMap, NewErrorObjectNotFound(errors.New("Could not find the network mapping"), nwName)
		}
		mappings = append(mappings, types.OvfNetworkMapping{Name: nwName, Network: mor})
	}
	return mappings, nwMap, nil
}

var resetUnitNumbers = func(spec *types.OvfCreateImportSpecResult) {
	s := &spec.ImportSpec.(*types.VirtualMachineImportSpec).ConfigSpec
	for _, d := range s.DeviceChange {
		n := d.GetVirtualDeviceConfigSpec().Device.GetVirtualDevice().UnitNumber
		if n != nil && *n == 0 {
			*n = -1
		}
	}
}

var uploadOvf = func(vm *VM, specResult *types.OvfCreateImportSpecResult, lease Lease) error {
	// Ask the server to wait on the NFC lease
	leaseInfo, err := lease.Wait()
	if err != nil {
		return fmt.Errorf("error waiting on the nfc lease: %v", err)
	}

	//FIXME (Preet): Hard coded to just upload the first device.
	url := leaseInfo.DeviceUrl[0].Url
	if strings.Contains(url, "*") {
		url = strings.Replace(url, "*", vm.Host, 1)
	}

	path := specResult.FileItem[0].Path
	if !filepath.IsAbs(path) {
		// If the path is not abs, convert it into an ABS path relative to the OVF file
		dir := filepath.Dir(vm.OvfPath)
		path = filepath.Join(dir, path)
	}
	file, err := open(path)
	if err != nil {
		return err
	}
	info, _ := file.Stat()
	totalBytes := info.Size()
	reader := NewProgressReader(file, totalBytes, lease)
	reader.StartProgress()
	err = createRequest(reader, "POST", vm.Insecure, totalBytes, url, "application/x-vnd.vmware-streamVmdk")
	if err != nil {
		return err
	}
	reader.Wait()
	return nil
}

var clientDo = func(c *http.Client, r *http.Request) (*http.Response, error) {
	return c.Do(r)
}

var createRequest = func(r io.Reader, method string, insecure bool, length int64, url string, contentType string) error {
	request, _ := http.NewRequest(method, url, r)
	request.Header.Add("Connection", "Keep-Alive")
	request.Header.Add("Content-Type", contentType)
	request.Header.Add("Content-Length", fmt.Sprintf("%d", length))
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure},
	}
	client := &http.Client{
		Transport: tr,
	}
	resp, err := clientDo(client, request)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusCreated {
		return NewErrorBadResponse(resp)
	}
	return nil
}

// findVM finds the vm Managed Object referenced by the name/instanceUuid
// or returns an error if it is not found.
var findVM = func(vm *VM, searchFilter VMSearchFilter) (*mo.VirtualMachine,
	error) {
	var (
		moVM *mo.VirtualMachine
		dc   *mo.Datacenter
		err  error
	)

	if searchFilter.InstanceUuid != "" {
		moVM, err = searchVmByUuid(vm, searchFilter)
	} else {
		dc, err = GetDatacenter(vm)
		if err != nil {
			return nil, err
		}
		moVM, err = searchTree(vm, &dc.VmFolder, searchFilter.Name)
	}
	if err != nil {
		return moVM, err
	}
	// Having a question pending during operations usually cause errors forcing
	// manual resolution. Anytime we look up a VM try first to resolve any
	// questions that we know how to answer.
	return moVM, vm.answerQuestion(moVM)
}

// addCustomField: adds custom field for given field name
func addCustomField(vm *VM, fieldName string, moType string) (int32, error) {
	fieldsManager, err := object.GetCustomFieldsManager(vm.client.Client)
	if err != nil {
		return -1, err
	}

	key, err := fieldsManager.FindKey(vm.ctx, fieldName)
	if err != nil {
		if err == object.ErrKeyNameNotFound {
			field, err := fieldsManager.Add(vm.ctx, fieldName, moType,
				nil, nil)
			if err != nil {
				return -1, err
			}
			key = field.Key
		} else {
			return -1, err
		}
	}

	return key, nil
}

// setCustomField: sets custom field and value for given vm/template
func setCustomField(vm *VM, vmMo *mo.VirtualMachine, fieldName string,
	moType string, fieldValue string) error {
	fieldsManager, err := object.GetCustomFieldsManager(vm.client.Client)
	if err != nil {
		return err
	}

	key, err := addCustomField(vm, fieldName, moType)
	if err != nil {
		return err
	}

	err = fieldsManager.Set(vm.ctx, vmMo.Reference(), key, fieldValue)
	if err != nil {
		return err
	}

	return nil
}

// splitPathToList: splits path containing special character including delimiter
// for slash "/" (\/) and return slice of strings containing folder/vm names
// for eg:  "vms\/test\/rec/rec\/1/rhel\/template\/vm"	: [vms/test/rec rec/1 rhel/template/vm]
// Directory structure: vm/test/rec -> rec/1 -> rhel/template/vm
func splitPathToList(path string) []string {
	pathList := make([]string, 0)

	// split at escaped '/'
	// if no escaped '/' are present length of returned slice will be 1
	slashInName := strings.SplitN(path, "\\/", 2)

	// split at '/' (path separator) and append to pathList to return
	pathList = append(pathList, strings.Split(slashInName[0], "/")...)

	// if there are no escaped '/'
	if len(slashInName) == 1 {
		return pathList
	}

	// look for more escaped '/' in 2nd part of string
	morePathList := splitPathToList(slashInName[1])
	lPathList := len(pathList)

	// join the path splitted at escaped '/'
	pathList[lPathList-1] += "/" + morePathList[0]
	pathList = append(pathList, morePathList[1:]...)
	return pathList
}

// searchTree: searches for vm/template at a given path
func searchTree(vm *VM, mor *types.ManagedObjectReference, name string) (
	*mo.VirtualMachine, error) {
	var (
		ref types.ManagedObjectReference
	)

	// splits path to list of folder and vm names
	pathList := splitPathToList(name)
	lPathList := len(pathList)

	iPathList := 0
	for mor != nil {
		// Fetch the childEntity property of the folder and check them
		folderMo := mo.Folder{}
		err := vm.collector.RetrieveOne(vm.ctx, *mor, []string{
			"childEntity"}, &folderMo)
		if err != nil {
			return nil, err
		}

		mor = nil
		for _, child := range folderMo.ChildEntity {
			switch child.Type {
			case "Folder":
				// skip if looking for vm/template
				if iPathList == lPathList-1 {
					continue
				}
				childMo := mo.Folder{}
				err = vm.collector.RetrieveOne(vm.ctx, child,
					[]string{"name"}, &childMo)
				if err != nil {
					if isObjectDeleted(err) {
						continue
					}
					return nil, err
				}

				// unescaping to convert any escaped character
				childName, err := url.QueryUnescape(
					childMo.Name)
				if err != nil {
					return nil, err
				}
				if childName == pathList[iPathList] {
					iPathList++
					ref = child
					mor = &ref
					break
				}
			case "VirtualMachine":
				// skip if looking for folder
				if iPathList != lPathList-1 {
					continue
				}
				// Base recursive case, compare for value
				vmMo := mo.VirtualMachine{}
				err := vm.collector.RetrieveOne(vm.ctx, child,
					[]string{"name", "config", "datastore",
						"guest",
						"snapshot.currentSnapshot",
						"summary", "runtime", "resourcePool"}, &vmMo)
				if err != nil {
					if isObjectDeleted(err) {
						continue
					}
					return nil, err
				}
				// unescaping to convert any escaped character
				vmName, err := url.QueryUnescape(vmMo.Name)
				if err != nil {
					return nil, err
				}
				if vmName == pathList[iPathList] {
					return &vmMo, nil
				}
			}
		}

		if mor == nil {
			return nil, NewErrorObjectNotFound(errors.New(
				"could not find the vm"), name)
		}
	}
	return nil, NewErrorObjectNotFound(errors.New("could not find the vm"),
		name)
}

// searchVmByUuid: searches vm with uuid: instanceUuid in datacenter
// or entire inventory
func searchVmByUuid(vm *VM, searchFilter VMSearchFilter) (
	*mo.VirtualMachine, error) {
	var (
		dcObj *object.Datacenter
		obj   object.Reference
		err   error
		dcMo  *mo.Datacenter
	)
	vmMo := mo.VirtualMachine{}
	s := object.NewSearchIndex(vm.client.Client)
	isInstanceUuid := new(bool)
	*isInstanceUuid = true

	dcObj = nil

	if searchFilter.SearchInDC {
		dcMo, err = GetDatacenter(vm)
		if err != nil {
			return nil, err
		}
		dcObj = object.NewDatacenter(vm.client.Client, dcMo.Self)
	}

	obj, err = s.FindByUuid(vm.ctx, dcObj, searchFilter.InstanceUuid, true,
		isInstanceUuid)
	if err != nil {
		return nil, err
	}

	vmObj, ok := obj.(*object.VirtualMachine)
	if !ok {
		return nil, NewErrorObjectNotFound(errors.New(
			"Invalid object with uuid found"), searchFilter.InstanceUuid)
	}
	err = vm.collector.RetrieveOne(vm.ctx, vmObj.Reference(), []string{
		"name", "config", "runtime", "summary", "guest",
		"resourcePool"}, &vmMo)
	if err != nil {
		return nil, err
	}
	return &vmMo, nil
}

func getEthernetBacking(vm *VM, nwMor types.ManagedObjectReference,
	name string) (types.BaseVirtualDeviceBackingInfo, error) {
	var (
		backing types.BaseVirtualDeviceBackingInfo
		err     error
	)
	switch nwMor.Type {
	case "Network":
		backing = &types.VirtualEthernetCardNetworkBackingInfo{
			VirtualDeviceDeviceBackingInfo: types.VirtualDeviceDeviceBackingInfo{
				DeviceName: name,
			},
			Network: &nwMor,
		}
	case "DistributedVirtualPortgroup":
		dvsObj := object.NewDistributedVirtualPortgroup(
			vm.client.Client, nwMor)
		backing, err =
			dvsObj.EthernetCardBackingInfo(
				vm.ctx)
		if err != nil {
			return nil, fmt.Errorf("error fetching ethernet card "+
				"backing info: %v", err)
		}
	}
	return backing, nil
}

// createNetworkDeviceSpec : createNetworkDeviceSpec creates the device spec for the network nwMor
func addNetworkDeviceSpec(vm *VM, nwMor types.ManagedObjectReference, name string) (*types.VirtualDeviceConfigSpec, error) {
	// create backing object
	backing, err := getEthernetBacking(vm, nwMor, name)
	if err != nil {
		return nil, err
	}
	// create ethernet card with the backing info
	device, err := object.EthernetCardTypes().CreateEthernetCard("vmxnet3", backing)
	if err != nil {
		return nil, err
	}
	// connect to the network when the nic is connected to vm
	device.GetVirtualDevice().Connectable = &types.VirtualDeviceConnectInfo{
		StartConnected:    true,
		AllowGuestControl: true,
	}
	// create spec to add the device to the vm
	spec := &types.VirtualDeviceConfigSpec{
		Operation: types.VirtualDeviceConfigSpecOperationAdd,
		Device:    device,
	}

	return spec, nil
}

// reconfigureNetworks : reconfigureNetworks configures the vm and attach it to the
// networks in the vm structure
func reconfigureNetworks(vm *VM, vmObj *object.VirtualMachine) ([]types.BaseVirtualDeviceConfigSpec, error) {
	var (
		deviceSpecs []types.BaseVirtualDeviceConfigSpec
		nw          Network
	)
	dcMo, err := GetDatacenter(vm)
	if err != nil {
		return nil, err
	}

	l, err := getVMLocation(vm, dcMo)
	if err != nil {
		return nil, err
	}
	// Create mapping of network managed object and network name
	networkMapping, _, err := createNetworkMapping(vm, vm.Networks, l.Networks)
	if err != nil {
		return nil, err
	}

	devices, err := vmObj.Device(vm.ctx)
	if err != nil {
		return nil, err
	}

	idx := 0
	// Modify existing networks in template with provided networks list
	for _, device := range devices {
		switch device.(type) {
		case *types.VirtualE1000, *types.VirtualE1000e, *types.VirtualVmxnet3:
			if idx >= len(vm.Networks) {
				// Remove extra networks
				spec := &types.VirtualDeviceConfigSpec{
					Operation: types.VirtualDeviceConfigSpecOperationRemove,
					Device:    device,
				}
				deviceSpecs = append(deviceSpecs, spec)
				continue
			}

			// Edit device
			nw = vm.Networks[idx]
			for _, nwMappingObj := range networkMapping {
				if nwMappingObj.Name != nw.Name {
					continue
				}
				backing, err := getEthernetBacking(vm,
					nwMappingObj.Network, nwMappingObj.Name)
				if err != nil {
					return nil, err
				}
				device.GetVirtualDevice().Backing = backing
				spec := &types.VirtualDeviceConfigSpec{
					Operation: types.VirtualDeviceConfigSpecOperationEdit,
					Device:    device,
				}
				deviceSpecs = append(deviceSpecs, spec)
				break
			}
			idx++
		}
	}

	// Add extra networks if any
	for _, nw = range vm.Networks[idx:] {
		for _, mapping := range networkMapping {
			if mapping.Name == nw.Name {
				spec, err := addNetworkDeviceSpec(vm, mapping.Network, mapping.Name)
				if err != nil {
					return nil, err
				}
				// add spec to array of the devices to be added/removed
				deviceSpecs = append(deviceSpecs, spec)
				break
			}
		}
	}
	return deviceSpecs, nil
}

// Function to search disk in disks array with its name
func findByVirtualDeviceFileName(disks []Disk, name string) *Disk {
	for _, disk := range disks {
		if disk.DiskFile == name {
			return &disk
		}
	}
	return nil
}

// Function which will resize or delete the existing volume in vmware template
func resizeAndDeleteVols(vmMo mo.VirtualMachine, disks []Disk) ([]types.BaseVirtualDeviceConfigSpec, error) {
	var deviceSpecs []types.BaseVirtualDeviceConfigSpec
	devices := object.VirtualDeviceList(vmMo.Config.Hardware.Device)
	for _, device := range devices {
		if editdisk, ok := device.(*types.VirtualDisk); ok {
			backing := editdisk.Backing
			fileBackingInfo := backing.(types.BaseVirtualDeviceFileBackingInfo).GetVirtualDeviceFileBackingInfo()
			disk := findByVirtualDeviceFileName(disks, fileBackingInfo.FileName)
			var dvconfig types.BaseVirtualDeviceConfigSpec
			if disk == nil {
				// If user wants to delete the disk
				dvconfig = &types.VirtualDeviceConfigSpec{
					Operation: types.VirtualDeviceConfigSpecOperationRemove,
					Device:    editdisk,
				}

			} else {
				capacityInKB := int64(disk.Size * 1024 * 1024)
				if editdisk.CapacityInKB > capacityInKB {
					// If user wants to shrink the disk capacity
					return nil, fmt.Errorf("error : Shrinking Virtual Disks is not supported")
				} else if editdisk.CapacityInKB < capacityInKB {
					// If user wants to expand the virtual disk capacity
					editdisk.CapacityInKB = capacityInKB
					dvconfig = &types.VirtualDeviceConfigSpec{
						Device:    editdisk,
						Operation: types.VirtualDeviceConfigSpecOperationEdit,
					}
				}
			}
			deviceSpecs = append(deviceSpecs, dvconfig)
		}

	}
	return deviceSpecs, nil
}

func findResourcePoolListAtPath(vm *VM, path string, properties []string) ([]mo.ResourcePool, error) {
	var (
		rpMor   []types.ManagedObjectReference
		allRpMo []mo.ResourcePool
	)
	// get resource pool list in the vcenter server
	allRps, err := vm.finder.ResourcePoolList(vm.ctx, path)
	if err != nil {
		return nil, err
	}

	for _, rp := range allRps {
		rpMor = append(rpMor, rp.Reference())
	}

	if len(rpMor) == 0 {
		return allRpMo, nil
	}
	err = vm.collector.Retrieve(vm.ctx, rpMor, properties, &allRpMo)
	if err != nil {
		return nil, err
	}

	return allRpMo, nil
}

func findResourcePoolByMOID(vm *VM, moid string) (*mo.ResourcePool, error) {
	// get resource pool list in the vcenter server
	rpListPath := "*/Resources"
	for n := 0; n < RESOURCE_POOL_DEPTH; n++ {
		rpListPath = rpListPath + "/*"
		prop := []string{"name", "owner"}
		rpList, _ := findResourcePoolListAtPath(vm, rpListPath, prop)
		for _, rp := range rpList {
			if moid == rp.Self.Value {
				return &rp, nil
			}
		}
	}
	return nil, NewErrorObjectNotFound(errors.New("could not find the resourcepool with moref id"),
		moid)
}

var cloneFromTemplate = func(vm *VM, dcMo *mo.Datacenter, usableDatastores []string) error {
	var (
		err   error
		dsMo  *mo.Datastore
		dsMor types.ManagedObjectReference
	)
	vm.datastore = util.ChooseRandomString(usableDatastores)
	if vm.datastore != "" {
		dsMo, err = findDatastore(vm, dcMo, vm.datastore)
		if err != nil {
			return err
		}
		dsMor = dsMo.Reference()
	}
	if vm.UseLocalTemplates {
		vm.Template.Name = createTemplateName(vm.Template.Name, vm.datastore)
	}
	vmMo, err := findVM(vm, getTempSearchFilter(vm.Template))
	if err != nil {
		return fmt.Errorf("error retrieving template: %v", err)
	}
	vmObj := object.NewVirtualMachine(vm.client.Client, vmMo.Reference())

	l, err := getVMLocation(vm, dcMo)
	if err != nil {
		return err
	}

	// TODO: If the network needs to be reconfigured as well then this needs
	// to delete all the network cards and create VirtualDevice specs.
	// For now only configure the datastore and the host.
	relocateSpec := types.VirtualMachineRelocateSpec{
		Pool: &l.ResourcePool,
	}

	if vm.Destination.DestinationType == DestinationTypeCluster {
		isDrsEnabled, err := IsClusterDrsEnabled(vm)
		if err != nil {
			return err
		}
		if vm.Destination.HostSystem != "" || !isDrsEnabled {
			relocateSpec.Host = &l.Host
		}
	}
	if dsMo != nil {
		relocateSpec.Datastore = &dsMor
	}

	deviceChangeSpec, err := reconfigureNetworks(vm, vmObj)
	if err != nil {
		return err
	}
	hotAddMemory := true
	hotAddCpu := true

	if vm.Flavor.NumCPUs <= 0 {
		vm.Flavor.NumCPUs = vmMo.Config.Hardware.NumCPU
	}

	if vm.Flavor.MemoryMB <= 0 {
		vm.Flavor.MemoryMB = int64(vmMo.Config.Hardware.MemoryMB)
	}

	config := types.VirtualMachineConfigSpec{
		NumCPUs:             vm.Flavor.NumCPUs,
		MemoryMB:            vm.Flavor.MemoryMB,
		MemoryHotAddEnabled: &hotAddMemory,
		CpuHotAddEnabled:    &hotAddCpu,
		NestedHVEnabled:     &vm.NestedHV,
	}
	config.DeviceChange = deviceChangeSpec

	if len(vm.FixedDisks) != 0 {
		// Resize (increase)/delete existing volumes in VM template
		conf, err := resizeAndDeleteVols(*vmMo, vm.FixedDisks)
		if err != nil {
			return err
		}
		config.DeviceChange = append(config.DeviceChange, conf...)
	}

	checkCustomSpecMutex.Lock()
	// Critical section - Only one thread should create custom spec
	// if not present
	err = checkAndCreateCustomSpec(vm)
	if err != nil {
		checkCustomSpecMutex.Unlock()
		return fmt.Errorf("Error creating custom spec: %v", err)
	}

	customizationSpecManager := object.NewCustomizationSpecManager(
		vm.client.Client)
	customSpecItem, err := customizationSpecManager.GetCustomizationSpec(
		vm.ctx, STATICIP_CUSTOM_SPEC_NAME)
	if err != nil {
		checkCustomSpecMutex.Unlock()
		return fmt.Errorf("Error retrieving custom spec: %v", err)
	}
	customSpec := updateCustomSpec(vm, vmMo, &customSpecItem.Spec)
	checkCustomSpecMutex.Unlock()

	cisp := types.VirtualMachineCloneSpec{
		Location:      relocateSpec,
		Template:      false,
		PowerOn:       false,
		Config:        &config,
		Customization: customSpec,
	}

	// To create a linked clone, we need to set the DiskMoveType and reference
	// the snapshot of the VM we are cloning.
	if vm.UseLinkedClones {
		relocateSpec = types.VirtualMachineRelocateSpec{
			Pool:         &l.ResourcePool,
			DiskMoveType: "createNewChildDiskBacking",
		}
		if vm.Destination.HostSystem != "" {
			relocateSpec.Host = &l.Host
		}
		if dsMo != nil {
			relocateSpec.Datastore = &dsMor
		}
		cisp = types.VirtualMachineCloneSpec{
			Location: relocateSpec,
			Template: false,
			PowerOn:  false,
			Config:   &config,
			Snapshot: vmMo.Snapshot.CurrentSnapshot,
		}
	}

	folderObj := object.NewFolder(vm.client.Client, dcMo.VmFolder)
	t, err := vmObj.Clone(vm.ctx, folderObj, vm.Name, cisp)
	if err != nil {
		return fmt.Errorf("error cloning vm from template: %v", err)
	}
	tInfo, err := t.WaitForResult(vm.ctx, nil)
	if err != nil {
		return fmt.Errorf("error waiting for clone task to finish: %v", err)
	}
	if tInfo.Error != nil {
		return fmt.Errorf("clone task finished with error: %v", tInfo.Error)
	}
	vmMo, err = findVM(vm, getVMSearchFilter(vm.Name))
	if err != nil {
		return fmt.Errorf("failed to retrieve cloned VM: %v", err)
	}
	if len(vm.Disks) > 0 {
		if err = reconfigureVM(vm, vmMo); err != nil {
			return err
		}
	}
	// power on
	if err = start(vm); err != nil {
		return err
	}
	if !vm.SkipIPWait {
		if err = waitForIP(vm, vmMo); err != nil {
			return err
		}
	}
	return nil
}

// diffDisks : diffDisks takes the devicelists as parameter and returns the
// file backing info of the disks (devList2 - devList1)
func diffDisks(devList2, devList1 object.VirtualDeviceList) []string {
	m := map[int32]int{}
	disks := make([]string, 0)
	for _, v := range devList1 {
		m[v.GetVirtualDevice().Key] = 0
	}

	for _, v := range devList2 {
		if _, ok := m[v.GetVirtualDevice().Key]; !ok {
			vmdkFile := v.GetVirtualDevice().Backing.(types.BaseVirtualDeviceFileBackingInfo).GetVirtualDeviceFileBackingInfo().FileName
			disks = append(disks, vmdkFile)
		}
	}
	return disks
}

// CreateDisk creates a new VirtualDisk device which can be added to a VM.
func CreateDisk(l object.VirtualDeviceList, c types.BaseVirtualController, ds types.ManagedObjectReference, name string, thinProvisioned bool) *types.VirtualDisk {
	// If name is not specified, one will be chosen for you.
	// But if when given, make sure it ends in .vmdk, otherwise it will be treated as a directory.
	if len(name) > 0 && filepath.Ext(name) != ".vmdk" {
		name += ".vmdk"
	}

	device := &types.VirtualDisk{
		VirtualDevice: types.VirtualDevice{
			Backing: &types.VirtualDiskFlatVer2BackingInfo{
				DiskMode:        string(types.VirtualDiskModePersistent),
				ThinProvisioned: types.NewBool(thinProvisioned),
				VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{
					FileName:  name,
					Datastore: &ds,
				},
			},
		},
	}

	l.AssignController(device, c)
	return device
}

var getDatastoreForVm = func(vm *VM, vmMo *mo.VirtualMachine) ([]string,
	error) {
	var (
		dsMos      []mo.Datastore
		datastores []string
		err        error
	)
	dsMors := vmMo.Datastore
	if len(dsMors) == 0 {
		return datastores, nil
	}
	err = vm.collector.Retrieve(vm.ctx, dsMors, []string{"info"}, &dsMos)
	if err != nil {
		return nil, fmt.Errorf(
			"Error retrieving datastores used by vm: %v", err)
	}
	for _, dsMo := range dsMos {
		datastores = append(datastores,
			dsMo.Info.GetDatastoreInfo().Name)
	}
	return datastores, nil
}

// reconfigureVM :reconfigureVM adds the disks to the vm and returns the vmdk
// file names of the disks added
// root disk datastore is used by default
var reconfigureVM = func(vm *VM, vmMo *mo.VirtualMachine) error {
	var (
		vDisk           *types.VirtualDisk
		thinProvisioned bool
		datastore       string
	)
	vmObj := object.NewVirtualMachine(vm.client.Client, vmMo.Reference())

	dcMo, err := GetDatacenter(vm)
	if err != nil {
		return err
	}

	if vm.datastore == "" {
		datastores, err := getDatastoreForVm(vm, vmMo)
		if err != nil {
			return err
		}
		vm.datastore = util.ChooseRandomString(datastores)
	}

	for index, disk := range vm.Disks {
		// root disk datastore is used by default
		if disk.Datastore == "" {
			datastore = vm.datastore
		} else {
			datastore = disk.Datastore
		}
		devices, err := vmObj.Device(vm.ctx)
		if err != nil {
			return fmt.Errorf("Failed to get devices while creating "+
				"Disks[%d] {%v} : %v", index, disk, err)
		}
		controller, err := devices.FindDiskController(disk.Controller)
		if err != nil {
			return fmt.Errorf("Failed to get controller while creating "+
				"Disks[%d] {%v} : %v", index, disk, err)
		}
		dsMo, err := findDatastore(vm, dcMo, datastore)
		if err != nil {
			return fmt.Errorf("Failed to get datastore while creating "+
				"Disks[%d] {%v} : %v", index, disk, err)
		}
		if strings.ToLower(disk.Provisioning) == "thick" {
			thinProvisioned = false
		} else {
			thinProvisioned = true
		}

		// getting device list before adding this disk
		devListBefore := devices

		vDisk = CreateDisk(devices, controller, dsMo.Reference(), "",
			thinProvisioned)
		vDisk.CapacityInKB = int64(disk.Size)
		if err := vmObj.AddDevice(vm.ctx, vDisk); err != nil {
			return fmt.Errorf("Failed to add device while creating "+
				"Disks[%d] {%v} : %v", index, disk, err)
		}

		// getting device list after adding disk and setting appropriate
		// vmdk filename to DiskFile
		devices, err = vmObj.Device(vm.ctx)
		if err != nil {
			return fmt.Errorf("Failed to get devices after creating "+
				"Disks[%d] {%v} : %v", index, disk, err)
		}
		vmdkFilename := diffDisks(devices, devListBefore)
		vm.Disks[index].DiskFile = vmdkFilename[0]
	}

	return nil
}

var waitForIP = func(vm *VM, vmMo *mo.VirtualMachine) error {
	vmObj := object.NewVirtualMachine(vm.client.Client, vmMo.Reference())
	// second parameter is to list v4 ips only and ignore v6 ips
	timeout := IPWAIT_TIMEOUT
	if value := os.Getenv("IPWAIT_TIMEOUT"); value != "" {
		// valid time units are "ns", "us", "ms", "s", "m", "h"
		duration, err := time.ParseDuration(value)
		if err == nil {
			timeout = duration
		}
	}
	ctx, cancel := context.WithTimeout(vm.ctx, timeout)
	defer cancel()
	ipMap, err := vmObj.WaitForNetIP(ctx, true)
	if err != nil {
		return fmt.Errorf("failed to wait for VM to get ips: %v", err)
	}

	// Parse the IP to make sure tools was running
	for _, ips := range ipMap {
		if len(ips) == 0 {
			continue
		}
		ip := net.ParseIP(ips[0])
		if ip != nil {
			return nil
		}
	}
	return fmt.Errorf("no valid ip assigned: %v", ipMap)
}

var halt = func(vm *VM) error {
	// Get a reference to the datacenter with host and vm folders populated
	state, err := getState(vm)
	if err != nil {
		return fmt.Errorf("Error getting state of vm : %v", err)
	}
	if state == "standby" {
		err = start(vm)
		if err != nil {
			return err
		}
	}
	vmMo, err := findVM(vm, getVMSearchFilter(vm.Name))
	if err != nil {
		return err
	}
	vmo := object.NewVirtualMachine(vm.client.Client, vmMo.Reference())
	poweroffTask, err := vmo.PowerOff(vm.ctx)
	if err != nil {
		return fmt.Errorf(
			"error creating a poweroff task on the vm: %v", err)
	}
	tInfo, err := poweroffTask.WaitForResult(vm.ctx, nil)
	if err != nil {
		return fmt.Errorf("error waiting for poweroff task: %v", err)
	}
	if tInfo.Error != nil {
		return fmt.Errorf("poweroff task returned an error: %v", err)
	}
	return nil
}

// shutDown Initiates guest shut down of this VM.
var shutDown = func(vm *VM) error {
	vmMo, err := findVM(vm, getVMSearchFilter(vm.Name))
	if err != nil {
		return err
	}
	vmo := object.NewVirtualMachine(vm.client.Client, vmMo.Reference())
	err = vmo.ShutdownGuest(vm.ctx)
	if err != nil {
		return fmt.Errorf("error initiating shutDown on the vm: %v", err)
	}

	state, err := getState(vm)
	if err != nil {
		return fmt.Errorf("Error getting state of vm : %v", err)
	}
	retry := RETRY_COUNT
	for state != "notRunning" && retry > 0 {
		state, _ = getState(vm)
		time.Sleep(5 * time.Second)
		retry--
	}
	if retry == 0 {
		return fmt.Errorf("Shutting down vm: %s timed out", vm.Name)
	}
	return nil
}

// waitForGuestStatus: wait for guest vm status to turn to either of the
// statuses sent as 'status'
func waitForGuestStatus(vm *VM, vmMo *mo.VirtualMachine, status int,
	timeout ...time.Duration) error {
	var (
		ctx        context.Context
		cancelFunc context.CancelFunc
	)
	if len(timeout) != 0 {
		ctx, cancelFunc = context.WithTimeout(vm.ctx, timeout[0])
		defer cancelFunc()
	} else {
		ctx = vm.ctx
	}

	collector := property.DefaultCollector(vm.client.Client)
	err := property.Wait(ctx, collector, vmMo.Reference(), []string{
		GUEST_HEART_BEAT_STATUS}, func(pc []types.PropertyChange) bool {
		for _, c := range pc {
			if c.Name != GUEST_HEART_BEAT_STATUS {
				continue
			}
			if c.Val == nil {
				continue
			}

			ps := c.Val.(types.ManagedEntityStatus)
			if status&GREEN_HEART_BEAT != 0 &&
				ps == types.ManagedEntityStatusGreen {
				return true
			}
			if status&GRAY_HEART_BEAT != 0 &&
				ps == types.ManagedEntityStatusGray {
				return true
			}
			if status&YELLOW_HEART_BEAT != 0 &&
				ps == types.ManagedEntityStatusYellow {
				return true
			}
			if status&RED_HEART_BEAT != 0 &&
				ps == types.ManagedEntityStatusRed {
				return true
			}
		}
		return false
	})
	return err
}

// restart Initiates guest reboot of this VM.
var restart = func(vm *VM) error {
	vmMo, err := findVM(vm, getVMSearchFilter(vm.Name))
	if err != nil {
		return err
	}
	vmo := object.NewVirtualMachine(vm.client.Client, vmMo.Reference())
	err = vmo.RebootGuest(vm.ctx)
	if err != nil {
		return fmt.Errorf("error initiating reboot on the vm: %v", err)
	}
	// wait for machine to shutdown - status will turn to gray
	// ignoring the error if timeout waiting for gray status
	waitForGuestStatus(vm, vmMo, GRAY_HEART_BEAT, GRAY_STATUS_CHECK_TIMEOUT)
	// wait for machine to come up again - status will turn to green
	err = waitForGuestStatus(vm, vmMo, GREEN_HEART_BEAT|YELLOW_HEART_BEAT,
		GREEN_STATUS_CHECK_TIMEOUT)
	if err != nil {
		return fmt.Errorf("error wating for vm to reboot : %v", err)
	}
	return nil
}

var start = func(vm *VM) error {
	vmMo, err := findVM(vm, getVMSearchFilter(vm.Name))
	if err != nil {
		return err
	}
	state := vmMo.Guest.GuestState
	if state == "shuttingdown" || state == "resetting" {
		return ErrorVMPowerStateChanging
	}
	vmo := object.NewVirtualMachine(vm.client.Client, vmMo.Reference())
	poweronTask, err := vmo.PowerOn(vm.ctx)
	if err != nil {
		return fmt.Errorf("error creating a poweron task on the vm: %v", err)
	}
	tInfo, err := poweronTask.WaitForResult(vm.ctx, nil)
	if err != nil {
		return fmt.Errorf("error waiting for poweron task: %v", err)
	}
	if tInfo.Error != nil {
		return fmt.Errorf("poweron task returned an error: %v", err)
	}
	if !vm.SkipIPWait {
		if err = waitForIP(vm, vmMo); err != nil {
			return err
		}
	}
	return nil
}

var reset = func(vm *VM) error {
	vmMo, err := findVM(vm, getVMSearchFilter(vm.Name))
	if err != nil {
		return err
	}
	vmo := object.NewVirtualMachine(vm.client.Client, vmMo.Reference())
	toolsRunning, err := vmo.IsToolsRunning(vm.ctx)
	if err != nil {
		return fmt.Errorf("Error checking status of tools : %v", err)
	}
	resetTask, err := vmo.Reset(vm.ctx)
	if err != nil {
		return fmt.Errorf("error creating a reset task on the vm: %v",
			err)
	}
	tInfo, err := resetTask.WaitForResult(vm.ctx, nil)
	if err != nil {
		return fmt.Errorf("error waiting for reset task: %v", err)
	}
	if tInfo.Error != nil {
		return fmt.Errorf("reset task returned an error: %v", err)
	}
	// wait for machine to reset - status will turn to red
	if toolsRunning {
		// wait for machine to shutdown - status will turn to gray
		err = waitForGuestStatus(vm, vmMo,
			GRAY_HEART_BEAT|RED_HEART_BEAT)
		if err != nil {
			return fmt.Errorf("error wating for vm to reset: %v",
				err)
		}
		err = waitForGuestStatus(vm, vmMo,
			GREEN_HEART_BEAT|YELLOW_HEART_BEAT)
		if err != nil {
			return fmt.Errorf("error wating for vm to reset : %v",
				err)
		}
	}
	return nil
}

var filterHosts = func(vm *VM, hosts []types.ManagedObjectReference) ([]types.ManagedObjectReference, error) {
	filteredHosts := []types.ManagedObjectReference{}
	for _, host := range hosts {
		valid, err := validateHost(vm, host)
		if err != nil {
			return nil, err
		}
		if valid {
			filteredHosts = append(filteredHosts, host)
		}
	}
	return filteredHosts, nil
}

var getVMLocation = func(vm *VM, dcMo *mo.Datacenter) (l location, err error) {
	switch vm.Destination.DestinationType {
	case DestinationTypeHost:
		var crMo *mo.ComputeResource
		crMo, err = findComputeResource(vm, dcMo, vm.Destination.DestinationName)
		if err != nil {
			return
		}
		var valid bool
		valid, err = validateHost(vm, crMo.Host[0])
		if err != nil {
			return
		}
		if !valid {
			err = NewErrorInvalidHost(vm.Destination.DestinationName, vm.datastore, vm.Networks)
			return
		}
		l.Host = crMo.Host[0]
		l.Networks = crMo.Network
		if crMo.ResourcePool == nil {
			err = fmt.Errorf("No valid resource pool found on the host")
			return
		}
		l.ResourcePool = *crMo.ResourcePool
	case DestinationTypeCluster:
		var crMo *mo.ClusterComputeResource
		crMo, err = findClusterComputeResource(vm, dcMo, vm.Destination.DestinationName)
		if err != nil {
			return
		}
		if len(crMo.Host) <= 0 {
			err = errNoHostsInCluster
			return
		}
		// If a host name was passed in try to find it within the cluster
		if vm.Destination.HostSystem != "" {
			var mo *mo.HostSystem
			mo, err = findHostSystem(vm, crMo.Host, vm.Destination.HostSystem)
			if err != nil {
				return
			}
			var valid bool
			valid, err = validateHost(vm, mo.Reference())
			if err != nil {
				return
			}
			if !valid {
				err = NewErrorInvalidHost(vm.Destination.HostSystem, vm.datastore, vm.Networks)
				return
			}
			ref := mo.Reference()
			l.Host = ref
		} else {
			var filteredHosts []types.ManagedObjectReference
			filteredHosts, err = filterHosts(vm, crMo.Host)
			if err != nil {
				return
			}
			if len(filteredHosts) <= 0 {
				err = fmt.Errorf("No suitable hosts found in the cluster")
				return
			}
			n := util.Random(0, len(filteredHosts)-1)
			l.Host = filteredHosts[n]
		}
		if crMo.ResourcePool == nil {
			err = fmt.Errorf("No valid resource pool found on the host")
			return
		}
		l.ResourcePool = *crMo.ResourcePool
		l.Networks = crMo.Network
	case DestinationTypeResourcePool:
		var rp *mo.ResourcePool
		dc := object.NewDatacenter(vm.client.Client, dcMo.Self)
		// Set datacenter
		vm.finder.SetDatacenter(dc)

		rp, err = findResourcePoolByMOID(vm, vm.Destination.MOID)
		if err != nil {
			return
		}
		cr := mo.ClusterComputeResource{}
		err = vm.collector.RetrieveOne(vm.ctx, rp.Owner, []string{"network"}, &cr)
		if err != nil {
			return
		}
		l.ResourcePool = rp.Reference()
		l.Networks = cr.Network
	default:
		err = ErrorDestinationNotSupported
		return
	}
	return
}

var createTemplateName = func(t string, ds string) string {
	return fmt.Sprintf("%s-%s", t, ds)
}

var uploadTemplate = func(vm *VM, dcMo *mo.Datacenter, selectedDatastore string) error {
	var template string
	if vm.UseLocalTemplates {
		template = createTemplateName(vm.Template.Name, selectedDatastore)
		vm.Template.Name = template
	}

	vm.datastore = selectedDatastore
	downloadOvaPath, err := ioutil.TempDir("", "")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(downloadOvaPath); err != nil {
			fmt.Errorf("can't remove temp directory, error: %v", err.Error())
		}
	}()
	// Read the ovf file
	if vm.OvaPathUrl != "" {
		vm.OvfPath, err = downloadOva(downloadOvaPath, vm.OvaPathUrl)
		if err != nil {
			return err
		}
	}
	ovfContent, err := parseOvf(vm.OvfPath)
	if err != nil {
		return err
	}

	dsMo, err := findDatastore(vm, dcMo, selectedDatastore)
	if err != nil {
		return err
	}
	l, err := getVMLocation(vm, dcMo)
	if err != nil {
		return err
	}
	// Create an import spec
	cisp := types.OvfCreateImportSpecParams{
		HostSystem:       &l.Host,
		EntityName:       template,
		DiskProvisioning: "thin",
		PropertyMapping:  nil,
	}

	ovfManager := object.NewOvfManager(vm.client.Client)
	rpo := object.NewResourcePool(vm.client.Client, l.ResourcePool)

	specResult, err := ovfManager.CreateImportSpec(vm.ctx, ovfContent, rpo,
		object.NewDatastore(vm.client.Client, dsMo.Reference()), cisp)
	if err != nil {
		return fmt.Errorf("failed to create an import spec for the VM: %v", err)
	}

	// FIXME (Preet) specResult can also have warnings. Need to log/return those.
	if specResult.Error != nil {
		return fmt.Errorf("errors returned from the ovf manager api. Errors: %v", specResult.Error)
	}

	// If any of the unit numbers in the spec are 0, they need to be reset to -1
	resetUnitNumbers(specResult)

	hso := object.NewHostSystem(vm.client.Client, l.Host)
	// Import into the DC's vm folder for now. We can make it user configurable later.
	fo := object.NewFolder(vm.client.Client, dcMo.VmFolder)
	lease, err := rpo.ImportVApp(vm.ctx, specResult.ImportSpec, fo, hso)
	if err != nil {
		return fmt.Errorf("error getting an nfc lease: %v", err)
	}

	err = uploadOvf(vm, specResult, NewLease(vm.ctx, lease))
	if err != nil {
		return fmt.Errorf("error uploading the ovf template: %v", err)
	}

	vmMo, err := findVM(vm, getTempSearchFilter(vm.Template))
	if err != nil {
		return fmt.Errorf("error getting the uploaded VM: %v", err)
	}

	// LinkedClones cannot be created from templates, but must be created from snapshots of VMs.
	// If UseLinkedClones is set to true, do not mark this is a template and instead
	// create the necessary snapshot to produce a linked clone from.
	vmo := object.NewVirtualMachine(vm.client.Client, vmMo.Reference())

	if vm.UseLinkedClones {
		s := snapshot{
			Name:        "snapshot-" + template,
			Description: "Snapshot created by Libretto for linked clones.",
			Memory:      false,
			Quiesce:     false,
		}

		snapshotTask, err := vmo.CreateSnapshot(vm.ctx, s.Name, s.Description, s.Memory, s.Quiesce)

		if err != nil {
			return fmt.Errorf("error creating snapshot of the vm: %v", err)
		}
		tInfo, err := snapshotTask.WaitForResult(vm.ctx, nil)
		if err != nil {
			return fmt.Errorf("error waiting for snapshot to finish: %v", err)
		}
		if tInfo.Error != nil {
			return fmt.Errorf("snapshot task returned an error: %v", err)
		}
	} else {
		err = vmo.MarkAsTemplate(vm.ctx)
		if err != nil {
			return fmt.Errorf("error converting the uploaded VM to a template: %v", err)
		}
	}
	return nil
}

var getNetworkName = func(vm *VM, network types.ManagedObjectReference) (string, error) {
	switch network.Type {
	case "Network":
		dst := mo.Network{}
		err := vm.collector.RetrieveOne(vm.ctx, network, []string{"name"}, &dst)
		if err != nil {
			return "", err
		}
		return dst.Name, nil
	case "DistributedVirtualPortgroup":
		dst := mo.DistributedVirtualPortgroup{}
		err := vm.collector.RetrieveOne(vm.ctx, network, []string{"name"}, &dst)
		if err != nil {
			return "", err
		}
		return dst.Name, nil
	}
	return "", fmt.Errorf("Could not retrieve the network name for: %s", network.Value)
}

// validateHost validates that the host-system contains the network and the datastore passed in
func validateHost(vm *VM, hsMor types.ManagedObjectReference) (bool, error) {
	nwValid := true
	dsValid := false
	// Fetch the managed object for the host system to populate the datastore and the network folders
	hsMo := mo.HostSystem{}
	err := vm.collector.RetrieveOne(vm.ctx, hsMor, []string{"network", "datastore"}, &hsMo)
	if err != nil {
		return false, err
	}
	hostNetworks := map[string]struct{}{}
	for _, nw := range hsMo.Network {
		name, err := getNetworkName(vm, nw)
		if err != nil {
			return false, err
		}
		if name == "" {
			return false, fmt.Errorf("Network name empty for: %s", nw.Value)
		}
		hostNetworks[name] = struct{}{}
	}
	for _, v := range vm.Networks {
		if _, ok := hostNetworks[v.Name]; !ok {
			nwValid = false
			break
		}
	}

	if vm.datastore == "" {
		return nwValid, nil
	}
	for _, ds := range hsMo.Datastore {
		dsMo := mo.Datastore{}
		err := vm.collector.RetrieveOne(vm.ctx, ds, []string{"name"}, &dsMo)
		if err != nil {
			return false, err
		}
		if dsMo.Name == vm.datastore {
			dsValid = true
			break
		}
	}
	return (nwValid && dsValid), nil
}

func getState(vm *VM) (state string, err error) {
	// Get a reference to the datacenter with host and vm folders populated
	vmMo, err := findVM(vm, getVMSearchFilter(vm.Name))
	if err != nil {
		return "", lvm.ErrVMInfoFailed
	}

	return vmMo.Guest.GuestState, nil
}

func getPowerState(vm *VM) (state string, err error) {
	// Get a reference to the datacenter with host and vm folders populated
	vmMo, err := findVM(vm, getVMSearchFilter(vm.Name))
	if err != nil {
		return "", lvm.ErrVMInfoFailed
	}

	return fmt.Sprintf("%s", vmMo.Runtime.PowerState), nil
}

// answerQuestion checks to see if there are currently pending questions on the
// VM which prevent further actions. If so, it automatically responds to the
// question based on the the vm.QuestionResponses map. If there is a problem
// responding to the question, the error is returned. If there are no pending
// questions or it does not map to any predefined response, nil is returned.
func (vm *VM) answerQuestion(vmMo *mo.VirtualMachine) error {
	q := vmMo.Runtime.Question
	if q == nil {
		return nil
	}

	for qre, ans := range vm.QuestionResponses {
		if match, err := regexp.MatchString(qre, q.Text); err != nil {
			return fmt.Errorf("error while parsing automated responses: %v", err)
		} else if match {
			ans, validOptions := resolveAnswerAndOptions(q.Choice.ChoiceInfo, ans)
			err = answerVSphereQuestion(vm, vmMo, q.Id, ans)
			if err != nil {
				return fmt.Errorf("error with answer %q to question %q: %v. Valid answers: %v", ans, q.Text, err, validOptions)
			}
		}
	}

	return nil
}

// resolveAnswerAndOptions takes the choiceInfo of a question object and the
// intended answer (index string or summary text) and returns the matching
// answer index as a string along with a human readable representation of the
// valid options. If the given answer does not match any of the choices summary
// text, the given answer is returned.
func resolveAnswerAndOptions(choiceInfo []types.BaseElementDescription, answer string) (resolvedAnswer, validOptions string) {
	resolvedAnswer = answer
	for _, e := range choiceInfo {
		ed := e.(*types.ElementDescription)
		validOptions = fmt.Sprintf("%s(%s) %s ", validOptions, ed.Key, ed.Description.Summary)
		if strings.EqualFold(ed.Description.Summary, answer) {
			resolvedAnswer = ed.Key
		}
	}
	return resolvedAnswer, strings.TrimSpace(validOptions)
}

var answerVSphereQuestion = func(vm *VM, vmMo *mo.VirtualMachine, questionID string, answer string) error {
	vmObj := object.NewVirtualMachine(vm.client.Client, vmMo.Reference())
	return vmObj.Answer(vm.ctx, questionID, answer)
}

var errorEmpty = errors.New("Folder is empty")

func init() {
	findMob = func(vm *VM, mor types.ManagedObjectReference, name string) (*types.ManagedObjectReference, error) {
		folder := mo.Folder{}
		// Get the child entity of the folder passed in

		err := vm.collector.RetrieveOne(vm.ctx, mor, []string{"childEntity"}, &folder)
		if err != nil {
			return nil, err
		}

		if len(folder.ChildEntity) == 0 {
			return nil, errorEmpty
		}

		for _, child := range folder.ChildEntity {
			if child.Type == "Folder" {
				// Search here first
				found, err := findMob(vm, child, name)
				if err == errorEmpty {
					continue
				} else if err != nil {
					return found, err
				}
			}
			if child.Type == "ComputeResource" {
				cr := mo.ComputeResource{}
				err := vm.collector.RetrieveOne(vm.ctx, child, []string{"name", "host", "resourcePool", "datastore", "network"}, &cr)
				if err != nil {
					return nil, err
				}
				if cr.Name == name {
					ref := cr.Reference()
					return &ref, nil
				}
			}
			if child.Type == "ClusterComputeResource" {
				cr := mo.ClusterComputeResource{}
				err := vm.collector.RetrieveOne(vm.ctx, child, []string{"name", "host", "resourcePool", "datastore", "network"}, &cr)
				if err != nil {
					return nil, err
				}
				if cr.Name == name {
					ref := cr.Reference()
					return &ref, nil
				}
			}
		}
		return nil, NewErrorObjectNotFound(errors.New("Could not find the mob"), name)
	}
}

// createCustomSpecStaticIp: creates custom spec for static ip from xml
func createCustomSpecStaticIp(vm *VM) error {
	csMgr := object.NewCustomizationSpecManager(vm.client.Client)
	csSpec, err := csMgr.XmlToCustomizationSpecItem(vm.ctx,
		XML_STATIC_IP_SPEC)
	if err != nil {
		return err
	}
	err = csMgr.CreateCustomizationSpec(vm.ctx, *csSpec)
	if err != nil {
		return err
	}
	return nil
}

// updateCustomSpec: updates custom spec structure with the ip settings
func updateCustomSpec(vm *VM, tempMo *mo.VirtualMachine,
	customSpec *types.CustomizationSpec) *types.CustomizationSpec {
	// if ip or subnet is not passed return nil
	if vm.NetworkSetting.Ip == "" || vm.NetworkSetting.SubnetMask == "" {
		return nil
	}
	// set ip address, subnet mask, default gateway
	nicSetting := customSpec.NicSettingMap[0]
	ip := nicSetting.Adapter.Ip
	ipValue := reflect.ValueOf(ip).Elem()
	ipAddress := ipValue.FieldByName("IpAddress")
	if ipAddress.CanSet() || ipAddress.IsValid() {
		ipAddress.SetString(vm.NetworkSetting.Ip)
	}
	nicSetting.Adapter.SubnetMask = vm.NetworkSetting.SubnetMask
	gateway := vm.NetworkSetting.Gateway
	nicSetting.Adapter.Gateway = append(nicSetting.Adapter.Gateway, gateway)

	// set dns server
	if vm.NetworkSetting.DnsServer != "" {
		dnsServerList := []string{vm.NetworkSetting.DnsServer}
		for _, ip := range tempMo.Guest.IpStack {
			dnsServerList = append(dnsServerList,
				ip.DnsConfig.IpAddress...)
		}
		customSpec.GlobalIPSettings.DnsServerList = append(
			customSpec.GlobalIPSettings.DnsServerList,
			dnsServerList...)
	}

	return customSpec
}

// IsClusterDrsEnabled: returns true if the cluster is drs enabled
func IsClusterDrsEnabled(vm *VM) (bool, error) {
	dcMo, err := GetDatacenter(vm)
	if err != nil {
		return false, err
	}
	crMo, err := findClusterComputeResource(vm, dcMo,
		vm.Destination.DestinationName)
	if err != nil {
		return false, err
	}

	drsEnabled := crMo.Configuration.DrsConfig.Enabled
	if drsEnabled != nil {
		return *drsEnabled, nil
	}

	return false, errors.New("error fetching cluster config details")
}

// checkAndCreateCustomSpec: checks if custom spec for static ip exists
// creates if doesn't exist
func checkAndCreateCustomSpec(vm *VM) error {
	customizationSpecManager := object.NewCustomizationSpecManager(
		vm.client.Client)

	exists, err := customizationSpecManager.DoesCustomizationSpecExist(
		vm.ctx, STATICIP_CUSTOM_SPEC_NAME)
	if err != nil {
		return err
	}

	if !exists {
		err = createCustomSpecStaticIp(vm)
		if err != nil {
			return fmt.Errorf("Error creating custom spec: %v", err)
		}
	}
	return nil
}

type VmProperties struct {
	Name       string
	Properties mo.VirtualMachine
}

// getVMsInAllDCs: Returns virtual machines from all DCs (entire inventory)
func getVMsInAllDCs(vm *VM) ([]VmProperties, error) {
	dcList, err := vm.finder.DatacenterList(vm.ctx, "*")
	if err != nil {
		return nil, fmt.Errorf("Error in getting datacenter "+
			"list: %v", err)
	}
	allDCsVMs := make([]VmProperties, 0)
	vmsInDc := make([]VmProperties, 0)
	for _, dcObj := range dcList {
		vmsInDc, err = getDcVMList(vm, dcObj)
		if err != nil {
			return nil, err
		}
		allDCsVMs = append(allDCsVMs, vmsInDc...)
	}
	return allDCsVMs, nil
}

// getHostsLookup: returns appropriate hostsLookup map for given destination
// cluster/host
func getHostsLookup(vm *VM) (map[string]bool, error) {
	hostsLookup := make(map[string]bool)
	// if host is provided update the hostsLookup map to have one host only
	if vm.Destination.HostSystem != "" {
		hostsLookup = map[string]bool{
			vm.Destination.HostSystem: false,
		}
	} else {
		var hsMos []mo.HostSystem
		// Get the cluster resource and its host
		dcMo, err := GetDatacenter(vm)
		if err != nil {
			return nil, err
		}
		crMo, err := findClusterComputeResource(vm, dcMo,
			vm.Destination.DestinationName)
		if err != nil {
			return nil, err
		}
		// get the hosts in cluster
		if len(crMo.Host) == 0 {
			return hostsLookup, nil
		}
		err = vm.collector.Retrieve(vm.ctx, crMo.Host, []string{"name"},
			&hsMos)
		if err != nil {
			return nil, err
		}
		for _, host := range hsMos {
			hostsLookup[host.Name] = false
		}
	}
	return hostsLookup, nil
}

// getVirtualMachines : Returns the virtual machines in a allDCs/dc/cluster/host
func getVirtualMachines(vm *VM, allDCs bool) ([]VmProperties, error) {
	var (
		hsMo mo.HostSystem
	)

	if allDCs {
		return getVMsInAllDCs(vm)
	}

	// get virtual machines for the whole datacenter
	vmsInDc := make([]VmProperties, 0)
	dcMo, err := GetDatacenter(vm)
	if err != nil {
		return nil, err
	}
	dcObj := object.NewDatacenter(vm.client.Client,
		dcMo.Reference())
	vmsInDc, err = getDcVMList(vm, dcObj)
	if err != nil {
		return nil, err
	}

	// if cluster name not given, return vms in datacenter
	if vm.Destination.DestinationName == "" {
		return vmsInDc, nil
	}

	// using hostsLookup to check if a vm is related to a host in map
	vmsInCluster := make([]VmProperties, 0)
	hostsLookup, err := getHostsLookup(vm)
	if err != nil {
		return nil, err
	}

	for _, vmProp := range vmsInDc {
		vmMo := vmProp.Properties

		if vmMo.Runtime.Host == nil {
			continue
		}
		err = vm.collector.RetrieveOne(vm.ctx, *vmMo.Runtime.Host,
			[]string{"name"}, &hsMo)
		if err != nil {
			return nil, err
		}
		if _, ok := hostsLookup[hsMo.Name]; ok {
			vmsInCluster = append(vmsInCluster, VmProperties{
				Name:       vmProp.Name,
				Properties: vmMo})
		}
	}
	return vmsInCluster, nil
}

// getDcVMList : returns list of VirtualMachine objects in a Datacenter
func getDcVMList(vm *VM, datacenter *object.Datacenter) (
	[]VmProperties, error) {
	// Set datacenter
	vm.finder.SetDatacenter(datacenter)
	folders, err := datacenter.Folders(vm.ctx)
	if err != nil {
		return nil, err
	}
	// datacenter's vmFolder has all the vms in all the clusters/hosts in
	// that datacetner, returning the vm in datacenter vmfolder
	return getVmsInFolder(vm, folders.VmFolder, "")
}

// getVmsInFolder: returns list of VmProperties which has full path and
// mo.Virtualmachine struct of vms in a vcenter vm folder
func getVmsInFolder(vm *VM, folder *object.Folder, path string) (
	[]VmProperties, error) {
	allVms := make([]VmProperties, 0)
	// get list of folders/vms/templates in folder
	children, err := folder.Children(vm.ctx)
	if err != nil {
		return nil, err
	}
	for _, entity := range children {
		mor := entity.Reference()
		switch mor.Type {
		// if child is a folder, look for vms in the folder recursively
		// and add to the hash
		case "Folder":
			// Fetch the childEntity property of the folder
			folderMo := mo.Folder{}
			err := vm.collector.RetrieveOne(vm.ctx, mor, []string{
				"name"}, &folderMo)
			if err != nil {
				if isObjectDeleted(err) {
					continue
				}
				return nil, err
			}
			// unescaping to convert any escaped character
			folderName, err := url.QueryUnescape(folderMo.Name)
			if err != nil {
				return nil, err
			}
			// Adding delimitter in case "/" is present in name
			folderName = strings.Replace(folderName, "/", "\\/",
				-1)
			folder := object.NewFolder(vm.client.Client,
				mor)
			// gettings vm in folder recursively
			vmProps, err := getVmsInFolder(vm, folder,
				path+folderName+"/")
			if err != nil {
				return nil, err
			}
			// updating the allVMs hash
			allVms = append(allVms, vmProps...)
		case "VirtualMachine":
			// if child is vm/template, return the full path and
			// mo of the vm
			vmMo := mo.VirtualMachine{}
			err := vm.collector.RetrieveOne(vm.ctx, mor, []string{
				"name", "guest", "config", "runtime",
				"summary", "resourcePool"}, &vmMo)
			if err != nil {
				if isObjectDeleted(err) {
					continue
				}
				return nil, err
			}
			// unescaping to convert any escaped character
			vmName, err := url.QueryUnescape(vmMo.Name)
			if err != nil {
				return nil, err
			}
			// Adding delimitter in case "/" is present in name
			vmName = path + strings.Replace(vmName, "/", "\\/",
				-1)
			allVms = append(allVms, VmProperties{
				Name:       vmName,
				Properties: vmMo})
		}
	}
	return allVms, nil
}

// getDatastoreInHost: lists datastores in a host in a cluster
func getDatastoreInHost(vm *VM, crMo *mo.ClusterComputeResource) ([]types.ManagedObjectReference, error) {
	var (
		err  error
		hsMo mo.HostSystem
	)
	for _, host := range crMo.Host {
		err = vm.collector.RetrieveOne(vm.ctx, host, []string{"name", "datastore"}, &hsMo)
		if err != nil {
			return nil, err
		}
		if hsMo.Name == vm.Destination.HostSystem {
			return hsMo.Datastore, nil
		}
	}
	return nil, fmt.Errorf("Host: %s not found in cluster: %s", vm.Destination.HostSystem, crMo.Name)
}

// getSharedDatastoreInCluster: get the datastores shared accross all the
// hosts in cluster
func getSharedDatastoreInCluster(vm *VM, crMo *mo.ClusterComputeResource) (
	[]types.ManagedObjectReference, error) {
	var (
		hsMos  []mo.HostSystem
		dsMors []types.ManagedObjectReference
		err    error
	)

	dsList := make(map[types.ManagedObjectReference]int)
	if len(crMo.Host) == 0 {
		return dsMors, nil
	}
	err = vm.collector.Retrieve(vm.ctx, crMo.Host, []string{
		"name", "datastore"}, &hsMos)
	if err != nil {
		return nil, err
	}
	for _, hsMo := range hsMos {
		for _, ds := range hsMo.Datastore {
			_, ok := dsList[ds]
			if ok {
				dsList[ds]++
			} else {
				dsList[ds] = 1
			}
		}
	}

	nHosts := len(hsMos)
	for key, value := range dsList {
		if value == nHosts {
			dsMors = append(dsMors, key)
		}
	}
	return dsMors, nil
}

// existActiveTasks: returns true if any active tasks done on vm are active
func isTaskInProgress(vm *VM, vmMo *mo.VirtualMachine) bool {
	var (
		taskMo mo.Task
		err    error
	)
	for _, task := range vmMo.RecentTask {
		err = vm.collector.RetrieveOne(vm.ctx, task, []string{
			"info"}, &taskMo)
		if err != nil {
			continue
		}
		switch taskMo.Info.State {
		// available states queued, running, success, error
		case types.TaskInfoStateQueued, types.TaskInfoStateRunning:
			return false
		}
	}
	return true
}

// waitForTasksToFinish: waits for any active tasks on vm
func waitForTasksToFinish(vm *VM, tasks []types.ManagedObjectReference) {
	// wait for tasks to finish
	for _, task := range tasks {
		tObj := object.NewTask(vm.client.Client, task)
		tObj.Wait(vm.ctx)
	}
}

// tagsHasKey: returns true if any of the tags has 'key'
func tagsHasKey(tags []types.Tag, key string) bool {
	for _, tag := range tags {
		if tag.Key == key {
			return true
		}
	}
	return false
}

// networkDeviceChangeSpec: returns network device change spec based on vm.Networks
func networkDeviceChangeSpec(vm *VM, vmMo *mo.VirtualMachine) (
	[]types.BaseVirtualDeviceConfigSpec, error) {
	var (
		addDeviceSpecs    []types.BaseVirtualDeviceConfigSpec
		removeDeviceSpecs []types.BaseVirtualDeviceConfigSpec
		deviceChangeSpec  []types.BaseVirtualDeviceConfigSpec
	)

	// get host associated with the vm
	hsMo := mo.HostSystem{}
	if vmMo.Runtime.Host == nil {
		return nil, errors.New("host associated with vm not found")
	}
	err := vm.collector.RetrieveOne(vm.ctx, *vmMo.Runtime.Host,
		[]string{"name", "network"}, &hsMo)
	if err != nil {
		return nil, fmt.Errorf(
			"error while fetching host info: %v", err)
	}

	// create map of network name and network mors
	_, nwMap, err := createNetworkMapping(vm, vm.Networks, hsMo.Network)
	devices := vmMo.Config.Hardware.Device

	for _, nw := range vm.Networks {
		spec := new(types.VirtualDeviceConfigSpec)
		switch nw.Operation {
		case "", "add":
			spec, err = addNetworkDeviceSpec(vm, nwMap[nw.Name],
				nw.Name)
			addDeviceSpecs = append(addDeviceSpecs, spec)
		case "remove":
			if nw.DeviceKey == nil {
				return nil, fmt.Errorf(
					"Device key not specified for network: %v",
					nw.Name)
			}
			spec, err = removeNetworkDeviceSpec(vm, nwMap[nw.Name],
				nw.Name, *nw.DeviceKey, devices)
			removeDeviceSpecs = append(removeDeviceSpecs, spec)
		default:
			err = fmt.Errorf(
				"invalid network device operation: %v for "+
					"network: %v", nw.Operation, nw.Name)
			return nil, err
		}
		if err != nil {
			return nil, fmt.Errorf(
				"error creating spec for network:%v, error: %v",
				nw.Name, err)
		}
	}
	deviceChangeSpec = append(deviceChangeSpec, addDeviceSpecs...)
	deviceChangeSpec = append(deviceChangeSpec, removeDeviceSpecs...)
	return deviceChangeSpec, nil
}

// removeNetworkDeviceSpec: returns the device config spec for removing network
func removeNetworkDeviceSpec(vm *VM, nwMor types.ManagedObjectReference,
	name string, deviceKey int32, devices object.VirtualDeviceList) (
	*types.VirtualDeviceConfigSpec, error) {
	backing, err := getEthernetBacking(vm, nwMor, name)
	if err != nil {
		return nil, err
	}
	nwDevices := devices.SelectByBackingInfo(backing)
	if len(nwDevices) == 0 {
		return nil, NewErrorObjectNotFound(fmt.Errorf(
			"device with network:%v not found", name), name)
	}
	device := nwDevices.FindByKey(deviceKey)
	if device == nil {
		return nil, fmt.Errorf("Network: %v device with key: %v not found",
			name, deviceKey)
	}
	spec := &types.VirtualDeviceConfigSpec{
		Operation: types.VirtualDeviceConfigSpecOperationRemove,
		Device:    device,
	}
	return spec, nil
}
