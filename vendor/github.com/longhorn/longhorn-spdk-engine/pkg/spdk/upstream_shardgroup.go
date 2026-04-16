package spdk

import (
	"sync"

	"github.com/cockroachdb/errors"
	grpccodes "google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/longhorn/longhorn-spdk-engine/pkg/api"
	"github.com/longhorn/longhorn-spdk-engine/pkg/types"
)

// shardGroupUpstream is the Upstream implementation for EC-mode engines. The
// engine carries exactly one shardGroupUpstream — the ShardGroup process owns
// the lvstore + head lvol on bdev_ec, and the engine sees a single base bdev
// regardless of how many shards underlie it.
//
// Mutable state (Mode, BdevName) and the address are protected by mu — the
// engine writes these during reconciliation (e.g., marking ERR on RPC failure
// during ValidateAndUpdate) and reads them concurrently from other engine
// methods. Like replicaUpstream, no long-lived gRPC client is held; each
// method that needs RPC access opens a fresh SPDK client against the
// upstream's address (the ShardGroup process's IM-pod gRPC service port,
// derived from the NVMe-oF transport address) and closes it on return.
//
// The "delete-path invariant" applies here too: this type implements no
// Delete method because the Upstream interface declares none. EC-aware
// teardown lives in the ShardGroup process and is gated by its own
// cleanupRequired flag.
type shardGroupUpstream struct {
	mu sync.RWMutex

	name    string // immutable; ShardGroup name (= upstream key in Engine.upstreams)
	address string // ip:port of the ShardGroup process's NVMe-oF target

	mode     types.Mode
	bdevName string
}

// newShardGroupUpstream constructs a shardGroupUpstream. Mode defaults to WO
// until the engine flips it to RW after connectNVMfBdev succeeds, mirroring
// replicaUpstream's lifecycle.
func newShardGroupUpstream(name, address string) *shardGroupUpstream {
	return &shardGroupUpstream{
		name:    name,
		address: address,
		mode:    types.ModeWO,
	}
}

func (u *shardGroupUpstream) Name() string { return u.name }

func (u *shardGroupUpstream) Address() string {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.address
}

func (u *shardGroupUpstream) Mode() types.Mode {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.mode
}

func (u *shardGroupUpstream) SetMode(m types.Mode) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.mode = m
}

func (u *shardGroupUpstream) BdevName() string {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.bdevName
}

func (u *shardGroupUpstream) SetBdevName(name string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.bdevName = name
}

// Get issues a single ShardGroupGet against the ShardGroup process and adapts
// the response into the topology-agnostic UpstreamView. The proto carries
// SpecSize/ActualSize/Head/Snapshots populated by refreshECSnapshotMapNoLock
// against the ShardGroup process's local lvol store, so the engine never has
// to query individual shards.
func (u *shardGroupUpstream) Get() (*UpstreamView, error) {
	cli, err := GetServiceClient(u.Address())
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get service client for shardgroup %s", u.name)
	}
	defer func() { _ = cli.Close() }()

	sg, err := cli.ShardGroupGet(u.name)
	if err != nil {
		return nil, err
	}

	snapshots := map[string]*api.Lvol{}
	for snapName, snapProtoLvol := range sg.Snapshots {
		snapshots[snapName] = api.ProtoLvolToLvol(snapProtoLvol)
	}

	return &UpstreamView{
		SpecSize:   sg.SpecSize,
		ActualSize: sg.ActualSize,
		Head:       api.ProtoLvolToLvol(sg.Head),
		Snapshots:  snapshots,
		// EC volumes do not surface backing-image state at the engine layer.
		BackingImageName: "",
		LvsUUID:          sg.LvsUuid,
	}, nil
}

// Expand is a no-op for ShardGroup. Resize is driven by ShardGroupController
// (against the ShardGroup-process node), not by the engine; the engine's
// raid1 picks up the size change via SPDK's BDEV_EVENT_RESIZE AEN chain
// (see lep.md "Expand transitional state").
func (u *shardGroupUpstream) Expand(size uint64) error {
	return nil
}

// SnapshotCreate forwards to ShardGroupSnapshotCreate. The lvol-side body
// executes inside the ShardGroup process; the engine-side raid1 head lvol
// UUID is unaffected, so no engine-side bracket is needed.
//
// SnapshotOptions metadata (UserCreated/Timestamp) is intentionally dropped
// in this initial release — ShardGroupSnapshotCreate's RPC does not carry
// these fields. The ShardGroup process records its own timestamp at the
// lvstore layer.
func (u *shardGroupUpstream) SnapshotCreate(snapshotName string, opts *api.SnapshotOptions) error {
	cli, err := GetServiceClient(u.Address())
	if err != nil {
		return errors.Wrapf(err, "failed to get service client for shardgroup %s", u.name)
	}
	defer func() { _ = cli.Close() }()
	_, err = cli.ShardGroupSnapshotCreate(u.name, snapshotName)
	return err
}

func (u *shardGroupUpstream) SnapshotDelete(snapshotName string) error {
	cli, err := GetServiceClient(u.Address())
	if err != nil {
		return errors.Wrapf(err, "failed to get service client for shardgroup %s", u.name)
	}
	defer func() { _ = cli.Close() }()
	return cli.ShardGroupSnapshotDelete(u.name, snapshotName)
}

// SnapshotRevert is the upstream half of the cross-process revert sequence
// described in lep.md "EC SnapshotRevert cross-process call sequence". The
// engine layer brackets this call with raid1 teardown (steps 1-2) and
// reconnect/raid-recreate (steps 6-7); this method issues
// ShardGroupSnapshotRevert which executes the lvol-side body (steps 3-5:
// head delete + clone + namespace swap) inside the ShardGroup process.
func (u *shardGroupUpstream) SnapshotRevert(snapshotName string) error {
	cli, err := GetServiceClient(u.Address())
	if err != nil {
		return errors.Wrapf(err, "failed to get service client for shardgroup %s", u.name)
	}
	defer func() { _ = cli.Close() }()
	return cli.ShardGroupSnapshotRevert(u.name, snapshotName)
}

func (u *shardGroupUpstream) SnapshotPurge() error {
	cli, err := GetServiceClient(u.Address())
	if err != nil {
		return errors.Wrapf(err, "failed to get service client for shardgroup %s", u.name)
	}
	defer func() { _ = cli.Close() }()
	return cli.ShardGroupSnapshotPurge(u.name)
}

// SnapshotHash is not supported for EC volumes in the initial release.
// Snapshot content hashing on EC requires reading through bdev_ec to
// reconstruct plaintext data; per-shard hashing would hash erasure-coded
// chunks which is not meaningful. Returning Unimplemented matches the
// initial-release support matrix in lep.md.
func (u *shardGroupUpstream) SnapshotHash(snapshotName string, rehash bool) error {
	return grpcstatus.Errorf(grpccodes.Unimplemented, "SnapshotHash is not supported for EC volumes (shardgroup %s)", u.name)
}

// BackingImageGet returns (nil, nil). EC volumes do not surface
// backing-image state at the engine layer; the equivalent lives in the
// ShardGroup process if and when it becomes meaningful.
func (u *shardGroupUpstream) BackingImageGet(name, lvsUUID string) (*api.BackingImage, error) {
	return nil, nil
}

// Compile-time check that shardGroupUpstream satisfies the Upstream interface.
var _ Upstream = (*shardGroupUpstream)(nil)
