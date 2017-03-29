// Copyright 2015 Apcera Inc. All rights reserved.

package vsphere

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/apcera/libretto/ssh"
	"github.com/apcera/libretto/util"
	lvm "github.com/apcera/libretto/virtualmachine"
	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// constants to compare with if the template already exists
// SKIPTEMPLATE_ERROR     : Errors out
// SKIPTEMPLATE_OVERWRITE : Overwrites the template and provision the vm
// SKIPTEMPLATE_USE       : Use the existing template and provision the vm
const (
	SKIPTEMPLATE_ERROR = iota
	SKIPTEMPLATE_OVERWRITE
	SKIPTEMPLATE_USE
)

type vmwareFinder struct {
	finder *find.Finder
}

func (v vmwareFinder) SetDatacenter(dc *object.Datacenter) *find.Finder {
	return v.finder.SetDatacenter(dc)
}

func (v vmwareFinder) DatacenterList(c context.Context, p string) ([]*object.Datacenter, error) {
	return v.finder.DatacenterList(c, p)
}

func (v vmwareFinder) VirtualMachineList(c context.Context, p string) ([]*object.VirtualMachine, error) {
	return v.finder.VirtualMachineList(c, p)
}

func (v vmwareFinder) ClusterComputeResourceList(c context.Context, p string) ([]*object.ClusterComputeResource, error) {
	return v.finder.ClusterComputeResourceList(c, p)
}

func (v vmwareFinder) NetworkList(c context.Context, p string) ([]object.NetworkReference, error) {
	return v.finder.NetworkList(c, p)
}

// NewLease creates a VMwareLease.
var NewLease = func(ctx context.Context, lease *object.HttpNfcLease) Lease {
	return VMwareLease{
		Ctx:   ctx,
		Lease: lease,
	}
}

// VMwareLease implements the Lease interface.
type VMwareLease struct {
	Ctx   context.Context
	Lease *object.HttpNfcLease
}

// HTTPNfcLeaseProgress takes a percentage as an int and sets that percentage as
// the completed percent.
func (v VMwareLease) HTTPNfcLeaseProgress(p int32) {
	v.Lease.HttpNfcLeaseProgress(v.Ctx, p)
}

// Wait waits for the underlying lease to finish.
func (v VMwareLease) Wait() (*types.HttpNfcLeaseInfo, error) {
	return v.Lease.Wait(v.Ctx)
}

// Complete marks the underlying lease as complete.
func (v VMwareLease) Complete() error {
	return v.Lease.HttpNfcLeaseComplete(v.Ctx)
}

// NewProgressReader returns a functional instance of ReadProgress.
var NewProgressReader = func(r io.Reader, t int64, l Lease) ProgressReader {
	return ReadProgress{
		Reader:     r,
		TotalBytes: t,
		Lease:      l,
		ch:         make(chan int64, 1),
		wg:         &sync.WaitGroup{},
	}
}

// ProgressReader is an interface for interacting with the vSphere SDK. It provides a
// `Start` method to start a monitoring go-routine which monitors the progress of the
// upload as well as a `Wait` method to wait until the upload is complete.
type ProgressReader interface {
	StartProgress()
	Wait()
	Read(p []byte) (n int, err error)
}

// ReadProgress wraps a io.Reader and submits progress reports on an embedded channel
type ReadProgress struct {
	Reader     io.Reader
	TotalBytes int64
	Lease      Lease

	wg *sync.WaitGroup
	ch chan int64 //Channel for getting progress reports
}

// Read implements the Reader interface.
func (r ReadProgress) Read(p []byte) (n int, err error) {
	n, err = r.Reader.Read(p)
	if err != nil {
		return
	}
	r.ch <- int64(n)
	return
}

// StartProgress starts a goroutine that updates local progress on the lease as
// well as pass it down to the underlying lease.
func (r ReadProgress) StartProgress() {
	r.wg.Add(1)
	go func() {
		var bytesReceived int64
		var percent int32
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		defer r.wg.Done()
		for {
			select {
			case b := <-r.ch:
				bytesReceived += b
				percent = int32((float32(bytesReceived) / float32(r.TotalBytes)) * 100)
			case <-tick.C:
				// TODO: Preet This can return an error as well, should return it
				r.Lease.HTTPNfcLeaseProgress(percent)
				if percent == 100 {
					return
				}
			}
		}
	}()
}

