package vlan

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"

	"git.code.oa.com/tkestack/galaxy/pkg/network"
	"git.code.oa.com/tkestack/galaxy/pkg/utils"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
)

const (
	VlanPrefix    = "vlan"
	BridgePrefix  = "docker"
	DefaultBridge = "docker"
)

type VlanDriver struct {
	//FIXME add a file lock cause we are running multiple processes?
	*NetConf
	// The device id of physical device which is to be the parent of all vlan devices, eg.eth1
	vlanParentIndex int
	// The device id of NetConf.Device or created vlan device
	DeviceIndex int
	sync.Mutex
}

type NetConf struct {
	types.NetConf
	// The device which has IDC ip address, eg. eth1 or eth1.12 (A vlan device)
	Device string `json:"device"`
	// Supports macvlan, bridge or pure(which avoid create unnecessary bridge), default bridge
	Switch string `json:"switch"`

	// Disable creating default bridge
	DisableDefaultBridge *bool `json:"disable_default_bridge"`

	DefaultBridgeName string `json:"default_bridge_name"`

	BridgeNamePrefix string `json:"bridge_name_prefix"`

	VlanNamePrefix string `json:"vlan_name_prefix"`
}

func (d *VlanDriver) LoadConf(bytes []byte) (*NetConf, error) {
	conf := &NetConf{}
	if err := json.Unmarshal(bytes, conf); err != nil {
		return nil, fmt.Errorf("failed to load netconf: %v", err)
	}
	if conf.DefaultBridgeName == "" {
		conf.DefaultBridgeName = DefaultBridge
	}
	if conf.BridgeNamePrefix == "" {
		conf.BridgeNamePrefix = BridgePrefix
	}
	if conf.VlanNamePrefix == "" {
		conf.VlanNamePrefix = VlanPrefix
	}
	d.NetConf = conf
	return conf, nil
}

func (d *VlanDriver) Init() error {
	device, err := netlink.LinkByName(d.Device)
	if err != nil {
		return fmt.Errorf("Error getting device %s: %v", d.Device, err)
	}
	d.DeviceIndex = device.Attrs().Index
	d.vlanParentIndex = device.Attrs().Index
	//defer glog.Infof("root device %q, vlan parent index %d", d.Device, d.vlanParentIndex)
	if device.Type() == "vlan" {
		//A vlan device
		d.vlanParentIndex = device.Attrs().ParentIndex
		//glog.Infof("root device %s is a vlan device, parent index %d", d.Device, d.vlanParentIndex)
	}
	if d.MacVlanMode() || d.IPVlanMode() {
		return nil
	}
	if d.PureMode() {
		if err := utils.UnSetArpIgnore("all"); err != nil {
			return err
		}
		if err := utils.UnSetArpIgnore(d.Device); err != nil {
			return err
		}
		if err := utils.SetProxyArp(d.Device); err != nil {
			return err
		}
		return utils.EnableNonlocalBind()
	}
	if d.DisableDefaultBridge != nil && *d.DisableDefaultBridge {
		return nil
	}
	v4Addr, err := netlink.AddrList(device, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("Errror getting ipv4 address %v", err)
	}
	filteredAddr := network.FilterLoopbackAddr(v4Addr)
	if len(filteredAddr) == 0 {
		bri, err := netlink.LinkByName(d.DefaultBridgeName)
		if err != nil {
			return fmt.Errorf("Error getting bri device %s: %v", d.DefaultBridgeName, err)
		}
		if bri.Attrs().Index != device.Attrs().MasterIndex {
			return fmt.Errorf("No available address found on device %s", d.Device)
		}
	} else {
		bri, err := getOrCreateBridge(d.DefaultBridgeName, device.Attrs().HardwareAddr)
		if err != nil {
			return err
		}
		if err := netlink.LinkSetUp(bri); err != nil {
			return fmt.Errorf("Failed to set up bridge device %s: %v", d.DefaultBridgeName, err)
		}
		rs, err := netlink.RouteList(device, nl.FAMILY_V4)
		if err != nil {
			return fmt.Errorf("Failed to list route of device %s", device.Attrs().Name)
		}
		defer func() {
			if err != nil {
				for i := range rs {
					_ = netlink.RouteAdd(&rs[i])
				}
			}
		}()
		for i := range filteredAddr {
			if err = netlink.AddrDel(device, &filteredAddr[i]); err != nil {
				return fmt.Errorf("Failed to remove v4address from device %s: %v", d.Device, err)
			}
			// nolint: errcheck
			defer func() {
				if err != nil {
					netlink.AddrAdd(device, &filteredAddr[i])
				}
			}()
			filteredAddr[i].Label = ""
			if err = netlink.AddrAdd(bri, &filteredAddr[i]); err != nil {
				if !strings.Contains(err.Error(), "file exists") {
					return fmt.Errorf("Failed to add v4address to bridge device %s: %v, address %v", d.DefaultBridgeName, err, filteredAddr[i])
				} else {
					err = nil
				}
			}
		}
		if err = netlink.LinkSetMaster(device, &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: d.DefaultBridgeName}}); err != nil {
			return fmt.Errorf("Failed to add device %s to bridge device %s: %v", d.Device, d.DefaultBridgeName, err)
		}
		for i := range rs {
			newRoute := netlink.Route{Gw: rs[i].Gw, LinkIndex: bri.Attrs().Index, Dst: rs[i].Dst, Src: rs[i].Src, Scope: rs[i].Scope}
			if err = netlink.RouteAdd(&newRoute); err != nil {
				if !strings.Contains(err.Error(), "file exists") {
					return fmt.Errorf("failed to add route %s", newRoute.String())
				} else {
					err = nil
				}
			}
		}
	}
	return nil
}

