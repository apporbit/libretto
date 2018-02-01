// Copyright 2016 Apcera Inc. All rights reserved.

package arm

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"strconv"
	"time"

	armStorage "github.com/Azure/azure-sdk-for-go/arm/storage"
	lvm "github.com/apcera/libretto/virtualmachine"

	"github.com/Azure/azure-sdk-for-go/arm/network"
	"github.com/Azure/azure-sdk-for-go/arm/resources/resources"
	"github.com/Azure/azure-sdk-for-go/storage"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/adal"
	"github.com/Azure/go-autorest/autorest/azure"
)

// getServicePrincipalToken retrieves a new ServicePrincipalToken using values of the
// passed credentials map.
func getServicePrincipalToken(creds *OAuthCredentials, scope string) (*adal.ServicePrincipalToken, error) {
	oauthConfig, err := adal.NewOAuthConfig(azure.PublicCloud.ActiveDirectoryEndpoint, creds.TenantID)
	if err != nil {
		return nil, err
	}
	return adal.NewServicePrincipalToken(*oauthConfig, creds.ClientID, creds.ClientSecret, scope)
}

type armParameter struct {
	Value string `json:"value"`
}

type armParameters struct {
	AdminUsername        *armParameter `json:"username,omitempty"`
	AdminPassword        *armParameter `json:"password,omitempty"`
	ImageOffer           *armParameter `json:"image_offer,omitempty"`
	ImagePublisher       *armParameter `json:"image_publisher,omitempty"`
	ImageSku             *armParameter `json:"image_sku,omitempty"`
	NetworkSecurityGroup *armParameter `json:"network_security_group,omitempty"`
	NicName              *armParameter `json:"nic,omitempty"`
	OSFileName           *armParameter `json:"os_file,omitempty"`
	PublicIPName         *armParameter `json:"public_ip,omitempty"`
	SSHAuthorizedKey     *armParameter `json:"ssh_authorized_key,omitempty"`
	SubnetName           *armParameter `json:"subnet,omitempty"`
	VirtualNetworkName   *armParameter `json:"virtual_network,omitempty"`
	StorageAccountName   *armParameter `json:"storage_account,omitempty"`
	StorageContainerName *armParameter `json:"storage_container,omitempty"`
	VMSize               *armParameter `json:"vm_size,omitempty"`
	VMName               *armParameter `json:"vm_name,omitempty"`
	DiskSize             *armParameter `json:"disk_size,omitempty"`
	DiskFile             *armParameter `json:"disk_file,omitempty"`
	AdditionalDisk       *armParameter `json:"additional_disk,omitempty"`
}

// Translates the given VM to arm parameters
func (vm *VM) toARMParameters() *armParameters {
	out := &armParameters{
		AdminUsername:        &armParameter{vm.SSHCreds.SSHUser},
		AdminPassword:        &armParameter{vm.SSHCreds.SSHPassword},
		ImageOffer:           &armParameter{vm.ImageOffer},
		ImagePublisher:       &armParameter{vm.ImagePublisher},
		ImageSku:             &armParameter{vm.ImageSku},
		NetworkSecurityGroup: &armParameter{vm.NetworkSecurityGroup},
		NicName:              &armParameter{vm.Nic},
		OSFileName:           &armParameter{vm.OsFile},
		PublicIPName:         &armParameter{vm.PublicIP},
		SSHAuthorizedKey:     &armParameter{vm.SSHPublicKey},
		StorageAccountName:   &armParameter{vm.StorageAccount},
		StorageContainerName: &armParameter{vm.StorageContainer},
		SubnetName:           &armParameter{vm.Subnet},
		VirtualNetworkName:   &armParameter{vm.VirtualNetwork},
		VMSize:               &armParameter{vm.Size},
		VMName:               &armParameter{vm.Name},
		DiskSize:             &armParameter{strconv.Itoa(vm.DiskSize)},
		DiskFile:             &armParameter{vm.DiskFile},
		AdditionalDisk:       &armParameter{"false"},
	}

	if vm.DiskSize > 0 {
		out.AdditionalDisk = &armParameter{"true"}
	}

	return out
}

