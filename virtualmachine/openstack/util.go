// Copyright 2015 Apcera Inc. All rights reserved.

package openstack

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/gophercloud/openstack/blockstorage/v1/volumes"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/volumeattach"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/images"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"

	"github.com/apcera/libretto/ssh"
	lvm "github.com/apcera/libretto/virtualmachine"
)

func getProviderClient(vm *VM) (*gophercloud.ProviderClient, error) {
	// Set the opts to autheticate clients. For now, we only support basic auth (host, username, password)
	// Or user can download its Openstack RC File and source it to its console, then opts will be read via ENV_VARS
	var opts gophercloud.AuthOptions
	var err error
	if vm.Username == "" || vm.Password == "" {
		opts, err = openstack.AuthOptionsFromEnv()

		if err != nil {
			return nil, ErrAuthOptions
		}
	} else {
		opts = gophercloud.AuthOptions{
			IdentityEndpoint: vm.IdentityEndpoint,
			Username:         vm.Username,
			Password:         vm.Password,
			TenantName:       vm.TenantName,
		}
	}

	providerClient, err := openstack.AuthenticatedClient(opts)
	if providerClient == nil || err != nil {
		return nil, fmt.Errorf("failed to authenticate the client")
	}

	return providerClient, nil
}

func getComputeClient(vm *VM) (*gophercloud.ServiceClient, error) {
	if vm.computeClient != nil {
		return vm.computeClient, nil
	}

	provider, err := getProviderClient(vm)
	if err != nil {
		return nil, ErrAuthenticatingClient
	}

	endpointOpts := gophercloud.EndpointOpts{
		Region: vm.Region,
	}

	client, err := openstack.NewComputeV2(provider, endpointOpts)
	if err != nil {
		return nil, ErrInvalidRegion
	}

	vm.computeClient = client
	return client, nil
}

func getNetworkClient(vm *VM) (*gophercloud.ServiceClient, error) {
	provider, err := getProviderClient(vm)
	if err != nil {
		return nil, ErrAuthenticatingClient
	}

	endpointOpts := gophercloud.EndpointOpts{
		Region: vm.Region,
	}

	client, err := openstack.NewNetworkV2(provider, endpointOpts)
	if err != nil {
		return nil, ErrInvalidRegion
	}
	return client, nil
}

func getBlockStorageClient(vm *VM) (*gophercloud.ServiceClient, error) {
	provider, err := getProviderClient(vm)
	if err != nil {
		return nil, ErrAuthenticatingClient
	}

	endpointOpts := gophercloud.EndpointOpts{
		Region: vm.Region,
	}

	client, err := openstack.NewBlockStorageV1(provider, endpointOpts)
	if err != nil {
		return nil, ErrInvalidRegion
	}
	return client, nil
}

// findImageAPIVersion finds the Image API version number. It first checks whether the given
// imageEndpoint has version info. If it is not, then a Get request is sent to imageEndpoint to
// fetch supported APIs. If any V2 api is supported then it returns 2, else If any V1 api is
// supported then it returns 1. Otherwise, it returns an error.
func findImageAPIVersion(tokenID string, imageEndpoint string) (int, error) {
	// Try to fetch image API version from the imageEndpoint
	if strings.HasSuffix(imageEndpoint, "/v1/") {
		return 1, nil
	}
	if strings.HasSuffix(imageEndpoint, "/v2/") {
		return 2, nil
	}

	// Try to fetch version number using the endpoint
	versionReq, err := http.NewRequest("GET", imageEndpoint, nil)
	if err != nil {
		return 0, fmt.Errorf("unable to get image API version")
	}

	versionReq.Header.Add("X-Auth-Token", tokenID)
	versionClient := &http.Client{}

	// Send the request to upload the image
	resp, err := versionClient.Do(versionReq)
	if err != nil {
		return 0, fmt.Errorf("failed to send a image API version request")
	}
	defer resp.Body.Close()

	body, _ := ioutil.ReadAll(resp.Body)
	bodyStr := string(body)
	if resp.StatusCode != http.StatusMultipleChoices {
		return 0, fmt.Errorf("image API version request returned bad response, %s", bodyStr)
	}

	// Prefer V2 over V1
	if match, _ := regexp.MatchString(".*\"id\": \"v2\\.[0-2]+.*\"", bodyStr); match {
		return 2, nil
	}

	if match, _ := regexp.MatchString(".*\"id\": \"v1\\.[0-1]+.*\"", bodyStr); match {
		return 1, nil
	}
	return 0, fmt.Errorf("image API version is not supported")
}

func imageVersionEncoded(imageEndpoint string) bool {
	if strings.HasSuffix(imageEndpoint, "/v1/") || strings.HasSuffix(imageEndpoint, "/v2/") {
		return true
	}
	return false
}