// Wait waits for the underlying waitgroup to be complete.
func (r ReadProgress) Wait() {
	r.wg.Wait()
	r.Lease.Complete()
}

var (
	// ErrorVMExists is returned when the VM being provisioned already exists.
	ErrorVMExists = errors.New("VM already exists")
	//ErrorDestinationNotSupported is returned when the destination is not supported for provisioning.
	ErrorDestinationNotSupported = errors.New("destination is not supported by this provisioner")
	// ErrorVMPowerStateChanging is returned when the power state of the VM is resetting or shuttingdown
	// The VM can't be started in this state
	ErrorVMPowerStateChanging = errors.New("the power state of the vm is changing, try again later")
	errNoHostsInCluster       = errors.New("the cluster does not have any hosts in it")
)

// ErrorParsingURL is returned when the sdk url passed to the vSphere provider is not valid
type ErrorParsingURL struct {
	uri string
	err error
}

// ErrorInvalidHost is returned when the host does not have a datastore or network selected by the user
type ErrorInvalidHost struct {
	host string
	ds   string
	nw   map[string]string
}

func (e ErrorInvalidHost) Error() string {
	return fmt.Sprintf("The host %q does not have a valid configuration. Required datastore: %q. Required network: %+v.", e.host, e.ds, e.nw)
}

// ErrorBadResponse is returned when an HTTP request gets a bad response
type ErrorBadResponse struct {
	resp *http.Response
}

func (e ErrorBadResponse) Error() string {
	body, _ := ioutil.ReadAll(e.resp.Body)
	return fmt.Sprintf("Bad response to HTTP request. Status code: %d Body: '%s'", e.resp.StatusCode, body)
}

// ErrorClientFailed is returned when a client cannot be created using the given creds
type ErrorClientFailed struct {
	err error
}

func (e ErrorClientFailed) Error() string {
	return fmt.Sprintf("error connecting to the VI SDK: %s", e.err)
}

// ErrorObjectNotFound is returned when the object being searched for is not found.
type ErrorObjectNotFound struct {
	err error
	obj string
}

func (e ErrorObjectNotFound) Error() string {
	return fmt.Sprintf("Could not retrieve the object '%s' from the vSphere API: %s", e.obj, e.err)
}

// ErrorPropertyRetrieval is returned when the object being searched for is not found.
type ErrorPropertyRetrieval struct {
	err error
	ps  []string
	mor types.ManagedObjectReference
}

func (e ErrorPropertyRetrieval) Error() string {
	return fmt.Sprintf("Could not retrieve '%s' for object '%s': %s", e.ps, e.mor, e.err)
}

func (e ErrorParsingURL) Error() string {
	if e.err != nil {
		return fmt.Sprintf("Error parsing sdk uri. Url: %s, Error: %s", e.uri, e.err)
	}
	if e.uri == "" {
		return "SDK URI cannot be empty"
	}
	return fmt.Sprintf("Unknown error while parsing the sdk uri: %s", e.uri)
}

// NewErrorParsingURL returns an ErrorParsingURL error.
func NewErrorParsingURL(u string, e error) ErrorParsingURL {
	return ErrorParsingURL{uri: u, err: e}
}

// NewErrorInvalidHost returns an ErrorInvalidHost error.
func NewErrorInvalidHost(h string, d string, n map[string]string) ErrorInvalidHost {
	return ErrorInvalidHost{host: h, ds: d, nw: n}
}

// NewErrorClientFailed returns an ErrorClientFailed error.
func NewErrorClientFailed(e error) ErrorClientFailed {
	return ErrorClientFailed{err: e}
}

// NewErrorObjectNotFound returns an ErrorObjectNotFound error.
func NewErrorObjectNotFound(e error, o string) ErrorObjectNotFound {
	return ErrorObjectNotFound{err: e, obj: o}
}

