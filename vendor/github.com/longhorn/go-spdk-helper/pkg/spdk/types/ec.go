package types

type BdevEcState string

const (
	BdevEcStateOnline   = BdevEcState("online")
	BdevEcStateDegraded = BdevEcState("degraded")
	BdevEcStateOffline  = BdevEcState("offline")
)

type BdevEcSlotState string

const (
	BdevEcSlotStateNormal    = BdevEcSlotState("normal")
	BdevEcSlotStateFailed    = BdevEcSlotState("failed")
	BdevEcSlotStateReplacing = BdevEcSlotState("replacing")
)

type BdevEcSlotRole string

const (
	BdevEcSlotRoleData   = BdevEcSlotRole("data")
	BdevEcSlotRoleParity = BdevEcSlotRole("parity")
)

type BdevEcRebuildState string

const (
	BdevEcRebuildStateIdle    = BdevEcRebuildState("idle")
	BdevEcRebuildStateRunning = BdevEcRebuildState("running")
	BdevEcRebuildStateDone    = BdevEcRebuildState("done")
	BdevEcRebuildStateError   = BdevEcRebuildState("error")
)

// EcBaseBdev represents one base device slot in the EC array.
type EcBaseBdev struct {
	Name         string          `json:"name"`
	Slot         uint32          `json:"slot"`
	Role         BdevEcSlotRole  `json:"role"`
	State        BdevEcSlotState `json:"state"`
	NeedsRebuild bool            `json:"needs_rebuild,omitempty"`
}

// BdevEcInfo is the response element for bdev_ec_get_bdevs.
// State is not returned by the SPDK JSON-RPC; it is derived by the Go layer
// from FailedCount and Offline: offline → "offline", failed_count > 0 → "degraded", else → "online".
type BdevEcInfo struct {
	Name              string                 `json:"name"`
	DataChunks        uint32                 `json:"k"`
	ParityChunks      uint32                 `json:"m"`
	TotalChunks       uint32                 `json:"n"` // total number of base devices (data + parity)
	StripSizeKB       uint32                 `json:"strip_size_kb"`
	FailedCount       uint32                 `json:"failed_count"`
	Offline           bool                   `json:"offline"`
	ReplaceInProgress bool                   `json:"replace_in_progress"`
	RebuildInProgress bool                   `json:"rebuild_in_progress"`
	RmwInFlight       uint32                 `json:"rmw_in_flight"`
	RmwDirtyStripes   uint64                 `json:"rmw_dirty_stripes"`
	RebuildProgress   *BdevEcRebuildProgress `json:"rebuild_progress,omitempty"`
	BaseBdevs         []EcBaseBdev           `json:"base_bdevs"`
	State             BdevEcState            `json:"-"`
}

// BdevEcCreateRequest is the request for bdev_ec_create.
type BdevEcCreateRequest struct {
	Name             string   `json:"name"`
	DataChunks       uint32   `json:"data_chunk_count"`
	ParityChunks     uint32   `json:"parity_chunk_count"`
	StripSizeKB      uint32   `json:"strip_size_kb"`
	BaseBdevs        []string `json:"base_bdevs"`
}

// BdevEcDeleteRequest is the request for bdev_ec_delete.
type BdevEcDeleteRequest struct {
	Name string `json:"name"`
}

// BdevEcGetBdevsRequest is the request for bdev_ec_get_bdevs.
// An empty Name lists all EC bdevs.
type BdevEcGetBdevsRequest struct {
	Name string `json:"name,omitempty"`
}

// BdevEcReplaceBaseBdevRequest is the request for bdev_ec_replace_base_bdev.
type BdevEcReplaceBaseBdevRequest struct {
	Name        string `json:"ec_name"`
	Slot        uint32 `json:"slot"`
	NewBdevName string `json:"new_bdev_name"`
}

// BdevEcReplaceBaseBdevResponse is the response for bdev_ec_replace_base_bdev.
// State is always "replacing" on success; NeedsRebuild is always true on success.
type BdevEcReplaceBaseBdevResponse struct {
	EcName      string          `json:"ec_name"`
	Slot        uint32          `json:"slot"`
	NewBdevName string          `json:"new_bdev_name"`
	State       BdevEcSlotState `json:"state"`
	NeedsRebuild bool           `json:"needs_rebuild"`
}