// Reserves an Image ID at the specified image endpoint using the information in given imageMetadata
// Returns the reserved Image ID if reservation is successful, otherwise returns an error.
// Requires client's token to reserve the image.
func reserveImage(tokenID string, imageEndpoint string, imageMetadata ImageMetadata, imageApiVersion int) (string, error) {
	// Form the URI to create the image
	imagesURI := ""
	if imageVersionEncoded(imageEndpoint) {
		imagesURI = fmt.Sprintf("%simages", imageEndpoint)
	} else {
		imagesURI = fmt.Sprintf("%sv%d/images", imageEndpoint, imageApiVersion)
	}

	// Prepare the request to create the image
	var createReq *http.Request
	var err error
	if imageApiVersion == 1 {
		createReq, err = http.NewRequest("POST", imagesURI, nil)
	} else {
		imageStr, imgErr := json.Marshal(imageMetadata)
		if imgErr != nil {
			return "", imgErr
		}

		createReq, err = http.NewRequest("POST", imagesURI, bytes.NewBuffer(imageStr))
	}
	if err != nil {
		return "", err
	}

	createReq.Header.Add("X-Auth-Token", tokenID)
	if imageApiVersion == 1 {
		createReq.Header.Add("Content-Type", "application/octet-stream")
		createReq.Header.Add("X-Image-Meta-Name", imageMetadata.Name)
		createReq.Header.Add("X-Image-Meta-container_format", imageMetadata.ContainerFormat)
		createReq.Header.Add("X-Image-Meta-disk_format", imageMetadata.DiskFormat)
		createReq.Header.Add("X-Image-Meta-min_disk", strconv.Itoa(imageMetadata.MinDisk))
		createReq.Header.Add("X-Image-Meta-min_ram", strconv.Itoa(imageMetadata.MinRAM))
	} else {
		createReq.Header.Add("Content-Type", "application/json")
	}

	// Send the request to create the image
	httpClient := &http.Client{}
	resp, err := httpClient.Do(createReq)
	if err != nil {
		return "", fmt.Errorf("failed to send a image reserve request")
	}
	defer resp.Body.Close()
	body, _ := ioutil.ReadAll(resp.Body)
	if resp.StatusCode != 201 {
		return "", fmt.Errorf("reserve image request returned bad response, %s", string(body))
	}

	// Parse the result to see if image is created
	var dat map[string]interface{}
	if err := json.Unmarshal(body, &dat); err != nil {
		return "", err
	}

	if imageApiVersion == 1 {
		dat = dat["image"].(map[string]interface{})
	}

	if dat["status"] != imageQueued {
		return "", fmt.Errorf("image has never been created")
	}

	// Retrieve the image ID from http response block
	idFromResponse := dat["id"]
	switch idFromResponse.(type) {
	case string:
		return idFromResponse.(string), nil
	default:
		return "", fmt.Errorf("unable to parse the upload image response")
	}
}

// Uploads the image to an reserved image location at the imageEndpoint using the reserved image ID and imageMetadata.
// Returns nil error if the upload is successful, otherwise returns an error.
// Requires client's token to upload the image.
func uploadImage(tokenID string, imageEndpoint string, imageID string, imagePath string, imageApiVersion int) error {
	// Read the image file
	file, err := os.Open(imagePath)
	if err != nil {
		return fmt.Errorf("unable to open image file")
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return fmt.Errorf("unable to get the stats of the image file: %s", err)
	}
	imageFileSize := stat.Size()

	// Prepare the request to upload the image file
	imageLocation := ""
	if imageVersionEncoded(imageEndpoint) {
		imageLocation = fmt.Sprintf("%simages/%s", imageEndpoint, imageID)
	} else {
		imageLocation = fmt.Sprintf("%sv%d/images/%s", imageEndpoint, imageApiVersion, imageID)
	}
	if imageApiVersion == 2 {
		imageLocation += "/file"
	}

	uploadReq, err := http.NewRequest("PUT", imageLocation, file)
	if err != nil {
		return fmt.Errorf("unable to upload image to the openstack")
	}

	uploadReq.Header.Add("Content-Type", "application/octet-stream")
	uploadReq.Header.Add("X-Auth-Token", tokenID)
	uploadReq.Header.Add("Content-Length", fmt.Sprintf("%d", imageFileSize))

	uploadClient := &http.Client{}

	// Send the request to upload the image
	resp, err := uploadClient.Do(uploadReq)
	if err != nil {
		return fmt.Errorf("failed to send a upload image request")
	}
	defer resp.Body.Close()

	body, _ := ioutil.ReadAll(resp.Body)
	if (imageApiVersion == 1 && resp.StatusCode != http.StatusOK) ||
		(imageApiVersion == 2 && resp.StatusCode != http.StatusNoContent) {
		return fmt.Errorf("upload image request returned bad response, %s", string(body))
	}

	return nil
}

