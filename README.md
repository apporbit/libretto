[![License][License-Image]][License-URL] [![ReportCard][ReportCard-Image]][ReportCard-URL] [![Build][Build-Status-Image]][Build-Status-URL] [![GoDoc][GoDoc-Image]][GoDoc-URL] [![Release][Release-Image]][Release-URL]

Libretto
=========

![Libretto](https://raw.githubusercontent.com/apcera/libretto/master/libretto.jpg)

Libretto is a Golang library to create Virtual Machines (VM) on any cloud and
Virtual Machine hosting platforms such as AWS, Azure, OpenStack, vSphere, VMware
Workstation/Fusion, Exoscale or VirtualBox. Different providers have different utilities
and API interfaces to achieve that, but the abstractions of their interfaces are
quite similar.

Supported Providers
====================
* AWS
* Azure
* DigitalOcean
* Exoscale
* Google Cloud Platform
* Openstack (Mirantis)
* Virtualbox >= 4.3.30
* VMware Fusion >= 8.0
* VMware Workstation >= 8.0
* vSphere > 5.0

Getting Started
================

Go version 1.9+ is required.

``` sh
go get -u github.com/apcera/libretto
```

Examples
=========

AWS
----

``` go
rawKey, err := ioutil.ReadFile(*key)
if err != nil {
	return err
}

vm := &aws.VM{
	Name:         "libretto-aws",
	AMI:          "ami-984734",
	InstanceType: "m4.large",
	SSHCreds: ssh.Credentials{
		SSHUser:       "ubuntu",
		SSHPrivateKey: string(rawKey),
	},
	Volumes: []aws.EBSVolume{
		{
			DeviceName: "/dev/sda1",
		},
	},
	Region:        "ap-northeast-1",
	KeyPair:       strings.TrimSuffix(filepath.Base(*key), filepath.Ext(*key)),
	SecurityGroup: "sg-9fdsfds",
}

err = aws.ValidCredentials(vm.Region)
if err != nil {
	return err
}

err = vm.Provision()
if err != nil {
	return err
}
```

Azure
------

``` go
vm := &azure.VM{
	Creds: azure.OAuthCredentials{
		SubscriptionID: os.Getenv("AZURE_SUBSCRIPTION_ID"),
		TenantID:       os.Getenv("AZURE_TENANT_ID"),
		ClientID:       os.Getenv("AZURE_CLIENT_ID"),
		ClientSecret:   os.Getenv("AZURE_CLIENT_SECRET"),
	},
	ImagePublisher:   "Canonical",
	ImageOffer:       "UbuntuServer",
	ImageSku:         "14.04.3-LTS",
	Size:             "Standard_A1",
	DiskSize:         40, // Set to 0 if you don't want to attach any additional disk
	Name:             "libretto",
	ResourceGroup:    "libretto-rg",
	StorageAccount:   "libretto-sa",
	StorageContainer: "libretto-sc",
	SSHCreds: ssh.Credentials{
		SSHUser:     os.Getenv("AZURE_USER"),
		SSHPassword: os.Getenv("AZURE_PASSWORD"),
	},

	NetworkSecurityGroup: "libretto-sg",
	VirtualNetwork:       "libretto-vn",
	Subnet:               "libretto-sn",
}

if err := vm.Provision(); err != nil {
	return err
}
```

Digital Ocean
--------------

``` go
token := os.Getenv("DIGITALOCEAN_API_KEY")
if token == "" {
	return fmt.Errorf("Please export your DigitalOcean API key to 'DIGITALOCEAN_API_KEY' and run again.")
}
config := digitalocean.Config{
	Name:   defaultDropletName,
	Region: defaultDropletRegion,
	Image:  defaultDropletImage,
	Size:   defaultDropletSize,
}

vm := digitalocean.VM{
	APIToken: token,
	Config:   config,
}

if err := vm.Provision(); err != nil {
	return err
}
```

Exoscale
---------

``` go
vm := exoscale.VM{
	Config: exoscale.Config{
		Endpoint:  "https://api.exoscale.ch/compute",
		APIKey:    os.Getenv("EXOSCALE_API_KEY"),
		APISecret: os.Getenv("EXOSCALE_API_SECRET"),
	},
	Template: exoscale.Template{
		Name:      "Linux Ubuntu 16.04 LTS 64-bit",
		StorageGB: 10,
		ZoneName:  "ch-dk-2",
	},
	ServiceOffering: exoscale.ServiceOffering{
		Name: exoscale.Medium,
	},
	SecurityGroups: []exoscale.SecurityGroup{
		{Name: "default"},
		{Name: "my sg"},
	},
	Zone: exoscale.Zone{
		Name: "ch-dk-2",
	},
	KeypairName: "mydev",
	Name:        "libretto-exoscale",
	Userdata: `
# cloud-config
manage_etc_hosts: true
fqdn: new.host
`,
}

if err := vm.Provision(); err != nil {
	fmt.Printf("Error provisioning machine: %s\n", err)
}

// get VM ID polling every 5 seconds, timeout after 30
vm.WaitVMCreation(30, 5)
```

 Google Cloud Platform
----------

``` go
vm := &gcp.VM{
	Name:        "libretto-vm-1",
	Zone:        "us-central1-a",
	MachineType: "n1-standard-1",
	SourceImage: "ubuntu-1404-trusty-v20160516",
	Disks: []gcp.Disk{
		{
			DiskType:   "pd-standard",
			DiskSizeGb: 10,
			AutoDelete: true,
		},
		{
			DiskType:   "pd-standard",
			DiskSizeGb: 500,
			AutoDelete: true,
			Name:       "addtional-disk",
		},
	},
	Preemptible:   false,
	Network:       "default",
	Subnetwork:    "default",
	UseInternalIP: false,
	Project:       os.Getenv("YOUR-GOOGLE-PROJECT"),
	Scopes:        scopes,

	AccountFile: accountFile,
	SSHCreds: ssh.Credentials{
		SSHUser:       "ubuntu",
		SSHPrivateKey: string(sshPrivateKey),
	},
	SSHPublicKey: string(sshPublicKey),
	Tags:         []string{"libretto"},
}

if err := vm.Provision(); err != nil {
	return err
}
```

 Openstack
----------

``` go
metadata := openstack.NewDefaultImageMetadata()
volume := openstack.NewDefaultVolume()

// Optional
cloudInit, err := ioutil.ReadFile("/your/user/data")
if err != nil {
	return err
}

vm := &openstack.VM{
	IdentityEndpoint: os.Getenv("OS_AUTH_URL"),
	Username:         os.Getenv("OS_USERNAME"),
	Password:         os.Getenv("OS_PASSWORD"),
	Region:           os.Getenv("OS_REGION_NAME"),
	TenantName:       os.Getenv("OS_TENANT_NAME"),
	FlavorName:       "m1.medium",
	ImageID:          "",
	ImageMetadata:    metadata,
	ImagePath:        os.Getenv("OS_IMAGE_PATH"),
	Volume:           volume,
	InstanceID:       "",
	Name:             "test",
	Networks:         []string{"eab29109-3363-4b03-8a56-8fe27b71f3a0"},
	FloatingIPPool:   "net04_ext",
	FloatingIP:       nil,
	SecurityGroup:    "test",
	AdminPassword:    os.Getenv("OS_ADMIN_PASSWORD"),
	UserData:         cloudInit,
	Credentials: ssh.Credentials{
		SSHUser:     "ubuntu",
		SSHPassword: "ubuntu",
	},
}

err = vm.Provision()
if err != nil {
	return err
}
```

 Virtualbox
-----------

``` go
var config virtualbox.Config
config.NICs = []virtualbox.NIC{
	virtualbox.NIC{Idx: 1, Backing: virtualbox.Bridged, BackingDevice: "en0: Wi-Fi (AirPort)"},
}
vm := virtualbox.VM{Src: "/Users/Admin/vm-bfb21a62-60c5-11e5-9fc5-a45e60e45ad5.ova",
	Credentials: ssh.Credentials{
		SSHUser:     "ubuntu",
		SSHPassword: "ubuntu",
	},
	Config: config,
}
if err := vm.Provision(); err != nil {
	return err
}
```

vSphere
--------

``` go
vm := &vsphere.VM{
	Host:       "10.2.1.11",
	Username:   "username",
	Password:   "password",
	Datacenter: "test-dc",
	Datastores: []string{"datastore1", "datastore2"},
	Networks:   map[string]string{"nat": "network1"},
	Credentials: ssh.Credentials{
		SSHUser:     "ubuntu",
		SSHPassword: "ubuntu",
	},
	SkipExisting: true,
	Insecure:     true,
	Destination: vsphere.Destination{
		DestinationName: "Host1",
		DestinationType: "host",
		HostSystem:      "",
	},
	Name:     "test-vm",
	Template: "test-template",
	OvfPath:  "/Users/Test/Downloads/file.ovf",
}
if err := vm.Provision(); err != nil {
	return err
}
```


VMware Fusion/Workstation (vmrun)
----------------------------------

``` go
var config vmrun.Config
config.NICs = []vmrun.NIC{
	vmrun.NIC{Idx: 0, Backing: vmrun.Nat, BackingDevice: "en0"},
}
vm := vmrun.VM{Src: "/Users/Admin/vmware_desktop/trusty-orchestrator-dev.vmx",
	Dst: "/Users/Admin/Documents/VMs",
	Credentials: ssh.Credentials{
		SSHUser:     "ubuntu",
		SSHPassword: "ubuntu",
	},
	Config: config,
}
if err := vm.Provision(); err != nil {
	return err
}
```


FAQ
====

* Why write Libretto?

We couldn't find a suitable Golang binding for this functionality, so we created
this library. There are a couple of similar libraries but not in golang
(fog.io in ruby, jcloud in java, libcloud in python).

Docker Machine is an effort toward that direction, but it is very Docker
specific, and its providers dictate the VM images in many cases to reduce the
number of parameters, but reduces the flexibility of the tool.

* What is the scope for Libretto?

Virtual machine creation and life cycle management as well as common
configuration steps during a deploy such as configuring SSH keys.

* Why use this library over other tools?

Can be called natively from Go in a Go application instead of shelling out to
other tools.

Known Issues
=============

*  Virtualbox networking is limited to using Bridged mode.

  Host to guest OS connectivity is not possible when using NAT networking in
  Virtualbox. As a result, presently networking configuration for VMs
  provisioned using the Virtualbox provider is limited to using Bridged
  networking. There should be a DHCP server running on the network that the VMs
  are bridged to.

Supported Platforms
====================
* Linux x64
* Windows 7 >= x64
* OS X >= 10.11

Other Operating Systems might work but have not been tested.

Adding Provisioners
====================

Create a new package inside the `virtualmachine` folder and implement the
Libretto `VirtualMachine` interface. The provider should work at the minimum on
the Linux, Windows and OS X platforms unless it is a platform specific provider
in which case it should at least compile and return a descriptive error.

Dependencies should be vendored using [`dep ensure`](https://github.com/golang/dep),
followed by [`dep prune`](https://github.com/golang/dep).

Errors should be lower case so that they can be wrapped by the calling code. If
possible, types defined in the top level `virtualmachine` package should be
reused.

Contributors
=============

https://github.com/apcera/libretto/graphs/contributors with special thanks to Renee French for original creation of the Go Gopher and Rachel Thieman for the outfit.

[License-URL]: https://opensource.org/licenses/Apache-2.0
[License-Image]: https://img.shields.io/:license-apache-blue.svg
[ReportCard-URL]: https://goreportcard.com/report/github.com/apcera/libretto
[ReportCard-Image]: https://goreportcard.com/badge/github.com/apcera/libretto
[Build-Status-URL]: http://travis-ci.org/apcera/libretto
[Build-Status-Image]: https://travis-ci.org/apcera/libretto.svg?branch=master
[GoDoc-URL]: https://godoc.org/github.com/apcera/libretto
[GoDoc-Image]: https://godoc.org/github.com/apcera/libretto?status.svg
[Release-URL]: https://github.com/apcera/libretto/releases/tag/v0.9.0
[Release-Image]: https://img.shields.io/badge/release-v0.9.0-1eb0fc.svg
