package driver

import (
	"net"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/pkg/errors"
	"github.com/vishvananda/netlink"
	"github.com/yunify/hostnic-cni/pkg/ipwrapper"
	"github.com/yunify/hostnic-cni/pkg/netlinkwrapper"
	"github.com/yunify/hostnic-cni/pkg/networkutils"
	"github.com/yunify/hostnic-cni/pkg/nswrapper"
	"golang.org/x/sys/unix"
	"k8s.io/klog"
)

const (
	// ip rules priority and leave 512 gap for future
	toContainerRulePriority = 512
	// 1024 is reserved for (ip rule not to <vpc's subnet> table main)
	fromContainerRulePriority = 1536

	// main routing table number
	mainRouteTable = unix.RT_TABLE_MAIN
	// MTU of veth - ENI MTU defined in pkg/networkutils/network.go
	ethernetMTU = 9001
)

// NetworkAPIs defines network API calls
type NetworkAPIs interface {
	SetupNS(hostVethName string, contVethName string, netnsPath string, addr *net.IPNet, table int, vpcCIDRs []string, tunnelNet string, useExternalSNAT bool) error
	TeardownNS(addr *net.IPNet, table int) error
}

type linuxNetwork struct {
	netLink          netlinkwrapper.NetLink
	ns               nswrapper.NS
	ip               ipwrapper.IP
	containerNetlink netlinkwrapper.NetLink
	networkClient    networkutils.NetworkAPIs
}

func newDriverNetworkAPI(netLink netlinkwrapper.NetLink, containerNetlink netlinkwrapper.NetLink, networkClient networkutils.NetworkAPIs, ns nswrapper.NS, ip ipwrapper.IP) NetworkAPIs {
	return &linuxNetwork{
		netLink:          netLink,
		ns:               ns,
		ip:               ip,
		containerNetlink: containerNetlink,
		networkClient:    networkClient,
	}
}

// New creates linuxNetwork object
func New() NetworkAPIs {
	return newDriverNetworkAPI(netlinkwrapper.NewNetLink(), netlinkwrapper.NewNetLink(), networkutils.New(), nswrapper.NewNS(), ipwrapper.NewIP())
}

// createVethPairContext wraps the parameters and the method to create the
// veth pair to attach the container namespace
type createVethPairContext struct {
	contVethName string
	hostVethName string
	addr         *net.IPNet
	netLink      netlinkwrapper.NetLink
	ip           ipwrapper.IP
}

func newCreateVethPairContext(contVethName string, hostVethName string, addr *net.IPNet, netLink netlinkwrapper.NetLink, ip ipwrapper.IP) *createVethPairContext {
	return &createVethPairContext{
		contVethName: contVethName,
		hostVethName: hostVethName,
		addr:         addr,
		netLink:      netLink,
		ip:           ip,
	}
}

// run defines the closure to execute within the container's namespace to
// create the veth pair
func (createVethContext *createVethPairContext) run(hostNS ns.NetNS) error {
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{
			Name:   createVethContext.contVethName,
			Flags:  net.FlagUp,
			MTU:    ethernetMTU,
			TxQLen: -1,
		},
		PeerName: createVethContext.hostVethName,
	}

	if err := createVethContext.netLink.LinkAdd(veth); err != nil {
		return err
	}

	hostVeth, err := createVethContext.netLink.LinkByName(createVethContext.hostVethName)
	if err != nil {
		return errors.Wrapf(err, "setup NS network: failed to find link %q", createVethContext.hostVethName)
	}

	// Explicitly set the veth to UP state, because netlink doesn't always do that on all the platforms with net.FlagUp.
	// veth won't get a link local address unless it's set to UP state.
	if err = createVethContext.netLink.LinkSetUp(hostVeth); err != nil {
		return errors.Wrapf(err, "setup NS network: failed to set link %q up", createVethContext.hostVethName)
	}

	contVeth, err := createVethContext.netLink.LinkByName(createVethContext.contVethName)
	if err != nil {
		return errors.Wrapf(err, "setup NS network: failed to find link %q", createVethContext.contVethName)
	}

	// Explicitly set the veth to UP state, because netlink doesn't always do that on all the platforms with net.FlagUp.
	// veth won't get a link local address unless it's set to UP state.
	if err = createVethContext.netLink.LinkSetUp(contVeth); err != nil {
		return errors.Wrapf(err, "setup NS network: failed to set link %q up", createVethContext.contVethName)
	}

	// Add a connected route to a dummy next hop (169.254.1.1)
	// # ip route show
	// default via 169.254.1.1 dev eth0
	// 169.254.1.1 dev eth0
	gw := net.IPv4(169, 254, 1, 1)
	gwNet := &net.IPNet{IP: gw, Mask: net.CIDRMask(32, 32)}

	if err = createVethContext.netLink.RouteReplace(&netlink.Route{
		LinkIndex: contVeth.Attrs().Index,
		Scope:     netlink.SCOPE_LINK,
		Dst:       gwNet}); err != nil {
		return errors.Wrap(err, "setup NS network: failed to add default gateway")
	}

	// Add a default route via dummy next hop(169.254.1.1). Then all outgoing traffic will be routed by this
	// default route via dummy next hop (169.254.1.1).
	if err = createVethContext.ip.AddDefaultRoute(gwNet.IP, contVeth); err != nil {
		return errors.Wrap(err, "setup NS network: failed to add default route")
	}

	if err = createVethContext.netLink.AddrAdd(contVeth, &netlink.Addr{IPNet: createVethContext.addr}); err != nil {
		return errors.Wrapf(err, "setup NS network: failed to add IP addr %s to %q", createVethContext.addr.String(), createVethContext.contVethName)
	}

	// add static ARP entry for default gateway
	// we are using routed mode on the host and container need this static ARP entry to resolve its default gateway.
	neigh := &netlink.Neigh{
		LinkIndex:    contVeth.Attrs().Index,
		State:        netlink.NUD_PERMANENT,
		IP:           gwNet.IP,
		HardwareAddr: hostVeth.Attrs().HardwareAddr,
	}

	if err = createVethContext.netLink.NeighAdd(neigh); err != nil {
		return errors.Wrap(err, "setup NS network: failed to add static ARP")
	}

	// Now that the everything has been successfully set up in the container, move the "host" end of the
	// veth into the host namespace.
	if err = createVethContext.netLink.LinkSetNsFd(hostVeth, int(hostNS.Fd())); err != nil {
		return errors.Wrap(err, "setup NS network: failed to move veth to host netns")
	}
	return nil
}

