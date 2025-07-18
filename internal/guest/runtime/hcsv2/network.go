//go:build linux
// +build linux

package hcsv2

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/vishvananda/netns"
	"go.opencensus.io/trace"

	"github.com/Microsoft/hcsshim/internal/bridgeutils/gcserr"
	"github.com/Microsoft/hcsshim/internal/guest/network"
	"github.com/Microsoft/hcsshim/internal/oc"
	"github.com/Microsoft/hcsshim/internal/protocol/guestresource"
)

var (
	// namespaceSync protects access to `namespaces`.
	namespaceSync sync.Mutex
	// namespaces is the set of `in-memory` namespace adapters know to the GCS.
	// These may or may not be assigned to a container as there is support for
	// pre-Add and post-Add.
	namespaces map[string]*namespace

	networkInstanceIDToName = network.InstanceIDToName
)

func init() {
	namespaces = make(map[string]*namespace)
}

// getNetworkNamespace returns the namespace found by `id`. If the namespace
// does not exist returns `gcserr.HrErrNotFound`.
func getNetworkNamespace(id string) (*namespace, error) {
	id = strings.ToLower(id)

	namespaceSync.Lock()
	defer namespaceSync.Unlock()

	ns, ok := namespaces[id]
	if !ok {
		return nil, gcserr.WrapHresult(errors.Errorf("namespace '%s' not found", id), gcserr.HrErrNotFound)
	}
	return ns, nil
}

// GetOrAddNetworkNamespace returns the namespace found by `id` or creates a new
// one and assigns `id.
func GetOrAddNetworkNamespace(id string) *namespace {
	id = strings.ToLower(id)

	namespaceSync.Lock()
	defer namespaceSync.Unlock()

	ns, ok := namespaces[id]
	if !ok {
		ns = &namespace{
			id: id,
		}
		namespaces[id] = ns
	}
	return ns
}

// RemoveNetworkNamespace removes the in-memory `namespace` found by `id`.
func RemoveNetworkNamespace(ctx context.Context, id string) (err error) {
	_, span := oc.StartSpan(ctx, "hcsv2::RemoveNetworkNamespace")
	defer span.End()
	defer func() { oc.SetSpanStatus(span, err) }()

	id = strings.ToLower(id)
	span.AddAttributes(trace.StringAttribute("id", id))

	namespaceSync.Lock()
	defer namespaceSync.Unlock()

	ns, ok := namespaces[id]
	if ok {
		ns.m.Lock()
		defer ns.m.Unlock()
		if len(ns.nics) > 0 {
			return errors.Errorf("network namespace '%s' contains adapters", id)
		}
		delete(namespaces, id)
	}

	return nil
}

// namespace struct maps all vNIC's to the namespace ID used by the HNS.
type namespace struct {
	id string

	m    sync.Mutex
	pid  int
	nics []*nicInNamespace
}

// ID is the id of the network namespace
func (n *namespace) ID() string {
	return n.id
}

// AssignContainerPid assigns `pid` to `n` but does NOT move any previously
// assigned adapters into this namespace. The caller MUST call `Sync()` to
// complete this operation.
func (n *namespace) AssignContainerPid(ctx context.Context, pid int) (err error) {
	_, span := oc.StartSpan(ctx, "namespace::AssignContainerPid")
	defer span.End()
	defer func() { oc.SetSpanStatus(span, err) }()
	span.AddAttributes(
		trace.StringAttribute("namespace", n.id),
		trace.Int64Attribute("pid", int64(pid)))

	n.m.Lock()
	defer n.m.Unlock()

	if n.pid != 0 {
		return errors.Errorf("previously assigned container pid %d to network namespace %q", n.pid, n.id)
	}

	n.pid = pid
	return nil
}

// Adapters returns a copy of the adapters assigned to `n` at the time of the
// call.
func (n *namespace) Adapters() []*guestresource.LCOWNetworkAdapter {
	n.m.Lock()
	defer n.m.Unlock()

	adps := make([]*guestresource.LCOWNetworkAdapter, len(n.nics))
	for i, nin := range n.nics {
		adps[i] = nin.adapter
	}
	return adps
}