func getOrCreateBridge(bridgeName string, mac net.HardwareAddr) (netlink.Link, error) {
	return getOrCreateDevice(bridgeName, func(name string) error {
		if err := utils.CreateBridgeDevice(bridgeName, mac); err != nil {
			return fmt.Errorf("Failed to add bridge device %s: %v", bridgeName, err)
		}
		return nil
	})
}

func getOrCreateDevice(name string, createDevice func(name string) error) (netlink.Link, error) {
	device, err := netlink.LinkByName(name)
	if err != nil {
		if err := createDevice(name); err != nil {
			return nil, fmt.Errorf("Failed to add %s: %v", name, err)
		}
		if device, err = netlink.LinkByName(name); err != nil {
			return nil, fmt.Errorf("Failed to get %s: %v", name, err)
		}
	}
	return device, nil
}

func (d *VlanDriver) CreateBridgeAndVlanDevice(vlanId uint16) (string, error) {
	if vlanId == 0 {
		return d.BridgeNameForVlan(vlanId), nil
	}
	d.Lock()
	defer d.Unlock()
	vlan, err := d.getOrCreateVlanDevice(vlanId)
	if err != nil {
		return "", err
	}
	master, err := getVlanMaster(vlan)
	if err != nil {
		return "", err
	}
	if master != nil {
		return master.Attrs().Name, nil
	}
	bridgeIfName := fmt.Sprintf("%s%d", d.BridgeNamePrefix, vlanId)
	bridge, err := getOrCreateBridge(bridgeIfName, nil)
	if err != nil {
		return "", err
	}
	if vlan.Attrs().MasterIndex != bridge.Attrs().Index {
		if err := netlink.LinkSetMaster(vlan, &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: bridgeIfName}}); err != nil {
			return "", fmt.Errorf("Failed to add vlan device %s to bridge device %s: %v", vlan.Attrs().Name, bridgeIfName, err)
		}
	}
	if err := netlink.LinkSetUp(bridge); err != nil {
		return "", fmt.Errorf("Failed to set up bridge device %s: %v", bridgeIfName, err)
	}
	if d.PureMode() {
		if err := utils.SetProxyArp(bridgeIfName); err != nil {
			return "", err
		}
	}
	return bridgeIfName, nil
}

func (d *VlanDriver) BridgeNameForVlan(vlanId uint16) string {
	if vlanId == 0 && d.PureMode() {
		return ""
	}
	bridgeName := d.DefaultBridgeName
	if vlanId != 0 {
		bridgeName = fmt.Sprintf("%s%d", d.BridgeNamePrefix, vlanId)
	}
	return bridgeName
}

func (d *VlanDriver) MaybeCreateVlanDevice(vlanId uint16) error {
	if vlanId == 0 {
		return nil
	}
	d.Lock()
	defer d.Unlock()
	_, err := d.getOrCreateVlanDevice(vlanId)
	return err
}

func (d *VlanDriver) getOrCreateVlanDevice(vlanId uint16) (netlink.Link, error) {
	// check if vlan created by user exist
	link, err := d.getVlanIfExist(vlanId)
	if err != nil || link != nil {
		if link != nil {
			d.DeviceIndex = link.Attrs().Index
		}
		return link, err
	}
	vlanIfName := fmt.Sprintf("%s%d", d.VlanNamePrefix, vlanId)
	// Get vlan device
	vlan, err := getOrCreateDevice(vlanIfName, func(name string) error {
		vlanIf := &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: vlanIfName, ParentIndex: d.vlanParentIndex}, VlanId: (int)(vlanId)}
		if err := netlink.LinkAdd(vlanIf); err != nil {
			return fmt.Errorf("Failed to add vlan device %s: %v", vlanIfName, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := netlink.LinkSetUp(vlan); err != nil {
		return nil, fmt.Errorf("Failed to set up vlan device %s: %v", vlanIfName, err)
	}
	d.DeviceIndex = vlan.Attrs().Index
	return vlan, nil
}

func getVlanMaster(link netlink.Link) (netlink.Link, error) {
	if vlan, ok := link.(*netlink.Vlan); !ok {
		return nil, fmt.Errorf("not a vlan device")
	} else if vlan.MasterIndex <= 0 {
		return nil, nil
	} else {
		link, err := netlink.LinkByIndex(vlan.MasterIndex)
		if err != nil {
			return nil, err
		}
		if link.Type() == "bridge" {
			return link, nil
		}
		return nil, nil
	}
}

func (d *VlanDriver) getVlanIfExist(vlanId uint16) (netlink.Link, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, err
	}
	for _, link := range links {
		if link.Type() == "vlan" {
			if vlan, ok := link.(*netlink.Vlan); !ok {
				return nil, fmt.Errorf("vlan device type case error: %T", link)
			} else {
				if vlan.VlanId == int(vlanId) && vlan.ParentIndex == d.vlanParentIndex {
					return link, nil
				}
			}
		}
	}
	return nil, nil
}

func (d *VlanDriver) MacVlanMode() bool {
	return d.Switch == "macvlan"
}

func (d *VlanDriver) IPVlanMode() bool {
	return d.Switch == "ipvlan"
}

func (d *VlanDriver) PureMode() bool {
	return d.Switch == "pure"
}
