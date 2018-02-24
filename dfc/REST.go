// Package dfc provides distributed file-based cache with Amazon and Google Cloud backends.
/*
 * Copyright (c) 2017, NVIDIA CORPORATION. All rights reserved.
 *
 */
package dfc

import "time"

// JSON-formatted control structures
type ActionMsg struct {
	Action string `json:"action"` // shutdown, restart, setconfig - the enum below
	Name   string `json:"name"`   // action-specific params
	Value  string `json:"value"`
}

// ActionMsg.Action enum
const (
	ActShutdown     = "shutdown"
	ActSyncSmap     = "syncsmap"  // synchronize cluster map aka Smap across all targets
	ActRebalance = "rebalance" // rebalance local caches upon target(s) joining and/or leaving the cluster
	ActLRU          = "lru"
	ActSyncLB       = "synclb"
	ActCreateLB     = "createlb"
	ActDestroyLB    = "destroylb"
	ActSetConfig    = "setconfig"
	ActRename       = "rename"
	ActEvict        = "evict"
)

// TODO: sort and some props are TBD
type GetMsg struct {
	GetWhat       string `json:"what"`        // "config" | "stats" ...
	GetSort       string `json:"sort"`        // "ascending, atime" | "descending, name"
	GetProps      string `json:"props"`       // e.g. "checksum, size" | "atime, size" | "ctime, iscached" | "bucket, size"
	GetTimeFormat string `json:"time_format"` // "RFC822" default - see the enum below
}

//===================
//
// RESTful GET
//
//===================

// GetMsg.GetWhat enum
const (
	GetWhatFile   = "file" // { "what": "file" } is implied by default and can be omitted
	GetWhatConfig = "config"
	GetWhatSmap   = "smap"
	GetWhatStats  = "stats"
)

// GetMsg.GetSort enum
const (
	GetSortAsc = "ascending"
	GetSortDes = "descending"
)

// GetMsg.GetTimeFormat enum
const (
	RFC822     = time.RFC822     // default
	Stamp      = time.Stamp      // e.g. "Jan _2 15:04:05"
	StampMilli = time.StampMilli // e.g. "Jan 12 15:04:05.000"
	RFC822Z    = time.RFC822Z
	RFC1123    = time.RFC1123
	RFC1123Z   = time.RFC1123Z
	RFC3339    = time.RFC3339
)

// GetMsg.GetProps enum
const (
	GetPropsChecksum = "checksum"
	GetPropsSize     = "size"
	GetPropsAtime    = "atime"
	GetPropsCtime    = "ctime"
	GetPropsIsCached = "iscached"
	GetPropsBucket   = "bucket"
)

//===================
//
// Bucket Listing <= GET /bucket result set
//
//===================

// file and directory metadata in response to the GetMsg
type BucketEntry struct {
	Name     string `json:"name"`     // name of the object - note: does not include the bucket name
	Size     int64  `json:"size"`     // size in bytes
	Ctime    string `json:"ctime"`    // formatted as per GetMsg.GetTimeFormat
	Checksum string `json:"checksum"` // checksum
	Type     string `json:"type"`     // "file" OR "directory"
	Atime    string `json:"atime"`    // ditto
	Bucket   string `json:"bucket"`
}

type BucketList struct {
	Entries []*BucketEntry `json:"entries"`
}

// RESTful URL path: /v1/....
const (
	Rversion  = "v1"
	Rfiles    = "files"
	Rcluster  = "cluster"
	Rdaemon   = "daemon"
	Rsyncsmap = ActSyncSmap
	Rebalance = ActRebalance
	Rfrom     = "from_id"
	Rto       = "to_id"
	Rsynclb   = ActSyncLB
)
