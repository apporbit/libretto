package arm

// Linux is the d efault arm template to provision a libretto (Linux) vm on Azure
const Linux = `{
  "$schema": "https://schema.management.azure.com/schemas/2015-01-01/deploymentTemplate.json#",
  "contentVersion": "1.0.0.0",
  "parameters": {
    "username": {
      "type": "string"
    },
    "password": {
      "type": "string"
    },
    "image_publisher": {
      "type": "string"
    },
    "image_offer": {
      "type": "string"
    },
    "image_sku": {
      "type": "string"
    },
    "network_security_group": {
      "type": "string"
    },
    "nic": {
      "type": "string"
    },
    "os_file": {
      "type": "string"
    },
    "disk_file": {
      "type": "string"
    },
    "public_ip": {
      "type": "string"
    },
    "ssh_authorized_key": {
      "type": "string"
    },
    "storage_account": {
      "type": "string"
    },
    "storage_container": {
      "type": "string"
    },
    "subnet": {
      "type": "string"
    },
    "virtual_network": {
      "type": "string"
    },
    "vm_size": {
      "type": "string"
    },
    "vm_name": {
      "type": "string"
    },
    "disk_size": {
      "type": "string"
    },
    "additional_disk": {
      "type": "string"
    }
  },
  "variables": {
    "diskAttachment": {
      "true": {
        "disks": [{
          "name": "datadisk1",
          "diskSizeGB": "[parameters('disk_size')]",
          "lun": 0,
          "vhd": {
            "uri": "[concat('http://',parameters('storage_account'),'.blob.core.windows.net/',parameters('storage_container'),'/', parameters('disk_file'))]"
          },
          "createOption": "Empty"
        }]
      },
      "false": {
        "disks": []
      }
    },
    "disksSettings": "[variables('diskAttachment')[parameters('additional_disk')]]",
    "disksArray": "[variables('disksSettings').disks]",
    "api_version": "2015-06-15",
    "location": "[resourceGroup().location]",
    "subnet_ref": "[concat(variables('vnet_id'),'/subnets/',parameters('subnet'))]",
    "vnet_id": "[resourceId('Microsoft.Network/virtualNetworks', parameters('virtual_network'))]"
  },
  "resources": [
    {
      "apiVersion": "[variables('api_version')]",
      "type": "Microsoft.Network/publicIPAddresses",
      "name": "[parameters('public_ip')]",
      "location": "[variables('location')]",
      "properties": {
        "publicIPAllocationMethod": "Dynamic",
        "dnsSettings": {
          "domainNameLabel": "[parameters('public_ip')]"
        }
      }
    },
    {
      "apiVersion": "[variables('api_version')]",
      "type": "Microsoft.Network/networkInterfaces",
      "name": "[parameters('nic')]",
      "location": "[variables('location')]",
      "dependsOn": [
        "[concat('Microsoft.Network/publicIPAddresses/', parameters('public_ip'))]"
      ],
      "properties": {
        "ipConfigurations": [
          {
            "name": "ipconfig",
            "properties": {
              "privateIPAllocationMethod": "Dynamic",
              "publicIPAddress": {
                "id": "[resourceId('Microsoft.Network/publicIPAddresses', parameters('public_ip'))]"
              },
              "subnet": {
                "id": "[variables('subnet_ref')]"
              }
            }
          }
        ],
        "networkSecurityGroup": {
          "id": "[resourceId('Microsoft.Network/networkSecurityGroups', parameters('network_security_group'))]"
        }
      }
    },
    {
      "apiVersion": "[variables('api_version')]",
      "type": "Microsoft.Compute/virtualMachines",
      "name": "[parameters('vm_name')]",
      "location": "[variables('location')]",
      "dependsOn": [
        "[concat('Microsoft.Network/networkInterfaces/', parameters('nic'))]"
      ],
      "properties": {
        "hardwareProfile": {
          "vmSize": "[parameters('vm_size')]"
        },
        "osProfile": {
          "computerName": "[parameters('vm_name')]",
          "adminUsername": "[parameters('username')]",
          "linuxConfiguration": {
            "disablePasswordAuthentication": true,
            "ssh": {
              "publicKeys": [
                {
                  "path": "[concat('/home/', parameters('username'), '/.ssh/authorized_keys')]",
                  "keyData": "[parameters('ssh_authorized_key')]"
                }
              ]
            }
          }
        },
        "storageProfile": {
          "imageReference": {
            "publisher": "[parameters('image_publisher')]",
            "offer": "[parameters('image_offer')]",
            "sku": "[parameters('image_sku')]",
            "version": "latest"
          },
          "dataDisks": "[variables('disksArray')]",
          "osDisk": {
            "name": "osdisk",
            "vhd": {
              "uri": "[concat('http://',parameters('storage_account'),'.blob.core.windows.net/',parameters('storage_container'),'/', parameters('os_file'))]"
            },
            "caching": "ReadWrite",
            "createOption": "FromImage"
          }
        },
        "networkProfile": {
          "networkInterfaces": [
            {
              "id": "[resourceId('Microsoft.Network/networkInterfaces', parameters('nic'))]"
            }
          ]
        },
        "diagnosticsProfile": {
          "bootDiagnostics": {
             "enabled": false
          }
        }
      }
    }
  ]
}`