// AddAdapter adds `adp` to `n` but does NOT move the adapter into the network
// namespace assigned to `n`. A user must call `Sync()` to complete this
// operation.
func (n *namespace) AddAdapter(ctx context.Context, adp *guestresource.LCOWNetworkAdapter) (err error) {
	ctx, span := oc.StartSpan(ctx, "namespace::AddAdapter")
	defer span.End()
	defer func() { oc.SetSpanStatus(span, err) }()
	span.AddAttributes(
		trace.StringAttribute("namespace", n.id),
		trace.StringAttribute("adapter", fmt.Sprintf("%+v", adp)))

	n.m.Lock()
	defer n.m.Unlock()

	for _, nic := range n.nics {
		if strings.EqualFold(nic.adapter.ID, adp.ID) {
			return errors.Errorf("adapter with id: '%s' already present in namespace", adp.ID)
		}
	}

	resolveCtx, cancel := context.WithTimeout(ctx, time.Second*5)
	defer cancel()
	ifname, err := networkInstanceIDToName(resolveCtx, adp.ID, adp.VPCIAssigned)
	if err != nil {
		return err
	}
	n.nics = append(n.nics, &nicInNamespace{
		adapter: adp,
		ifname:  ifname,
	})
	return nil
}

// RemoveAdapter removes the adapter matching `id` from `n`. If `id` is not
// found returns no error.
func (n *namespace) RemoveAdapter(ctx context.Context, id string) (err error) {
	_, span := oc.StartSpan(ctx, "namespace::RemoveAdapter")
	defer span.End()
	defer func() { oc.SetSpanStatus(span, err) }()
	span.AddAttributes(
		trace.StringAttribute("namespace", n.id),
		trace.StringAttribute("adapterID", id))

	n.m.Lock()
	defer n.m.Unlock()

	// TODO: do we need to remove anything guestside from a sandbox namespace?

	i := -1
	for j, nic := range n.nics {
		if strings.EqualFold(nic.adapter.ID, id) {
			i = j
			break
		}
	}
	if i > -1 {
		n.nics = append(n.nics[:i], n.nics[i+1:]...)
	}
	return nil
}

// Sync moves all adapters to the network namespace of `n` if assigned.
func (n *namespace) Sync(ctx context.Context) (err error) {
	ctx, span := oc.StartSpan(ctx, "namespace::Sync")
	defer span.End()
	defer func() { oc.SetSpanStatus(span, err) }()
	span.AddAttributes(trace.StringAttribute("namespace", n.id))

	n.m.Lock()
	defer n.m.Unlock()

	if n.pid != 0 {
		for i, a := range n.nics {
			if a.adapter.PolicyBasedRouting {
				a.adapter.EnableLowMetric = (i > 0)
			}
			err = a.assignToPid(ctx, n.pid)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// nicInNamespace represents a single network adapter that has been added to the
// guest and its mapping to the linux `ifname`.
type nicInNamespace struct {
	// adapter captures the network settings when the nic was added
	adapter *guestresource.LCOWNetworkAdapter
	// ifname is the interface name resolved for this adapter
	ifname string
	// assignedPid will be `0` for any nic in this namespace that has not been
	// moved into a specific pid network namespace.
	assignedPid int
}

// assignToPid assigns `nin.adapter`, represented by `nin.ifname` to `pid`.
func (nin *nicInNamespace) assignToPid(ctx context.Context, pid int) (err error) {
	ctx, span := oc.StartSpan(ctx, "nicInNamespace::assignToPid")
	defer span.End()
	defer func() { oc.SetSpanStatus(span, err) }()
	span.AddAttributes(
		trace.StringAttribute("adapterID", nin.adapter.ID),
		trace.StringAttribute("ifname", nin.ifname),
		trace.Int64Attribute("pid", int64(pid)))

	if err := network.MoveInterfaceToNS(nin.ifname, pid); err != nil {
		return errors.Wrapf(err, "failed to move interface %s to network namespace", nin.ifname)
	}

	// Get a reference to the new network namespace
	ns, err := netns.GetFromPid(pid)
	if err != nil {
		return errors.Wrapf(err, "netns.GetFromPid(%d) failed", pid)
	}
	defer ns.Close()

	netNSCfg := func() error {
		return network.NetNSConfig(ctx, nin.ifname, pid, nin.adapter)
	}

	if err := network.DoInNetNS(ns, netNSCfg); err != nil {
		return errors.Wrapf(err, "failed to configure adapter aid: %s, if id: %s", nin.adapter.ID, nin.ifname)
	}
	nin.assignedPid = pid
	return nil
}