// Creates an Image based on the given FilePath and returns the UUID of the image
func createImage(vm *VM) (string, error) {
	// Get the openstack provider
	provider, err := getProviderClient(vm)
	if err != nil {
		return "", ErrAuthenticatingClient
	}

	endpointOpts := gophercloud.EndpointOpts{
		Region: vm.Region,
	}
	// Find the Image Endpoint to upload the image
	imageEndpoint, err := findImageEndpoint(provider, endpointOpts)
	if err != nil {
		return "", err
	}

	// Find the Image API version number
	version, err := findImageAPIVersion(provider.TokenID, imageEndpoint)
	if err != nil {
		return "", err
	}

	// Reserve an ImageID at imageEndpoint using the given image metadata
	imageID, err := reserveImage(provider.TokenID, imageEndpoint, vm.ImageMetadata, version)
	if err != nil {
		return "", err
	}

	// Upload the image to the imageEndpoint with reserved ImageID using the given image path
	err = uploadImage(provider.TokenID, imageEndpoint, imageID, vm.ImagePath, version)
	if err != nil {
		return "", err
	}

	return imageID, nil
}

// getServer returns the Openstack server object for the VM. An error is returned
// if the instance ID is missing, if there was a problem querying Openstack, or if
// there is no instances with the given VM ID.
func getServer(vm *VM) (*servers.Server, error) {
	if vm.InstanceID == "" {
		// Probably need to call Provision first.
		return nil, ErrNoInstanceID
	}

	client, err := getComputeClient(vm)
	if err != nil {
		return nil, err
	}

	status, err := servers.Get(client, vm.InstanceID).Extract()
	if status != nil && err != nil {
		return nil, fmt.Errorf("failed to retrieve the server for VM")
	}

	return status, nil
}

// Finds the image endpoint in the given openstack Region. Region is passed within gophercloud.EndpointOpts
func findImageEndpoint(client *gophercloud.ProviderClient, eo gophercloud.EndpointOpts) (string, error) {
	eo.ApplyDefaults("image")
	url, err := client.EndpointLocator(eo)
	if err != nil {
		return "", fmt.Errorf("error on locating image endpoint")
	}
	return url, nil
}

// Waits until the given VM becomes in requested state in given ActionTimeout seconds
func waitUntil(vm *VM, state string) error {
	var curState string
	var err error
	for i := 0; i < ActionTimeout; i++ {
		curState, err = vm.GetState()
		if err != nil {
			return err
		}

		if curState == state {
			break
		}

		if curState == lvm.VMError {
			return fmt.Errorf("failed to bring the VM to state: %s", state)
		}

		time.Sleep(1 * time.Second)
	}
	if curState != state {
		return ErrActionTimeout
	}
	return nil
}

// Waits until the given VM becomes ready. Basically, waits until vm can be sshed.
func waitUntilSSHReady(vm *VM) error {
	client, err := vm.GetSSH(ssh.Options{})
	if err != nil {
		return err
	}
	return client.WaitForSSH(SSHTimeout)
}

// createAndAttachVolume creates a new volume with the given volume specs and then attaches this volume to the given VM.
func createAndAttachVolume(vm *VM) error {
	if vm.InstanceID == "" {
		// Probably need to call Provision first.
		return ErrNoInstanceID
	}

	cClient, err := getComputeClient(vm)
	if err != nil {
		return fmt.Errorf("compute client is not set for the VM, %s", err)
	}

	bsClient, err := getBlockStorageClient(vm)
	if err != nil {
		return err
	}

	// Creates a new Volume for this VM
	volume := vm.Volume
	vOpts := volumes.CreateOpts{Size: volume.Size, Name: volume.Name, VolumeType: volume.Type}
	vol, err := volumes.Create(bsClient, vOpts).Extract()
	if err != nil {
		return fmt.Errorf("failed to create a new volume for the VM: %s", err)
	}

	// Cleanup the volume if something goes wrong
	var cleanup = func(err error) error {
		if errDeleteVolume := volumes.Delete(bsClient, vol.ID).ExtractErr(); errDeleteVolume != nil {
			return fmt.Errorf("%s %s", err, errDeleteVolume)
		}

		return err
	}

	// Wait until Volume becomes available
	err = waitUntilVolume(bsClient, vol.ID, volumeStateAvailable)
	if err != nil {
		return cleanup(fmt.Errorf("failed to create a new volume for the VM: %s", err))
	}

	// Attach the new volume to this VM
	vaOpts := volumeattach.CreateOpts{Device: volume.Device, VolumeID: vol.ID}
	va, err := volumeattach.Create(cClient, vm.InstanceID, vaOpts).Extract()
	if err != nil {
		return cleanup(fmt.Errorf("failed to attach the volume to the VM: %s", err))
	}

	// Wait until Volume is attached to the VM
	err = waitUntilVolume(bsClient, vol.ID, volumeStateInUse)
	if err != nil {
		errVaDelete := volumeattach.Delete(cClient, vm.InstanceID, vol.ID).ExtractErr()
		err = fmt.Errorf("%s %s", err, errVaDelete)
		return cleanup(fmt.Errorf("failed to attach the volume to the VM: %s", err))
	}

	vm.Volume.ID = vol.ID
	vm.Volume.Device = va.Device

	return nil
}

