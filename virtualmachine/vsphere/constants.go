package vsphere

const STATICIP_CUSTOM_SPEC_NAME = "static-ip-libretto"
const XML_STATIC_IP_SPEC = `
<ConfigRoot>
  <_type>vim.CustomizationSpecItem</_type>
  <info>
    <_type>vim.CustomizationSpecInfo</_type>
    <changeVersion>1505235815</changeVersion>
    <description/>
    <lastUpdateTime>2017-09-12T17:03:35Z</lastUpdateTime>
    <name>static-ip-libretto</name>
    <type>Linux</type>
  </info>
  <spec>
    <_type>vim.vm.customization.Specification</_type>
    <globalIPSettings>
      <_type>vim.vm.customization.GlobalIPSettings</_type>
      <dnsServerList>
        <_length>1</_length>
        <_type>string[]</_type>
        <e id="0">8.8.8.8</e>
      </dnsServerList>
      <dnsSuffixList>
        <_length>1</_length>
        <_type>string[]</_type>
        <e id="0">gsintlab.com</e>
      </dnsSuffixList>
    </globalIPSettings>
    <identity>
      <_type>vim.vm.customization.LinuxPrep</_type>
      <domain>gsintlab.com</domain>
      <hostName>
        <_type>vim.vm.customization.VirtualMachineNameGenerator</_type>
      </hostName>
    </identity>
    <nicSettingMap>
      <_length>1</_length>
      <_type>vim.vm.customization.AdapterMapping[]</_type>
      <e id="0">
        <_type>vim.vm.customization.AdapterMapping</_type>
        <adapter>
          <_type>vim.vm.customization.IPSettings</_type>
          <gateway>
            <_length>1</_length>
            <_type>string[]</_type>
            <e id="0">10.10.24.1</e>
          </gateway>
          <ip>
            <_type>vim.vm.customization.FixedIp</_type>
            <ipAddress>10.10.24.100</ipAddress>
          </ip>
          <subnetMask>255.255.255.0</subnetMask>
        </adapter>
      </e>
    </nicSettingMap>
    <options>
      <_type>vim.vm.customization.LinuxOptions</_type>
    </options>
  </spec>
</ConfigRoot>
`