// NewErrorPropertyRetrieval returns an ErrorPropertyRetrieval error.
func NewErrorPropertyRetrieval(m types.ManagedObjectReference, p []string, e error) ErrorPropertyRetrieval {
	return ErrorPropertyRetrieval{err: e, mor: m, ps: p}
}

// NewErrorBadResponse returns an  ErrorBadResponse error.
func NewErrorBadResponse(r *http.Response) ErrorBadResponse {
	return ErrorBadResponse{resp: r}
}

const (
	// DestinationTypeHost represents an ESXi host in the vSphere inventory.
	DestinationTypeHost = "host"
	// DestinationTypeCluster represents a cluster in the vSphere inventory.
	DestinationTypeCluster = "cluster"
	// DestinationTypeResourcePool represents a resource pool in the vSphere inventory.
	DestinationTypeResourcePool = "resource_pool"
)

type collector interface {
	RetrieveOne(context.Context, types.ManagedObjectReference, []string, interface{}) error
	Retrieve(context.Context, []types.ManagedObjectReference, []string, interface{}) error
}

// Disk represents a vSphere Disk to attach to the VM
type Disk struct {
	Size       int64
	Controller string
}

// Snapshot represents a vSphere snapshot to create
type snapshot struct {
	Name        string
	Description string
	Memory      bool
	Quiesce     bool
}

type finder interface {
	DatacenterList(context.Context, string) ([]*object.Datacenter, error)
	ClusterComputeResourceList(context.Context, string) ([]*object.ClusterComputeResource, error)
	VirtualMachineList(context.Context, string) ([]*object.VirtualMachine, error)
	NetworkList(context.Context, string) ([]object.NetworkReference, error)
	SetDatacenter(*object.Datacenter) *find.Finder
}

type vmwareCollector struct {
	collector *property.Collector
}

func (v vmwareCollector) RetrieveOne(c context.Context, mor types.ManagedObjectReference, ps []string, dst interface{}) error {
	return v.collector.RetrieveOne(c, mor, ps, dst)
}

func (v vmwareCollector) Retrieve(c context.Context, mor []types.ManagedObjectReference, ps []string, dst interface{}) error {
	return v.collector.Retrieve(c, mor, ps, dst)
}

type location struct {
	Host         types.ManagedObjectReference
	ResourcePool types.ManagedObjectReference
	Networks     []types.ManagedObjectReference
}

// Destination represents a destination on which to provision a Virtual Machine
type Destination struct {
	// Represents the name of the destination as described in the API
	DestinationName string
	// Only the "host" type is supported for now. The VI SDK supports host, cluster
	// and resource pool.
	DestinationType string
	// HostSystem specifies the name of the host to run the VM on. DestinationType ESXi
	// will have one host system. A cluster will have more than one,
	HostSystem string
}

// Lease represents a type that wraps around a HTTPNfcLease
type Lease interface {
	HTTPNfcLeaseProgress(int32)
	Wait() (*types.HttpNfcLeaseInfo, error)
	Complete() error
}

type Flavor struct {
	// Represents the number of CPUs
	NumCPUs int32
	// Represents the size of main memory in MB
	MemoryMB int64
}

var _ lvm.VirtualMachine = (*VM)(nil)

