// Copyright 2015 The rkt Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// kvm.go file provides networking supporting functions for kvm flavor
package networking

import (
	"bufio"
	"crypto/sha512"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/appc/cni/pkg/ip"
	cnitypes "github.com/appc/cni/pkg/types"
	"github.com/appc/spec/schema/types"
	"github.com/hashicorp/errwrap"
	"github.com/vishvananda/netlink"

	"github.com/coreos/rkt/common"
	"github.com/coreos/rkt/networking/netinfo"
	"github.com/coreos/rkt/networking/tuntap"
	nettypes "github.com/coreos/rkt/networking/types"
)

const (
	defaultBrName     = "kvm-cni0"
	defaultSubnetFile = "/run/flannel/subnet.env"
	defaultMTU        = 1500
	masqComment       = "rkt-lkvm masquerading"
)

type BridgeNetConf struct {
	nettypes.NetConf
	BrName string `json:"bridge"`
	IsGw   bool   `json:"isGateway"`
}

// setupTapDevice creates persistent tap device
// and returns a newly created netlink.Link structure
func setupTapDevice(podID types.UUID) (netlink.Link, error) {
	// network device names are limited to 16 characters
	// the suffix %d will be replaced by the kernel with a suitable number
	nameTemplate := fmt.Sprintf("rkt-%s-tap%%d", podID.String()[0:4])
	ifName, err := tuntap.CreatePersistentIface(nameTemplate, tuntap.Tap)
	if err != nil {
		return nil, errwrap.Wrap(errors.New("tuntap persist"), err)
	}

	link, err := netlink.LinkByName(ifName)
	if err != nil {
		return nil, errwrap.Wrap(fmt.Errorf("cannot find link %q", ifName), err)
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return nil, errwrap.Wrap(fmt.Errorf("cannot set link up %q", ifName), err)
	}
	return link, nil
}

type MacVTapNetConf struct {
	nettypes.NetConf
	Master string `json:"master"`
	Mode   string `json:"mode"`
}