// validateVM validates the members of given VM object
func validateVM(vm *VM) error {
	// Validate the OAUTH Credentials
	if vm.Creds.ClientID == "" {
		return fmt.Errorf("a client id must be specified")
	}

	if vm.Creds.ClientSecret == "" {
		return fmt.Errorf("a client secret must be specified")
	}

	if vm.Creds.TenantID == "" {
		return fmt.Errorf("a tenant id must be specified")
	}

	if vm.Creds.SubscriptionID == "" {
		return fmt.Errorf("a subscription id must be specified")
	}

	// Validate the image
	if vm.ImagePublisher == "" {
		return fmt.Errorf("an image publisher must be specified")
	}

	if vm.ImageOffer == "" {
		return fmt.Errorf("an image offer must be specified")
	}

	if vm.ImageSku == "" {
		return fmt.Errorf("an image sku must be specified")
	}

	// Validate the deployment
	if vm.ResourceGroup == "" {
		return fmt.Errorf("a resource group must be specified")
	}

	if vm.StorageAccount == "" {
		return fmt.Errorf("a storage account must be specified")
	}

	// Validate the network
	if vm.NetworkSecurityGroup == "" {
		return fmt.Errorf("a network security group must be specified")
	}

	if vm.Subnet == "" {
		return fmt.Errorf("a subnet must be specified")
	}

	if vm.VirtualNetwork == "" {
		return fmt.Errorf("a virtual network must be specified")
	}

	return nil
}

// deploy deploys the given VM based on the default Linux arm template over the
// VM's resource group.
func (vm *VM) deploy() error {
	// Get the auth token.
	tok, err := getServicePrincipalToken(&vm.Creds, azure.PublicCloud.ResourceManagerEndpoint)
	if err != nil {
		return err
	}

	// Pass the parameters to the arm templacte
	vmParams := vm.toARMParameters()
	deployment, err := createDeployment(Linux, *vmParams)
	if err != nil {
		return err
	}

	// Create and send the deployment to the resource group
	deploymentsClient := resources.NewDeploymentsClient(vm.Creds.SubscriptionID)
	deploymentsClient.Authorizer = autorest.NewBearerAuthorizer(tok)

	_, errc := deploymentsClient.CreateOrUpdate(vm.ResourceGroup, vm.DeploymentName, *deployment, nil)
	if err := <-errc; err != nil {
		return err
	}

	// Make sure the deployment is succeeded
	for i := 0; i < actionTimeout; i++ {
		result, err := deploymentsClient.Get(vm.ResourceGroup, vm.DeploymentName)
		if err != nil {
			return err
		}
		if result.Properties != nil && result.Properties.ProvisioningState != nil {
			if *result.Properties.ProvisioningState == succeeded {
				return nil
			}
		}

		time.Sleep(1 * time.Second)
	}

	return ErrActionTimeout
}

// getPublicIP returns the public IP of the given VM, if exists one.
func (vm *VM) getPublicIP(authorizer autorest.Authorizer) (net.IP, error) {
	publicIPAddressesClient := network.NewPublicIPAddressesClient(vm.Creds.SubscriptionID)
	publicIPAddressesClient.Authorizer = authorizer

	resPublicIP, err := publicIPAddressesClient.Get(vm.ResourceGroup, vm.PublicIP, "")
	if err != nil {
		return nil, err
	}

	if resPublicIP.PublicIPAddressPropertiesFormat == nil || resPublicIP.PublicIPAddressPropertiesFormat.IPAddress == nil ||
		*resPublicIP.PublicIPAddressPropertiesFormat.IPAddress == "" {
		return nil, fmt.Errorf("VM has no public IP address")
	}
	return net.ParseIP(*resPublicIP.PublicIPAddressPropertiesFormat.IPAddress), nil
}

// getPrivateIP returns the private IP of the given VM, if exists one.
func (vm *VM) getPrivateIP(authorizer autorest.Authorizer) (net.IP, error) {
	interfaceClient := network.NewInterfacesClient(vm.Creds.SubscriptionID)
	interfaceClient.Authorizer = authorizer

	iface, err := interfaceClient.Get(vm.ResourceGroup, vm.Nic, "")
	if err != nil {
		return nil, err
	}

	if iface.InterfacePropertiesFormat == nil || iface.InterfacePropertiesFormat.IPConfigurations == nil ||
		len(*iface.InterfacePropertiesFormat.IPConfigurations) == 0 {
		return nil, fmt.Errorf("VM has no private IP address")
	}
	ipConfigs := *iface.InterfacePropertiesFormat.IPConfigurations
	if len(ipConfigs) > 1 {
		return nil, fmt.Errorf("VM has multiple private IP addresses")
	}
	return net.ParseIP(*ipConfigs[0].InterfaceIPConfigurationPropertiesFormat.PrivateIPAddress), nil
}