// VM represents a vSphere VM.
type VM struct {
	// Host represents the vSphere host to use for creating this VM.
	Host string
	// Destination represents the destination on which to clone this VM.
	Destination Destination
	// Username represents the username to use for connecting to the sdk.
	Username string
	// Password represents the password to use for connecting to the sdk.
	Password string
	// Insecure allows connecting without cert validation when set to true.
	Insecure bool
	// Datacenter configures the datacenter onto which to import the VM.
	Datacenter string
	//Flavor for the number of CPUs and size of main memory
	Flavor Flavor
	// OvfPath represents the location of the OVF file on disk.
	OvfPath string
	// OvaPathUrl represents the location of local/remote ova file
	// If OvaPathUrl is given then OvaPathUrl will be used, if not then OvfPath will be used
	// If Both are given preference will be given to OvaPathUrl.
	OvaPathUrl string
	// Networks defines a mapping from each network label inside the ovf file
	// to a vSphere network. Must be available on the host or deploy will fail.
	Networks map[string]string
	// Name is the name to use for the VM on vSphere and internally.
	Name string
	// Template is the name to use for the VM's template
	Template string
	// Datastores is a slice of permissible datastores. One is picked out of these.
	Datastores []string
	// UseLocalTemplates is a flag to indicate whether a template should be uploaded on all
	// the datastores that were passed in.
	UseLocalTemplates bool
	// SkipExisting when set to '2' lets Provision succeed even if the VM already exists.
	SkipExisting int
	// Credentials are the credentials to use when connecting to the VM over SSH
	Credentials ssh.Credentials
	// Disks is a slice of extra disks to attach to the VM
	Disks []Disk
	// QuestionResponses is a map of regular expressions to match question text
	// to responses when a VM encounters a questions which would otherwise
	// prevent normal operation. The response strings should be the string value
	// of the intended response index.
	QuestionResponses map[string]string
	// UseLinkedClones is a flag to indicate whether VMs cloned from templates should be
	// linked clones.
	UseLinkedClones bool
	uri             *url.URL
	ctx             context.Context
	cancel          context.CancelFunc
	client          *govmomi.Client
	finder          finder
	collector       collector
	datastore       string
}

// Provision provisions this VM.
func (vm *VM) Provision() (err error) {
	if err := SetupSession(vm); err != nil {
		return fmt.Errorf("Error setting up vSphere session: %s", err)
	}

	// Cancel the sdk context
	defer vm.cancel()

	// Get a reference to the datacenter with host and vm folders populated
	dcMo, err := GetDatacenter(vm)
	if err != nil {
		return fmt.Errorf("Failed to retrieve datacenter: %s", err)
	}

	// Upload a template to all the datastores if `UseLocalTemplates` is set.
	// Otherwise pick a random datastore out of the list that was passed in.
	var datastores = vm.Datastores
	if !vm.UseLocalTemplates {
		n := util.Random(1, len(vm.Datastores))
		datastores = []string{vm.Datastores[n-1]}
	}

	usableDatastores := []string{}
	for _, d := range datastores {
		template := createTemplateName(vm.Template, d)
		// Does the VM template already exist?
		e, err := Exists(vm, dcMo, template)
		if err != nil {
			return fmt.Errorf("failed to check if the template already exists: %s", err)
		}

		// If it does exist, return an error if the skip existing is set to 0/SKIPTEMPLATE_ERROR
		if e {
			switch vm.SkipExisting {
			case SKIPTEMPLATE_USE: //PASS
			case SKIPTEMPLATE_ERROR:
				return fmt.Errorf("Template already exists: %s", vm.Template)
			case SKIPTEMPLATE_OVERWRITE:
				fmt.Println("Overwriting Template")
				if err := DeleteTemplate(vm); err != nil {
					return err
				}

				if err := uploadTemplate(vm, dcMo, d); err != nil {
					return err
				}
			default:
				return fmt.Errorf("Unsupported value for SkipExisting parameter %d", vm.SkipExisting)
			}
		} else {
			// Upload the template if  it does not exist. If it exists and SkipExisting is '2',
			// use the existing template
			if err := uploadTemplate(vm, dcMo, d); err != nil {
				return err
			}
		}
		// Upload successful or the template was found with the SkipExisting flag set to true
		usableDatastores = append(usableDatastores, d)
	}

	// Does the VM already exist?
	e, err := Exists(vm, dcMo, vm.Name)
	if err != nil {
		return fmt.Errorf("failed to check if the vm already exists: %s", err)
	}
	if e {
		return ErrorVMExists
	}

	err = cloneFromTemplate(vm, dcMo, usableDatastores)
	if err != nil {
		return fmt.Errorf("error while cloning vm from template: %s", err)
	}
	return
}

// GetName returns the name of this VM.
func (vm *VM) GetName() string {
	return vm.Name
}

