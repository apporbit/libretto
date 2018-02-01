// Copyright 2015 Apcera Inc. All rights reserved.

package vmrun

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	libssh "github.com/apcera/libretto/ssh"
	"github.com/apcera/libretto/util"
	lvm "github.com/apcera/libretto/virtualmachine"
)

// Backing information for Fusion network cards
const (
	Nat     Backing = iota // Nat network card. Addressable from guest os
	Bridged                // Connected to the outside network. Also addressable from the guest AND outside hosts.
	Unsupported
)

var nicTemplate = `ethernet{{.Idx}}.addresstype = "generated"
ethernet{{.Idx}}.bsdname = "{{.BackingDevice}}"
ethernet{{.Idx}}.connectiontype = "{{.Backing}}"
ethernet{{.Idx}}.displayname = "Ethernet"
ethernet{{.Idx}}.present = "TRUE"
ethernet{{.Idx}}.virtualdev = "vmxnet3"
`

const vmrunTimeout = 90 * time.Second

// ErrVmrunTimeout is returned when vmrun doesn't finish executing in `vmrunTimeout` seconds.
var ErrVmrunTimeout = errors.New("Timed out waiting for vmrun")

// ErrVmrunNotFound is returned when no vmrun was found in host.
var ErrVmrunNotFound = errors.New("Failed to find vmrun")

// Regular expression to parse the VMX file
var ethernetRegexp = regexp.MustCompile(`ethernet.*\n`)

var runner Runner = vmrunRunner{}

// Backing is the network card backing type for VMware virtual machines.
type Backing int

// Config is a config struct that can be passed in to change the configuration of the vm being provisioned.
type Config struct {
	NICs []NIC
}

// NIC is represents a network card on a VMware vm
type NIC struct {
	Idx           int     // Which network card to change on the vm. Starts at 1
	Backing       Backing // What type of backing should the card have (Bridged vs NAT)
	BackingDevice string  // BSD string for the network card (en0, en1)
}

// Runner is an encapsulation around the vmrun utility.
type Runner interface {
	Run(args ...string) (string, string, error)
	RunCombinedError(args ...string) (string, error)
}

// vmrunRunner implements the Runner interface.
type vmrunRunner struct {
}

// Run runs a vmrun command.
func (f vmrunRunner) Run(args ...string) (string, string, error) {
	var vmrunPath string

	// If vmrun is not found in the system path, fall back to the
	// hard coded path (VMRunPath).
	path, err := exec.LookPath("vmrun")
	if err == nil {
		vmrunPath = path
	} else {
		vmrunPath = VMRunPath
	}

	if vmrunPath == "" {
		return "", "", ErrVmrunNotFound
	}

	cmd := exec.Command(vmrunPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	err = cmd.Start()
	timer := time.AfterFunc(vmrunTimeout, func() {
		cmd.Process.Kill()
		err = ErrVmrunTimeout
	})
	e := cmd.Wait()
	timer.Stop()

	if err != nil || e != nil {
		err = lvm.WrapErrors(err, e)
	}
	return stdout.String(), stderr.String(), err
}

// RunCombinedError runs a vmrun command.  The output is stdout and the the
// combined err/stderr from the command.
func (f vmrunRunner) RunCombinedError(args ...string) (string, error) {
	wout, werr, err := f.Run(args...)
	if err != nil {
		if werr != "" {
			return wout, fmt.Errorf("%s: %s", err, werr)
		}
		return wout, err
	}

	return wout, nil
}

// VM represents a single VMware VM and all the operations for provisioning that type
type VM struct {
	Name        string
	Src         string
	Dst         string
	VmxFilePath string
	ips         []net.IP
	Credentials libssh.Credentials
	Config      Config
}

var backingList = []string{"nat", "bridged"}

// GetName returns the name of the virtual machine
func (vm *VM) GetName() string {
	return vm.Name
}

// GetSSH returns an ssh client for the the vm.
func (vm *VM) GetSSH(options libssh.Options) (libssh.Client, error) {
	ips, err := util.GetVMIPs(vm, options)
	if err != nil {
		return nil, err
	}
	vm.ips = ips

	client := libssh.SSHClient{Creds: &vm.Credentials, IP: ips[0], Port: 22, Options: options}
	return &client, nil
}

// Destroy powers off the VM and deletes its files from disk.
func (vm *VM) Destroy() (err error) {
	err = vm.haltWithFlag(true)
	if err != nil {
		return err
	}
	if vm.Dst != "" {
		err = os.RemoveAll(vm.Dst)
		if err != nil {
			err = lvm.ErrDeletingVM
			return
		}
	}
	return
}

func (vm *VM) haltWithFlag(hard bool) error {
	src := vm.Src
	dst := vm.Dst

	_, vmxFileName := filepath.Split(src)
	vm.VmxFilePath = fmt.Sprintf("%s/%s", dst, vmxFileName)

	// FIXME: Cannot use nogui flag here, it breaks vmrun's getGuestIP
	// functionality.
	flag := "soft"
	if hard {
		flag = "hard"
	}

	_, err := runner.RunCombinedError("stop", vm.VmxFilePath, flag)
	return err
}

// Halt powers off the VM without destroying it
func (vm *VM) Halt() error {
	return vm.haltWithFlag(false)
}

// Suspend suspends the active state of the VM.
func (vm *VM) Suspend() error {
	src := vm.Src
	dst := vm.Dst

	_, vmxFileName := filepath.Split(src)
	vm.VmxFilePath = fmt.Sprintf("%s/%s", dst, vmxFileName)

	// FIXME: Cannot use nogui flag here, it breaks vmrun's getGuestIP
	// functionality.
	_, err := runner.RunCombinedError("suspend", vm.VmxFilePath)
	return err
}

// Resume suspends the active state of the VM.
func (vm *VM) Resume() error {
	return vm.Start()
}

// Start powers on the VM
func (vm *VM) Start() error {
	src := vm.Src
	dst := vm.Dst

	_, vmxFileName := filepath.Split(src)
	vm.VmxFilePath = fmt.Sprintf("%s/%s", dst, vmxFileName)

	// FIXME: Cannot use nogui flag here, it breaks vmrun's getGuestIP
	// functionality.
	out, err := runner.RunCombinedError("start", vm.VmxFilePath)
	if err != nil {
		return lvm.WrapErrors(err, errors.New(out))
	}

	return nil
}

// GetIPs returns a list of ip addresses associated with the vm through VMware tools
func (vm *VM) GetIPs() ([]net.IP, error) {
	vm.waitUntilReady()

	return vm.ips, nil
}

// GetState gets the power state of the VM through VMware tools.
func (vm *VM) GetState() (string, error) {
	stdout, stderr, err := runner.Run("list")
	if err != nil {
		return "", err
	}
	if stderr != "" {
		return "", fmt.Errorf("Failed to get state using the vmrun utility: %s", stderr)
	}

	if strings.Contains(stdout, vm.Dst) {
		return lvm.VMRunning, nil
	}

	// Maybe vm.Dst is a symlink?
	p, err := filepath.EvalSymlinks(vm.Dst)
	if err != nil {
		return "", err
	}

	absp, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}

	if strings.Contains(stdout, absp) {
		return lvm.VMRunning, nil
	}

	// Originally reported these as VMHalted, but for our purposes it was better
	// to list them as VMSuspended so that we did not assume they would start up
	// in an initial state.
	return lvm.VMSuspended, nil
}

