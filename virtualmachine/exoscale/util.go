package exoscale

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/pyr/egoscale"
)

func (vm *VM) getExoClient() *egoscale.Client {
	return egoscale.NewClient(vm.Config.Endpoint, vm.Config.APIKey, vm.Config.APISecret)
}

// WaitVMCreation waits for the virtual machine to be created, and stores the virtual machine ID
// VM structure must contain a valid JobID.
func (vm *VM) WaitVMCreation(timeoutSeconds int, pollIntervalSeconds int) error {

	if vm.JobID == "" {
		return fmt.Errorf("No JobID informed. Cannot poll machine creation state")
	}

	jobCh := make(chan *egoscale.QueryAsyncJobResultResponse, 1)
	errCh := make(chan error, 1)

	go func() {
		params := url.Values{}
		params.Set("jobid", vm.JobID)

		for {
			client := vm.getExoClient()
			resp, err := client.Request("queryAsyncJobResult", params)
			if err != nil {
				errCh <- err
			}

			jobResult := &egoscale.QueryAsyncJobResultResponse{}
			err = json.Unmarshal(resp, jobResult)
			if err != nil {
				errCh <- err
			}

			if jobResult.Jobstatus == 1 {
				jobCh <- jobResult
				break
			}
			time.Sleep(time.Duration(pollIntervalSeconds) * time.Second)
		}
	}()

	select {
	case jobResult := <-jobCh:
		var vmWrap egoscale.DeployVirtualMachineWrappedResponse
		if err := json.Unmarshal(jobResult.Jobresult, &vmWrap); err != nil {
			return err
		}
		vm.ID = vmWrap.Wrapped.Id
	case err := <-errCh:
		return err
	case <-time.After(time.Duration(timeoutSeconds) * time.Second):
		return fmt.Errorf("Create VM Job has not completed after %d seconds", timeoutSeconds)
	}

	return nil
}

// fillTemplateID fills the template identifier based on name, storage and zone name.
// If no matching template is found, ID remains unchanged and an error is returned.
func (vm *VM) fillTemplateID() error {

	params := url.Values{}
	params.Set("Name", vm.Template.Name)
	params.Set("templatefilter", "featured")

	client := vm.getExoClient()
	resp, err := client.Request("listTemplates", params)
	if err != nil {
		return fmt.Errorf("Getting template ID for '%s/%d/%s': %s", vm.Template.Name, vm.Template.StorageGB, vm.Template.ZoneName, err)
	}

	templates := &egoscale.ListTemplatesResponse{}
	if err := json.Unmarshal(resp, templates); err != nil {
		return fmt.Errorf("Decoding response for template '%s/%d/%s': %s", vm.Template.Name, vm.Template.StorageGB, vm.Template.ZoneName, err)
	}

	// iterate templates to get ID matching size and zone name
	storage := int64(vm.Template.StorageGB) << 30
	zoneName := strings.ToLower(vm.Template.ZoneName)

	for _, template := range templates.Templates {
		if template.Size == storage {
			if template.Zonename == zoneName {
				vm.Template.ID = template.Id
				return nil
			}
		}
	}

	return fmt.Errorf("Template ID for '%s/%d/%s' could not be found", vm.Template.Name, vm.Template.StorageGB, vm.Template.ZoneName)
}

// fillServiceOfferingID fills the service offering identifier based on name.
// If no matching service offering is found, ID remains unchanged and an error is returned.
func (vm *VM) fillServiceOfferingID() error {

	params := url.Values{}
	params.Set("name", strings.ToLower(string(vm.ServiceOffering.Name)))

	client := vm.getExoClient()
	resp, err := client.Request("listServiceOfferings", params)
	if err != nil {
		return fmt.Errorf("Getting service offering ID for %q: %s", vm.ServiceOffering.Name, err)
	}

	so := &egoscale.ListServiceOfferingsResponse{}
	if err := json.Unmarshal(resp, so); err != nil {
		return fmt.Errorf("Decoding response for service offering %q: %s", vm.ServiceOffering.Name, err)
	}

	if so.Count != 1 {
		return fmt.Errorf("Expected 1 service offering matching %q, but returned %d", vm.ServiceOffering.Name, so.Count)
	}

	vm.ServiceOffering.ID = so.ServiceOfferings[0].Id

	return nil
}