// AddDisk to the vm
func (vm *VM) AddDisk() ([]string, error) {
	if err := SetupSession(vm); err != nil {
		return nil, fmt.Errorf("Error setting up vSphere session: %s", err)
	}

	// Cancel the sdk context
	defer vm.cancel()

	// Get a reference to the datacenter with host and vm folders populated
	dcMo, err := GetDatacenter(vm)
	if err != nil {
		return nil, fmt.Errorf("Failed to retrieve datacenter: %s", err)
	}

	// Finds the vm with name vm.Name
	vmMo, err := findVM(vm, dcMo, vm.Name)
	if err != nil {
		return nil, fmt.Errorf("VM not found", vm.Name, err)
	}

	// Gets a random datastore from the list of datastores to create disk
	n := util.Random(1, len(vm.Datastores))
	vm.datastore = vm.Datastores[n-1]

	// Reconfigures vm with the new Disk
	diskList, err := reconfigureVM(vm, vmMo)
	if err != nil {
		return nil, fmt.Errorf("Reconfigure failed : ", err)
	}
	return diskList, nil
}

// RemoveDisk removes the disk attached to the virtualmachine 'vm', vmdkName is the name of the vmdk file for the disk
func (vm *VM) RemoveDisk(vmdkFiles []string) error {
	var errorMessage string
	if err := SetupSession(vm); err != nil {
		return fmt.Errorf("Error setting up vSphere session: %s", err)
	}

	// Cancel the sdk context
	defer vm.cancel()

	// Get a reference to the datacenter with host and vm folders populated
	dcMo, err := GetDatacenter(vm)
	if err != nil {
		return fmt.Errorf("Failed to retrieve datacenter: %s", err)
	}

	for _, vmdkName := range vmdkFiles {
		// finds the virtualmachine with name vm.Name
		vmMo, err := findVM(vm, dcMo, vm.Name)
		if err != nil {
			return fmt.Errorf("VM not found", vm.Name, err)
		}

		// find the virtual disk to be removed from the vm
		var deviceMo *types.VirtualDisk
		for _, d := range vmMo.Config.Hardware.Device {
			switch device := d.(type) {
			case *types.VirtualDisk:
				fileName := d.GetVirtualDevice().Backing.(types.BaseVirtualDeviceFileBackingInfo).GetVirtualDeviceFileBackingInfo().FileName
				if strings.HasSuffix(fileName, vmdkName) {
					deviceMo = device
					break
				}
			}
		}

		if deviceMo == nil {
			errorMessage += fmt.Sprintf("%s : No disk with name\n", vmdkName)
			continue
		}

		// Creates the virtualmachine object to remove the disk
		vmo := object.NewVirtualMachine(vm.client.Client, vmMo.Reference())
		if err = vmo.RemoveDevice(vm.ctx, false, deviceMo); err != nil {
			errorMessage += fmt.Errorf("%s : Delete disk task returned an error : %s \n", vmdkName, err).Error()
		}
	}
	if errorMessage != "" {
		return errors.New(errorMessage)
	}
	return nil
}

// GetIPs returns the IPs of this VM. Returns all the IPs known to the API for
// the different network cards for this VM. Includes IPV4 and IPV6 addresses.
func (vm *VM) GetIPs() ([]net.IP, error) {
	if err := SetupSession(vm); err != nil {
		return nil, err
	}
	defer vm.cancel()

	// Get a reference to the datacenter with host and vm folders populated
	dcMo, err := GetDatacenter(vm)
	if err != nil {
		return nil, err
	}
	vmMo, err := findVM(vm, dcMo, vm.Name)
	if err != nil {
		return nil, err
	}
	// Lazy initialized when there is an IP address later.
	var ips []net.IP
	for _, nic := range vmMo.Guest.Net {
		for _, ip := range nic.IpAddress {
			netIP := net.ParseIP(ip)
			if netIP == nil {
				continue
			}
			if ips == nil {
				ips = make([]net.IP, 0, 1)
			}
			ips = append(ips, netIP)
		}
	}
	if ips == nil && vmMo.Guest.IpAddress != "" {
		ip := net.ParseIP(vmMo.Guest.IpAddress)
		if ip != nil {
			ips = append(ips, ip)
		}
	}
	return ips, nil
}