// Provision clones this VM and powers it on, while waiting for it to get an IP address.
// FIXME (Preet): Should make the wait for IP part optional.
func (vm *VM) Provision() error {
	src := vm.Src
	dst := vm.Dst

	if src == "" {
		return lvm.ErrSourceNotSpecified
	}

	if dst == "" {
		return lvm.ErrDestNotSpecified
	}

	srcPath, _ := filepath.Abs(filepath.Dir(src))
	srcPath += "/"

	// Check if the path exists, if not try to create it.
	if _, err := os.Stat(dst); err != nil {
		if os.IsNotExist(err) {
			dir := os.Mkdir(dst, 0777)

			if dir != nil {
				return dir
			}
		} else {
			return err
		}
	} else {
		return lvm.ErrCreatingVM
	}

	// Copy over the source path to the destination.
	err := copyDir(srcPath, dst)
	if err != nil {
		return err
	}

	_, vmxFileName := filepath.Split(src)
	vm.VmxFilePath = fmt.Sprintf("%s/%s", dst, vmxFileName)

	err = vm.configure()
	if err != nil {
		return err
	}

	_, _, err = runVMware()
	if err != nil {
		return err
	}

	return vm.waitUntilReady()
}

func (vm *VM) configure() error {
	b, err := ioutil.ReadFile(vm.VmxFilePath)
	if err != nil {
		return err
	}

	vmxString := string(b)
	newVmxString := ethernetRegexp.ReplaceAllString(vmxString, "")

	for _, nic := range vm.Config.NICs {
		var b bytes.Buffer

		data := struct {
			Idx           int
			BackingDevice string
			Backing       string
		}{
			nic.Idx,
			nic.BackingDevice,
			backingList[nic.Backing],
		}

		tmpl, err := template.New("nicTemplate").Parse(nicTemplate)
		if err != nil {
			log.Println(err)
			return err
		}

		err = tmpl.Execute(&b, data)
		if err != nil {
			log.Println(err)
			return err
		}

		newVmxString += b.String()
	}

	return ioutil.WriteFile(vm.VmxFilePath, []byte(newVmxString), 0755)
}

// This function makes a single request to get IPs from a VM.
func (vm *VM) requestIPs() []net.IP {
	ips := []net.IP{}
	// FIXME: Cannot use nogui flag here, it breaks vmrun's getGuestIP
	// functionality.
	stdout, _, _ := runner.Run("getGuestIPAddress", vm.VmxFilePath, "wait")
	if stdout != "" {
		if ip := net.ParseIP(strings.TrimSpace(stdout)); ip != nil {
			ips = append(ips, ip)
		}
	}

	vm.ips = ips

	return ips
}

func (vm *VM) waitUntilReady() error {
	errorChannel := make(chan error, 1)
	// Wait up to 90s until the VM boots up
	timer := time.NewTimer(time.Second * 90)
	go func() {
		err := vm.Start()
		errorChannel <- err
	}()

ForLoop:
	for {
		select {
		case e := <-errorChannel:
			timer.Stop()
			if e != nil {
				return e
			}
			break ForLoop
		case <-timer.C:
			return lvm.ErrVMBootTimeout
		}
	}

	var wg sync.WaitGroup
	quit := make(chan bool, 1)
	success := make(chan bool, 1)
	timer = time.NewTimer(time.Second * 90)

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-quit:
				success <- false
				return
			default:
				ips := vm.requestIPs()

				if len(ips) == 0 {
					time.Sleep(2 * time.Second)
					continue
				} else {
					success <- true
					return
				}
			}
		}
	}()

	var r error
OuterLoop:
	for {
		select {
		case s := <-success:
			if !s {
				r = lvm.ErrVMNoIP
			}
			timer.Stop()
			break OuterLoop
		case <-timer.C:
			quit <- true
			r = lvm.ErrVMBootTimeout
			break OuterLoop
		}
	}

	wg.Wait()

	return r
}

func (vm *VM) AddDisk() error {
        return errors.New("TO-DO")
}

func (vm *VM) RemoveDisk(diskName string) error {
        return errors.New("TO-DO")
}