// setupTapDevice creates persistent macvtap device
// and returns a newly created netlink.Link structure
// using part of pod hash and interface number in interface name
func setupMacVTapDevice(podID types.UUID, config MacVTapNetConf, interfaceNumber int) (netlink.Link, error) {
	master, err := netlink.LinkByName(config.Master)
	if err != nil {
		return nil, errwrap.Wrap(fmt.Errorf("cannot find master device '%v'", config.Master), err)
	}
	var mode netlink.MacvlanMode
	switch config.Mode {
	// if not set - defaults to bridge mode as in:
	// https://github.com/coreos/rkt/blob/master/Documentation/networking.md#macvlan
	case "", "bridge":
		mode = netlink.MACVLAN_MODE_BRIDGE
	case "private":
		mode = netlink.MACVLAN_MODE_PRIVATE
	case "vepa":
		mode = netlink.MACVLAN_MODE_VEPA
	case "passthru":
		mode = netlink.MACVLAN_MODE_PASSTHRU
	default:
		return nil, fmt.Errorf("unsupported macvtap mode: %v", config.Mode)
	}
	mtu := master.Attrs().MTU
	if config.MTU != 0 {
		mtu = config.MTU
	}
	interfaceName := fmt.Sprintf("rkt-%s-vtap%d", podID.String()[0:4], interfaceNumber)
	link := &netlink.Macvtap{
		Macvlan: netlink.Macvlan{
			LinkAttrs: netlink.LinkAttrs{
				Name:        interfaceName,
				MTU:         mtu,
				ParentIndex: master.Attrs().Index,
			},
			Mode: mode,
		},
	}

	if err := netlink.LinkAdd(link); err != nil {
		return nil, errwrap.Wrap(errors.New("cannot create macvtap interface"), err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		// remove the newly added link and ignore errors, because we already are in a failed state
		_ = netlink.LinkDel(link)
		return nil, errwrap.Wrap(errors.New("cannot set up macvtap interface"), err)
	}
	return link, nil
}

// kvmSetupNetAddressing calls IPAM plugin (with a hack) to reserve an IP to be
// used by newly create tuntap pair
// in result it updates nettypes.ActiveNet.Runtime configuration
func kvmSetupNetAddressing(network *Networking, n *nettypes.ActiveNet, ifName string) error {
	// TODO: very ugly hack, that go through upper plugin, down to ipam plugin
	if err := ip.EnableIP4Forward(); err != nil {
		return errwrap.Wrap(errors.New("failed to enable forwarding"), err)
	}

	// patch plugin type only for single IPAM run time, then revert this change
	original_type := n.Conf.Type
	n.Conf.Type = n.Conf.IPAM.Type
	output, err := network.execNetPlugin("ADD", n, ifName)
	n.Conf.Type = original_type
	if err != nil {
		return errwrap.Wrap(fmt.Errorf("problem executing network plugin %q (%q)", n.Conf.Type, ifName), err)
	}

	result := cnitypes.Result{}
	if err = json.Unmarshal(output, &result); err != nil {
		return errwrap.Wrap(fmt.Errorf("error parsing %q result", n.Conf.Name), err)
	}

	if result.IP4 == nil {
		return fmt.Errorf("net-plugin returned no IPv4 configuration")
	}

	n.Runtime.IP, n.Runtime.Mask, n.Runtime.HostIP, n.Runtime.IP4 = result.IP4.IP.IP, net.IP(result.IP4.IP.Mask), result.IP4.Gateway, result.IP4

	return nil
}

func ensureHasAddr(link netlink.Link, ipn *net.IPNet) error {
	addrs, err := netlink.AddrList(link, syscall.AF_INET)
	if err != nil && err != syscall.ENOENT {
		return errwrap.Wrap(errors.New("could not get list of IP addresses"), err)
	}

	// if there're no addresses on the interface, it's ok -- we'll add one
	if len(addrs) > 0 {
		ipnStr := ipn.String()
		for _, a := range addrs {
			// string comp is actually easiest for doing IPNet comps
			if a.IPNet.String() == ipnStr {
				return nil
			}
		}
		return fmt.Errorf("%q already has an IP address different from %v", link.Attrs().Name, ipn.String())
	}

	addr := &netlink.Addr{IPNet: ipn, Label: link.Attrs().Name}
	if err := netlink.AddrAdd(link, addr); err != nil {
		return errwrap.Wrap(fmt.Errorf("could not add IP address to %q", link.Attrs().Name), err)
	}
	return nil
}

func bridgeByName(name string) (*netlink.Bridge, error) {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return nil, errwrap.Wrap(fmt.Errorf("could not lookup %q", name), err)
	}
	br, ok := l.(*netlink.Bridge)
	if !ok {
		return nil, fmt.Errorf("%q already exists but is not a bridge", name)
	}
	return br, nil
}

func ensureBridgeIsUp(brName string, mtu int) (*netlink.Bridge, error) {
	br := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: brName,
			MTU:  mtu,
		},
	}

	if err := netlink.LinkAdd(br); err != nil {
		if err != syscall.EEXIST {
			return nil, errwrap.Wrap(fmt.Errorf("could not add %q", brName), err)
		}

		// it's ok if the device already exists as long as config is similar
		br, err = bridgeByName(brName)
		if err != nil {
			return nil, err
		}
	}

	if err := netlink.LinkSetUp(br); err != nil {
		return nil, err
	}

	return br, nil
}

func addRoute(link netlink.Link, podIP net.IP) error {
	route := netlink.Route{
		LinkIndex: link.Attrs().Index,
		Scope:     netlink.SCOPE_LINK,
		Dst: &net.IPNet{
			IP:   podIP,
			Mask: net.IPv4Mask(0xff, 0xff, 0xff, 0xff),
		},
	}
	return netlink.RouteAdd(&route)
}