// Destroy deletes this VM from vSphere.
func (vm *VM) Destroy() (err error) {
	if err := SetupSession(vm); err != nil {
		return err
	}
	defer vm.cancel()

	state, err := getState(vm)
	if err != nil {
		return err
	}

	// Can't destroy a suspended VM, power it on and update the state
	if state == "standby" {
		err = start(vm)
		if err != nil {
			return err
		}
	}

	if state != "notRunning" {
		// Only possible states are running, shuttingDown, resetting or notRunning
		timer := time.NewTimer(time.Second * 90)
		wg := sync.WaitGroup{}
		wg.Add(1)

		go func() {
			defer timer.Stop()
			defer wg.Done()
		Outerloop:
			for {
				state, e := getState(vm)
				if e != nil {
					err = e
					break
				}
				if state == "notRunning" {
					break
				}

				if state == "running" {
					e = halt(vm)
					if e != nil {
						err = e
						break
					}
				}

				select {
				case <-timer.C:
					err = fmt.Errorf("timed out waiting for VM to power off")
					break Outerloop
				default:
					// No action
				}
				time.Sleep(time.Second)
			}
		}()
		wg.Wait()
		if err != nil {
			return err
		}
	}

	// Get a reference to the datacenter with host and vm folders populated
	dcMo, err := GetDatacenter(vm)
	if err != nil {
		return err
	}
	vmMo, err := findVM(vm, dcMo, vm.Name)
	if err != nil {
		return err
	}
	vmo := object.NewVirtualMachine(vm.client.Client, vmMo.Reference())
	destroyTask, err := vmo.Destroy(vm.ctx)
	if err != nil {
		return fmt.Errorf("error creating a destroy task on the vm: %s", err)
	}
	tInfo, err := destroyTask.WaitForResult(vm.ctx, nil)
	if err != nil {
		return fmt.Errorf("error waiting for destroy task: %s", err)
	}
	if tInfo.Error != nil {
		return fmt.Errorf("destroy task returned an error: %s", err)
	}
	return nil
}

// GetState returns the power state of this VM.
func (vm *VM) GetState() (state string, err error) {
	if err := SetupSession(vm); err != nil {
		return "", lvm.ErrVMInfoFailed
	}
	defer vm.cancel()

	state, err = getState(vm)
	if err != nil {
		return "", err
	}

	if state == "running" {
		return lvm.VMRunning, nil
	} else if state == "standby" {
		return lvm.VMSuspended, nil
	} else if state == "shuttingDown" || state == "resetting" || state == "notRunning" {
		return lvm.VMHalted, nil
	}
	// VM state "unknown"
	return "", lvm.ErrVMInfoFailed
}

// Suspend suspends this VM.
func (vm *VM) Suspend() (err error) {
	if err := SetupSession(vm); err != nil {
		return err
	}
	defer vm.cancel()

	// Get a reference to the datacenter with host and vm folders populated
	dcMo, err := GetDatacenter(vm)
	if err != nil {
		return err
	}
	vmMo, err := findVM(vm, dcMo, vm.Name)
	if err != nil {
		return err
	}
	vmo := object.NewVirtualMachine(vm.client.Client, vmMo.Reference())
	suspendTask, err := vmo.Suspend(vm.ctx)
	if err != nil {
		return fmt.Errorf("error creating a suspend task on the vm: %s", err)
	}
	tInfo, err := suspendTask.WaitForResult(vm.ctx, nil)
	if err != nil {
		return fmt.Errorf("error waiting for suspend task: %s", err)
	}
	if tInfo.Error != nil {
		return fmt.Errorf("suspend task returned an error: %s", err)
	}
	return nil
}

// Halt halts this VM.
func (vm *VM) Halt() (err error) {
	if err := SetupSession(vm); err != nil {
		return err
	}
	defer vm.cancel()
	return halt(vm)
}

