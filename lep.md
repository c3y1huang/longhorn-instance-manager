# V2 Sharding

This document provides a detailed proposal and design for the Longhorn V2 sharding feature - erasure coded storage with Reed-Solomon coded data distribution across multiple physical devices.

## Summary

Longhorn V2 currently replicates entire volumes via RAID1 across nodes - each replica stores a full copy of the data. This provides simple fault tolerance but at Nx storage overhead.

V2 Sharding aims to provide a new storage tier option with erasure coding (EC). A k+m EC configuration splits data into k chunks and m parity chunks using Reed-Solomon coding (ISA-L). Any m simultaneous disk failures can be tolerated while storing only (k+m)/k * the original data, compared to m+1 * for replication. For example:
```
A 4+2 EC layer provides 2-failure tolerance using 6 devices at 1.5* storage overhead (150 GB raw for 100 GB usable), compared to 3-way replication with provides 2-failure tolerance using 3 devices overhead (300GB raw for 100GB usable).
```

This enhancement introduces a new SPDK bdev module (`bdev_ec`) that sits below the lvol/blobstore layer and above the base bdev layer, performing transparent erasure coding on all I/O. The EC bdev's base devices can be remote NVMe-oF attached bdevs on other nodes. This breaks the physical node boundary of the current replication model: instead of each node holding a full replica, chunks are distributed across nodes at the EC layer. Longhorn's control plane sees only a single lvol volume and does not manage chunk placement - the cross-node distribution is entirely transparent at the SPDK layer.

Fault tolerance comes from Reed-Solomon coding across the distributed base devices.

### Related Issues

- https://github.com/longhorn/longhorn/issues/1061

## Motivation

### Goals


SPDK layout:
- Reduce storage overhead from 2-3x (RAID1 replication) to 1.25-1.5* (erasure coding) while having the same fault tolerance. Specifically: a 4+1 EC layer (1.25* overhead) tolerate 1 disk failure, matching 2-way replication (2* overhead); a 4+2 EC layout (1.5* overhead) tolerate 2 disk failures, matching 3-way replication (3* overhead).
- Support online capacity expansion (resize).
- Support hot-swap disk replacement and rebuild for failed disks.
- Support existing Longhorn features.

### Non-goals [optional]

- k -> k+1 grow (`bdev_ec_grow`). Adding a new data disk to an existing array would change the encode matrix, recompute parity for every stripe, and requires redistribution under quiesce.
- Resizing the WIB region granularity (`EC_WIB_REGION_STRIPES`). This constant is fixed at compile time (1024 stripes per bit). Making this tunable per-volume would require versioning the on-disk WIB header, handling regranularization during resize, and would not solve any current pain point - 1024 stripes/bit lands well on the size/scrub-cost tradeoff curve for any volume from gigabytes to hundreds of TiB. The maximum supported volume size at the default `strip_size=64` is ~128 TiB for a 4+2 array, with larger strip sizes pushing the limit into the petabyte range; see "[WIB region granularity and sizing](#wib-region-granularity-and-sizing)".
- Changing `strip_size_kb` after volume creation. The strip size is fixed at `bdev_ec_create` time and cannot be altered for the life of the volume. Every stripe on disk is laid out and parity-encoded against the original value, and the WIB region size derives from it, so changing it would require relocating every stripe, re-encoding every parity chunk against the new chunk boundaries, and rewriting the WIB header - this means an offline rewrite of the entire volume. The `bdev_ec_resize` path preserves `strip_size_kb` (resize is in-place - growing existing chunks, not redrawing them).

## Proposal

### SPDK Proposal 1 - EC Full-Stripe write (Layered stack)

This approach places EC at the botterm of the stack and requires all writes to be aligned to full EC stripes. An FTL (Flash Translation Layer) bdev above the EC bdev absorbs unaligned writes by journaling them to a RAID 1 write cache, then flush full stripe to the EC layers.

**Pros**
- EC layer only ever sees full-stripe writes; require only the simplest possible EC implementation.
- No read-modify-write (RMW) overhead; every write touches the exact k data chunks and m parity chunks with no pre-read.
- Parity computation is alwasy for fresh full stripes, avoid stale-data risks.

**Cons**
- Deep stack: 7 layers between NVMe-oF and physical disk. Each layer adds latency and complexity.
- Capacity expansion requires RAID concat growth.
- Debugging across 7 layers could be difficult.

### SPDK Proposal 2 - EC Read-Modify-Write (4-Layered stack)

This approach moves the EC bdev to handle arbitrary I/O sizes directly using a read-modify-write (RMW) path for sub-stripe writes. SPDK's `split_on_optimal_io_boundary` splits incoming I/O at chunk boundaries, so the EC module receives at most one chunk per I/O.

For a sub-stripe write, the EC module reads all k data chunks for the target stripe, overlays the new payload in memory, re-encodes m parity chunks using ISA-L, and writes back. Full-stripe-aligned writes skip the read phase entirely.

**Pros**
- Flat stack: 4 layers. Simpler to debug, lower baseline latency.
- No upper-layer alignment constraints.
- Online expansion is simpler.
- Simpler SPDK sharding stack - no RAID concat for stitching multiple FTL address spaces together, no FTL write journal for stripe alignment. Capacity expansion is a single `bdev_ec_resize` operation rather than adding concat members.

**Cons**
- Every sub-stripe write requires reading k chunks, encoding, and writing 1+m chunks (the modified data chunk plus all parity chunks), regardless of payload size.
- Increased DMA memory pressure: each in-flight RMW holds n chunk buffers (~256 KB for 64 KB chunks, k=2, m=2).
- Only one sub-stripe write can be in flight per stripe at a time. A per-stripe dirty bitmap prevents concurrent RMW operations from corrupting each other's parity - if a second write targets the same stripe, it is requeued until the first completes. This limits write parallelism for small random writes hitting the same stripe.

### Decision

Proposal 2 is selected. The reduced stack complexity, simpler expansion path outweigh the RMW overhead.

### User Stories

#### Story 1: Reduced Storage Cost
By using EC sharding with a 4+2 configuration, the volume can achieve 2-disk fault tolerance at 1.5* overhead compared to a 3-replica volume.


#### Story 2: Online Expansion

A cluster needs to expand an existing EC volume without downtime. For the common case (same k/m, bigger disks), the cluster expands each shard's lvol and Longhorn calls `bdev_ec_resize`. The EC bdev capacity increases with no data movement and no I/O interruption.

#### Story 3: Disk Failure and Rebuild

A physical disk fails in a 2+2 EC array. The EC module detects the failure via SPDK's bdev remove event, transitions the slot to FAILED state, and continues service reads via ISA-L Reed-Solomon reconstruction from the surviving k disks. The administrator hot-swaps a replacement disk using `bdev_ec_replace_base_bdev`, then starts a background rebuild with `bdev_ec_start_rebuild`. The rebuild proceeds stripe-by-stripe while forground I/O continues normally.

### User Experience In Detail

From the downstream user/application perspective, an EC sharded volume should be seem identical to a replicaed volume. It is a standard block device exposed via NVMe-oF and mounted by a Kubernetes pod. The user configures EC parameters (k, m, stripe size) at volume creation.

Volume lifecycle operation (snapshot, clone, resize, backup, restore) should work identically. The EC layer is transparent - it sits below the lvol/blobstore layer and above physical storage.

**Kubernetes CR model changes**

The current Longhorn V2 CR hierarchy is Volume -> Engine -> Replica, where each Replica CR represents a full copy of the volume. With EC sharding there are no full replicas - data is distributed as chunks across nodes. The CR model introduces two new resource types for EC volumes:

- **Volume CR:** gains `spec.dataLayout`, a struct (`{type, mode, dataChunks, parityChunks, stripSizeKB}`) that declares the user's intended data layout and, for EC volumes, the EC parameters. `type: sharded` activates EC mode; `type: replicated` activates RAID1 mode. The entire `spec.dataLayout` struct is **immutable after creation**. The Volume controller uses `spec.dataLayout.type` as the authoritative mode selector (see `spec.dataLayout` section below).
- **Engine CR:** carries **no EC-specific spec or status fields**. For EC volumes, the engine consumes a single upstream endpoint (`ShardGroup.Status.{IP, Port, NQN}`) just as a single-replica RAID1 engine consumes one replica endpoint. The Engine controller's `CreateInstance` path passes that endpoint via the standard `replica_address_map` (one entry for EC), plus a `data_layout_type` field on `InstanceCreateRequest`/`EngineCreateRequest` (see "Engine layout dispatch" below) so the engine selects the correct upstream-RPC dispatch (`ReplicaGet` vs. `ShardGroupGet`, etc.). The engine builds a `bdev_raid1` aggregator over its upstream endpoint(s) - k+m for EC do **not** apply at the engine layer; the EC `bdev_ec_create` lives in the ShardGroup process. `EngineGet` includes a `data_layout` field in its response so the manager can verify the engine's active layout matches the Volume spec. Per-slot health (NORMAL / FAILED / REPLACING) is observable on each child Shard CR's `Status.State` and aggregated on `ShardGroup.Status` (`FailedCount`, `RebuildInProgress`, `State`); it is not duplicated onto the Engine CR, and `EngineGet` does **not** return per-slot EC fields.
- **ShardGroup CR (new):** represents the EC array configuration for one volume. Contains k, m, strip_size, engine node, aggregate health state, and references to individual Shard CRs. One ShardGroup per EC volume. The ShardGroup controller watches ShardGroup CRs and is responsible for creating/deleting child Shard CRs, coordinating rebuild orchestration, and aggregating health state.
- **Shard CR (new):** represents one base device slot in the EC array. Contains the node, disk, slot index, role (data/parity), health state (normal/failed/replacing), storage IP, and NVMe-oF port. Each EC volume has k+m Shard CRs instead of 2-3 Replica CRs. The Shard CR lifecycle (create lvol -> expose via NVMe-oF -> attach on engine node -> monitor health -> replace on failure -> rebuild) mirrors the Replica CR lifecycle but with EC-specific state tracking.

### API changes

#### SPDK JSON-RPC
New SPDK JSON-RPC methods introduced by the `bdev_ec` module:

| Methods | Description |
| --- | --- |
| `bdev_ec_create` | Create an EC bdev with specified k, m, stripe_size_kb, and base bdev names. |
| `bdev_ec_delete` | Delete an EC bdev by name. |
| `bdev_ec_get_bdevs` | List EC bdevs with configuration (k, m, strip_size) and per disk health status. |
| `bdev_ec_replace_base_bdev` | Hot-swap a failed disk with a new bdev. The new disk receives live writes immediately but is marked REPLACING until a full rebuild completes. |
| `bdev_ec_start_rebuild` | Start background rebuild of all REPLACING slots. |
| `bdev_ec_get_rebuild_progress` | Query rebuild progress ( current slot, stripe, total) |
| `bdev_ec_stop_rebuild` | Stop a running rebuild. Returns `-ENOENT` if no rebuild is in progress. The in-progress rebuild drains its current stripe I/O, then finishes with `-ECANCELED` via `ec_rebuild_finish`. |
| `bdev_ec_set_rebuild_qos` | Set rebuild rate limit in stripes/sec. `max_stripes_per_sec = 0` means unlimited (continuous rebuild at full speed). `paused = true` suspends the rebuild poller without cancelling the rebuild. Applied immediately to any in-progress rebuild. |
| `bdev_ec_resize` | In-place capacity expansion (same k/m, bigger disks). Relocate the WIB to the new disk tail and updates geometry. No data movement. |
| `bdev_ec_get_wib_status` | Query WIB state: dirty region count, regeneration, persist status. |
| `bdev_ec_get_scrub_progress` | Query startup scrub progress: current region, stripes scrubbed, regions remaining. |

