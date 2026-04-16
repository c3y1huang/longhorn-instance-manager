package spdk

import (
	"sync"

	"github.com/cockroachdb/errors"

	"github.com/longhorn/longhorn-spdk-engine/pkg/api"
	"github.com/longhorn/longhorn-spdk-engine/pkg/types"
)

// replicaUpstream is the Upstream implementation for RAID1-mode engines.
// Each replicaUpstream wraps a single Replica process; the engine carries one
// per Replica CR backing the volume.
//
// Mutable state (Mode, BdevName) is protected by mu — these are written by
// the engine's reconciliation loop (e.g., marking ERR on RPC failure during
// ValidateAndUpdate) and read concurrently by other engine methods that
// iterate upstreams. The engine no longer has a single replicaStatus pointer
// it can field-mutate; mode/bdev-name updates go through SetMode/SetBdevName
// here.
//
// The struct holds no long-lived gRPC client. Each method that needs RPC
// access opens a fresh SPDK client (via GetServiceClient) against the
// upstream's address and closes it on return — the same lazy per-call
// pattern Engine.getReplicaClients() uses today. This eliminates reconnect
// logic and handles upstream restart implicitly: the next call dials the
// current address.
type replicaUpstream struct {
	mu sync.RWMutex

	name    string // immutable; replica name (= upstream key in Engine.upstreams)
	address string // mutable: ip:port of the replica's NVMe-oF target;
	//             // engine writes this on rebuild (replica gets a new port)

	mode     types.Mode
	bdevName string
}

// newReplicaUpstream constructs a replicaUpstream. Initial mode/bdevName are
// the engine's responsibility to populate after connectNVMfBdev succeeds.
func newReplicaUpstream(name, address string) *replicaUpstream {
	return &replicaUpstream{
		name:    name,
		address: address,
		mode:    types.ModeWO, // default until validated; engine flips to RW after connectNVMfBdev
	}
}

func (u *replicaUpstream) Name() string { return u.name }

func (u *replicaUpstream) Address() string {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.address
}

// SetAddress is replicaUpstream-specific (not on the Upstream interface). The
// engine calls this when a replica is rebuilt/replaced and gets a new
// transport address. shardGroupUpstream does not need this — the ShardGroup
// process address is set once at engine start and tracked via reconcile, not
// updated per-call.
func (u *replicaUpstream) SetAddress(address string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.address = address
}

func (u *replicaUpstream) Mode() types.Mode {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.mode
}

func (u *replicaUpstream) SetMode(m types.Mode) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.mode = m
}

func (u *replicaUpstream) BdevName() string {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.bdevName
}

func (u *replicaUpstream) SetBdevName(name string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.bdevName = name
}

// Get issues ReplicaGet against the replica's SPDK service and adapts the
// response into the topology-agnostic UpstreamView the engine consumes.
func (u *replicaUpstream) Get() (*UpstreamView, error) {
	cli, err := GetServiceClient(u.Address())
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get service client for replica %s", u.name)
	}
	defer func() { _ = cli.Close() }()

	r, err := cli.ReplicaGet(u.name)
	if err != nil {
		return nil, err
	}
	return &UpstreamView{
		SpecSize:         r.SpecSize,
		ActualSize:       r.ActualSize,
		Head:             r.Head,
		Snapshots:        r.Snapshots,
		BackingImageName: r.BackingImageName,
		LvsUUID:          r.LvsUUID,
	}, nil
}

func (u *replicaUpstream) Expand(size uint64) error {
	cli, err := GetServiceClient(u.Address())
	if err != nil {
		return errors.Wrapf(err, "failed to get service client for replica %s", u.name)
	}
	defer func() { _ = cli.Close() }()
	return cli.ReplicaExpand(u.name, size)
}

func (u *replicaUpstream) SnapshotCreate(snapshotName string, opts *api.SnapshotOptions) error {
	cli, err := GetServiceClient(u.Address())
	if err != nil {
		return errors.Wrapf(err, "failed to get service client for replica %s", u.name)
	}
	defer func() { _ = cli.Close() }()
	return cli.ReplicaSnapshotCreate(u.name, snapshotName, opts)
}

func (u *replicaUpstream) SnapshotDelete(snapshotName string) error {
	cli, err := GetServiceClient(u.Address())
	if err != nil {
		return errors.Wrapf(err, "failed to get service client for replica %s", u.name)
	}
	defer func() { _ = cli.Close() }()
	return cli.ReplicaSnapshotDelete(u.name, snapshotName)
}

func (u *replicaUpstream) SnapshotRevert(snapshotName string) error {
	cli, err := GetServiceClient(u.Address())
	if err != nil {
		return errors.Wrapf(err, "failed to get service client for replica %s", u.name)
	}
	defer func() { _ = cli.Close() }()
	return cli.ReplicaSnapshotRevert(u.name, snapshotName)
}

func (u *replicaUpstream) SnapshotPurge() error {
	cli, err := GetServiceClient(u.Address())
	if err != nil {
		return errors.Wrapf(err, "failed to get service client for replica %s", u.name)
	}
	defer func() { _ = cli.Close() }()
	return cli.ReplicaSnapshotPurge(u.name)
}

func (u *replicaUpstream) SnapshotHash(snapshotName string, rehash bool) error {
	cli, err := GetServiceClient(u.Address())
	if err != nil {
		return errors.Wrapf(err, "failed to get service client for replica %s", u.name)
	}
	defer func() { _ = cli.Close() }()
	return cli.ReplicaSnapshotHash(u.name, snapshotName, rehash)
}

func (u *replicaUpstream) BackingImageGet(name, lvsUUID string) (*api.BackingImage, error) {
	cli, err := GetServiceClient(u.Address())
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get service client for replica %s", u.name)
	}
	defer func() { _ = cli.Close() }()
	return cli.BackingImageGet(name, lvsUUID)
}

// Compile-time check that replicaUpstream satisfies the Upstream interface.
var _ Upstream = (*replicaUpstream)(nil)