// Start powers on this VM.
func (vm *VM) Start() (err error) {
	if err := SetupSession(vm); err != nil {
		return err
	}
	defer vm.cancel()
	return start(vm)
}

// Resume resumes this VM from a suspended or powered off state.
func (vm *VM) Resume() (err error) {
	return vm.Start()
}

// GetSSH returns an ssh client configured for this VM.
func (vm *VM) GetSSH(options ssh.Options) (ssh.Client, error) {
	ips, err := util.GetVMIPs(vm, options)
	if err != nil {
		return nil, err
	}

	client := ssh.SSHClient{Creds: &vm.Credentials, IP: ips[0], Port: 22, Options: options}
	return &client, nil
}

// DeleteTemplate deletes the vm-template, created during vm provisioning
func DeleteTemplate(vm *VM) error {
	// for the templates that do not exist in server
	missingTemplates := make([]string, 0)
	if err := SetupSession(vm); err != nil {
		return err
	}
	dcMo, err := GetDatacenter(vm)
	if err != nil {
		return fmt.Errorf("Failed to retrieve datacenter: %s", err)
	}
	// find and delete vm-templates from all provided datastores
	for _, datastore := range vm.Datastores {
		// generate template name <provided-name>-<datastore-name>
		template := createTemplateName(vm.Template, datastore)
		// finds the template vm in Host specified in vm.Destination in Datacenter dcMo
		templateVm, err := findVM(vm, dcMo, template)
		if err != nil {
			// add to missing templates list if it doesn't exist or in case of error
			missingTemplates = append(missingTemplates, template)
			continue
		}
		// create vm object for found vm-template and calls destroy function on the vm
		vmo := object.NewVirtualMachine(vm.client.Client, templateVm.Reference())
		task, err := vmo.Destroy(vm.ctx)
		if err != nil {
			return err
		}
		// wait for the task to complete and checks for the errors if any
		tInfo, err := task.WaitForResult(vm.ctx, nil)
		if err != nil {
			return fmt.Errorf("Error waiting for task : %s", err)
		}
		if tInfo.Error != nil {
			return fmt.Errorf("Destroy task returned error : %s", tInfo.Error)
		}
	}
	//  If there are any missing templates, return error
	if len(missingTemplates) != 0 {
		return fmt.Errorf("Following templates not found.\n[ %s ].\nHowever any found templates are deleted", strings.Join(missingTemplates, ", "))
	}
	return nil
}

// GetDcNetworkList : GetDcNetworkList returns the list of networks in
// all the datacenters in vcenter server
func GetDcNetworkList(vm *VM) (map[string][]string, error) {
	networkList := map[string][]string{}
	// set up session to vcenter server
	if err := SetupSession(vm); err != nil {
		return nil, err
	}
	// get datacenter list in the vcenter server
	dcList, err := vm.finder.DatacenterList(vm.ctx, "*")
	if err != nil {
		return nil, err
	}

	// for all datacenters in the vcenter server
	for _, dc := range dcList {
		// Set datacenter
		vm.finder.SetDatacenter(dc)
		// find the networks in selected datacenter
		allNetworks, err := vm.finder.NetworkList(vm.ctx, "*")
		if err != nil {
			switch err.(type) {
			case *find.NotFoundError:
				networkList[dc.Name()] = []string{}
				continue
			}
			return nil, err
		}

		var networksMor []types.ManagedObjectReference
		for _, network := range allNetworks {
			networksMor = append(networksMor, network.Reference())
		}

		// get the network names
		var network mo.Network
		for _, networkMor := range networksMor {
			switch networkMor.Type {
			case "Network":
				err = vm.collector.RetrieveOne(vm.ctx, networkMor, []string{"name"}, &network)
				if err != nil {
					return nil, err
				}
				networkList[dc.Name()] = append(networkList[dc.Name()], network.Name)
			}
		}
	}
	return networkList, nil
}

