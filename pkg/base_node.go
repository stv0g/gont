package gont

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"

	"github.com/stv0g/gont/internal/utils"
	nl "github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

type BaseNode struct {
	Node
	*Namespace

	network *Network

	name string

	BasePath string

	Interfaces []*Interface

	// Options
	ConfiguredInterfaces    []*Interface
	ExistingNamespace       string
	ExistingDockerContainer string

	logger *zap.Logger
}

func (n *Network) AddNode(name string, opts ...Option) (*BaseNode, error) {
	var err error

	basePath := filepath.Join(n.BasePath, "nodes", name)
	for _, path := range []string{"ns"} {
		path = filepath.Join(basePath, path)
		if err := os.MkdirAll(path, 0755); err != nil {
			return nil, err
		}
	}

	node := &BaseNode{
		name:     name,
		network:  n,
		BasePath: basePath,
		logger:   zap.L().Named("node").With(zap.String("node", name)),
	}

	node.logger.Info("Adding new node")

	for _, opt := range opts {
		if nopt, ok := opt.(NodeOption); ok {
			nopt.Apply(node)
		}
	}

	if node.ExistingNamespace != "" {
		// Use an existing namespace created by "ip netns add"
		if nsh, err := netns.GetFromName(node.ExistingNamespace); err != nil {
			return nil, fmt.Errorf("failed to find existing network namespace %s: %w", node.ExistingNamespace, err)
		} else {
			node.Namespace = &Namespace{
				Name:     node.ExistingNamespace,
				NsHandle: nsh,
			}
		}
	} else if node.ExistingDockerContainer != "" {
		// Use an existing net namespace from a Docker container
		if nsh, err := netns.GetFromDocker(node.ExistingDockerContainer); err != nil {
			return nil, fmt.Errorf("failed to find existing docker container %s: %w", node.ExistingNamespace, err)
		} else {
			node.Namespace = &Namespace{
				Name:     node.ExistingDockerContainer,
				NsHandle: nsh,
			}
		}
	} else {
		// Create a new network namespace
		nsName := fmt.Sprintf("%s%s-%s", n.NSPrefix, n.Name, name)
		if node.Namespace, err = NewNamespace(nsName); err != nil {
			return nil, err
		}
	}

	if node.Handle == nil {
		node.Handle, err = nl.NewHandleAt(node.NsHandle)
		if err != nil {
			return nil, err
		}
	}

	src := fmt.Sprintf("/proc/self/fd/%d", int(node.NsHandle))
	dst := filepath.Join(basePath, "ns", "net")
	if err := utils.Touch(dst); err != nil {
		return nil, err
	}
	if err := unix.Mount(src, dst, "", syscall.MS_BIND, ""); err != nil {
		return nil, fmt.Errorf("failed to bind mount netns fd: %s", err)
	}

	n.Register(node)

	return node, nil
}

// Getter

func (n *BaseNode) NetNSHandle() netns.NsHandle {
	return n.NsHandle
}

func (n *BaseNode) NetlinkHandle() *nl.Handle {
	return n.Handle
}

func (n *BaseNode) Name() string {
	return n.name
}

func (n *BaseNode) String() string {
	return fmt.Sprintf("%s/%s", n.Network(), n.Name())
}

func (n *BaseNode) Network() *Network {
	return n.network
}

func (n *BaseNode) Interface(name string) *Interface {
	for _, i := range n.Interfaces {
		if i.Name == name {
			return i
		}
	}

	return nil
}