func removeAllRoutesOnLink(link netlink.Link) error {
	routes, err := netlink.RouteList(link, netlink.FAMILY_V4)
	if err != nil {
		return errwrap.Wrap(fmt.Errorf("cannot list routes on link %q", link.Attrs().Name), err)
	}

	for _, route := range routes {
		if err := netlink.RouteDel(&route); err != nil {
			return errwrap.Wrap(fmt.Errorf("error in time of route removal for route %q", route), err)
		}
	}

	return nil
}

func getChainName(podUUIDString, confName string) string {
	h := sha512.Sum512([]byte(podUUIDString))
	return fmt.Sprintf("CNI-%s-%x", confName, h[:8])
}

type FlannelNetConf struct {
	nettypes.NetConf

	SubnetFile string                 `json:"subnetFile"`
	Delegate   map[string]interface{} `json:"delegate"`
}

func loadFlannelNetConf(bytes []byte) (*FlannelNetConf, error) {
	n := &FlannelNetConf{
		SubnetFile: defaultSubnetFile,
	}
	if err := json.Unmarshal(bytes, n); err != nil {
		return nil, errwrap.Wrap(errors.New("failed to load netconf"), err)
	}
	return n, nil
}

type subnetEnv struct {
	nw     *net.IPNet
	sn     *net.IPNet
	mtu    int
	ipmasq bool
}

func loadFlannelSubnetEnv(fn string) (*subnetEnv, error) {
	f, err := os.Open(fn)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	se := &subnetEnv{}

	s := bufio.NewScanner(f)
	for s.Scan() {
		parts := strings.SplitN(s.Text(), "=", 2)
		switch parts[0] {
		case "FLANNEL_NETWORK":
			_, se.nw, err = net.ParseCIDR(parts[1])
			if err != nil {
				return nil, err
			}

		case "FLANNEL_SUBNET":
			_, se.sn, err = net.ParseCIDR(parts[1])
			if err != nil {
				return nil, err
			}

		case "FLANNEL_MTU":
			mtu, err := strconv.ParseUint(parts[1], 10, 32)
			if err != nil {
				return nil, err
			}
			se.mtu = int(mtu)

		case "FLANNEL_IPMASQ":
			se.ipmasq = parts[1] == "true"
		}
	}
	if err := s.Err(); err != nil {
		return nil, err
	}

	return se, nil
}

func hasKey(m map[string]interface{}, k string) bool {
	_, ok := m[k]
	return ok
}

func isString(i interface{}) bool {
	_, ok := i.(string)
	return ok
}

func kvmTransformFlannelNetwork(net *nettypes.ActiveNet) error {
	n, err := loadFlannelNetConf(net.ConfBytes)
	if err != nil {
		return err
	}

	fenv, err := loadFlannelSubnetEnv(n.SubnetFile)
	if err != nil {
		return err
	}

	if n.Delegate == nil {
		n.Delegate = make(map[string]interface{})
	} else {
		if hasKey(n.Delegate, "type") && !isString(n.Delegate["type"]) {
			return fmt.Errorf("'delegate' dictionary, if present, must have (string) 'type' field")
		}
		if hasKey(n.Delegate, "name") {
			return fmt.Errorf("'delegate' dictionary must not have 'name' field, it'll be set by flannel")
		}
		if hasKey(n.Delegate, "ipam") {
			return fmt.Errorf("'delegate' dictionary must not have 'ipam' field, it'll be set by flannel")
		}
	}

	n.Delegate["name"] = n.Name

	if !hasKey(n.Delegate, "type") {
		n.Delegate["type"] = "bridge"
	}

	if !hasKey(n.Delegate, "ipMasq") {
		// if flannel is not doing ipmasq, we should
		ipmasq := !fenv.ipmasq
		n.Delegate["ipMasq"] = ipmasq
	}

	if !hasKey(n.Delegate, "mtu") {
		mtu := fenv.mtu
		n.Delegate["mtu"] = mtu
	}

	if n.Delegate["type"].(string) == "bridge" {
		if !hasKey(n.Delegate, "isGateway") {
			n.Delegate["isGateway"] = true
		}
	}

	n.Delegate["ipam"] = map[string]interface{}{
		"type":   "host-local",
		"subnet": fenv.sn.String(),
		"routes": []cnitypes.Route{
			cnitypes.Route{
				Dst: *fenv.nw,
			},
		},
	}

	bytes, err := json.Marshal(n.Delegate)
	if err != nil {
		return errwrap.Wrap(errors.New("error in marshaling generated network settings"), err)
	}

	*net = nettypes.ActiveNet{
		ConfBytes: bytes,
		Conf:      &nettypes.NetConf{},
		Runtime: &netinfo.NetInfo{
			IP4: &cnitypes.IPConfig{},
		},
	}
	net.Conf.Name = n.Name
	net.Conf.Type = n.Delegate["type"].(string)
	net.Conf.IPMasq = n.Delegate["ipMasq"].(bool)
	net.Conf.MTU = n.Delegate["mtu"].(int)
	net.Conf.IPAM.Type = "host-local"
	return nil
}