// BdevEcStartRebuildRequest is the request for bdev_ec_start_rebuild.
type BdevEcStartRebuildRequest struct {
	Name string `json:"ec_name"`
}

// BdevEcStartRebuildResponse is the response for bdev_ec_start_rebuild.
type BdevEcStartRebuildResponse struct {
	EcName     string `json:"ec_name"`
	NumStripes uint64 `json:"num_stripes"`
	FirstSlot  uint32 `json:"first_slot"`
}

// BdevEcGetRebuildProgressRequest is the request for bdev_ec_get_rebuild_progress.
type BdevEcGetRebuildProgressRequest struct {
	Name string `json:"ec_name"`
}

// BdevEcStopRebuildRequest is the request for bdev_ec_stop_rebuild.
type BdevEcStopRebuildRequest struct {
	Name string `json:"ec_name"`
}

// BdevEcSetRebuildQosRequest is the request for bdev_ec_set_rebuild_qos.
type BdevEcSetRebuildQosRequest struct {
	Name             string `json:"ec_name"`
	MaxStripesPerSec uint32 `json:"max_stripes_per_sec"`
	Paused           bool   `json:"paused"`
}

// BdevEcResizeRequest is the request for bdev_ec_resize.
type BdevEcResizeRequest struct {
	Name string `json:"ec_name"`
}

// BdevEcResizeResponse is the response for bdev_ec_resize.
type BdevEcResizeResponse struct {
	EcName      string `json:"ec_name"`
	OldBlockcnt uint64 `json:"old_blockcnt"`
	NewBlockcnt uint64 `json:"new_blockcnt"`
}

// BdevEcGetWibStatusRequest is the request for bdev_ec_get_wib_status.
type BdevEcGetWibStatusRequest struct {
	Name string `json:"ec_name"`
}

// BdevEcGetScrubProgressRequest is the request for bdev_ec_get_scrub_progress.
type BdevEcGetScrubProgressRequest struct {
	Name string `json:"ec_name"`
}

// BdevEcRebuildProgress is the response for bdev_ec_get_rebuild_progress.
// It is also embedded in BdevEcInfo when RebuildInProgress is true.
// RebuildState is not returned by the SPDK JSON-RPC; it is derived by the Go layer:
// -ENOENT → "idle", percent_complete == 100 → "done", non-ENOENT error → "error", otherwise → "running".
type BdevEcRebuildProgress struct {
	EcName          string             `json:"ec_name"`
	CurrentSlot     uint32             `json:"current_slot"`
	CurrentStripe   uint64             `json:"current_stripe"`
	NumStripes      uint64             `json:"num_stripes"`
	StripesRebuilt  uint64             `json:"stripes_rebuilt"`
	SlotsToRebuild  uint32             `json:"slots_to_rebuild"`
	PercentComplete uint32             `json:"percent_complete"`
	RebuildState    BdevEcRebuildState `json:"-"`
}

// BdevEcWibStatus is the response for bdev_ec_get_wib_status.
type BdevEcWibStatus struct {
	EcName         string `json:"ec_name"`
	NumRegions     uint32 `json:"num_regions"`
	DirtyRegions   uint32 `json:"dirty_regions"`
	Generation     uint32 `json:"generation"`
	PersistPending bool   `json:"persist_pending"`
}

// BdevEcScrubProgress is the response for bdev_ec_get_scrub_progress.
type BdevEcScrubProgress struct {
	EcName            string `json:"ec_name"`
	CurrentRegion     uint32 `json:"current_region"`
	NumRegions        uint32 `json:"num_regions"`
	TotalDirtyRegions uint32 `json:"total_dirty_regions"`
	CurrentStripe     uint64 `json:"current_stripe"`
	StripesScrubbed   uint64 `json:"stripes_scrubbed"`
	RegionsScrubbed   uint64 `json:"regions_scrubbed"`
	PercentComplete   uint32 `json:"percent_complete"`
}