// deleteOSFile deletes the OS file from the VM's storage account, returns an error if the operation
// does not succeed.
func (vm *VM) deleteVMFiles(authorizer autorest.Authorizer) error {
	storageAccountsClient := armStorage.NewAccountsClient(vm.Creds.SubscriptionID)
	storageAccountsClient.Authorizer = authorizer

	result, err := storageAccountsClient.ListKeys(vm.ResourceGroup, vm.StorageAccount)
	if err != nil {
		return err
	}

	accountKeys := result.Keys
	if accountKeys == nil || len(*accountKeys) == 0 {
		return fmt.Errorf("no account keys for storage account %q", vm.StorageAccount)
	}

	accountKey := *(*accountKeys)[0].Value
	storageClient, err := storage.NewBasicClient(vm.StorageAccount, accountKey)
	if err != nil {
		return err
	}

	// Get a reference to the OS file.
	blobStorageClient := storageClient.GetBlobService()
	container := blobStorageClient.GetContainerReference(vm.StorageContainer)
	osFileBlob := container.GetBlobReference(vm.OsFile)

	// Delete the OS file.
	opts := &storage.DeleteBlobOptions{Timeout: 30} // 30s timeout
	err = osFileBlob.Delete(opts)

	if vm.DiskSize <= 0 {
		return err
	}

	// Delete the disk file.
	diskFileBlob := container.GetBlobReference(vm.DiskFile)
	diskFileErr := diskFileBlob.Delete(opts)
	if err != nil {
		return fmt.Errorf("failed to delete OS file and disk file: %v, %v", err, diskFileErr)
	}

	return diskFileErr
}

// deleteNic deletes the network interface for the given VM from the VM's resource group, returns an error
// if the operation does not succeed.
func (vm *VM) deleteNic(authorizer autorest.Authorizer) error {
	interfaceClient := network.NewInterfacesClient(vm.Creds.SubscriptionID)
	interfaceClient.Authorizer = authorizer

	_, errc := interfaceClient.Delete(vm.ResourceGroup, vm.Nic, nil)
	return <-errc
}

// deletePublicIP deletes the reserved Public IP of the given VM from the VM's resource group, returns an error
// if the operation does not succeed.
func (vm *VM) deletePublicIP(authorizer autorest.Authorizer) error {
	// Delete the Public IP of this VM
	publicIPAddressesClient := network.NewPublicIPAddressesClient(vm.Creds.SubscriptionID)
	publicIPAddressesClient.Authorizer = authorizer

	_, errc := publicIPAddressesClient.Delete(vm.ResourceGroup, vm.PublicIP, nil)
	return <-errc
}

// deleteDeployment deletes the deployed azure arm template for this vm.
func (vm *VM) deleteDeployment(authorizer autorest.Authorizer) error {
	// Get the deployments client
	deploymentsClient := resources.NewDeploymentsClient(vm.Creds.SubscriptionID)
	deploymentsClient.Authorizer = authorizer

	// Delete the deployment
	_, errc := deploymentsClient.Delete(vm.ResourceGroup, vm.DeploymentName, nil)
	return <-errc
}

func createDeployment(template string, params armParameters) (*resources.Deployment, error) {
	templateMap, err := unmarshalTemplate(template)
	if err != nil {
		return nil, err
	}

	parametersMap, err := unmarshalParameters(params)
	if err != nil {
		return nil, err
	}

	return &resources.Deployment{
		Properties: &resources.DeploymentProperties{
			Mode:       resources.Incremental,
			Template:   templateMap,
			Parameters: parametersMap,
		},
	}, nil
}

func unmarshalTemplate(template string) (*map[string]interface{}, error) {
	var t map[string]interface{}
	err := json.Unmarshal([]byte(template), &t)

	if err != nil {
		return nil, err
	}

	return &t, nil
}

func unmarshalParameters(params armParameters) (*map[string]interface{}, error) {
	b, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}

	var t map[string]interface{}
	err = json.Unmarshal(b, &t)
	if err != nil {
		return nil, err
	}

	return &t, nil
}

// translateState converts an Azure state to a libretto state.
func translateState(azureState string) string {
	switch azureState {
	case running:
		return lvm.VMRunning
	case stopped:
		return lvm.VMHalted
	default:
		return lvm.VMUnknown
	}
}

func randStringRunes(n int) string {
	var letterRunes = []rune("abcdefghijklmnopqrstuvwxyz")
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	b := make([]rune, n)
	for i := range b {
		b[i] = letterRunes[r.Intn(len(letterRunes))]
	}
	return string(b)
}