// SetupNS wires up linux networking for a pod's network
func (os *linuxNetwork) SetupNS(hostVethName string, contVethName string, netnsPath string, addr *net.IPNet, table int, vpcCIDRs []string, tunnelNet string, useExternalSNAT bool) error {
	klog.V(2).Infof("SetupNS: hostVethName=%s,contVethName=%s, netnsPath=%s table=%d\n", hostVethName, contVethName, netnsPath, table)
	return setupNS(hostVethName, contVethName, netnsPath, addr, table, vpcCIDRs, useExternalSNAT, tunnelNet, os.netLink, os.containerNetlink, os.ns, os.ip)
}

func setupNS(hostVethName string, contVethName string, netnsPath string, addr *net.IPNet, table int, vpcCIDRs []string, useExternalSNAT bool, tunnelNet string,
	netLink netlinkwrapper.NetLink, containerNetlink netlinkwrapper.NetLink, ns nswrapper.NS, ip ipwrapper.IP) error {
	// Clean up if hostVeth exists.
	if oldHostVeth, err := netLink.LinkByName(hostVethName); err == nil {
		if err = netLink.LinkDel(oldHostVeth); err != nil {
			return errors.Wrapf(err, "setupNS network: failed to delete old hostVeth %q", hostVethName)
		}
		klog.V(2).Infof("Clean up old hostVeth: %v\n", hostVethName)
	}

	createVethContext := newCreateVethPairContext(contVethName, hostVethName, addr, containerNetlink, ip)
	if err := ns.WithNetNSPath(netnsPath, createVethContext.run); err != nil {
		klog.Errorf("Failed to setup NS network %v", err)
		return errors.Wrap(err, "setupNS network: failed to setup NS network")
	}

	hostVeth, err := netLink.LinkByName(hostVethName)
	if err != nil {
		return errors.Wrapf(err, "setupNS network: failed to find link %q", hostVethName)
	}

	// Explicitly set the veth to UP state, because netlink doesn't always do that on all the platforms with net.FlagUp.
	// veth won't get a link local address unless it's set to UP state.
	if err = netLink.LinkSetUp(hostVeth); err != nil {
		return errors.Wrapf(err, "setupNS network: failed to set link %q up", hostVethName)
	}

	klog.V(2).Infof("Setup host route outgoing hostVeth, LinkIndex %d\n", hostVeth.Attrs().Index)
	addrHostAddr := &net.IPNet{
		IP:   addr.IP,
		Mask: net.CIDRMask(32, 32)}

	// Add host route
	route := netlink.Route{
		LinkIndex: hostVeth.Attrs().Index,
		Scope:     netlink.SCOPE_LINK,
		Dst:       addrHostAddr}

	// Add or replace route
	if err := netLink.RouteReplace(&route); err != nil {
		return errors.Wrapf(err, "setupNS: unable to add or replace route entry for %s", route.Dst.IP.String())
	}
	klog.V(2).Infof("Successfully set host route to be %s/0", route.Dst.IP.String())

	toContainerFlag := true
	err = addContainerRule(netLink, toContainerFlag, addr, toContainerRulePriority, mainRouteTable)

	if err != nil {
		klog.Errorf("Failed to add toContainer rule for %s err=%v, ", addr.String(), err)
		return errors.Wrap(err, "setupNS network: failed to add toContainer")
	}

	klog.V(1).Infof("Added toContainer rule for %s", addr.String())

	// add from-pod rule, only need it when it is not primary ENI
	if table > 0 {
		if useExternalSNAT {
			// add rule: 1536: from <podIP> use table <table>
			toContainerFlag = false
			err = addContainerRule(netLink, toContainerFlag, addr, fromContainerRulePriority, table)

			if err != nil {
				klog.Errorf("Failed to add fromContainer rule for %s err: %v", addr.String(), err)
				return errors.Wrap(err, "add NS network: failed to add fromContainer rule")
			}
			klog.V(1).Infof("Added rule priority %d from %s table %d", fromContainerRulePriority, addr.String(), table)
		} else {
			if tunnelNet != "" {
				klog.V(2).Infof("Append tunnel net %s to vpc cidrs", tunnelNet)
				vpcCIDRs = append(vpcCIDRs, tunnelNet)
			}
			// add rule: 1536: list of from <podIP> to <vpcCIDR> use table <table>
			for _, cidr := range vpcCIDRs {
				podRule := netLink.NewRule()
				_, podRule.Dst, _ = net.ParseCIDR(cidr)
				podRule.Src = addr
				podRule.Table = table
				podRule.Priority = fromContainerRulePriority

				err = netLink.RuleAdd(podRule)
				if networkutils.IsRuleExistsError(err) {
					klog.Warningf("Rule already exists [%v]", podRule)
				} else {
					if err != nil {
						klog.Errorf("Failed to add pod IP rule [%v]: %v", podRule, err)
						return errors.Wrapf(err, "setupNS: failed to add pod rule [%v]", podRule)
					}
				}
				var toDst string

				if podRule.Dst != nil {
					toDst = podRule.Dst.String()
				}
				klog.V(1).Infof("Successfully added pod rule[%v] to %s", podRule, toDst)
			}
		}
	}
	return nil
}