func (n *BaseNode) ConfigureInterface(i *Interface) error {
	logger := n.logger.With(zap.Any("intf", i))
	logger.Info("Configuring interface")

	if i.LinkAttrs.MTU != 0 {
		logger.Info("Setting interface MTU",
			zap.Int("mtu", i.LinkAttrs.MTU),
		)
		if err := n.Handle.LinkSetMTU(i.Link, i.LinkAttrs.MTU); err != nil {
			return err
		}
	}

	if i.LinkAttrs.HardwareAddr != nil {
		logger.Info("Setting interface MAC address",
			zap.Any("mac", i.LinkAttrs.HardwareAddr),
		)
		if err := n.Handle.LinkSetHardwareAddr(i.Link, i.LinkAttrs.HardwareAddr); err != nil {
			return err
		}
	}

	if i.LinkAttrs.TxQLen > 0 {
		logger.Info("Setting interface transmit queue length",
			zap.Int("txqlen", i.LinkAttrs.TxQLen),
		)
		if err := n.Handle.LinkSetTxQLen(i.Link, i.LinkAttrs.TxQLen); err != nil {
			return err
		}
	}

	if i.LinkAttrs.Group != 0 {
		logger.Info("Setting interface group",
			zap.Uint32("group", i.LinkAttrs.Group),
		)
		if err := n.Handle.LinkSetGroup(i.Link, int(i.LinkAttrs.Group)); err != nil {
			return err
		}
	}

	var pHandle uint32 = nl.HANDLE_ROOT
	if i.Flags&WithQdiscNetem != 0 {
		attr := nl.QdiscAttrs{
			LinkIndex: i.Link.Attrs().Index,
			Handle:    nl.MakeHandle(1, 0),
			Parent:    pHandle,
		}

		netem := nl.NewNetem(attr, i.Netem)

		logger.Info("Adding Netem qdisc to interface")
		if err := n.Handle.QdiscAdd(netem); err != nil {
			return err
		}

		pHandle = netem.Handle
	}
	if i.Flags&WithQdiscTbf != 0 {
		i.Tbf.LinkIndex = i.Link.Attrs().Index
		i.Tbf.Limit = 0x7000
		i.Tbf.Minburst = 1600
		i.Tbf.Buffer = 300000
		i.Tbf.Peakrate = 0x1000000
		i.Tbf.QdiscAttrs = nl.QdiscAttrs{
			LinkIndex: i.Link.Attrs().Index,
			Handle:    nl.MakeHandle(2, 0),
			Parent:    pHandle,
		}

		logger.Info("Adding TBF qdisc to interface")
		if err := n.Handle.QdiscAdd(&i.Tbf); err != nil {
			return err
		}
	}

	logger.Info("Setting interface up")
	if err := n.Handle.LinkSetUp(i.Link); err != nil {
		return err
	}

	n.Interfaces = append(n.Interfaces, i)

	if err := n.network.GenerateHostsFile(); err != nil {
		return fmt.Errorf("failed to update hosts file")
	}

	return nil
}

func (n *BaseNode) Teardown() error {
	if err := n.Namespace.Close(); err != nil {
		return err
	}

	nsMount := filepath.Join(n.BasePath, "ns", "net")
	if err := unix.Unmount(nsMount, 0); err != nil {
		return err
	}

	if err := os.RemoveAll(n.BasePath); err != nil {
		return err
	}

	return nil
}

func (n *BaseNode) WriteProcFS(path, value string) error {
	n.logger.Info("Updating procfs",
		zap.String("path", path),
		zap.String("value", value),
	)

	return n.RunFunc(func() error {
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer f.Close()

		_, err = f.WriteString(value)

		return err
	})
}

func (n *BaseNode) EnableForwarding() error {
	if err := n.WriteProcFS("/proc/sys/net/ipv4/conf/all/forwarding", "1"); err != nil {
		return err
	}

	if err := n.WriteProcFS("/proc/sys/net/ipv6/conf/all/forwarding", "1"); err != nil {
		return err
	}

	return nil
}

func (n *BaseNode) LinkAddAddress(name string, addr net.IPNet) error {
	link, err := n.Handle.LinkByName(name)
	if err != nil {
		return err
	}

	nlAddr := &nl.Addr{
		IPNet: &addr,
	}

	n.logger.Info("Adding new address to interface",
		zap.String("intf", fmt.Sprintf("%s/%s", n, name)),
		zap.String("addr", addr.String()),
	)

	if err := n.Handle.AddrAdd(link, nlAddr); err != nil {
		return err
	}

	return err
}

func (n *BaseNode) AddRoute(r nl.Route) error {
	n.logger.Info("Add route",
		zap.Any("dst", r.Dst),
		zap.Any("gw", r.Gw),
	)

	return n.Handle.RouteAdd(&r)
}

func (n *BaseNode) AddDefaultRoute(gw net.IP) error {
	if gw.To4() != nil {
		return n.AddRoute(nl.Route{
			Dst: &DefaultIPv4Mask,
			Gw:  gw,
		})
	} else {
		return n.AddRoute(nl.Route{
			Dst: &DefaultIPv6Mask,
			Gw:  gw,
		})
	}
}

func (n *BaseNode) AddInterface(i *Interface) {
	n.ConfiguredInterfaces = append(n.ConfiguredInterfaces, i)
}