// kvmSetup prepare new Networking to be used in kvm environment based on tuntap pair interfaces
// to allow communication with virtual machine created by lkvm tool
func kvmSetup(podRoot string, podID types.UUID, fps []ForwardedPort, netList common.NetList, localConfig string) (*Networking, error) {
	network := Networking{
		podEnv: podEnv{
			podRoot:      podRoot,
			podID:        podID,
			netsLoadList: netList,
			localConfig:  localConfig,
		},
	}
	var e error
	network.nets, e = network.loadNets()
	if e != nil {
		return nil, errwrap.Wrap(errors.New("error loading network definitions"), e)
	}

	for i, n := range network.nets {
		if n.Conf.Type == "flannel" {
			if err := kvmTransformFlannelNetwork(n); err != nil {
				return nil, errwrap.Wrap(errors.New("cannot transform flannel network into basic network"), err)
			}
		}
		switch n.Conf.Type {
		case "ptp":
			link, err := setupTapDevice(podID)
			if err != nil {
				return nil, err
			}
			ifName := link.Attrs().Name
			n.Runtime.IfName = ifName

			err = kvmSetupNetAddressing(&network, n, ifName)
			if err != nil {
				return nil, err
			}

			// add address to host tap device
			err = ensureHasAddr(
				link,
				&net.IPNet{
					IP:   n.Runtime.IP4.Gateway,
					Mask: net.IPMask(n.Runtime.Mask),
				},
			)
			if err != nil {
				return nil, errwrap.Wrap(fmt.Errorf("cannot add address to host tap device %q", ifName), err)
			}

			if err := removeAllRoutesOnLink(link); err != nil {
				return nil, errwrap.Wrap(fmt.Errorf("cannot remove route on host tap device %q", ifName), err)
			}

			if err := addRoute(link, n.Runtime.IP); err != nil {
				return nil, errwrap.Wrap(errors.New("cannot add on host direct route to pod"), err)
			}

		case "bridge":
			config := BridgeNetConf{
				NetConf: nettypes.NetConf{
					MTU: defaultMTU,
				},
				BrName: defaultBrName,
			}
			if err := json.Unmarshal(n.ConfBytes, &config); err != nil {
				return nil, errwrap.Wrap(fmt.Errorf("error parsing %q result", n.Conf.Name), err)
			}

			br, err := ensureBridgeIsUp(config.BrName, config.MTU)
			if err != nil {
				return nil, errwrap.Wrap(errors.New("error in time of bridge setup"), err)
			}
			link, err := setupTapDevice(podID)
			if err != nil {
				return nil, errwrap.Wrap(errors.New("can not setup tap device"), err)
			}
			err = netlink.LinkSetMaster(link, br)
			if err != nil {
				rErr := tuntap.RemovePersistentIface(n.Runtime.IfName, tuntap.Tap)
				if rErr != nil {
					stderr.PrintE("warning: could not cleanup tap interface", rErr)
				}
				return nil, errwrap.Wrap(errors.New("can not add tap interface to bridge"), err)
			}

			ifName := link.Attrs().Name
			n.Runtime.IfName = ifName

			err = kvmSetupNetAddressing(&network, n, ifName)
			if err != nil {
				return nil, err
			}

			if config.IsGw {
				err = ensureHasAddr(
					br,
					&net.IPNet{
						IP:   n.Runtime.IP4.Gateway,
						Mask: net.IPMask(n.Runtime.Mask),
					},
				)

				if err != nil {
					return nil, errwrap.Wrap(fmt.Errorf("cannot add address to host bridge device %q", br.Name), err)
				}
			}

		case "macvlan":
			config := MacVTapNetConf{}
			if err := json.Unmarshal(n.ConfBytes, &config); err != nil {
				return nil, errwrap.Wrap(fmt.Errorf("error parsing %q result", n.Conf.Name), err)
			}
			link, err := setupMacVTapDevice(podID, config, i)
			if err != nil {
				return nil, err
			}
			ifName := link.Attrs().Name
			n.Runtime.IfName = ifName

			err = kvmSetupNetAddressing(&network, n, ifName)
			if err != nil {
				return nil, err
			}

		default:
			return nil, fmt.Errorf("network %q have unsupported type: %q", n.Conf.Name, n.Conf.Type)
		}

		if n.Conf.IPMasq {
			chain := getChainName(podID.String(), n.Conf.Name)
			if err := ip.SetupIPMasq(&net.IPNet{
				IP:   n.Runtime.IP,
				Mask: net.IPMask(n.Runtime.Mask),
			}, chain, masqComment); err != nil {
				return nil, err
			}
		}
		network.nets[i] = n
	}
	if err := network.forwardPorts(fps, network.GetDefaultIP()); err != nil {
		return nil, err
	}

	return &network, nil
}