// GetDcImageList : GetDcImageList returns the list of images in
// all the datacenters in vcenter server
func GetDcImageList(vm *VM) (map[string][]string, error) {
	imageList := map[string][]string{}
	// set up session to vcenter server
	if err := SetupSession(vm); err != nil {
		return nil, err
	}
	// get datacenter list in the vcenter server
	dcList, err := vm.finder.DatacenterList(vm.ctx, "*")
	if err != nil {
		return nil, err
	}

	// for all datacenters in the vcenter server
	for _, dc := range dcList {
		// Set datacenter
		vm.finder.SetDatacenter(dc)
		// find the virtualmachines in selected datacenter
		allVms, err := vm.finder.VirtualMachineList(vm.ctx, "*")
		if err != nil {
			switch err.(type) {
			case *find.NotFoundError:
				imageList[dc.Name()] = []string{}
				continue
			}
			return nil, err
		}
		var vmsMor []types.ManagedObjectReference
		for _, vm := range allVms {
			vmsMor = append(vmsMor, vm.Reference())
		}
		// get the vm names and config
		var allVmsMo []mo.VirtualMachine
		err = vm.collector.Retrieve(vm.ctx, vmsMor, []string{"name", "config"}, &allVmsMo)
		if err != nil {
			return nil, err
		}
		// generate response for the images in datacenter. In the response map
		// the key is the datacenter name and value is the list of images in datacenter
		for _, vmMo := range allVmsMo {
			if vmMo.Config.Template {
				imageList[dc.Name()] = append(imageList[dc.Name()], vmMo.Name)
			}
		}
	}
	return imageList, nil
}

// GetDcClusterList : GetDcClusterList returns the datacenter and the clusters in the datacenter
func GetDcClusterList(vm *VM) (map[string][]string, error) {
	dcClusterList := map[string][]string{}
	// setupSession
	if err := SetupSession(vm); err != nil {
		return nil, err
	}

	// get datacenter list in the vcenter server
	dcList, err := vm.finder.DatacenterList(vm.ctx, "*")
	if err != nil {
		return nil, err
	}

	// for all datacenters in the vcenter server
	for _, dc := range dcList {
		// Set datacenter
		vm.finder.SetDatacenter(dc)
		// find the clusters in selected datacenter
		allClusters, err := vm.finder.ClusterComputeResourceList(vm.ctx, "*")
		if err != nil {
			switch err.(type) {
			case *find.NotFoundError:
				dcClusterList[dc.Name()] = []string{}
				continue
			}
			return nil, err
		}
		var clustersMor []types.ManagedObjectReference
		for _, cluster := range allClusters {
			clustersMor = append(clustersMor, cluster.Reference())
		}
		// get the cluster names
		var allClustersMo []mo.ClusterComputeResource
		err = vm.collector.Retrieve(vm.ctx, clustersMor, []string{"name"}, &allClustersMo)
		if err != nil {
			return nil, err
		}
		// generate response for the cluster in datacenter. In the response map
		// the key is the datacenter name and value is the list of clusters in datacenter
		for _, cluster := range allClustersMo {
			dcClusterList[dc.Name()] = append(dcClusterList[dc.Name()], cluster.Name)
		}
	}
	return dcClusterList, err
}

// GetDatacenterList : return the list of datacenters in vcenter server
func GetDatacenterList(vm *VM) ([]string, error) {
	var (
		dcList  []string
		dcMor   []types.ManagedObjectReference
		allDcMo []mo.Datacenter
	)

	// set up session to vcenter server
	if err := SetupSession(vm); err != nil {
		return nil, err
	}

	// Get datacenter list in the vcenter server
	allDcs, err := vm.finder.DatacenterList(vm.ctx, "*")
	if err != nil {
		switch err.(type) {
		case *find.NotFoundError:
		default:
			return nil, err
		}
	}

	for _, dc := range allDcs {
		dcMor = append(dcMor, dc.Reference())
	}

	// get the datacenter names
	err = vm.collector.Retrieve(vm.ctx, dcMor, []string{"name"}, &allDcMo)
	if err != nil {
		return nil, err
	}

	for _, dc := range allDcMo {
		dcList = append(dcList, dc.Name)
	}

	return dcList, nil
}
