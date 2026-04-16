package spdk

import (
	"github.com/longhorn/longhorn-spdk-engine/pkg/api"
	"github.com/longhorn/longhorn-spdk-engine/pkg/types"
)

// Upstream is the abstraction the Engine uses to interact with a single peer
// SPDK process that contributes a base bdev to the engine's bdev_raid1
// aggregator.
//
// Two implementations exist:
//   - replicaUpstream — wraps a Replica process (RAID1 mode).
//   - shardGroupUpstream — wraps a ShardGroup process (EC mode), introduced
//     in a follow-on commit.
//
// The interface is intentionally narrow:
//   - Identity getters (Name/Address) and mutable status accessors
//     (Mode/BdevName) are pure observers/setters — no RPC.
//   - The remaining methods perform RPCs against the upstream's SPDK
//     service. Implementations are expected to construct an SPDK client
//     per-call (lazy, ephemeral) — see lep.md "Connection pattern".
//
// **Delete-path invariant.** This interface declares NO Delete() method by
// design. Engine teardown calls bdev_raid_delete + bdev_nvme_detach_controller
// directly without going through Upstream. Adding a Delete method here would
// open a layout-aware path through the engine's cleanup code — exactly the
// class of bug the EC volume detach data-loss issue exposed. Any future
// upstream-aware cleanup needs to add the method deliberately and review the
// blast radius. See lep.md "Delete-path invariant".
type Upstream interface {
	// Identity (immutable for the life of the upstream).
	Name() string
	Address() string

	// Mutable state. The engine reads/writes these directly during its
	// reconciliation loop (e.g., marking ERR on RPC failure during
	// ValidateAndUpdate, recording the local nvme bdev name after
	// connectNVMfBdev).
	Mode() types.Mode
	SetMode(m types.Mode)
	BdevName() string
	SetBdevName(name string)

	// RPC methods. Each constructs its own SPDK client per-call.

	// Get returns a snapshot of the upstream's current state. For
	// replicaUpstream this issues ReplicaGet; for shardGroupUpstream it
	// issues ShardGroupGet (the engine never queries individual shards).
	Get() (*UpstreamView, error)

	// Expand requests the upstream resize its head lvol. For
	// replicaUpstream this dispatches to ReplicaExpand; for
	// shardGroupUpstream this is a no-op (ShardGroupExpand is driven by
	// the ShardGroup controller, not by the engine).
	Expand(size uint64) error

	// Snapshot operations forward to the upstream's snapshot RPCs.
	SnapshotCreate(snapshotName string, opts *api.SnapshotOptions) error
	SnapshotDelete(snapshotName string) error
	SnapshotRevert(snapshotName string) error
	SnapshotPurge() error
	SnapshotHash(snapshotName string, rehash bool) error

	// BackingImageGet resolves a backing-image record on the upstream.
	// RAID1 dispatches to BackingImageGet on the replica's SPDK service;
	// shardGroupUpstream returns (nil, nil) — EC volumes do not surface
	// backing-image state at the engine layer.
	BackingImageGet(name, lvsUUID string) (*api.BackingImage, error)
}

// UpstreamFactory builds an Upstream for a given (name, address) pair. The
// engine receives the factory at Create time from the server layer, which
// picks newReplicaUpstream or newShardGroupUpstream keyed on
// EngineCreateRequest.DataLayoutType. The engine itself never reads the
// layout — it just calls the factory once per replica_address_map entry.
// This is the only seam through which layout enters the engine; there is no
// dataLayoutType field on Engine, and engine.go contains no
// `if dataLayout == ...` branches.
type UpstreamFactory func(name, address string) Upstream

// UpstreamView is the engine's view of an upstream's current state, returned
// by Upstream.Get(). Both implementations populate the same fields from their
// respective RPC responses (ReplicaGet vs. ShardGroupGet); the engine reads
// the unified shape without knowing which topology produced it.
type UpstreamView struct {
	SpecSize         uint64
	ActualSize       uint64
	Head             *api.Lvol
	Snapshots        map[string]*api.Lvol
	BackingImageName string
	LvsUUID          string
}