/*
extend Networking struct with methods to clean up kvm specific network configurations
*/

// teardownKvmNets teardown every active networking from networking by
// removing tuntap interface and releasing its ip from IPAM plugin
func (n *Networking) teardownKvmNets() {
	for _, an := range n.nets {
		switch an.Conf.Type {
		case "ptp", "bridge":
			// remove tuntap interface
			tuntap.RemovePersistentIface(an.Runtime.IfName, tuntap.Tap)

		case "macvlan":
			link, err := netlink.LinkByName(an.Runtime.IfName)
			if err != nil {
				stderr.PrintE(fmt.Sprintf("cannot find link `%v`", an.Runtime.IfName), err)
				continue
			} else {
				err := netlink.LinkDel(link)
				if err != nil {
					stderr.PrintE(fmt.Sprintf("cannot remove link `%v`", an.Runtime.IfName), err)
					continue
				}
			}

		default:
			stderr.Printf("unsupported network type: %q", an.Conf.Type)
			continue
		}
		// ugly hack again to directly call IPAM plugin to release IP
		an.Conf.Type = an.Conf.IPAM.Type

		_, err := n.execNetPlugin("DEL", an, an.Runtime.IfName)
		if err != nil {
			stderr.PrintE("error executing network plugin", err)
		}
		// remove masquerading if it was prepared
		if an.Conf.IPMasq {
			chain := getChainName(n.podID.String(), an.Conf.Name)
			err := ip.TeardownIPMasq(&net.IPNet{
				IP:   an.Runtime.IP,
				Mask: net.IPMask(an.Runtime.Mask),
			}, chain, masqComment)
			if err != nil {
				stderr.PrintE("error on removing masquerading", err)
			}
		}
	}
}

// kvmTeardown network teardown for kvm flavor based pods
// similar to Networking.Teardown but without host namespaces
func (n *Networking) kvmTeardown() {

	if err := n.unforwardPorts(); err != nil {
		stderr.PrintE("error removing forwarded ports (kvm)", err)
	}
	n.teardownKvmNets()
}

// GetActiveNetworks returns activeNets to be used as NetDescriptors
// by plugins, which are required for stage1 executor to run (only for KVM)
func (e *Networking) GetActiveNetworks() []*nettypes.ActiveNet {
	return e.nets
}