The EC bdev reports its SPDK product name as "ErasureCode Volume" (set by `_ec_bdev_create`). Any consumer that identifies bdev types by product name (including `go_spdk_helper`'s `GetBdevType()` - See below) must match this exact string.

#### go-spdk-helper

`go-spdk-helper` is the Go library that wraps SPDK JSON-RPC and is consumed by `longhorn-spdk-engine`. It needs new types, a new client method per JSON-RPC, and a CLI subcommand grounp.

##### New type definition (`pkg/spdk/types/ec.go`)
| JSON-RPC | Request Message | Request Fields | Response Message | Response Fields | Example |
| --- | --- | --- | --- | --- | --- |
| `bdev_ec_create` | `BdevEcCreateRequest` | `Name string`, `DataChunks uint32` (`json:"data_chunk_count"`), `ParityChunks uint32` (`json:"parity_chunk_count"`), `StripSizeKB uint32`, `BaseBdevs []string` | (none; returns `bool`) | - | req: `{Name: "ec0", DataChunks: 4, ParityChunks: 2, StripSizeKB: 64, BaseBdevs: []string{"lvs0/s0", "lvs0/s1", "nvmf-s2n1", "nvmf-s3n1", "lvs0/s4", "nvmf-s5n1"}}` <br> resp: `true` |
| `bdev_ec_delete` | `BdevEcDeleteRequest` | `Name string` | (none; return `bool`) | - | req: `{Name: "ec0"}` <br> resp: `true` |
| `bdev_ec_get_bdevs` | `BdevEcGetBdevsRequest` | `Name string` (optional; empty returns all EC bdevs) | `[]BdevEcInfo` | `Name string`, `DataChunks uint32` (`json:"k"`), `ParityChunks uint32` (`json:"m"`), `TotalChunks uint32` (`json:"n"`), `StripSizeKB uint32`, `FailedCount uint32`, `Offline bool`, `ReplaceInProgress bool`, `RebuildInProgress bool`, `RmwInFlight uint32`, `RmwDirtyStripes uint64`, `RebuildProgress *BdevEcRebuildProgress` (present only when `RebuildInProgress` is true), `BaseBdevs []EcBaseBdev` | req: `{}` or `{Name: "ec0"}` <br> resp: `[{Name: "ec0", DataChunks: 4, ParityChunks: 2, TotalChunks: 6, StripSizeKB: 64, FailedCount: 1, Offline: false, ReplaceInProgress: true, RebuildInProgress: true, RmwInFlight: 0, RmwDirtyStripes: 0, RebuildProgress: {CurrentSlot: 3, CurrentStripe: 12480, NumStripes: 80000, StripesRebuilt: 12479}, BaseBdevs: [...6 slots...]}]` |
| `bdev_ec_replace_base_bdev` | `BdevEcReplaceBaseBdevRequest` | `Name string`, `Slot uint32`, `NewBdevName string` | `BdevEcReplaceBaseBdevResponse` | `EcName string`, `Slot uint32`, `NewBdevName string`, `State BdevEcSlotState`, `NeedsRebuild bool` | req: `{Name: "ec0", Slot: 3, NewBdevName: "nvmf-s3new-n1"}` <br> resp: `{EcName: "ec0", Slot: 3, NewBdevName: "nvmf-s3new-n1", State: "replacing", NeedsRebuild: true}` |
| `bdev_ec_start_rebuild` | `BdevEcStartRebuildRequest` | `Name string` | `BdevEcStartRebuildResponse` | `EcName string`, `NumStripes uint64`, `FirstSlot uint32` | req: `{Name: "ec0"}` <br> resp: `{EcName: "ec0", NumStripes: 80000, FirstSlot: 3}` |
| `bdev_ec_get_rebuild_progress` | `BdevEcGetRebuildProgressRequest` | `Name string` | `BdevEcRebuildProgress` | `EcName string`, `CurrentSlot uint32`, `CurrentStripe uint64`, `NumStripes uint64`, `StripesRebuilt uint64`, `SlotsToRebuild uint32`, `PercentComplete uint32` | req: `{Name: "ec0"}` <br> resp: `{EcName: "ec0", CurrentSlot: 3, CurrentStripe: 12480, NumStripes: 80000, StripesRebuilt: 12479, SlotsToRebuild: 1, PercentComplete: 15}` |
| `bdev_ec_stop_rebuild` | `BdevEcStopRebuildRequest` | `Name string` | (none; returns `bool`) | - | req: `{Name: "ec0"}` <br> resp: `true` (returns `-ENOENT` error if no rebuild is in progress) |
| `bdev_ec_set_rebuild_qos` | `BdevEcSetRebuildQosRequest` | `Name string`, `MaxStripesPerSec uint32`, `Paused bool` | (none; returns `bool`) | - | req: `{Name: "ec0", MaxStripesPerSec: 5000, Paused: false}` <br> resp: `true` |
| `bdev_ec_resize` | `BdevEcResizeRequest` | `Name string` | `BdevEcResizeResponse` | `EcName string`, `OldBlockcnt uint64`, `NewBlockcnt uint64` | req: `{Name: "ec0"}` <br> resp: `{EcName: "ec0", OldBlockcnt: 1024000, NewBlockcnt: 2048000}` |
| `bdev_ec_get_wib_status` | `BdevEcGetWibStatusRequest` | `Name string` (`json:"ec_name"`) | `BdevEcWibStatus` | `EcName string`, `NumRegions uint32`, `DirtyRegions uint32`, `Generation uint32`, `PersistPending bool` | req wire: `{"ec_name": "ec0"}` <br> resp: `{EcName: "ec0", NumRegions: 320, DirtyRegions: 0, Generation: 47, PersistPending: false}` |
| `bdev_ec_get_scrub_progress` | `BdevEcGetScrubProgressRequest` | `Name string` (`json:"ec_name"`) | `BdevEcScrubProgress` | `EcName string`, `CurrentRegion uint32`, `NumRegions uint32`, `TotalDirtyRegions uint32`, `CurrentStripe uint64`, `StripesScrubbed uint64`, `RegionsScrubbed uint64`, `PercentComplete uint32` | req wire: `{"ec_name": "ec0"}` <br> resp: `{EcName: "ec0", CurrentRegion: 12, NumRegions: 320, TotalDirtyRegions: 18, CurrentStripe: 12544, StripesScrubbed: 11520, RegionsScrubbed: 11, PercentComplete: 61}` |

**Wire-key note:** In SPDK, the JSON field used to identify an EC bdev varies by RPC category (`module/bdev/ec/bdev_ec_rpc.c`).
    - **Lifecycle operation:** `bdev_ec_create`, `bdev_ec_delete` - use `"name"`.
    - **EC-specific operations:** such as `bdev_ec_replace_base_bdev`, `bdev_ec_start_rebuild`, `bdev_ec_stop_rebuild`, `bdev_ec_get_rebuild_progress`, `bdev_ec_set_rebuild_qos`, `bdev_ec_resize`, `bdev_ec_get_wib_status`, `bdev_ec_get_scrub_progress` - use `"ec_name"`.

The `go-spdk-helper` request structs reflects this split by using `json:"name"` for create/delete and `json: "ec_name"` for all other EC RPCs. All response consistently use `"ec_name"` to return the bdev identifier.

See *Naming principle (request-key split)* below for rationale.

Note: `EcBaseBdev` is a nested type within `BdevEcInfo.BaseBdevs`. Its fields are `Name string`, `Slot uint32`, `Role BdevEcSlotRole`, `State BdevEcSlotState`, and `NeedsRebuild bool` (present only when `State` is `"replacing"`).
Example: `{Name: "nvmf-s3n1", Slot: 3, Role: "parity", State: "replacing", NeedsRebuild: true}`.

Note: `BdevEcReplaceBaseBdevResponse` returns the slot state immediately after a hot-swap, removing the need for a follow-up `bdev_ec_get_bdevs` call. Fields include `EcName string`, `Slot uint32`, `NewBdevName string`, `State BdevEcSlotState` (always `"replacing"` on success), and `NeedsRebuild bool` (always `true` on success, since the slot has been installed but not yet rebuilt).

Note: The `RebuildProgress` field in `BdevEcInfo` reuses `BdevEcRebuildProgress`, but only partially populates it: `CurrentSlot`, `CurrentStripe`, `NumStripes`, and `StripesRebuilt`. `SlotToRebuild` and `PercentComplete are zero` - use `bdev_ec_get_rebuild_progress` for the full view. When `RebuildInProgress` is true, `go-spdk-helper` sets `RebuildState` to `"running"` in `BdevEcGetBdevs`, which is the only case where inline `RebuildProgress` appears.

**Enum types**
| Type | Value | Where used |
| --- | --- | --- |
| `BdevEcState` | `"online"`, `"degraded"`, `"offline"` | Derived by `go-spdk-helper` from `FailedCount` and `Offline` fields (not returned directly by the SPDK JSON-RPC - the code returns `failed_count` and `offline` as separate fields; the Go layer computes: `offline` -> `"offline"`, `failed_count > 0` -> `"degraded"`, else -> `"online"`) |
| `BdevEcSlotState` | `"normal"`, `"failed"`, `"replacing"` | `EcBaseBdev.State` |

Note: Thd SPDK EC module defines three slot states: `normal`, `failed`, and `replacing`. The Shard CR uses the same set - there is no per-shard `"rebuilding"` state. SPDK `"replacing"` always maps directly to Shard CR `"replacing"`, regardless of rebuild activity.

Rebuild status is tracked at the ShardGroup level:
    - `ShardGroup.Status.RebuildInProgress == true` means an active rebuilt.
    - `syncStatus` sets `ShardGroup.Status.State = rebuilding` to surface this to controllers and volume robustness logic.

| `BdevEcSlotRole` | `"data"`, `"parity"` | `EcBaseBdev.Role` |
| `BdevEcRebuildState` | `"idle"`, `"running"`, `"done"`, `"error"` | Derived by `go-spdk-helper` from `bdev_ec_get_rebuild_progress` response context: SPDK returns `-ENOENT` when no rebuild is active -> `"idle"`; `percent_complete == 100` -> `"done"`; any other non-`ENOENT` RPC error -> `"error"`; otherwise -> `"running"`. **This enum is never serialized in any gRPC message field** — it exists in `spdkrpc/ec.proto` solely to generate the Go type constant for use inside go-spdk-helper's JSON-RPC interpretation logic. Do not add it to any proto message. |

Note: **Wire-level chunk-count keys**: SPDK uses different field names for chunk counts on request vs response:
    - Request (`bdev_ec_create`): `"data_chunk_count"`, `"parity_chunk_count"`
    - Response (`bdev_ec_get_bdevs`): `"k"`, `"m"`, `"n"`
This asymmetry is preserved in `go-spdk-helper` via struct tags:
    - `BdevEcCreateRequest` -> `json: "data_chunk_count"`
    - `BdevEcInfo` -> `json:"k"`, `json:"m"`, `json:`

Note: **Naming principle (request-key split)**: The `"name"` vs `"ec_name"` split following SPDK conventions:
    - **Lifecycle operation** use `"name"` to align with the generic SPDK bdev pattern (`bdev_<type>_create/delete`).
    - **Type-specific operation** uses `"ec_name"` to explicityly target an existing EC bdev and enforce type correctness at the API boundary.
This behavior originate from SPDK, and `go-spdk-helper` mirrors it.

Note: **Layering principle (Go-side naming)**: Go field names follow Longhorn conventions, independent of SPDK's wire format:
    - Go uses: `DataChunks`, `ParityChunks`, `TotalChunks`
    - Matching Longhorn CRD and proto naming (`dataChunks`, `parityChunks`, `data_chunks`, `parity_chunks`)
    - SPDK used: `k`, `m`, `n` (Reed-Solomon terminology)
This is a deliberate translation layer:
    - The Go API exposes only Longhorn terminology
    - SPDK-specific naming is contained within JSON struct tags.

**Chunk-count mapping across layers**
| Surface | Spelling |
| --- | --- |
| Go struct field | `DataChunks`, `ParityChunks`, `TotalChunks` |
| Go function parameter | `dataChunks`, `parityChunks` (lowerCamelCase of the struct field) |
| JSON tag — request wire | `data_chunk_count`, `parity_chunk_count` |
| JSON tag — response wire | `k`, `m`, `n` |

Function parameters follow standard Go convention (lowerCamelCase) and track struct field names - not wire keys - keeping the API consistent regardless of SPDK's encoding difference.

**Extend existing types** (`pkg/spdk/types/bdev.go`)
- Add `BdevProductNameEc = "ErasureCode Volume"` - this must exactly match the `product_name` set in SPDK's `_ec_bdev_create`. `GetBdevType()` relies on string matching against `product_name`; any mismatch will cause EC bdevs to be classified as `unknown`, and disabling all EC-specific code paths.
- Add `BdevTypeEc = "ec"`
- Add and `Ec *BdevEcInfo` field to `BdevDriverSpecific`, consistent with existing fields such as `Lvol`, `Raid`, `Nvme`, etc.
- Extend `GetBdevType()` to recognize EC bdevs based on the product name.

##### New client methods (`pkg/spdk/client/ec.go`)

| Client Method | JSON-RPC Method | Return Type |
| --- | --- | --- |
| `BdevEcCreate(name string, dataChunks, parityChunks, stripSizeKB uint32, baseBdevs []string)` | `bdev_ec_create` | `string` (bdev name; SPDK returns `true` on success, Go layer returns the `name` parameter for caller convenience) |
| `BdevEcDelete(name string)` | `bdev_ec_delete` | `bool` |
| `BdevEcGetBdevs(name string)` | `bdev_ec_get_bdevs` | `[]BdevEcInfo` |
| `BdevEcReplaceBaseBdev(name string, slot uint32, newBdevName string)` | `bdev_ec_replace_base_bdev` | `BdevEcReplaceBaseBdevResponse` |
| `BdevEcStartRebuild(name string)` | `bdev_ec_start_rebuild` | `BdevEcStartRebuildResponse` |
| `BdevEcGetRebuildProgress(name string)` | `bdev_ec_get_rebuild_progress` | `BdevEcRebuildProgress` |
| `BdevEcStopRebuild(name string)` | `bdev_ec_stop_rebuild` | `bool` |
| `BdevEcSetRebuildQos(name string, maxStripesPerSec uint32, paused bool)` | `bdev_ec_set_rebuild_qos` | `bool` |
| `BdevEcResize(name string)` | `bdev_ec_resize` | `BdevEcResizeResponse` |
| `BdevEcGetWibStatus(name string)` | `bdev_ec_get_wib_status` | `BdevEcWibStatus` |
| `BdevEcGetScrubProgress(name string)` | `bdev_ec_get_scrub_progress` | `BdevEcScrubProgress` |

Note: `BdevLvolGrowLvstore(lvsName, uuid string)` wrapping `bdev_lvol_grow_lvstore` is a general lvol store method (not EC-specific) added to `pkg/spdk/client/basic.go` with its request type `BdevLvolGrowLvstoreRequest` in `pkg/spdk/types/lvol.go`. For EC volumes it is called by `ShardGroupExpand` (in the ShardGroup process, on the same SPDK process that owns the lvstore) after `bdev_ec_resize`, to grow the lvol store to the new EC bdev capacity. The engine's `EngineExpand` runs separately to grow the upstream raid1 aggregator. A corresponding `grow` subcommand is added to the `bdev-lvstore` CLI group (`app/cmd/basic/bdev_lvstore.go`), accepting `--lvs-name` or `--uuid`.

##### New CLI commands (`app/cmd/basic/bdev_ec.go`)
A `BdevEcCmd()` function exposing subcommands registered in `main.go`. These are primarily developer-facing for manual testing and field debugging.
- `create`
- `delete`
- `get`
- `replace`
- `rebuild-start`
- `rebuild-stop`
- `rebuild-progress`
- `rebuild-qos-set`
- `resize`
- `wib-status`
- `scrub-progress`

##### longhorn spdk-engine gRPC (`spdkrpc.SPDKService`)

The existing `Replica*` and `Engine*` methods remain available for V2 replicated volumes. EC volumes add two new method families to the same service. The `Engine*` methods are **EC-agnostic** - the engine for an EC volume looks structurally identical to a single-replica RAID1 engine (consume one upstream NVMe-oF endpoint, build raid1, expose). All EC-specific stack construction lives in the new `ShardGroup*` methods.

- `Shard*` methods - parallel to the existing `Replica*` methods. Called on shard-hosting nodes to manage per-slot lvol lifecycle.
- `ShardGroup*` methods - parallel to the existing `Replica*` methods at the lvstore-owning layer, but the backing bdev is `bdev_ec` instead of `aio`. Called on the ShardGroup-process-hosting node (typically the engine node) to manage the bdev_ec + lvstore + head lvol + NVMe-oF expose lifecycle, plus shard-replace and rebuild orchestration.
- `Engine*` methods: **no EC-specific behavior**. The same code path serves both topologies.

**New `Shard*` methods**

| gRPC Method | Input | Return | JSON-RPC Call(s) | Purpose |
| --- | --- | --- | --- | --- |
| `ShardCreate` | `volume_name`, `slot_index`, `size_bytes`, `lvs_name`, `lvs_uuid`, `port_count` | `shard_id`, `uuid`, `lvs_name`, `lvs_uuid`, `bdev_name`, `nvmf_subsystem_nqn`, `state`, `ip`, `port` | `bdev_lvol_create` + NVMe-oF export | Create one shard lvol per slot. `ip` and `port` carry the NVMe-oF transport address of the exported subsystem. The shard controller reads these from `InstanceResponse.Status.PortStart` and the IM pod's storage network IP to construct the `EcShardInfo.Address` (`ip:port`) passed to `ShardGroupCreate` - `connectNVMfBdev` inside the ShardGroup process calls `net.SplitHostPort(address)` so the address must be in `ip:port` format, not the NQN string. **`spdkrpc.Shard` must include `ip string` and `port int32` fields**; without them the port cannot cross the SPDK gRPC boundary and every shard address is permanently `storageIP:0`. Called k+m times during EC volume creation. The `role` parameter is intentionally omitted - the shard node creates a raw lvol + NVMe-oF export that behaves identically regardless of role. Role is derived inside the ShardGroup process from `slot_index < k` when constructing the EC bdev. All k+m shards must be the same size, which includes the WIB reservation (`shard_size + 2 × strip_size`). The EC module's `ec_compute_geometry` uniformly subtracts `2 × strip_size` from the per-disk capacity to reserve space for the WIB (the WIB data itself is only stored on parity disks, but the geometry calculation uses the minimum disk size across all slots, so all shards must be identically sized). The ShardGroup controller computes this inflated size and passes it to all k+m `ShardCreate` calls. The shard node does not distinguish data vs parity - it creates an lvol of exactly `size_bytes`. |
| `ShardDelete` | `shard_id` | `ok` | NVMe-oF teardown + `bdev_lvol_delete` | Delete one shard lvol. Called k+m times during EC volume teardown. |
| `ShardGet` | `shard_id` | shard state, bdev info | - | Get status of a single shard on this node. |
| `ShardList` | - | All shards on node | - | List all shards managed by this node. |
| `ShardExpand` | `shard_id`, `new_size` | `ok` | `bdev_lvol_resize` | Resize a shard's lvol for volume expansion. |
| `ShardWatch` | - | stream of state change | - | Watch shard state changes on this node. The primary consumer is the instance-manager's `watchSPDKShard` goroutine, which forwards shard lifecycle events (e.g., lvol creation, deletion, NVMe-oF export failure) to the `InstanceWatch` notification channel. Note: authoritative EC slot state (NORMAL/FAILED/REPLACING) lives in the **ShardGroup process** and is read via `ShardGroupGet`; `ShardWatch` reports local shard-node events only (lvol health, disk I/O errors). The ShardGroup controller uses both: `ShardGroupGet` for slot-level status and `InstanceWatch` (shard events) for shard-level lifecycle. |

**`Engine*` methods**

The Engine methods have the same gRPC signatures for both RAID1 and EC. For an EC volume the engine consumes a single upstream endpoint (the ShardGroup process's exposed lvol), structurally identical to a single-replica RAID1 engine **at the bdev layer** - both topologies attach via NVMe-oF and aggregate via `bdev_raid1`. Where they differ is the **upstream-RPC surface**: RAID1 engines query `ReplicaGet`/`ReplicaExpand`/`ReplicaSnapshot*` on each upstream; EC engines query `ShardGroupGet` and skip per-upstream expand and per-upstream snapshot RPCs (those are driven externally - see "Engine layout dispatch" below). `EngineCreate` carries a `data_layout_type` field that selects which dispatch path the engine uses. **The `EngineDelete` path does not consult `data_layout_type` and remains layout-blind by construction** (see "Engine layout dispatch").

| gRPC Method | Input | Return | JSON-RPC Call(s) | Purpose |
| --- | --- | --- | --- | --- |
| `EngineCreate` | `engine_name`, `replica_address_map` (single entry for EC pointing at `ShardGroup.Status.{IP, Port}` formatted as `ip:port`, keyed by **volume name** — the same value as the ShardGroup CR name and the Shard CR external-name prefix; one entry per replica name for RAID1), `data_layout_type` (`REPLICATED` for RAID1, `SHARDED` for EC), frontend params | `engine_id`, `frontend_endpoint` | `bdev_nvme_attach_controller` (one per upstream endpoint) + `bdev_raid_create` (level 1) + frontend setup | Build the engine stack: NVMe-attach to each upstream endpoint, aggregate via raid1, expose via NVMe-oF. For EC volumes the map has exactly one entry; for RAID1 it has N entries. `data_layout_type` selects the upstream-RPC dispatch (RAID1: `replicaUpstream`; EC: `shardGroupUpstream` - see "Engine layout dispatch"). **Failure behavior:** All upstream endpoints must be reachable; any single NVMe-connect failure is fatal and `EngineCreate` returns an error. **Crash recovery dispatch:** `EngineCreate` with `salvage_requested=true` is RAID1-only (it filters replicas for salvageable data). EC salvage happens at the ShardGroup process layer (via `ShardGroupCreate` with `salvage_requested=true`, which dispatches to `RecreateEC` semantics inside the ShardGroup process), not at the engine layer. |
| `EngineDelete` | `engine_name`, `cleanup_required` | `ok` | Frontend unexport + `bdev_raid_delete` + `bdev_nvme_detach_controller` (per upstream endpoint) | Tear down engine stack. **No `bdev_lvol_delete`, no `bdev_lvol_delete_lvstore`, no `bdev_ec_delete` calls** - the engine owns no persistent state. `cleanup_required` is plumbed for symmetry with replica/shardgroup teardown but does not affect engine teardown's calls (the engine has nothing to gate). |
| `EngineGet` | `engine_name` | `engine_id`, `current_state`, `frontend_endpoint`, `data_layout` (type only - per-slot EC health is at `ShardGroupGet`) | (engine-local only) | Return engine state. **Per-slot EC health (slots[], wib_status, scrub_progress, rebuild_progress) is no longer returned by `EngineGet`** - those fields move to `ShardGroupGet`. The control plane reads engine state from `EngineGet` and EC-specific state from `ShardGroupGet`. |
| `EngineList` | unchanged | unchanged | - | List all engines |
| `EngineWatch` | unchanged | unchanged | - | Engine state change events; EC-slot health events flow through `ShardGroupWatch`. |
| `EngineExpand` | `engine_name`, `new_size` | `ok` | (none — auto-resize via NVMe AEN chain) | Engine-side raid1 grow is **automatic via SPDK's `BDEV_EVENT_RESIZE` chain**: when the upstream lvol resizes, the NVMe-oF target fires an Asynchronous Event Notification (`AER_NS_ATTR_CHANGED`); the engine's `bdev_nvme` initiator detects the new namespace size and calls `spdk_bdev_notify_blockcnt_change`; the raid module's `raid1_resize` callback ([raid1.c:504](https://github.com/spdk/spdk/blob/master/module/bdev/raid/raid1.c#L504)) fires automatically and grows the raid bdev (no SPDK RPC is involved). `EngineExpand` therefore performs no SPDK call - it polls the engine's raid bdev `blockcnt` until it matches `new_size`, returns `ok`. For EC volumes this is the final step after `ShardExpand` (each shard) and `ShardGroupExpand` (bdev_ec + lvstore + head lvol grow); for RAID1 the same auto-resize chain runs once each replica resizes. |
| `EngineExpandPrecheck` | `engine_name`, `new_size` | `expansion_required` (bool) | (engine-local only) | Engine-local pre-validate (no other expansion in progress). |
| `EngineFrontendSwitchOver` | unchanged | unchanged | - | Live migration works through existing frontend switchover, identical for both topologies. |
| `EngineSnapshotCreate`/`Delete`/`Revert`/`Purge`/`Hash`/`HashStatus`/`Clone` | unchanged | unchanged | (delegates to ShardGroupSnapshot* for EC volumes; ReplicaSnapshot* for RAID1) | API signature is unchanged. For EC engines, snapshot operations forward to `ShardGroupSnapshot*` (the lvstore lives in the ShardGroup process). For RAID1 they continue to forward to per-replica snapshot RPCs. `SnapshotHash`/`HashStatus`/`Clone` return `Unimplemented` for EC in the initial release. |
| `EngineBackupCreate`/`BackupStatus`/`BackupRestore`/`RestoreStatus` | unchanged | unchanged | - | **All backup/restore operations return `Unimplemented` for EC engines in the initial release.** Backup/restore is a future addition that will operate on the ShardGroup process's lvol. |

**New `ShardGroup*` methods**

The ShardGroup methods own the bdev_ec + lvstore + head lvol + NVMe-oF expose lifecycle for EC volumes, plus shard-replace and rebuild orchestration. They are called on the InstanceManager hosting the ShardGroup process (typically the engine node).

| gRPC Method | Input | Return | JSON-RPC Call(s) | Purpose |
| --- | --- | --- | --- | --- |
| `ShardGroupCreate` | `volume_name`, `data_chunks`, `parity_chunks`, `strip_size_kb`, `shards map<string, EcShardInfo{address (ip:port), slot_index}>`, `port_count`, `salvage_requested` (bool) | `shardgroup_id`, `lvs_uuid`, `head_lvol_uuid`, `ip`, `port`, `nvmf_subsystem_nqn` | `bdev_nvme_attach_controller` ×(k+m) + `bdev_ec_create` + `bdev_lvol_create_lvstore` + `bdev_lvol_create` + NVMe-oF export | Build the EC backing stack and expose the head lvol. The shard map key is the Shard CR external name; the engine derives the NVMe controller name internally via `GetShardName(volumeName, slotIndex)`. **Dispatch:** With `salvage_requested=false`, calls `CreateEC` (fresh lvstore + head lvol). With `salvage_requested=true`, calls `RecreateEC` semantics: tolerates missing shard connections by passing `""` for each unreachable slot, and skips lvstore/head-lvol creation - SPDK's `bdev_examine` discovers the existing lvstore from the encoded blocks. Used for ShardGroup process restart on engine-node failover. |
| `ShardGroupDelete` | `shardgroup_id`, `cleanup_required` (bool) | `ok` | NVMe-oF unexport + `bdev_lvol_delete` (head lvol) + `bdev_lvol_delete_lvstore` + `bdev_ec_delete` + `bdev_nvme_detach_controller` ×(k+m) | Tear down the EC backing stack. **The `cleanup_required` flag is the close-vs-delete distinction.** `cleanup_required=false` (volume detach): unexpose NVMe-oF and disconnect from shards but **do not** call `bdev_lvol_delete` or `bdev_lvol_delete_lvstore` - the lvstore + head lvol are preserved on the encoded blocks. `cleanup_required=true` (volume delete): perform full destruction including lvol/lvstore deletes. This mirrors `ReplicaDelete`'s `cleanup_required` semantics exactly. |
| `ShardGroupGet` | `shardgroup_id` | `shardgroup_id`, `state`, `process_state`, `ip`, `port`, `nvmf_subsystem_nqn`, `lvs_uuid`, `head_lvol_uuid`, `spec_size`, `actual_size`, `snapshots` (`map<string, Lvol>` keyed by snapshot lvol name; carries the parent/child chain), `ec_status` (`EcStatus{state, slots[], wib_status, scrub_progress, rebuild_in_progress, rebuild_progress}`) | `bdev_ec_get_bdevs` + `bdev_ec_get_wib_status` + `bdev_ec_get_scrub_progress` + `bdev_get_bdevs` (filtered for the EC lvstore's lvols) | Return ShardGroup process state, sizing, lineage, and EC health. Replaces the per-slot EC health fields previously returned by `EngineGet` for EC volumes. The engine's `shardGroupUpstream` reads `spec_size` / `actual_size` / `snapshots` from this response so `EngineGet` and `Volume.Status.ActualSize` keep working without engine-side EC awareness. |
| `ShardGroupList` | - | All ShardGroup processes on node | - | List ShardGroup processes managed by this InstanceManager. |
| `ShardGroupWatch` | - | stream of state changes | - | Watch ShardGroup process and EC slot state changes. |
| `ShardGroupExpand` | `shardgroup_id`, `new_size` | `ok` | `bdev_ec_resize` + `bdev_lvol_grow_lvstore` + `bdev_lvol_resize` (head lvol) | Grow the EC stack after each shard's lvol has been expanded via `ShardExpand`. The engine subsequently calls `EngineExpand` to grow its raid1 view. |
| `ShardGroupExpandPrecheck` | `shardgroup_id`, `new_size` | `expansion_required` (bool) | (process-local only) | Pre-validate ShardGroup-side expansion preconditions: no rebuild, scrub, or resize in progress; all slots NORMAL. Does **not** validate shard-node free space - the ShardGroup controller calls `ShardExpand` on each shard node directly; if a shard lacks space, the call fails and is retried. |
| `ShardGroupShardReplace` | `shardgroup_id`, `shard_name`, `shard_address` (`ip:port`) | `ok`, `slot_state` (REPLACING) | `bdev_nvme_attach_controller` + `bdev_ec_replace_base_bdev` | Swap the backing bdev for an existing FAILED slot. The ShardGroup process looks up the slot by `shard_name` in its in-memory map, NVMe-connects to `shard_address`, then calls `bdev_ec_replace_base_bdev`. The slot must be in FAILED state; the slot transitions FAILED -> REPLACING and the new bdev immediately starts receiving foreground writes. First step of a two-step rebuild; second step is `ShardGroupShardRebuildStart`. (Same semantics as the previous `EngineShardReplace` - just relocated to the ShardGroup process.) |
| `ShardGroupShardRebuildStart` | `shardgroup_id` | `ok`, `ec_name`, `num_stripes`, `first_slot` | `bdev_ec_start_rebuild` | Start background rebuild of all REPLACING slots. (Same semantics as `EngineShardRebuildStart`, relocated.) |
| `ShardGroupShardRebuildProgress` | `shardgroup_id` | rebuild progress fields | `bdev_ec_get_rebuild_progress` | Poll EC rebuild status. (Same as `EngineShardRebuildProgress`, relocated.) |
| `ShardGroupShardRebuildStop` | `shardgroup_id` | `ok` | `bdev_ec_stop_rebuild` | Stop a running rebuild. (Same as `EngineShardRebuildStop`, relocated.) |
| `ShardGroupShardRebuildQosSet` | `shardgroup_id`, `max_stripes_per_sec`, `paused` | `ok` | `bdev_ec_set_rebuild_qos` | Set rebuild rate limit. (Same as `EngineShardRebuildQosSet`, relocated.) |
| `ShardGroupShardForceFail` | `shardgroup_id`, `shard_name` | `ok`, `slot_state` (FAILED) | `bdev_nvme_detach_controller` | Force a NORMAL slot to transition to FAILED immediately, without waiting for the NVMe-oF connection to time out. The ShardGroup process detaches the NVMe controller backing the named shard, which fires `SPDK_BDEV_EVENT_REMOVE` on the slot's local bdev and drives `ec_slot_set_failed`. Used by the ShardGroup controller on **intentional** Shard CR deletion (admin-triggered, eviction, drain) to bypass the `ctrlr_loss_timeout_sec` wait that would otherwise gate the subsequent `ShardGroupShardReplace`. **Idempotency / safety:** if the slot is already FAILED, returns success. If the slot is REPLACING, returns `FailedPrecondition` (a rebuild is already underway and force-fail would corrupt it). If the slot is NORMAL but the named shard is not the one currently bound to that slot (post-replace), returns `FailedPrecondition`. The RPC is intentionally narrow - it touches only the connection layer and never the bdev_ec module's failure counters directly; failure accounting flows through the standard `BDEV_EVENT_REMOVE` path so dirty-region bookkeeping and degraded-mode gating remain correct. |
| `ShardGroupSnapshotCreate`/`Delete`/`Revert`/`Purge` | `shardgroup_id`, snapshot params | snapshot result | `bdev_lvol_snapshot`, `bdev_lvol_delete`, `bdev_lvol_clone` | Snapshot operations on the head lvol. Forwarded to here by `EngineSnapshot*` for EC volumes. |

**Rebuild sequence**
EC rebuild is a two-step operation. The control plane calls (against the ShardGroup process, not the engine):
1. `ShardGroupShardReplace(shardgroup_id, shard_name=<name>, shard_address=<ip:port>)` - hot-swap. The ShardGroup process looks up the slot by `shard_name` in its in-memory map, uses `GetShardName(volumeName, slotIndex)` as the NVMe controller name, NVMe-connects to `shard_address`, then calls `bdev_ec_replace_base_bdev`. The slot transitions FAILED -> REPLACING. The new bdev immediately receives foreground writes but is not yet used for reads; its pre-failure content is still missing.
1. `ShardGroupShardRebuildStart(shardgroup_id)` - kick off the background rebuild poller that reads k chunks from NORMAL disks, reconstructs each stripe's missing chunk via ISA-L, and writes to all REPLACING slots. This wraps `bdev_ec_start_rebuild`.

Progress is polled via `ShardGroupShardRebuildProgress`. On completion the slot transitions REPLACING -> NORMAL and `failed_count` decrements.

The engine is not involved in shard rebuild - it sees only the ShardGroup endpoint, which continues to serve I/O throughout. From the engine's perspective, an EC rebuild is invisible (just like a RAID1 single-replica's internal operations are invisible to whatever consumes its lvol).

**JSON-RPC coverage**
Every `bdev_ec_*` JSON-RPC is reachable via exactly one gRPC method on the ShardGroup process:
- `bdev_nvme_attach_controller` × (k+m) - called internally by `ShardGroupCreate` (once per slot) and by `ShardGroupShardReplace` (once for the replacement slot).
- `bdev_nvme_detach_controller` - called internally by `ShardGroupDelete` (for all slots) and by `ShardGroupShardReplace` (for the old failed slot's bdev).
- `bdev_ec_create`, `bdev_ec_delete` - wrapped by `ShardGroupCreate`, `ShardGroupDelete`.
- `bdev_ec_get_bdevs`, `bdev_ec_get_wib_status`, `bdev_ec_get_scrub_progress` - combined into `ShardGroupGet`.
- `bdev_ec_replace_base_bdev` - wrapped by `ShardGroupShardReplace`.
- `bdev_ec_start_rebuild`, `bdev_ec_get_rebuild_progress` - wrapped by `ShardGroupShardRebuildStart`, `ShardGroupShardRebuildProgress`.
- `bdev_ec_stop_rebuild` - wrapped by `ShardGroupShardRebuildStop`.
- `bdev_ec_set_rebuild_qos` - wrapped by `ShardGroupShardRebuildQosSet`.
- `bdev_nvme_detach_controller` (single-slot, fast-fail variant) - wrapped by `ShardGroupShardForceFail`. The same JSON-RPC is also called internally by `ShardGroupDelete` (all slots) and `ShardGroupShardReplace` (the failed slot's bdev), but those paths run after the slot is already FAILED. `ShardGroupShardForceFail` is the one entry point that uses detach to **drive** the slot from NORMAL to FAILED.
- `bdev_ec_resize` - wrapped by `ShardGroupExpand`.
- `bdev_lvol_create_lvstore`, `bdev_lvol_create`, `bdev_lvol_delete`, `bdev_lvol_delete_lvstore` - wrapped by `ShardGroupCreate`/`ShardGroupDelete` (with the `cleanup_required` flag gating the deletes).
- `bdev_lvol_snapshot`, `bdev_lvol_clone` - wrapped by `ShardGroupSnapshot*`.

**Naming convention**
The `ShardGroupShard*` family (`ShardGroupShardReplace`, `ShardGroupShardRebuildStart`, etc.) operates on a slot within an EC group rather than adding a new replica. `ShardGroupShardReplace` does not add anything: EC slot count is frozen at `bdev_ec_create` time, so the slot was already in FAILED state and this RPC only swaps which physical bdev sits in that existing slot. This is structurally different from RAID1's `EngineReplicaAdd` (which can grow replica count from N to N+1).

**Slot-index resolution**
The control plane passes the slot index for each shard via the `EcShardInfo.slot_index` field in `ShardGroupCreate`, where `shards` is a `map<string, EcShardInfo>` keyed by the Shard CR external name (`<volumeName>-<slotIndex>`, e.g. `vol0-3`). The ShardGroup process stores these in its in-memory shard map (keyed by Shard CR name). It derives the NVMe controller name internally from `GetShardName(volumeName, slotIndex)`. Role is derived - not passed - since it is a pure function of `(slot_index, k)`: indices 0..k-1 are DATA, k..k+m-1 are PARITY. For `ShardGroupShardReplace`, the process looks up the slot by `shard_name` in the existing map; the replacement shard inherits the same slot index as the original.

**Connection ownership**
Both `ShardGroupCreate` and `ShardGroupShardReplace` accept `ip:port` transport addresses (not pre-connected local bdev names), so the **ShardGroup process owns the full NVMe-TCP connection lifecycle for EC shards** - the same ownership pattern that the engine has for replicas in RAID1 mode. The Longhorn manager provides addresses; the SPDK process performs the connect/disconnect. The engine, in turn, owns the NVMe-TCP connection from itself to the ShardGroup process - one connection per upstream endpoint, exactly as for a single-replica RAID1 engine. Each layer owns its outgoing connections; no cross-layer pre-connection passing is required.


##### longhorn-instance-manager changes

The Instance Service (`pkg/instance/instance.go`) dispatches volume lifecycle operations (Create, Delete, Get, List, Watch) through the `InstanceOps` interface, which has V1 and V2 implementations. For V2, `V2DataEngineInstanceOps` dispatches `InstanceCreate` by instance type (`engine`, `replica`, `engine-frontend`). EC sharding requires extending this layer to route both Shard and ShardGroup operations through the Instance Service.

**New instance types:** Add two constants to `pkg/types/types.go` alongside the existing `InstanceTypeEngine`, `InstanceTypeReplica`, and `InstanceTypeEngineFrontend`:
- `InstanceTypeShard = "shard"` - per-slot lvol on a shard node
- `InstanceTypeShardGroup = "shardgroup"` - bdev_ec + lvstore + head lvol + NVMe-oF expose, on the engine node

**InstanceCreate dispatch:** `V2DataEngineInstanceOps.InstanceCreate` gains two new cases in its type switch:
- `types.InstanceTypeShard` -> calls `c.ShardCreate(...)` on the SPDK client, passing volume name, slot index, size, `lvs_name`, `lvs_uuid` from the `SpdkInstanceSpec`, and `port_count` from `InstanceSpec.port_count`. The SPDK server uses `lvs_name`/`lvs_uuid` directly (via `isLvsExist`) to verify the target lvstore exists - the same pattern as replica creation. The `Shard` response proto identifies the shard by `lvs_name`/`lvs_uuid` (matching the `Replica` message), since the shard is identified by its lvstore, not a disk ID.
- `types.InstanceTypeShardGroup` -> calls `c.ShardGroupCreate(...)` on the SPDK client, passing the `ShardGroupSpec` (`data_chunks`, `parity_chunks`, `strip_size_kb`, `shards` map, `salvage_requested`) from `SpdkInstanceSpec`, plus `port_count` from `InstanceSpec.port_count`.

**Engine create simplification:** The engine case no longer branches on `ec_spec`. For both EC and RAID1 volumes, the engine receives `replica_address_map` (one entry for EC pointing at the ShardGroup endpoint, N entries for RAID1) and follows the same SPDK setup sequence: NVMe-attach to each entry, build raid1, expose. There is no `ec_spec` field on `SpdkInstanceSpec` for engine instances; EC parameters live on `SpdkInstanceSpec` for ShardGroup instances only. Layout discrimination for upstream-RPC dispatch is carried out-of-band of `SpdkInstanceSpec` via `data_layout_type` on `InstanceCreateRequest` (and forwarded to `EngineCreateRequest`) - see "Engine layout dispatch".

**InstanceDelete dispatch:** `V2DataEngineInstanceOps.InstanceDelete` gains two new cases:
- `types.InstanceTypeShard` -> calls `c.ShardDelete(...)` only when `req.CleanupRequired == true`. When `cleanupRequired=false` (detach/stop), the delete is skipped entirely - the lvol persists on disk for later reattach. Mirrors `ReplicaDelete`.
- `types.InstanceTypeShardGroup` -> calls `c.ShardGroupDelete(req.Name, req.CleanupRequired)`. The `cleanup_required` flag is passed through to the SPDK service, which gates `bdev_lvol_delete` + `bdev_lvol_delete_lvstore` (cleanup=true) versus skipping those calls (cleanup=false, the detach path). This is the **central mechanism** that prevents the EC volume detach data-loss bug: the longhorn-manager sets `cleanup_required = (cr.DeletionTimestamp != nil)` exactly as it does for `ReplicaDelete`, so detach passes `cleanup_required=false` and the lvstore + head lvol are preserved.

**InstanceGet/InstanceList:** `V2DataEngineInstanceOps.InstanceGet` and `InstanceList` include both shard and shardgroup instances. `InstanceList` calls `c.ShardList()` and `c.ShardGroupList()` and merges them into the response alongside engine, replica, and engine-frontend instances.

**InstanceWatch - new `watchSPDKShard` and `watchSPDKShardGroup` goroutines:** `InstanceWatch` currently runs three goroutines for V2 instances. Two additional goroutines are added: `watchSPDKShard` (via `c.ShardWatch()`) and `watchSPDKShardGroup` (via `c.ShardGroupWatch()`). Both follow the existing pattern: receive state-change events from the SPDK service stream and forward them to the instance manager's notification channel.

**InstanceSuspend / InstanceResume / InstanceSwitchOverTarget / InstanceDeleteTarget / InstanceReplace for shard and shardgroup types:** Shard instances are passive storage (lvol + NVMe-oF export) with no frontend, so `InstanceSuspend` and `InstanceResume` return `InvalidArgument` for type `shard`. `InstanceSwitchOverTarget` and `InstanceDeleteTarget` also return `InvalidArgument` (shards have no target concept). `InstanceReplace` returns `Unimplemented` for type `shard` - shard replacement goes through `ShardGroupShardReplace` + `ShardGroupShardRebuildStart`. ShardGroup instances similarly have no frontend (their NVMe-oF export is consumed by the engine, not by a workload), so the same RPCs return `InvalidArgument`/`Unimplemented` for type `shardgroup` - ShardGroup process restart on a new node is handled by `InstanceDelete(cleanup=false)` + `InstanceCreate` orchestrated by the ShardGroup controller, not by an instance-level migration RPC.

**SpdkInstanceSpec protobuf extension:** The `SpdkInstanceSpec` message (used in `InstanceCreateRequest`) needs new fields for the new instance types:
- `shardgroup_spec` (`ShardGroupSpec`) - ShardGroup process creation parameters; nil for non-shardgroup instances. `ShardGroupSpec` contains: `data_chunks uint32`, `parity_chunks uint32`, `strip_size_kb uint32`, `shards map<string, EcShardInfo>` (key is Shard CR external name `<volumeName>-<slotIndex>`; value has address + slot index), `salvage_requested bool` (true for ShardGroup process restart on a new node, dispatching to `RecreateEC` semantics).
- `slot_index` (`uint32`) - slot position within the EC array, used when instance type is `shard`. Zero for engine, replica, shardgroup, and frontend instances.
- `lvs_name` (`string`) - SPDK bdev name of the target lvstore, used when instance type is `shard`.
- `lvs_uuid` (`string`) - SPDK lvstore UUID, used when instance type is `shard`.

The mode-discriminant field groups in `SpdkInstanceSpec` are:
- **RAID1 engine:** `replica_address_map` with N entries (field 1)
- **EC engine:** `replica_address_map` with exactly 1 entry pointing at the ShardGroup endpoint (field 1; same field as RAID1)
- **Shard:** `slot_index`, `lvs_name`, `lvs_uuid`
- **ShardGroup:** `shardgroup_spec`

EC and RAID1 engines are **structurally identical inside `SpdkInstanceSpec`** - this is the property that keeps the `InstanceDelete` path layout-blind. Layout discrimination is carried at the request level instead: `InstanceCreateRequest` gains a top-level `data_layout_type` field, which the instance-manager forwards to `spdkrpc.EngineCreateRequest.data_layout_type`. `SpdkInstanceSpec` does **not** carry the layout discriminator; placing it on the request rather than the spec is intentional, because the spec is what the delete path reads.

There is **no longer a separate `ec_spec` field on engine instances**. The engine consumes a uniform `replica_address_map` regardless of topology - the only difference between RAID1 and EC engines is the entry count plus the request-level `data_layout_type`.

`shardgroup_spec` is populated by the longhorn-manager when the instance type is `shardgroup`. For all other instance types, `shardgroup_spec` is nil. The longhorn-manager sets `salvage_requested=true` on `ShardGroupCreate` when re-provisioning an existing ShardGroup process on a new node (engine-node failover) - the SPDK service interprets this as "the lvstore on bdev_ec already exists; re-discover via examine rather than re-create."


**SPDK client naming:** The SPDK internal shard name is `shard-<volumeName>-<slotIndex>` (produced by `GetShardName` in longhorn-spdk-engine). However, the external name exposed through the Longhorn control plane is `<volumeName>-<slotIndex>` (no `shard-` prefix) - matching the Shard CR name. The translation is handled entirely inside the SPDK client (`longhorn-spdk-engine/pkg/client/client_shard.go`): `ShardGet`, `ShardDelete`, and `ShardExpand` prepend `"shard-"` to the caller-supplied name before sending gRPC, and `ProtoShardToShard` strips the prefix from `ShardId` in the response (`strings.TrimPrefix(s.ShardId, "shard-")`). Instance-manager code uses the external name (`<volumeName>-<slotIndex>`) everywhere and never constructs the internal `shard-` prefixed name itself.

**Response conversion:** A new `shardResponseToInstanceResponse()` function converts `ShardResponse` from the SPDK service into the generic `InstanceResponse` used by the Instance Service. This follows the same pattern as `engineResponseToInstanceResponse()` and `replicaResponseToInstanceResponse()`. Sets `PortStart` and `PortEnd` from the shard's allocated port so the shard controller can construct the NVMe-oF transport address. Sets `Uuid` from `api.Shard.UUID` (the SPDK lvol UUID propagated through `spdkrpc.Shard.uuid` -> `ProtoShardToShard` -> `api.Shard.UUID`) so that the orphan admission webhook's `InstanceUUID` validation passes when creating `OrphanTypeShardInstance` CRs.

**State string conversion:** Unlike replicas (whose `spdkrpc.Replica.State` is a plain `string` field already carrying `"running"`, `"error"`, etc.), `spdkrpc.Shard.State` is an `EcSlotState` enum. `EcSlotState.String()` returns `"EC_SLOT_STATE_NORMAL"` or `"EC_SLOT_STATE_FAILED"` - not the standard Longhorn instance state strings. `ProtoShardToShard()` must map the enum to standard strings before the state reaches `InstanceResponse.Status.State`: `EC_SLOT_STATE_NORMAL` -> `"running"`, `EC_SLOT_STATE_FAILED` -> `"error"`. Without this conversion the shard controller in longhorn-manager never observes a shard in state `"running"` and reconciliation loops indefinitely.

**`Shard.role` gRPC field:** The `spdkrpc.Shard` message contains a `role` field (`EcSlotRole`). The shard-side SPDK service **cannot** populate this field because `ShardCreateRequest` deliberately omits k (the number of data chunks) - the shard node creates a raw lvol regardless of role. The `role` field in every `ShardGet`/`ShardList` response is therefore always `EC_SLOT_ROLE_DATA` (the proto3 zero value). Implementors must not rely on this field from the gRPC layer. The authoritative role is computed at the ShardGroup controller from `slot_index < dataChunks` (DATA) or `>= dataChunks` (PARITY) and written into the Kubernetes Shard CR `status.role` for observability.

##### Proxy service impact

The Proxy service (`pkg/proxy/proxy.go`) abstracts V1/V2 engine operations through the `ProxyOps` interface. For RAID1 volumes, the proxy exposes `ReplicaAdd`, `ReplicaList`, `ReplicaRebuildingStatus`, `ReplicaRebuildingQosSet`, `ReplicaRemove`, and `ReplicaVerifyRebuild` methods.

For EC volumes, the Proxy layer is **not extended** with shard equivalents. This is a deliberate architectural choice: EC rebuild is orchestrated differently from RAID1 rebuild and targets the ShardGroup process rather than the engine.
- **RAID1 rebuild:** manager -> proxy -> `EngineReplicaAdd` -> RAID1 adds member, engine drives rebuild internally.
- **EC rebuild:** manager -> SPDK service on the ShardGroup-process-hosting node -> `ShardGroupShardReplace` (hot-swap) + `ShardGroupShardRebuildStart` (background rebuild). The rebuild is a two-step operation coordinated by the ShardGroup controller, executed inside the ShardGroup process, not the engine, and not the proxy.

All EC-specific operations (`ShardGroupShardReplace`, `ShardGroupShardRebuildStart`, `ShardGroupShardRebuildStop`, `ShardGroupShardRebuildProgress`, `ShardGroupShardRebuildQosSet`, `ShardGroupExpand`, `ShardGroupGet`, `ShardGroupSnapshot*`) go through the SPDK service (port 8504) on the ShardGroup-process-hosting node directly, bypassing the proxy (port 8501). The ShardGroup controller connects to that node's SPDK service to invoke these methods. The proxy remains unchanged for both EC and RAID1 volumes.

##### Disk service impact

The Disk service (`pkg/disk/disk.go`) manages storage accounting through the `DiskOps` interface. `DiskReplicaInstanceList` returns metadata for all replica lvols on a given disk, which the scheduler uses for free-space calculations.

For EC volumes, shard lvols also consume disk space. However, the existing `InstanceList` (Instance service) already enumerates all shards, and `InstanceDelete` can remove orphans. A dedicated `DiskShardInstanceList` RPC is **deferred** until longhorn-manager's disk controller requires disk-scoped shard queries for scheduling or orphan cleanup. At that point, a new proto message (`DiskShardInstanceListRequest`/`DiskShardInstanceListResponse` with a `ShardInstance` type) would be added to `longhorn/types` first, then wired through the `DiskOps` interface here.

The existing `DiskReplicaInstanceList` is not modified - it continues to return only RAID1 replica lvols.

##### longhorn-manager changes

New CRDs (`ShardGroup`, `Shard`), and StorageClass parameters (`dataLayout.dataChunks`, `dataLayout.parityChunks`, `dataLayout.stripSizeKB`). See Longhorn Control-plane Implementation Overview section below.

## Design

**Terminology:**

- **Chunk**: A single unit of data stored on one disk within a stripe. In SPDK, this is referred to as `strip_size` following RAID conventions. This document uses "chunk" instead to avoid confusion with "stripe"
- **Stripe**: One full row across all k+m disks. Each stripe contains k data chunks and m parity chunks.

### SPDK Implementation Overview

The EC module implements a virtual SPDK bdev backed by k+m base bdevs. The base bdevs can be any SPDK bdev type. The module is registered as an SPDK bdev module and exposes its operations via JSON-RPC.

#### Async bdev_ec_create

`bdev_ec_create` is an asynchronous JSON-RPC. The response (`true`) is sent only after the EC bdev is fully initialized and ready — including the WIB load from disk. This is deliberate: blocking the SPDK reactor during WIB load would stall I/O for every other bdev on the system while the parity disks are read.

The internal flow is:
1. `rpc_bdev_ec_create` decodes the request, allocates `ec_bdev_create_async_ctx` (holds the decoded params and the pending `spdk_jsonrpc_request *`), and calls `ec_bdev_create_async`.
2. `ec_bdev_create_async` allocates the `ec_bdev` struct, opens all base bdev descriptors, computes geometry, allocates DMA buffers and WIB arrays, registers per-thread I/O channels, and registers the bdev with the SPDK bdev layer. All of these complete synchronously on the caller's stack. It then hands off to `ec_wib_load_async`.
3. `ec_wib_load_async` drives a non-blocking callback chain that reads both WIB copies from each parity disk, validates CRC and generation, and OR-merges the result into the in-memory `wib_region_map`. Each read is submitted as a standard async bdev I/O — the reactor is free to process other I/O between callbacks.
4. On completion, `ec_bdev_create_wib_done` is called. If the load succeeded and dirty regions were found, `ec_bdev_scrub_start` is called to kick off the background scrub. The JSON-RPC response (`true`) is then sent via the saved `spdk_jsonrpc_request *`, and the `ec_bdev_create_async_ctx` is freed.

**Invariant:** The bdev is registered with the SPDK bdev layer before WIB loading begins (so it appears in `bdev_get_bdevs`), but is not yet safe to issue foreground writes against until WIB loading completes. In practice `bdev_ec_create` is always called before the lvol store and lvol are created on top of it, so no I/O is in flight during this window. If WIB loading fails, the bdev is unregistered and an error is returned.

#### How data is laid out:

Data is distributed across all k data disks in fixed size chunks (configurable). A stripe is one row across all disks - every disk holds exactly one chunk at each row, with no gaps. The k disks hold data chunks and the remaining m disks hold parity chunks, all the same size. Parity chunks are computed using ISA-L Reed-Solomon encoding over the k data chunks in the same row.

```
Example: k=2, m=2, chunk_size=32KB

             disk 0      disk 1      disk 2      disk 3
             (data 0)    (data 1)    (parity 0)  (parity 1)

stripe 0:   [32KB data]  [32KB data]  [32KB par]  [32KB par]
stripe 1:   [32KB data]  [32KB data]  [32KB par]  [32KB par]
stripe 2:   [32KB data]  [32KB data]  [32KB par]  [32KB par]

Every disk, every row - fully occupied. No wasted space.
Usable capacity = k / (k+m) = 50% of total raw storage.
```

**Slot index permanence.** The position of a disk in `BaseBdevs[]` at `bdev_ec_create` time is its permanent slot assignment for the entire life of the volume. This is not a soft label — it is baked into the Reed-Solomon encode matrix.

ISA-L builds the encode matrix via `gf_gen_rs_matrix` once at `bdev_ec_create`. Each row of the matrix corresponds to one output chunk (data or parity). The coefficients in row `i` depend on `i` — so parity on disk 2 encodes "chunk at slot 0" and "chunk at slot 1" with specific Galois-field weights; parity on disk 3 uses different weights. This mapping is fixed on-disk from the moment the first stripe is written.

Consequences:
- Two disks in the same EC bdev cannot be swapped without re-encoding every parity chunk on disk. Doing so silently corrupts all parity.
- A replacement disk (hot-swap via `bdev_ec_replace_base_bdev`) must occupy the **same slot index** as the disk it replaces. The rebuild writes reconstructed data for that slot to the new disk — slot identity is preserved.
- The control plane must **persist `slot_index`** in the Shard CR at creation time and pass it faithfully to every subsequent `ShardGroupCreate` and `ShardGroupShardReplace` call. It is not a hint that can be inferred from node layout or disk order at runtime; it is part of the volume's on-disk identity.

#### How address translation works through the stack:

When an application writes to a mounted filesystem, the write passes through several layers, each translating the address before passing it down. Example of how a write to logical byte offset 100 KB traces through the stack (k=2, m=2, chunk_size=32 KB, 4 KB block size).
```
Layer 1: Filesystem (ext4)
    Application writes to file offset -> ext4 maps to logical block number
    Example: file write -> logical block 25 (= byte offset 100 KB / 4 KB)

Layer 2: NVMe-oF initiator (kernel)
    Logical block 25 -> NVMe command send over TCP to spdk_tgt

Layer 3: NVMe-oF target (spdk_tgt)
    Receives NVMe command for block 25 -> forwards to the lvol bdev

Layer 4: Lvol / Blobstore
    Lvol 25 -> blobstore looks up which cluster this block belongs to, maps it to a physical offset within the lvol store's address space.
    Example: cluster 0 starts at blobstore offset X, block 25 is at blobstore offset X + 25.
    The blobstore translate this to an offset on the underlying bdev (ec0).
    Example ec0 block 25

Layer 5: EC bdev (ec0)
    This is where the EC address translation happens.
    ec0 block -> which stripe? which disk? which offset on that disk?

    chunk_size_in_blocks = chunk_size / block_size
                         = 32 KB / 4KB
                         = 8 blocks per chunk

    stripe blocks = k * chunk_size_in_blocks
                  = 2 * 8
                  = 16 blocks per stripe

    stripe index = 25 / 16
                 = 1 (second stripe, 0-indexed)

    offset_in_stripe = 25 % 16
                     = 9 (9th block within the stripe)

    disk_index = 9 / 8
               = 1 (second data disk, since chunk = 8 blocks)
    
    offset_in_chunk = 9 % 8
                    = 1 (1st block within disk 1's chunk)

    disk_lba = stripe_index * chunk_size_in_block + offset_in_chunk
             = 1 * 8 + 1
             = LBA 9 on disk 1

Layer 6: Base bdev (disk 1 = AIO / NVMe / NVMe-oF)
    LBA 9 -> physical I/O to the underlying storage device
```

The EC layer is the only layer that fans out to multiple disks. All other layers are 1-to-1 address translation. For writes, the EC layer also computes parity and write to the parity disks at the same stripe LBA.

#### How reads work:

In the normal case (all disks healthy), a read maps directly to on data disk (no parity is involved). If only pariaty disks have failed, reads still go directly to data disk since parity is not needed for reads.

If a data disk has failed, the module reads k chunks from the first k healthy disks (scanning in order, skipping failed ones - this includes data and parity disks). It then reconstructs the missing data chunk using ISA-L Reed-Soloman (RS) decoding (`gf_invert_matrix` to build recovery coefficients, then `ec_encode_data` to apply them).The RS math should guarantee reconstruction succeeds regardless of which specific k disks survices, as long as at least k are healthy (i.e., up to m failures of any combination).

When multiple data disk fails simultaneously (e.g., 2 out of 4 in a 4+2 array), the module reconstructs all missing chunks together within the same read request, instead of repeating the full recovery process for each one. The flow is:

1. **Collect survivors**: Read k chunks from the first k healthy disks, just like the single-failure case. These k chunks could be a mix of data and parity.
1. **Build recovery table**: To understand this step, we will first look at how RS parity is created, then how recovery reverses the process.

    **How parity is created (encoding):**
    Each parity chunk is computed from all data chunks using a formula (`gf_gen_rs_matrix`) generated by the ISA-L library when the EC array is created. This formula is stored in memory for the lifetime of the array and used for every parity computation. For a simple k=2, m=2 example, suppose the data chunk contains value D0 and D1:
    ```
    parity 0 = (weight_a * D0) + (weight_b * D1)
    parity 1 = (weight_c * D0) + (weight_d * D1)
    ```
    The weight (a, b, c, d) are fixed number chosen by ISA-L so that any 2 of the 4 chunks (D0, D1, P0, P1) are sufficient to recover the other 2.
    ```
                    D0      D1
    disk 0 (D0):   [ 1       0 ] -> D0 = 1*D0 + 0*D1
    disk 1 (D1):   [ 0       1 ] -> D1 = 0*D0 + 1*D1
    disk 2 (P0):   [ a       b ] -> P0 = a*D0 + b*D1
    disk 3 (P1):   [ c       d ] -> P1 = c*D0 + d*D1
    ```

    We can store this as a table, where each row describes how to produce one disk's chunk from the original data:
    ```
                    D0    D1
    disk 0 (D0):  [ 1     0 ]   -> D0 = 1×D0 + 0×D1  (just store D0 directly)
    disk 1 (D1):  [ 0     1 ]   -> D1 = 0×D0 + 1×D1  (just store D1 directly)
    disk 2 (P0):  [ a     b ]   -> P0 = a×D0 + b×D1   (weighted mix)
    disk 3 (P1):  [ c     d ]   -> P1 = c×D0 + d×D1   (weighted mix)
    ```

    **How recovery works (decoding):**
    Suppose disk 1 (D1) fails. We can read from disk 0, 2 and 3. So the module picks disk 0 and disk 2:
    ```
    disk 0 gives us D0 (directly, it's a data disk)
    disk 2 gives us P0 = a*D0 + b*D1 (in table we stored earlier)
    ```
    We knoe D0 (we read it) and P0 (we read it). We know that value of a and b becasue they were generated when the EC array was created and are stored in memeory - they are the same value used to compute P0 in the first place. So we can solve for D1:
    ```
    P0 = a*D0 + b*D1
    -> b*D1 = P0 - a*D0
    -> D1   = (P0 - a*D0) / b
    ```
    That "solving" step is what table inversion (`gf_invert_matrix`) does - it takes the rows for the survivors and computes new recovery wrights that tell us how to combine the survior chunks to get back the missing data. The recovery table is not stored on disk - it is computed on the flow during each degraded read from the original encoding table (which is kept in memory for the lifetime of the EC array). It must be recomputed each time becasue which disk have failed can change at any moment.

    **The multi-failure is the same idea**
    If 2 disk fail (say disk 0 and disk 1 - both data disks gone), we read from disk 2 (P0) and disk 3 (P1)
    ```
    P0 = a*D0 + b*D1
    P1 = c*D0 + d*D1
    ```
    Two equations, two unknown (D0 and D1). The table inversion solves both simultaneously, producing recovery weight that say "combine P0 and P1 with these new weight to get D0" and "combine P0 and P1 with these other weight to get D1". ISAL applies both recipes to the survivor data in one pass.

    The RS algorithm guarantees this system of equations is wasy solvable as long as we have at least k survivors - no matter which specific disk failed. This is why up to m failures can be tolerated.

1. **Extract recovery recipes**: From the inverted table, extract one row per failed disk. Each row is a "receipt" - a set of weights that says how to combine the k survivor chunks to reproduce that specific missing chunk. If 2 disk failed, we extract 2 recipes.
1. **Recovery all at once**: Feed al the receipt and the k survivor chunks into ISA-L (`encode_data` - the same function used for encoding, since the math is identical; the difference is which coefficient are used) in one call. ISA-L applies each recipe to the survivor data and produces all missing chunks simultaneously.

#### How write works

A full-stripe write (the entire stripe is being written at once) encodes parity directly from the new data and writes all k+m chunks in parallel. No read are needed.

A sub-stripe write (paritial stripe) follows a read-modify-write (RMW) sequence:

1. Mark the stripe as "dirty" (see dirty bitmap details below). If already dirty, the write is requeued.
1. Read all k data chunks for the stripe from healthy disks.
1. If any data disk has failed, reconstruct the missing chunks from the surviving k disks using RS decoding (`gf_invert_matrix` + `ec_encode_data`).
1. Copy the caller's write data into the data buffer at the offset corresponding to the write's postion within the stripe. Fro example, if the stripe covers logical blocks 0-63 and the write targets block 20-25, the new data is copied into the buffer at the position for block 20-25, leaving the rest of the stripe's data unchanged.
1. Re-encode all m parity chunks from the modified data (`ec_encode_data`).
1. Write the modified data chunk and all m parity chunks back to disk. The other k-1 data chunks are not written back - they were read for parity encoding but their on-disk content is unchanged, so writing them would be wasted I/O. This reduces write amplification from k+m writes per RMW to 1+m writes.
1. Clear the dirty bit.

#### Dirty bitmap details

The EC module maintains a dirty bitmap in memory to track which stripes have an RMW in progress. The bitmap is an array of `unit64_t` integers. Each `unit64_t` is 64 bits wide, se each element in the array can track 64 stripes - on bit per stripe:

```
array[0] holds bits for stripes 0–63:
+-------------------------------------------------------+
| bit0  bit1  bit2  bit3  ............... bit62  bit63  |
| str0  str1  str2  str3  ............... str62  str63  |
+-------------------------------------------------------+

array[1] holds bits for stripes 64–127:
+-------------------------------------------------------+
| bit0  bit1  bit2  bit3  ............... bit62  bit63  |
| str64 str65 str66 str67 ............... str126 str127 |
+-------------------------------------------------------+

array[2] holds bits for stripes 128–191...
...
```

To find which bit represents a given stripe, two steps:
- Which array element? Divide the stripe number by 64, drop the remainder. This gives use which group of 64 the stripe falls in.
- Which bit within that element? Subtract the group's staring stripe number. This give use the position within the group.

Example:
```
Stripe 100:
    100 % 64 = 1    -> array[1] (covers stripe 64-127)
    100 - 64 = 36   -> bit 36 inside array[1]

Stripe 230:
    230 % 64 = 3    -> array[3] (covers stripe 192-255)
    230 - 192 = 38  -> bit 38 inside array[3]

Stripe 5:
    5 % 64 = 0      -> array[0] (covers stripe 0-63)
    5 - 0  = 5      -> bit 5 inside array[0]
```
The bit operations:
```
Set dirty:      array[1] |=  (1 << 36)
Clear dirty:    array[1] &= ~(1 << 36)
Check dirty:    array[1] &   (1 << 36)
```
The bitmap is allocated when the EC array is created, sized to cover all stripes. All bts start at 0 (clean). No locking because all I/O runs on a single SPDK reactor thread.

Before starting RMW, the module checks the stripe bit - if its dirty (another RMW is in progress), then requeue the write. If is clean, the module sets it dirty to claim exclusive access to the stripe.

#### Crash safety of the dirty bitmap

The dirty bitmap is in-memory only - it does not survive a node crash or reboot. On restart, all bits start at zero (clean). This is safe for bitmap's primary purpose (preventing concurrent RMW on the same stripe) becasue the crash kills all in flight I/O, so there are no conflicting operations when the node restarts.

However, there is a seperate crash-saftly concerns: If the node crashes mid-RMW - after writing some chunks of a stripe to a disk but not all - that stripe has inconsistent data and parity on disk. For example, the data chunk may bave een updated but the parity chunk was not uet written. After restart, the bitmap is gone so the module doesn't know which stripes were mid-write. A subsequent disk failure could cause silent data corruption becasue the stale parity doesn't match the updated data.

The following approach is implemented:
- Write-intent bitmap (WIB, persistent on disk) - a persistent bitmap stores on the parity disk that marks which regions have writes in progress. On crash recovery, only the marked regions need scrubbing, not the entire volume. this is the same pattern used by Linux md-raid ( see [kernel RAID document](https://docs.kernel.org/admin-guide/md.html) and [md-raid write intent bitmap wiki](https://archive.kernel.org/oldwiki/raid.wiki.kernel.org/index.php/Write-intent_bitmap.html)). It combines fast startup with simple scrubbing. The samll write overhead during normal operation (the on-disk bitmap must be updated before each RMW) is acceptable because it only applies to the first RWM into a cold region - subsequent RMW's to the saem region skip the persist (the bit is already set).

#### WIB on-disk layout

The WIB is stored as two double-buffered copies at the tail of each parity disk, occupying the last 2 * strip_size. `ec_compute_geometry` subtracts this reservation from the usable capacity.
```
[ magic       u64 ]   - "EC WIB0" (0x45432057494230)
[ version     u32 ]   - 1
[ generation  u32 ]   - monotonically increasing; highest wins on startup
[ num_regions u32 ]   - ceil(num_stripes / 1024)
[ _pad        u32 ]
[ region_bits[]   ]   - ceil(num_regions / 64) uint64_t words, one bit per region
[ crc32c      u32 ]   - covers everything above
```
Write alternate between copies (double-buffering). The active copy is flipped only after all m parity disk writes complete successfully. This ensures the previous copy remains valid if a crash interrupts the persist. On startup, `ec_wip_load` reads both copy from all m parity diskss, validates each (magic, version, CRC), picks the highest-generation valid copy from each disk, and OR-merges across all disks - a region is dirty if any parity disk's copy has the bit set.

#### WIB region granularity and sizing

The Write Intent Bitmap (WIB) tracks dirty regions of the volume so that after a crash, scrub only has to re-encode parity for regions that had writes in flight - not entire volume.

#### The regsion size is fixed at 1024 stripes per bit

Each bit in the WIB covers a **region** of `EC_WIB_REGION_STRIPES = 1024` stripes. The bitmap grows with the volume -  `wib_num_regions = ceil(num_stripes / 1024)`, but the size each bit covers does not.

With the default (64 KB strips on a 4+2 array), one stripe holds 256 KB of user data, so:

> 1 bit = 1024 stripes = 256 MiB

This is the blast radius of a dirty bit: the amount of data scrub has to en-code if that bit was set when the system crashed.

##### Why 1024?

Region size is a tradeoff between different bitmap size, how often the bitmap has to be persisted during normal writes, and how much work scrub has to do per dirty bit after a crash. The two extremes show why the middle would be suitable.

**1 stripe per bit (too small).** The bitmap is huge - one bit per stripe across the whole volume. Almost every write dirties a fresh bit and forces a bitmap persist. The upside is a tiny scrub blast radius: one dirty bit means re-encoding parity for exactly one stripe, or 256 KB. The overhead is continuous (on every write) while the benefit is occasional (only at crash recovery).

**65,536 stripes per bit (too large).** The bitmap is tiny and persists are nearly free, becasue most writes land in already-dirty regions. But one dirty bit now covers 16GiB, and scrub has to re-encode all of it on restart even if only one stripe was in flight.

**1024 stripes per bit (the chosen).** Each region is 256 MiB, which is small enough to scrub, and large enough that the bitmap stays compact and persists are rare - a bit only needs write the first time a region goes from clean to dirty, and in steady I/O most writes land in regions that are already dirty. The bitmap is cheaper during normal operations and recovery is reasonable.

#### On-disk reservation

Each parity disk holds a copy of the WIB, and reserves space for two copies, so that bitmap udates are crash-safe. When the bitmap changes, the new version is written to the none-active slot first, only then marked as active. If the system crashes partway through, the old copy is still intact and gets used on restart. The reservation is on strip per copy. so two strips total. With the default 64 KB strip size (128 KiB reserved on each parity disk). The size is fixed - if the actual bitmap is smaller than one strip, the leftover space is zero-padded. This reserved space is subtracted from the parity disk's usable capacity by `ec_comput_geometry`.

#### Maximum volume size

Every EC volume has a hard capacity ceiling, and it's determined at creation time. the ceiling exists because the entire WIB has to fit inside a single strip - once the bitmap fills the strip, there is no rooom to track more dirty regions, and the volume cannot grow.

```
max_user_bytes = (strip_bytes − 28) × 8 × 1024 × stripe_bytes
                  └────────┬──────┘   │     │         │
                           │          │     │         └─ user data per stripe = k × strip_bytes
                           │          │     └─ EC_WIB_REGION_STRIPES (stripes per WIB bit, fixed)
                           │          └─ bits per byte
                           └─ bytes left in one strip after the on-disk header (24 B) + CRC32C (4 B) = 28 B
```

In words: count the bytes available for the bitmap inside one strip, multiply by 8 to bet bits, by 1024 to get the stripes those bits address, and by the per-stripe user-data size to get total addressable capacity. `ec_compute_geometry` enforces this bound and refuses to register the bdev if exceeded.

| Strip size          | k=2       | k=4         | k=8         | k=16        |
| ------------------- | --------- | ----------- | ----------- | ----------- |
| 4 KB                | 0.2 TiB   | 0.5 TiB     | 1.0 TiB     | 2.0 TiB     |
| 16 KB               | 4 TiB     | 8 TiB       | 16 TiB      | 32 TiB      |
| **64 KB (default)** | **64 TiB**| **128 TiB** | **256 TiB** | **512 TiB** |
| 256 KB              | 1 PiB     | 2 PiB       | 4 PiB       | 8 PiB       |
| 1 MB                | 16 PiB    | 32 PiB      | 64 PiB      | 128 PiB     |

The two scaling axes compose: doubling k doubles the max volume size, and doubling strip_size *quadruples* it - both the bytes-per-bit and the bits-that-fit-in-one-strip grow linearly with strip size.

For the default Longhorn EC configuration (4+2 with 64 KB strips), the limit is ~128 TiB per volume. Larger volumes need either a larger strip size at create time. The limit cannot be raised after creation, because changing `strip_size_kb` would invalidate the parity layout.

##### About `strip_size_kb`

`strip_size_kb` lis set at volume creation via `bdev_ec_create` (`dataLayout.stripSizeKb` in Longhorn StorageClass / Volume spec).

**Constraints** (enforced by `_ec_bdev_create` and `ec_compute_geometry`):
- **Power of two.** Address translation (LBA -> stripe / chunk / offset) reduces to bit shift and masks instead of integer division on every read and write.
- **At leas one block.** `strip_size = strip_size_kb * 1024 / blocklen` must be >= 1, otherwise a chunk as zero addressable blocks and address translation is undefined. `ec_compute_geometry` returns `-EINVAL`.
- **Fixed for the volume's lifetime.** Every stripe on disk is laid out and parity-encoded against the original value, and the WIB region size derives from it. Changing strip size would require relocating every stripe, re-encoding every parity chunk, and rewriting the WIB header - effectively a full offline rewrite. `bdev_ec_resize` preserves `strip_size_kb` because resize is in-place (growing existing chunks, not redrawing them).

**Practical range: 4 KB to 1 MB.** The max-volume-size table above covers this range. Outside it, thing break down:
    - *Below 4 KB:* RMW overhead dominates. Small writes pay full read-modify-write cost without getting the throughput benefit of larger I/O.
    - *Above 1 MB:* sub-stripe write amplification gets painful (a 4 KB random write at k=4 reads ~4 MB to recompute parity), the WIB scrub blast radius exceeds 4 GiB per dirty region, and DMA buffer pressure rises sharply. The code does not reject these values, but operational properties have not been validated there.

**Choosing strip size by workload:**
| Strip size                 | When it fits                                                                  |
| -------------------------- | ----------------------------------------------------------------------------- |
| Smaller (4–16 KB)          | Small random writes (databases, VMs with many small files). Smaller RMW read penalty. More stripes per volume -> smaller max addressable size. |
| **Default (64 KB)**        | General-purpose or mixed workloads. Reasonable across the board.              |
| Larger (256 KB – 1 MB)     | Large sequential writes (media, backup targets, archives). Better throughput and larger max volume size, at the cost of higher RMW penalty for sub-stripe writes. |

##### How k and m affect WIB sizing

The two EC shape parameters affect the WIB very differently:

- **k (data chunks per stripe) scales WIB sizing linearly.** Doubling k
  doubles the user data per stripe, which doubles bytes-per-bit, which
  doubles the max addressable volume size at a fixed strip size. The
  formula is `bytes_per_bit = 1024 × k × strip_size_kb × 1024`.
- **m (parity chunks per stripe) does not affect WIB sizing at all.**
  More parity disks means more *copies* of the WIB, but each copy is
  the same size. The reservation per parity disk is always
  `2 × strip_size`.

Holding strip size at the default 64 KB and varying k:

| Geometry          | k  | Bytes per bit | Scrub blast radius | Max volume size |
| ----------------- | -- | ------------- | ------------------ | --------------- |
| 2+m               | 2  | 128 MiB       | 128 MiB            | ~64 TiB         |
| **4+m (default)** | 4  | **256 MiB**   | **256 MiB**        | **~128 TiB**    |
| 6+m               | 6  | 384 MiB       | 384 MiB            | ~192 TiB        |
| 8+m               | 8  | 512 MiB       | 512 MiB            | ~256 TiB        |
| 16+m              | 16 | 1 GiB         | 1 GiB              | ~512 TiB        |

##### How to choose k, m, and strip_size

Pick parameters in this order, since each depends only on the ones above
it:

1. **m - durability.** How many simultaneous disk failures must the
   volume tolerate? Pick m to match. Independent of WIB sizing.
2. **k - efficiency vs. fan-out.** Higher k means better storage
   efficiency (`k / (k+m)` of raw capacity is usable) but more disks to
   touch on every write. Higher k also requires more schedulable nodes
   for anti-affinity.
3. **strip_size - workload shape.** Large sequential writes want bigger
   strips. Small random writes want smaller strips. Mixed or unknown
   workloads should stay on the 64 KB default.
4. **Verify max volume size.** Cross-check the chosen (k, strip_size)
   against the capacity table. If the volume needs more, doubling
   strip_size quadruples the ceiling.

##### Recommended configurations by volume size

The default 4+2 / 64 KB is the right answer for most deployments.
Petabyte-scale volumes need either a wider strip or a wider array.

| Volume size       | Configuration  | Overhead | Rationale                                                                                                                                                                                |
| ----------------- | -------------- | -------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| ≤ 100 TiB         | **4+2, 64 KB** | 1.5×     | Default. Best latency for small random writes, simple rebuild, small fan-out (6 shards).                                                                                                 |
| 100 TiB – 1 PiB   | 4+2, 256 KB    | 1.5×     | Same durability, wider strip raises the ceiling to ~2 PiB. RMW read amplification grows 4× but stays manageable. Scrub blast radius grows to 1 GiB (still seconds on local NVMe).         |
| 1 PiB – 5 PiB     | 8+2, 256 KB    | 1.25×    | Storage efficiency matters at PiB scale. Wider fan-out (10 shards) assumes a mostly cold/archival workload where the RMW path is rarely exercised. Needs 10+ schedulable nodes.           |
| 5 PiB+            | 16+4, 1 MB     | 1.25×    | Enterprise cold storage. Minutes-long scrub windows and wide rebuild fan-out; only appropriate for mostly-sequential workloads (bulk backup, media archive).                              |

Two caveats when reading this table. Every row is a *creation-time*
decision - `strip_size_kb` and the (k, m) shape are immutable for the
volume's life, so size for the volume's lifetime capacity, not just
provisioning-day capacity. And the overhead column is raw-to-usable ratio
only; it does not include filesystem overhead, snapshot reserves, or
replica placement at the Longhorn layer.



#### WIB persist lifecycle
1. Region goes cold -> dirty: The first RMW into a clean region sets the in-memory dirty bit and persists it to disk before any data/parity writes proceed. The RMW's data writes are deferred until the persist confirm the bit on disk (via `ec_wib_set_cb`).
1. Persist coaliescing: If a persist is already in flight (`wib_persist_pending`), the new dirty bit is recorded in memory and `wib_repersist_needed` is set. Data writes for the new RMW are queued on `wib_deferred_writes` (a TAILQ). When the in-flight persist completes, `ec_wib_persists_write_cb` starts a follow-up persist to capture the new bit, then drains the derred queue once the follow-up completes. This quarantees that every dirty bit reaches disk befor its corresponding data wries, evnen under concurrent RMW to different cold regions.
1. Region goes idle -> clear: A background poller (`ec_wib_idle_poller_cb`, every 100ms) scans regions for those with the dirty bit set, in-flight cound of zero, and idle for more than 500 ms (`EC_WIB_IDLE_MS = 500`). It clears the in-memory bit and fires a persit to write the cleared state to disk.

#### Startup scrub:

On startup, if `ec_wib_load` finds any dirty regions, `ec_bdev_scrub_start` drives a background scrub. For each dirty region, for each stipe in that region: read k data chunks from readable disks, re-encode all m parity chunks, and write parity back. After all tripes in a region are scrubbed, the region bit is cleared. After all dirty regions are scrubbed, the cleared bitmap is persisted.

The bdev is registered and avoiable for read immediately. RMW writes to a strip whose region has not yet been scrubbed return `EAGAIN` (NOMEM -> requeue by SPDK) to prevent the scrubber and forground RMW from racing on the same parity data.

The scrub requires all k data to be NORMAL (so it can read correct data). If any data disk is FAILED at startup, the scrub is deferred until the disk is replaced and rebuilt - the dirty region bits remain set and scrub runs automatically after `ec_rebuild_finish` completes. This is by design and not fixable without adding a persistent scrub journal - the scrub needs correct data to re-encode parity, and with a failed data disk the only way to get the data is through RS reconstruction from surviving disks, which itself relies on parity being consistent (the very thing the scrub is trying to fix). The circular dependency means deferral is the only safe option.

#### IO splitting

SPDK's bdev layer has a built-in I/O splitting feature that any bdev modlule an opt into. The EC module explicitly enables it by setting `optimal_io_boundary = strip_size` and `split_on_optimal_io_boundary = true` on the `spdk_bdev` struct when the EC bdev is created. This tells SPDK to automatically split any incoming I/O that crosses a chunk never receives a write larger than one chunk (e.g., 32KB), so each RMW only needs to buffer on stripe's worth data.

Other SPDK RAID modules handle I/O boundaries differently:
- RAID0: takes the same `optimal_io_boundary` splitting as the EC module like EC, RAID0 stripes data across disks, so an I/O that crosses a chunk boundary would span two different member disks. Splitting ensures each I/O the RAID0 module receives maps to a single member disk.
- RAID5F takes a stricter approach: it sets `write_unit_size = stripe_size` which cause the bdev layer to reject (not split) any write that isn't full stripe. This is why RAID5F requires full-stripe writes and we need to pair with an FLT layer.
- RAID1 doesn't need splitting: It mirrors I/O to all member identically, with no striping.
- Concat doesn't need splitting - it maps I//O to the correct member bdev based on offset ranges, with no striping.


#### Why splitting is necessary
Without splitting, a write can cross a chunk bondary and land on two differnt disks. For example, a 32KB write starts at byte offset 16 KB in a k=2, 32 KB chunk setup:
```
stripe layout (k=2, chunk_size=32KB)

            disk 0 (data)       disk 1 (data)
            bytes 0-31KB        bytes 32-63KB
stripe 0  [................]  [................]

A 32 KB write arraives at logical offset 16 KB

            disk 0 (data)       disk 1 (data)
            bytes 0-31KB        bytes 32-63KB
stripe 0  [........████████]  [████████........]

The write crosses the chunk boundary.
```

The EC modules' RMW will only handle one chunk on one disk. It might be possible to handle cross-chunk I/O. But this could add more complexity:
- Need to detect chunk boundary crossing and handle writes that span two stips (two different rows, two differnt parity computations).
- Need to track multiple dirty bits for the crossed stripes.
- Need to handle two seperate read-encode-write cycles and merge the completions.

Since SPDK already has the splitting mechanism, the EC module will let the bdev layer handle it, to keep it simple: one chunk, one stripe, one dirty bit, one RMW cycle.


#### EC bdev operation states:
The EC bdev has three states, determined by how many disks are currently failed (`failed_count`):
- ONLINE (`failed_count == 0`): All base bdevs healthy. Reads go directly to one data disk. Writes encode parity and write all k+m chunks normally.
- DEGRADED (`0 < failed_count <= m`): One or more disks have failed, but within the RS tolerance limit. Reads reconstruct missing data from surviving disks. Writes skip failed disks (parity is still computed from available data).
- OFFLINE (`failed_count > m`): Too many failures. All new I/O is rejected with an error. The bdev still exists but is unusable until enough disks are replaced and rebuilt.

```ascii

                disk fails           disk fails
             (failed_count++)     (failed_count++)
                    |                    |
                    v                    v
┌--------┐     ┌----------┐     ┌------------------┐
| ONLINE |---->| DEGRADED |---->|     OFFLINE      |
|   (0)  |     |  (1..m)  |     | (>m, reject I/O) |
└--------┘     └----------┘     └------------------┘
     ^               ^                    ^
     |               |                    |
     |       replace + rebuild    replace + rebuild
     |       (failed_count--)     (failed_count--)
     |               |                    |
     └---------------┘<-------------------┘

```

#### How disk failure is handled:

When a base bdev is removed (disk fails, NVMe-oF connection drops, etc.):
1. Sets the disk's state to FAILED - each disk position in the EC array has a state field that tracks its health.
    ```
    NORMAL = healthy
    FAILED = dead
    REPLACING = new disk installed, rebuild pending
    ```
    The module also increments a `failed_count` counter. This counter avoids scanning all disk state on every I/O - the module checks `failed_count` to quickly decide:
    ```
    0   = all healthy (fast read path; directly read from data disk)
    >0  = degraded (read survivors and reconstruct)
    >m  = too many failures (reject all I/O)
    ```
1. Drains in-flight I/O to that disk via an async cleanup sequence:
    - Quiesce: The module calls `spdk_bdev_quiesce`, this tells SPDK to stop submitting new I/O and wait for all in-flight I/O to compete. This ensures no IO Is actively using the failed disk.
    - Reset: Once quiesced, the module calls `spdk_bdev_reset` on the failed disk's bdev descriptor per disk in an array (`ec->descs[]`), indexed by disk position (0 to k+m-1). This tells the underlying bdev driver to abort any stuck I/O. If the disk is dead (unplugged, device failure, connection lost, etc), the reset will fail becasue there is no device to talk to - the module logs a warning and continue to the next step regardless, best-effort cleanup.
    - Release per-thread channels: SPDK maintains I/O channels for each base bdev on every thread that uses the EC bdev. The module calls `spdk_for_each_channel`, which visits every thread that has a channel for this EC bdev. On each thread, the callback calls `spdk_put_io_channel` on the failed disk's channel and set it to NULL. Then `spdk_put_io_channel` decrements the channel's reference count - when the count is zero, SPDK knows no one is using the channel. Without this step, the reference count never reaches zero and the bdev descriptor cannot be closed and would cause base bdev deletion to hang.
    - Close descriptor: After all channels are released, the module closes the failed disk's bdev descriptor (`spdk_bdev_close`) and sets it to NULL. then module unquiesces the EC bdev so I/O resumes - now operating in degrade mode, skipping the failed disk.

    Note: while quiesced, all IO to the EC bdev is queued by SPDK - no read or write is processed. However, it should be very brief, `spdk_bdev_quiesce` is a standard SPDK bdev API, and that SPDK's other modules also use it for similar purpose.
1. If failure count exceeds m, the EC bdev sets its internal state to OFFLINE. In this state the bdev still exists (its not deleted), but every new I/O request is rejected with an error. The bdev will transition back to ONLINE when Longhorn replaces enough failed disks and completes rebuild to bring the failure count back to >=m.
1. Other wise, the EC bdev continues operating in degrade mode - reads reconstruct missing data from surviving disks, write skip the failed disk.

#### NVMe-oF transient failure vs permanent disk failure

The `SPDK_BDEV_EVENT_REMOVE` callback that triggers slot failure does not fire immediately on network disconnection. SPDK's NVMe-oF initiator has built-in reconnect logic controlled by three parameters configured on `bdev_nvme_attach_controller`:
- `ctrlr_loss_timeout_sec` - total time to attempt reconnection before giving up. `-1` = infinite retry. Recommended: `120` (2 minutes) for production, giving brief network events time to recover.
- `reconnect_delay_sec` - delay between reconnect attempts. Recommended: `5`.
- `fast_io_fail_timeout_sec` - if set, I/O submitted during reconnect fails fast after this many seconds instead of waiting for `ctrlr_loss_timeout_sec`. Recommended: `15` to give applications quick feedback while reconnect continues in the background.

During a transient NVMe-oF disconnection (e.g., a brief network blip or a shard node reboot):
1. The NVMe-oF initiator detects the connection loss.
2. I/O to the affected shard is queued (or fails fast per `fast_io_fail_timeout_sec`).
3. The initiator retries connection every `reconnect_delay_sec` seconds.
4. If the shard node comes back within `ctrlr_loss_timeout_sec`, the connection is re-established, queued I/O drains, and the EC module sees no state change - the slot remains NORMAL.
5. If the shard node does not come back within `ctrlr_loss_timeout_sec`, `SPDK_BDEV_EVENT_REMOVE` fires, and the slot transitions to FAILED (the permanent failure path described above).

This means a brief network blip causes I/O latency (queued during reconnect) but not slot failure. The `ShardGroupCreate` call must set these timeout values on every `bdev_nvme_attach_controller` invocation to a shard endpoint: `ctrlr_loss_timeout_sec=120`, `reconnect_delay_sec=5`, `fast_io_fail_timeout_sec=15` (matching the V2 replica connection defaults). The engine's NVMe-attach to the ShardGroup endpoint uses the same defaults as a single-replica RAID1 attach - no EC-specific tuning.

#### Shard node reboot vs permanent disk failure

When a shard node reboots (not permanent disk failure):
1. The shard node's InstanceManager restarts and re-exports the shard lvol via NVMe-oF (the lvol data is intact on the node's disk).
2. The engine's NVMe-oF initiator reconnects automatically within the `ctrlr_loss_timeout_sec` window (per the transient failure handling above).
3. **No rebuild is needed** - the shard data is intact. The EC slot remains NORMAL throughout.
4. I/O latency spikes briefly during the reconnect window but recovers without intervention.

The ShardGroup controller should distinguish between "shard node down temporarily" and "shard disk permanently failed" before scheduling a replacement. This is achieved by reusing the existing `replica-replenishment-wait-interval` setting (default: 600 seconds) as a debounce - the controller waits this interval after a shard transitions to FAILED before scheduling a replacement. If the shard recovers (node reboots, NVMe-oF reconnects, slot returns to NORMAL), no replacement is needed. This mirrors the existing V2 replica replenishment behavior.

#### How disk replacement and rebuild work:

When Longhorn create a replacement disk (shard):
1. Longhorn provides a new bdev name via `bdev_ec_replace_base_bdev` RPC. The module opens a bdev descriptor for the new disk and stores it in `ec->descs[]` at the same index as the old failed disk (e.g., if the disk at index 0 failed, the new descriptor goes into `ec->desc[0]`, replacing the old one). The per-disk state for that index changes from FAILED to REPLACING, meaning, the new disk is there and receiving new writes, but hasn't been fully rebuilt with historical data yet. (see `How foreground writes and rebuild interact` below)
1. A per-thread chanel walk (`spdk_for_each_channel`) opens an I/O channels for the new disk on every SPDK thread that has an EC bdev channel. Each EC bdev I/O channel holds one sub-channel per base disk - when the EC moudle needs to read or write a chunk on base disk N, it submits the I/O through that disk's sub-channel (SPDK requires a channel for every I/O submission to track ownership and route completions). The failed disk's sub-channel was set to NULL during cleanup. The walk replaces that NULL with a new sub-channel for the replacement disk.
1. The new disk receives forground writes, but is not used for reads.

The background rebuild then prceeds:
1. A poller processes one stripe per tick (100 µs / `EC_REBUILD_POLL_PERIOD_US = 100`, TODO: make this configurable in future improvement).
1. For each stripe: read k chunks from healthy disk and reconstruct the missing chunk vis ISA-L, then write to the replacement disk.
1. For data disk rebuild, same as degraded read path.
1. For parity disk rebuild, re-encode from all k data chunks.
1. After all stripes for all REPLACING disks are rebuilt, the disk transition to NORMAL and the failure count decremented.

#### How foreground writes and rebuild interact:

During the rebuild, both the foreground I/O path and the rebuild poller write to the REPLACING disk:

- **Foreground writes**: When a new write arrives, the EC module computes parity from all k data chunks and writes to all writable slots (NORMAL + REPLACING). The REPLACING disk receives the new data at the affected stripe, just like every other healthy disk.

- **Rebuild writes**: The rebuild poller reads k chunks from NORMAL (readable) disks, reconstructs the missing chunk via ISA-L, and writes the reconstructed data to the REPLACING disk. One stripe at a time, covering every stripe in the array.

**Correctness analysis**: If a foreground write hits stripe 1000 before the rebuild reaches it, the foreground write updates ALL writable disks - including the NORMAL data disks, NORMAL parity disks, AND the REPLACING disk. When the rebuild later reaches stripe 1000, it reads from the NORMAL disks which already contain the new (post-write) data. The rebuild reconstructs the chunk from the new data and writes it to the REPLACING disk. The result: the REPLACING disk ends up with the correct new data - identical to what the foreground write already put there. The reconstruction is idempotent in this case.

The key insight is that the rebuild reads from NORMAL disks, not from a saved pre-failure snapshot. Since foreground writes update NORMAL disks before (or concurrently with) the REPLACING disk, the NORMAL disks always reflect the latest data. The rebuild therefore always reconstructs from the latest data, never from stale pre-write data.

**Single-reactor-thread serialization guarantee:** The correctness of the foreground-write + rebuild interaction depends on the fact that the rebuild poller and foreground I/O run on the same SPDK reactor thread. SPDK reactors are single-threaded event loops - there is no true concurrency between a rebuild read and a foreground write to the same stripe. If the rebuild reads a stripe, the foreground write to that stripe cannot interleave mid-read; it either completes before the rebuild read starts (rebuild sees new data) or starts after the rebuild read completes (rebuild sees old data, writes old data, then foreground write overwrites with new data). Both orderings produce correct results. This single-thread guarantee eliminates the need for per-stripe locking between rebuild and foreground I/O.

**Performance note**: When a foreground write has already written correct data to the REPLACING disk for a given stripe, the rebuild's read-reconstruct-write cycle for that stripe is redundant - it produces the same result that's already on disk. A future optimization could track which stripes have received foreground writes during rebuild (via a bitmap similar to the RMW stripe dirty bitmap) and skip those stripes in the rebuild poller. This would reduce rebuild I/O but is not required for correctness.

**Replacement disk failure during rebuild**: If the REPLACING disk itself fails during rebuild, `ec_handle_base_bdev_failure` fires immediately via the SPDK event callback, sets the slot back to FAILED (without incrementing `failed_count` - it was already counted as non-NORMAL), and clears `needs_rebuild`. The rebuild's next I/O on that slot fails, causing `ec_rebuild_finish(-EIO)`. The operator can then provide another replacement disk and start the rebuild again from the beginning.

**Additional healthy disk failure during rebuild**: If a *different* NORMAL disk fails while a rebuild is in progress (e.g., in a 4+2 array, slot 3 is REPLACING and slot 1 suddenly fails), the behavior depends on whether the EC bdev is still within its fault tolerance:
- The SPDK event callback `ec_handle_base_bdev_failure` fires for the newly failed slot, transitioning it to FAILED and incrementing `failed_count`.
- If `failed_count <= m` (still within tolerance): the rebuild continues, but now reads fewer NORMAL disks. The rebuild poller's read path uses the same degraded-read logic as foreground I/O - it reads from the first k healthy disks (skipping FAILED slots) and reconstructs missing chunks via ISA-L. Since slot 1 is now FAILED, the rebuild reconstructs *both* slot 1's and slot 3's data from the remaining survivors when processing each stripe. This works as long as at least k disks remain readable (NORMAL or REPLACING-but-not-the-target).
- If `failed_count > m` (exceeded tolerance): the EC bdev transitions to OFFLINE, all I/O is rejected, and the rebuild's next I/O fails with an error, causing `ec_rebuild_finish(-EIO)`. The ShardGroup controller must replace enough failed slots and restart the rebuild from scratch.
- The ShardGroup controller handles the new failure independently: it detects slot 1's transition to FAILED via the next `ShardGroupGet` poll, creates a replacement Shard CR, and queues `ShardGroupShardReplace` for slot 1. However, slot 1's replacement cannot start rebuilding until the current rebuild (for slot 3) completes or fails - `bdev_ec_start_rebuild` rebuilds all REPLACING slots in one pass, so both replacements are handled together if both `ShardGroupShardReplace` calls complete before `ShardGroupShardRebuildStart`.

#### How in-place resize works(same k/m, bigger disks):
In-place resize (`bdev_ec_resize`) handles the case where the underlying base bdev have grown. For example, the controller expanded the lvols on each node's lvstore to use more of the physical disk. The EC bdev's k, m, encoding tables, and data layout are unchanged; only `blockcnt`,`num_stripes`, the dirty bitmap, and WIB arrays are updated.

The resize process is asynchronous:
1. **Validate:** check that no rebuild, scrub, or resize is already in progress, no disk are failed, and the base bdevs have actually grown (new effective size > old effective size after subtracting the 2* strip_size WIB reservation). **Base bdev size consistency:** `bdev_ec_resize` computes the new per-disk capacity as `min(all k+m base bdev sizes) - WIB_reservation`. If some base bdevs are larger than others (e.g., due to partial `ShardExpand` success), the EC bdev uses the smallest common size. The ShardGroup controller ensures all `ShardExpand` calls succeed before calling `ShardGroupExpand` (which is what triggers `bdev_ec_resize` inside the ShardGroup process), so in normal operation all base bdevs have the same size. The min-size behavior is a safety net, not a normal operating mode.
1. **Quiesce:** The EC bdev is quiesced to prevent I/O during geometry update.
1. **Update geometry:** `blockcnt` and `num_stripes` are updated to reflect the new capacity. `spdk_bdev_notify_blockcnt_change` notifies the blobstore layer.
1. **Reallocate WIB arrays:** The new `wib_num_regions` (derived from the new `num_stripes`) may be larger. New arrays are allocated, old dirty bits are copied, and new regions start clean. The old arrays are saved in a temporary `resize_ctx` structure (allocated at the start of the resize operation and freed on completion or rollback) (`TODO: where is saved?`). Answer: `resize_ctx` is a heap-allocated structure created at the start of the resize operation. It holds pointers to the old `wib_region_map` arrays and old geometry values (`old_blockcnt`, `old_num_stripes`, `old_wib_num_regions`). It is freed on successful completion (after unquiesce) or on rollback (after restoring old arrays). The structure lives in the EC bdev's context for the duration of the async resize sequence.
1. **Relocated WIB on disk:** The WIB copies are written to the new tail position on each parity disk. `ec_wib_lba()` computes the new position from the parity disk's update `blockcnt`. Both copies are written synchronously. During this step `wib_persist_pending` is set to prevent the idle poller form concurrently overwriting `wib_buf`.
1. **Reallocate dirty bitmap:** A new stripe dirty bitmap is allocated for the expanded stripe count via `calloc`. If allocation fails (OOM), the geometry is clamped back to the old bitmap's coverage to prevent out-of-bounds access (`TODO: what does this mean? why would there be OOM? isn't it reallocated?`). Answer: OOM can occur because the new bitmap may be significantly larger than the old one (e.g., doubling volume size doubles the number of stripes), and `calloc` returns NULL when the process's heap memory is exhausted. "Clamping" means: the new `blockcnt` and `num_stripes` are reduced to the maximum values the old bitmap can safely track (i.e., `old_bitmap_size * 64` stripes), so the bdev exposes only partial new capacity. This is a degraded-but-safe fallback - the volume grows partially rather than failing entirely. The operator can retry `bdev_ec_resize` to pick up the remaining capacity. Note: the dirty bitmap uses regular heap allocation (`calloc`), not DPDK hugepage memory (`spdk_dma_malloc`). The WIB DMA buffer (`wib_buf`) does use `spdk_dma_zmalloc` because it is used for disk I/O, but the in-memory stripe dirty bitmap does not require DMA-capable memory.
1. **Unquiesce:** I/O resumes with the new geometry (`TODO: what happends to the frontend I/O before quiesce?`). Answer: Frontend I/O submitted during the quiesce window is queued by SPDK's bdev layer (standard quiesce behavior - `spdk_bdev_quiesce` pauses new I/O submission and waits for in-flight I/O to complete). When the bdev unquiesces, all queued I/O is drained automatically. No I/O is lost - the latency spike during the quiesce window is brief (geometry update + WIB write, typically milliseconds).

If the WIB write fails mid-way (e.g., parity disk I/O error), the resize rolls back: geometry reverts to old values, old WIB arrays are restored from `resize_ctx`, new ones are freed (`TODO: so does that mean it will not resize? what happends after roll back?`). Answer: Correct - the resize does not take effect. After rollback, the EC bdev continues operating at the old capacity as if `bdev_ec_resize` was never called. The old WIB is still valid at its original tail position on the parity disks (the failed write was to the *new* tail position, so the old data is untouched). The volume remains fully functional at the original size. The operator can retry `bdev_ec_resize` after addressing the parity disk issue (e.g., replacing the failed disk and rebuilding).

#### How snapshot work with the EC stack:
SPDK's lvol snapshot operation swaps the lvol's identify - the original bdev becomes a read-only snapshot and a new clone becomes the writable head. However, SPDK's implementation handles this transparently: the new writable clone reuses the same internal `spdk_bdev` struct, so the NVMe-oF namespace (which holds a pointer to the bdev struct) continues to point at the writable head automatically. The NVMe-oF initiator sees no disruption - reads and writes continues without remount.

```
NVMe-oF namespace -> lvol (writable head, bdev struct reused after snapshot) -> lvol store -> EC bdev -> [base bdevs]
```

<!-- TODO: Longhorn's V2 existing architecture inserts a RAID1 bdev with a single member between the lvol and NVMe-oF. Need to check if RAID 1 layer is still required for EC-sahrded volumes (e.g., for UUID-based replica tracking at the control plane) -->
The RAID1 layer is retained for EC-sharded volumes as a single-member identity layer. It serves compatibility with the existing engine's replica-tracking logic that identifies volumes by RAID bdev UUID. See the "RAID1 layer disposition" section below for details.

##### EC snapshot implementation: ShardGroup-local lvol operations (not per-shard, not engine-local)

The gRPC `EngineSnapshot*` operations are "unchanged" in API signature and caller semantics, but the implementation for EC volumes **must not** forward to individual shards, and **must not** treat the lvstore as engine-local. In RAID1 mode, snapshot operations are forwarded to each replica node via `ReplicaSnapshotCreate` / `ReplicaSnapshotDelete` etc. (per-replica RPCs). For EC volumes, the snapshot chain lives on the **ShardGroup process**'s lvol store on top of `bdev_ec` - not on any shard node, and not in the engine. The engine forwards `EngineSnapshot*` to `ShardGroupSnapshot*` on the ShardGroup process; the ShardGroup process calls SPDK JSON-RPCs directly on its own local SPDK process:

| Operation | ShardGroup process action | Notes |
|---|---|---|
| `SnapshotCreate` | `bdev_lvol_snapshot(lvol=<ecLvsName>/<volumeName>, snapshot_name=<name>)` | Returns new snapshot UUID; ShardGroup process refreshes `SnapshotMap` |
| `SnapshotDelete` | `bdev_lvol_delete_snapshot(snapshot_name=<ecLvsName>/<snapshotName>)` | ShardGroup process refreshes `SnapshotMap` after deletion |
| `SnapshotRevert` | cross-process: engine tears down its raid1 identity layer; ShardGroup deletes head + clones snapshot + re-exposes; engine reconnects and rebuilds raid1 | `bdev_lvol_snapshot_revert` is not yet available in go-spdk-helper; the clone-based workaround changes the head UUID, requiring engine raid1 + connection teardown and recreation. Requires `Frontend == FrontendEmpty`. ShardGroup process calls `refreshECSnapshotMapNoLock` to rebuild `Head` and `SnapshotMap` after reconstruction. See cross-process call sequence below. |
| `SnapshotPurge` | iterate `SnapshotMap`, delete non-user-created (system) snapshots with ≤ 1 child | SPDK automatically merges a single-child snapshot's data into the child on deletion; threshold `> 1` children means the snapshot is still shared and must be kept. Same `bdev_lvol_delete` path as above (in the ShardGroup process). After each deletion, the in-memory `SnapshotMap` must be kept consistent: the deleted snapshot's single child gets its `.Parent` pointer updated to the grandparent, and the grandparent's `.Children` map is updated (remove deleted entry, add child) - identical to `SnapshotDelete`. **Initial implementation note:** the first cut deletes only orphan snapshots (`0 children`) and does not distinguish user-created from system snapshots — i.e., it is stricter than the description above. Reaching the full `≤ 1 child + non-user-created` semantics requires (a) xattr-based user/system filtering on each snapshot lvol and (b) `SnapshotMap` rewiring after a single-child snapshot's data is auto-merged into its child. Both are tracked as a follow-up refinement; the orphan-only behavior is safe (no chance of accidentally deleting a shared snapshot) but reclaims less space than the full purge. |
| `SnapshotHash` / `HashStatus` / `Clone` | not supported in initial release | Return `Unimplemented`. `SnapshotClone` is deferred: although clone conceptually works at the lvol layer above the EC bdev (see "Clone support for EC volumes"), the cross-volume lvol attach required by the current clone path is not yet implemented for EC. |

**EC `SnapshotRevert` cross-process call sequence (requires `Frontend == FrontendEmpty`):**

Because the lvstore lives in the ShardGroup process and the raid1 identity layer lives in the engine, revert spans two processes. The Volume controller drives the steps in order; each step's target process is shown in the left column:
```
1. Engine     : bdev_raid_delete(name=<volumeName>-raid)
                -> tears down the single-member raid1 identity bdev in the engine.
                -> NVMe-oF frontend is already unexported (FrontendEmpty guard).

2. Engine     : bdev_nvme_detach_controller(name=<shardgroup-controller>)
                -> closes the engine's NVMe-TCP connection to the ShardGroup endpoint.
                -> required because step 3 invalidates the head lvol UUID the engine was using.

3. ShardGroup : bdev_lvol_delete(name=<ecLvsName>/<volumeName>)
                -> deletes the current writable head lvol on bdev_ec.
                -> the target snapshot lvol is unaffected.

4. ShardGroup : bdev_lvol_clone(snapshot=<ecLvsName>/<snapshotName>, clone_name=<volumeName>)
                -> creates a new writable head from the snapshot.
                -> this clone gets a new UUID, different from the deleted head.

5. ShardGroup : nvmf_subsystem_remove_ns(nsid=1)
                -> removes the old head lvol's namespace from the NVMe-oF subsystem.
                -> required because SPDK has no in-place namespace bdev swap RPC; the only
                   way to point the same NQN at a different bdev is remove + re-add.
                nvmf_subsystem_add_ns(bdev_name=<volumeName>, opts.nsid=1)
                -> re-adds at the same NSID with the new head lvol as backing bdev.
                -> ShardGroup keeps the same NQN, transport, and listen address; only the
                   namespace's backing bdev changes.
                -> Note: the engine's bdev_nvme initiator unregisters the local
                   nvmf-shardgroup bdev when it sees the namespace removal AEN
                   (depopulate_namespace -> spdk_bdev_unregister) and creates a fresh
                   local bdev when the re-added namespace AEN arrives. This is why
                   steps 1, 2, 6, 7 (engine raid + connection teardown and rebuild)
                   are mandatory - the engine cannot keep its raid1 base bdev across
                   the namespace transition.
                -> ShardGroup runs refreshECSnapshotMapNoLock to rebuild Head/SnapshotMap.

6. Engine     : bdev_nvme_attach_controller(traddr=<sg-ip>, trsvcid=<sg-port>, subnqn=<sg-nqn>)
                -> reconnects to the ShardGroup endpoint.
                -> the new head lvol UUID is picked up automatically through NVMe namespace discovery.

7. Engine     : bdev_raid_create(name=<volumeName>-raid, raid_level=1,
                                 base_bdevs=[<sg-controller>n1])
                -> recreates the engine-side identity layer on top of the reconnected upstream.
                -> this raid1 has a new UUID (member set "changed" from the engine's perspective
                   even though there is still one member); manager refreshes engine status.
```

The head lvol UUID change in step 4 is what forces the engine raid1 and the engine-to-ShardGroup NVMe connection to be torn down (steps 1-2) and rebuilt (steps 6-7). When `bdev_lvol_snapshot_revert` becomes available in go-spdk-helper (it preserves the `spdk_bdev` struct and UUID at the ShardGroup layer), steps 1, 2, 6, and 7 can be eliminated and the sequence reduces to a single ShardGroup-side `bdev_lvol_snapshot_revert` + `refreshECSnapshotMapNoLock`.

**Contrast with RAID1 `SnapshotRevert`:** In RAID1 mode the engine tears down the raid1 bdev and recreates it after revert because the raid bdev holds direct references to per-replica NVMe bdevs whose head lvol UUIDs change during revert. The cross-process structure is structurally similar - replica processes own the lvols and revert them while the engine tears down and rebuilds raid1 - but for RAID1 there are N replica processes (one per copy) and for EC there is one ShardGroup process. The single-vs-N count is the only meaningful difference at this layer; the sequence above is the EC analog of the existing RAID1 revert pattern.

**`SnapshotMap` and `Head` refresh:** After every snapshot operation, the **ShardGroup process** must refresh its `SnapshotMap` and `Head` by querying its local lvol store. This is the EC analog of `checkAndUpdateInfoFromReplicasNoLock()` used in RAID1 mode (which queries each replica). The ShardGroup process calls `refreshECSnapshotMapNoLock`, which uses `GetBdevLvolMapWithFilter` (a client-side filter over `bdev_get_bdevs`) to list all lvols in its `EcLvsName` and rebuild the snapshot ancestry map and head pointer. `GetBdevLvolMapWithFilter` is used instead of `bdev_lvol_get_lvols` because it also enriches each bdev with xattr data (`UserCreated`, `SnapshotTimestamp`) that is required for correct snapshot map construction. For `SnapshotCreate` and `SnapshotDelete`, the ShardGroup process additionally applies the map change in-memory before returning, avoiding a full SPDK query on the fast path. For `SnapshotRevert` (and ShardGroup-process restart - i.e., the relocated `RecreateEC` path), a full `refreshECSnapshotMapNoLock` query is always performed since the lvol chain is structurally rebuilt. The engine reads the consolidated map from the ShardGroup process via `ShardGroupSnapshotList` rather than maintaining its own.

**`ActualSize`:** For EC volumes, `ActualSize` reflects the actual allocated capacity of the head lvol on the **ShardGroup process's** EC lvol store. The ShardGroup process refreshes it via `refreshECSnapshotMapNoLock` after snapshot operations (set to `newHead.ActualSize`, derived from `NumAllocatedClusters × clusterSize`) and after expand operations by reading the updated head lvol bdev info. The engine surfaces this value through `EngineGet` by querying `ShardGroupGet` rather than computing it locally.

#### How teardown works:
EC bdev teardown happens in the **ShardGroup process** (not the engine), and only on permanent volume deletion (`cleanupRequired=true`). On detach, the ShardGroup process keeps running and bdev_ec is not deleted; this is what preserves the lvstore + head lvol across attach cycles. EC bdev deletion (when it does happen, on volume delete) is asynchronous: when the EC bdev is unregistered, SPDK destroys per-thread I/O channels first (releasing all references on base bdevs), then the completion callback closes base bdev descriptors and frees resources. This order is critical - closing descriptors before channel refs are released causes base bdev deletion to hang indefinitely waiting for refs to drain. The correct teardown sequence is enforced by SPDK's `spdk_bdev_unregister` callback chain: channel destruction happens automatically as part of unregistration, and the module's `destruct` callback only runs after all channels are gone. This is not a bug to resolve - it is SPDK's intended design. The module does not need to do anything special; `spdk_bdev_unregister` handles the sequencing. If the ShardGroup process crashes during teardown, the incomplete teardown is harmless: the EC bdev and its base bdevs are ephemeral SPDK constructs that exist only in the crashed process's memory. On restart (volume re-attach or process restart), the ShardGroup process reconstructs the stack from scratch via `bdev_ec_create` and `bdev_examine` re-discovers the lvstore (the normal crash recovery path; this is the relocated `RecreateEC` semantics).

#### How crash recovery works (end-to-end):

Crash safety for and EC volume is a layered story. The EC bdev is one layer in a stack that includes the filesystem, the application adn the underlying disks - each layer has its own crash-recovery responsibility, and the combined stack is what gives the user a "no data loss for committed transaction" experience. The EC bdev's job is narrow: keep parity consistent with on-disk data so that any future disk failues can be recovered without silent corruption. Everything above EC handles the "my in-flight write didn't make it to disk" problem in its own way.

*The layered crash safety model:*
 
| Layer | What it guarantees after a crash | Mechanism |
|---|---|---|
| Disk sector | Per-sector atomicity (each 4 KB sector is either all-old or all-new, never torn) | Hardware (NVMe atomic write unit) |
| EC bdev | Parity matches whatever data is currently on disk, so future disk failures decode correctly | Write-Intent Bitmap (WIB) + startup scrub |
| Filesystem (ext4/xfs) | Filesystem metadata is internally consistent (inode table, free bitmap, directory entries) | Journal replay at mount time |
| Application (DB, etc.) | Transaction-level atomicity - either the transaction is fully committed or fully rolled back | Write-Ahead Log (WAL) replay at startup |
| User | "My SQL transaction either happened or didn't" | All of the above combined |

Each layer adds its own guarantee on top of the layer below. EC sits in the middle: it doesn't try to recover lost in-flight writes, and it doesn't try to maintain filesystem consistency (that's ext4's job, via the journal). It guarantees exactly one thing: whatever bytes are on the data shards after a crash, the parity shards are consistent with those bytes, the a subsequent disk failure can be recovered without introducing new corruption.

*What kind of crashes the WIB protect?*
The WIB exists to protect against **engine-node crashes that interrupt an RMW mid flight**. Contretly: the engine node's `spdk_tgt` process dies while a sub-stripe write is partway through its read -> encode -> write sequence. After the crash, the data shard may hold a mix of old and new chunks, and the parity shards may old parity computed from yet a diferent combination. Without the WIB, a later disk failure would cause the EC decoder to compute wrong values for the lost chunk, becasue the surviving parity wouldn't match the surviving data.

The WIB is not about disk failures. A disk failure is handled by an entirely different code path: `SPDK_BDEV_EVENT_REMOVE`-> slot translation to FAILED -> degraded reads via Reed-Solomon decode -> eventually hot-swap and rebuild. That path is independent of the WIB and works whether or not an RMW was in flight at the time of failure.

*End-to-end engine-crash recovery walkthrough:*
To make the layering concrete, here is what happens from the moment the engine node crashes until the application reaches a consistent state. Note: in V2 EC, the engine and the ShardGroup process typically run on the same node (co-located by default), so an "engine node crash" usually takes out both processes together. The walkthrough below assumes both go down at once - the recovery sequence handles the more general case.

1. **Engine node crash.** The InstanceManager pod on the engine node dies, taking the engine process and the co-located ShardGroup process with it. All in-memory state in both processes - the RMW state machine, the dirty stripe bitmap, the in-flight bdev_io contexts, the bdev_ec, the lvstore handle, the head lvol handle, the raid1 aggregator - vanishes instantly. The shard nodes (which host the underlying chunk lvols) are unaffected: they are still running, still healthy, still holding whatever bytes had landed on their disks before the engine node died. The lvstore + head lvol metadata is also intact, encoded across the shards.
1. **Application I/O starts failing.** The workload pod's NVMe-oF connection to the engine node is now dead. The kernel's NVMe initiator times out, and the filesystem above starts to see EIO. After enough errors, ext4 typically remounts itself read-only or returns errors to userspace. The application's writes start failing; reads of cached data might still work briefly, but anything hitting the block layer fails.
1. **Longhorn detects the failure.** The Longhorn manager notices the engine node's instance manager is unhealthy and the Volume controller's reconciliation loop kicks in. Depending on the volume's `migratable` configuration, Longhorn either waits for the engine node to come back, or schedules a fresh ShardGroup process and engine on a different healthy node. In both cases, the next steps are: re-bind the ShardGroup process to a new node and reconstruct the bdev_ec stack, then start a new engine.
1. **ShardGroup process re-binds and reconstructs the bdev_ec stack.** The ShardGroup controller observes the failed process (via `Status.ProcessState != Running`) and re-provisions it on the new engine node. The new ShardGroup process re-attaches to all k+m shard lvols via NVMe-oF and calls `bdev_ec_create` with the same k, m, strip size, and base bdev list as before. This is idempotent - the EC bdev has no persistent state of its own beyond what is already on the shards. After bdev_ec is created, SPDK's `bdev_examine` automatically discovers the lvstore + head lvol superblock from the encoded blocks (the lvstore is a property of the bytes on the shards, not of the previous ShardGroup process's memory). The head lvol is re-exposed via NVMe-oF on the new node, and `Status.{IP, Port, NQN, ProcessState=Running}` are updated. The Engine controller then starts a new engine on the new node, which NVMe-attaches to the ShardGroup endpoint and builds raid1(1).
1. **WIB loads (`ec_wib_load`):** Inside `bdev_ec_create`, the EC module synchronously reads the on-disk WIB from all m parity disks, validates each copy (magic, version, CRC), picks the highest-generation valid copy from each disk (each WIB copy has a monotonically increasing `generation` counter incremented on every persist; the module reads both double-buffered copies from each parity disk, discards any with invalid magic/version/CRC, and selects the copy with the highest `generation` value - this is the most recently successfully persisted state), and OR-merges across all parity disks into the in-memory `wib_region_map`. The result: a bitmap of regions that might have had an in-flight RMW at the moment of crash. If no parity disk had a valid WIB (first boot, all-zero disks), the map is all-clean and there is nothing to scrub.
1. **Start scrub (`ec_bdev_scrub_start`):** If any region bit is set, the EC module starts a background scrub poller and the bdev is registered. The bdev becomes immediately available for I/O - the scrub does not block registration. This is intentional: the application should not have to wait for the scrub before it can mount the filesystem. The reasoning is that read correctness is not affected by dirty regions (reads return whatever data is on disk, which is self-consistent per-sector due to NVMe atomic writes), and RMW writes to unscrubbed regions are safely gated by the NOMEM requeue mechanism described in the "Startup scrub" section above. Blocking registration would impose unnecessary downtime on the entire volume for what is typically a small number of dirty regions.
1. **NVMe-oF target re-exported.** Two NVMe-oF exports come up: the ShardGroup process re-exposes the head lvol on top of the rebuilt `bdev_ec`, and the engine re-exports its raid1 frontend on top of the ShardGroup endpoint. The workload node's NVMe initiator detects the engine's new target and reconnects. The block device on the workload node (`/dev/nvme0n1` or equivalent) becomes responsive again.
1. **Pod restart and remount:** In normal cases, the workload pod has been killed by Longhorn. The new pod's `NodeStageVolume` runs mount. Crucially, the new pod is mounting the same volume that the previous pod was using - the PV survives pod restarts, and the filesystem on it (including the journal) survives with it. The journal is a region of blocks inside the filesystem allocated at `mkfs` time (see [ext4 disk layout documentation](https://ext4.wiki.kernel.org/index.php/Ext4_Disk_Layout#Journal_.28jbd2.29)); the journal blocks are stored on the EC bdev as ordinary data blocks within the lvol, persisted on the shard disks which are hosted on shard nodes unaffected by the engine-node crash. When the ShardGroup process restarts and reconstructs the EC stack and the engine restarts and reconnects, the journal blocks are readable again through the rebuilt bdev path. The journal persists across pod, engine, and ShardGroup-process restarts, and contains entries written by whichever pod last had the volume mounted.
1. **Filesystem journal replay:** When `mount` opens the block device, the kernel reads the ext4 superblock and sees that the filesystem was not cleanly unmounted (the previous pod was killed mid-flight; Longhorn does attempt `umount` during graceful pod termination via the CSI `NodeUnstageVolume` call, but in a crash scenario the unmount never happens). It runs `jbd2_journal_recover`: reads the journal blocks, finds the last consistent commit record, and replays committed metadata transactions to bring the filesystem to a consistent state. Transactions that were in flight at crash time but never committed are discarded. After replay, the filesystem is mountable and consistent. Crucially, the journal blocks are read from the EC bdev via the normal read path - and if any of them happens to live in a region that the EC startup scrub hasn't yet completed, the read still succeeds because reads are not gated by scrub progress.
    - What the journal does and does not recover: the ext4 journal replays filesystem metadata changes - inode size and timestamps, block allocation bitmap updates, directory entries, extent tree updates (see [ext4 wiki: Journal](https://ext4.wiki.kernel.org/index.php/Ext4_Disk_Layout#Journal_.28jbd2.29) and [kernel source: fs/ext4/super.c](https://git.kernel.org/pub/scm/linux/kernel/git/torvalds/linux.git/tree/fs/ext4/super.c)), not the content of the user's files. In the default `data=ordered` mode (set by ext4's default mount options; other modes are `data=journal` which journals data blocks too - safer but slower, and `data=writeback` which provides no data ordering - fastest but risks stale data exposure after crash; see [ext4 mount options](https://www.kernel.org/doc/html/latest/filesystems/ext4/index.html)), ext4 orders data writes before committing the metadata that points at them, which ensures the filesystem never points at garbage; but the actual data bytes themselves are not journaled. If a write was in flight at crash time and the data blocks didn't all land, ext4 simply discards the (uncommitted) metadata that would have pointed at them - the file's size stays at its pre-write value, and any partial bytes sitting in those blocks become orphaned (ext4 cleans these up automatically during recovery via the orphan inode list - see `ext4_orphan_cleanup()` in the kernel source). The journal replay's job is to make the filesystem internally consistent (no inode pointing at an unallocated block, no directory entry pointing at a deleted inode, etc.), **not to make the user's data complete**. Recovering the user's lost in-flight write is the application layer's job in step 10.
1. **Application starts to run its own recovery.** With the filesystem mounted, the application process inside the pod opens its data files. If the application is a database or any system with its own crash-safety design, it runs its own recovery layer on top of the filesystem. Applications without their own WAL (caches, build artifacts, ad-hoc files) simply read whatever bytes the filesystem returns. This is true on any storage system, not just EC - the storage contract is "acknowledged writes are durable; unacknowledged writes are best-effort" (see [POSIX fsync semantics](https://pubs.opengroup.org/onlinepubs/9699919799/functions/fsync.html) and the general "write-ahead logging" pattern described in [ARIES: A Transaction Recovery Method](https://cs.stanford.edu/people/chr101/cs345/aries.pdf)).
    The application's recovery determins whether each in-flight operation was committed (reply from the WAL) or uncommitted (discard the partial state). The user querying the application sees a clean transactional outcome: every committed transactoin is durable, every uncommitted transaction is rolled back, and there is never a partial / half state visible at the application level.
1. **EC startup scrub completes in the background.** While steps 8-10 are running, the EC startup scrub poller is grinding through the dirty regions in the background, re-reading data from each stripe and re-encoding parity. Foreground reads to dirty regions go through unchanged; foreground RMW writes to unscrubbed regions are requeued via `SPDK_BDEV_IO_STATUS_NOMEM` (this is SPDK's standard backpressure mechanism - it tells the bdev layer to requeue the I/O and retry later, which is the same status used by other SPDK modules like RAID5F when they need to defer I/O; `EAGAIN` is the internal errno but `NOMEM` is the bdev_io status that triggers automatic requeue) until the scrubber has passed (see "RMW guard" in the scrub section above). When the scrub finishes, the dirty bits are cleared and persisted, and the EC bdev returns to its normal steady state.

**What application sees, in concreate terms:**
Suppose the application wrote `(D0_new, D1_new, D2_new, D3_new)` to one stripe and the engine crashed after only D0 had landed. After the recovery sequence above, the bytes physically on the data shards are `(D0_new, D1_old, D2_old, D3_old)` - the EC startup scrub re-encoded parity to match this state, but it cannot restore D1, D2, D3 to their new values because that data was lost when the engine's RMW context vanished. The application's bytes are partially gone. However, the application also never received an acknowledgment for this write - the EC module completes the bdev_io back to the caller only after all data and parity chunks have been successfully written to disk (the `ec_rmw_write_cb` completion fires only when all k+m chunk writes complete). The engine crashed before `ec_rmw_complete` could fire `spdk_bdev_io_complete`, so the write was in-flight at crash time. This is the same guarantee as the current V2 replication mode: RAID1's `spdk_bdev_io_complete` fires only after all member writes complete, so a crash before completion means the write was unacknowledged in both modes. The contract every storage layer makes is: unacknowledged writes are not guaranteed durable. If the application was a database, the WAL record for this write either:
- Never made it to disk before the crash -> no WAL recorded -> on recovery the database treats the operation as never having happened -> application sees the pre-write state, consistent.
- Made it to disk before the crash -> WAL has the record -> on recovery the database replays the WAL -> re-issue the write through the (now-recovered) EC bdev -> all four chunks land correctly -> application sees the post-write state, consistent.

Either way, the application reaches a consistent state. The "lost" bytes are not really lost from the application's perspective, becasue the application's transactional layer either rolls them back or replays them from high-level log. EC's contribution to this outcome is exclusively step 5-6 and 11: make sure that during the gap between crash and full recovery, parity stays consistent with on-disk data so that is a disk also fails during this window, the EC decoder doesn't introduce new corruption.

**Why a brand-new pod inderit the journal from the previous pod?**
A common point of confusion: if a brand-new pod mounts the volume after the crash, why does it benefit from journal replay? The answer is that the journal is not in the pod - it's on the volume. A Kubernetes pod with a PVC isn't "a pod with its own storage." It's "a pod that temporarily mounts a long-lived storage volume." The PV exists before the pod, persists through the pod's lifeline, and surives after the pod dies. The filesystem on it - including the superblock, inode table, data blocks and journal - is property of the volume, not the pod. When a new pod mounts the same PVC, it sees the same bytes the previous pod saw, including the dirty journal. Ther kernel's mount code doesn't casre which pod wrote the journal entries; it just sees a filesystem that wasn't cleanly unmounted and runs recovery. This is exactly the same as moving a hard drive between physical servers: the new server's `mount` runs journal recovery on the filesystem from the disk, regardless of which machine originally wrote the journal entries. Pod with PVCs are the cloud-native version of that pattern.

**Combined-failure corner case:**
There is one narrow window where the layered model cannot recover the original pre-crash bytes: a crash leaves a region dirty in the WIB, and before the startup scrub finishes that region, a seconf failure (a disk on one of the shards) happens. The EC layer protects against silent corruption in this window by gating I/O rather than reconstructing from possibly-stale parity. There are four guards working together:
1. Degraded reads in dirty regions return EIO immediately.
    `ec_submit_degraded_read` checks `ec_wib_region_is_dirty` before invking ISA-L reconstruction. If the region is still dirty, the read fails with EIO rather than returning bytes derived from parity that may not yet match the on-disk data. The `degraded_read_eio_dirty` counter (exposed in `bdev_get_bdevs`) tracks how often this fires, giving operators a production signal for whether the EIO window is wide enough to warrent per-stripe refinement of the guard.
1. RMW writes to dirty regions during an active scrub are requeued.
    `ec_submit_rmw_write` checks `scrub_ctx`: stripes the scrubber has not yet processed in the current region, and stripe in pending-scrub regions ahead of the scrubber, are blocked with `-EAGAIN`. The bdev layer translates this to `SPDK_BDEV_IO_STATUS_NOMEM`, which SPDK requeues automatically - the write retries once the scrubber has cleared the region.
1. RMW writes during a deferred scrub are also requeued. When `scrub_ctx == NULL` because the scrube was deferred (a data slot was non-NORMAL at boot, so the scrube will only run after the rebuild completes). a degraded RMW would reconstruct the missing data chunk using the stale on-disk parity from the crash, then compute and persist new parity from the wrong data. The deferred-scrub guard (`scrub_ctx == NULL && failed_count > 0` + dirty region) blocks these writes via the same NOMEM requeue path until the rebuild completes and the deferred scrub runs.
1. Full-stripe write are gated against the active scrubber. Without this guard, the scrubber could re-encode parity from old data it read just before a full-stripe write landed, then write that parity over the correct parity the full-stripe write had just installed - producing new data with old parity. `ec_submit_full_write` mirrors the active-scrube region/stripe check from the RMW path and requeues conflicting writes via NOMEM.

The start scrub is also deffered entirely if any data slot is non-NORMAL at bdev creation (`ec_bdev_scrub_start` returns early), and is restarted automatically once a rebuild restore all data slots to NORMAL (`ec_rebuild_finish` invokes `ec_bdev_scrub_start` if any region is still dirty). Dirty region bit remain set across this entire window, so the read and write guards above stay in effect - the deferral check alone is not sufficient; the guards are what makes the deferred period safe.

**What this does not cover.** Stripes that were physically mid-RMW at the crash moment have data and parity bytes on disk that genuinely do no agree, and the EC layer cannot reconstruct that those bytes "should" have been - that information was lost when the engine died. After the fixes the application sees honeset EIO for reads of those stripes until either:
1. the scrube re-encode parity to match whatever daa did reach disk (which makes the stripe self-consistent at whatever value is stablized at, not at the pre-crash intended value)
1. The application overwrites the affected region with afull-stripe write (which installs both new data and new parity atomically from the application's persipective).
    Recovery of the original intended bytes is the responsiblities of higher layers - the filesystem journal, database WAL - which are designed to detect EIO and replay or restore as apppropriate. The EC layer's contract is "no silent corruption", not "no date loss"; those are different problems and only the first is solvable inside the EC module.

### Longhorn Data-plane Implementation Overview

A V2 RAID1 volume's bdev stack is split across two process roles: each Replica process owns a per-disk stack (aio bdev -> lvstore -> head lvol -> NVMe-oF export); the Engine process aggregates the exposed lvols (NVMe-connect x N -> bdev_raid1 -> NVMe-oF export). Persistent state lives in the Replica processes; the Engine process owns no persistent state.

V2 EC volumes follow the same shape, with the **ShardGroup process** playing the role that Replica plays in RAID1. The ShardGroup process owns the bdev_ec construction + lvstore + head lvol + NVMe-oF export; the Engine process NVMe-connects to that single endpoint and aggregates it via `bdev_raid1` (with one base bdev). Persistent state for an EC volume lives in the ShardGroup process; the Engine process owns no persistent state. This symmetry is intentional: the engine code path is topology-agnostic, and persistent-state-owning processes (Replica, ShardGroup) share a common lifecycle discipline including the `cleanupRequired` flag that distinguishes detach (close) from delete.

#### Engine role for EC volumes (clarification)

The engine is **not removed** for EC mode — it is unchanged. What changed is the work allocated to it: the EC-specific machinery (`bdev_ec`, lvstore, head lvol, Reed-Solomon encode/decode, rebuild, scrub, WIB) moved out of the engine and into the ShardGroup process. The engine itself still does everything it does for a RAID1 volume:

| Engine responsibility | RAID1 | EC |
|---|---|---|
| Receives workload-side NVMe-oF traffic | ✓ | ✓ |
| Aggregates upstream NVMe-oF endpoints into a single block device via `bdev_raid1` | ✓ (N replicas) | ✓ (1 ShardGroup endpoint) |
| Frontend management (frontend type, switchover, ublk/dm) | ✓ | ✓ |
| Snapshot / revert / expand orchestration entry-point | ✓ | ✓ |
| Engine-node placement and scheduling | ✓ | ✓ |
| Engine CR + EngineController + InstanceManager engine instance | ✓ | ✓ |

The engine is what the workload pod talks to. Delete the engine and the volume is unreachable — same in both topologies.

What is identical between RAID1 and EC at the engine layer:
- No `if DataLayout == EC` branches in EngineController.
- No `EngineCreateEC` RPC; just `EngineCreate` with `ReplicaAddressMap`.
- No EC-specific fields on `Engine.Spec` or `Engine.Status`.
- Engine teardown calls `bdev_raid_delete` + `bdev_nvme_detach_controller` only — no EC-specific cleanup.

What differs between RAID1 and EC at the engine layer:
- For RAID1, `ReplicaAddressMap` has N entries (one per replica).
- For EC, `ReplicaAddressMap` has exactly 1 entry, pointing at the ShardGroup process's exposed head lvol.

The engine's bdev stack for an EC volume:

```
                NVMe-oF target ← workload pod connects here
                       ↑
                 bdev_raid1   ← aggregates one upstream endpoint (level=1, single member)
                       ↑
                 nvmf-shardgroupn1   ← bdev_nvme_attach_controller's
                                       result: a local view of the
                                       ShardGroup's exposed head lvol
                       ↑
                  ──────── NVMe-oF wire ────────
                       ↓
                 ShardGroup process's exposed head lvol  (separate process; possibly different node)
                       ↓
                  lvstore  ->  bdev_ec  ->  k+m shard endpoints
```

Same shape as a single-replica RAID1, just with the upstream endpoint terminating in a ShardGroup process rather than a Replica process.

Why keep the engine in the picture rather than collapse it for EC:
1. **One stack, one code path.** Keeping the engine in both topologies means the workload-facing layers (frontend, switchover, snapshot dispatch, expansion entry-point) share one implementation. Deleting the engine for EC would create a second code path for everything that's not EC-specific.
2. **Stateless aggregator + persistent-state-owning upstream is a clean separation.** The engine owns nothing durable in either topology; the upstream process owns the lvstore. Engine teardown is always safe; persistent state is always preserved across detach. This structural property is what fixes the EC volume detach data-loss bug — and applying it uniformly to both topologies costs nothing.

#### Bdev stack construction:

EC volume creation involves three process layers: the Shard processes (one per shard, on shard nodes), one ShardGroup process (per volume, typically co-located with the engine), and the Engine process. The ShardGroup process is the new role introduced by EC sharding; it is structurally analogous to a Replica process but its backing bdev is `bdev_ec` instead of an aio file.

```
Layer 1 - Shard processes (k+m total, one per shard node, via ShardCreate):
    Each shard node creates a local lvol and exports it via NVMe-oF, returning its ip, port,
    and NvmfNqn. The control plane stores these in Shard.Status.{StorageIP, Port}.

Layer 2 - ShardGroup process (one per EC volume, via InstanceCreate kind=shardgroup):
    The ShardGroup process is provisioned on ShardGroup.Spec.NodeID (typically the engine node).
    It performs:

    1. NVMe-attach to all k+m shard endpoints:
        bdev_nvme_attach_controller(name="nvmf-slot0", subnqn="nqn..shard-vol0-slot0", ...)
        -> local bdev "nvmf-slot0n1"
        ... (repeated for all k+m slots) ...

    2. Create EC bdev:
        bdev_ec_create(name="<volumeName>-ec",
                       k=4, m=2, stripe_size_kb=64,
                       base_bdevs=["nvmf-slot0n1", ..., "nvmf-slot5n1"])

    3. Create lvol store on top of the EC bdev:
        bdev_lvol_create_lvstore(bdev_name="<volumeName>-ec",
                                 lvs_name="<volumeName>-lvs")

    4. Create head lvol:
        bdev_lvol_create(lvol_name="<volumeName>",
                         size=<volumeSize>,
                         lvs_name="<volumeName>-lvs")

    5. Export head lvol via NVMe-oF:
        nvmf_create_subsystem + nvmf_subsystem_add_ns + nvmf_subsystem_add_listener
        -> ShardGroup.Status.{IP, Port, NQN, LvstoreUUID, HeadLvolUUID} populated

Layer 3 - Engine process (per attach, via EngineCreate):
    The engine consumes the ShardGroup-exposed lvol as a single upstream endpoint.
    Same code path as a single-replica RAID1 engine:

    1. NVMe-attach to ShardGroup endpoint:
        bdev_nvme_attach_controller(name="nvmf-shardgroup",
                                     traddr=ShardGroup.Status.IP,
                                     trsvcid=ShardGroup.Status.Port,
                                     subnqn=ShardGroup.Status.NQN)
        -> local bdev "nvmf-shardgroupn1"

    2. Create RAID1 bdev (single member; identity layer shared with RAID1 mode):
        bdev_raid_create(name="<volumeName>",
                         raid_level=1,
                         base_bdevs=["nvmf-shardgroupn1"])

    3. Export via NVMe-oF (frontend)
```

The Engine layer for EC is structurally identical to a single-replica RAID1 engine **at the SPDK bdev layer** - both topologies attach via NVMe-oF and aggregate via `bdev_raid1`. Where they differ is the **upstream-RPC surface** the engine exercises against the peer SPDK process: RAID1 talks to a Replica (`ReplicaGet`/`ReplicaExpand`/`ReplicaSnapshot*`); EC talks to a ShardGroup (`ShardGroupGet`; `ShardGroupExpand` and `ShardGroupSnapshot*` are driven externally - see below). This dispatch is selected by the `data_layout_type` field on `EngineCreateRequest`. See "Engine layout dispatch" for the abstraction.

##### Engine layout dispatch

The engine and the EC stack live in separate processes (engine vs. ShardGroup process), so the engine's interaction with its upstream is split between two surfaces:

- **bdev surface.** RAID1 and EC are identical here. Both attach via NVMe-oF and aggregate via `bdev_raid1`; the only difference is the number of base bdevs (N for RAID1, 1 for EC). Engine teardown operates exclusively on this surface (`bdev_raid_delete` + `bdev_nvme_detach_controller`) and is therefore layout-blind by construction.
- **upstream-RPC surface.** RAID1 talks to a Replica (`ReplicaGet`/`ReplicaExpand`/`ReplicaSnapshot*`); EC talks to a ShardGroup (`ShardGroupGet`; per-upstream expand and snapshot RPCs do not apply because EC drives those externally — `ShardGroupExpand` from the ShardGroup controller, `ShardGroupSnapshot*` from the engine but as a delegated forward, not as work the engine performs against multiple peers). This is the surface that needs a layout discriminator.

The engine carries an `Upstream` interface with two implementations:

- `replicaUpstream` — RAID1; methods dispatch to `ReplicaGet`/`ReplicaExpand`/`ReplicaSnapshot*` against the upstream's SPDK service.
- `shardGroupUpstream` — EC. The engine constructs one `shardGroupUpstream` per upstream entry in `replica_address_map`, which for EC is exactly one. Method behavior:
  - **Reads** (`Size`, `ActualSize`, `SnapshotLineage`) dispatch to a single `ShardGroupGet` RPC — the engine does not query individual shards.
  - **`Expand()`** is a no-op. `ShardGroupExpand` is driven by the `ShardGroupController` against the ShardGroup-process node, not by the engine. The engine's raid1 picks up the size change automatically via the SPDK `BDEV_EVENT_RESIZE` AEN chain (see "Expand transitional state" above).
  - **`SnapshotCreate` / `SnapshotDelete` / `SnapshotPurge`** are thin forwards (~10 lines each) — `shardGroupUpstream` calls `cli.ShardGroupSnapshot*` against the ShardGroup-process node and returns the result. The lvol-side body executes inside the ShardGroup process; no engine-side bracket is needed because the head lvol UUID is unaffected.
  - **`SnapshotRevert`** is the cross-process sequence described in "EC `SnapshotRevert` cross-process call sequence" above. The engine layer brackets the upstream call with its raid1 teardown (steps 1-2) and reconnect/raid-recreate (steps 6-7); `shardGroupUpstream.SnapshotRevert` issues `cli.ShardGroupSnapshotRevert`, which executes the lvol-side body (steps 3-5: head delete + clone + namespace swap) inside the ShardGroup process. This mirrors how `replicaUpstream.SnapshotRevert` is bracketed by raid1 teardown/recreate in RAID1 mode; the only difference is one upstream peer (EC) versus N (RAID1).
  - **`SnapshotHash` / `HashStatus` / `SnapshotClone`** return `Unimplemented`, matching the initial-release support matrix.
  - **`BackingImageGet`** returns `(nil, nil)`. EC volumes do not surface backing-image state at the engine layer; the equivalent lives in the ShardGroup process if and when it becomes meaningful.
  - **`RebuildingDstShallowCopyCheck`** returns `(false, nil)`. Rebuild is driven entirely inside the ShardGroup process via `ShardGroupShardRebuildStart`; the engine never participates in EC rebuild orchestration.

  **Connection pattern (lazy per-call, matches `replicaUpstream`).** `shardGroupUpstream` stores only the upstream's NVMe-oF transport address (`ip:port`, taken from `replica_address_map`) — no long-lived gRPC connection on the struct. Each method that needs RPC access constructs a fresh `*spdkclient.SPDKClient` against `ip:<spdkServicePort>` (the IM pod's gRPC service port — same convention `Engine.getReplicaClients()` uses for replicas), runs the call, and closes the client. This pattern matches the existing replica path, eliminates reconnect logic, and handles ShardGroup process restart (e.g., engine-node failover) implicitly: the next call opens a new connection against whatever address `replica_address_map` carries at that moment. Snapshot operations are not in the data path, so per-call gRPC connection setup overhead is acceptable.

The engine constructs the appropriate implementation in `EngineCreate`, keyed on `req.data_layout_type`. After construction, engine code iterates `e.upstreams map[string]Upstream` uniformly — there are no `if dataLayout == SHARDED` branches in `engine.go`. The interface concentrates layout-aware behavior into the two implementations.

**Delete-path invariant.** The engine's delete path operates on attached NVMe controllers and the local raid bdev only; it never reads `data_layout_type` and never calls `Upstream` methods. This is what keeps the detach data-loss bug class structurally impossible: with no layout-aware branching in delete, there is no path through which the engine could selectively destroy EC-specific persistent state. The invariant is enforced **structurally**, not by convention:

1. **The `Upstream` interface declares no `Delete()` method.** A reviewer cannot accidentally introduce a layout-aware delete via the interface because there is no method to call. Any future work that needs upstream-aware cleanup has to first add a method to the interface, which is a deliberate design change reviewable in isolation.
2. **The `Engine` struct does not carry `data_layout_type` as a field.** The discriminator lives on `EngineCreateRequest` and is consumed by `server_engine.go` to pick the upstream implementation; once `e.upstreams` is populated, the engine never re-reads the layout. There is nothing for `Engine.delete()` to dispatch on.
3. **`Engine.delete()` references neither `e.upstreams` nor `data_layout_type`.** The body calls `bdev_raid_delete` + `bdev_nvme_detach_controller` directly; neither operation goes through the `Upstream` abstraction. This is enforced at compile time once (1) and (2) hold — the abstraction has no delete entry-point and the struct has no layout field.

The original detach data-loss bug existed because the engine ran an EC-aware teardown (`bdev_lvol_delete_lvstore` on the lvstore living on `bdev_ec`). Under this design the engine cannot run EC-aware teardown — the EC-aware teardown lives in the ShardGroup process and is gated by its own `cleanup_required` flag, only fired on volume delete (not detach).

**Why not push layout into `SpdkInstanceSpec`?** The instance-manager's `InstanceDelete` path reads `SpdkInstanceSpec`. Putting the discriminator on the spec would create a channel through which layout-aware behavior could leak into delete code. Carrying the discriminator one level up — on `InstanceCreateRequest` (top-level, not nested inside `SpdkInstanceSpec`) and forwarded to `spdkrpc.EngineCreateRequest` — keeps the spec layer layout-uniform and the delete path layout-blind, while still letting the engine know its layout for create/validate/expand/info dispatch.

**Why an interface and not an enum + helper?** The number of upstream-RPC call sites is large (~10 — `Get`, `Expand`, `SnapshotCreate/Delete/Revert/Purge/Hash`, `BackingImageGet`, `RebuildingDstShallowCopyCheck`, ancestor resolution). An enum + per-site switch statement creates a maintenance burden where each new upstream-touching feature must remember to add a switch arm, and Go enums lack exhaustiveness checking. An interface forces both implementations to satisfy the full method set at compile time, and concentrates "what does layout mean here?" into the implementations rather than the call sites.

**Expand transitional state (initial release).** The unified target is: `Engine.Expand(size)` calls `u.Expand(size)` on each upstream (RAID1: `ReplicaExpand`; EC: no-op), then polls the engine-side raid bdev `blockcnt` until it reflects the upstream resize via SPDK's `BDEV_EVENT_RESIZE` AEN chain (NVMe-oF target -> `bdev_nvme` initiator -> `raid1_resize`). This works for both layouts and is the c6b destination. For the initial release (c6a) only EC takes the AEN-poll path; RAID1 keeps its current tear-down/expand-replicas/reconstruct cycle. The dispatch lives in `server_engine.go` (calls `engine.ExpandViaAEN(size)` for EC, `engine.Expand(size)` for RAID1, keyed on `req.data_layout_type`) — not inside `Engine` itself, so `Engine`'s no-`data_layout_type`-field invariant is preserved. c6b removes the dispatch and consolidates both topologies onto `engine.ExpandViaAEN` once the AEN chain is verified reliable on the SPDK version this branch ships against, and once the RAID1 expand E2E test runs against the consolidated path. Until c6b lands, the temporary server-layer branch is the only place the engine's expand path knows about layout.

**Lvstore residency.** For an EC volume the lvstore + head lvol live on `bdev_ec` inside the **ShardGroup process** - not in the engine. This is the same pattern as RAID1, where the lvstore + head lvol live on `aio` bdevs inside each Replica process. In both topologies, the engine is a stateless aggregator and persistent state lives in the upstream lvstore-owning process(es). For EC, the lvstore's bytes are encoded across the shards (because they are written through `bdev_ec`), so the lvstore survives any combination of disk failures up to `m`; the ShardGroup process is a runtime materialization of that distributed state, not its owner. ShardGroup process death is recoverable (restart, reconnect shards, re-build bdev_ec, `bdev_examine` discovers the lvstore from the encoded blocks - the same `RecreateEC` semantics described later, just relocated from the engine).

**Snapshot/clone/backup operations** still work at the lvstore layer above bdev_ec - they execute in the ShardGroup process rather than the engine process. The shard lvols below bdev_ec are raw chunk storage and are not individually snapshotted; you cannot independently snapshot k+m chunks whose data is striped/parity-encoded.

**Partial failure and idempotent teardown:** Both the ShardGroup process create (`InstanceCreate` kind=shardgroup -> NVMe-attach k+m + bdev_ec + lvstore + head lvol + NVMe-oF expose) and the Engine create (NVMe-attach + raid1 + NVMe-oF expose) are multi-step. If any step fails mid-sequence, the corresponding `InstanceDelete` (with `cleanupRequired=true` in the create-failure path, since no successful state needs preserving) must clean up the partial stack. Both `ShardGroup.Delete` and `Engine.Delete` check whether each layer exists before tearing it down; this mirrors the existing replica/engine teardown pattern.

**Remote base bdev provisioning (cross-node EC)**
When shards span multiple nodes, each shard node exports its shard lvol as an NVMe-oF subsystem - the same model as V2 replicas. This is handled by `ShardCreate` on the shard node, which calls `StartExposeBdev` and returns ip, port, and NvmfNqn to the control plane. The control plane stores `ip:port` in `Shard.Status.storageIP`/`port` for use as `EcShardInfo.Address`, and stores `NvmfNqn` in `Shard.Status.nvmfNqn` for observability (the engine derives the NQN locally from the shard name via `GetNQN` and does not need it passed back).

The sequence for a shard on node B, consumed by the **ShardGroup process on node A** (typically the engine node):
```
Node B (Shard node - via ShardCreate gRPC):
    1. bdev_lvol_create(lvs_uuid=..., name="shard-vol0-slot3", size=...)
    2. nvmf_create_subsystem(nqn="nqn.longhorn:shard-vol0-slot3")
    3. nvmf_subsystem_add_ns(nqn="...", bdev_name="lvs0/shard-vol0-slot3")
    4. nvmf_subsystem_add_listener(nqn="...", traddr=<node-B-IP>, trsvcid=4421)
    -> returns ip, port, NvmfNqn to caller

Node A (ShardGroup-process node - internally, as part of ShardGroupCreate):
    1. bdev_nvme_attach_controller(name="nvmf-slot3",
                                   trtype="tcp",
                                   traddr=<node-B-IP>,
                                   trsvcid=4421,
                                   subnqn="nqn.longhorn:shard-vol0-slot3")
    -> produces local bdev "nvmf-slot3n1" passed to bdev_ec_create
```
The control plane passes the ip:port address (from `Shard.Status.storageIP`/`port`, populated by `ShardCreate`) to `ShardGroupCreate` via `shards` (a map of `EcShardInfo{address, slot_index}`). The map key is the Shard CR external name (`<volumeName>-<slotIndex>`, e.g. `vol0-3`); the ShardGroup process derives the NVMe controller name internally via `GetShardName(volumeName, slotIndex)` and derives the role from `slot_index < k` - the caller never handles bdev names directly. The engine, on its side, receives a single upstream endpoint (the ShardGroup's exposed lvol) and connects to it exactly the way a single-replica RAID1 engine connects to its one replica. The EC module on node A is transport-agnostic; each shard's NVMe-oF bdev is indistinguishable from a local bdev once attached.

#### gRPC API surface
The gRPC methos that wraps the EC JSON-RPCs (new `Shard*` methods and extended/new `Engine*` methods on `spdkrpc.SPDKService`) are degined in the API Changes section above. 

#### Health event propagation

The EC module handles disk failures internally (SPDK_BDEV_EVENT_REMOVE -> state transition -> async cleanup) inside the **ShardGroup process** (which owns `bdev_ec`). The ShardGroup process detects state changes by polling `bdev_ec_get_bdevs` periodically (e.g. every 5 seconds) or on demand when the instance manager queries health via `ShardGroupGet`. The polling response includes per-slot state (NORMAL/FAILED/REPLACING), `failed_count`, `offline` flag.

When the ShardGroup process detects a slot transition to FAILED, it surfaces the event via `ShardGroupWatch` (the long-lived stream consumed by the instance manager's `watchSPDKShardGroup` goroutine), which forwards it to the longhorn-manager. The ShardGroup controller's `syncECHealth` reads the updated state from `ShardGroupGet` on the next reconcile tick and triggers the failure-recovery path (see control-plane section).

An alternative to polling is for the ShardGroup process to register a callback on `spdk_bdev_notify_event` for `bdev_ec` and push state changes immediately. This reduces detection latency from seconds to milliseconds but requires a new notification path in the ShardGroup process's event loop.

**Engine `ValidateAndUpdate` is topology-agnostic:** The engine's existing `ValidateAndUpdate` periodic sync inspects only its own `bdev_raid1` and per-upstream NVMe connections - the same code path for both RAID1 (N replica connections) and EC (1 ShardGroup connection). It marks the engine as `InstanceStateError` when its raid1 aggregator is degraded beyond tolerance (which, for EC, means the single ShardGroup endpoint is unreachable). Per-slot EC health (`failed_count`, slot states) is **not** observed by the engine - it is observed by the ShardGroup process and exposed via `ShardGroupGet`. There is no `validateAndUpdateECNoLock` branch in the engine; the engine no longer carries an `EcShardStatusMap` field and does not call `bdev_ec_get_bdevs`.

#### Shard status propagation (control-plane ↔ data-plane)

The full data flow for shard health status from the SPDK layer to Shard CRs is:

```
SPDK EC module inside the ShardGroup process (SPDK_BDEV_EVENT_REMOVE -> slot state transition)
  ↓
ShardGroup process polls bdev_ec_get_bdevs (every ~5s) -> detects per-slot state changes
  ↓
ShardGroupGet gRPC (called by ShardGroup controller against the ShardGroup-process node)
  -> returns: ec_status.slots[] with {index, role, state, bdev_name}, failed_count, offline
  ↓
ShardGroup controller reads ShardGroup-process status (via ShardGroupGet)
  ↓
ShardGroup controller maps slot states to Shard CRs:
  - For each slot in ShardGroupGet response:
    - Look up Shard CR by slotIndex in ShardGroup.status.shardRefs
    - Update Shard.status.state to match the EC slot state (NORMAL->normal, FAILED->failed, REPLACING->replacing)
    - Note: SPDK REPLACING maps to Shard CR `replacing` unconditionally, regardless of whether a
      rebuild is active. Whether a rebuild is running is expressed at the ShardGroup level via
      ShardGroup.Status.RebuildInProgress - callers should read that field, not Shard.Status.State,
      to distinguish "replacing, waiting for rebuild" from "replacing, rebuild in progress".
  ↓
ShardGroup controller aggregates Shard states -> updates ShardGroup.status:
  - state: healthy (all normal) | degraded (1..m failed) | rebuilding (RebuildInProgress) | offline (>m failed)
  - failedCount: count of non-normal slots
  - rebuildInProgress: true while SPDK reports an active rebuild poller
```

The ShardGroup controller is the single owner of Shard CR status updates. No separate Shard controller is needed because the authoritative slot state lives in the EC bdev on the engine node (not on the shard nodes). The shard nodes only know about their local lvol health - they cannot observe EC-level slot states (FAILED, REPLACING) which are determined by the engine's EC module. The ShardGroup controller bridges this by reading the engine's aggregated view and distributing it to individual Shard CRs.

#### Startup and crash recovery:

**Shard node recovery:** On SPDK server or IM pod restart, the in-memory `shardMap` is empty but shard lvols persist in the blobstore. The SPDK server's monitoring loop calls `rebuildCachedLvolObjects` on each tick, which scans all live lvol bdevs and rebuilds the in-memory cache. Shard recovery follows the same pattern as replica recovery:

1. `IsProbablyShardName(lvolName)` detects shard lvols by the `^shard-.+-\d+$` naming pattern.
2. `ParseShardName(lvolName)` extracts `volumeName` and `slotIndex` from the name. The last `-`-separated token is the numeric slot index; everything before it is the volume name (which may itself contain hyphens).
3. A `NewShard` is constructed with `lvsName`, `lvsUUID`, and `sizeBytes` from the bdev info. `lvsName` and `lvsUUID` are resolved directly from the bdev's lvstore - no `diskMap` lookup or `lvsUUIDToDiskID` map is needed because `ShardCreate` now carries `lvs_name`/`lvs_uuid` directly (same pattern as replica). The `UUID` field is pre-populated from `bdevLvol.UUID`. The shard is added to both `state.shardMap` and `state.shardMapForSync`.
4. `verifyState` carries both a `shardMap` field and a `shardMapForSync` field. `newVerifyState` seeds both from `s.shardMap` (existing known shards), and `rebuildCachedLvolObjects` appends newly detected shards to both. After `rebuildCachedLvolObjects`, `verify()` atomically replaces `s.shardMap` under lock, then calls `syncVerifiedObjects`.
5. `syncVerifiedObjects` iterates `state.shardMapForSync` and calls `Shard.Sync(spdkClient)` on each entry - parallel to the replica sync loop. `Shard.Sync` mirrors `Replica.Sync`'s passive model: it never re-allocates a port and never re-exposes the bdev. Behavior depends on the shard's current `State`:
    - **`Pending`** (newly discovered by `rebuildCachedLvolObjects` after IM restart): walks to `InstanceStateStopped` (mirrors `Replica.construct`). The ShardGroup controller's existing failure-recovery path handles re-provisioning - the ShardGroup process separately observes the NVMe-oF disconnect and reports the slot as FAILED via `ShardGroupGet`, which `reconcileFailedShard` then replaces on a new node.
    - **`Running`** (in-process shard, normal heartbeat): fetches `subsystemMap` and validates that the live exposed port matches `s.Port`. Subsystem missing when `IsExposed=true`, subsystem present when `IsExposed=false`, or port drift all transition the shard to `InstanceStateError`.
    - **other** (`Stopped`, `Error`, `Terminating`): no-op.

    **Why uniform passive recovery, not in-place re-expose:** A previous design re-allocated a port and called `StartExposeBdev` whenever the subsystem was missing during sync. That model required reserving the recovered port back into the in-process bitmap on the discovery side, but `go-common-libs/bitmap` exposes only `AllocateRange`/`ReleaseRange` - it has no method to mark a specific port in-use. More fundamentally, "subsystem alive" is a fragile proxy for data integrity. Replicas avoid both problems by always going Pending -> Stopped -> replaced; shards adopt the same uniform model. The trade-off is that an IM-only restart triggers an EC rebuild for every shard on that node even when the underlying SPDK subsystem was still live - in the typical Longhorn deployment IM and SPDK are co-located in the same pod, so this case is rare or non-existent, and EC rebuild is a first-class supported operation regardless.

**ShardGroup process recovery:** On ShardGroup process restart (node reboot, IM crash, engine-node failover), the **ShardGroup process** - not the engine - reconstructs the EC stack. The Longhorn manager triggers recovery by calling `ShardGroupCreate` with `salvage_requested=true`; the SPDK server dispatches this to the relocated `RecreateEC` semantics inside the ShardGroup process instead of `CreateEC`. The sequence differs from fresh creation because the lvol store and head lvol already exist as persistent blobstore structures on `bdev_ec` (their bytes are encoded across the surviving shards):

1. The ShardGroup process reconnects to all k+m shard NVMe-oF exports (`bdev_nvme_attach_controller` × k+m). Unreachable slots are passed as the empty string `""` so the EC module marks them FAILED but proceeds.
1. The ShardGroup process calls `bdev_ec_create` with the same k, m, strip_size, and base bdev list. Internally, `ec_bdev_create` calls `ec_wib_load` to read the persisted write-intent bitmap from parity disks and `ec_bdev_scrub_start` to scrub any regions that were mid-write at crash time.
1. SPDK's bdev examine mechanism auto-discovers the existing blobstore superblock on the EC bdev and auto-imports the lvol store and its lvols - no `bdev_lvol_create_lvstore` or `bdev_lvol_create` is needed (calling them would fail because the blobstore already exists). **Ordering assumption:** The EC bdev must be fully initialized (WIB loaded, scrub started, bdev registered) before the blobstore examine runs. This is guaranteed by SPDK's design: `bdev_ec_create` completes WIB load and scrub initialization synchronously before calling `spdk_bdev_register`, and `spdk_bdev_register` triggers the bdev examine mechanism synchronously as part of registration. This ordering must be preserved - if `bdev_ec_create` is ever made asynchronous in the future, the blobstore examine could race with WIB scrub and attempt to read metadata from stripes whose parity is still inconsistent.
1. The ShardGroup process re-exposes the discovered head lvol via NVMe-oF on the new node and populates `ShardGroup.Status.{IP, Port, NQN, ProcessState=Running, LvstoreUUID, HeadLvolUUID}`.

The ShardGroup process monitors scrub progress via `bdev_ec_get_scrub_progress` and reports the array as "degraded (scrubbing)" via `ShardGroupGet` until scrub completes; the engine surfaces the same value through `EngineGet`.

**Engine recovery:** Once the ShardGroup process has re-exposed the head lvol, engine recovery is structurally identical to single-replica RAID1 engine startup. The Volume controller sets `e.Spec.DesireState = Running` on the engine CR; the engine `bdev_nvme_attach_controller`s to the ShardGroup endpoint, calls `bdev_raid_create` with that one upstream as the only base bdev, and re-exports the NVMe-oF frontend. There is **no engine-side `bdev_ec_create`, no engine-side lvstore reconstruction, and no `EngineCreate` salvage path** - the engine has no persistent state to salvage. Salvage is a property of the ShardGroup process layer only.

If a data disk was FAILED at the time the ShardGroup process restarted (no valid base bdev to open), the ShardGroup process creates the EC bdev with k+m-1 base bdevs. The EC module tolerates this: `ec_open_base_bdevs` is called with the available disks and the missing slot's state is set to FAILED. The startup scrub is deferred until the disk is replaced and rebuilt (the scrub requires all k data disks to be NORMAL). The ShardGroup process reports the missing slot to the ShardGroup controller via `ShardGroupGet` for replacement scheduling.

**Missing slot convention for `bdev_ec_create`:** When creating or recreating an EC bdev with a missing slot, the caller passes an empty string `""` at the missing slot's position in the `BaseBdevs` array. The EC module's `ec_open_base_bdevs` skips the empty-string entry at that index and marks the slot as FAILED. Example:
```
bdev_ec_create(
    name="ec0", k=4, m=2, strip_size_kb=64,
    base_bdevs=["nvmf-slot0n1", "", "nvmf-slot2n1", "nvmf-slot3n1", "nvmf-slot4n1", "nvmf-slot5n1"]
)
// slot 1 is missing -> ec_open_base_bdevs skips index 1, marks it FAILED
```
This convention preserves slot-index alignment - the array position always equals the slot index. The corresponding `BdevEcCreateRequest.BaseBdevs` field in `go-spdk-helper` follows the same convention.

**Why slot order in `BaseBdevs` is immutable.** The array position passed to `bdev_ec_create` (inside the ShardGroup process) is the slot assignment for the volume's entire life (see "Slot index permanence" in the data layout section). The ISA-L encode matrix is built once from these positions; every stripe on disk encodes the chunk at slot `i` using the coefficients in row `i` of that matrix. The Shard CR's `slot_index` field is the authoritative record of this assignment. It must be persisted at `ShardCreate` time and echoed verbatim to every `ShardGroupCreate` and `ShardGroupShardReplace` call. If the ShardGroup controller loses or re-derives `slot_index` incorrectly (e.g., by re-sorting shards), a subsequent ShardGroup process start will pass the wrong base bdev at some slot, silently reading and writing corrupt data.

#### RAID1 layer disposition
The engine's `bdev_raid1` layer is unchanged between RAID1 and EC modes - it is the engine's standard aggregation layer. RAID1 mode uses N base bdevs (N = replica count, e.g., 3); EC mode uses 1 base bdev (the ShardGroup process's exposed lvol). The single-member RAID1 in EC mode is **not** a special-purpose identity wrapper; it is the same engine code path RAID1 uses, with `base_bdevs` of length 1 because EC's redundancy is provided by `bdev_ec` inside the ShardGroup process rather than by raid1. No EC-specific raid1 handling is required in the engine. If a future refactor allows the engine to expose a single base bdev directly without the raid1 wrapper, it can be removed for both topologies (RAID1 with replicaCount=1 has the same opportunity).


### Longhorn Control-plane Implementation Overview

For EC volumes, introduce ShardGroup and Shard CRs and controllers for failure recovery, rebuild and grow.

#### CRD layering and responsibilities

The control-plane uses five CRDs for an EC volume. The first three (Volume, Engine, EngineFrontend) are pre-existing and remain **EC-agnostic** - they have zero EC-specific fields and treat an EC volume identically to a V2 RAID1 volume. The two new CRDs (ShardGroup, Shard) carry all EC-specific state. The ShardGroup CR additionally owns a long-lived SPDK process (the **ShardGroup process**) which holds the EC volume's lvstore + head lvol, mirroring the role a Replica process plays for a RAID1 volume.

```
Volume                user intent + lifecycle
  │                   - Spec.DataLayout (canonical EC config)
  │                   - Spec.Size, Spec.Frontend, Spec.AccessMode, Spec.Encrypted, ...
  │                   - Status.State, Status.Robustness, Status.CurrentSize
  │
  ├─ Engine           per-attach engine instance on a node
  │                   - Spec.NodeID, Spec.Image, Spec.DesireState, Spec.VolumeName
  │                   - NO EC-specific fields; for EC volumes consumes a single
  │                     upstream endpoint (the ShardGroup process's exposed lvol),
  │                     identical in shape to a single-replica RAID1 setup
  │
  ├─ EngineFrontend   workload-side NVMe-oF / ublk initiator (V2-only)
  │                   - Frontend lifecycle, switchover, frontend-side expansion
  │                   - Completely EC-agnostic
  │
  └─ ShardGroup       EC orchestration root + lvstore-owning process (one per EC volume)
       │              - Spec: VolumeName, DataChunks, ParityChunks, StripSizeKB,
       │                NodeID, InstanceManagerName
       │                (NodeID identifies the node hosting the ShardGroup process,
       │                 typically co-located with the engine)
       │              - Status.ecShardAddressMap (canonical slot->address map for shards)
       │              - Status process fields: ProcessState, IP, Port, NQN,
       │                LvstoreUUID, HeadLvolUUID (the exposed lvol the engine consumes)
       │              - Status.State, FailedCount, RebuildInProgress, EvictingSlots, ...
       │
       └─ Shard × (k+m)   per-slot lvol + NVMe-oF target on a shard node
                          - Spec: SlotIndex, Size, NodeID, DiskUUID, EvictionRequested
                          - Status: State (normal/failed/replacing), Role,
                            StorageIP, Port, RebuildProgress, LastFailureTimestamp
```

| CR | Owns spec | Owns status | EC-specific? | Written by |
|---|---|---|---|---|
| Volume | user intent (`DataLayout`, size, frontend, ...) | volume-level state, robustness | partial (`Spec.DataLayout`) | CSI / user, VolumeController |
| Engine | engine instance (node, image, desire state) | engine runtime state, current size | **no** | VolumeController (spec), EngineController (status) |
| EngineFrontend | initiator-side desire (V2 only) | frontend runtime state | **no** | VolumeController (spec), EngineFrontendController (status) |
| ShardGroup | k/m/strip_size, NodeID, InstanceManagerName | `ecShardAddressMap`, process state + exposed endpoint, aggregated health, eviction tracking | **yes** | VolumeController (spec creation + `NodeID`), ShardGroupController (status, finalizer, process lifecycle) |
| Shard | slot index, size, placement | per-slot state, instance address | **yes** | ShardGroupController (status, owner), scheduler (placement spec), node controller (eviction flag) |

Three design principles fall out:

1. **Generic CRs stay generic.** Engine and EngineFrontend have no EC-specific spec or status fields. The Engine CR for an EC volume looks structurally identical to a single-replica RAID1 Engine - both consume one upstream NVMe-oF endpoint and aggregate it via `bdev_raid1`.
2. **EC plumbing concentrates in ShardGroup + Shard.** All EC orchestration state, all per-slot state, all scheduling, all rebuild logic lives there. The ShardGroup CR additionally owns the lvstore-bearing process, which is the single durable holder of the EC volume's lvol metadata.
3. **Engines own no persistent state, in either topology.** A RAID1 engine aggregates per-replica lvols (each lvol's persistent state lives on its replica's aio bdev). An EC engine aggregates one ShardGroup process's lvol (whose persistent state lives encoded across the shards via bdev_ec). In both cases, engine teardown can never destroy persistent state because the engine doesn't own any.

##### Linkage between CRs (no explicit pointer fields)

The Volume -> Engine -> ShardGroup -> Shard graph is wired together by **naming convention** and `Spec` back-references, not pointer fields:

- `Engine.Spec.VolumeName == Volume.Name`
- `ShardGroup.Name == Volume.Name` (1:1, immutable, load-bearing)
- `ShardGroup.Spec.VolumeName == Volume.Name` (back-ref)
- `Shard.Spec.ShardGroupName == ShardGroup.Name`

The 1:1 `ShardGroup.Name == Volume.Name` invariant is what allows the Engine controller's `CreateInstance` to look up the ShardGroup directly:

```go
sg, _ := ds.GetShardGroupRO(engine.Spec.VolumeName)  // same name as the volume
```

without an explicit `Engine.Spec.ShardGroupName` field. **The webhook validates `ShardGroup.Spec.VolumeName == ShardGroup.Name` on `CREATE`** to make the convention enforced rather than merely conventional.

#### New CRD

**ShardGroup:** one per EC volume, analogous to the set of Replicas for a RAID1 volume.

**Ownership and garbage collection:** The CR ownership chain is Volume -> ShardGroup -> Shard, using Kubernetes owner references for observability. Deletion is **not** driven by Kubernetes cascade GC - it is sequenced explicitly by the Volume and ShardGroup controllers using finalizers, matching the existing Volume -> Engine -> Replica pattern:

1. Volume controller stops and marks the Engine for deletion; waits for all Engine CRs to be gone.
2. Volume controller calls `DeleteShardGroup` only after engines are confirmed gone - this ensures the EC engine disconnects from all shard NVMe-oF connections before shard teardown begins.
3. ShardGroup controller's deletion path (`DeletionTimestamp != nil`) calls `cleanupDeletedShards` first (tears down SPDK shard instances and removes Shard finalizers), then `cleanupShardGroup` (marks any remaining Shard CRs for deletion and waits for all Shard CRs to be fully removed before removing the ShardGroup finalizer).
4. Volume controller waits for the ShardGroup CR to be gone, then removes the Volume finalizer.

The ShardGroup CR name equals the Volume CR name (`v.Name`). This allows every controller that holds the volume name to derive the ShardGroup name without a separate lookup.

```yaml
apiVersion: longhorn.io/v1beta2
kind: ShardGroup
metadata:
    name: vol0              # equals Volume CR name
    labels:
        longhornvolume: vol0
    ownerReferences:
        - apiVersion: longhorn.io/v1beta2
          kind: Volume
          name: vol0
          uid: <volume-uid>
    finalizers:
        - longhorn.io/shardgroup-cleanup
spec:
    volumeName: vol0
    dataChunks: 4              # k
    parityChunks: 2            # m
    stripSizeKB: 64
    nodeID: ""                 # set by Volume controller at attach time (typically = Engine.Spec.NodeID for co-location)
    instanceManagerName: ""    # set by ShardGroup controller during process provisioning; the IM hosting the ShardGroup process
status:
    ownerID: ""                # ID of the node that owns this ShardGroup CR (matches NodeID in steady state)

    # Aggregated EC orchestration state (existing)
    state: healthy             # healthy | degraded | offline | rebuilding | growing
    failedCount: 0
    shardRefs:                 # ordered list, index = slot number
        - vol0-0
        - vol0-1
        - vol0-2
        - vol0-3
        - vol0-4
        - vol0-5
    ecShardAddressMap:         # canonical slot->address map; populated from healthy Shard CRs by syncStatus
        "0": 10.0.0.10:20011
        "1": 10.0.0.11:20011
        "2": 10.0.0.12:20011
        "3": 10.0.0.13:20011
        "4": 10.0.0.14:20011
        "5": 10.0.0.15:20011
    wibDirtyRegion: 0
    scrubInProgress: false
    rebuildInProgress: false
    growInProgress: false

    # ShardGroup process state and exposed endpoint (new)
    processState: Running      # Pending | Running | Stopped | Error | Restarting
    ip: 10.0.0.20              # IM pod's storage network IP on Spec.NodeID
    port: 20100                # NVMe-oF port allocated for the head lvol export
    nqn: nqn.longhorn:shardgroup-vol0
    lvstoreUUID: <uuid>        # bdev_lvol_create_lvstore returned UUID; observability/debug
    headLvolUUID: <uuid>       # head lvol UUID
```

`ip`, `port`, `nqn` together form the upstream endpoint that the engine NVMe-attaches to. The Engine controller's `CreateInstance` for an EC volume reads these three fields from `ShardGroup.Status` and constructs a `replica_address_map` with one entry, identical in shape to a single-replica RAID1 setup.

The ShardGroup CR uses a **finalizer** (`longhorn.io/shardgroup-cleanup`) to ensure the ShardGroup process is torn down (with `cleanupRequired=true`) and the lvstore + head lvol are deleted before the CR is garbage collected. The ShardGroup controller removes the finalizer only after `InstanceDelete` confirms the process has been torn down. The finalizer parallels the Replica CR's finalizer in RAID1.

**Shard:** one per base device slot (k+m per volume)

The `role` field is not stored in `spec` because it is a pure function of `(slotIndex, k)`: indices 0..k-1 are DATA, k..k+m-1 are PARITY. Storing it in spec would create a consistency risk if `slotIndex` and `role` disagree. Instead, `role` is computed by the ShardGroup controller (`slot_index < dataChunks ? DATA : PARITY`) and placed in `status` for informational purposes. Any consumer can derive it from `slotIndex` and the ShardGroup's `dataChunks`.

**Note on `spdkrpc.Shard.role`:** The gRPC `Shard` message (returned by `ShardGet`/`ShardList`) contains a `role` field (`EcSlotRole`). The shard-node SPDK service cannot populate this field because `ShardCreateRequest` deliberately omits k — the shard node creates a raw lvol that behaves identically regardless of data or parity role. The gRPC `Shard.role` field is therefore always `EC_SLOT_ROLE_DATA` (proto3 zero value) in every response. Implementors must not read `spdkrpc.Shard.role` for authoritative role information; use the ShardGroup controller's computed value in `Shard CR status.role` instead.

Shard CRs use a **finalizer** (`longhorn.io/shard-cleanup`) to ensure the shard node tears down the NVMe-oF export and deletes the shard lvol before the CR is garbage collected. This mirrors the existing Replica CR finalizer pattern. The ShardGroup controller removes the finalizer (via `cleanupDeletedShards`) only after `InstanceDelete` confirms the shard's SPDK instance has been torn down on the shard node.

Shard CR names are slot-deterministic: `<shardGroupName>-<slotIndex>`. Because the ShardGroup name equals the Volume name, Shard names are `<volumeName>-<slotIndex>` (e.g., `vol0-3` for slot 3 of volume `vol0`).

```yaml
apiVersion: longhorn.io/v1beta2
kind: Shard
metadata:
    name: vol0-3            # <shardGroupName>-<slotIndex>
    labels:
        longhornvolume: vol0
        shardgroup: vol0
    ownerReferences:
        - apiVersion: longhorn.io/v1beta2
          kind: ShardGroup
          name: vol0
          uid: <shardgroup-uid>
    finalizers:
        - longhorn.io/shard-cleanup
spec:
    shardGroupName: vol0
    slotIndex: 3
    size: "10737418240"  # bytes (string-encoded int64), set by ShardGroup controller. Passed to ShardCreate as size_bytes. Required for idempotent reconciliation.
    nodeID: node-b
    diskPath: /dev/sdb
    diskUUID: disk-uuid-xxx
status:
    ownerID: ""          # ID of the node that owns this Shard
    state: normal       # normal | failed | replacing
    role: parity         # derived from slotIndex and k (controller-computed)
    storageIP: 10.0.0.2 # IM pod's storage network IP (set by ShardGroup controller in syncShardInstances, same pattern as replicas)
    port: 4420          # NVMe-oF port
    rebuildProgress: 0  # 0-100 percent
    lastFailureTimestamp: ""
```

**StorageClass parameters:** users configure EC volumes via StorageClass, and the volume controller reads these and creates the ShardGroup CR accordingly:
```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
    name: longhorn-ec
provisioner: driver.longhorn.io
parameters:
    dataLayout.type: "sharded"
    dataLayout.mode: "erasureCoding"
    dataLayout.dataChunks: "4"     #k
    dataLayout.parityChunks: "2"   #m
    dataLayout.stripSizeKB: "64"
    dataLocality: disabled
    diskSelector: "ssd"
    nodeSelector: ""
    # Standard Longhorn parameters still apply
    numberOfReplicas: "1"   # RAID1 replica count (1 for EC, no replication)
    staleReplicaTimeout: "30"
```

When `dataLayout.type: "sharded"` is present, the volume controller creates a ShardGroup CR instead of Replica CRs. `numberOfReplicas` is set to 1 (no RAID replicas - EC provides fault tolerance). The `numberOfReplicas` field is repurposed for EC volumes: it controls the RAID1 member count of the identity layer above the EC bdev (always 1), **not** the number of data copies. Fault tolerance is determined entirely by `dataLayout.parityChunks` (m). Setting `numberOfReplicas` to any value other than 1 for EC volumes is rejected by the webhook.

**`dataLocality` constraint:** EC volumes require `dataLocality: disabled`. The `bestEffort` and `strictLocal` modes are incompatible with EC because data chunks are distributed across k+m nodes by design - there is no single node that holds a complete copy of the data. The Volume webhook rejects EC volumes with `dataLocality` set to anything other than `disabled`.

#### Volume controller EC mode detection

The Volume controller detects EC mode by checking whether `Volume.Spec.DataLayout.Type == VolumeDataLayoutTypeSharded`. When EC mode is detected:

1. **Volume creation (`reconcileVolumeCreation()`):** The existing `reconcileVolumeCreation()` calls `createEngine()` to create the Engine CR and then `replenishReplicas()` to create Replica CRs. For EC volumes, the Engine CR is still created (the engine runs on the attach node), but `replenishReplicas()` is skipped. Instead, the controller creates a single ShardGroup CR with the EC parameters from the Volume spec. The ShardGroup controller then takes over shard placement, ShardGroup-process creation, and shard-to-ShardGroup wiring. **`createEngine()` EC handling:** No EC-specific fields are copied to `engine.Spec`. For EC volumes the Engine spec is topology-agnostic: `replica_address_map` is populated with one entry pointing at `ShardGroup.Status.{IP, Port, NQN}` (set by the ShardGroup controller after the ShardGroup process is up). The InstanceManager dispatches to the **single** `EngineCreate` path - no `EngineCreateEC` exists; the engine code is identical for both topologies. `Volume.Spec.DataLayout` is immutable post-creation and is consulted only by the manager (for sanity-checking and for `Volume.Status.Robustness` derivation), not by the engine.
2. **Volume attach (`openVolumeDependentResources()`):** The existing flow starts replicas, collects their `StorageIP:Port` addresses into `e.Spec.ReplicaAddressMap`, and sets `e.Spec.NodeID = v.Spec.NodeID`, `e.Spec.DesireState = Running`, `e.Spec.Frontend`, etc. For EC volumes, the replica-start logic is replaced by ShardGroup-process provisioning: the Volume controller updates `ShardGroup.Spec.NodeID` to match `v.Spec.NodeID`, signaling the ShardGroup controller to start shard instances, provision the ShardGroup process, and expose its head lvol. Once `ShardGroup.Status.{IP, Port, NQN, ProcessState=Running}` are populated, the Volume controller fills `e.Spec.ReplicaAddressMap` with that single endpoint and sets `e.Spec.DesireState = Running` (gated by `isECVolumeReady`). The Volume controller still sets `e.Spec.NodeID` and `e.Spec.Frontend` as before. **Implementation note:** The EC guard in `openVolumeDependentResources()` must be placed after the existing `e.Spec.NodeID` / `e.Spec.DesireState` / `e.Spec.Frontend` assignments (these apply to all V2 volumes) and before the replica address-map construction and `startReplica` calls (EC-only skip; the ShardGroup controller drives shard/process startup instead).
3. **Volume detach (`closeVolumeDependentResources()`):** The existing flow sets `e.Spec.NodeID = ""` and `e.Spec.DesireState = InstanceStateStopped`, then waits for `e.Status.CurrentState == InstanceStateStopped` before stopping replicas. For EC volumes, the Volume controller follows the same ordering: it stops the engine first and waits for the engine to reach `InstanceStateStopped`. **`ShardGroup.Spec.NodeID` is NOT cleared on detach** - the ShardGroup process keeps running across detach so the lvstore + head lvol stay live for the next attach (this mirrors RAID1, where replica processes keep running across detach). The ShardGroup process is only torn down on volume deletion (via the ShardGroup CR's DeletionTimestamp path) or relocated to a new node on engine-node failover (via Volume controller updating `ShardGroup.Spec.NodeID`). Because `ShardGroup.Spec.NodeID` is not touched on detach, the engine-stop ordering only needs to guard the engine teardown itself - no cross-controller race exists between Volume controller and ShardGroup controller during detach.
4. **Replica-path bypass:** All replica-specific reconciliation paths are guarded with `if v.Spec.DataLayout.Type == VolumeDataLayoutTypeSharded { return }` early-returns at the top of each function. The complete list of guarded functions is:

   **Volume controller (`controller/volume_controller.go`):**
   - `replenishReplicas()` — EC volumes have no Replica CRs to replenish.
   - `ReconcileEngineReplicaState()` — EC path early-returns from replica logic and derives `Volume.Status.Robustness` from `ShardGroup.status.failedCount` vs `m` (see "Volume.Status.Robustness mapping" below).
   - `cleanupCorruptedOrStaleReplicas()` — No Replica CRs to clean up.
   - `reconcileVolumeCondition()` — EC volumes set `VolumeConditionTypeScheduled=True` unconditionally (scheduling is handled by the ShardGroup controller). The snapshot threshold condition (`TooManySnapshots`) is still evaluated.
   - `areVolumeDependentResourcesOpened()` — EC path checks only Engine + EngineFrontend running state, bypassing the empty replica iteration that would otherwise always return false.
   - `reconcileVolumeSize()` — EC path skips `CheckReplicasSizeExpansion` (no Replica CRs to check disk space against) and replica size propagation; expansion is driven through Engine and EngineFrontend spec only.

   **Readiness gate (`types/volumes.go`):**
   - `IsVolumeReady()` — EC volumes with zero Replica CRs must not fail the `allReplicaScheduled` check. The function skips the replica scheduling assessment when `DataLayout.Type == Sharded`. This is a cross-cutting concern: `IsVolumeReady` is called by the CSI controller server, the API model layer, and the webhook expansion validator.

   **Auto-salvage:** `isAutoSalvageNeeded()` is skipped for EC volumes; a separate `isECSalvageNeeded()` check uses `ShardGroup.status.state == offline` as the trigger condition (see "Auto-salvage for EC volumes" below).

   The Volume controller delegates all EC health management to the ShardGroup controller and reads `ShardGroup.status` for robustness derivation.

This detection pattern is consistent with how the Volume controller already branches on `Volume.Spec.DataEngine` (V1 vs V2). The EC branch is an additional check within the V2 path.

#### CRD validation (webhook)

The existing Longhorn admission webhook (`webhook/resources/`) is extended to validate the new CRDs. This follows the same pattern as the existing Volume, Engine, and Replica webhook validators.

**ShardGroup webhook (`webhook/resources/shardgroup/validator.go`):**
- **Create:**
    - `dataChunks` >= 1
    - `parityChunks` >= 1
    - `dataChunks + parityChunks` <= 32 (EC_MAX_BASE_BDEVS)
    - `stripSizeKB` must be a power of 2, range [4, 1024]
    - Referenced Volume CR must exist
    - Data engine must be V2 (EC is V2-only)
- **Update - immutable fields** (following the existing `validateImmutable()` pattern from the Volume webhook):
    - `spec.volumeName` - immutable (a ShardGroup cannot be re-parented to a different Volume; owner references in `metadata` do not block spec field mutation)
    - `spec.dataChunks` - immutable (changing k invalidates all on-disk parity)
    - `spec.parityChunks` - immutable (changing m invalidates EC geometry)
    - `spec.stripSizeKB` - immutable (changing strip size invalidates on-disk layout)
- **Delete:** allowed unconditionally (cleanup handled by finalizers on child Shard CRs).

**Shard webhook (`webhook/resources/shard/validator.go`):**
- **Create:**
    - Referenced ShardGroup CR must exist
    - `slotIndex` must be in range [0, k+m-1]
    - Data engine must be V2
- **Update - immutable fields:**
    - `spec.slotIndex` - immutable
    - `spec.shardGroupName` - immutable
- **Delete:** block deletion when the remaining healthy Shard CRs (excluding the one being deleted) would drop below `k` - the minimum needed for RS decoding. This is stricter than a "last healthy shard" guard: losing any shard that reduces healthy count below `k` makes the volume undecodable.

**Volume webhook extension:**
- When `dataLayout.type: "sharded"` is set in StorageClass parameters:
    - Validate the same constraints as ShardGroup create (`dataLayout.dataChunks` >= 1, `dataLayout.parityChunks` >= 1, sum <= 32, `dataLayout.stripSizeKB` is a power of 2 in [4, 1024])
    - `numberOfReplicas` must be 1 (EC provides fault tolerance, not RAID1)
    - `dataEngine` must be V2
    - `dataLocality` must be `disabled` (EC distributes data across nodes by design; `bestEffort` and `strictLocal` are incompatible)
    - `fromBackup` with EC parameters: allowed regardless of the backup source's replication mode (RAID1 or EC). Backup data is raw block data, independent of the underlying replication/EC scheme (see Backup/restore section below). The webhook validates only that the target volume's EC parameters (k, m, strip_size) satisfy the ShardGroup constraints - it does not require matching the backup source's EC parameters
- **Update - immutable fields** (following the existing `validateImmutable()` pattern):
    - `spec.dataLayout` - the entire struct is immutable after creation. This covers `type`, `mode`, `dataChunks`, `parityChunks`, and `stripSizeKB` with a single rule. Changing any sub-field post-creation is rejected by the webhook.

#### ShardGroup controller architecture

The ShardGroup controller follows the same patterns as existing Longhorn controllers (VolumeController, EngineController, ReplicaController).

**Registration:** Registered in `controller_manager.go`'s `StartControllers()` alongside existing controllers. Uses `ds.ShardGroupInformer()` as its primary informer with a `cacheSyncs` entry to ensure the ShardGroup cache is populated before the controller starts processing.

**`isResponsibleFor()`:** The ShardGroup controller on each manager node runs `isResponsibleFor()` to determine ownership. Ownership follows the ShardGroup process node: the manager node matching `ShardGroup.Spec.NodeID` (where the ShardGroup process runs) is responsible for the ShardGroup. `ShardGroup.Spec.NodeID` is set on first attach by the Volume controller (typically equal to `Engine.Spec.NodeID` for co-location) and is not cleared on detach; it only changes on engine-node failover. There is no "empty NodeID" case during normal lifecycle - ownership is stable across detach/re-attach cycles. This ensures exactly one manager instance reconciles each ShardGroup at any time.

**Watches:**
- Primary: ShardGroup CRs (owned resource)
- Secondary: Shard CRs (child resources, via owner reference), Engine CRs (to detect engine health changes), Volume CRs (to detect attach/detach/resize requests)

**Reconcile loop responsibilities:**
1. **Shard lifecycle:** Create/delete child Shard CRs to match `spec.dataChunks + spec.parityChunks`. On creation, invoke the scheduler for shard placement (`scheduleShards`), then call `InstanceCreate` (kind=shard) via the Instance Manager on each shard node (`syncShardInstance`). `syncShardInstance` runs every reconcile cycle and issues a live `InstanceGet`: when the IM reports the instance as Running, `Shard.Status.storageIP` and `Shard.Status.port` are populated/refreshed (`storageIP` is read from the IM pod's storage network IP via `GetIPFromPodByCNISetting`; `port` is read from `InstanceResponse.Status.PortStart`); when the IM reports the instance as Stopped or Error, `syncShardInstance` transitions the Shard CR to `ShardStateFailed` and seeds `LastFailureTimestamp` (`markShardInstanceNotRunning`), routing the next reconcile cycle through `reconcileFailedShard`. `InstanceResponse.Status.Endpoint` carries the NVMe-oF NQN for observability but is not used to populate `storageIP` - the IM pod IP is the actual routing address. The InstanceManager does not write Kubernetes CRs - it only returns instance status in the gRPC response.
2. **ShardGroup process lifecycle (`syncProcess`):** Create/manage/destroy the ShardGroup process on `ShardGroup.Spec.NodeID` via `InstanceCreate` (kind=shardgroup) / `InstanceDelete`. `syncProcess` runs every reconcile cycle and:
    - Provisions the process when the dual readiness gate passes (`ecShardAddressMap` has k+m entries AND every Shard is `ShardStateNormal`) and `Spec.NodeID` is set. Provisioning sends `ShardGroupSpec{shards, k, m, strip_size_kb}` and reads back `Status.{IP, Port, NQN, LvstoreUUID, HeadLvolUUID, ProcessState}` on success.
    - Tears the process down with `cleanupRequired = (sg.DeletionTimestamp != nil)` - matching the Replica controller's pattern. `cleanupRequired=false` on detach preserves the lvstore + head lvol on the encoded blocks; `cleanupRequired=true` on delete authorizes `bdev_lvol_delete` + `bdev_lvol_delete_lvstore`.
    - Re-binds the process to a new node when `ShardGroup.Spec.NodeID` changes (engine node failover). On restart on the new node, the process re-attaches shards, calls `bdev_ec_create`, and `bdev_examine` discovers the existing lvstore from the encoded blocks (`RecreateEC` semantics, relocated from engine to ShardGroup process).
    - Surfaces process state via `Status.ProcessState` (Running, Stopped, Error, Restarting) and emits Kubernetes events on transitions for operator visibility.
3. **Health aggregation:** Three complementary failure-detection paths feed CR state. `syncECHealth` polls the ShardGroup process's bdev_ec to read per-slot states (`normal`/`failed`/`replacing`) and propagates them to Shard CRs - this is authoritative once the ShardGroup process is running. `syncShardInstance` polls each shard's IM-side instance state every cycle and transitions the Shard CR to `ShardStateFailed` when the instance is not Running - this catches shard failures before the ShardGroup process is up (e.g., the run.log scenario where a shard's SPDK process stopped while the address was still cached on the Shard CR) and after ShardGroup process teardown. `syncProcess` polls the ShardGroup process's IM-side instance state and transitions `Status.ProcessState` accordingly. All three paths feed the appropriate failure-recovery dispatch.
4. **Failure recovery:** Detect FAILED slots, schedule replacements, orchestrate the two-step rebuild (`ShardGroupShardReplace` + `ShardGroupShardRebuildStart`). RPCs target the ShardGroup process (which owns the bdev_ec instance) rather than the engine.
5. **Expansion:** Coordinate `ShardExpand` across shard nodes, then call `ShardGroupExpand` on the ShardGroup process to resize bdev_ec + lvstore + head lvol (`syncShardGrow`). The engine then calls `EngineExpand` to grow its raid1 view, identical to RAID1 expand.
6. **Rebuild QoS:** Read `Volume.Spec.ReplicaRebuildingBandwidthLimit` and call `ShardGroupShardRebuildQosSet` on the ShardGroup process during active rebuilds (`syncShardRebuildQoS`).

**Separation from Volume controller:** The Volume controller continues to own the Volume CR lifecycle (attach, detach, frontend management). For EC volumes, the Volume controller:
- Creates the ShardGroup CR (instead of Replica CRs) when the volume is first created and EC parameters are present.
- Sets `ShardGroup.Spec.NodeID` whenever the engine node changes (typically equal to `Engine.Spec.NodeID` for co-location). The shard slot-to-address map is owned by the ShardGroup controller (`ShardGroup.Status.ecShardAddressMap`), populated from healthy Shard CRs in its `syncStatus` - the Volume controller does not touch the address map. The ShardGroup process's exposed endpoint (`Status.IP/Port/NQN`) is also owned by the ShardGroup controller.
- Reads `ShardGroup.Status` to derive `Volume.Status.Robustness` (see Robustness mapping below).
- Does **not** call any EC-specific gRPC methods directly - all EC operations are delegated to the ShardGroup controller. The Engine controller treats the ShardGroup endpoint as just another upstream NVMe-oF endpoint, identical to a single-replica RAID1 setup.

**The ShardGroup controller does not create or start the engine.** Engine lifecycle (start, stop, crash-recovery) is driven exclusively by the Volume controller through the existing `e.Spec.DesireState` mechanism, exactly as for RAID1 volumes. The ShardGroup controller's role is to ensure shards are running, the ShardGroup process is up with its lvstore + head lvol exposed, and the engine has an upstream endpoint to connect to. It signals readiness by populating `ShardGroup.Status.{IP, Port, NQN, ProcessState=Running}`. The volume controller's `openVolumeDependentResources` reads `Status.ProcessState == Running && Status.IP != ""` via `isECVolumeReady`; only once both gates pass does it set `e.Spec.DesireState = Running`.

#### Engine node selection

The engine node for an EC volume is selected using the **same mechanism as V2 replicated volumes**: `Engine.Spec.NodeID` is copied from `Volume.Spec.NodeID` at attach time. There is no independent engine scheduler.

The existing V2 volume attach flow in `VolumeController.openVolumeDependentResources()` sets `e.Spec.NodeID = v.Spec.NodeID`. For EC volumes, the Volume controller additionally sets `ShardGroup.Spec.NodeID` to match `e.Spec.NodeID` so that the ShardGroup process is co-located with the engine, and so the ShardGroup controller knows which InstanceManager to call for `InstanceCreate(kind=shardgroup)`.

**Shard address discovery (`syncStatus` in ShardGroup controller):** The ShardGroup controller's `syncStatus` runs on every reconcile tick. It reads `Shard.Status.storageIP` and `Shard.Status.port` from all k+m Shard CRs and writes the addresses of healthy shards into `ShardGroup.Status.ecShardAddressMap` (keyed by slot index string). Only shards in `ShardStateNormal` with non-empty `storageIP`/`port` are included; shards in `ShardStateFailed` or `ShardStateReplacing` are omitted from the map even if they still carry a cached address. This matters because `Status.StorageIP`/`Port` persist across SPDK process restarts (the IM may re-allocate the port silently), so address presence alone is not a liveness signal. The ShardGroup controller's `syncProcess` enforces a two-part readiness check before provisioning the ShardGroup process: (a) `ShardGroup.Status.ecShardAddressMap` has k+m non-empty addresses, and (b) every Shard CR is in `ShardStateNormal`. Both must hold. The controller retries on the next reconcile tick if either gate fails. Once both pass, `syncProcess` calls `InstanceCreate(kind=shardgroup, ShardGroupSpec={shards: ecShardAddressMap, k, m, strip_size_kb})` on the engine-node InstanceManager. The volume controller's `openVolumeDependentResources` then sees `Status.ProcessState == Running && Status.IP != ""` (via `isECVolumeReady`) and sets `e.Spec.DesireState = Running`. This is analogous to how `openVolumeDependentResources` builds `replicaAddressMap` from running replicas for RAID1 volumes - the EC case collapses the replica-set into a single ShardGroup endpoint that the engine consumes as a single base bdev.

**Engine node vs shard node anti-affinity:** The engine node *can* be a shard node. A single node failure in this case takes out both a shard AND the engine (and the ShardGroup process, since it is co-located with the engine), but this is acceptable:
- The shard loss is handled by the EC fault tolerance (the array degrades but remains functional as long as `failed_count <= m`).
- The engine + ShardGroup process loss triggers the standard detach -> re-attach cycle: `VolumeController.closeVolumeDependentResources()` clears `e.Spec.NodeID`, the volume transitions to Detached, and re-attach on a new node (via `v.Spec.NodeID` from VolumeAttachment) reconstructs both the ShardGroup process and the engine on the new node from the surviving shards.
- Requiring a dedicated engine node (k+m+1 nodes) would be too restrictive for small clusters. The minimum cluster size is k+m nodes.

**Engine node failure and recovery:**
1. The InstanceManager on the engine node becomes unreachable.
2. `VolumeController.closeVolumeDependentResources()` sets `e.Spec.NodeID = ""` and `e.Spec.DesireState = InstanceStateStopped`.
3. The volume transitions through `Detaching -> Detached`. Both the engine and the ShardGroup process on the failed node are unreachable; the ShardGroup process is re-bound to a new node by the ShardGroup controller as part of the re-attach flow (or proactively, depending on the failure-detection policy).
4. Kubernetes reschedules the workload pod (or the user manually re-attaches). The new `Volume.Spec.NodeID` triggers `openVolumeDependentResources()`, which sets `e.Spec.NodeID` to the new attach node.
5. `reconcileShardGroup` updates `ShardGroup.Spec.NodeID` to the new node. The ShardGroup controller's `syncProcess` provisions a new ShardGroup process on the new engine node: it NVMe-attaches to all k+m shards, calls `bdev_ec_create`, and `bdev_examine` discovers the existing lvstore + head lvol from the encoded blocks (the `RecreateEC` salvage semantics, now in the ShardGroup process). On success, `Status.{IP, Port, NQN, ProcessState=Running}` are populated. `openVolumeDependentResources` sees the endpoint ready (via `isECVolumeReady`) and sets `e.Spec.DesireState = Running`. The instance handler calls `EngineCreate` on the new node's InstanceManager; the new engine NVMe-attaches to the ShardGroup endpoint and builds raid1(1) - identical to RAID1 single-replica engine startup.
6. NVMe-oF target is re-exported, workload pod's initiator reconnects.

This is identical to how V2 replicated volumes handle engine node failure today - the engine is never "migrated" on failure; it is stopped and recreated on the new attach node, alongside the ShardGroup process for EC volumes.

**`ShardGroup.Spec.NodeID` mutability:** The Volume controller is the sole owner of this field. It updates `ShardGroup.Spec.NodeID` whenever `Engine.Spec.NodeID` changes (attach, detach, migration). The ShardGroup controller reads this field but never writes it. This avoids cross-controller coordination - the same ownership model as `Engine.Spec.NodeID` being set exclusively by the Volume controller. The detach guard (`e.Spec.NodeID == "" && e.Status.CurrentState != Stopped` -> skip until engine fully stopped) is not needed for the ShardGroup process because the ShardGroup process keeps running across detach (its NodeID is **not** cleared on detach; it is only cleared on volume delete or node-failure-driven re-bind).

**`reconcileShardGroup` - volume controller coordination function:** `VolumeController.reconcileShardGroup(v, e)` is called on every volume reconcile tick for EC volumes. It:
1. Creates the ShardGroup CR if it does not yet exist (with EC parameters from `v.Spec.DataLayout`).
2. Syncs `ShardGroup.Spec.NodeID` to `e.Spec.NodeID` for ShardGroup-process placement (typically the engine node for co-location). Re-binding to a new node on engine failover is handled by `syncProcess` in the ShardGroup controller.
The address-map maintenance previously listed here moves to the ShardGroup controller's `syncStatus`. `reconcileShardGroup` does **not** read shard statuses or write any Engine spec fields.

`openVolumeDependentResources` calls `isECVolumeReady`, which reads `ShardGroup.Status.ProcessState` and `Status.IP`. Both must be set (`ProcessState == Running` and `IP != ""`) before `DesireState = Running` is issued for the engine. This single gate replaces the dual gate from the previous design - the ShardGroup controller has already enforced the shard-level dual gate as part of process provisioning, so the volume controller only needs to verify that the ShardGroup endpoint is alive.

**Live migration (deferred - not in initial implementation):** The migration path described below is not implemented in the initial EC sharding release. EC volumes created in this release are non-migratable. The design is documented here for the follow-on implementation.

Because the lvstore + `bdev_ec` live in the **ShardGroup process**, not the engine, the migration unit is the engine - not the EC stack. Two implementation strategies are possible:

- **Strategy A (engine-only migration, ShardGroup process stays put):** The migration engine starts on the migration node and NVMe-attaches to the **existing** ShardGroup process's exposed lvol endpoint over NVMe-oF (cross-node). Both old engine and migration engine simultaneously connect to the one ShardGroup endpoint as initiators (NVMe-oF supports multiple initiators). Frontend switchover transfers the active frontend from old to migration engine, then the old engine is torn down. The ShardGroup process never moves. This is the simplest path and avoids any blobstore-exclusivity concern - there is only ever one blobstore consumer (the ShardGroup process). Cost: cross-node NVMe-oF traffic from the migration engine to the ShardGroup-process node until the volume is detached and re-attached, at which point the ShardGroup process can be relocated to the new engine node for co-location.
- **Strategy B (ShardGroup process also migrates):** A second ShardGroup process is provisioned on the migration node, NVMe-attaches to the same k+m shard endpoints as the original, calls `bdev_ec_create`, and `bdev_examine` auto-imports the existing blobstore. Both ShardGroup processes are simultaneously active during the brief migration window. The blobstore claim type must be `SHARED` (read-only auto-import) so the two processes do not race on metadata writes; only one is the active writer at any time. After frontend switchover, the old ShardGroup process and old engine are torn down.

The recommended strategy for the initial migration implementation is **A** - it is simpler, has no blobstore-exclusivity safety dependency, and the cross-node ShardGroup-to-engine traffic is short-lived (only spans the migration window).

For both strategies, `EngineFrontendSwitchOver` transfers the active frontend from old engine to migration engine, the **Volume controller** updates `ShardGroup.Spec.NodeID` to the migration node only after switchover completes (in the migration confirmation path of `processMigration()`, alongside updating `v.Status.CurrentNodeID` and switching the current engine), and the ShardGroup controller's `syncProcess` then relocates the ShardGroup process to the new node. This maintains the invariant that the Volume controller is the sole writer of `ShardGroup.Spec.NodeID`.

**Blobstore exclusivity (Strategy B only):** If Strategy B is chosen, two ShardGroup processes simultaneously have the blobstore imported. The SPDK blobstore is single-writer - two blobstore consumers cannot actively write simultaneously on the same bdev. The sequence is:
1. Migration ShardGroup: `bdev_nvme_attach_controller` × (k+m) + `bdev_ec_create` - the EC bdev is registered and discovers the existing blobstore superblock via SPDK's bdev examine mechanism. SPDK's `bdev_lvol_examine` auto-imports the blobstore read-only (claim type `SHARED` via `spdk_bdev_module_claim_bdev`), but does not issue writes until a write operation (e.g., lvol create, snapshot) is explicitly requested.
2. Old engine: quiesce frontend via `InstanceSuspend` - all new I/O is paused, in-flight I/O drains.
3. Migration ShardGroup: re-expose head lvol; migration engine `InstanceResume` with new frontend - the migration engine becomes the active writer. The old engine and old ShardGroup process are torn down as part of `EngineDelete` + `ShardGroupDelete(cleanupRequired=false)`.

The blobstore claim model ensures that both ShardGroup processes can have the blobstore imported simultaneously during the brief migration window, but only one is actively writing at any time. The `cleanupRequired=false` on the old ShardGroup is critical: it must not call `bdev_lvol_delete_lvstore` on the shared lvstore.

**Safety note (Strategy B only):** The correctness of Strategy B depends on SPDK's `bdev_lvol_examine` using a `SHARED` (read-only) claim type during auto-import. If the claim type is `SHARED_WRITE` or if the auto-examine triggers any blobstore metadata writes (e.g., superblock update), two simultaneous blobstore instances could corrupt metadata. The implementation must verify that SPDK's auto-examine path is read-only by default, or alternatively, disable auto-examine on the migration ShardGroup and defer blobstore import until after the old ShardGroup process is quiesced. Strategy A avoids this concern entirely.

#### Scheduling (shard placement)
The shard scheduler selects nodes and disks for k+m shards. Constraints:
- **Node anti-affinity (hard)**: no two shards of the same volume on the same node. If the cluster has fewer nodes than k+m, the volume creation should be blocked. Unlike the existing replica scheduler, the shard scheduler **always enforces hard node anti-affinity** - the `ReplicaSoftAntiAffinity` setting is ignored for EC volumes. Co-locating two shards on the same node wastes EC fault tolerance (a single node failure would take out two slots instead of one), so soft anti-affinity is never appropriate. The `ReplicaDiskSoftAntiAffinity` setting is also ignored since each shard must be on a separate node.
- **Zone anti-affinity**: If Kubernetes topology labels are present (`topology.kubernetes.io/zone`), spread shards across zones for zone-level fault tolerance. The `ReplicaZoneSoftAntiAffinity` setting is respected for EC volumes - zones can be reused if the cluster has fewer zones than k+m, but the scheduler prefers unused zones first.
- **Capacity**: each selected disk must have at least `volumeSize / k + 2 × strip_size` free space. All k+m shards are identically sized: the EC module's `ec_compute_geometry` uniformly subtracts `2 × strip_size` from the per-disk capacity (the WIB data is stored only on parity disks, but all disks must be the same size for the geometry calculation). The `2 × strip_size` overhead is negligible (e.g., 128 KiB at the default 64 KB strip size) and can be ignored for scheduling heuristics. The `ShardCreate` RPC's `size_bytes` parameter for all k+m shards includes this reservation (`shard_size + 2 * strip_size`). The ShardGroup controller computes this inflated size before calling `ShardCreate` - the shard node itself has no knowledge of `strip_size` or `k` and simply creates an lvol of exactly `size_bytes`.
- **Disk selector**: respects the `diskSelector` label.

The scheduler outputs a list of (node, disk, slotIndex, role) tuples. The ShardGroup controller creates Shard CRs and triggers the InstanceManager on the engine node to build the EC stack.

**InstanceManager selection for shard instances:** Each Shard CR specifies `Shard.Spec.Node`, which identifies which node the shard lvol and NVMe-oF export run on. The ShardGroup controller finds the InstanceManager for a shard using the same lookup pattern as the Replica controller: `ds.GetInstanceManagerByInstanceRO(shard)` resolves to the InstanceManager pod running on `Shard.Spec.Node`. The InstanceManager on the shard node handles `InstanceCreate` (type=shard), creating the shard lvol and exporting it via NVMe-oF. The InstanceManager on the engine node is separate - it handles `InstanceCreate` (type=engine) for the EC engine stack.

**Shard instance monitoring:** Individual shard instance health is monitored indirectly through the EC module **inside the ShardGroup process**. The EC bdev there detects I/O errors on shard NVMe-oF connections and transitions the corresponding slot to FAILED. The ShardGroup controller reads these slot states via `ShardGroupGet` and propagates them to Shard CRs. There is no separate shard-side health polling - if a shard's lvol is unhealthy but the NVMe-oF export is still up (serving errors), the ShardGroup process observes the I/O errors and fails the slot. If the shard's InstanceManager crashes, the NVMe-oF export goes down, and the ShardGroup process detects this via the NVMe-oF reconnect timeout (`ctrlr_loss_timeout_sec`). The ShardGroup controller does not poll shard InstanceManagers directly for slot health - the ShardGroup process is the single source of truth for slot state.

#### Orphaned shard instance cleanup

A shard instance is orphaned when it is running on an InstanceManager but no ShardGroup CR exists for the volume it belongs to (e.g., after a manager crash during volume deletion left the SPDK process behind, or if the ShardGroup CR was deleted out-of-band). Orphaned shard instances consume disk and memory on the shard node indefinitely if not cleaned up.

**Detection - InstanceManager monitor:** The InstanceManager monitor's `syncInstances` poll loop calls `InstanceList` on each reconcile tick, which enumerates all running SPDK instances regardless of type. `categorizeProcesses` splits the result into engine, engine-frontend, replica, and shard process maps. The shard process map is synced to `InstanceManagerStatus.InstanceShards` (mirroring `InstanceEngines`/`InstanceReplicas` for other types) and is also passed to `syncOrphans`. For each shard process, `isShardOrphaned` extracts the volume name from the instance name (`<volumeName>-<slotIndex>`) and checks whether a ShardGroup CR exists. If the ShardGroup is absent, `createOrphanForShardInstances` creates an `OrphanTypeShardInstance` Orphan CR with the node ID, instance manager name, and instance name stored in `spec.parameters`.

**Webhook - orphan mutator:** The orphan admission mutator (`webhook/resources/orphan/mutator.go`) handles `OrphanTypeShardInstance` by calling `GetOrphanLabelsForOrphanedShardInstance`, which sets `longhornshard=<instanceName>`, `longhorninstancemanager=<imName>`, `longhornnode=<nodeID>`, and `longhorn.io/orphan-type=shard-instance` labels on the CR. Without this case the webhook rejects the CR with `invalid orphan labels`.

**Cleanup - orphan controller:** The orphan controller's `cleanupOrphanedShardInstance` obtains a client to the instance's InstanceManager and calls `InstanceDelete(type=shard, cleanupRequired=true)` to permanently tear down the running SPDK process and delete the underlying lvol. `cleanupRequired=true` is required here - passing `false` would skip `ShardDelete` entirely and leave the orphaned lvol on disk, causing the detect/sync loop to immediately rediscover and re-report it as orphaned. On success (or `NotFound`), the function returns `isCleanupComplete=true`; the orphan controller then removes the Orphan CR's finalizer, triggering Kubernetes garbage collection of the CR.

**Orphan CR lifecycle in the IM controller:** The IM controller's `deleteOrphans` handles `OrphanTypeShardInstance` symmetrically with engine and replica instance orphans: `instanceExist` is set from `im.Status.InstanceShards[instanceName]` (true while the process is still reported by the IM monitor); `instanceCRScheduledBack` is set by `isShardGroupManaged`, which returns true if the ShardGroup CR now exists - indicating the shard is managed again and the orphan CR can be removed. The normal cleanup path (orphan controller removes the finalizer after `InstanceDelete`) handles most cases; `deleteOrphans` covers the edge cases: IM termination, auto-delete grace period expiry, and ShardGroup re-creation.

#### Failure recovery (ShardGroup controller reconcile path):

Failure recovery is handled by the ShardGroup controller as a reconcile path. When `syncECHealth` reads a slot in FAILED state from `ShardGroupGet`, it records `Shard.Status.LastFailureTimestamp` (RFC3339) on first detection, emits a `Warning ShardFailed` Kubernetes event on the ShardGroup CR (`reason: ShardFailed`, message includes shard name, slot index, and volume name), and sets the ShardGroup state to `degraded` (or `offline` if `failedCount > m`). The `reconcileFailedShard` function then drives the following state machine for each failed shard:

1. **Debounce (self-healing window):** Wait `replica-replenishment-wait-interval` from `LastFailureTimestamp`. During this window the controller takes no action, allowing transient NVMe-oF reconnections to restore the slot to NORMAL without triggering a re-provisioning cycle. If the slot returns to NORMAL (detected by `syncECHealth` on the next poll), no replacement is needed. **Note:** The original design called for issuing `ShardGroupShardReplace` with the existing address as an active probe during the debounce window (early recovery if the node came back). This is deferred to a future improvement - the current implementation only waits.
1. **Replace failed (node still down):** If `ShardGroupShardReplace` fails after the debounce window, delete the Shard CR. The finalizer (`longhorn.io/shard-cleanup`) ensures `cleanupDeletedShards` calls `InstanceDelete` on the failed node when it eventually becomes reachable, preventing stale lvol accumulation. The Shard CR name is slot-deterministic (e.g., `vol0-3` for slot 3 of volume `vol0`); deleting and recreating the CR for the same slot reuses the same name.
1. **Re-provision:** `syncShards` detects the deleted slot and creates a new Shard CR with the same name and `slotIndex`. `scheduleShards` places it on a new node (hard anti-affinity prevents reuse of currently healthy shard nodes) and `syncShardInstance` calls `InstanceCreate` on the new node's InstanceManager. **Note:** `syncECHealth` will set `LastFailureTimestamp` on the new Shard CR only after `StorageIP` is set (the timestamp is suppressed while the instance is still provisioning, preventing a premature debounce that would have caused a deadlock). However, once `StorageIP` is populated and the slot is still FAILED in SPDK, `syncECHealth` does set `LastFailureTimestamp`, causing `shouldDelayReplace` to apply the full `replica-replenishment-wait-interval` debounce a second time before calling `ShardGroupShardReplace`. This is a known implementation behavior that doubles recovery time for replaced shards; it is tracked for improvement in a future iteration.
1. **Rebuild:** Once the new instance is running (`Shard.Status.StorageIP` set) and the debounce has elapsed, `reconcileFailedShard` calls `ShardGroupShardReplace(new_address)` followed by `ShardGroupShardRebuildStart`. The ShardGroup state transitions to `rebuilding`.
1. **Progress and completion:** `syncECHealth` reads `EcRebuildProgress` on each reconcile tick and updates `Shard.Status.RebuildProgress`. On rebuild completion the slot transitions from REPLACING -> NORMAL; `syncStatus` sets `ShardGroup.Status.State = healthy` once `RebuildInProgress` clears and `failedCount == 0`.

If the replacement disk itself fails during rebuild, the engine reports the slot as FAILED again, `syncECHealth` records a new `LastFailureTimestamp`, and step 1 restarts.

**Shard CR lifecycle on replacement:** The failed Shard CR is deleted (step 2 above). The finalizer blocks garbage collection until `cleanupDeletedShards` confirms the SPDK instance is torn down on the shard node. Recreating the CR with the same slot-deterministic name (step 3) works correctly because `syncShards` only creates a CR when no CR exists for that slot index - it waits for the old CR to be fully GC'd first.

**`ShardGroupState` during rebuild:** When `ShardGroup.Status.RebuildInProgress == true` and the array is not offline, `syncStatus` sets `State = rebuilding` regardless of `failedCount`. During rebuild the previously FAILED slot transitions to REPLACING; REPLACING slots do not increment `failedCount`, so `deriveState` would otherwise return `healthy` while the array is still not fully redundant. The explicit `rebuilding` state prevents this premature healthy report.

**Rebuild bandwidth control:** EC rebuild bandwidth is controlled using the same mechanism as V2 replica rebuild. The existing `Volume.Spec.ReplicaRebuildingBandwidthLimit` setting (per-volume, falling back to the global `SettingNameReplicaRebuildingBandwidthLimit`) is reused. The engine controller's `checkAndApplyRebuildQoS()` already applies bandwidth limiting for V2 volumes by calling `engineClientProxy.ReplicaRebuildQosSet()`. For EC volumes, the equivalent call is `ShardGroupShardRebuildQosSet(shardgroup_id, max_stripes_per_sec, paused)` which wraps `bdev_ec_set_rebuild_qos` inside the ShardGroup process. The **ShardGroup controller** performs the MB/s -> stripes/sec conversion (it has access to k, m, and strip_size from the ShardGroup spec): `max_stripes_per_sec = mbps * 1024 * 1024 / (strip_size_kb * 1024 * (k + m))`. Setting `paused = true` suspends the rebuild poller without cancelling the rebuild (useful for temporarily yielding I/O bandwidth to foreground workloads). The `concurrent-replica-rebuild-per-node-limit` global setting limits how many rebuild operations can run on a single node across all volumes. For EC volumes, each ShardGroup counts as one rebuild operation (since an EC rebuild is a single coordinated operation across all REPLACING slots, unlike RAID1 where each replica rebuilds independently). The ShardGroup controller enforces this limit via `canStartShardRebuild()`, which queries all ShardGroups sharing the same node (the node hosting the ShardGroup process) and counts those with `RebuildInProgress == true`. The ShardGroup-process node is used as the reference node because the rebuild coordinator runs there. Both `reconcileFailedShard` (before calling `ShardGroupShardRebuildStart` after a successful replace) and the atomicity guard in `syncECHealth` (before re-arming an interrupted rebuild) check the limit; if exceeded, the rebuild start is deferred and retried on the next reconcile tick.

**Rebuild atomicity guard (manager crash between replace and rebuild-start):** The rebuild is a two-step operation (`ShardGroupShardReplace` then `ShardGroupShardRebuildStart`). If the Longhorn manager crashes after step 1 (hot-swap) but before step 2 (rebuild-start), the shard is left in REPLACING state with no rebuild running. The ShardGroup controller's reconcile loop must detect and recover from this state. The reconcile invariant is:

> If any Shard CR has `status.state == replacing` and `ShardGroup.status.rebuildInProgress == false`, the ShardGroup controller must re-issue `ShardGroupShardRebuildStart` to resume the rebuild.

This is safe because `ShardGroupShardRebuildStart` wraps `bdev_ec_start_rebuild`, which is idempotent - calling it when a rebuild is already running returns success, and calling it when REPLACING slots exist but no rebuild is active starts the rebuild. The ShardGroup controller checks this condition on every reconcile tick, so recovery happens within one reconcile interval after manager restart.

This follows the same reconcile pattern as the existing V2 replica rebuild. The EC case uses ShardGroup/Shard CRs and calls `bdev_ec_replace_base_bdev` + `bdev_ec_start_rebuild` instead of the RAID1 rebuild RPCs.

#### Fast-path for intentional Shard CR deletion (`ShardGroupShardForceFail`)

The failure recovery path described above is tuned for **unintentional** failures (disk loss, node panic, NVMe-oF transport drop). On those paths the slot is reported FAILED by `bdev_ec` within seconds of the SPDK_BDEV_EVENT_REMOVE that fires when the upstream NVMe-oF target stops answering, and `reconcileFailedShard` proceeds to debounce + replace.

For **intentional** Shard CR deletion (admin issues `kubectl delete shard ...`, eviction, drain), the same path is unacceptably slow. The ShardGroup process's `bdev_nvme` initiator does not see SPDK_BDEV_EVENT_REMOVE the moment the Shard CR is deleted - it sees TCP-level disconnects only after `cleanupShard` tears down the upstream NVMe-oF subsystem on the shard node, and even then `bdev_nvme` waits `ctrlr_loss_timeout_sec` (default 120s) before declaring the controller permanently lost and propagating REMOVE downstream. Layered on top is the standard `replica-replenishment-wait-interval` debounce (default 600s) that `reconcileFailedShard` applies once it observes `LastFailureTimestamp`. End-to-end, an intentional delete takes ~720s before `ShardGroupShardReplace` is even attempted - and the application sees a flood of "connect refused" log noise for the entire window because `bdev_nvme` retries on its standard cadence.

The fast-path closes both gaps. When the ShardGroup controller's reconcile loop observes a Shard CR with `DeletionTimestamp != nil` whose slot is still NORMAL in `ShardGroupGet`, it issues `ShardGroupShardForceFail(shardgroup_id, shard_name)` *before* `cleanupShard` tears down the upstream subsystem. The ShardGroup process detaches the named NVMe controller, which fires SPDK_BDEV_EVENT_REMOVE synchronously on the slot's local bdev, drives `ec_slot_set_failed`, and the slot transitions NORMAL -> FAILED in one reconcile tick. `cleanupShard` then proceeds normally to delete the upstream lvol; the controller (now in FAILED state and detached) raises no further connection-retry log noise. `syncShards` schedules a fresh slot, and the standard recovery sequence (replace + rebuild-start) runs immediately - no 120s ctrlr_loss wait.

The 600s `replica-replenishment-wait-interval` debounce is **bypassed on the intentional-delete path** for the same reason: the wait exists to absorb transient failures, and an admin-initiated delete is not transient. The ShardGroup controller marks the Shard CR with an annotation (`longhorn.io/intentional-delete: "true"`) immediately before issuing `ShardGroupShardForceFail`; `shouldDelayReplace` reads this annotation on the *replacement* Shard CR (which inherits the slot index but is a fresh CR after re-provisioning) via cross-tracking on the ShardGroup status. If the prior delete was intentional, the new shard's `LastFailureTimestamp` is suppressed entirely and `triggerShardReplace` is allowed to fire as soon as `StorageIP` is set on the new instance.

**Idempotency rules** (enforced by the SPDK process, not the controller):
- Slot already FAILED: success. (The reconcile loop may retry after a manager crash mid-sequence; the second call is a no-op.)
- Slot REPLACING: `FailedPrecondition`. A rebuild is in flight; force-failing now would invalidate the in-progress reconstruction. The controller must wait for rebuild completion before deleting.
- Slot NORMAL but `shard_name` is not the slot's currently bound shard: `FailedPrecondition`. Defends against a stale delete request racing past a successful replace.

**Safety boundary:** `ShardGroupShardForceFail` is layout-narrow by design. It calls only `bdev_nvme_detach_controller`; it does not touch `bdev_ec` directly, does not modify failure counters, and does not call `bdev_ec_replace_base_bdev`. Failure accounting (failed_count, dirty-region bookkeeping, degraded-mode gating) flows through the standard SPDK_BDEV_EVENT_REMOVE path that `bdev_ec` already handles for unintentional failures. The only new behavior is the *trigger*; everything downstream is unchanged.

**Backwards compatibility:** Older spdk-engine builds without `ShardGroupShardForceFail` return `Unimplemented`. The ShardGroup controller treats `Unimplemented` as a fall-through and proceeds along the slow path (wait for `ctrlr_loss_timeout_sec` + `replica-replenishment-wait-interval`). No control-plane change is required to interoperate with older data-plane builds; the fast-path is opportunistic.

**Comparison with RAID1:** The RAID1 equivalent of intentional replica deletion uses a different mechanism - `EngineReplicaAdd` is hot-ADD (grows the replica set), not hot-SWAP, so an intentional replica delete is followed by `EngineReplicaAdd` of a fresh replica without ever needing to mark the deleted one FAILED at the bdev_raid layer (RAID1 simply stops including it in the replica list). EC cannot use the same approach because the EC slot count is fixed at `bdev_ec_create` time and slot indices are addressable - the slot has to traverse FAILED -> REPLACING -> NORMAL whether the cause is intentional or a real failure. `ShardGroupShardForceFail` is the EC-specific accelerator that reaches the FAILED state in seconds rather than minutes.

#### EC volume creation - end-to-end round-trip

The following sequence traces a fresh EC volume from PVC creation to the volume serving I/O. Each step is driven by a specific controller function.

```
1. User creates PVC with EC StorageClass (dataLayout.type=sharded, k=4, m=2, stripSizeKB=64)
   -> CSI ControllerCreateVolume creates Volume CR with spec.dataLayout populated

2. VolumeController.reconcileVolumeCreation
   -> Creates Engine CR (no EC-specific spec fields; for EC volumes the engine
      consumes a single upstream endpoint resolved at CreateInstance time from
      ShardGroup.Status.{IP, Port, NQN})
   -> Calls reconcileShardGroup -> creates ShardGroup CR (name = volume name, no NodeID yet,
      with spec.dataChunks/parityChunks/stripSizeKB from Volume.Spec.DataLayout)

3. ShardGroupController.syncShards
   -> Creates k+m Shard CRs (name = <volumeName>-<slotIndex>, Spec.Size=0 initially)

4. ShardGroupController.scheduleShards
   -> For each unscheduled Shard CR: calls ShardScheduler.ScheduleShard
   -> Sets Shard.Spec.NodeID, DiskUUID, DiskPath, Size = ceil(volSize/k) + 2*stripBytes

5. ShardGroupController.syncShardInstance (one per Shard CR)
   -> ds.GetInstanceManagerByInstance(shard) -> finds IM on shard node
   -> instanceManagerClient.ShardInstanceCreate -> InstanceCreate(type=shard) on shard IM
   -> SPDK: bdev_lvol_create + nvmf_create_subsystem + nvmf_subsystem_add_ns + nvmf_subsystem_add_listener
   -> On success: Shard.Status.StorageIP = IM pod storage IP, Shard.Status.Port = allocated port

6a. VolumeController.reconcileShardGroup (runs every volume reconcile tick)
    -> User attaches volume: v.Spec.NodeID is set -> e.Spec.NodeID = v.Spec.NodeID
    -> reconcileShardGroup: ShardGroup.Spec.NodeID = e.Spec.NodeID
       (co-locates the ShardGroup process with the engine)

6b. ShardGroupController.syncStatus (runs every ShardGroup reconcile tick)
    -> Builds ShardGroup.Status.ecShardAddressMap from Shard.Status.StorageIP/Port
       (only Shards in ShardStateNormal are included; Failed/Replacing omitted)

7. ShardGroupController.syncProcess - provision the ShardGroup process
   -> Gate (a): ShardGroup.Status.ecShardAddressMap has k+m non-empty addresses
   -> Gate (b): every Shard CR is in ShardStateNormal
   -> Both gates pass: ds.GetInstanceManagerByNode(ShardGroup.Spec.NodeID) -> finds IM on engine node
   -> instanceManagerClient.InstanceCreate(kind=shardgroup, ShardGroupSpec={
        shards: ecShardAddressMap, data_chunks, parity_chunks, strip_size_kb})
   -> SPDK on engine node (ShardGroup process):
        bdev_nvme_attach_controller x(k+m)
        bdev_ec_create(k, m, strip_size_kb, base_bdevs=[nvmf-slot0n1..nvmf-slot5n1])
        bdev_lvol_create_lvstore(bdev_name=<volumeName>-ec, lvs_name=<volumeName>-lvs)
        bdev_lvol_create(lvol_name=<volumeName>, lvs_name=<volumeName>-lvs)
        nvmf_create_subsystem + nvmf_subsystem_add_ns + nvmf_subsystem_add_listener
   -> On success: ShardGroup.Status.{IP, Port, NQN, LvstoreUUID, HeadLvolUUID, ProcessState=Running}

8. VolumeController.openVolumeDependentResources -> isECVolumeReady
   -> Gate: ShardGroup.Status.ProcessState == Running AND ShardGroup.Status.IP != ""
   -> Sets e.Spec.DesireState = Running

9. EngineController / InstanceHandler.EngineCreate (on engine IM)
   -> EngineController.CreateInstance for EC volumes reads ShardGroup.Status.{IP, Port, NQN}
      and passes a single upstream endpoint via EngineInstanceCreateRequest
   -> SPDK on engine process (single-endpoint path, identical shape to single-replica RAID1):
        bdev_nvme_attach_controller(name="nvmf-shardgroup",
                                     traddr=ShardGroup.Status.IP,
                                     trsvcid=ShardGroup.Status.Port,
                                     subnqn=ShardGroup.Status.NQN)
        bdev_raid_create(name=<volumeName>, level=1, base_bdevs=["nvmf-shardgroupn1"])
   -> NVMe-oF export (frontend)

10. EngineFrontend starts -> NVMe-oF target exposed to workload node
    -> Volume.Status.State = Attached, Robustness = Healthy
```

**Key invariants:**
- **Step 7's dual gate** (every slot has an address AND every Shard CR is ShardStateNormal) ensures the ShardGroup process never starts with a partial shard set or a stale-but-non-empty address from a stopped SPDK process. The ShardGroup controller provides shards (steps 3-5) and continuously verifies their liveness via `syncShardInstance` (transitioning to ShardStateFailed when the IM reports the instance as Stopped/Error); the controller's own `syncProcess` observes the resulting state and gates ShardGroup process provisioning.
- **Step 8's single gate** (ShardGroup process Running with a valid endpoint) is all the engine needs to know about EC. The engine sees one upstream endpoint, indistinguishable in shape from a single-replica RAID1 endpoint.

#### EC volume detach - end-to-end round-trip

The following sequence traces a clean volume detach (e.g., pod deleted or volume unmounted). Only the engine is torn down; the ShardGroup process and the shard processes keep running, with their lvstore and head lvol intact. This mirrors RAID1 detach, where only the engine is torn down and replicas keep running with their lvstores intact.

```
1. User or Longhorn detaches volume (v.Spec.NodeID cleared, or VolumeAttachment deleted)
   -> VolumeController.closeVolumeDependentResources
   -> Sets e.Spec.DesireState = InstanceStateStopped
   -> Sets e.Spec.NodeID = ""
   (ShardGroup.Spec.NodeID is NOT cleared - the ShardGroup process keeps running so the
    lvstore + head lvol stay live for the next attach.)

2. EngineController / InstanceHandler.EngineDelete (on engine IM, cleanupRequired=false)
   -> Frontend unexport (NVMe-oF target removed; workload node's initiator disconnects)
   -> bdev_raid_delete(name=<volumeName>)             # engine's raid1 aggregation layer
   -> bdev_nvme_detach_controller(name=<shardgroup-endpoint>)
                                                       # disconnect from ShardGroup's exposed lvol
   (Engine owns no persistent state; teardown touches no lvstore, no lvol, no bdev_ec.
    cleanupRequired=false is the standard close-vs-delete distinction shared with RAID1.)

3. Engine CR reaches CurrentState = InstanceStateStopped
   -> VolumeController.closeVolumeDependentResources detects engine stopped
   -> Volume.Status.State = Detached, Robustness = Unknown

4. ShardGroup process keeps running on ShardGroup.Spec.NodeID
   -> bdev_ec, lvstore, head lvol, NVMe-oF expose all remain active
   -> ShardGroup.Status.{IP, Port, NQN, LvstoreUUID, HeadLvolUUID} unchanged
   -> ShardGroup.Status.ecShardAddressMap remains populated; shard instances unaffected
   -> ShardGroup.Status.State stays at its last known value (healthy/degraded)

5. Shard NVMe-oF exports remain active on shard nodes (still serving the ShardGroup process)
   -> Shard.Status.StorageIP/Port remain set; no InstanceDelete called
```

**No data-destroying calls on detach.** The engine teardown sequence above contains zero calls that touch persistent state: `bdev_raid_delete` removes an in-memory aggregation; `bdev_nvme_detach_controller` closes a network connection. There is no `bdev_lvol_delete`, no `bdev_lvol_delete_lvstore`, no `bdev_ec_delete` in the engine teardown. The lvstore, head lvol, bdev_ec, and shard connections all live in the ShardGroup process, which keeps running across detach. This preserves the user's data unconditionally - re-attach reconnects the engine to the unchanged ShardGroup endpoint and the filesystem mounts the same blockdev with the same UUID, no remount-as-blank, no reformat. The same `cleanupRequired=false` discipline that protects RAID1 replica state during detach now protects EC ShardGroup state symmetrically.

**Re-attach** is the inverse: `e.Spec.NodeID` is set to the new attach node, `e.Spec.DesireState = Running`, the engine `bdev_nvme_attach_controller`s the still-exposed ShardGroup endpoint, builds `bdev_raid1` with one base bdev, and re-exports the NVMe-oF frontend. Volume.Status returns to Attached/Healthy. No bdev_ec rebuild, no lvstore reconstruction, no salvage probe is needed because nothing was destroyed.

**ShardGroup process lifecycle is independent of attach state.** The ShardGroup process is provisioned on volume creation (or first attach) and remains running until volume deletion. This matches RAID1 replicas, which are not torn down on every volume detach. If the operator wants to free engine-node resources between attaches, the ShardGroup process can optionally be migrated or stopped via a separate control-plane policy; the default and recommended behavior is to keep it running for fast re-attach.

#### EC volume deletion - end-to-end round-trip

The following sequence traces permanent volume deletion. The critical ordering invariant is engine -> ShardGroup process -> shards: the engine must disconnect from the ShardGroup endpoint before the ShardGroup process tears down (otherwise the engine sees an NVMe-oF disconnect mid-flight); the ShardGroup process must disconnect from all shard endpoints before shard teardown begins.

```
1. User deletes PVC / Volume CR
   -> VolumeController sets volume.Status.State = VolumeStateDeleting
   -> Deletes all Snapshot CRs
   -> Deletes all Engine CRs; waits for Engine CRs to be fully gone

2. EngineController tears down engine (cleanupRequired=true since e.DeletionTimestamp != nil)
   -> bdev_raid_delete(name=<volumeName>)
   -> bdev_nvme_detach_controller(name=<shardgroup-endpoint>)
   -> Engine CR removed
   (Engine still owns no persistent state - cleanupRequired changes nothing in its teardown
    sequence. The flag is plumbed for symmetry with replica/shardgroup teardown.)

3. VolumeController calls DeleteShardGroup(volume.Name) after all engines confirmed gone
   -> ShardGroup CR DeletionTimestamp set; ShardGroup finalizer blocks GC

4. ShardGroupController deletion path (DeletionTimestamp != nil)
   -> Tear down the ShardGroup process first (cleanupRequired=true since sg.DeletionTimestamp != nil):
       -> InstanceDelete(kind=shardgroup, cleanupRequired=true) on engine node IM
       -> SPDK in ShardGroup process:
              nvmf_subsystem_remove_listener + nvmf_subsystem_remove_ns + nvmf_delete_subsystem
              bdev_lvol_delete(<volumeName>-lvs/<volumeName>)        # head lvol
              bdev_lvol_delete_lvstore(<volumeName>-lvs)             # EC lvol store
              bdev_ec_delete(<volumeName>-ec)                         # EC bdev (async; SPDK drains channels first)
              bdev_nvme_detach_controller x(k+m)                      # disconnect from shards
       (cleanupRequired=true is what authorizes the lvol/lvstore deletes. With cleanupRequired=false
        the ShardGroup process teardown would stop at the NVMe-oF unexpose + bdev disconnects
        and leave the lvstore intact - that is the detach behavior, not deletion.)

   -> cleanupDeletedShards: for each Shard CR with DeletionTimestamp:
       -> InstanceDelete(kind=shard, cleanupRequired=true) on shard node IM
       -> SPDK on shard node: nvmf_subsystem_remove_ns + nvmf_delete_subsystem
                               + bdev_lvol_delete(shard lvol)
       -> Remove longhorn.io/shard-cleanup finalizer -> Shard CR GC'd
   -> cleanupShardGroup: marks any remaining Shard CRs for deletion
   -> Waits for all Shard CRs to be fully gone (no shard finalizers remaining)
   -> Removes ShardGroup finalizer -> ShardGroup CR GC'd

5. VolumeController confirms ShardGroup gone (GetShardGroupRO returns NotFound)
   -> Deletes PV, PVC, LHVolumeAttachment
   -> Removes Volume CR finalizer -> Volume CR GC'd
```

**`cleanupRequired=true` is mandatory for permanent deletion** of both the ShardGroup process and the shard instances. For the ShardGroup process, `cleanupRequired=true` authorizes `bdev_lvol_delete` + `bdev_lvol_delete_lvstore`; passing `false` would leave the lvstore intact and is only correct for the detach path. For each shard, `cleanupRequired=true` authorizes `bdev_lvol_delete` of the shard lvol; passing `false` would leave the chunk lvol on disk and `rebuildCachedLvolObjects` on the next SPDK server restart would rediscover it as an orphan. The same `cleanupRequired` discipline that already governs replica teardown in V2 RAID1 governs both the new ShardGroup-process teardown and the existing shard-instance teardown for V2 EC.

**Unreachable engine node during deletion:** If the engine node hosting the ShardGroup process is permanently unreachable, `GetInstanceManagerByInstance` for the ShardGroup process returns `NotFound`. The ShardGroup controller skips the process `InstanceDelete` and proceeds with shard teardown. The orphaned ShardGroup process (lvstore + bdev_ec) on the unreachable node is harmless: it consumes only memory until the node restarts, at which point the SPDK process is gone and the orphan mechanism (extended to recognize orphaned ShardGroup processes) cleans up any leftover bdev_ec controllers. The shards on other nodes are still cleaned up normally.

**Unreachable shard node during deletion:** If a shard node is permanently unreachable, `GetInstanceManagerByInstance` returns `NotFound`. The ShardGroup controller skips the `InstanceDelete` call and removes the shard finalizer directly. The shard lvol remains on the unreachable node's disk, but the control-plane CR chain is cleanly torn down. The orphan mechanism on the shard node will handle cleanup once the node recovers.

#### EC shard failure - end-to-end round-trip

The following sequence traces a single shard failure from SPDK detection to full recovery.

```
1. Physical disk fails on shard node (or NVMe-oF connection permanently lost)
   -> SPDK on engine node: SPDK_BDEV_EVENT_REMOVE fires for nvmf-slotXn1
   -> EC module: slot X transitions NORMAL -> FAILED, failed_count++
   -> EC bdev continues serving I/O in degraded mode (reads reconstruct via RS decoding)

2. ShardGroupController.syncECHealth (runs every reconcile tick)
   -> ShardGroupGet returns ec_status.slots[X].state = EC_SLOT_STATE_FAILED
   -> Shard CR for slot X: Status.State = failed, Status.LastFailureTimestamp = now()
   -> ShardGroup.Status.FailedCount++, State = degraded (or offline if failedCount > m)
   -> Kubernetes Warning event: ShardFailed on ShardGroup CR

3. ShardGroupController.reconcileFailedShard - debounce phase
   -> shouldDelayReplace: checks time.Since(LastFailureTimestamp) < replenishmentWaitInterval
   -> If not yet elapsed: wait; if slot returns to NORMAL during wait -> no action needed

4. After debounce: reconcileFailedShard -> triggerShardReplace
   -> getLiveShardPort: InstanceGet on the shard's IM to refresh PortStart
     -> If instance is not Running (Stopped/Error/missing): delete Shard CR vol0-X for re-provisioning
        (skip ShardGroupShardReplace - issuing it with a stale address makes the ShardGroup process
         burn its NVMe-oF retry budget on a dead target. Re-provisioning via syncShards on the next
         cycle places a fresh shard with a known-live address.)
     -> If instance is Running: shardAddress = oldIP:livePort (live PortStart, in case IM re-allocated)
   -> ShardGroupShardReplace(shardgroup_id=vol0, shardName=vol0-X, shardAddress=oldIP:livePort)
   -> If success (node recovered transiently): slot FAILED -> REPLACING immediately
   -> If RPC failure: delete Shard CR vol0-X
     -> Shard CR DeletionTimestamp set; finalizer blocks GC

5. cleanupShard (on deleted Shard CR)
   -> InstanceDelete(type=shard, cleanupRequired=true) on failed node
     (cleanupRequired=true: delete the shard lvol once the node is reachable again, preventing stale lvol
      accumulation. If the node is permanently unreachable, the InstanceManager CR is eventually removed by
      the node controller, at which point GetInstanceManagerByInstance returns NotFound, the IM call is
      skipped, and the finalizer is removed. This mirrors the existing replica cleanup pattern.)
   -> Remove finalizer -> Shard CR GC'd

6. syncShards: detects missing slot X -> creates new Shard CR vol0-X (Size=0)
   scheduleShards: places on new node (hard anti-affinity excludes healthy shard nodes)
   -> Shard.Spec.NodeID = newNode, Size = ceil(volSize/k) + 2*stripBytes
   syncShardInstance: InstanceCreate(type=shard) on new node's IM
   -> New lvol created + NVMe-oF exported; Shard.Status.StorageIP/Port set
   NOTE: syncECHealth will set LastFailureTimestamp on the new Shard CR only after StorageIP is set
         (Fix: the timestamp is not set before the instance is provisioned, preventing a premature debounce
         that would have caused a deadlock when reconcileFailedShard tried to issue ShardGroupShardReplace
         before the shard had a valid address). However, once StorageIP is populated and the slot is still
         FAILED in the ShardGroup process, syncECHealth does set LastFailureTimestamp, causing
         shouldDelayReplace to wait the full debounce period again before calling ShardGroupShardReplace.
         This doubles recovery time for replaced shards and is tracked for improvement in a future iteration.

7. reconcileFailedShard - replace phase (new address)
   -> triggerShardReplace: ShardGroupShardReplace(shardgroup_id=vol0, shardName=vol0-X, shardAddress=newIP:port)
   -> ShardGroup process: bdev_nvme_attach_controller(newIP, port) -> new local bdev
   -> ShardGroup process: bdev_ec_replace_base_bdev(ecName, slot=X, newBdev) -> slot FAILED -> REPLACING
   -> Shard.Status.ReplaceTriggered = true

8. reconcileFailedShard - rebuild phase
   -> canStartShardRebuild: checks concurrent-rebuild-per-node-limit
   -> triggerShardRebuild: ShardGroupShardRebuildStart(shardgroup_id=vol0)
   -> ShardGroup process: bdev_ec_start_rebuild -> background poller starts
   -> ShardGroup.Status.RebuildInProgress = true, State = rebuilding

9. syncECHealth (each reconcile tick during rebuild)
   -> ShardGroupGet returns RebuildProgress.PercentComplete
   -> Shard.Status.RebuildProgress updated
   -> syncShardRebuildQoS: ShardGroupShardRebuildQosSet if bandwidth limit configured

10. Rebuild completes
    -> SPDK: slot REPLACING -> NORMAL, failed_count--
    -> syncECHealth: Shard.Status.State = normal, ReplaceTriggered cleared
    -> syncStatus: FailedCount = 0, RebuildInProgress = false -> State = healthy
    -> Volume.Status.Robustness = Healthy
```

**Atomicity guard:** If the manager crashes between steps 7 and 8 (replace done, rebuild not started), `syncECHealth` detects `anyReplacing=true && RebuildInProgress=false` on restart and re-issues `ShardGroupShardRebuildStart`. This is safe because `bdev_ec_start_rebuild` is idempotent.

**Intentional Shard CR delete (fast-path):** Steps 1-3 above describe the unintentional-failure path (~720s end-to-end on default settings: 120s `ctrlr_loss_timeout_sec` + 600s `replica-replenishment-wait-interval`). When a Shard CR is deleted intentionally (admin, eviction, drain), the ShardGroup controller observes the `DeletionTimestamp` while the slot is still NORMAL in `ShardGroupGet`, issues `ShardGroupShardForceFail` to drive the slot to FAILED in one reconcile tick, annotates the CR as intentionally deleted to bypass the debounce on the replacement, and then runs `cleanupShard` and re-provisioning normally. The full sequence completes in seconds rather than minutes. See "Fast-path for intentional Shard CR deletion" above for the full mechanism.

#### Auto-salvage for EC volumes

The existing auto-salvage mechanism (`SettingNameAutoSalvage`) for RAID1 volumes works as follows: (1) `isAutoSalvageNeeded()` checks if zero healthy replicas exist with at least one failed replica, (2) the volume is set to `Faulted` and `e.Spec.SalvageRequested = true`, (3) the volume transitions to `Detached`, (4) the controller identifies "usable" failed replicas (those with `HealthyAt != ""`, valid node/disk, node is up), (5) among usable replicas, those that failed within `AutoSalvageTimeLimit` of the last failure have their `FailedAt` cleared, and (6) `RemountRequestedAt` is set for automatic remount.

For EC volumes, auto-salvage is adapted as follows:

- **Trigger condition:** The Volume controller detects `ShardGroup.Status.State == offline` (i.e., `failedCount > m`, too many failures for EC tolerance). This is analogous to the RAID1 condition where all replicas are `ERR`. The volume is set to `Faulted`.
- **Salvage action:** EC auto-salvage does **not** use `e.Spec.SalvageRequested` - that flag is RAID1-specific and tells the engine to restart accepting a particular replica set. The engine for an EC volume owns no persistent state and has nothing to salvage; salvage applies to the ShardGroup process, not the engine. `autoSalvageECVolume()` in the Volume controller waits for the volume to reach `Detached` state, then - if `AutoSalvage` is enabled and the volume is not a standby or restore target - sets `v.Status.RemountRequestedAt` to trigger a re-attach attempt. On re-attach, the ShardGroup controller's `syncProcess` re-provisions the ShardGroup process; during process startup the bdev_ec is rebuilt with whichever shards are reachable (degraded-mode `bdev_ec_create` tolerates absent slots up to m), and `reconcileFailedShard` calls `ShardGroupShardReplace` with each failed shard's existing address as a **self-healing probe**: if the underlying node recovered (transient NVMe-oF disconnection), the replace succeeds and the shard transitions from FAILED to REPLACING and then NORMAL. If enough slots recover to bring `failedCount <= m`, the volume resumes in DEGRADED mode without any manual intervention.
- **Lvstore preservation across salvage:** Because the lvstore lives on the encoded blocks in the shards (not in the engine or ShardGroup process memory), it survives any failure scenario where at least k shards remain. When the ShardGroup process restarts during salvage and rebuilds bdev_ec, `bdev_examine` automatically re-discovers the lvstore + head lvol superblock from the encoded blocks. No salvage-specific RPC is needed for the lvstore - it is recovered as a side effect of process restart. This is the relocated equivalent of the engine-side `RecreateEC` salvage in the previous design.
- **No last-replica heuristic:** Unlike RAID1 (which picks the "last healthy replica" for salvage), EC salvage does not pick individual shards - it re-evaluates all k+m slots. The EC module's fault tolerance makes this straightforward: any combination of k healthy slots is sufficient.
- **Salvage failure:** If re-probing still shows `failedCount > m`, `isECSalvageNeeded` fires again, the volume goes Faulted and detaches, and `autoSalvageECVolume` triggers another re-attach after a `ECAutoSalvageRetryInterval` cooldown (5 minutes). The cooldown is enforced by comparing the current time against `v.Status.RemountRequestedAt`; if the last trigger was less than 5 minutes ago the function returns early. This prevents tight retry loops when shards remain permanently unrecoverable while still retrying automatically once the cooldown expires. The loop continues until enough shards recover or the operator disables `AutoSalvage` and replaces the failed hardware.

#### Shard eviction and node drain

When a node is cordoned or drained, shards on that node must be gracefully relocated. This mirrors the existing replica eviction mechanism.

**Interaction with `VolumeEvictionController`:** The existing `VolumeEvictionController` handles replica eviction by creating VolumeAttachment tickets to keep the volume attached during eviction. For EC volumes, the `VolumeEvictionController` is extended to detect shard eviction across the full 5-step replacement lifecycle using two signals. It also registers a ShardGroup informer so that when `reconcileEvictedShards` clears `ShardGroupStatus.EvictingSlots` (rebuild complete), the controller is woken immediately and releases the attachment ticket without waiting for the next Volume event.

1. `hasShardEvictionRequested()` - returns true while the original Shard CR (with `Spec.EvictionRequested = true`) still exists (steps 1–2).
2. `hasShardRelocationInProgress()` - returns true while `ShardGroupStatus.EvictingSlots` is non-empty (steps 2–5). This field is set by `reconcileEvictedShards` immediately before deleting the old Shard CR and cleared only after the replacement shard's `Status.State` returns to `ShardStateNormal` with `StorageIP` set, confirming rebuild completion.

The attachment ticket is held as long as either signal is true, ensuring the volume stays attached for the full replacement cycle. The actual replacement logic is handled by the ShardGroup controller's `reconcileEvictedShards()` - not the Volume controller's `EvictReplicas()`.

- **Eviction trigger:** The node controller's `syncShardEvictionRequested()` runs after `syncReplicaEvictionRequested()` on every node reconcile. It lists all Shard CRs on the node via `ListShardsByNode`, resolves each shard's disk spec by matching `Shard.Spec.DiskUUID` against `node.Status.DiskStatus`, and calls `shouldEvictShard()` to determine whether to set `Shard.Spec.EvictionRequested = true`. The eviction condition mirrors replicas: node or disk `EvictionRequested = true`, or node is cordoned with `NodeDrainPolicy = block-for-eviction`. Unlike replicas, there is no `BlockForEvictionIfContainsLastReplica` equivalent for EC - any shard can be rebuilt as long as k slots survive.
- **Eviction process:** `reconcileEvictedShards()` in the ShardGroup controller reuses the existing failure recovery pipeline:
    1. `reconcileEvictedShards` detects any Shard CR with `EvictionRequested = true` and no `DeletionTimestamp`. One shard per reconcile tick is processed.
    2. The slot index is appended to `ShardGroupStatus.EvictingSlots`. The Shard CR is then deleted. Since the node is still up during a drain, `cleanupDeletedShards` calls `InstanceDelete` on the old node immediately (the finalizer does not block). The old NVMe-oF export drops.
    3. The EC engine detects the connection drop and marks the slot FAILED. `syncShards` recreates the Shard CR for the slot; `syncShardInstances` schedules and provisions it on a new node (hard anti-affinity excludes nodes with surviving healthy shards).
    4. Once the new instance is running (`StorageIP` set), `reconcileFailedShard` calls `ShardGroupShardReplace(new_address)` followed by `ShardGroupShardRebuildStart`. The slot rebuilds from the remaining k healthy shards.
    5. On completion the slot transitions REPLACING -> NORMAL. `reconcileEvictedShards` detects `Status.State == ShardStateNormal` with `StorageIP` set for the replacement shard, removes the slot from `ShardGroupStatus.EvictingSlots`, and releases the tracking. The ShardGroup informer registered in `VolumeEvictionController` wakes the controller immediately; it sees both signals are false and deletes the attachment ticket.
- **Eviction ordering:** `reconcileEvictedShards` guards with `RebuildInProgress || FailedCount > 0` and processes only one shard per tick. This ensures at most one slot is in transition at any time, staying within the EC fault tolerance budget of m simultaneous failures.
- **Node drain with insufficient capacity:** If `syncShardInstances` cannot schedule the recreated shard (no node satisfies anti-affinity and capacity), it logs a warning and retries on the next tick. The shard remains unprovisioned until a suitable node becomes available. This is the same retry behavior as the failure recovery path - no separate `EvictionBlocked` event is emitted in the current implementation.

#### Backup and restore for EC volumes

Backup and restore for EC volumes operate at the lvol layer above the EC bdev, making them transparent to the EC encoding:

- **Backup (`EngineBackupCreate`):** The engine reads data through the EC bdev (which handles RS decoding for any failed slots) and streams it to the backup target (S3/NFS) as a standard Longhorn backup. The backup stores raw volume data - not individual shard chunks or parity. The backup metadata records the volume's EC parameters (k, m, strip_size_kb) for informational purposes but does not require them for restore.
- **Restore (`EngineBackupRestore`):** The engine writes restored data through the EC bdev, which handles RS encoding and distributes chunks to the k+m shards. Restore creates a new EC volume with the same (or different) EC parameters - the backup data is layout-agnostic.
- **Cross-mode restore:** A backup from a RAID1 volume can be restored to an EC volume and vice versa. The backup contains raw block data independent of the underlying replication/EC scheme. The user specifies the target volume's EC parameters (or RAID1 replica count) at restore time.

This design means backup performance depends on the EC bdev's read throughput (which may be degraded if slots are FAILED), and restore performance depends on EC write throughput (including RMW overhead for sub-stripe writes during restore).

#### Volume expansion (resize, same k+m)

Standard volume expansion works through the existing Kubernetes CSI expansion flow. EC expansion fans out across three process layers (per-shard, ShardGroup, engine). The key addition is `bdev_ec_resize` inside the **ShardGroup process**, which recalculates EC geometry after shard lvols have grown:
1. User edits PVC `spec.resources.requests.storage` -> larger value.
1. Kubernetes CSI sidecar calls `ControllerExpandVolume` on the Longhorn CSI driver.
1. The Volume controller updates the Volume CR's `spec.size`.
1. `syncShardGrow` sets `GrowInProgress = true` (which `syncStatus` converts to `ShardGroup.status.state = growing` only when the array is healthy). The ShardGroup controller then calls `ShardExpand` gRPC on **each shard node's InstanceManager SPDK service** (`GetRunningInstanceManagerByNodeRO(shard.Spec.NodeID)`), which calls `bdev_lvol_resize` locally on the shard lvol. The ShardGroup-process node cannot call `bdev_lvol_resize` on remote shard lvols directly - `ShardExpand` must go to the shard node's own SPDK service. The `ShardExpand` calls are serial in the current implementation; on any failure the function returns early and the full batch retries on the next reconcile tick. `ShardExpand` is idempotent (resizing an lvol to its current size is a no-op), so the retry is safe. `ShardGroupExpand` is only called after all k+m `ShardExpand` calls have succeeded. If a shard fails while expansion is in flight, `syncShardGrow` is blocked by the `RebuildInProgress || FailedCount > 0` guard; `GrowInProgress` remains `true` but `syncStatus` emits `degraded` or `rebuilding` (not `growing`) until the failure is resolved and expansion can resume.
1. The ShardGroup controller calls `ShardGroupExpand` on the ShardGroup-process node's SPDK service. Internally, the **ShardGroup process** runs the cross-cutting EC stack grow: it calls `bdev_ec_resize` (EC module detects the larger base bdevs, quiesces I/O, updates `blockcnt`/`num_stripes`, reallocates WIB arrays and dirty bitmap, relocates the WIB to the new parity disk tail, unquiesces - quick, no data movement), then `bdev_lvol_grow_lvstore` to grow the lvol store on top of the resized `bdev_ec`, then `bdev_lvol_resize` on the head lvol. `ShardGroupExpand` is idempotent: if it fails, the next reconcile calls `ShardGroupExpandPrecheck` (or equivalent gating logic) which still returns `ExpansionRequired = true`, causing `syncShardGrow` to retry `ShardExpand` (no-op) and `ShardGroupExpand` again. Once `ShardGroupExpand` succeeds, the next precheck returns `ExpansionRequired = false`.
1. The ShardGroup controller calls `EngineExpand` on the engine node's SPDK service. The **engine** does not issue any SPDK call here: when `bdev_lvol_resize` ran on the head lvol inside the ShardGroup process (previous step), SPDK fired `AER_NS_ATTR_CHANGED` on the NVMe-oF subsystem. The engine's `bdev_nvme` initiator processes the AEN ([bdev_nvme.c:5080](https://github.com/spdk/spdk/blob/master/module/bdev/nvme/bdev_nvme.c#L5080)), detects the new namespace size, and calls `spdk_bdev_notify_blockcnt_change` on the local `nvmf-shardgroup` bdev. The raid1 module's `resize` callback fires automatically and grows the raid bdev. `EngineExpand` polls until the engine's raid bdev `blockcnt` matches `new_size` and returns `ok`. The engine does **not** call `bdev_ec_resize` or `bdev_lvol_grow_lvstore` - those happen one layer up in the ShardGroup process - and there is no `bdev_raid_grow` RPC to call (the SPDK raid module exposes only `create`, `delete`, `add_base_bdev`, `remove_base_bdev`, `set_options`, `get_bdevs`; resize is a callback-driven side effect of the upstream resize). Once `EngineExpand` succeeds, `syncShardGrow` clears `GrowInProgress = false` and `syncStatus` derives `state = healthy` (or `degraded`/`rebuilding` if other conditions apply).
1. Kubernetes calls `NodeExpandVolume` -> Longhorn runs `resize2fs`/`xfs_growfs`. Online resize is supported: `resize2fs` (ext4) and `xfs_growfs` (XFS) both support growing a mounted filesystem without unmount or detach. The volume does not need to be detached - this is the same behavior as current V2 replicated volumes.

#### Volume.Status.Robustness mapping

For EC volumes, `Volume.Status.Robustness` is derived from `ShardGroup.status.state` inside `ReconcileEngineReplicaState()`. The function early-returns from all RAID1 replica logic when `v.Spec.DataLayout.Type == VolumeDataLayoutTypeSharded`, then reads `ShardGroup.status.failedCount` to compute robustness directly - it does not inspect `Engine.Status.ReplicaModeMap` for EC volumes. The mapping:

| ShardGroup.status.state | Volume.Status.Robustness | Condition |
|---|---|---|
| `healthy` | `VolumeRobustnessHealthy` | All k+m slots NORMAL; `failedCount == 0`, `RebuildInProgress == false` |
| `degraded` | `VolumeRobustnessDegraded` | 1..m slots FAILED, `RebuildInProgress == false` |
| `rebuilding` | `VolumeRobustnessDegraded` | `RebuildInProgress == true` and array not offline; one or more slots REPLACING - `failedCount` may be 0 but array is not yet fully redundant |
| `offline` | `VolumeRobustnessFaulted` | >m slots FAILED, volume I/O rejected |
| `growing` | `VolumeRobustnessHealthy` | Expansion in progress; set when `GrowInProgress == true` and array is healthy (not offline, degraded, or rebuilding), cleared when `EngineExpandPrecheck` returns `ExpansionRequired = false` on the reconcile tick following successful `EngineExpand`. If a shard fails during expansion, `syncShardGrow` is blocked by the `FailedCount > 0` guard and the state falls back to `degraded` or `rebuilding` until the failure is resolved. |

The Volume controller reads `ShardGroup.status.state` in its `ReconcileEngineReplicaState()` path. For RAID1 volumes, the controller inspects `Engine.Status.ReplicaModeMap` and counts healthy replicas. For EC volumes, the equivalent is reading `ShardGroup.status.failedCount` and comparing against `m`:
- `failedCount == 0` -> Healthy
- `0 < failedCount <= m` -> Degraded
- `failedCount > m` -> Faulted

This ensures downstream consumers (UI, metrics, alerts, `kubectl get volumes`) see the same `Robustness` field regardless of whether the volume uses RAID1 or EC.

#### spec.dataLayout

`spec.dataLayout` is a struct that declares the user's intended data layout. It is **immutable after creation** - the entire struct, including all sub-fields, is treated as a single immutable unit by the Volume webhook (see Volume webhook immutable fields above).

Sub-fields:
| Field | Type | Meaning |
|---|---|---|
| `type` | `string` | Topology selector: `replicated` or `sharded` |
| `mode` | `string` | Mechanism within the type: `""`, `raid1`, `erasureCoding` |
| `dataChunks` | `int` | EC k parameter. Required when `type: sharded`; zero for replicated volumes. |
| `parityChunks` | `int` | EC m parameter. Required when `type: sharded`; zero for replicated volumes. |
| `stripSizeKB` | `int` | EC chunk size in KiB. Required when `type: sharded`; zero for replicated volumes. |

Valid `(type, mode)` combinations:
| `dataLayout.type` | `dataLayout.mode` | Meaning |
|---|---|---|
| `replicated` | `""` | V1 replication (Longhorn replica mechanism, no SPDK RAID1) |
| `replicated` | `raid1` | V2 RAID1 replication via SPDK `bdev_raid_create` |
| `sharded` | `erasureCoding` | V2 EC sharding: k+m chunks distributed across nodes via Reed-Solomon coding, node boundary broken |

`type` is the authoritative mode selector - when `type: sharded`, the Volume controller activates the EC path (creates ShardGroup CR, skips Replica CRs) regardless of other spec fields. `mode` refines the specific mechanism within the type and is provided for clarity and future extensibility. The separation allows new topologies (e.g., local EC where all shards reside on the same node) or new mechanisms to be added without a breaking API change.

Grouping the EC parameters under `spec.dataLayout` rather than as separate top-level VolumeSpec fields makes the semantic relationship explicit in the schema: `dataChunks`, `parityChunks`, and `stripSizeKB` are only meaningful when `type: sharded`. A replicated volume's spec shows `dataLayout: {type: replicated, mode: raid1}` with no EC fields visible. A sharded volume's spec shows `dataLayout: {type: sharded, mode: erasureCoding, dataChunks: 4, parityChunks: 2, stripSizeKB: 64}`. This grouping also simplifies the webhook: instead of five separately-immutable fields, the entire sub-struct is immutable - one rule, no risk of forgetting a field.

Note: `numberOfReplicas` is logically related to `dataLayout` (it is `1` for EC volumes, `2-3` for RAID1 volumes), but is kept as a top-level VolumeSpec field for backward compatibility. For EC volumes, `numberOfReplicas` must be 1.

**Existing volumes (upgrade compatibility):** Volumes created before v1.12 have an empty `spec.dataLayout` (zero-value struct). The Volume controller treats empty `spec.dataLayout.type` as `replicated`, and sets `spec.dataLayout.mode` based on `spec.dataEngine`: `""` for V1, `raid1` for V2. No user action is required - existing volumes continue to operate as RAID1.

#### Monitoring and metrics:

>v1.12

#### Clone support for EC volumes

**Initial release: `SnapshotClone` returns `Unimplemented` for EC engines.** The design below describes the intended future implementation.

Clone operations on EC volumes work at the lvol layer above the EC bdev. Since the lvol store (including the snapshot chain) lives in the **ShardGroup process** on top of `bdev_ec`, `EngineSnapshotClone` forwards to `ShardGroupSnapshotClone`, which operates on the ShardGroup-process-side lvol - the same as any other lvol operation in that process. The cloned volume is a new EC volume with its own ShardGroup and Shard CRs (and its own ShardGroup process); the clone's data is initially backed by the source volume's snapshot (copy-on-write at the blobstore layer) and diverges as writes occur.

**Constraint:** Both the source and clone volumes must be attached to the same engine node during the clone operation (because the clone is a blobstore-level operation on the shared EC bdev). After the clone completes, the clone volume can be detached and re-attached to a different node.

**Enforcement:** The Volume controller's `checkAndInitVolumeClone()` enforces this by setting the clone volume's `v.Spec.NodeID` to match the source volume's `Engine.Spec.NodeID` before triggering attach. If the source volume's engine node is unavailable, the clone is deferred until the source is attached. The Volume webhook validates that a clone request targeting an EC source volume does not specify a conflicting `NodeID`. During cloning, only one replica (or one ShardGroup for EC) is created - additional replicas/shards are deferred until the clone completes, consistent with the existing `getReplenishReplicasCount()` clone constraint.

#### Volume Deletion

When a Volume CR is deleted, the Volume controller follows the same deletion sequence as the existing V2 flow, with ShardGroup replacing Replicas:

1. Set `volume.Status.State = VolumeStateDeleting`.
1. Delete all Snapshot CRs.
1. Delete all Engine CRs.
1. **Wait for all engines to be fully stopped/gone** - this is critical for V2 to avoid SPDK "no such device" errors. The engine must be torn down before shards.
1. Volume controller explicitly calls `DeleteShardGroup(volume.Name)` - the ShardGroup controller's deletion path (`DeletionTimestamp != nil`) calls `cleanupShard` on each child Shard CR with a DeletionTimestamp, and `cleanupShardGroup` marks remaining Shard CRs for deletion. Each Shard CR's finalizer (`longhorn.io/shard-cleanup`) ensures `InstanceDelete` on the shard node tears down the NVMe-oF export and deletes the lvol before the finalizer is removed. Kubernetes owner references on Shard CRs are set for observability but the deletion sequencing is driven explicitly by the ShardGroup controller - not by Kubernetes cascade GC.
1. Volume controller waits (`GetShardGroupRO` returns NotFound) for the ShardGroup and all Shard CRs to be fully gone.
1. Delete PV, PVC, and LHVolumeAttachment.
1. Remove the Volume CR finalizer.

For remote shards, the InstanceManager on the engine node calls `bdev_nvme_detach_controller` (as part of engine teardown in step 3-4), and the InstanceManager on each shard node tears down the exported NVMe-oF subsystem and deletes the shard lvol (as part of shard cleanup in step 5).

Note: A separate Shard controller is not needed - individual Shard CR lifecycle is managed by the ShardGroup controller and the InstanceManager on each node.


### Test plan

Integration test plan.

For engine enhancement, also requires engine integration test plan.

**Critical test categories:**

1. **EC volume CRUD lifecycle**
    - Create EC volume (4+2, 64KB strip) -> verify k+m Shard CRs created, ShardGroup CR healthy, engine running
    - Attach EC volume -> mount filesystem, write data, read back, verify integrity
    - Detach EC volume -> verify clean teardown (frontend unexport, EC bdev delete, NVMe-oF detach)
    - Delete EC volume -> verify ShardGroup and Shard CRs garbage collected, shard lvols deleted on shard nodes

2. **Snapshot/Clone/Revert on EC volume**
    - Create snapshot -> verify snapshot lvol created above EC bdev, data readable from snapshot
    - Clone from snapshot -> verify clone volume operational
    - Revert to snapshot -> verify data matches snapshot state
    - Purge snapshot -> verify space reclaimed

3. **Online expansion**
    - Expand PVC -> verify all k+m ShardExpand calls succeed, ShardGroupExpand calls bdev_ec_resize + bdev_lvol_grow_lvstore + bdev_lvol_resize, EngineExpand grows raid1, resize2fs succeeds
    - Partial ShardExpand failure -> verify retry and eventual success

4. **Single shard failure -> degraded -> replace -> rebuild -> healthy**
    - Fail one shard (simulate disk removal) -> verify EC bdev transitions to DEGRADED, ShardGroup status.state = degraded
    - Read/write during degraded mode -> verify I/O succeeds via RS reconstruction
    - Replacement shard scheduled -> verify ShardGroupShardReplace + ShardGroupShardRebuildStart called
    - Poll rebuild progress -> verify ShardGroupShardRebuildProgress reports advancing stripe count
    - Rebuild completes -> verify slot transitions to NORMAL, ShardGroup returns to healthy

5. **Multi-shard failure within tolerance**
    - Fail m shards simultaneously -> verify EC bdev remains DEGRADED, I/O succeeds
    - Replace and rebuild all failed shards -> verify recovery to healthy

6. **Multi-shard failure exceeding tolerance**
    - Fail m+1 shards -> verify EC bdev transitions to OFFLINE, I/O rejected
    - Replace enough shards to bring failure count ≤ m -> verify recovery to DEGRADED, then rebuild to healthy

7. **Engine crash + WIB scrub recovery**
    - Kill engine process during active writes -> restart engine -> verify WIB loaded, scrub runs, data consistent after scrub
    - Verify reads succeed during scrub, RMW writes to unscrubbed regions are requeued

8. **Engine crash + disk failure during scrub window**
    - Kill engine during writes -> fail one shard before scrub completes -> verify degraded reads in dirty regions return EIO (not corrupt data)
    - Verify recovery after shard replacement + rebuild + deferred scrub

9. **Manager crash between ShardGroupShardReplace and ShardGroupShardRebuildStart**
    - Crash manager after hot-swap but before rebuild-start -> restart manager -> verify ShardGroup controller detects REPLACING state and re-issues ShardGroupShardRebuildStart

10. **Live migration of EC volume**
    - Migrate EC volume to new engine node -> verify both engines connect to same shards, frontend switchover succeeds, data intact

11. **Shard node reboot (transient failure, no rebuild)**
    - Reboot shard node -> verify NVMe-oF reconnects within timeout, slot remains NORMAL, no rebuild triggered

12. **Rebuild interrupted by second shard failure**
    - Start rebuild on slot A -> fail slot B during rebuild -> verify behavior depends on total failed_count vs m (continue rebuild with degraded reads, or transition to OFFLINE)

13. **Shard eviction with node drain**
    - Drain a shard node -> verify VolumeEvictionController creates attachment ticket, ShardGroup controller evicts one shard at a time, rebuild completes before next eviction starts
    - Drain a node on a cluster with exactly k+m nodes -> verify shard remains on draining node (no suitable anti-affinity candidate), `syncShardInstances` retries each reconcile tick until capacity becomes available

14. **Cross-mode backup/restore**
    - Backup a RAID1 volume -> restore as EC volume -> verify data integrity
    - Backup an EC volume -> restore as RAID1 volume -> verify data integrity
    - Backup an EC volume (4+2) -> restore as EC volume with different parameters (3+1) -> verify data integrity

15. **Concurrent EC volume creation**
    - Create multiple EC volumes simultaneously -> verify scheduler respects per-volume node anti-affinity (different volumes can share nodes), all volumes reach healthy state

### Upgrade strategy

- V2 Sharding is a new feature, not a migration from existing V2 RAID1 volumes, no in-place upgrade of existing volume is planned for v1.12.0
- The new ShardGroup and Shard CRDs are installed as part of the Helm chart upgrade (or `kubectl apply` of the updated CRD manifests). On upgrade from v1.11 to v1.12, no ShardGroup or Shard CRs exist - the new controllers gracefully handle this (they simply have no work to do and process no events until an EC volume is created).

**`spec.dataLayout` for existing volumes**

Volumes created before v1.12 have an empty `spec.dataLayout` (zero-value struct). The Volume controller treats empty `spec.dataLayout.type` as `replicated` - no migration or user action is required. Existing volumes are unaffected and continue operating as RAID1.

## Initial release scope

### Feature support matrix: EC vs V2 replication

This table compares Longhorn features across V2 RAID1 replication and the EC sharding initial release. Features marked ✗ return `Unimplemented` or are blocked by a webhook/guard, and are candidates for future releases.

| Feature | V2 Replication | EC Initial Release | Notes |
|---|---|---|---|
| Volume create / attach / detach / delete | ✓ | ✓ | |
| Online volume expansion (resize) | ✓ | ✓ | EC requires all k+m `ShardExpand` calls to succeed before `EngineExpand` |
| `SnapshotCreate` / `Delete` / `Revert` / `Purge` | ✓ | ✓ | EC: engine forwards to `ShardGroupSnapshot*`; lvol operations happen in the ShardGroup process (no per-shard forwarding) |
| `SnapshotHash` / `HashStatus` | ✓ | ✗ | Returns `Unimplemented`; deferred to future release |
| `SnapshotClone` | ✓ | ✗ | Returns `Unimplemented`; cross-volume lvol attach not yet implemented for EC (see "Clone support for EC volumes") |
| Backup (`BackupCreate` / `BackupStatus`) | ✓ | ✗ | Returns `Unimplemented`; backup path delegates to per-replica RPCs that have no EC equivalent |
| Restore (`BackupRestore` / `RestoreStatus` / `RestoreFinish`) | ✓ | ✗ | Returns `Unimplemented`; same constraint as backup |
| DR volume (standby) | ✓ | ✗ | Deferred; depends on backup/restore support |
| Shard/replica failure detection + degraded mode | ✓ | ✓ | EC: slot FAILED detected via `bdev_ec_get_bdevs` polling in `validateAndUpdateECNoLock` |
| Shard/replica replacement + rebuild | ✓ | ✓ | EC: two-step `ShardGroupShardReplace` + `ShardGroupShardRebuildStart` (both target the ShardGroup process) |
| Rebuild bandwidth control (QoS) | ✓ | ✓ | EC: `ShardGroupShardRebuildQosSet` wrapping `bdev_ec_set_rebuild_qos` (ShardGroup process) |
| Crash recovery (engine node) | ✓ | ✓ | EC: `bdev_ec_create` with WIB load + startup scrub; `RecreateEC` tolerates missing shards |
| Crash recovery (shard / replica node) | ✓ | ✓ | EC: lvols re-detected by `IsProbablyShardName` on startup; `Shard.Sync()` is a passive validator (mirrors `Replica.Sync`) - walks discovered shards to `Stopped`; ShardGroup controller's FAILED -> replace path handles re-provisioning |
| Auto-salvage | ✓ | ✓ | EC: triggered by `ShardGroup.status.state == offline` (analogous to all-replica-failed condition) |
| Node drain / shard eviction | ✓ | ✓ | EC: evicts one shard at a time; waits for rebuild to complete before next eviction |
| Live migration | ✓ | ✗ | Deferred; EC volumes are non-migratable in initial release |
| `InstanceReplace` (engine live-upgrade) | ✓ | ✗ | Returns `Unimplemented` for shard type; not applicable to EC engines in initial release |
| `dataLocality: bestEffort` / `strictLocal` | ✓ | ✗ | Incompatible with EC - data is distributed across k+m nodes by design; only `dataLocality: disabled` is accepted |
| Prometheus metrics / monitoring | ✓ | ✗ | EC-specific metrics deferred to a post-initial-release iteration |
| In-place RAID1 -> EC migration | N/A | ✗ | Not planned; use backup/restore or application-level replication to migrate data |

### What is new in EC that has no V2 replication equivalent

| EC feature | Description |
|---|---|
| `ShardCreate` / `Delete` / `Get` / `List` / `Expand` / `Watch` | New `Shard*` gRPC methods for managing per-slot lvol lifecycle on shard nodes |
| `ShardGroupShardReplace` | Hot-swap a failed slot via `bdev_ec_replace_base_bdev` inside the ShardGroup process; conceptual analogue of `EngineReplicaAdd` for EC |
| `ShardGroupShardRebuildStart` / `Progress` / `Stop` / `QosSet` | EC rebuild lifecycle on the ShardGroup process; no RAID1 equivalent (RAID1 rebuild is driven per-replica) |
| `ShardGroupCreate` / `Delete` / `Get` / `List` / `Watch` | ShardGroup process lifecycle (own bdev_ec + lvstore + head lvol + NVMe-oF expose); no RAID1 equivalent |
| `ShardGroupExpand` / `ShardGroupExpandPrecheck` | EC stack grow (`bdev_ec_resize` + `bdev_lvol_grow_lvstore` + head `bdev_lvol_resize`) inside the ShardGroup process; precheck blocks if scrub / rebuild / existing resize is in progress |
| ShardGroup CR + controller | Orchestrates k+m shard placement, health aggregation, rebuild, and expansion |
| Shard CR | One per EC slot; replaces Replica CRs for EC volumes |
| WIB (write-intent bitmap) | Persistent parity-consistency guard for engine-node crash recovery; no RAID1 equivalent |
| Startup scrub | Re-encodes parity for dirty regions after crash; runs in background on EC bdev registration |

## Note [optional]

### Future improvements

- **DR volume (standby) for EC volumes:** DR (Disaster Recovery) volumes in standby mode are not supported for EC volumes in v1.12. In the current RAID1 architecture, a DR volume is a read-only standby volume that continuously restores from the latest backup of a primary volume. For EC volumes, this would require the standby volume to maintain its own EC shard group and engine stack while periodically pulling incremental backups. The backup/restore path is EC-transparent (backups contain raw block data, not EC-encoded chunks), so the standby volume's EC parameters can differ from the primary's. However, the orchestration complexity (maintaining a standby ShardGroup + Engine that is always restoring but never serving foreground I/O, coordinating incremental restore with the EC bdev's WIB state, handling shard failures on the standby cluster) is significant. This is deferred to a future release.
- **Monitoring and metrics for EC volumes:** EC-specific Prometheus metrics (per-slot health, rebuild progress, WIB dirty region count, RMW in-flight count, scrub progress) are deferred to a post-v1.12 release.
- **In-place RAID1-to-EC migration:** Converting an existing V2 RAID1 volume to EC sharding without data loss (redistribute replica data into k+m shards) is not planned. Users should create a new EC volume and migrate data via backup/restore or application-level replication.
- **Configurable rebuild poller interval:** The rebuild poller interval (`EC_REBUILD_POLL_PERIOD_US = 100`) is currently a compile-time constant. Making this configurable per-volume via the control plane is a future enhancement.
- **Self-healing probe during debounce:** The current failure recovery debounce simply waits `replica-replenishment-wait-interval` before replacing a failed shard. A future improvement would issue `ShardGroupShardReplace` with the shard's existing address as an active probe during the wait window - if the node recovered transiently, the replace succeeds and rebuild starts immediately, cutting recovery time to seconds instead of the full wait interval. (The complementary `ShardGroupShardForceFail` fast-path already handles the **intentional**-delete case; the self-healing probe targets the unintentional/transient-failure case.)
- **Doubled debounce on replaced shard:** As described in `EC shard failure - end-to-end round-trip` step 6, once `StorageIP` is populated on a freshly re-provisioned Shard CR and the slot is still FAILED in SPDK, `syncECHealth` records a new `LastFailureTimestamp` and `shouldDelayReplace` applies the full `replica-replenishment-wait-interval` debounce a second time. The intentional-delete fast-path bypasses this via the `longhorn.io/intentional-delete` annotation, but the unintentional-failure path still pays the doubled wait. A future improvement is to suppress the second debounce uniformly when the prior CR's failure was already past its debounce window.

### Current limitation
- **In-place resize requires base bdevs to remain open.** The EC module detects base bdev removal as disk failure. If the underlying bdev type (e.g., AIO) does not support live size notification, the base bdevs cannot be deleted and recreated to pick up the new size - doing so triggers REMOVE events that mark EC slots as FAILED and take the bdev OFFLINE. Base bdevs must support in-place resize (e.g., lvol resize via `bdev_lvol_resize`) for `bdev_ec_resize` to work correctly.
- **ISA-L Reed-Solomon** requires `n=k+m <= 255`. The compile-time limit `EC_MAX_BASE_BDEVS` is set to 32.
- **Per-Stripe RMW serialization:** only one RMW can be in flight per stripe at a time. Concurrent writes to different stripes proceed in parallel. This may limit IOPS for small-random-write-heavy workloads targeting the same stripe.
- **WIB region granularity tradeoff:** one dirty bit covers 1024 stripes per dirty region, even if only one stripe was mid-write. Finer granularity would reduce scrub time but increase the on-disk bitmap size and persist frequency.
- **Crash + disk failure during the scrub window:** if an engine crash leaves WIB regions dirty and a second failure (a shard disk) happens before the startup scrub finishes those regions, the EC layer protects against silent corruption by gating I/O rather than reconstructing from possibly-stale parity:
    - **Degraded reads** in dirty regions return EIO immediately (`ec_submit_degraded_read` checks `ec_wib_region_is_dirty` before reconstructing). The `degraded_read_eio_dirty` counter exposed in `bdev_get_bdevs` tracks how often this fires.
    - **RMW writes** to stripes in dirty regions are requeued via NOMEM until the scrub clears the region (active-scrub case) or until the rebuild completes and a deferred scrub runs (degraded-with-failed-data-disk case).
    - **Full-stripe writes** to stripes the scrubber is currently processing are also requeued, closing a race where the scrubber would otherwise overwrite the new parity with parity computed from the data it read just before the write landed.
    - The startup scrub is deferred entirely if any data slot is non-NORMAL at bdev creation, and is restarted automatically once a rebuild restores all data slots to NORMAL. Dirty region bits remain set across this window, so the read and write guards above stay in effect - the deferral check alone is not sufficient; the guards are what makes the deferred period safe.

    **What this does not recover**: stripes physically mid-RMW at the crash moment have data and parity bytes that genuinely do not agree on disk, and the EC layer cannot reconstruct the intended pre-crash value - that information was lost when the engine died. After the scrub the application sees honest EIO for those stripes until the scrub re-encodes parity to match whatever data did reach disk (making the stripe self-consistent at its stabilised value, not at the pre-crash intended value) or until the application overwrites the affected region with a full-stripe write. Recovery of the original intended bytes is the responsibility of higher layers (filesystem journal, database WAL), which detect the EIO and replay or restore as appropriate. The EC layer's contract is "no silent corruption", not "no data loss".