// deattachAndDeleteVolume deattaches the volume from the given VM and then completely deletes the volume.
func deattachAndDeleteVolume(vm *VM) error {
	if vm.InstanceID == "" {
		// Probably need to call Provision first.
		return ErrNoInstanceID
	}

	cClient, err := getComputeClient(vm)
	if err != nil {
		return fmt.Errorf("compute client is not set for the VM, %s", err)
	}

	bsClient, err := getBlockStorageClient(vm)
	if err != nil {
		return err
	}

	// Deattach the volume from the VM
	err = volumeattach.Delete(cClient, vm.InstanceID, vm.Volume.ID).ExtractErr()
	if err != nil {
		return fmt.Errorf("failed to deattach volume from the VM: %s", err)
	}

	// Wait until Volume is de-attached from the VM
	err = waitUntilVolume(bsClient, vm.Volume.ID, volumeStateAvailable)
	if err != nil {
		return fmt.Errorf("failed to deattach volume from the VM: %s", err)
	}

	// Delete the volume
	err = volumes.Delete(bsClient, vm.Volume.ID).ExtractErr()
	if err != nil {
		return fmt.Errorf("failed to delete volume: %s", err)
	}

	// Wait until Volume is deleted
	err = waitUntilVolume(bsClient, vm.Volume.ID, volumeStateDeleted)
	if err != nil {
		return fmt.Errorf("failed to delete volume: %s", err)
	}

	return nil
}

// deleteVM deletes the instance.
func deleteVM(client *gophercloud.ServiceClient, vmID string) error {
	err := servers.Delete(client, vmID).ExtractErr()
	if err != nil {
		return fmt.Errorf("failed to destroy vm %s: %s", vmID, err)
	}
	return nil
}

// findImageIDByName finds the ImageID for the given imageName, returns an error if there is
// no image or more than one image with the given Image Name.
func findImageIDByName(client *gophercloud.ServiceClient, imageName string) (string, error) {
	if imageName == "" {
		return "", fmt.Errorf("empty image name")
	}

	// We have the option of filtering the image list. If we want the full
	// collection, leave it as an empty struct
	opts := images.ListOpts{Name: imageName}

	// Retrieve image list
	page, err := images.ListDetail(client, opts).AllPages()
	if err != nil {
		return "", fmt.Errorf("error on retrieving image pages: %s", err)
	}

	imageList, err := images.ExtractImages(page)
	if err != nil {
		return "", fmt.Errorf("error on extracting image list: %s", err)
	}

	if len(imageList) == 0 {
		return "", nil
	}

	if len(imageList) > 1 {
		return "", fmt.Errorf("there exists more than one image with the same name")
	}

	return imageList[0].ID, err
}

// waitUntilVolume waits until the given volume turns into given state under given VolumeActionTimeout seconds
func waitUntilVolume(blockStorateClient *gophercloud.ServiceClient, volumeID string, state string) error {
	for i := 0; i < VolumeActionTimeout; i++ {
		vol, err := volumes.Get(blockStorateClient, volumeID).Extract()
		switch {
		case vol == nil && state == "nil":
			return nil
		case vol == nil || err != nil:
			return fmt.Errorf("failed on getting volume Status: %s", err)
		case vol.Status == state:
			return nil
		case vol.Status == lvm.VMError || vol.Status == volumeStateErrorDeleting:
			return fmt.Errorf("failed to bring the volume to state %s, ended up at state %s", state, vol.Status)
		}
		time.Sleep(1 * time.Second)
	}
	return ErrActionTimeout
}

// NewDefaultImageMetadata creates a ImageMetadata with default values
func NewDefaultImageMetadata() ImageMetadata {
	return ImageMetadata{
		ContainerFormat: "bare",
		DiskFormat:      "qcow2",
		MinDisk:         10,
		MinRAM:          1024,
		Name:            "new-image",
	}
}

// NewDefaultVolume creates a Volume with default values
func NewDefaultVolume() Volume {
	return Volume{
		Name:   "test",
		Size:   10,
		Device: "/dev/vdb",
	}
}