func addContainerRule(netLink netlinkwrapper.NetLink, isToContainer bool, addr *net.IPNet, priority int, table int) error {
	containerRule := netLink.NewRule()

	if isToContainer {
		containerRule.Dst = addr
	} else {
		containerRule.Src = addr
	}
	containerRule.Table = table
	containerRule.Priority = priority

	err := netLink.RuleDel(containerRule)
	if err != nil && !networkutils.ContainsNoSuchRule(err) {
		return errors.Wrapf(err, "addContainerRule: failed to delete old container rule for %s", addr.String())
	}

	err = netLink.RuleAdd(containerRule)
	if err != nil {
		return errors.Wrapf(err, "addContainerRule: failed to add container rule for %s", addr.String())
	}
	return nil
}

// TeardownPodNetwork cleanup ip rules
func (os *linuxNetwork) TeardownNS(addr *net.IPNet, table int) error {
	klog.V(2).Infof("TeardownNS: addr %s, table %d", addr.String(), table)
	return tearDownNS(addr, table, os.netLink, os.networkClient)
}

func tearDownNS(addr *net.IPNet, table int, netLink netlinkwrapper.NetLink, networkClient networkutils.NetworkAPIs) error {
	// remove to-pod rule
	toContainerRule := netLink.NewRule()
	toContainerRule.Dst = addr
	toContainerRule.Priority = toContainerRulePriority
	err := netLink.RuleDel(toContainerRule)

	if err != nil {
		klog.Errorf("Failed to delete toContainer rule for %s err %v", addr.String(), err)
	} else {
		klog.V(1).Infof("Delete toContainer rule for %s ", addr.String())
	}

	if table > 0 {
		// remove from-pod rule only for non main table
		err := deleteRuleListBySrc(networkClient, *addr)
		if err != nil {
			klog.Errorf("Failed to delete fromContainer for %s %v", addr.String(), err)
			return errors.Wrapf(err, "delete NS network: failed to delete fromContainer rule for %s", addr.String())
		}
		klog.V(1).Infof("Delete fromContainer rule for %s in table %d", addr.String(), table)
	}

	addrHostAddr := &net.IPNet{
		IP:   addr.IP,
		Mask: net.CIDRMask(32, 32)}

	// cleanup host route:
	if err = netLink.RouteDel(&netlink.Route{
		Scope: netlink.SCOPE_LINK,
		Dst:   addrHostAddr}); err != nil {
		klog.Errorf("delete NS network: failed to delete host route for %s, %v", addr.String(), err)
	}
	return nil
}

func deleteRuleListBySrc(networkClient networkutils.NetworkAPIs, src net.IPNet) error {
	return networkClient.DeleteRuleListBySrc(src)
}