// fillSecurityGroupsID fills the security group identifiers based on name.
// If a security group already has the ID, it will remain unchanged.
// If any of the security groups is not founds error is returned.
func (vm *VM) fillSecurityGroupsID() error {

	params := url.Values{}

	client := vm.getExoClient()
	resp, err := client.Request("listSecurityGroups", params)
	if err != nil {
		return fmt.Errorf("Getting security groups: %s", err)
	}

	sgRemotes := &egoscale.ListSecurityGroupsResponse{}
	if err := json.Unmarshal(resp, sgRemotes); err != nil {
		return fmt.Errorf("Decoding response for security groups: %s", err)
	}

	for i, sg := range vm.SecurityGroups {

		if sg.ID != "" {
			continue
		}

		newID := ""
		for _, sgRemote := range sgRemotes.SecurityGroups {
			if sg.Name == sgRemote.Name {
				newID = sgRemote.Id
				break
			}
		}

		if newID == "" {
			return fmt.Errorf("Could not find security group ID for %q", sg.Name)
		}
		vm.SecurityGroups[i].ID = newID
	}

	return nil
}

// fillZoneID fills the zone identifier based on name.
// If no matching zone is found, ID remains unchanged and an error is returned.
func (vm *VM) fillZoneID() error {

	params := url.Values{}
	params.Set("name", strings.ToLower(string(vm.Zone.Name)))

	client := vm.getExoClient()
	resp, err := client.Request("listZones", params)
	if err != nil {
		return fmt.Errorf("Getting zones ID for %q: %s", vm.ServiceOffering.Name, err)
	}

	zones := &egoscale.ListZonesResponse{}
	if err := json.Unmarshal(resp, zones); err != nil {
		return fmt.Errorf("Decoding response for zones list: %s", err)
	}

	for _, zone := range zones.Zones {
		if zone.Name == vm.Zone.Name {
			vm.Zone.ID = zone.Id
			return nil
		}
	}

	return fmt.Errorf("Zone ID for %q could not be found", vm.Zone.Name)
}

func (vm *VM) updateInfo() error {

	if vm.ID == "" {
		return fmt.Errorf("Need an ID to retrieve virtual machine Info")
	}

	params := url.Values{}
	params.Set("id", vm.ID)

	client := vm.getExoClient()
	resp, err := client.Request("listVirtualMachines", params)
	if err != nil {
		return fmt.Errorf("Listing virtual machine %q to update info: %s", vm.ID, err)
	}

	listVM := &egoscale.ListVirtualMachinesResponse{}
	if err := json.Unmarshal(resp, listVM); err != nil {
		return fmt.Errorf("Listing virtual machine %q to update info: %s", vm.ID, err)
	}

	if listVM.Count != 1 {
		return fmt.Errorf("Expected 1 virtual machine in listing matching %q, but returned %d", vm.ID, listVM.Count)
	}

	vm.Template.ID = listVM.VirtualMachines[0].Templateid
	vm.Template.Name = listVM.VirtualMachines[0].Templatename
	vm.Template.ZoneName = listVM.VirtualMachines[0].Zonename

	vm.ServiceOffering.ID = listVM.VirtualMachines[0].Serviceofferingid
	vm.ServiceOffering.Name = ServiceOfferingType(listVM.VirtualMachines[0].Serviceofferingname)

	vm.Name = listVM.VirtualMachines[0].Displayname

	// suppose 1 ip per nic, and grow capacity using append if necessary
	ips := make([]net.IP, 0, len(listVM.VirtualMachines[0].Nic))
	for _, nic := range listVM.VirtualMachines[0].Nic {
		if nic.Ipaddress != "" {
			ips = append(ips, net.ParseIP(nic.Ipaddress))
		}
		if nic.Ip6address != "" {
			ips = append(ips, net.ParseIP(nic.Ip6address))
		}
	}

	vm.ips = ips
	vm.KeypairName = listVM.VirtualMachines[0].Keypair
	vm.state = listVM.VirtualMachines[0].State

	return nil
}
